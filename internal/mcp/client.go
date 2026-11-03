package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// envAllowlist : variables héritées par les serveurs stdio. Le reste de
// l'environnement du bot (clés d'API, secrets issus de .env...) n'est pas
// transmis ; un serveur qui en a besoin le déclare dans "env".
var envAllowlist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LANGUAGE", "TERM",
	"TZ", "TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
	"XDG_CACHE_HOME", "SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
}

func baseEnv() []string {
	var env []string
	for _, k := range envAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}

// maxResultBytes borne le texte renvoyé au LLM par un appel d'outil.
const maxResultBytes = 20000

func truncateResult(s string) string {
	if len(s) <= maxResultBytes {
		return s
	}
	cut := maxResultBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[... résultat tronqué ...]"
}

func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }

const protocolVersion = "2025-06-18"

// ToolInfo est un outil annoncé par un serveur.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method,omitempty"`
	Result json.RawMessage  `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// transport envoie une requête JSON-RPC et retourne son résultat, ou envoie
// une notification (id nil, pas de réponse attendue).
type transport interface {
	roundTrip(ctx context.Context, req rpcRequest) (json.RawMessage, error)
	close()
}

// Client est une connexion à un serveur MCP.
type Client struct {
	Name    string
	Timeout time.Duration
	t       transport
	nextID  int64
	mu      sync.Mutex
}

type connectOpts struct{ oauth OAuthEnv }

// ConnectOption personnalise Connect.
type ConnectOption func(*connectOpts)

// WithOAuth fournit l'environnement du flux OAuth (serveurs avec "oauth").
func WithOAuth(env OAuthEnv) ConnectOption { return func(o *connectOpts) { o.oauth = env } }

// Connect démarre/contacte le serveur et réalise la poignée de main
// "initialize".
func Connect(ctx context.Context, name string, cfg ServerConfig, opts ...ConnectOption) (*Client, error) {
	var co connectOpts
	for _, o := range opts {
		o(&co)
	}
	c := &Client{Name: name, Timeout: 60 * time.Second}
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
			c.Timeout = d
		}
	}
	if cfg.URL != "" {
		ht := &httpTransport{url: cfg.URL, headers: cfg.Headers, hc: &http.Client{}}
		if cfg.OAuth != nil {
			ht.auth = newOAuthSession(name, cfg.URL, *cfg.OAuth, co.oauth)
		}
		c.t = ht
	} else {
		st, err := startStdio(cfg)
		if err != nil {
			return nil, err
		}
		c.t = st
	}
	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "bot", "version": "1"},
	}); err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if _, err := c.t.roundTrip(ctx, rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		c.Close()
		return nil, fmt.Errorf("notifications/initialized: %w", err)
	}
	return c, nil
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	return c.t.roundTrip(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
}

// ListTools retourne tous les outils du serveur (pagination suivie).
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	cursor := ""
	for page := 0; page < 20; page++ {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("tools/list: réponse invalide: %w", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return all, nil
}

// CallTool appelle un outil et retourne le texte du résultat. Un résultat
// "isError" est retourné comme erreur.
func (c *Client) CallTool(ctx context.Context, tool string, args json.RawMessage) (string, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	var a any = map[string]any{}
	if len(bytes.TrimSpace(args)) > 0 {
		a = args
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": tool, "arguments": a})
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			URI      string `json:"uri"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("tools/call: réponse invalide: %w", err)
	}
	var parts []string
	for _, p := range res.Content {
		switch p.Type {
		case "text":
			parts = append(parts, p.Text)
		case "resource_link":
			parts = append(parts, "[ressource: "+p.URI+"]")
		default:
			parts = append(parts, "["+p.Type+" non affichable]")
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" && len(res.StructuredContent) > 0 {
		text = string(res.StructuredContent)
	}
	text = truncateResult(text)
	if res.IsError {
		if text == "" {
			text = "erreur sans détail"
		}
		return "", errors.New(text)
	}
	return text, nil
}

// Close arrête la connexion (et le sous-processus pour stdio).
func (c *Client) Close() {
	if c.t != nil {
		c.t.close()
	}
}

// ---- stdio ----

type stdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	done    chan struct{}
	err     error
}

func startStdio(cfg ServerConfig) (*stdioTransport, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = baseEnv()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// stderr du serveur : jeté (le terminal du chat n'est pas à lui).
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lancement de %q: %w", cfg.Command, err)
	}
	t := &stdioTransport{cmd: cmd, stdin: stdin, pending: map[int64]chan rpcResponse{}, done: make(chan struct{})}
	go t.readLoop(stdout)
	return t, nil
}

func (t *stdioTransport) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var resp rpcResponse
			if json.Unmarshal(line, &resp) == nil && resp.ID != nil && resp.Method == "" {
				var id int64
				if json.Unmarshal(*resp.ID, &id) == nil {
					t.mu.Lock()
					ch := t.pending[id]
					delete(t.pending, id)
					t.mu.Unlock()
					if ch != nil {
						ch <- resp
					}
				}
			}
			// Requêtes/notifications du serveur (sampling, roots, logs...) :
			// non supportées, ignorées.
		}
		if err != nil {
			t.mu.Lock()
			t.err = fmt.Errorf("serveur MCP arrêté: %v", err)
			close(t.done)
			t.mu.Unlock()
			return
		}
	}
}

func (t *stdioTransport) roundTrip(ctx context.Context, req rpcRequest) (json.RawMessage, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	var ch chan rpcResponse
	if req.ID != nil {
		ch = make(chan rpcResponse, 1)
		t.mu.Lock()
		select {
		case <-t.done:
			t.mu.Unlock()
			return nil, t.err
		default:
		}
		t.pending[*req.ID] = ch
		t.mu.Unlock()
	}
	t.writeMu.Lock()
	_, err = t.stdin.Write(data)
	t.writeMu.Unlock()
	if err != nil {
		return nil, err
	}
	if ch == nil {
		return nil, nil
	}
	select {
	case resp := <-ch:
		return resp.unwrap()
	case <-t.done:
		return nil, t.err
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.pending, *req.ID)
		t.mu.Unlock()
		if req.Method == "tools/call" {
			n := rpcRequest{JSONRPC: "2.0", Method: "notifications/cancelled", Params: map[string]any{"requestId": *req.ID}}
			if b, err := json.Marshal(n); err == nil {
				t.writeMu.Lock()
				t.stdin.Write(append(b, '\n'))
				t.writeMu.Unlock()
			}
		}
		return nil, ctx.Err()
	}
}

func (t *stdioTransport) close() {
	t.stdin.Close()
	go func() {
		select {
		case <-t.done:
		case <-time.After(2 * time.Second):
			t.cmd.Process.Kill()
		}
		t.cmd.Wait()
	}()
}

func (r rpcResponse) unwrap() (json.RawMessage, error) {
	if r.Error != nil {
		return nil, fmt.Errorf("erreur MCP %d: %s", r.Error.Code, r.Error.Message)
	}
	return r.Result, nil
}

// ---- http (Streamable HTTP) ----

type httpTransport struct {
	url     string
	headers map[string]string
	hc      *http.Client
	mu      sync.Mutex
	session string
	auth    *oauthSession // nil = pas d'OAuth
}

func (t *httpTransport) roundTrip(ctx context.Context, req rpcRequest) (json.RawMessage, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		hr.Header.Set("Content-Type", "application/json")
		hr.Header.Set("Accept", "application/json, text/event-stream")
		hr.Header.Set("MCP-Protocol-Version", protocolVersion)
		for k, v := range t.headers {
			hr.Header.Set(k, v)
		}
		tok := ""
		if t.auth != nil {
			if tok = t.auth.token(ctx); tok != "" {
				hr.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		t.mu.Lock()
		if t.session != "" {
			hr.Header.Set("Mcp-Session-Id", t.session)
		}
		t.mu.Unlock()
		resp, err = t.hc.Do(hr)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && t.auth != nil && attempt == 0 {
			wa := resp.Header.Get("WWW-Authenticate")
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if err := t.auth.reauthorize(ctx, tok, wa); err != nil {
				return nil, fmt.Errorf("OAuth: %w", err)
			}
			continue
		}
		break
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		t.mu.Lock()
		t.session = s
		t.mu.Unlock()
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if req.ID == nil {
		io.Copy(io.Discard, resp.Body)
		return nil, nil
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSE(resp.Body, *req.ID)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var r rpcResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("réponse invalide: %w", err)
	}
	return r.unwrap()
}

// readSSE lit le flux jusqu'à la réponse portant l'id demandé.
func readSSE(body io.Reader, id int64) (json.RawMessage, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 32<<20)
	var data []string
	flush := func() (json.RawMessage, bool, error) {
		if len(data) == 0 {
			return nil, false, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		var r rpcResponse
		if json.Unmarshal([]byte(payload), &r) != nil || r.ID == nil || r.Method != "" {
			return nil, false, nil
		}
		var got int64
		if json.Unmarshal(*r.ID, &got) != nil || got != id {
			return nil, false, nil
		}
		res, err := r.unwrap()
		return res, true, err
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if res, ok, err := flush(); ok {
				return res, err
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	if res, ok, err := flush(); ok {
		return res, err
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("flux SSE terminé sans réponse")
}

func (t *httpTransport) close() {
	t.mu.Lock()
	s := t.session
	t.mu.Unlock()
	if s == "" {
		return
	}
	req, err := http.NewRequest(http.MethodDelete, t.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", s)
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	hc := &http.Client{Timeout: 2 * time.Second}
	if resp, err := hc.Do(req); err == nil {
		resp.Body.Close()
	}
}

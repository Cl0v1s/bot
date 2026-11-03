package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"bot/internal/mcp"
)

// mcpConnectTimeout borne le démarrage (initialize + tools/list) de chaque
// serveur : un serveur lent ou bloqué ne doit pas empêcher le bot de démarrer.
const mcpConnectTimeout = 30 * time.Second

// mcpOAuthTimeout : budget d'un serveur OAuth en mode interactif (le temps
// d'autoriser dans le navigateur, voir mcp.OAuthEnv.AuthTimeout).
const mcpOAuthTimeout = 6 * time.Minute

// MCPTool expose un outil d'un serveur MCP (déclaré dans la configuration,
// voir mcp.ConfigFileName) au LLM sous le nom "mcp_<serveur>_<outil>".
// La configuration étant écrite par l'utilisateur réel (fichier de confiance),
// l'appel n'est soumis à aucune confirmation supplémentaire.
type MCPTool struct {
	Client *mcp.Client
	Info   mcp.ToolInfo
	name   string
}

func (t *MCPTool) Name() string { return t.name }

func (t *MCPTool) Description() string {
	d := strings.TrimSpace(t.Info.Description)
	if d == "" {
		d = t.Info.Name
	}
	return fmt.Sprintf("[serveur MCP %q] %s", t.Client.Name, d)
}

func (t *MCPTool) ParametersSchema() json.RawMessage {
	var probe struct {
		Type string `json:"type"`
	}
	if len(t.Info.InputSchema) == 0 || json.Unmarshal(t.Info.InputSchema, &probe) != nil || probe.Type != "object" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return t.Info.InputSchema
}

func (t *MCPTool) Call(ctx context.Context, argsJSON string) (string, error) {
	if strings.TrimSpace(argsJSON) == "" {
		argsJSON = "{}"
	}
	if !json.Valid([]byte(argsJSON)) {
		return "", fmt.Errorf("arguments invalides: JSON attendu")
	}
	return t.Client.CallTool(ctx, t.Info.Name, json.RawMessage(argsJSON))
}

var mcpNameRe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// mcpToolName construit un nom accepté par l'API (^[A-Za-z0-9_-]{1,64}$).
func mcpToolName(server, tool string) string {
	n := "mcp_" + server + "_" + mcpNameRe.ReplaceAllString(tool, "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// LoadMCPTools lit la configuration MCP (absente = aucun outil), connecte
// chaque serveur actif en parallèle et retourne leurs outils. Un serveur en
// échec est signalé via warn et ignoré. Appeler le closer retourné à l'arrêt
// pour terminer les sous-processus.
func LoadMCPTools(ctx context.Context, path string, warn func(string)) ([]Tool, func()) {
	list, _, closeFn := LoadMCPToolsStatus(ctx, path, warn)
	return list, closeFn
}

// MCPStatus décrit l'état d'un serveur MCP configuré au démarrage.
type MCPStatus struct {
	Name     string
	Disabled bool
	OK       bool
	Tools    int
	Err      string
}

// String : ligne d'état lisible ("✓ nom : 3 outil(s)", "✗ nom : erreur"…).
func (s MCPStatus) String() string {
	switch {
	case s.Disabled:
		return fmt.Sprintf("- %s : désactivé", s.Name)
	case s.OK:
		return fmt.Sprintf("✓ %s : fonctionne (%d outil(s))", s.Name, s.Tools)
	default:
		return fmt.Sprintf("✗ %s : %s", s.Name, s.Err)
	}
}

// MCPLoadOptions : réglages de LoadMCPToolsStatus.
type MCPLoadOptions struct {
	// Interactive : autorise le flux OAuth dans le navigateur (mode chat).
	// false (mode mail) : seuls les jetons déjà stockés sont utilisés.
	Interactive bool
	// Notify : message à l'utilisateur (URL d'autorisation OAuth).
	Notify func(string)
}

// LoadMCPToolsStatus fait comme LoadMCPTools et retourne en plus l'état de
// chaque serveur configuré (triés par nom, désactivés inclus).
func LoadMCPToolsStatus(ctx context.Context, path string, warn func(string), opts ...MCPLoadOptions) ([]Tool, []MCPStatus, func()) {
	var lo MCPLoadOptions
	if len(opts) > 0 {
		lo = opts[0]
	}
	oauthEnv := mcp.OAuthEnv{
		TokensFile:  filepath.Join(filepath.Dir(path), mcp.TokensFileName),
		Interactive: lo.Interactive,
		Notify:      lo.Notify,
	}
	noop := func() {}
	if path == "" {
		return nil, nil, noop
	}
	cfg, err := mcp.LoadConfig(path)
	if err != nil {
		warn(fmt.Sprintf("MCP: %v", err))
		return nil, nil, noop
	}
	if cfg == nil {
		return nil, nil, noop
	}
	var statuses []MCPStatus
	var all []string
	for n := range cfg.Servers {
		all = append(all, n)
	}
	sort.Strings(all)
	stIdx := map[string]int{}
	for _, n := range all {
		stIdx[n] = len(statuses)
		statuses = append(statuses, MCPStatus{Name: n, Disabled: cfg.Servers[n].Disabled})
	}
	// warn est appelé depuis plusieurs goroutines : on sérialise.
	var warnMu sync.Mutex
	rawWarn := warn
	warn = func(m string) {
		warnMu.Lock()
		defer warnMu.Unlock()
		rawWarn(m)
	}
	names := cfg.ActiveServers()
	type result struct {
		client *mcp.Client
		infos  []mcp.ToolInfo
	}
	results := make([]result, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timeout := mcpConnectTimeout
			if cfg.Servers[name].OAuth != nil && lo.Interactive {
				timeout = mcpOAuthTimeout
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			c, err := mcp.Connect(cctx, name, cfg.Servers[name], mcp.WithOAuth(oauthEnv))
			if err != nil {
				statuses[stIdx[name]].Err = err.Error()
				return
			}
			infos, err := c.ListTools(cctx)
			if err != nil {
				statuses[stIdx[name]].Err = "tools/list: " + err.Error()
				c.Close()
				return
			}
			statuses[stIdx[name]].OK = true
			statuses[stIdx[name]].Tools = len(infos)
			results[i] = result{c, infos}
		}()
	}
	wg.Wait()

	var list []Tool
	var clients []*mcp.Client
	seen := map[string]bool{}
	for _, r := range results {
		if r.client == nil {
			continue
		}
		clients = append(clients, r.client)
		for _, info := range r.infos {
			n := mcpToolName(r.client.Name, info.Name)
			if seen[n] {
				warn(fmt.Sprintf("MCP %q: outil %q ignoré (nom %q en doublon)", r.client.Name, info.Name, n))
				continue
			}
			seen[n] = true
			list = append(list, &MCPTool{Client: r.client, Info: info, name: n})
		}
	}
	return list, statuses, func() {
		for _, c := range clients {
			c.Close()
		}
	}
}

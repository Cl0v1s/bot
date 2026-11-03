package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Processus factice : TestMain l'exécute comme serveur MCP stdio.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_FAKE_SERVER") == "1" {
		fakeStdioServer()
		return
	}
	os.Exit(m.Run())
}

func fakeStdioServer() {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		var req map[string]any
		if dec.Decode(&req) != nil {
			return
		}
		id, has := req["id"]
		if !has {
			continue
		}
		var res any
		switch req["method"] {
		case "tools/list":
			res = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "d", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			a := req["params"].(map[string]any)["arguments"].(map[string]any)
			res = map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo:" + a["msg"].(string)}}}
		default:
			res = map[string]any{}
		}
		enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": res})
	}
}

func TestStdioRoundTrip(t *testing.T) {
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, "fake", ServerConfig{Command: exe, Env: map[string]string{"MCP_FAKE_SERVER": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %+v, %v", tools, err)
	}
	out, err := c.CallTool(ctx, "echo", json.RawMessage(`{"msg":"hi"}`))
	if err != nil || out != "echo:hi" {
		t.Fatalf("CallTool = %q, %v", out, err)
	}
}

func TestHTTPJSONAndSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if r.Header.Get("Authorization") != "Bearer x" {
			http.Error(w, "no", 401)
			return
		}
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		switch req.Method {
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"))
			w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":" + itoa(*req.ID) + ",\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"sse ok\"}]}}\n\n"))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*req.ID) + `,"result":{}}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c, err := Connect(ctx, "h", ServerConfig{URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := c.CallTool(ctx, "t", nil)
	if err != nil || out != "sse ok" {
		t.Fatalf("CallTool = %q, %v", out, err)
	}
	if _, err := Connect(ctx, "bad", ServerConfig{URL: srv.URL}); err == nil {
		t.Fatal("401 attendu")
	}
}

func itoa(i int64) string { b, _ := json.Marshal(i); return string(b) }

func TestParseConfig(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"mcpServers":{"a":{"command":"x"},"b":{"url":"https://h","timeout":"5s"}}}`)); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"les deux":   `{"mcpServers":{"a":{"command":"x","url":"http://h"}}}`,
		"aucun":      `{"mcpServers":{"a":{}}}`,
		"nom":        `{"mcpServers":{"a b":{"command":"x"}}}`,
		"champ":      `{"mcpServers":{"a":{"command":"x","foo":1}}}`,
		"timeout":    `{"mcpServers":{"a":{"command":"x","timeout":"zz"}}}`,
		"url schéma": `{"mcpServers":{"a":{"url":"ftp://h"}}}`,
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("%s: erreur attendue", name)
		}
	}
	_ = strings.TrimSpace
}

func TestTruncateResult(t *testing.T) {
	s := strings.Repeat("é", maxResultBytes)
	got := truncateResult(s)
	if !utf8.ValidString(got) || len(got) > maxResultBytes+50 || !strings.Contains(got, "tronqué") {
		t.Fatalf("troncature invalide: len=%d", len(got))
	}
	if truncateResult("court") != "court" {
		t.Fatal("un résultat court ne doit pas changer")
	}
}

func TestBaseEnvExcludesSecrets(t *testing.T) {
	t.Setenv("MON_SECRET_API_KEY", "x")
	t.Setenv("LC_ALL", "C")
	var hasLC bool
	for _, kv := range baseEnv() {
		if strings.HasPrefix(kv, "MON_SECRET") {
			t.Fatal("secret transmis")
		}
		hasLC = hasLC || kv == "LC_ALL=C"
	}
	if !hasLC {
		t.Fatal("LC_* attendu")
	}
}

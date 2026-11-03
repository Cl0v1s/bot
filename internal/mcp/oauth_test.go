package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeOAuthServer : serveur MCP protégé + serveur d'autorisation (même hôte).
type fakeOAuthServer struct {
	*httptest.Server
	registrations, authCodes, refreshes atomic.Int32
	validToken                          atomic.Value // string
	challenge                           string
}

func newFakeOAuthServer(t *testing.T) *fakeOAuthServer {
	f := &fakeOAuthServer{}
	f.validToken.Store("")
	mux := http.NewServeMux()
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	js := func(w http.ResponseWriter, v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"authorization_servers": []string{f.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"issuer": f.URL, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token", "registration_endpoint": f.URL + "/register"})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		f.registrations.Add(1)
		js(w, map[string]any{"client_id": "dyn-client"})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("resource") != f.URL+"/mcp" {
			http.Error(w, "bad", 400)
			return
		}
		f.challenge = q.Get("code_challenge")
		f.authCodes.Add(1)
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=thecode&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "thecode" || r.Form.Get("code_verifier") == "" {
				js(w, map[string]any{"error": "invalid_grant"})
				return
			}
			f.validToken.Store("tok1")
			js(w, map[string]any{"access_token": "tok1", "refresh_token": "ref1", "expires_in": 3600})
		case "refresh_token":
			if r.Form.Get("refresh_token") != "ref1" {
				w.WriteHeader(400)
				js(w, map[string]any{"error": "invalid_grant"})
				return
			}
			f.refreshes.Add(1)
			f.validToken.Store("tok2")
			js(w, map[string]any{"access_token": "tok2", "expires_in": 3600})
		}
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID *int64 `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		want, _ := f.validToken.Load().(string)
		if want == "" || r.Header.Get("Authorization") != "Bearer "+want {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.URL+`/.well-known/oauth-protected-resource"`)
			http.Error(w, "unauthorized", 401)
			return
		}
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*req.ID) + `,"result":{}}`))
	})
	return f
}

// browserFollow simule le navigateur : GET de l'URL d'autorisation (suit la redirection vers le callback local).
func browserFollow(t *testing.T) func(string) {
	return func(u string) {
		go func() {
			resp, err := http.Get(u)
			if err != nil {
				t.Errorf("navigateur: %v", err)
				return
			}
			resp.Body.Close()
		}()
	}
}

func TestOAuthFlowRefreshAndPersistence(t *testing.T) {
	f := newFakeOAuthServer(t)
	tokens := filepath.Join(t.TempDir(), ".bot_mcp_tokens.json")
	cfg := ServerConfig{URL: f.URL + "/mcp", OAuth: &OAuthConfig{}}
	var notified string
	env := OAuthEnv{TokensFile: tokens, Interactive: true, Notify: func(m string) { notified = m }, OpenBrowser: browserFollow(t)}

	ctx := context.Background()
	c, err := Connect(ctx, "srv", cfg, WithOAuth(env))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if f.registrations.Load() != 1 || f.authCodes.Load() != 1 || !strings.Contains(notified, "/authorize") {
		t.Fatalf("flux: reg=%d codes=%d notif=%q", f.registrations.Load(), f.authCodes.Load(), notified)
	}

	// Jetons persistés : reconnexion non interactive sans nouveau flux.
	env.Interactive = false
	c, err = Connect(ctx, "srv", cfg, WithOAuth(env))
	if err != nil {
		t.Fatalf("reconnexion avec jeton stocké: %v", err)
	}
	c.Close()
	if f.authCodes.Load() != 1 {
		t.Fatal("nouveau flux inattendu")
	}

	// Jeton révoqué côté serveur : 401 → refresh automatique, sans interaction.
	f.validToken.Store("rotated")
	// le refresh renvoie tok2, accepté
	f.validToken.Store("other")
	c, err = Connect(ctx, "srv", cfg, WithOAuth(env))
	if err != nil {
		t.Fatalf("refresh sur 401: %v", err)
	}
	c.Close()
	if f.refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d", f.refreshes.Load())
	}
}

func TestOAuthNonInteractiveWithoutToken(t *testing.T) {
	f := newFakeOAuthServer(t)
	env := OAuthEnv{TokensFile: filepath.Join(t.TempDir(), "t.json")}
	_, err := Connect(context.Background(), "srv", ServerConfig{URL: f.URL + "/mcp", OAuth: &OAuthConfig{}}, WithOAuth(env))
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, ErrAuthRequired attendu", err)
	}
}

func TestParseConfigOAuth(t *testing.T) {
	ok := `{"mcpServers":{"s":{"url":"https://x/mcp","oauth":{"clientId":"a","clientSecret":"b","scopes":["x"],"callbackPort":8123}}}}`
	if _, err := ParseConfig([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"mcpServers":{"s":{"command":"x","oauth":{}}}}`,
		`{"mcpServers":{"s":{"url":"https://x","headers":{"authorization":"b"},"oauth":{}}}}`,
		`{"mcpServers":{"s":{"url":"https://x","oauth":{"clientSecret":"b"}}}}`,
		`{"mcpServers":{"s":{"url":"https://x","oauth":{"authorizationUrl":"https://a"}}}}`,
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("config acceptée à tort: %s", bad)
		}
	}
}

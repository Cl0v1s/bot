package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"bot/internal/sandbox"
)

// OAuthConfig active OAuth 2.1 (code d'autorisation + PKCE) pour un serveur
// MCP distant. Tous les champs sont facultatifs : "oauth": {} suffit pour un
// serveur qui annonce ses métadonnées (RFC 9728/8414) et accepte
// l'enregistrement dynamique de client (RFC 7591). Sinon (ex: Slack),
// déclarer le client pré-enregistré chez le fournisseur.
type OAuthConfig struct {
	ClientID     string   `json:"clientId,omitempty"`
	ClientSecret string   `json:"clientSecret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	// AuthorizationURL / TokenURL : court-circuitent la découverte.
	AuthorizationURL string `json:"authorizationUrl,omitempty"`
	TokenURL         string `json:"tokenUrl,omitempty"`
	// CallbackPort : port local de redirection (http://localhost:<port>/callback),
	// à enregistrer chez le fournisseur pour un client pré-enregistré.
	// 0 = port éphémère (enregistrement dynamique uniquement).
	CallbackPort int `json:"callbackPort,omitempty"`
}

// OAuthEnv regroupe ce que le flux OAuth tire de l'environnement du bot.
type OAuthEnv struct {
	// TokensFile : fichier de jetons (voir sandbox.MCPTokensFileName). "" = en mémoire seulement.
	TokensFile string
	// Interactive autorise l'ouverture du navigateur pour obtenir une
	// autorisation. false (mode mail) : seuls jetons stockés/rafraîchis.
	Interactive bool
	// Notify affiche un message à l'utilisateur (URL d'autorisation...).
	Notify func(string)
	// OpenBrowser ouvre l'URL (défaut: xdg-open/open ; échec ignoré, l'URL est affichée).
	OpenBrowser func(string)
	// AuthTimeout : attente max de l'autorisation (défaut 5 min).
	AuthTimeout time.Duration
	HTTPClient  *http.Client
}

// ErrAuthRequired : aucun jeton valide et le flux interactif est indisponible.
var ErrAuthRequired = errors.New("autorisation OAuth requise : lancez `./bot chat` une fois pour autoriser ce serveur")

// tokenRecord : état persistant d'un serveur (jetons + client utilisé).
type tokenRecord struct {
	ServerURL    string    `json:"server_url"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	TokenURL     string    `json:"token_url,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	ClientSecret string    `json:"client_secret,omitempty"`
}

// tokenStore sérialise l'accès au fichier de jetons, partagé par tous les serveurs.
var tokenStoreMu sync.Mutex

func loadRecord(file, name string) (tokenRecord, bool) {
	if file == "" {
		return tokenRecord{}, false
	}
	tokenStoreMu.Lock()
	defer tokenStoreMu.Unlock()
	m := readTokens(file)
	r, ok := m[name]
	return r, ok
}

func readTokens(file string) map[string]tokenRecord {
	m := map[string]tokenRecord{}
	data, err := sandbox.ReadTrustedFile(file)
	if err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

func saveRecord(file, name string, r tokenRecord) error {
	if file == "" {
		return nil
	}
	tokenStoreMu.Lock()
	defer tokenStoreMu.Unlock()
	m := readTokens(file)
	m[name] = r
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return sandbox.WriteTrustedFile(file, data)
}

// oauthSession gère le jeton d'un serveur. Sûr pour un usage concurrent.
type oauthSession struct {
	name      string
	serverURL string
	cfg       OAuthConfig
	env       OAuthEnv
	hc        *http.Client

	mu  sync.Mutex
	rec tokenRecord
}

func newOAuthSession(name, serverURL string, cfg OAuthConfig, env OAuthEnv) *oauthSession {
	hc := env.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	s := &oauthSession{name: name, serverURL: serverURL, cfg: cfg, env: env, hc: hc}
	if r, ok := loadRecord(env.TokensFile, name); ok && r.ServerURL == serverURL {
		s.rec = r
	}
	return s
}

// token retourne un jeton d'accès valide (rafraîchi si expiré), "" si aucun.
func (s *oauthSession) token(ctx context.Context) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec.AccessToken == "" {
		return ""
	}
	if !s.rec.Expiry.IsZero() && time.Until(s.rec.Expiry) < 30*time.Second {
		if s.refreshLocked(ctx) != nil {
			return ""
		}
	}
	return s.rec.AccessToken
}

// reauthorize est appelé sur 401 : rafraîchit, sinon relance le flux
// interactif. stale = jeton rejeté (si un autre appel l'a déjà remplacé, on
// n'en refait pas un).
func (s *oauthSession) reauthorize(ctx context.Context, stale, wwwAuth string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec.AccessToken != "" && s.rec.AccessToken != stale {
		return nil
	}
	if s.rec.RefreshToken != "" && s.refreshLocked(ctx) == nil {
		return nil
	}
	if !s.env.Interactive {
		return ErrAuthRequired
	}
	return s.authorizeLocked(ctx, wwwAuth)
}

func (s *oauthSession) refreshLocked(ctx context.Context) error {
	if s.rec.RefreshToken == "" || s.rec.TokenURL == "" {
		return errors.New("pas de refresh token")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.rec.RefreshToken},
		"client_id":     {s.rec.ClientID},
		"resource":      {s.serverURL},
	}
	if s.rec.ClientSecret != "" {
		form.Set("client_secret", s.rec.ClientSecret)
	}
	keep := s.rec.RefreshToken
	if err := s.tokenRequestLocked(ctx, s.rec.TokenURL, form); err != nil {
		s.rec.AccessToken = ""
		return err
	}
	if s.rec.RefreshToken == "" {
		s.rec.RefreshToken = keep
	}
	return saveRecord(s.env.TokensFile, s.name, s.rec)
}

func (s *oauthSession) tokenRequestLocked(ctx context.Context, tokenURL string, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	_ = json.Unmarshal(b, &tr)
	if resp.StatusCode/100 != 2 || tr.AccessToken == "" {
		msg := tr.Error
		if tr.ErrorDesc != "" {
			msg += ": " + tr.ErrorDesc
		}
		if msg == "" {
			msg = strings.TrimSpace(string(b))
			if len(msg) > 200 {
				msg = msg[:200]
			}
		}
		return fmt.Errorf("échange de jeton (HTTP %d): %s", resp.StatusCode, msg)
	}
	s.rec.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		s.rec.RefreshToken = tr.RefreshToken
	}
	s.rec.Expiry = time.Time{}
	if tr.ExpiresIn > 0 {
		s.rec.Expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return nil
}

// ---- découverte ----

type asMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
	AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
}

func (s *oauthSession) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// wwwAuthParam extrait un paramètre (ex: resource_metadata) d'un en-tête WWW-Authenticate.
func wwwAuthParam(h, key string) string {
	i := strings.Index(h, key+"=\"")
	if i < 0 {
		return ""
	}
	rest := h[i+len(key)+2:]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

func (s *oauthSession) discover(ctx context.Context, wwwAuth string) (asMetadata, error) {
	var md asMetadata
	if s.cfg.AuthorizationURL != "" && s.cfg.TokenURL != "" {
		md.AuthorizationEndpoint, md.TokenEndpoint = s.cfg.AuthorizationURL, s.cfg.TokenURL
		return md, nil
	}
	su, err := url.Parse(s.serverURL)
	if err != nil {
		return md, err
	}
	origin := su.Scheme + "://" + su.Host
	// 1. serveur de ressources → serveur d'autorisation (RFC 9728)
	issuer := origin
	var pr struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	cands := []string{}
	if u := wwwAuthParam(wwwAuth, "resource_metadata"); u != "" {
		cands = append(cands, u)
	}
	if p := strings.TrimSuffix(su.Path, "/"); p != "" {
		cands = append(cands, origin+"/.well-known/oauth-protected-resource"+p)
	}
	cands = append(cands, origin+"/.well-known/oauth-protected-resource")
	for _, u := range cands {
		if s.getJSON(ctx, u, &pr) == nil && len(pr.AuthorizationServers) > 0 {
			issuer = strings.TrimSuffix(pr.AuthorizationServers[0], "/")
			break
		}
	}
	// 2. métadonnées du serveur d'autorisation (RFC 8414, puis OIDC)
	iu, err := url.Parse(issuer)
	if err != nil {
		return md, err
	}
	iorigin := iu.Scheme + "://" + iu.Host
	ipath := strings.TrimSuffix(iu.Path, "/")
	mcands := []string{
		iorigin + "/.well-known/oauth-authorization-server" + ipath,
		iorigin + "/.well-known/openid-configuration" + ipath,
		issuer + "/.well-known/openid-configuration",
	}
	for _, u := range mcands {
		var m asMetadata
		if s.getJSON(ctx, u, &m) == nil && m.AuthorizationEndpoint != "" && m.TokenEndpoint != "" {
			if s.cfg.AuthorizationURL != "" {
				m.AuthorizationEndpoint = s.cfg.AuthorizationURL
			}
			if s.cfg.TokenURL != "" {
				m.TokenEndpoint = s.cfg.TokenURL
			}
			return m, nil
		}
	}
	return md, fmt.Errorf("métadonnées OAuth introuvables pour %s (renseigner authorizationUrl et tokenUrl dans \"oauth\")", s.serverURL)
}

// ---- flux interactif ----

func randString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *oauthSession) register(ctx context.Context, endpoint, redirect string) (string, string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "bot",
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var r struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 || json.Unmarshal(b, &r) != nil || r.ClientID == "" {
		return "", "", fmt.Errorf("enregistrement dynamique du client refusé (HTTP %d)", resp.StatusCode)
	}
	return r.ClientID, r.ClientSecret, nil
}

func defaultOpenBrowser(u string) {
	for _, bin := range []string{"xdg-open", "open"} {
		if p, err := exec.LookPath(bin); err == nil {
			_ = exec.Command(p, u).Start()
			return
		}
	}
}

func (s *oauthSession) authorizeLocked(ctx context.Context, wwwAuth string) error {
	md, err := s.discover(ctx, wwwAuth)
	if err != nil {
		return err
	}
	port := s.cfg.CallbackPort
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("écoute du callback OAuth: %w", err)
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://localhost:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	clientID, clientSecret := s.cfg.ClientID, s.cfg.ClientSecret
	if clientID == "" {
		if s.rec.ClientID != "" && s.rec.TokenURL == md.TokenEndpoint && port != 0 {
			clientID, clientSecret = s.rec.ClientID, s.rec.ClientSecret
		} else if md.RegistrationEndpoint != "" {
			if clientID, clientSecret, err = s.register(ctx, md.RegistrationEndpoint, redirect); err != nil {
				return err
			}
		} else {
			return errors.New("serveur sans enregistrement dynamique : renseigner clientId (et clientSecret) dans \"oauth\"")
		}
	}

	verifier := randString(48)
	sum := sha256.Sum256([]byte(verifier))
	state := randString(24)
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"resource":              {s.serverURL},
	}
	scopes := s.cfg.Scopes
	if len(scopes) == 0 {
		if sc := wwwAuthParam(wwwAuth, "scope"); sc != "" {
			scopes = strings.Fields(sc)
		}
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	authURL, err := url.Parse(md.AuthorizationEndpoint)
	if err != nil {
		return err
	}
	aq := authURL.Query()
	for k, v := range q {
		aq[k] = v
	}
	authURL.RawQuery = aq.Encode()

	type cbResult struct{ code, err string }
	ch := make(chan cbResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		qs := r.URL.Query()
		res := cbResult{code: qs.Get("code")}
		switch {
		case qs.Get("state") != state:
			res = cbResult{err: "paramètre state invalide"}
		case qs.Get("error") != "":
			res = cbResult{err: qs.Get("error") + " " + qs.Get("error_description")}
		case res.code == "":
			res = cbResult{err: "code absent"}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if res.err != "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, "Autorisation échouée : "+res.err)
		} else {
			fmt.Fprintln(w, "Autorisation reçue, vous pouvez fermer cet onglet et retourner au bot.")
		}
		select {
		case ch <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	if s.env.Notify != nil {
		s.env.Notify(fmt.Sprintf("MCP %q : autorisation OAuth requise. Ouvrez cette URL si le navigateur ne s'ouvre pas :\n  %s", s.name, authURL.String()))
	}
	open := s.env.OpenBrowser
	if open == nil {
		open = defaultOpenBrowser
	}
	open(authURL.String())

	timeout := s.env.AuthTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var res cbResult
	select {
	case res = <-ch:
	case <-tctx.Done():
		return errors.New("autorisation OAuth non reçue à temps")
	}
	if res.err != "" {
		return fmt.Errorf("autorisation OAuth refusée: %s", res.err)
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"redirect_uri":  {redirect},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {s.serverURL},
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	s.rec = tokenRecord{ServerURL: s.serverURL, TokenURL: md.TokenEndpoint, ClientID: clientID, ClientSecret: clientSecret}
	if err := s.tokenRequestLocked(ctx, md.TokenEndpoint, form); err != nil {
		s.rec = tokenRecord{}
		return err
	}
	if err := saveRecord(s.env.TokensFile, s.name, s.rec); err != nil {
		return fmt.Errorf("sauvegarde des jetons: %w", err)
	}
	return nil
}

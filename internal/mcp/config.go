// Package mcp est un client minimal du Model Context Protocol (MCP), en
// stdlib uniquement : il lance ou contacte les serveurs déclarés dans le
// fichier de configuration du workspace (voir LoadConfig) et expose leurs
// outils au LLM (voir internal/tools.MCPTool).
//
// Transports supportés : "stdio" (sous-processus, JSON-RPC une ligne par
// message) et "http" (Streamable HTTP : POST JSON-RPC, réponse JSON ou SSE).
// Seuls les outils (tools/list, tools/call) sont pris en charge.
package mcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"bot/internal/sandbox"
)

// ConfigFileName est le fichier de configuration, à la racine du workspace.
// Fichier de confiance (voir sandbox.ReadTrustedFile) : il déclare des
// commandes exécutées sous l'identité réelle, le compte sandbox ne doit donc
// jamais pouvoir le modifier.
const ConfigFileName = sandbox.MCPConfigFileName

// TokensFileName : jetons OAuth, à côté du fichier de configuration.
const TokensFileName = sandbox.MCPTokensFileName

// ServerConfig décrit un serveur MCP. Format compatible avec le champ
// "mcpServers" de Claude Desktop / Claude Code :
//
//	{"mcpServers": {
//	  "fs":  {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]},
//	  "web": {"url": "https://exemple.org/mcp", "headers": {"Authorization": "Bearer ..."}}
//	}}
type ServerConfig struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Dir     string            `json:"cwd,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// OAuth : active OAuth 2.1 + PKCE (serveur http uniquement, voir OAuthConfig).
	OAuth *OAuthConfig `json:"oauth,omitempty"`
	// Disabled : serveur déclaré mais ignoré.
	Disabled bool `json:"disabled,omitempty"`
	// Timeout : durée max d'un appel d'outil (Go duration, ex "30s"). Défaut 60s.
	Timeout string `json:"timeout,omitempty"`
}

// Config est le contenu du fichier de configuration.
type Config struct {
	Servers map[string]ServerConfig `json:"mcpServers"`
}

var serverNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// ParseConfig décode et valide data. Un serveur invalide est une erreur :
// mieux vaut refuser une configuration ambiguë que l'ignorer en silence.
func ParseConfig(data []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("configuration MCP invalide: %w", err)
	}
	for name, s := range c.Servers {
		if !serverNameRe.MatchString(name) {
			return nil, fmt.Errorf("serveur MCP %q: nom invalide (lettres, chiffres, '_' et '-', 32 max)", name)
		}
		if (s.Command == "") == (s.URL == "") {
			return nil, fmt.Errorf("serveur MCP %q: exactement un de \"command\" (stdio) ou \"url\" (http) est requis", name)
		}
		if s.URL != "" && !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			return nil, fmt.Errorf("serveur MCP %q: url doit commencer par http:// ou https://", name)
		}
		if s.OAuth != nil {
			if s.URL == "" {
				return nil, fmt.Errorf("serveur MCP %q: \"oauth\" ne s'applique qu'à un serveur \"url\"", name)
			}
			for h := range s.Headers {
				if strings.EqualFold(h, "Authorization") {
					return nil, fmt.Errorf("serveur MCP %q: \"oauth\" et un en-tête Authorization sont incompatibles", name)
				}
			}
			if s.OAuth.ClientSecret != "" && s.OAuth.ClientID == "" {
				return nil, fmt.Errorf("serveur MCP %q: oauth.clientSecret exige oauth.clientId", name)
			}
			if s.OAuth.CallbackPort < 0 || s.OAuth.CallbackPort > 65535 {
				return nil, fmt.Errorf("serveur MCP %q: oauth.callbackPort invalide", name)
			}
			if (s.OAuth.AuthorizationURL == "") != (s.OAuth.TokenURL == "") {
				return nil, fmt.Errorf("serveur MCP %q: oauth.authorizationUrl et oauth.tokenUrl vont ensemble", name)
			}
		}
		if s.Timeout != "" {
			if _, err := time.ParseDuration(s.Timeout); err != nil {
				return nil, fmt.Errorf("serveur MCP %q: timeout invalide: %w", name, err)
			}
		}
	}
	return &c, nil
}

// LoadConfig lit le fichier de configuration. Absent = (nil, nil) : MCP est
// simplement inutilisé.
func LoadConfig(path string) (*Config, error) {
	data, err := sandbox.ReadTrustedFile(path)
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseConfig(data)
}

// ActiveServers retourne les noms de serveurs actifs, triés (ordre d'outils stable).
func (c *Config) ActiveServers() []string {
	var out []string
	for n, s := range c.Servers {
		if !s.Disabled {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

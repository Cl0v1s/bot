package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// HTTPGetTool effectue une requête HTTP GET simple.
//
// Avertissement : aucune protection SSRF n'est implémentée (pas de filtrage
// des adresses privées/locales). À activer en connaissance de cause si le
// LLM traite des entrées non fiables (voir README).
type HTTPGetTool struct {
	Timeout      time.Duration
	MaxBodyBytes int
}

func (t *HTTPGetTool) Name() string { return "http_get" }

func (t *HTTPGetTool) Description() string {
	return "Effectue une requête HTTP GET vers une URL http(s) et retourne le statut et le corps de la réponse (tronqué si volumineux)."
}

func (t *HTTPGetTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "URL http(s) à récupérer."}
		},
		"required": ["url"],
		"additionalProperties": false
	}`)
}

type httpGetArgs struct {
	URL string `json:"url"`
}

func (t *HTTPGetTool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args httpGetArgs
	if err := decodeArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if args.URL == "" {
		return "", fmt.Errorf(`paramètre "url" requis`)
	}

	parsed, err := url.Parse(args.URL)
	if err != nil {
		return "", fmt.Errorf("URL invalide: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("schéma %q non autorisé (http/https uniquement)", parsed.Scheme)
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, args.URL, nil)
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("requête HTTP: %w", err)
	}
	defer resp.Body.Close()

	maxBytes := t.MaxBodyBytes
	if maxBytes <= 0 {
		maxBytes = 20000
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return "", fmt.Errorf("lecture réponse: %w", err)
	}

	truncated := len(body) > maxBytes
	if truncated {
		body = body[:maxBytes]
	}

	result := fmt.Sprintf("HTTP %s\n\n%s", resp.Status, string(body))
	if truncated {
		result += "\n[... corps tronqué ...]"
	}
	return result, nil
}

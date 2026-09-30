package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// STT transcrit l'audio via le même serveur Unsloth Studio que le LLM
// (POST {racine}/api/inference/audio/transcribe/raw, corps WAV brut).
// Contrairement à sa route OpenAI /v1/audio/transcriptions, celle-ci
// accepte le moteur et le périphérique à chaque requête : la route OpenAI
// choisit d'office le moteur Transformers pour Whisper et recharge un
// modèle déchargé sur le GPU, où il évince le LLM.
type STT struct {
	// BaseURL : URL du LLM (".../v1"), dont on retire le "/v1" final.
	BaseURL  string
	APIKey   string
	Model    string
	Language string
	// Engine : moteur Unsloth ("gguf" = whisper.cpp, "mtmd" = Qwen3-ASR via
	// llama.cpp, "transformers"). "" = choix du serveur.
	Engine string
	// Device : "cpu", "gpu" ou "auto". "" = choix du serveur.
	Device string
	// Timeout : 0 = 2 min (le premier appel peut inclure le chargement du
	// modèle côté serveur).
	Timeout    time.Duration
	HTTPClient *http.Client
	// MaxRetries : relances après une erreur passagère (voir errRetryable),
	// espacées de RetryBackoff doublé à chaque fois. 0 = 3 relances à partir
	// de 1 s : le serveur de dictée, déchargé après quelques minutes
	// d'inactivité, peut répondre 500 le temps d'être relancé. < 0 = aucune
	// relance.
	MaxRetries   int
	RetryBackoff time.Duration
	// OnRetry, si non nil, est appelé avant chaque relance.
	OnRetry func(attempt, max int, err error)
}

// errRetryable : échec passager côté serveur (5xx) ou connexion, qui
// justifie de rejouer la requête à l'identique.
type errRetryable struct{ err error }

func (e errRetryable) Error() string { return e.err.Error() }
func (e errRetryable) Unwrap() error { return e.err }

// Transcribe envoie wav et retourne le texte reconnu, en relançant la
// requête sur une erreur passagère (voir MaxRetries).
func (s STT) Transcribe(ctx context.Context, wav []byte) (string, error) {
	maxRetries, backoff := s.MaxRetries, s.RetryBackoff
	if maxRetries == 0 {
		maxRetries = 3
	}
	if backoff <= 0 {
		backoff = time.Second
	}
	text, err := s.transcribeOnce(ctx, wav)
	for attempt := 1; attempt <= maxRetries && errors.As(err, new(errRetryable)) && ctx.Err() == nil; attempt++ {
		if s.OnRetry != nil {
			s.OnRetry(attempt, maxRetries, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		text, err = s.transcribeOnce(ctx, wav)
	}
	return text, err
}

func (s STT) endpoint() string {
	q := url.Values{}
	for k, v := range map[string]string{"model": s.Model, "language": s.Language, "engine": s.Engine, "device": s.Device} {
		if v != "" {
			q.Set(k, v)
		}
	}
	root := strings.TrimSuffix(strings.TrimRight(s.BaseURL, "/"), "/v1")
	u := root + "/api/inference/audio/transcribe/raw"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func (s STT) transcribeOnce(ctx context.Context, wav []byte) (string, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint(), bytes.NewReader(wav))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "audio/wav")
	if s.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
	}
	hc := s.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		err = fmt.Errorf("requête STT : %w", err)
		if ctx.Err() == nil {
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				return "", errRetryable{err}
			}
		}
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("lecture de la réponse STT : %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("STT : statut %d : %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		if resp.StatusCode >= 500 {
			return "", errRetryable{err}
		}
		return "", err
	}
	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("réponse STT illisible : %w (%s)", err, strings.TrimSpace(string(raw)))
	}
	return strings.TrimSpace(parsed.Text), nil
}

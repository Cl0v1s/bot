package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// STT est un client de transcription au format OpenAI
// (POST {BaseURL}/audio/transcriptions, multipart), servi par le même
// serveur que le LLM (Unsloth Studio).
type STT struct {
	BaseURL  string
	APIKey   string
	Model    string
	Language string
	// Timeout : 0 = 2 min (le premier appel peut inclure le chargement du
	// modèle côté serveur).
	Timeout    time.Duration
	HTTPClient *http.Client
}

// Transcribe envoie wav et retourne le texte reconnu.
func (s STT) Transcribe(ctx context.Context, wav []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "voice.wav")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(wav); err != nil {
		return "", err
	}
	fields := map[string]string{"model": s.Model, "language": s.Language, "response_format": "json"}
	for k, v := range fields {
		if v == "" {
			continue
		}
		if err := mw.WriteField(k, v); err != nil {
			return "", err
		}
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := strings.TrimRight(s.BaseURL, "/") + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if s.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
	}
	hc := s.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("requête STT : %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("lecture de la réponse STT : %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("STT : statut %d : %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("réponse STT illisible : %w (%s)", err, strings.TrimSpace(string(raw)))
	}
	return strings.TrimSpace(parsed.Text), nil
}

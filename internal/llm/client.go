// Package llm est un client minimal pour une API compatible OpenAI
// (endpoint /v1/chat/completions), sans dépendance externe.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Message struct {
	Role       string     `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // pour role="tool" : lie le résultat à l'appel

	// ReasoningContent : raisonnement du modèle avant sa réponse/ses
	// tool_calls (convention "reasoning_content" exposée par llama.cpp/vLLM
	// pour les modèles à raisonnement, ex: DeepSeek-R1, QwQ, Qwen en mode
	// "thinking"). Jamais envoyé dans une requête (un tour précédent ne
	// renvoie pas son propre raisonnement au modèle) : uniquement lu depuis
	// une réponse, pour affichage — voir agent.EventReasoning.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// Tool décrit une fonction proposée au LLM (format OpenAI "function tool").
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON schema
}

// ToolCall est une invocation de fonction demandée par le LLM dans sa réponse.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // objet JSON sérialisé en chaîne
}

// Usage donne le nombre de tokens réellement comptés par le serveur pour un
// échange (issu du champ "usage" de la réponse OpenAI-compatible).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type Client struct {
	BaseURL    string // ex: http://localhost:8080/v1
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// DefaultTimeout : durée maximale d'une requête complète au LLM (voir
// config.Config.LLMTimeout). Large, car en mode non-streamé le serveur
// (llama.cpp, vLLM...) n'envoie ses en-têtes qu'une fois toute la réponse
// générée : sur une machine peu puissante, traitement du prompt + génération
// dépassent facilement quelques minutes.
const DefaultTimeout = 10 * time.Minute

func New(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTPClient: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Tools         []Tool         `json:"tools,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ChatCompletion envoie l'historique de messages (et, si fourni, la liste de
// tools proposés) et retourne le message assistant complet (non-streamé,
// avec ses éventuels tool_calls) ainsi que l'usage de tokens.
func (c *Client) ChatCompletion(ctx context.Context, messages []Message, tools []Tool) (Message, Usage, error) {
	reqBody, err := json.Marshal(chatRequest{
		Model:    c.Model,
		Messages: messages,
		Stream:   false,
		Tools:    tools,
	})
	if err != nil {
		return Message{}, Usage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return Message{}, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return Message{}, Usage{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Message{}, Usage{}, err
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Message{}, Usage{}, fmt.Errorf("réponse LLM invalide (status %d): %s", resp.StatusCode, string(body))
	}
	if parsed.Error != nil {
		return Message{}, Usage{}, fmt.Errorf("erreur LLM: %s", parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return Message{}, Usage{}, fmt.Errorf("LLM a répondu avec le status %d: %s", resp.StatusCode, string(body))
	}
	if len(parsed.Choices) == 0 {
		return Message{}, Usage{}, fmt.Errorf("réponse LLM sans choix: %s", string(body))
	}

	var usage Usage
	if parsed.Usage != nil {
		usage = *parsed.Usage
	}

	return parsed.Choices[0].Message, usage, nil
}

// ChatCompletionStream envoie l'historique et appelle onDelta pour chaque
// fragment de texte reçu (format SSE "data: {...}"). Retourne le texte
// complet et l'usage de tokens rapporté par le serveur (demandé via
// stream_options.include_usage, supporté par les serveurs llama.cpp/vLLM/etc.
// compatibles OpenAI récents).
func (c *Client) ChatCompletionStream(ctx context.Context, messages []Message, onDelta func(string)) (string, Usage, error) {
	reqBody, err := json.Marshal(chatRequest{
		Model:         c.Model,
		Messages:      messages,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	})
	if err != nil {
		return "", Usage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", Usage{}, fmt.Errorf("LLM a répondu avec le status %d: %s", resp.StatusCode, string(body))
	}

	var full strings.Builder
	var usage Usage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		if data == "" {
			continue
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // fragment ignoré, ne casse pas le stream
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		if delta == "" {
			continue
		}
		full.WriteString(delta)
		if onDelta != nil {
			onDelta(delta)
		}
	}
	if err := scanner.Err(); err != nil {
		return full.String(), usage, err
	}

	return full.String(), usage, nil
}

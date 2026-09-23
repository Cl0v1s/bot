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

	// ContextTokens : taille de contexte demandée lors d'un chargement
	// automatique du modèle (voir tryLoadModel) — 0 = laisse le serveur
	// choisir sa propre valeur par défaut au chargement, qui peut être
	// nettement plus petite que le contexte natif du modèle (vu en
	// pratique) : à régler explicitement (ex: depuis
	// config.Config.ContextMaxTokens) pour obtenir le contexte réellement
	// voulu plutôt qu'une valeur arbitraire choisie par le serveur.
	ContextTokens int
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

// modelNotLoadedSubstring identifie (sous-chaîne insensible à la casse, dans
// le texte final de l'erreur remontée par chatCompletionOnce/
// chatCompletionStreamOnce) l'erreur "aucun modèle chargé" que renvoient
// certains serveurs d'inférence locaux à backend paresseux (ex: Unsloth
// Studio — message d'origine observé : "No model loaded. Call POST
// /inference/load first.") quand aucun modèle n'est en mémoire au moment de
// la requête. Sur un tel serveur, ChatCompletion/ChatCompletionStream
// tentent alors un chargement automatique (voir tryLoadModel) puis rejouent
// la requête UNE fois, plutôt que de faire échouer tout de suite un mail ou
// un tour de chat pour une simple absence de modèle chargé — sans effet sur
// un serveur qui ne renvoie jamais ce message précis (ex: llama.cpp/vLLM,
// où un modèle est chargé une fois pour toutes au démarrage) : le
// comportement reste alors identique à avant, l'erreur d'origine remontée
// telle quelle.
const modelNotLoadedSubstring = "no model loaded"

// tryLoadModel demande au serveur d'inférence de charger c.Model, via
// l'endpoint propriétaire POST {BaseURL}/load (vu sur Unsloth Studio — non
// standardisé OpenAI, mais servi sous la même base d'URL que
// /chat/completions). c.Model peut porter un suffixe ":quant" (ex:
// "org/modèle:UD-Q4_K_XL", convention déjà utilisée telle quelle comme
// "model" dans les requêtes de complétion, et affichée sous cette forme par
// GET {BaseURL}/models, où "id" et "quant" apparaissent comme deux champs
// séparés) : il faut le scinder avant d'appeler /load, qui refuse un
// "model_path" contenant ":" (vérifié empiriquement : il tente alors de le
// résoudre comme un dépôt HuggingFace, pour lequel ':' est un caractère
// invalide, et échoue). "max_seq_length" n'est transmis que si
// ContextTokens est configuré (voir son commentaire) : l'omettre ne
// "laisse rien inchangé", ça fait choisir au serveur une valeur par défaut
// (vu en pratique : nettement plus petite que le contexte natif du modèle).
func (c *Client) tryLoadModel(ctx context.Context) error {
	modelPath, variant, _ := strings.Cut(c.Model, ":")

	reqBody, err := json.Marshal(struct {
		ModelPath    string `json:"model_path"`
		GGUFVariant  string `json:"gguf_variant,omitempty"`
		MaxSeqLength int    `json:"max_seq_length,omitempty"`
	}{
		ModelPath:    modelPath,
		GGUFVariant:  variant,
		MaxSeqLength: c.ContextTokens,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/load", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("chargement automatique du modèle %q: %w", c.Model, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("chargement automatique du modèle %q refusé (status %d): %s", c.Model, resp.StatusCode, string(body))
	}
	return nil
}

// ChatCompletion envoie l'historique de messages (et, si fourni, la liste de
// tools proposés) et retourne le message assistant complet (non-streamé,
// avec ses éventuels tool_calls) ainsi que l'usage de tokens — avec une
// tentative de chargement automatique du modèle puis une seule relance si le
// serveur répond "aucun modèle chargé" (voir modelNotLoadedSubstring).
func (c *Client) ChatCompletion(ctx context.Context, messages []Message, tools []Tool) (Message, Usage, error) {
	msg, usage, err := c.chatCompletionOnce(ctx, messages, tools)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), modelNotLoadedSubstring) {
		if loadErr := c.tryLoadModel(ctx); loadErr == nil {
			return c.chatCompletionOnce(ctx, messages, tools)
		}
		// Le chargement automatique a lui-même échoué (serveur sans cet
		// endpoint, modèle introuvable...) : on retombe sur l'erreur
		// d'origine, plus parlante pour un serveur qui ne supporte de toute
		// façon pas ce mécanisme, plutôt que de la masquer derrière l'échec
		// du chargement.
	}
	return msg, usage, err
}

func (c *Client) chatCompletionOnce(ctx context.Context, messages []Message, tools []Tool) (Message, Usage, error) {
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
// compatibles OpenAI récents) — avec la même tentative de chargement
// automatique + relance unique que ChatCompletion (voir
// modelNotLoadedSubstring) si le serveur répond "aucun modèle chargé".
//
// Si le premier essai a déjà appelé onDelta avant d'échouer (peu probable
// pour cette erreur précise, qui survient avant tout token produit, mais pas
// structurellement impossible), la relance rappelle onDelta depuis le début
// : un onDelta idempotent-à-l'affichage (ex: impression directe sur la
// sortie, comme le fait le mode chat) afficherait alors un double texte
// partiel — cas non observé en pratique pour cette erreur précise, donc pas
// traité spécifiquement ici.
func (c *Client) ChatCompletionStream(ctx context.Context, messages []Message, onDelta func(string)) (string, Usage, error) {
	full, usage, err := c.chatCompletionStreamOnce(ctx, messages, onDelta)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), modelNotLoadedSubstring) {
		if loadErr := c.tryLoadModel(ctx); loadErr == nil {
			return c.chatCompletionStreamOnce(ctx, messages, onDelta)
		}
	}
	return full, usage, err
}

func (c *Client) chatCompletionStreamOnce(ctx context.Context, messages []Message, onDelta func(string)) (string, Usage, error) {
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

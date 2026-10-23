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
	"os"
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

	// FinishReason : raison de fin de génération rapportée par le serveur
	// ("stop", "tool_calls", "length"...). Jamais sérialisé : renseigné par
	// le client à la lecture d'une réponse. FinishReasonLength signale une
	// réponse coupée par la limite max_tokens (voir Client.MaxTokens).
	FinishReason string `json:"-"`
}

// FinishReasonLength : finish_reason d'une génération interrompue par la
// limite de tokens de sortie (max_tokens) plutôt que terminée normalement.
const FinishReasonLength = "length"

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

	// Parallel : nombre de slots de décodage parallèles demandé lors d'un
	// chargement automatique (champ "n_parallel" de /load). 0 = non transmis :
	// le serveur applique alors son propre défaut (vu sur Unsloth Studio : 4
	// slots, qui se partagent le contexte), et non le réglage fait dans son
	// interface, qui n'est pas conservé d'un chargement à l'autre.
	Parallel int

	// DisableVision : charge le modèle sans son projecteur multimodal (champ
	// "disable_vision" de /load) — le bot n'envoie jamais d'image, et le
	// projecteur (mmproj) occupe de la mémoire pour rien.
	DisableVision bool

	// MaxTokens : nombre maximal de tokens générés par réponse (champ
	// "max_tokens" de la requête, raisonnement compris sur les serveurs qui
	// le comptent — llama.cpp notamment). 0 = non transmis : le serveur
	// applique alors sa propre limite, souvent toute la fenêtre de contexte
	// (vu avec Unsloth Studio/llama.cpp : n_predict = 65000) — un modèle qui
	// part en boucle dans son raisonnement peut alors générer pendant des
	// dizaines de minutes avant de rendre la main.
	MaxTokens int

	// EnableThinking : si non nil, envoie chat_template_kwargs.enable_thinking
	// (Qwen3.x ne raisonne pas sans). nil = comportement par défaut du serveur.
	EnableThinking *bool

	// ReasoningEffort : "low", "medium" ou "high" — envoyé en
	// reasoning_effort (API OpenAI) et chat_template_kwargs.reasoning_effort
	// (gpt-oss et templates à niveaux). Les Qwen3.x n'ont PAS de niveaux
	// (seulement enable_thinking) : le champ y est ignoré par le template.
	// "" = non envoyé.
	ReasoningEffort string

	// MaxRetries / RetryBackoff : relances automatiques d'une requête après
	// une erreur transitoire — connexion au serveur impossible ou coupée,
	// flux interrompu en cours de génération (EOF inattendu), status
	// 502/503/504 — et délai avant la première, doublé ensuite (voir
	// withRetry). MaxRetries = 0 désactive les relances.
	MaxRetries   int
	RetryBackoff time.Duration
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
		MaxRetries:   DefaultMaxRetries,
		RetryBackoff: DefaultRetryBackoff,
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
	MaxTokens     int            `json:"max_tokens,omitempty"`
	// ChatTemplateKwargs : enable_thinking pour les Qwen3.x (llama.cpp /
	// Unsloth). Omis quand Client.EnableThinking est nil (défaut serveur).
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	ReasoningEffort    string         `json:"reasoning_effort,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
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
		ModelPath     string `json:"model_path"`
		GGUFVariant   string `json:"gguf_variant,omitempty"`
		MaxSeqLength  int    `json:"max_seq_length,omitempty"`
		NParallel     int    `json:"n_parallel,omitempty"`
		DisableVision bool   `json:"disable_vision,omitempty"`
	}{
		ModelPath:     modelPath,
		GGUFVariant:   variant,
		MaxSeqLength:  c.ContextTokens,
		NParallel:     c.Parallel,
		DisableVision: c.DisableVision,
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
// serveur répond "aucun modèle chargé" (voir modelNotLoadedSubstring), et
// des relances automatiques sur erreur transitoire (voir withRetry).
func (c *Client) ChatCompletion(ctx context.Context, messages []Message, tools []Tool) (Message, Usage, error) {
	return c.withRetry(ctx, func() (Message, Usage, error) {
		return c.chatCompletionWithAutoLoad(ctx, messages, tools)
	})
}

func (c *Client) chatCompletionWithAutoLoad(ctx context.Context, messages []Message, tools []Tool) (Message, Usage, error) {
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
		Model:     c.Model,
		Messages:  messages,
		Stream:    false,
		Tools:     tools,
		MaxTokens: c.MaxTokens,

		ChatTemplateKwargs: c.templateKwargs(),
		ReasoningEffort:    c.ReasoningEffort,
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
	if isUnavailableStatus(resp.StatusCode) {
		return Message{}, Usage{}, &statusError{status: resp.StatusCode, body: string(body)}
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Message{}, Usage{}, fmt.Errorf("réponse LLM invalide (status %d): %s", resp.StatusCode, string(body))
	}
	if parsed.Error != nil {
		return Message{}, Usage{}, &serverError{message: parsed.Error.Message}
	}
	if resp.StatusCode != http.StatusOK {
		return Message{}, Usage{}, &statusError{status: resp.StatusCode, body: string(body)}
	}
	if len(parsed.Choices) == 0 {
		return Message{}, Usage{}, fmt.Errorf("réponse LLM sans choix: %s", string(body))
	}

	var usage Usage
	if parsed.Usage != nil {
		usage = *parsed.Usage
	}

	msg := parsed.Choices[0].Message
	msg.FinishReason = parsed.Choices[0].FinishReason
	return msg, usage, nil
}

// StreamCallbacks : fonctions appelées au fil du streaming (voir
// ChatCompletionStream), toutes optionnelles. OnContent reçoit chaque
// fragment du texte de réponse, OnReasoning chaque fragment de raisonnement
// ("reasoning_content", voir Message.ReasoningContent). Les tool_calls ne
// sont pas transmis fragment par fragment : ils ne sont exploitables qu'une
// fois leurs arguments complets, et sont rendus dans le Message final.
type StreamCallbacks struct {
	OnContent   func(string)
	OnReasoning func(string)
}

// ChatCompletionStream envoie l'historique (et, si fourni, la liste de tools
// proposés) en mode streaming (format SSE "data: {...}"), appelle cb au fil
// des fragments reçus, et retourne le message assistant complet reconstitué
// (texte, raisonnement, tool_calls, finish_reason) et l'usage de tokens
// rapporté par le serveur (demandé via stream_options.include_usage,
// supporté par les serveurs llama.cpp/vLLM/etc. compatibles OpenAI récents)
// — avec la même tentative de chargement automatique + relance unique que
// ChatCompletion (voir modelNotLoadedSubstring) si le serveur répond "aucun
// modèle chargé".
//
// Les erreurs transitoires (connexion impossible ou coupée, flux interrompu
// en pleine génération, status 502/503/504) sont elles aussi relancées
// (voir withRetry). La génération interrompue ne pouvant pas être reprise
// là où elle s'était arrêtée, une relance rappelle cb depuis le début de la
// nouvelle réponse : l'appelant qui affiche les fragments au fil de l'eau
// est prévenu avant chaque relance via WithRetryNotifier, pour pouvoir
// signaler que le texte partiel déjà affiché est abandonné.
func (c *Client) ChatCompletionStream(ctx context.Context, messages []Message, tools []Tool, cb StreamCallbacks) (Message, Usage, error) {
	return c.withRetry(ctx, func() (Message, Usage, error) {
		return c.chatCompletionStreamWithAutoLoad(ctx, messages, tools, cb)
	})
}

func (c *Client) chatCompletionStreamWithAutoLoad(ctx context.Context, messages []Message, tools []Tool, cb StreamCallbacks) (Message, Usage, error) {
	msg, usage, err := c.chatCompletionStreamOnce(ctx, messages, tools, cb)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), modelNotLoadedSubstring) {
		if loadErr := c.tryLoadModel(ctx); loadErr == nil {
			return c.chatCompletionStreamOnce(ctx, messages, tools, cb)
		}
	}
	return msg, usage, err
}

// streamToolCallDelta : fragment d'un tool_call en streaming (format
// OpenAI). Index identifie l'appel auquel le fragment appartient (plusieurs
// appels possibles dans une même réponse) ; id, type et nom n'arrivent
// normalement que dans le premier fragment, les arguments sont concaténés
// au fil des fragments suivants.
type streamToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (c *Client) chatCompletionStreamOnce(ctx context.Context, messages []Message, tools []Tool, cb StreamCallbacks) (Message, Usage, error) {
	reqBody, err := json.Marshal(chatRequest{
		Model:         c.Model,
		Messages:      messages,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Tools:         tools,
		MaxTokens:     c.MaxTokens,

		ChatTemplateKwargs: c.templateKwargs(),
		ReasoningEffort:    c.ReasoningEffort,
	})
	if err != nil {
		return Message{}, Usage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return Message{}, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return Message{}, Usage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return Message{}, Usage{}, &statusError{status: resp.StatusCode, body: string(body)}
	}

	var content, reasoning strings.Builder
	var tags thinkSplitter
	var toolCalls []ToolCall
	var finishReason string
	var usage Usage
	done := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		if data == "" {
			continue
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string                `json:"content"`
					ReasoningContent string                `json:"reasoning_content"`
					Reasoning        string                `json:"reasoning"` // alias (Ollama, llama.cpp/vLLM récents)
					ToolCalls        []streamToolCallDelta `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // fragment ignoré, ne casse pas le stream
		}
		if chunk.Error != nil {
			return Message{}, usage, &serverError{message: chunk.Error.Message}
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
		emitReasoning := func(d string) {
			if d == "" {
				return
			}
			reasoning.WriteString(d)
			if cb.OnReasoning != nil {
				cb.OnReasoning(d)
			}
		}
		emitContent := func(d string) {
			if d == "" {
				return
			}
			content.WriteString(d)
			if cb.OnContent != nil {
				cb.OnContent(d)
			}
		}
		emitReasoning(choice.Delta.ReasoningContent)
		emitReasoning(choice.Delta.Reasoning)
		// Qwen & co. : raisonnement parfois inline dans content, entre
		// <think>…</think> (serveur sans reasoning parser).
		tags.feed(choice.Delta.Content, emitReasoning, emitContent)
		for _, tc := range choice.Delta.ToolCalls {
			if tc.Index < 0 {
				continue
			}
			for len(toolCalls) <= tc.Index {
				toolCalls = append(toolCalls, ToolCall{Type: "function"})
			}
			call := &toolCalls[tc.Index]
			if tc.ID != "" {
				call.ID = tc.ID
			}
			if tc.Type != "" {
				call.Type = tc.Type
			}
			call.Function.Name += tc.Function.Name
			call.Function.Arguments += tc.Function.Arguments
		}
	}

	tags.flush(func(d string) {
		reasoning.WriteString(d)
		if cb.OnReasoning != nil {
			cb.OnReasoning(d)
		}
	}, func(d string) {
		content.WriteString(d)
		if cb.OnContent != nil {
			cb.OnContent(d)
		}
	})
	msg := Message{
		Role:             "assistant",
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		ToolCalls:        toolCalls,
		FinishReason:     finishReason,
	}
	if err := scanner.Err(); err != nil {
		return msg, usage, err
	}
	if !done && finishReason == "" {
		return msg, usage, errStreamTruncated
	}
	return msg, usage, nil
}

// thinkSplitter sépare un flux de texte en raisonnement (<think>…</think>) et
// réponse. Les balises peuvent être coupées entre deux fragments : un suffixe
// pouvant être le début d'une balise est retenu jusqu'au fragment suivant.
type thinkSplitter struct {
	inThink bool
	pending string
}

func (t *thinkSplitter) feed(chunk string, reason, content func(string)) {
	if chunk == "" {
		return
	}
	buf := t.pending + chunk
	t.pending = ""
	for buf != "" {
		tag := "<think>"
		if t.inThink {
			tag = "</think>"
		}
		if i := strings.Index(buf, tag); i >= 0 {
			t.out(buf[:i], reason, content)
			buf = buf[i+len(tag):]
			t.inThink = !t.inThink
			continue
		}
		// retient le plus long suffixe qui est un préfixe de la balise
		keep := 0
		for n := min(len(tag)-1, len(buf)); n > 0; n-- {
			if strings.HasSuffix(buf, tag[:n]) {
				keep = n
				break
			}
		}
		t.out(buf[:len(buf)-keep], reason, content)
		t.pending = buf[len(buf)-keep:]
		return
	}
}

func (t *thinkSplitter) out(d string, reason, content func(string)) {
	if d == "" {
		return
	}
	if t.inThink {
		reason(d)
	} else {
		content(d)
	}
}

func (t *thinkSplitter) flush(reason, content func(string)) {
	if t.pending != "" {
		t.out(t.pending, reason, content)
		t.pending = ""
	}
}

func (c *Client) templateKwargs() map[string]any {
	kw := map[string]any{}
	if c.EnableThinking != nil {
		kw["enable_thinking"] = *c.EnableThinking
	}
	if c.ReasoningEffort != "" {
		kw["reasoning_effort"] = c.ReasoningEffort
	}
	if len(kw) == 0 {
		return nil
	}
	return kw
}

// ParseReasoningEffort normalise un niveau de réflexion ("low", "medium",
// "high" ; "" ou "auto" = aucun).
func ParseReasoningEffort(s string) (string, bool) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "", "auto":
		return "", true
	case "low", "medium", "high":
		return v, true
	}
	return "", false
}

// TryUnloadModel demande au serveur d'inférence de décharger le modèle, via
// l'endpoint propriétaire POST {BaseURL}/unload (vu sur Unsloth Studio — non
// standardisé OpenAI). Retourne une erreur si le serveur ne supporte pas ce
// mécanisme ou si la requête échoue. Sur un serveur qui charge les modèles
// au démarrage (llama.cpp/vLLM sans Unsloth), cette méthode est généralement
// ignorée : aucun modèle n'est déchargé, mais non plus d'erreur retournée.
func (c *Client) TryUnloadModel(ctx context.Context) error {
	reqBody, err := json.Marshal(struct {
		ModelPath string `json:"model_path"`
	}{
		ModelPath: c.Model,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/unload", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("déchargement du modèle %q: %w", c.Model, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("déchargement du modèle %q refusé (status %d): %s", c.Model, resp.StatusCode, string(body))
	}
	fmt.Fprintln(os.Stderr, "[modèle déchargé]")
	return nil
}

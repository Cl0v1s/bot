// Package convo gère l'historique d'une conversation avec le LLM, y compris
// l'estimation de la taille du contexte et sa compaction automatique quand
// un seuil (par défaut 90%) de la fenêtre de contexte du modèle est atteint.
package convo

import (
	"context"
	"fmt"

	"bot/internal/llm"
)

type Conversation struct {
	SystemPrompt string
	Messages     []llm.Message // n'inclut pas le system prompt

	MaxContextTokens int     // taille de contexte du modèle, en tokens (approx.)
	CompactAt        float64 // fraction (0-1) du contexte déclenchant la compaction
	KeepLast         int     // nombre de messages récents conservés tels quels lors d'une compaction

	// LastKnownTokens/LastKnownMessageCount mémorisent le dernier usage réel
	// (champ "usage" de l'API) reçu du LLM, et le nombre de messages à ce
	// moment-là. Permet d'estimer le contexte actuel à partir d'un compte
	// exact plutôt que d'une pure heuristique. Remis à zéro après compaction.
	LastKnownTokens       int
	LastKnownMessageCount int
}

func New(systemPrompt string, maxContextTokens int, compactAt float64, keepLast int) *Conversation {
	return &Conversation{
		SystemPrompt:     systemPrompt,
		MaxContextTokens: maxContextTokens,
		CompactAt:        compactAt,
		KeepLast:         keepLast,
	}
}

func (c *Conversation) AddUser(content string) {
	c.Messages = append(c.Messages, llm.Message{Role: "user", Content: content})
}

func (c *Conversation) AddAssistant(content string) {
	c.Messages = append(c.Messages, llm.Message{Role: "assistant", Content: content})
}

// AppendRaw ajoute un message tel quel (utilisé par la boucle d'agent pour
// les messages assistant porteurs de tool_calls et les messages role="tool"
// contenant les résultats d'exécution).
func (c *Conversation) AppendRaw(m llm.Message) {
	c.Messages = append(c.Messages, m)
}

func (c *Conversation) Reset() {
	c.Messages = nil
	c.LastKnownTokens = 0
	c.LastKnownMessageCount = 0
}

// RecordUsage enregistre l'usage réel (champ "usage" de la réponse API) du
// dernier échange, pour affiner les futures estimations de contexte. À
// appeler juste après AddAssistant avec la réponse correspondante.
func (c *Conversation) RecordUsage(u llm.Usage) {
	if u.TotalTokens <= 0 {
		return
	}
	c.LastKnownTokens = u.TotalTokens
	c.LastKnownMessageCount = len(c.Messages)
}

// Full retourne l'historique complet à envoyer au LLM, system prompt en tête.
func (c *Conversation) Full() []llm.Message {
	full := make([]llm.Message, 0, len(c.Messages)+1)
	if c.SystemPrompt != "" {
		full = append(full, llm.Message{Role: "system", Content: c.SystemPrompt})
	}
	full = append(full, c.Messages...)
	return full
}

// estimateTokens approxime le nombre de tokens d'un texte (~4 caractères/token
// pour du français/anglais courant), plus un léger overhead par message pour
// les séparateurs de rôle.
func estimateTokens(s string) int {
	return (len(s)+3)/4 + 4
}

// EstimateTokens donne la meilleure estimation disponible du nombre de
// tokens de la conversation complète (system prompt inclus). Si un usage
// réel a été enregistré via RecordUsage, il sert de base exacte à laquelle
// on ajoute seulement l'estimation heuristique des messages ajoutés depuis
// (typiquement le tout dernier message utilisateur, pas encore envoyé au
// LLM). Sinon, tout est estimé par heuristique.
func (c *Conversation) EstimateTokens() int {
	if c.LastKnownTokens > 0 && c.LastKnownMessageCount <= len(c.Messages) {
		total := c.LastKnownTokens
		for _, m := range c.Messages[c.LastKnownMessageCount:] {
			total += estimateTokens(m.Content)
		}
		return total
	}

	total := 0
	if c.SystemPrompt != "" {
		total += estimateTokens(c.SystemPrompt)
	}
	for _, m := range c.Messages {
		total += estimateTokens(m.Content)
	}
	return total
}

// TokensAreExact indique si EstimateTokens() retourne actuellement un compte
// basé sur un usage réel de l'API (sans estimation heuristique ajoutée).
func (c *Conversation) TokensAreExact() bool {
	return c.LastKnownTokens > 0 && c.LastKnownMessageCount == len(c.Messages)
}

// NeedsCompaction indique si le contexte estimé a atteint le seuil de compaction.
func (c *Conversation) NeedsCompaction() bool {
	if c.MaxContextTokens <= 0 {
		return false
	}
	threshold := int(float64(c.MaxContextTokens) * c.CompactAt)
	return c.EstimateTokens() >= threshold
}

const summarizeSystemPrompt = `Tu vas résumer le début d'une conversation entre un utilisateur et un assistant.
Produis un résumé concis (quelques phrases ou une liste à puces) qui conserve :
- les faits et informations importantes échangés
- les décisions prises
- les préférences ou contraintes exprimées par l'utilisateur
- toute tâche en cours non terminée
Ne dis pas "voici un résumé", ne commente pas la demande : donne directement le contenu du résumé.`

// Compact résume via le LLM les messages les plus anciens et les remplace
// par un unique message contenant le résumé, en conservant tels quels les
// KeepLast derniers messages. Ne fait rien si la conversation est trop
// courte pour être compactée.
func (c *Conversation) Compact(ctx context.Context, client *llm.Client) (bool, error) {
	keep := c.KeepLast
	if keep < 0 {
		keep = 0
	}
	if len(c.Messages) <= keep {
		return false, nil
	}

	toSummarize := c.Messages[:len(c.Messages)-keep]
	kept := append([]llm.Message(nil), c.Messages[len(c.Messages)-keep:]...)

	summarizeReq := make([]llm.Message, 0, len(toSummarize)+1)
	summarizeReq = append(summarizeReq, llm.Message{Role: "system", Content: summarizeSystemPrompt})
	summarizeReq = append(summarizeReq, toSummarize...)

	// L'usage renvoyé ici correspond au prompt de résumé, pas à la
	// conversation réelle : on ne l'enregistre pas via RecordUsage.
	summaryMsg, _, err := client.ChatCompletion(ctx, summarizeReq, nil)
	if err != nil {
		return false, fmt.Errorf("compaction du contexte: %w", err)
	}

	compacted := make([]llm.Message, 0, len(kept)+1)
	compacted = append(compacted, llm.Message{
		Role:    "system",
		Content: "Résumé de la conversation précédente (contexte compacté) :\n" + summaryMsg.Content,
	})
	compacted = append(compacted, kept...)
	c.Messages = compacted

	// Le compte exact précédent ne correspond plus à ce nouvel historique :
	// on retombe sur l'heuristique jusqu'au prochain appel réel.
	c.LastKnownTokens = 0
	c.LastKnownMessageCount = 0

	return true, nil
}

// CompactIfNeeded compacte le contexte si le seuil est atteint. Retourne true
// si une compaction a effectivement eu lieu.
func (c *Conversation) CompactIfNeeded(ctx context.Context, client *llm.Client) (bool, error) {
	if !c.NeedsCompaction() {
		return false, nil
	}
	return c.Compact(ctx, client)
}

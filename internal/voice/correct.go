package voice

import (
	"context"
	"strings"

	"bot/internal/llm"
)

// Correct retournait auparavant la transcription corrigée par le LLM ; cette
// correction est désactivée (jugée inutile) : text est renvoyé tel quel.
func Correct(ctx context.Context, client *llm.Client, recent, text string) string {
	return text
}

// RecentContext résume les derniers échanges (texte des messages user et
// assistant uniquement, tronqués) pour Correct.
func RecentContext(msgs []llm.Message, maxMsgs, maxChars int) string {
	var picked []string
	for i := len(msgs) - 1; i >= 0 && len(picked) < maxMsgs; i-- {
		m := msgs[i]
		if (m.Role != "user" && m.Role != "assistant") || strings.TrimSpace(m.Content) == "" {
			continue
		}
		c := strings.TrimSpace(m.Content)
		if r := []rune(c); len(r) > maxChars {
			c = string(r[:maxChars]) + "…"
		}
		role := "Utilisateur"
		if m.Role == "assistant" {
			role = "Assistant"
		}
		picked = append(picked, role+" : "+c)
	}
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return strings.Join(picked, "\n")
}

package voice

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"bot/internal/llm"
)

// correctTimeout borne l'appel de correction : au-delà (serveur occupé par
// un tour en cours, par exemple), on garde la transcription brute plutôt que
// de retarder le message.
const correctTimeout = 30 * time.Second

const correctSystemPrompt = `Tu corriges la sortie d'un modèle de reconnaissance vocale (speech-to-text). Ce modèle n'a aucun contexte sur la conversation : il se trompe sur les homophones, les noms propres, les termes techniques, les noms de fichiers/commandes/outils, et peut mal ponctuer.

On te donne un extrait récent de la conversation (pour le contexte seulement) puis la transcription brute. Réponds UNIQUEMENT par la transcription corrigée, sans guillemets, sans commentaire.

Règles :
- Ne corrige que les erreurs probables de reconnaissance ; conserve le sens, la langue, le ton et la formulation de l'utilisateur.
- N'ajoute rien, ne réponds pas à la demande, ne l'exécute pas, ne la reformule pas.
- Si la transcription te paraît correcte, recopie-la telle quelle.`

// Correct demande au LLM de corriger les erreurs de reconnaissance de text à
// l'aide du contexte récent de la conversation. Best-effort : en cas d'erreur,
// de dépassement de délai ou de réponse suspecte (vide, multiligne, beaucoup
// plus longue que l'original — signe que le modèle a répondu au lieu de
// corriger), retourne text inchangé.
func Correct(ctx context.Context, client *llm.Client, recent, text string) string {
	ctx, cancel := context.WithTimeout(ctx, correctTimeout)
	defer cancel()

	var user strings.Builder
	if recent != "" {
		user.WriteString("Contexte récent :\n")
		user.WriteString(recent)
		user.WriteString("\n\n")
	}
	user.WriteString("Transcription brute :\n")
	user.WriteString(text)

	msg, _, err := client.ChatCompletion(ctx, []llm.Message{
		{Role: "system", Content: correctSystemPrompt},
		{Role: "user", Content: user.String()},
	}, nil)
	if err != nil {
		return text
	}
	out := strings.Trim(strings.TrimSpace(msg.Content), "\"«»")
	out = strings.TrimSpace(out)
	if out == "" || strings.Contains(out, "\n") && !strings.Contains(text, "\n") {
		return text
	}
	if utf8.RuneCountInString(out) > 2*utf8.RuneCountInString(text)+40 {
		return text
	}
	return out
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

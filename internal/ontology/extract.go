package ontology

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"bot/internal/llm"
)

const (
	// maxTranscriptChars borne la taille de la conversation soumise à
	// l'extraction (on garde la fin, la plus récente).
	maxTranscriptChars = 24000
	maxMessageChars    = 2000
	knownNamesInPrompt = 80
)

const extractionSystemPrompt = `Tu es un module d'extraction de connaissances. À partir d'un extrait de conversation entre un utilisateur et un assistant, tu identifies les CONCEPTS durables et les LIENS entre eux, pour alimenter un graphe de connaissances.

Concepts à retenir : personnes et rôles, organisations, projets, outils/logiciels/serveurs, technologies, méthodes, lieux, décisions, préférences de l'utilisateur, notions de son domaine. Chacun a un nom court et canonique, un type (un mot : Personne, Organisation, Projet, Outil, Technologie, Methode, Lieu, Decision, Preference, Notion...) et une description d'une phrase au plus.

Liens : triplets source -> relation -> cible entre deux concepts, relation en verbe/groupe verbal court (ex: "appartient à", "utilise", "est une sous-catégorie de", "travaille sur", "dépend de", "héberge", "préfère"). Utilise "est une sous-catégorie de" pour les hiérarchies (React -> Framework Frontend).

Règles :
- Ne retiens que ce qui est dit ou clairement établi dans l'extrait, jamais d'invention. Ignore le bavardage, les détails éphémères (une commande ponctuelle, une erreur passagère) et les chemins de fichiers temporaires.
- Réutilise EXACTEMENT le nom d'un concept déjà connu (liste fournie) quand c'est le même, plutôt que d'en créer une variante.
- Les noms de source/cible d'une relation doivent désigner des concepts de "entities" ou des concepts déjà connus.
- Si rien ne mérite d'être retenu, renvoie des listes vides.

Réponds UNIQUEMENT par un objet JSON, sans texte autour ni bloc de code, de la forme :
{"entities":[{"name":"...","type":"...","description":"..."}],"relations":[{"source":"...","relation":"...","target":"..."}]}`

// transcript sérialise les messages utilisateur/assistant (texte
// uniquement : ni system prompt, ni appels d'outils et leurs résultats,
// bruyants et hors sujet pour un graphe de concepts).
func transcript(messages []llm.Message) string {
	var parts []string
	for _, m := range messages {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		var who string
		switch m.Role {
		case "user":
			who = "Utilisateur"
		case "assistant":
			who = "Assistant"
		default:
			continue
		}
		parts = append(parts, who+" : "+truncateRunes(text, maxMessageChars))
	}
	out := strings.Join(parts, "\n\n")
	if r := []rune(out); len(r) > maxTranscriptChars {
		out = "[...]\n" + string(r[len(r)-maxTranscriptChars:])
	}
	return out
}

// parseExtraction décode la réponse du modèle, en tolérant un bloc de code
// markdown ou du texte autour de l'objet JSON.
func parseExtraction(content string) (Extraction, error) {
	var ext Extraction
	s := strings.TrimSpace(content)
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return ext, fmt.Errorf("pas d'objet JSON dans la réponse : %q", truncateRunes(s, 200))
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &ext); err != nil {
		return ext, fmt.Errorf("JSON invalide : %w", err)
	}
	return ext, nil
}

// Since retourne les messages postérieurs à last (le dernier message déjà
// traité, ou nil). Si last n'est plus dans messages (nouvelle conversation,
// compaction qui l'a résumé), retourne tout : l'extraction étant idempotente,
// retraiter est sans danger, contrairement à sauter des messages.
func Since(messages []llm.Message, last *llm.Message) []llm.Message {
	if last == nil {
		return messages
	}
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == last.Role && m.Content == last.Content && m.ToolCallID == last.ToolCallID && len(m.ToolCalls) == len(last.ToolCalls) {
			return messages[i+1:]
		}
	}
	return messages
}

// Distiller extrait concepts et liens d'une conversation via le LLM et les
// fusionne dans Store.
type Distiller struct {
	Store  *Store
	Client *llm.Client
}

// Run analyse messages. Ne fait rien (Stats vide, pas d'appel LLM) s'il n'y
// a aucun texte à analyser.
func (d *Distiller) Run(ctx context.Context, messages []llm.Message) (Stats, error) {
	text := transcript(messages)
	if text == "" {
		return Stats{}, nil
	}

	var known string
	if names, err := d.Store.KnownNames(ctx, knownNamesInPrompt); err == nil && len(names) > 0 {
		known = "Concepts déjà connus : " + strings.Join(names, " ; ") + "\n\n"
	}
	req := []llm.Message{
		{Role: "system", Content: extractionSystemPrompt},
		{Role: "user", Content: known + "Extrait de conversation :\n\n" + text + "\n\n--- fin de l'extrait ---\n\nProduis l'objet JSON."},
	}
	msg, _, err := d.Client.ChatCompletion(ctx, req, nil)
	if err != nil {
		return Stats{}, fmt.Errorf("extraction: %w", err)
	}
	ext, err := parseExtraction(msg.Content)
	if err != nil {
		return Stats{}, fmt.Errorf("extraction: %w", err)
	}
	return d.Store.Apply(ctx, ext)
}

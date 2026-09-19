// Package agent exécute la boucle d'appel LLM + tool calls : à chaque tour,
// si le modèle demande l'exécution d'un ou plusieurs outils, ceux-ci sont
// exécutés et leurs résultats renvoyés au modèle, jusqu'à une réponse finale
// sans tool_calls (ou l'atteinte du nombre maximal d'étapes).
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"bot/internal/convo"
	"bot/internal/llm"
	"bot/internal/tools"
)

const defaultMaxSteps = 8

// ToolUsagePrompt rappelle au modèle d'essayer un outil plutôt que de
// deviner s'il y a accès ou de demander la permission lui-même en langage
// naturel : sans ce rappel, un modèle prudent tend à répondre "je n'ai pas
// accès, voulez-vous que j'essaie ?" au lieu d'appeler l'outil — qui
// indiquera de toute façon explicitement s'il manque une autorisation
// (l'utilisateur est alors sollicité automatiquement, voir
// request_directory_access).
const ToolUsagePrompt = `Des outils sont disponibles. Quand une demande peut être satisfaite par un outil (lire un fichier, exécuter une commande, faire une requête HTTP...), appelle-le directement au lieu de décrire ce que tu ferais ou de demander la permission toi-même : l'outil indiquera explicitement s'il manque une autorisation, auquel cas l'utilisateur sera sollicité automatiquement. Ne devine jamais si tu as accès à quelque chose : essaie l'outil, et adapte ta réponse au résultat réel qu'il retourne.`

// AppendToolUsagePrompt ajoute ToolUsagePrompt au system prompt de conv. À
// appeler une fois, quand le registre d'outils de la session/du message
// n'est pas vide.
func AppendToolUsagePrompt(conv *convo.Conversation) {
	conv.SystemPrompt = strings.TrimSpace(conv.SystemPrompt + "\n\n" + ToolUsagePrompt)
}

// ReflectionPrompt pousse le modèle à décomposer une demande non triviale en
// sous-questions et à chercher activement à les vérifier (via un outil s'il
// y en a un de pertinent, sinon par un raisonnement explicite) avant de
// conclure, plutôt que de répondre du tac au tac ou d'inventer un fait.
// Contrairement à ToolUsagePrompt, ne dépend pas de la présence d'outils :
// ajouté systématiquement au system prompt (voir main.go), en mode chat
// comme en mode mail.
const ReflectionPrompt = `Avant de répondre à une question non triviale ou composée de plusieurs parties, décompose-la mentalement en sous-questions, et pour chacune, identifie si tu en connais déjà la réponse avec certitude ou si tu dois la vérifier. Si un outil disponible peut lever le doute (lire un fichier, exécuter une commande, faire une requête HTTP...), utilise-le avant de conclure, plutôt que de deviner. Si aucun outil ne peut t'aider et qu'une incertitude demeure, dis-le explicitement dans ta réponse plutôt que d'affirmer comme certain un fait non vérifié.`

// EventKind distingue le début d'un appel d'outil de son résultat, pour
// permettre un affichage en deux temps (utile en mode interactif : on
// affiche l'appel avant même que le résultat soit connu).
type EventKind int

const (
	EventToolCall EventKind = iota
	EventToolResult
	// EventReasoning : commentaire du modèle avant d'appeler un ou plusieurs
	// outils (ReasoningContent s'il est fourni par le serveur, sinon Content
	// si le modèle a simplement écrit du texte avant ses tool_calls) — sans
	// ça, ce texte est silencieusement conservé dans l'historique
	// (conv.AppendRaw) mais jamais montré : seuls les tool_calls et leurs
	// résultats étaient visibles en mode interactif, pas ce qui les motive.
	EventReasoning
)

type Event struct {
	Kind   EventKind
	Tool   string
	Args   string // JSON brut des arguments
	Result string // uniquement pour EventToolResult
	Err    error  // uniquement pour EventToolResult
}

// Format retourne une représentation texte multi-lignes de l'événement,
// nettement délimitée pour rester lisible et distincte du texte de réponse
// normal du modèle. Utilisée identiquement par le mode chat (affichage
// console) et le mode mail (journalisation), pour une présentation cohérente
// entre les deux interfaces.
func (e Event) Format() string {
	var b strings.Builder
	switch e.Kind {
	case EventReasoning:
		b.WriteString("\n┄ réflexion\n")
		for _, line := range strings.Split(strings.TrimRight(truncateForDisplay(e.Result, 4000), "\n"), "\n") {
			fmt.Fprintf(&b, "┆ %s\n", line)
		}
	case EventToolCall:
		fmt.Fprintf(&b, "\n╭─ outil › %s\n", e.Tool)
		fmt.Fprintf(&b, "│ args: %s\n", compactJSON(e.Args))
		b.WriteString("╰─ exécution…\n")
	case EventToolResult:
		fmt.Fprintf(&b, "\n╭─ résultat › %s\n", e.Tool)
		if e.Err != nil {
			fmt.Fprintf(&b, "│ erreur: %v\n", e.Err)
		} else {
			for _, line := range strings.Split(strings.TrimRight(truncateForDisplay(e.Result, 4000), "\n"), "\n") {
				fmt.Fprintf(&b, "│ %s\n", line)
			}
		}
		b.WriteString("╰─\n")
	}
	return b.String()
}

func compactJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return s
	}
	return buf.String()
}

func truncateForDisplay(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n[... tronqué pour l'affichage ...]"
}

// Run exécute la boucle agentique sur la conversation conv, jusqu'à une
// réponse finale sans tool_calls. onEvent (optionnel) est appelé pour
// chaque appel/résultat d'outil, afin d'en permettre un affichage séparé du
// texte de réponse au fur et à mesure.
func Run(ctx context.Context, client *llm.Client, conv *convo.Conversation, registry *tools.Registry, maxSteps int, onEvent func(Event)) (string, llm.Usage, error) {
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	specs := registry.Specs()

	var lastUsage llm.Usage
	for step := 0; step < maxSteps; step++ {
		if _, err := conv.CompactIfNeeded(ctx, client); err != nil {
			return "", lastUsage, fmt.Errorf("compaction du contexte: %w", err)
		}

		msg, usage, err := client.ChatCompletion(ctx, conv.Full(), specs)
		if err != nil {
			return "", lastUsage, err
		}
		lastUsage = usage

		// Enregistré immédiatement après cet AppendRaw (avant d'ajouter les
		// résultats d'outils ci-dessous, dont le coût réel n'est pas encore
		// connu) : usage correspond exactement à l'historique jusqu'à ce
		// message assistant inclus. Fait à chaque étape, pas seulement à la
		// dernière (sans tool_calls) — sinon, pendant un tour à plusieurs
		// étapes d'outils, l'estimation de contexte reste basée sur l'usage
		// réel du tour précédent (potentiellement très périmé) pour toute la
		// durée de celui-ci, au moment même où le contexte se remplit le
		// plus vite.
		conv.AppendRaw(msg)
		conv.RecordUsage(usage)

		if len(msg.ToolCalls) == 0 {
			return msg.Content, usage, nil
		}

		// Le modèle a motivé ses appels d'outils (reasoning_content si le
		// serveur le distingue, sinon le Content écrit à côté des
		// tool_calls) : à afficher avant ceux-ci plutôt qu'à le laisser
		// invisible dans l'historique.
		if onEvent != nil {
			note := strings.TrimSpace(msg.ReasoningContent)
			if note == "" {
				note = strings.TrimSpace(msg.Content)
			}
			if note != "" {
				onEvent(Event{Kind: EventReasoning, Result: note})
			}
		}

		for _, tc := range msg.ToolCalls {
			if onEvent != nil {
				onEvent(Event{Kind: EventToolCall, Tool: tc.Function.Name, Args: tc.Function.Arguments})
			}

			result, callErr := registry.Call(ctx, tc.Function.Name, tc.Function.Arguments)
			if callErr != nil {
				result = "erreur: " + callErr.Error()
			}

			if onEvent != nil {
				onEvent(Event{Kind: EventToolResult, Tool: tc.Function.Name, Result: result, Err: callErr})
			}

			conv.AppendRaw(llm.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
	}

	return "", lastUsage, fmt.Errorf("nombre maximal d'étapes d'outils atteint (%d) sans réponse finale", maxSteps)
}

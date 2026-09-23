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

// WorkspacePrompt décrit l'architecture du workspace persistant du modèle
// (workspaceDir), pour qu'il sache où écrire quoi : skillsDir (déclarations
// de skills, à ne modifier que pour en ajouter une nouvelle), scratchDir
// (ses propres fichiers de travail, qu'il est censé nettoyer lui-même en fin
// de tâche), memoryFile (mémoire long terme, voir plus bas) vs. /tmp
// (fichiers vraiment éphémères, à ne jamais faire s'accumuler dans le
// workspace). Ajouté systématiquement au system prompt (voir main.go),
// indépendamment des outils, comme ReflectionPrompt : un modèle sans accès
// fichier n'en tire simplement aucun parti.
//
// memoryContent, si non vide, est le contenu de memoryFile au démarrage,
// injecté tel quel dans le prompt (voir main.go) : compter sur le modèle
// pour penser à appeler read_file dessus avant de répondre s'est révélé peu
// fiable en pratique (un modèle qui voit "MEMORY.md" dans un list_dir n'a
// pas forcément le réflexe de le lire) — l'avoir toujours sous les yeux
// supprime ce pari. Contrepartie acceptée, comme pour les skills (dont seuls
// nom/description sont chargés au démarrage) : une modification faite en
// cours de session (par le modèle ou par une compaction automatique)
// n'apparaît qu'après redémarrage, pas dans le reste de la session en cours.
func WorkspacePrompt(workspaceDir, skillsDir, scratchDir, memoryFile, memoryContent string) string {
	memorySection := fmt.Sprintf(`- %s : ta mémoire long terme, censée survivre à cette conversation (contrairement au reste, qui n'existe que le temps de la tâche en cours). Écris-y (write_file, en relisant d'abord pour compléter plutôt qu'écraser) quand l'utilisateur te demande explicitement de retenir quelque chose, ou quand tu identifies toi-même un fait/une préférence/une contrainte qui mériterait de survivre à un reset de la conversation — une compaction automatique du contexte y ajoute aussi, de son côté, ce qu'elle juge digne d'être retenu.`, memoryFile)
	if memoryContent != "" {
		memorySection += fmt.Sprintf(`
  Son contenu, tel qu'il était au démarrage de cette session, est reproduit ci-dessous : CONSULTE-LE avant de répondre à toute question sur le contexte de l'utilisateur (préférences, projets en cours, faits déjà donnés) ou avant de dire que tu ne sais pas — ne demande jamais à l'utilisateur une information qui s'y trouve déjà. S'il a pu changer depuis (une écriture plus tard dans cette session, ou une compaction automatique), relis-le directement (read_file) pour la version à jour.
  --- contenu de %s (au démarrage) ---
%s
  --- fin de %s ---`, memoryFile, memoryContent, memoryFile)
	} else {
		memorySection += " Vide pour l'instant : lis-le (read_file) si tu veux vérifier, mais rien n'y a encore été consigné."
	}

	return fmt.Sprintf(`Ton workspace persistant est %s (accessible sans demander de permission). Il contient :
- %s : les skills déclarées, à ne modifier que pour en déclarer une nouvelle (voir la skill "creer-une-skill" pour le format).
- %s : ton scratchpad, pour tes propres fichiers de travail pendant une tâche (brouillons, fichiers générés, résultats intermédiaires...). En fin de traitement d'une tâche, utilise list_dir dessus et supprime (via run_shell, si disponible) ce qui n'a plus d'utilité : ne le laisse pas s'accumuler d'une tâche à l'autre.
%s
- D'autres fichiers à la racine du workspace sont des réglages internes au harnais (permissions accordées, configuration git du sandbox...) : ne les modifie pas toi-même.

Pour un fichier vraiment éphémère (utile seulement le temps d'une commande ou d'un pipeline shell, à jeter immédiatement après, jamais à relire plus tard) : utilise /tmp plutôt que le scratchpad, lui aussi accessible sans permission — ça évite d'encombrer un espace censé rester lisible d'une tâche à l'autre.`, workspaceDir, skillsDir, scratchDir, memorySection)
}

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
	// EventWarning : problème non fatal, qui n'empêche pas le tour de
	// continuer (ex: échec de la compaction automatique du contexte — voir
	// Run) — à afficher, pas à faire échouer la réponse pour autant.
	EventWarning
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
	case EventWarning:
		fmt.Fprintf(&b, "\n[avertissement: %s]\n", e.Result)
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

// shellFailureNudge est injecté dans la conversation (voir Run,
// maxConsecutiveShellFailures) quand run_shell a échoué N fois de suite :
// pousse le modèle à changer d'approche plutôt qu'à retenter indéfiniment
// une commande qui échoue de la même façon. Rôle "user" plutôt que "system"
// (déjà utilisé une fois en tête de conversation, réinjecter ce rôle en
// cours de route n'est pas uniformément bien supporté par tous les
// gabarits de chat) — universellement traité comme une entrée à laquelle
// répondre, quel que soit le serveur/modèle.
const shellFailureNudgeTemplate = "[harnais] %d appels run_shell d'affilée ont échoué pour cette tâche. N'insiste pas avec la même commande ou la même approche : arrête-toi, explique brièvement ce qui a échoué, et choisis une méthode différente pour atteindre l'objectif — un autre outil, une autre stratégie, ou explique clairement à l'utilisateur ce qui bloque si tu ne vois pas d'autre option."

// shellCallFailed indique si le résultat d'un appel à "run_shell" doit
// compter comme un échec, pour maxConsecutiveShellFailures (voir Run).
// callErr non nil couvre les échecs "durs" du tool lui-même (permission
// refusée, commande bloquée, confirmation as_real_user refusée...) ; sinon,
// la commande a pu s'exécuter mais échouer (code de sortie non nul,
// timeout) — détecté via les marqueurs que tools.ShellTool.Call ajoute
// lui-même en fin de sortie plutôt que de les faire remonter comme une
// erreur Go (un run_shell qui s'exécute mais dont la commande échoue N'EST
// PAS une erreur du point de vue du tool : le modèle doit pouvoir lire
// cette sortie comme un résultat normal, pas un échec d'appel).
func shellCallFailed(result string, callErr error) bool {
	if callErr != nil {
		return true
	}
	return strings.Contains(result, "[commande terminée avec erreur:") ||
		strings.Contains(result, "[commande interrompue après")
}

// Run exécute la boucle agentique sur la conversation conv, jusqu'à une
// réponse finale sans tool_calls. onEvent (optionnel) est appelé pour
// chaque appel/résultat d'outil, afin d'en permettre un affichage séparé du
// texte de réponse au fur et à mesure.
//
// maxConsecutiveShellFailures : voir shellFailureNudge et
// config.Config.AgentMaxConsecutiveShellFailures. <= 0 = désactivé.
func Run(ctx context.Context, client *llm.Client, conv *convo.Conversation, registry *tools.Registry, maxSteps int, maxConsecutiveShellFailures int, onEvent func(Event)) (string, llm.Usage, error) {
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	specs := registry.Specs()

	var lastUsage llm.Usage
	// compactionFailed : dès qu'une tentative échoue dans cet appel à Run,
	// on arrête d'en retenter à chaque étape suivante (même avertissement,
	// même échec probable — pas la peine de payer un appel LLM par étape
	// pour ça) ; sert aussi à ne montrer l'avertissement qu'une seule fois.
	compactionFailed := false
	// consecutiveShellFailures : remis à zéro dès qu'un run_shell réussit,
	// ou dès que shellFailureNudgeTemplate est injecté (voir plus bas) — pas
	// un compteur cumulatif sur toute la durée de Run, seulement "combien
	// d'affilée en ce moment".
	consecutiveShellFailures := 0
	for step := 0; step < maxSteps; step++ {
		// Un échec de compaction ne doit JAMAIS faire échouer le tour : la
		// compaction est une optimisation de contexte, pas un prérequis pour
		// répondre. Un modèle qui échoue systématiquement à produire un
		// résumé propre (observé : échappe un tool_call en texte brut à
		// chaque tentative, de façon reproductible, pas un aléa isolé)
		// bloquerait sinon définitivement la conversation — chaque tour
		// retenterait la même compaction, échouerait de la même façon, sans
		// qu'aucun message ne puisse plus jamais aboutir. On continue donc
		// simplement sans compacter, avec l'historique complet tel quel.
		if !compactionFailed {
			if _, err := conv.CompactIfNeeded(ctx, client); err != nil {
				compactionFailed = true
				if onEvent != nil {
					onEvent(Event{Kind: EventWarning, Result: fmt.Sprintf("échec de la compaction du contexte, poursuite sans compacter : %v", err)})
				}
			}
		}
		// Filet de sécurité de dernier recours : si l'historique dépasse
		// encore la fenêtre réelle malgré (une tentative de) compaction —
		// notamment quand compactionFailed vient de se déclencher —,
		// supprime les plus vieux messages plutôt que d'envoyer une requête
		// vouée à être rejetée par le serveur pour dépassement de contexte.
		if dropped := conv.EnsureFitsContext(); dropped > 0 && onEvent != nil {
			onEvent(Event{Kind: EventWarning, Result: fmt.Sprintf("contexte encore trop grand après compaction : %d message(s) le plus ancien(s) supprimé(s) sans résumé", dropped)})
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

			// Évalué sur le résultat/l'erreur d'ORIGINE, avant la réécriture
			// de result juste en dessous : shellCallFailed regarde callErr
			// indépendamment, et les marqueurs qu'elle cherche dans result
			// ne peuvent de toute façon apparaître QUE dans une sortie de
			// ShellTool (jamais dans "erreur: ..."), donc l'ordre n'affecte
			// pas le résultat ici — gardé simplement dans l'ordre le plus
			// naturel à lire.
			if tc.Function.Name == "run_shell" {
				if shellCallFailed(result, callErr) {
					consecutiveShellFailures++
				} else {
					consecutiveShellFailures = 0
				}
			}

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

		// Vérifié une fois le lot de tool_calls de cette étape entièrement
		// traité, jamais au milieu (voir la boucle ci-dessus) : l'API exige
		// une réponse "tool" pour CHAQUE tool_call demandé par le même
		// message assistant avant d'accepter le tour suivant — s'arrêter en
		// cours de lot casserait ce contrat, même pour injecter ce message.
		if maxConsecutiveShellFailures > 0 && consecutiveShellFailures >= maxConsecutiveShellFailures {
			consecutiveShellFailures = 0
			nudge := fmt.Sprintf(shellFailureNudgeTemplate, maxConsecutiveShellFailures)
			conv.AddUser(nudge)
			if onEvent != nil {
				onEvent(Event{Kind: EventWarning, Result: nudge})
			}
		}
	}

	return "", lastUsage, fmt.Errorf("nombre maximal d'étapes d'outils atteint (%d) sans réponse finale", maxSteps)
}

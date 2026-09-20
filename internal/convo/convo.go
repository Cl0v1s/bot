// Package convo gère l'historique d'une conversation avec le LLM, y compris
// l'estimation de la taille du contexte et sa compaction automatique quand
// un seuil (par défaut 90%) de la fenêtre de contexte du modèle est atteint.
package convo

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

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

	// MemoryFile : chemin du fichier de mémoire long terme (MEMORY.md du
	// workspace, voir config.Config.MemoryFile) dans lequel Compact
	// persiste, si elle en trouve, les informations qui méritent de
	// survivre à cette conversation (voir persistMemory). "" = désactivé
	// (aucune tentative d'extraction).
	MemoryFile string
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

// messageEstimateTokens estime le coût en tokens d'un message complet, tool
// calls compris. Un message assistant porteur d'un appel d'outil a
// typiquement un Content vide (voir llm.Message) : sans compter aussi
// Function.Arguments, un tel message serait estimé à quasiment 0 token quel
// que soit le volume réel de ses arguments (ex: un write_file avec un gros
// contenu, ou plusieurs run_shell), sous-estimant fortement le contexte
// pendant les tours à base d'outils — précisément ceux les plus susceptibles
// de le remplir.
func messageEstimateTokens(m llm.Message) int {
	total := estimateTokens(m.Content)
	for _, tc := range m.ToolCalls {
		total += estimateTokens(tc.Function.Name) + estimateTokens(tc.Function.Arguments)
	}
	return total
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
			total += messageEstimateTokens(m)
		}
		return total
	}

	total := 0
	if c.SystemPrompt != "" {
		total += estimateTokens(c.SystemPrompt)
	}
	for _, m := range c.Messages {
		total += messageEstimateTokens(m)
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

const summarizeSystemPrompt = `Tu vas résumer le journal d'une session de travail entre un utilisateur et un assistant, pour qu'elle puisse continuer sans perdre le fil une fois ce début remplacé par ton résumé. Un résumé qui ne garde que "la tâche en cours" en sacrifiant le reste du contexte est un échec : préfère un résumé détaillé et complet à un résumé court.
Structure ta réponse en trois parties :
- Contexte : faits, informations, décisions et préférences/contraintes qui ont de la valeur pour la suite, même de détail — pas seulement les toutes dernières.
- Déjà fait : ce qui a été accompli ou tenté jusqu'ici (fichiers créés/modifiés, commandes exécutées, résultats obtenus, erreurs rencontrées...), assez précisément pour ne pas le refaire par erreur ni perdre le fil de ce qui a déjà été essayé.
- Tâche en cours : ce qu'il reste à faire, explicitement — cette partie ne doit JAMAIS être vide s'il reste quelque chose en cours, même si ça semblait évident dans les derniers messages.
Ne dis pas "voici un résumé", ne commente pas la demande : donne directement le contenu, en français.
Réponds uniquement par du texte, jamais par un appel d'outil ni par une syntaxe qui y ressemble (ex: balises <tool_call>, <function=...>), même si le journal fourni en comporte : ta seule tâche ici est de résumer ce journal, pas de continuer la session ni d'agir.`

// memoryNothingSentinel : réponse attendue de memoryExtractionSystemPrompt
// quand rien ne mérite d'être retenu à long terme — un texte vide ferait
// tout aussi bien l'affaire, mais un sentinel explicite évite de compter
// comme "mémoire" un modèle qui répondrait par un simple espace ou un
// commentaire vide de sens.
const memoryNothingSentinel = "RIEN"

// memoryExtractionSystemPrompt : appel séparé du résumé de compaction
// (summarizeSystemPrompt) — un résumé de conversation et une note de
// mémoire long terme n'ont pas le même public ni la même durée de vie (le
// premier vit et meurt avec cette conversation, la seconde est censée
// survivre à un /reset ou à une future session) ni le même format souhaité,
// les mélanger dans un seul appel aurait rendu l'un ou l'autre bâclé.
var memoryExtractionSystemPrompt = fmt.Sprintf(`Le journal d'une session de travail est en train d'être compacté (un résumé en est produit séparément, ne le duplique pas ici). Cette mémoire est partagée entre TOUTES les tâches futures, même sans aucun rapport avec celle-ci : sois très sélectif, ce n'est pas un second résumé de la session.

Ne retiens QUE ce qui resterait utile pour une tâche complètement différente, plus tard :
- préférences de travail de l'utilisateur (formats, conventions, outils préférés, façon dont il aime que tu procèdes...)
- contraintes ou règles qu'il a demandé de respecter systématiquement
- corrections qu'il t'a faites sur ton comportement
- emplacements de fichiers/dossiers récurrents (le chemin lui-même, pas leur contenu)

Ne retiens JAMAIS le contenu ou le sujet traité pendant cette session (l'histoire, les personnages, l'intrigue, les détails d'un projet ponctuel, ce qui a été fait ou produit...) : ça n'aide en rien pour une tâche différente, et alourdit cette mémoire un peu plus à chaque compaction pour rien. Le résumé de compaction s'occupe déjà de conserver le détail de LA tâche en cours.

Le cas normal est qu'il n'y a RIEN à retenir ici : dans le doute, ne retiens rien plutôt que trop.
Si rien de tel ne s'en dégage, réponds exactement %q et rien d'autre.
Sinon, réponds uniquement par une liste à puces très concise (une idée par puce, jamais plus de 3-4 puces), sans préambule ni commentaire.
Réponds uniquement par du texte, jamais par un appel d'outil ni par une syntaxe qui y ressemble (ex: balises <tool_call>, <function=...>), même si le journal fourni en comporte : ta seule tâche ici est d'extraire de ce journal, pas de continuer la session ni d'agir.`, memoryNothingSentinel)

// serializeForSummary transforme messages en un journal texte lisible, à
// donner comme DONNÉE dans un unique message "user" plutôt que de rejouer
// les vrais rôles user/assistant/tool des messages d'origine. Nécessaire en
// pratique (vérifié empiriquement) : un modèle à qui l'on rejoue la
// conversation avec ses vrais rôles, en la terminant sur un message
// assistant, a tendance à CONTINUER la conversation (produire le tour
// suivant, ex: répondre "veux-tu que je...") plutôt qu'à obéir à
// l'instruction système de résumer — un dernier message "user" demandant
// explicitement de résumer un texte fourni est un schéma bien plus naturel
// à suivre pour un modèle de chat. Volontairement aucune mention
// "Utilisateur"/"Assistant", même en préfixe : ces mots sont probablement ce
// qui déclenche justement ce réflexe de continuation — remplacés par des
// étiquettes neutres (Demande/Action effectuée/Résultat obtenu/Réponse
// donnée) qui gardent l'information utile sans ressembler à un tour de chat.
func serializeForSummary(messages []llm.Message) string {
	var b strings.Builder
	b.WriteString("Voici le journal d'une session à résumer :\n\n")
	for _, m := range messages {
		switch m.Role {
		case "user":
			fmt.Fprintf(&b, "Demande : %s\n\n", m.Content)
		case "assistant":
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, "Action effectuée : %s(%s)\n\n", tc.Function.Name, tc.Function.Arguments)
				}
			} else {
				fmt.Fprintf(&b, "Réponse donnée : %s\n\n", m.Content)
			}
		case "tool":
			fmt.Fprintf(&b, "Résultat obtenu : %s\n\n", m.Content)
		}
	}
	return strings.TrimSpace(b.String())
}

// toolCallArtifactMarkers : sous-chaînes trahissant une tentative d'appel
// d'outil échappée en texte brut plutôt qu'en tool_calls structuré — observé
// avec certains modèles/serveurs qui gardent leur gabarit de function
// calling actif même quand la requête ne propose aucun tool (ex: un résumé
// de compaction ou une extraction mémoire qui recopie tel quel "<tool_call>
// <function=run_shell>..."). Une réponse qui en contient une n'est jamais un
// résumé/une extraction valide, quel que soit le reste de son contenu.
var toolCallArtifactMarkers = []string{
	"<tool_call>", "</tool_call>",
	"<function=", "</function>",
	"<|tool_call|>",
}

func looksLikeToolCallArtifact(s string) bool {
	lower := strings.ToLower(s)
	for _, marker := range toolCallArtifactMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// persistMemory extrait, via un appel LLM séparé du résumé de compaction,
// ce qui parmi messages mérite de survivre à cette conversation (voir
// memoryExtractionSystemPrompt), et l'ajoute à c.MemoryFile si le modèle en
// a trouvé. Ne fait rien si c.MemoryFile est vide.
//
// Purement best-effort : toute erreur (appel LLM, écriture disque) est
// journalisée puis ignorée plutôt que remontée à l'appelant — le résultat
// de Compact (la compaction elle-même a réussi ou non) ne doit jamais
// dépendre du succès de cet à-côté.
func (c *Conversation) persistMemory(ctx context.Context, client *llm.Client, messages []llm.Message) {
	if c.MemoryFile == "" {
		return
	}

	req := []llm.Message{
		{Role: "system", Content: memoryExtractionSystemPrompt},
		{Role: "user", Content: serializeForSummary(messages) + "\n\n--- fin du journal ---\n\nExtrais-en ce qui mérite d'être retenu, selon les instructions données."},
	}

	msg, _, err := client.ChatCompletion(ctx, req, nil)
	if err != nil {
		log.Printf("convo: extraction mémoire (compaction): %v", err)
		return
	}
	content := strings.TrimSpace(msg.Content)
	if content == "" || strings.EqualFold(content, memoryNothingSentinel) {
		return
	}
	if looksLikeToolCallArtifact(content) {
		log.Printf("convo: extraction mémoire ignorée (appel d'outil échappé en texte au lieu d'une extraction valide) : %s", content)
		return
	}

	entry := fmt.Sprintf("\n## %s (compaction automatique)\n%s\n", time.Now().Format("2006-01-02 15:04"), content)
	f, err := os.OpenFile(c.MemoryFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("convo: écriture mémoire (%s): %v", c.MemoryFile, err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(entry); err != nil {
		log.Printf("convo: écriture mémoire (%s): %v", c.MemoryFile, err)
	}
}

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

	cut := len(c.Messages) - keep
	// Ne jamais couper au milieu d'un groupe [message assistant à tool_calls
	// + ses résultats d'outils] : côté API, un message role="tool" ne peut
	// apparaître qu'immédiatement après le message assistant qui l'a
	// demandé. Couper pile à la limite de keep, sans égard pour cette
	// structure, produit soit un message assistant à tool_calls sans ses
	// résultats en fin de résumé (rejeté par le serveur : "Cannot continue
	// an assistant message that contains tool calls"), soit, symétriquement,
	// un message "tool" orphelin en tête de l'historique conservé — les deux
	// cassent le tour suivant. On recule donc jusqu'à un point de coupure
	// sûr (jamais sur un message "tool").
	for cut > 0 && cut < len(c.Messages) && c.Messages[cut].Role == "tool" {
		cut--
	}
	if cut <= 0 {
		// Tout ce qui précédait le point visé fait partie d'un même groupe
		// tool_calls : rien à résumer sans casser ce groupe.
		return false, nil
	}

	toSummarize := c.Messages[:cut]
	kept := append([]llm.Message(nil), c.Messages[cut:]...)

	// persistMemory lancée en parallèle du résumé (pas après) : deux appels
	// LLM indépendants sur les mêmes messages, autant ne pas payer leur
	// latence en série. Contrepartie assumée : contrairement à avant,
	// l'appel mémoire a maintenant lieu même si le résumé échoue ensuite
	// (retenté au prochain tour) — un aller-retour LLM de plus dans ce cas
	// précis, en échange d'une compaction deux fois plus rapide dans le cas
	// normal (succès). wg.Wait() avant de retourner : que Compact réussisse
	// ou échoue, on ne rend la main qu'une fois persistMemory terminée
	// (déterministe pour les appelants/tests, qui peuvent alors vérifier
	// MEMORY.md immédiatement après).
	var wg sync.WaitGroup
	if c.MemoryFile != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.persistMemory(ctx, client, toSummarize)
		}()
	}

	summarizeReq := []llm.Message{
		{Role: "system", Content: summarizeSystemPrompt},
		{Role: "user", Content: serializeForSummary(toSummarize) + "\n\n--- fin du journal à résumer ---\n\nRésume ce journal, selon les instructions données."},
	}

	// L'usage renvoyé ici correspond au prompt de résumé, pas à la
	// conversation réelle : on ne l'enregistre pas via RecordUsage.
	summaryMsg, _, err := client.ChatCompletion(ctx, summarizeReq, nil)
	wg.Wait()
	if err != nil {
		return false, fmt.Errorf("compaction du contexte: %w", err)
	}
	summary := strings.TrimSpace(summaryMsg.Content)
	if summary == "" || looksLikeToolCallArtifact(summary) {
		// Aucun tool n'est proposé dans summarizeReq (dernier argument nil),
		// mais certains modèles/serveurs peuvent quand même renvoyer un
		// tool_call (Content alors vide) — ou pire, l'échapper en texte
		// brut dans Content plutôt qu'en tool_calls structuré (gabarit de
		// chat qui garde le function calling actif indépendamment de la
		// liste "tools" de la requête), auquel cas Content est non vide
		// mais n'est pas un résumé. Mieux vaut échouer clairement ici
		// (compaction retentée au prochain tour) que remplacer l'historique
		// par un résumé vide ou par un appel d'outil recopié tel quel.
		return false, fmt.Errorf("compaction du contexte: résumé invalide renvoyé par le modèle (vide, ou appel d'outil échappé en texte au lieu d'un résumé)")
	}

	compacted := make([]llm.Message, 0, len(kept)+1)
	compacted = append(compacted, llm.Message{
		Role: "system",
		// "à titre d'information, pas d'instruction" : sans cette précision,
		// le modèle a tendance à reprendre de lui-même la "Tâche en cours"
		// décrite dans summary au prochain message de l'utilisateur, même
		// quand celui-ci n'a aucun rapport (observé après une compaction
		// manuelle via /compact, où l'utilisateur veut souvent justement
		// changer de sujet ensuite) — comportement correct seulement quand la
		// compaction a lieu au milieu d'un tour déjà en cours sur cette tâche
		// (compaction automatique), pas quand elle précède un message qui n'y
		// est pas lié.
		Content: "Résumé de la conversation précédente (contexte compacté, à titre d'information, pas d'instruction à suivre : ne reprends la \"tâche en cours\" qu'il décrit que si le prochain message s'y rapporte réellement) :\n" + summary,
	})
	compacted = append(compacted, kept...)
	c.Messages = compacted

	// Le compte exact précédent ne correspond plus à ce nouvel historique :
	// on retombe sur l'heuristique jusqu'au prochain appel réel.
	c.LastKnownTokens = 0
	c.LastKnownMessageCount = 0

	// persistMemory déjà lancée en parallèle plus haut, et déjà terminée
	// (wg.Wait() ci-dessus) : rien à refaire ici.

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

// EnsureFitsContext est le filet de sécurité de dernier recours, à appeler
// juste avant d'envoyer une requête, après (une tentative de) compaction :
// si l'historique dépasse encore MaxContextTokens — la vraie limite dure du
// modèle, pas seulement le seuil CompactAt qui déclenche une compaction
// "propre" — supprime les messages les plus anciens, sans passer par le
// LLM (aucun résumé, juste une coupe brutale), jusqu'à repasser sous la
// limite. Nécessaire notamment quand la compaction elle-même a échoué de
// façon répétée (voir agent.Run) : sans ça, l'historique complet continue
// de grossir à chaque tour jusqu'à ce que le serveur rejette purement et
// simplement la requête pour dépassement de contexte.
//
// Garde toujours au moins le tout dernier message (jamais un historique
// vidé complètement), et ne coupe jamais au milieu d'un groupe [assistant à
// tool_calls + ses résultats] — même précaution que Compact, voir son
// commentaire. Retourne le nombre de messages supprimés (0 = rien à faire).
func (c *Conversation) EnsureFitsContext() int {
	if c.MaxContextTokens <= 0 {
		return 0
	}
	dropped := 0
	for len(c.Messages) > 1 && c.EstimateTokens() > c.MaxContextTokens {
		cut := 1
		for cut < len(c.Messages) && c.Messages[cut].Role == "tool" {
			cut++
		}
		c.Messages = c.Messages[cut:]
		dropped += cut
	}
	if dropped > 0 {
		// Le compte exact précédent ne correspond plus à ce nouvel
		// historique : on retombe sur l'heuristique jusqu'au prochain appel
		// réel (même raisonnement que Compact).
		c.LastKnownTokens = 0
		c.LastKnownMessageCount = 0
	}
	return dropped
}

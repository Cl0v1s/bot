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

	// LastCompactionDropped : nombre de messages supprimés SANS résumé lors
	// de la dernière compaction (voir Compact), pour que l'appelant puisse
	// le signaler. 0 = aucun.
	LastCompactionDropped int
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

// memoryNothingSentinel : réponse attendue de memoryMaintenanceSystemPrompt
// quand rien ne mérite d'être gardé du tout (mémoire vidée) — un texte vide
// ferait tout aussi bien l'affaire, mais un sentinel explicite évite de
// compter comme "mémoire" un modèle qui répondrait par un simple espace ou
// un commentaire vide de sens.
const memoryNothingSentinel = "RIEN"

// memoryMaintenanceSystemPrompt : appel séparé du résumé de compaction
// (summarizeSystemPrompt) — un résumé de conversation et une note de
// mémoire long terme n'ont pas le même public ni la même durée de vie (le
// premier vit et meurt avec cette conversation, la seconde est censée
// survivre à un /reset ou à une future session) ni le même format souhaité,
// les mélanger dans un seul appel aurait rendu l'un ou l'autre bâclé.
//
// Volontairement une opération de MAINTENANCE (révision complète de ce qui
// existe déjà + ce qui vient de cette session), pas une simple extraction
// qui ajoute au fil de l'eau : le modèle reçoit toute la mémoire actuelle et
// renvoie son contenu final complet, avec le pouvoir de retirer/fusionner ce
// qui n'est plus utile, pas seulement d'éviter d'ajouter un doublon. Sans
// ça, une mémoire purement en ajout ne fait que grossir indéfiniment, même
// en évitant les doublons exacts (une préférence reformulée légèrement
// différemment à chaque fois, une entrée devenue obsolète mais jamais
// retirée...).
var memoryMaintenanceSystemPrompt = fmt.Sprintf(`Tu fais la maintenance de la mémoire long terme, partagée entre TOUTES les tâches futures, même sans aucun rapport avec la session en cours en train d'être compactée. Ce n'est PAS un journal de compactions successives où l'on empile : c'est une révision complète à chaque fois.

On te donne, ci-dessous : (1) le contenu ACTUEL de cette mémoire (vide s'il n'y a encore rien), puis (2) le journal de la session en cours. Ta tâche : produire le contenu COMPLET et FINAL de la mémoire après révision — pas seulement ce qui change, pas un résumé de ce qui change, le contenu entier tel qu'il doit exister après cette révision.

Pour chaque idée, existante ou candidate depuis le journal de cette session, ne la garde que si elle resterait utile pour une tâche complètement différente, plus tard :
- préférences de travail de l'utilisateur (formats, conventions, outils préférés, façon dont il aime que tu procèdes...)
- contraintes ou règles qu'il a demandé de respecter systématiquement
- corrections qu'il t'a faites sur ton comportement
- emplacements de fichiers/dossiers récurrents (le chemin lui-même, pas leur contenu)

Retire activement (pas seulement "n'ajoute pas") :
- ce qui est devenu obsolète ou contredit par quelque chose de plus récent
- ce qui fait doublon ou quasi-doublon avec une autre entrée — fusionne en une seule entrée claire plutôt que d'en garder plusieurs versions
- le contenu ou le sujet traité pendant CETTE session (l'histoire, les personnages, l'intrigue, les détails d'un projet ponctuel, ce qui a été fait ou produit...) : ça n'aide en rien pour une tâche différente, que ce soit déjà présent dans la mémoire actuelle ou candidat depuis le journal de cette session. Le résumé de compaction s'occupe déjà de conserver le détail de LA tâche en cours.
- tout ce qui, en te relisant, ne passerait pas le test "utile pour une tâche complètement différente" même si ça y figurait déjà avant

Le cas normal est une mémoire COURTE, et qui reste courte au fil du temps plutôt que de grossir sans fin : dans le doute, retire plutôt que garder. Une mémoire qui ne fait qu'accumuler est un échec de cette tâche de maintenance, pas une réussite prudente.

Format : liste à puces concise, groupée par thème si plusieurs sujets distincts, sans horodatage ni mention de session (ce n'est pas un journal, juste l'état actuel des choses qui comptent).
Si rien ne mérite d'être gardé au final (mémoire actuelle vide et rien de nouveau ne le mérite, OU plus rien de l'existant ne passe la révision), réponds exactement %q et rien d'autre.
Réponds uniquement par le contenu final de la mémoire (ou le sentinel), jamais un commentaire sur ta démarche ("voici la mémoire mise à jour" etc.), jamais un appel d'outil ni une syntaxe qui y ressemble (ex: balises <tool_call>, <function=...>), même si le journal fourni en comporte.`, memoryNothingSentinel)

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

// memoryContextCap : nombre max de caractères de c.MemoryFile transmis au
// modèle (voir persistMemory) — garde-fou contre un fichier devenu
// pathologiquement gros (corruption, écriture manuelle malheureuse...), pas
// un mécanisme normal : maintenant que persistMemory renvoie le contenu
// COMPLET de la mémoire (voir memoryMaintenanceSystemPrompt), tronquer
// l'entrée revient à supprimer silencieusement tout ce qui dépasse — jamais
// anodin comme ça l'était quand la mémoire ne faisait que s'accumuler par
// ajout. Volontairement généreux (une mémoire qui a besoin d'plus que ça est
// déjà le signe que la maintenance elle-même a échoué à la garder courte) ;
// un dépassement est journalisé (voir persistMemory), jamais silencieux.
const memoryContextCap = 60000

// persistMemory fait la maintenance de c.MemoryFile via un appel LLM séparé
// du résumé de compaction (voir memoryMaintenanceSystemPrompt) : lui donne
// le contenu actuel de la mémoire ainsi que le journal de cette session, et
// REMPLACE tout le fichier par ce que le modèle renvoie — pas un ajout, une
// révision complète qui peut aussi bien retirer/fusionner de l'existant
// qu'ajouter du nouveau. Ne fait rien si c.MemoryFile est vide.
//
// Purement best-effort : toute erreur (lecture ou écriture disque, appel
// LLM) est journalisée puis ignorée plutôt que remontée à l'appelant — le
// résultat de Compact (la compaction elle-même a réussi ou non) ne doit
// jamais dépendre du succès de cet à-côté.
func (c *Conversation) persistMemory(ctx context.Context, client *llm.Client, messages []llm.Message) {
	if c.MemoryFile == "" {
		return
	}

	existing := ""
	if data, err := os.ReadFile(c.MemoryFile); err == nil {
		existing = strings.TrimSpace(string(data))
		if len(existing) > memoryContextCap {
			log.Printf("convo: mémoire (%s) tronquée à %d caractères avant maintenance (%d au total) — le surplus ne sera pas revu et pourrait être perdu de la révision", c.MemoryFile, memoryContextCap, len(existing))
			existing = existing[len(existing)-memoryContextCap:]
		}
	}

	userContent := "Mémoire actuelle : (vide, rien n'est encore enregistré)\n\n--- fin de la mémoire actuelle ---\n\n"
	if existing != "" {
		userContent = "Mémoire actuelle :\n\n" + existing + "\n\n--- fin de la mémoire actuelle ---\n\n"
	}
	userContent += serializeForSummary(messages) + "\n\n--- fin du journal de cette session ---\n\nProduis le contenu complet et final de la mémoire après révision, selon les instructions données."

	req := []llm.Message{
		{Role: "system", Content: memoryMaintenanceSystemPrompt},
		{Role: "user", Content: userContent},
	}

	msg, _, err := client.ChatCompletion(ctx, req, nil)
	if err != nil {
		log.Printf("convo: maintenance mémoire (compaction): %v", err)
		return
	}
	content := strings.TrimSpace(msg.Content)
	if looksLikeToolCallArtifact(content) {
		log.Printf("convo: maintenance mémoire ignorée (appel d'outil échappé en texte au lieu d'un contenu de mémoire valide) : %s", content)
		return
	}
	if strings.EqualFold(content, memoryNothingSentinel) {
		content = ""
	}

	if content == existing {
		// Rien à écrire : évite une écriture disque et une resynchronisation
		// sandbox (voir write_file, dont la logique n'est pas dupliquée ici —
		// persistMemory écrit toujours sous l'identité réelle) pour un
		// contenu identique, cas courant quand la révision ne change rien.
		return
	}

	if err := os.WriteFile(c.MemoryFile, []byte(content), 0o644); err != nil {
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

	// Le résumé est demandé en une seule requête contenant tout ce qu'il y a
	// à résumer : c'est-à-dire, au moment où la compaction se déclenche,
	// presque tout le contexte — plus la place de générer le résumé. Sans
	// borne, cette requête déborde précisément quand la compaction est le
	// plus nécessaire. On retire donc d'emblée les plus vieux messages tant
	// que la requête dépasse summaryInputBudget, puis, à chaque échec pour
	// dépassement de contexte, on en retire encore une part et on retente.
	// Les messages ainsi retirés sont définitivement supprimés de la
	// conversation (sans résumé), même si toutes les tentatives échouent :
	// la conversation rétrécit à chaque échec au lieu de rester bloquée à
	// une taille qui ne passe plus.
	budget := c.summaryInputBudget()
	dropped := 0
	for len(toSummarize) > 0 && budget > 0 && summarizeRequestTokens(toSummarize) > budget {
		n := dropOldestGroupSafe(toSummarize, 1)
		toSummarize = toSummarize[n:]
		dropped += n
	}

	var summaryMsg llm.Message
	var err error
	for attempt := 1; ; attempt++ {
		if len(toSummarize) == 0 {
			break
		}
		summarizeReq := []llm.Message{
			{Role: "system", Content: summarizeSystemPrompt},
			{Role: "user", Content: serializeForSummary(toSummarize) + "\n\n--- fin du journal à résumer ---\n\nRésume ce journal, selon les instructions données."},
		}
		// L'usage renvoyé ici correspond au prompt de résumé, pas à la
		// conversation réelle : on ne l'enregistre pas via RecordUsage.
		summaryMsg, _, err = client.ChatCompletion(ctx, summarizeReq, nil)
		if err == nil || !IsContextOverflow(err) || attempt >= maxCompactionAttempts || ctx.Err() != nil {
			break
		}
		n := dropOldestGroupSafe(toSummarize, (len(toSummarize)+compactionDropDivisor-1)/compactionDropDivisor)
		log.Printf("convo: compaction (tentative %d) : contexte dépassé, %d message(s) le(s) plus ancien(s) supprimé(s) avant nouvel essai : %v", attempt, n, err)
		toSummarize = toSummarize[n:]
		dropped += n
	}

	if dropped > 0 {
		// Suppression définitive, que le résumé ait finalement réussi ou non.
		c.Messages = append(append([]llm.Message(nil), toSummarize...), kept...)
		c.LastKnownTokens = 0
		c.LastKnownMessageCount = 0
		c.LastCompactionDropped = dropped
		log.Printf("convo: compaction : %d message(s) le(s) plus ancien(s) supprimé(s) sans résumé pour tenir dans le contexte", dropped)
	}
	if len(toSummarize) == 0 {
		// Plus rien à résumer : tout l'ancien historique a dû être supprimé.
		// La conversation a quand même été ramenée à kept.
		return dropped > 0, nil
	}
	if err != nil {
		return dropped > 0, fmt.Errorf("compaction du contexte: %w", err)
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
		return dropped > 0, fmt.Errorf("compaction du contexte: résumé invalide renvoyé par le modèle (vide, ou appel d'outil échappé en texte au lieu d'un résumé)")
	}

	// Maintenance mémoire APRÈS le résumé, pas en parallèle : les serveurs
	// locaux (LM Studio...) partagent une même réserve de contexte entre
	// les générations simultanées — deux gros appels en même temps font
	// déborder les deux ("The model ran out of context space while
	// generating. This happens when several chats [...] generate at the
	// same time"). Seulement en cas de succès du résumé, et sur les seuls
	// messages effectivement résumés (déjà bornés par budget).
	if c.MemoryFile != "" {
		c.persistMemory(ctx, client, toSummarize)
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

	return true, nil
}

const (
	// maxCompactionAttempts : nombre max de tentatives de résumé dans un
	// même appel à Compact (voir la boucle de réessai).
	maxCompactionAttempts = 6
	// compactionDropDivisor : à chaque échec pour dépassement de contexte,
	// 1/compactionDropDivisor des messages restant à résumer (les plus
	// anciens) est supprimé avant de retenter.
	compactionDropDivisor = 4
	// summaryInputShare : part de MaxContextTokens que la requête de résumé
	// peut occuper au maximum, le reste étant laissé à la génération du
	// résumé lui-même (et à la marge d'erreur de l'estimation).
	summaryInputShare = 0.6
)

// summaryInputBudget : taille max (tokens estimés) de la requête de résumé ;
// 0 = pas de borne (MaxContextTokens inconnu).
func (c *Conversation) summaryInputBudget() int {
	if c.MaxContextTokens <= 0 {
		return 0
	}
	return int(float64(c.MaxContextTokens) * summaryInputShare)
}

// summarizeRequestTokens estime la taille de la requête de résumé pour
// messages.
func summarizeRequestTokens(messages []llm.Message) int {
	return estimateTokens(summarizeSystemPrompt) + estimateTokens(serializeForSummary(messages)) + 50
}

// IsContextOverflow indique si err est un refus du serveur LLM pour
// dépassement de la fenêtre de contexte (requête trop longue, ou plus de
// place pour générer). Heuristique sur le message d'erreur : les serveurs
// compatibles OpenAI (LM Studio, llama.cpp, vLLM...) n'ont pas de code
// d'erreur commun pour ce cas.
func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"context window", "context length", "context space", "context size",
		"maximum context", "context_length_exceeded", "too long", "exceeds the",
		"ran out of context",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// dropOldestGroupSafe retourne le nombre de messages à retirer en tête de
// messages pour en supprimer au moins n, sans jamais couper au milieu d'un
// groupe [assistant à tool_calls + ses résultats] (voir Compact) : un
// message "tool" ne peut pas se retrouver en tête. Peut retourner
// len(messages).
func dropOldestGroupSafe(messages []llm.Message, n int) int {
	if n < 1 {
		n = 1
	}
	if n >= len(messages) {
		return len(messages)
	}
	for n < len(messages) && messages[n].Role == "tool" {
		n++
	}
	return n
}

// DropOldest supprime au moins une fraction (0-1) des messages les plus
// anciens de la conversation, sans casser de groupe tool_calls et en gardant
// toujours au moins le dernier message. Filet de sécurité quand le serveur
// refuse une requête pour dépassement de contexte malgré les estimations
// (voir agent.Run). Retourne le nombre de messages supprimés.
func (c *Conversation) DropOldest(fraction float64) int {
	if len(c.Messages) <= 1 {
		return 0
	}
	n := dropOldestGroupSafe(c.Messages, int(float64(len(c.Messages))*fraction+0.5))
	if n >= len(c.Messages) {
		n = len(c.Messages) - 1
		for n > 0 && c.Messages[n].Role == "tool" {
			n--
		}
	}
	if n <= 0 {
		return 0
	}
	c.Messages = append([]llm.Message(nil), c.Messages[n:]...)
	c.LastKnownTokens = 0
	c.LastKnownMessageCount = 0
	return n
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

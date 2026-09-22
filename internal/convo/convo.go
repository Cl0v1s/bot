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

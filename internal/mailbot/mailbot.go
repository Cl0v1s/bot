// Package mailbot lit périodiquement les mails non lus d'une boîte IMAP,
// génère une réponse via le LLM et l'envoie automatiquement par SMTP.
package mailbot

import (
	"context"
	"fmt"
	"log"
	"net"
	"regexp"
	"strings"
	"time"

	"bot/internal/agent"
	"bot/internal/convo"
	"bot/internal/imapclient"
	"bot/internal/llm"
	"bot/internal/smtpclient"
	"bot/internal/tools"
)

type Options struct {
	IMAPHost     string
	IMAPPort     string
	IMAPTLS      bool
	IMAPUser     string
	IMAPPassword string
	IMAPMailbox  string

	SMTP *smtpclient.Client

	PollInterval time.Duration
	SystemPrompt string

	ContextMaxTokens int
	ContextCompactAt float64
	ContextKeepLast  int

	// MemoryFile : voir convo.Conversation.MemoryFile — "" = désactivé.
	// ATTENTION : le corps d'un mail entrant est un contenu non fiable (voir
	// Tools ci-dessous) ; l'activer expose l'extraction mémoire de la
	// compaction à un mail malveillant qui y injecterait une "mémoire"
	// persistante, susceptible d'influencer le modèle bien au-delà de ce
	// seul mail — y compris en mode chat, ce fichier étant partagé. Accepté
	// en connaissance de cause à la demande explicite de l'opérateur.
	MemoryFile string

	// Conversations : historique de conversation par fil de discussion (voir
	// threadKey/normalizeSubject), pour qu'une réponse dans un fil en cours
	// reprenne le contexte des échanges précédents au lieu de repartir de
	// zéro à chaque mail — comme le fait déjà le mode chat entre deux
	// messages d'une même session. Un fil est identifié par (expéditeur,
	// sujet normalisé) : le sujet seul ne suffit pas, sous peine de mélanger
	// le contexte de deux personnes différentes qui écrivent avec le même
	// sujet (ou sans sujet). Initialisé automatiquement si nil par Run.
	// Vit en mémoire du process : perdu au redémarrage, pas persisté sur
	// disque, comme le reste de l'état du mode mail (et du mode chat).
	Conversations map[string]*convo.Conversation

	// AllowFrom : liste blanche d'adresses (en minuscules) autorisées à
	// déclencher une réponse automatique. Vide = pas de filtre.
	AllowFrom    []string
	MaxBodyChars int

	// Tools : registre d'outils optionnel (nil/vide = pas de tool calling).
	// ATTENTION : le corps du mail est un contenu non fiable — inclure
	// run_shell/write_file ici est un vecteur d'exécution de code arbitraire
	// par injection de prompt, à n'accepter qu'en connaissance de cause (voir
	// la mise en garde dans main.go et MAIL_ALLOW_FROM/le sandbox système).
	Tools         *tools.Registry
	ToolsPerms    *tools.DirPermissions // rafraîchi depuis le fichier partagé à chaque cycle de poll
	AgentMaxSteps int
	// AgentMaxConsecutiveShellFailures : voir agent.Run — <= 0 désactive.
	AgentMaxConsecutiveShellFailures int
}

// logBlock journalise body sous forme de bloc encadré, nettement délimité
// des autres lignes de log — même style que les événements d'outils
// (internal/agent.Event.Format), pour que les requêtes reçues et les
// réponses renvoyées soient faciles à repérer dans les logs du mode mail.
func logBlock(title, body string) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n╭─ %s\n", title)
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		fmt.Fprintf(&b, "│ %s\n", line)
	}
	b.WriteString("╰─")
	log.Print(b.String())
}

func (o Options) isAllowed(fromAddress string) bool {
	if len(o.AllowFrom) == 0 {
		return true
	}
	fromAddress = strings.ToLower(fromAddress)
	for _, allowed := range o.AllowFrom {
		if fromAddress == allowed {
			return true
		}
	}
	return false
}

func (o Options) imapAddr() string {
	return net.JoinHostPort(o.IMAPHost, o.IMAPPort)
}

// subjectPrefixRe reconnaît un préfixe de réponse/transfert en tête de
// sujet (Re:, Fwd:, Fw:, Tr:...), pour le retirer lors de la normalisation.
var subjectPrefixRe = regexp.MustCompile(`(?i)^(re|fwd?|tr)\s*:\s*`)

// normalizeSubject réduit un sujet de mail à sa forme canonique pour
// regrouper les mails d'un même fil de discussion : préfixes de réponse/
// transfert retirés (répétés, ex: "Re: Fwd: Question"), espaces superflus et
// casse normalisés. Correspondance approximative par sujet — pas le suivi
// RFC des en-têtes References/In-Reply-To (plus robuste mais plus complexe),
// choisi ici parce que c'est ce qu'un humain considère spontanément comme
// "le même sujet".
func normalizeSubject(subject string) string {
	s := strings.TrimSpace(subject)
	for {
		trimmed := strings.TrimSpace(subjectPrefixRe.ReplaceAllString(s, ""))
		if trimmed == s {
			break
		}
		s = trimmed
	}
	return strings.ToLower(s)
}

// threadKey identifie un fil de discussion par (expéditeur, sujet
// normalisé) : le sujet seul ne suffit pas, sous peine de mélanger le
// contexte de deux personnes différentes qui écrivent avec le même sujet
// (fréquent avec un sujet vide, ex: toujours "" pour qui n'en met jamais).
func threadKey(senderAddr, subject string) string {
	return strings.ToLower(senderAddr) + "\x00" + normalizeSubject(subject)
}

// getOrCreateConversation retourne la conversation existante pour le fil
// (senderAddr, subject) si ce fil est déjà en cours, ou en crée une
// nouvelle sinon (et l'enregistre pour les mails suivants du même fil). Le
// rappel de l'usage des outils (voir agent.AppendToolUsagePrompt) n'est
// ajouté qu'à la création : le répéter à chaque réutilisation grossirait le
// system prompt inutilement à chaque mail du fil.
func (o Options) getOrCreateConversation(senderAddr, subject string) *convo.Conversation {
	key := threadKey(senderAddr, subject)
	if conv, ok := o.Conversations[key]; ok {
		return conv
	}

	conv := convo.New(o.SystemPrompt, o.ContextMaxTokens, o.ContextCompactAt, o.ContextKeepLast)
	conv.MemoryFile = o.MemoryFile
	if !o.Tools.Empty() {
		agent.AppendToolUsagePrompt(conv)
	}
	o.Conversations[key] = conv
	return conv
}

// Run boucle indéfiniment (jusqu'à annulation du contexte) en interrogeant
// la boîte mail toutes les PollInterval.
func Run(ctx context.Context, client *llm.Client, opts Options) error {
	log.Printf("mailbot: démarrage — boîte %s@%s, poll toutes les %s", opts.IMAPUser, opts.imapAddr(), opts.PollInterval)

	if opts.Conversations == nil {
		opts.Conversations = make(map[string]*convo.Conversation)
	}

	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()

	for {
		if err := pollOnce(ctx, client, opts); err != nil {
			log.Printf("mailbot: erreur pendant le poll: %v", err)
		}

		select {
		case <-ctx.Done():
			log.Printf("mailbot: arrêt demandé")
			return nil
		case <-ticker.C:
		}
	}
}

func pollOnce(ctx context.Context, client *llm.Client, opts Options) error {
	// Relit les répertoires accordés par ailleurs (ex: via le mode chat)
	// depuis le fichier partagé, sans jamais pouvoir en ajouter soi-même
	// (Options.ToolsPerms n'a pas de DirGrantFunc en mode mail).
	if opts.ToolsPerms != nil {
		if err := opts.ToolsPerms.Refresh(); err != nil {
			log.Printf("mailbot: échec du rafraîchissement des répertoires autorisés: %v", err)
		}
	}

	ic, err := imapclient.Dial(opts.imapAddr(), opts.IMAPTLS)
	if err != nil {
		return fmt.Errorf("connexion IMAP: %w", err)
	}
	defer ic.Close()

	if err := ic.Login(opts.IMAPUser, opts.IMAPPassword); err != nil {
		return fmt.Errorf("login IMAP: %w", err)
	}
	defer ic.Logout()

	if err := ic.Select(opts.IMAPMailbox); err != nil {
		return fmt.Errorf("select %s: %w", opts.IMAPMailbox, err)
	}

	ids, err := ic.SearchUnseen()
	if err != nil {
		return fmt.Errorf("search unseen: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	log.Printf("mailbot: %d message(s) non lu(s)", len(ids))

	for _, seq := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := handleMessage(ctx, client, ic, opts, seq); err != nil {
			log.Printf("mailbot: erreur sur le message #%d: %v", seq, err)
		}
	}
	return nil
}

func handleMessage(ctx context.Context, client *llm.Client, ic *imapclient.Client, opts Options, seq int) error {
	raw, err := ic.FetchRFC822(seq)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	parsed, err := parseRaw(raw)
	if err != nil {
		return fmt.Errorf("parsing: %w", err)
	}
	log.Printf("mailbot: traitement du mail de %s — %q", parsed.From, parsed.Subject)

	senderAddr := extractAddress(parsed.From)
	if !opts.isAllowed(senderAddr) {
		log.Printf("mailbot: expéditeur %s non autorisé (allowFrom), mail ignoré", senderAddr)
		if err := ic.MarkSeen(seq); err != nil {
			return fmt.Errorf("marquage lu: %w", err)
		}
		return nil
	}

	body := parsed.Body
	if opts.MaxBodyChars > 0 && len(body) > opts.MaxBodyChars {
		body = body[:opts.MaxBodyChars] + "\n[... contenu tronqué ...]"
	}

	// Volontairement aucun framing ni métadonnée (pas de "De:"/"Sujet:", pas
	// de mention qu'il s'agit d'un mail) : seul le corps est transmis au
	// modèle. Le savoir pousse le modèle vers un style email trop formel
	// (formules de politesse, signature...).
	userMessage := body
	logBlock(fmt.Sprintf("requête mail › de %s", senderAddr), userMessage)

	conv := opts.getOrCreateConversation(senderAddr, parsed.Subject)
	conv.AddUser(userMessage)

	var reply string
	if !opts.Tools.Empty() {
		// Les appels d'outils et leurs résultats sont journalisés à part,
		// nettement séparés du texte de la réponse envoyée par mail.
		reply, _, err = agent.Run(ctx, client, conv, opts.Tools, opts.AgentMaxSteps, opts.AgentMaxConsecutiveShellFailures, func(e agent.Event) {
			log.Print(strings.TrimRight(e.Format(), "\n"))
		})
		if err != nil {
			return fmt.Errorf("agent LLM: %w", err)
		}
	} else {
		if _, err := conv.CompactIfNeeded(ctx, client); err != nil {
			log.Printf("mailbot: échec compaction contexte: %v", err)
		}
		// Filet de sécurité de dernier recours : voir le commentaire de
		// convo.Conversation.EnsureFitsContext.
		if dropped := conv.EnsureFitsContext(); dropped > 0 {
			log.Printf("mailbot: contexte encore trop grand après compaction : %d message(s) le plus ancien(s) supprimé(s) sans résumé", dropped)
		}
		var usage llm.Usage
		var msg llm.Message
		msg, usage, err = client.ChatCompletion(ctx, conv.Full(), nil)
		if err != nil {
			return fmt.Errorf("appel LLM: %w", err)
		}
		reply = msg.Content
		conv.AddAssistant(reply)
		conv.RecordUsage(usage)
	}

	logBlock(fmt.Sprintf("réponse mail › à %s", senderAddr), reply)

	// Même sujet que le mail original, sans préfixe "Re:". Le threading
	// (affichage "conversation" côté client, ex. Thunderbird) repose sur les
	// en-têtes In-Reply-To/References, pas sur le préfixe du sujet.
	references := parsed.MessageID
	if parsed.References != "" {
		references = parsed.References + " " + parsed.MessageID
	}

	err = opts.SMTP.Send(smtpclient.Mail{
		To:         senderAddr,
		Subject:    parsed.Subject,
		Body:       reply,
		InReplyTo:  parsed.MessageID,
		References: references,
	})
	if err != nil {
		return fmt.Errorf("envoi réponse: %w", err)
	}

	if err := ic.MarkSeen(seq); err != nil {
		return fmt.Errorf("marquage lu: %w", err)
	}

	log.Printf("mailbot: réponse envoyée à %s", senderAddr)
	return nil
}

// Package config charge la configuration du harnais depuis les variables
// d'environnement (et un éventuel fichier .env local).
package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"bot/internal/sandbox"
)

type Config struct {
	// LLM (API compatible OpenAI, endpoint local)
	LLMBaseURL   string // ex: http://localhost:8000/v1
	LLMAPIKey    string // souvent une valeur factice pour un serveur local
	LLMModel     string
	SystemPrompt string
	// LLMTimeout : durée maximale d'une requête au LLM, de l'envoi à la fin
	// de la réponse (voir llm.DefaultTimeout). 0 = aucune limite (seule
	// l'annulation du contexte, ex: Ctrl+C en mode chat, interrompt alors la
	// requête).
	LLMTimeout time.Duration
	// LLMMaxTokens : nombre maximal de tokens générés par réponse (voir
	// llm.Client.MaxTokens). 0 = aucune limite transmise (le serveur
	// applique la sienne). Par défaut, defaultMaxTokensShare de
	// ContextMaxTokens.
	LLMMaxTokens int
	// LLMMaxRetries : nombre de relances automatiques d'une requête au LLM
	// après une erreur transitoire (connexion impossible ou coupée, flux
	// interrompu, status 502/503/504 — voir llm.Client.MaxRetries). 0 =
	// aucune relance.
	LLMMaxRetries int

	// Gestion du contexte de conversation
	ContextMaxTokens   int     // taille de contexte du modèle, en tokens (approx.)
	ContextCompactAt   float64 // fraction du contexte (0-1) déclenchant la compaction
	ContextKeepLastMsg int     // nombre de messages récents conservés tels quels lors d'une compaction

	// IMAP (lecture des mails)
	IMAPHost     string
	IMAPPort     string
	IMAPUser     string
	IMAPPassword string
	IMAPMailbox  string
	IMAPTLS      bool // true = TLS implicite (port 993)

	// SMTP (envoi des réponses)
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string
	SMTPFrom     string
	SMTPTLSMode  string // "starttls" | "tls" | "none"

	PollInterval time.Duration

	// MailAllowFrom : liste blanche d'adresses autorisées à déclencher une
	// réponse automatique (vide = toutes les adresses sont autorisées).
	MailAllowFrom    []string
	MailMaxBodyChars int // troncature du corps du mail transmis au LLM

	// Tools (function calling)
	ChatToolsEnabled bool // mode chat : shell + fichiers + http (supervisé par un humain)
	MailToolsEnabled bool // mode mail : uniquement lecture fichier + http (jamais shell/écriture, contenu non fiable)

	// SandboxUserEnabled : si vrai, run_shell (mode chat uniquement) est
	// exécuté sous le compte système restreint "llm" (voir internal/sandbox)
	// plutôt que sous l'identité de l'utilisateur courant. Contrôle aussi le
	// contrôle d'accès de read_file/write_file : quand actif, un répertoire
	// est considéré autorisé si ce compte y a réellement accès au niveau OS
	// (voir tools.DirPermissions), pas seulement listé dans un fichier —
	// donc pas seulement "shell" malgré le nom. La configuration
	// (utilisateur/groupe système, règle sudo) est vérifiée à chaque
	// lancement et effectuée si besoin, avec confirmation explicite.
	SandboxUserEnabled bool

	// SandboxSSHKey : chemin d'une clé privée SSH de l'utilisateur courant,
	// à laquelle le compte sandbox "llm" reçoit un accès en lecture seule
	// (voir internal/sandbox.EnsureSSHKeyAccess) pour que les commandes git
	// exécutées via run_shell (clone/push/pull sur un dépôt distant en SSH)
	// puissent s'authentifier avec la véritable identité de l'utilisateur.
	// Affaiblit délibérément l'isolation du sandbox pour ce cas précis : "" =
	// désactivé (défaut si aucune clé usuelle n'est trouvée). Sans effet si
	// SandboxUserEnabled est faux.
	SandboxSSHKey string

	ToolsShellTimeout time.Duration
	// ToolsShellMaxTimeout : borne supérieure du timeout que le modèle peut
	// demander pour une commande run_shell précise (voir
	// internal/tools.ShellTool, paramètre "timeout_seconds") — sans plafond,
	// une commande interactive ou bloquante par erreur monopoliserait le
	// sandbox indéfiniment.
	ToolsShellMaxTimeout time.Duration
	// ToolsShellNotifyThreshold : durée d'exécution réelle au-delà de
	// laquelle une commande run_shell déclenche une notification de bureau à
	// la fin (voir internal/tools.ShellTool.NotifyThreshold, notify.go). <=
	// 0 = désactivé. Mode chat uniquement : ignoré en mode mail.
	ToolsShellNotifyThreshold time.Duration
	ToolsHTTPTimeout          time.Duration

	// ToolsClaude* : voir internal/tools.ClaudeTool (run_claude). Le tool
	// n'est proposé que si ToolsClaudeEnabled et que ToolsClaudeBin est
	// trouvé dans le PATH.
	ToolsClaudeEnabled        bool
	ToolsClaudeBin            string
	ToolsClaudePermissionMode string
	ToolsClaudeTimeout        time.Duration
	ToolsClaudeMaxTimeout     time.Duration
	// ToolsBrowserFetchTimeout : voir internal/tools.BrowserFetchTool.Timeout
	// — plus long que ToolsHTTPTimeout par défaut, le démarrage d'un
	// navigateur (et de geckodriver pour Firefox) prenant plus de temps
	// qu'une simple requête HTTP.
	ToolsBrowserFetchTimeout time.Duration
	AgentMaxSteps            int

	// AgentMaxConsecutiveShellFailures : nombre d'échecs run_shell d'affilée
	// (code de sortie non nul, timeout, ou erreur du tool lui-même — voir
	// agent.shellCallFailed) au-delà duquel le harnais injecte un message
	// poussant le modèle à arrêter d'insister sur la même approche et à en
	// chercher une différente, plutôt que de le laisser retenter
	// indéfiniment une commande qui échoue de la même façon. Remis à zéro
	// dès qu'un run_shell réussit. <= 0 = désactivé (jamais de message
	// injecté, quel que soit le nombre d'échecs).
	AgentMaxConsecutiveShellFailures int

	// AgentRealUserWindow : durée de la fenêtre ouverte quand le modèle
	// demande "as_real_user_window: true" sur run_shell et que
	// l'utilisateur confirme (voir internal/tools.ShellTool.ConfirmRealUser
	// et internal/chat.confirmRealUser) — les commandes "as_real_user"
	// suivantes sont alors autorisées sans redemander tant que la fenêtre
	// est ouverte. Sans "as_real_user_window", chaque commande
	// "as_real_user" redemande confirmation individuellement (mode
	// "oneshot"), quelle que soit cette valeur. <= 0 = défaut (5 min).
	AgentRealUserWindow time.Duration

	// WorkspaceDir : répertoire toujours accessible en lecture/écriture pour
	// read_file/write_file (voir internal/tools.DirPermissions.AlwaysAllow),
	// que ce soit en mode chat ou mail, sans passer par
	// request_directory_access. Défaut : "bot-workspace" dans le dossier
	// personnel de l'utilisateur courant.
	WorkspaceDir string

	// OntologyEnabled : active le graphe de connaissances (voir
	// internal/ontology) : tool query_ontology, et en mode chat extraction
	// automatique des concepts après OntologyIdle sans message utilisateur.
	OntologyEnabled bool
	// OntologyIdle : durée d'inactivité de l'utilisateur déclenchant
	// l'extraction. 0 = pas d'extraction automatique (la base reste
	// interrogeable).
	OntologyIdle time.Duration

	// Voice* / STT* : commande vocale du mode chat (voir internal/voice).
	// VoiceRecordCmd : commande de capture qui écrit du PCM s16 mono 16 kHz
	// brut sur sa sortie standard ; vide = pw-record. La transcription passe
	// toujours par le serveur du LLM (LLMBaseURL/LLMAPIKey), Unsloth Studio.
	VoiceEnabled          bool
	VoiceRecordCmd        []string
	VoiceMaxDuration      time.Duration
	VoiceSilenceStop      time.Duration
	VoiceSilenceThreshold float64
	// VoiceTTSEnabled / VoiceTTSCmd : lecture à voix haute des réponses du
	// bot quand le message venait de la commande vocale (jamais le
	// raisonnement ni les appels d'outils). VoiceTTSCmd reçoit le texte sur
	// son entrée standard ; défaut : espeak-ng avec la voix robosoft8.
	VoiceTTSEnabled bool
	VoiceTTSCmd     []string
	STTModel        string
	STTLanguage     string
	// STTEngine / STTDevice : moteur et emplacement du modèle de dictée sur
	// Unsloth Studio ("gguf" = whisper.cpp et "cpu" par défaut, pour laisser
	// tout le GPU au LLM ; "" = au choix du serveur). Voir voice.STT.
	STTEngine string
	STTDevice string
}

// SkillsDir retourne le sous-répertoire "skills" du workspace, où les
// skills sont déclarées (voir internal/skills).
func (c Config) SkillsDir() string {
	return filepath.Join(c.WorkspaceDir, "skills")
}

// ScratchpadDir retourne le sous-répertoire "scratchpad" du workspace,
// destiné aux fichiers de travail du modèle lui-même (brouillons, résultats
// intermédiaires...) — voir agent.WorkspacePrompt.
func (c Config) ScratchpadDir() string {
	return filepath.Join(c.WorkspaceDir, "scratchpad")
}

// MemoryFile retourne le chemin du fichier "MEMORY.md" à la racine du
// workspace : mémoire long terme du modèle, à la fois consultable et
// modifiable par lui (read_file/write_file, voir agent.WorkspacePrompt) et
// alimentée automatiquement par convo.Conversation.Compact quand elle
// identifie quelque chose qui mérite de survivre à la conversation en
// cours.
func (c Config) MemoryFile() string {
	return filepath.Join(c.WorkspaceDir, "MEMORY.md")
}

// OntologyFile retourne le chemin de la base du graphe de connaissances
// (ontology.db), à la racine du workspace.
func (c Config) OntologyFile() string {
	return filepath.Join(c.WorkspaceDir, "ontology.db")
}

// defaultWorkspaceDir retourne "<home>/bot-workspace", ou "bot-workspace"
// (relatif au répertoire courant) si le dossier personnel de l'utilisateur
// est introuvable.
func defaultWorkspaceDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "bot-workspace"
	}
	return filepath.Join(home, "bot-workspace")
}

// ConfigFilePath retourne le chemin du fichier de configuration (.env) du
// harnais — toujours dans le workspace, jamais relatif au répertoire depuis
// lequel le programme est lancé (qui change d'une invocation à l'autre,
// contrairement au workspace — même raison que WorkspaceDir/allowedDirsFile
// dans main.go : lancer ./bot depuis un dossier différent d'une fois sur
// l'autre ne doit "perdre" ni la config ni rien d'autre).
//
// Respecte une variable d'environnement WORKSPACE_DIR déjà présente au
// niveau du process (réglée par le shell/l'appelant, PAS par le fichier de
// config lui-même : sa propre localisation ne peut pas dépendre de son
// propre contenu, ce serait circulaire) ; sinon, ~/bot-workspace, comme la
// valeur par défaut de WorkspaceDir lui-même — cohérent : sans rien régler
// nulle part, le fichier de config attendu et le workspace effectif sont le
// même répertoire.
func ConfigFilePath() string {
	return filepath.Join(getenv("WORKSPACE_DIR", defaultWorkspaceDir()), sandbox.ConfigFileName)
}

// EnsureConfigFile crée path avec defaultContent s'il est absent — jamais
// s'il existe déjà, y compris vidé/modifié par l'utilisateur (même principe
// que skills.EnsureDefaults/MEMORY.md). Au tout premier lancement, ça
// initialise la configuration avec des valeurs d'exemple plutôt que de
// démarrer sans aucun fichier. Mode 0600 (pas 0644 comme MEMORY.md) : ce
// fichier contient potentiellement des secrets (clé API, mots de passe
// IMAP/SMTP...).
func EnsureConfigFile(path string, defaultContent []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("création de %q: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, defaultContent, 0o600); err != nil {
		return fmt.Errorf("création de %q: %w", path, err)
	}
	return nil
}

// defaultSandboxSSHKey retourne le premier fichier de clé privée SSH usuel
// trouvé dans ~/.ssh (ordre de préférence : ed25519, ecdsa, rsa), ou "" si
// aucun n'existe — auquel cas SandboxSSHKey reste désactivé par défaut
// (aucun changement de comportement pour qui ne s'en sert pas).
func defaultSandboxSSHKey() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		path := filepath.Join(home, ".ssh", name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

// LoadDotEnv lit un fichier .env (KEY=VALUE par ligne) s'il existe et
// définit les variables d'environnement correspondantes, sans écraser
// celles déjà présentes dans l'environnement.
//
// Lu via sandbox.ReadTrustedFile : le .env vit dans le workspace, accordé
// au compte sandbox — un .env recréé par lui (ex: SANDBOX_USER_ENABLED=false
// pour le lancement suivant) est refusé, et un .env resté accessible au
// groupe est remis en 0600.
func LoadDotEnv(path string) error {
	data, err := sandbox.ReadTrustedFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}
	return scanner.Err()
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// getenvAllowEmpty est comme getenv, mais une variable explicitement définie
// à vide dans l'environnement (LLM_API_KEY= par exemple) est respectée telle
// quelle plutôt que de retomber sur fallback : certains serveurs locaux
// rejettent tout en-tête Authorization non vide (jeton non reconnu), donc il
// faut pouvoir demander explicitement l'absence de clé.
func getenvAllowEmpty(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// parseList découpe une valeur d'environnement séparée par des virgules en
// une liste de tokens non vides, en les mettant en minuscules si lower=true
// (adapté aux adresses mail, pas aux chemins de fichiers sensibles à la casse).
func parseList(raw string, lower bool) []string {
	var out []string
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if lower {
			tok = strings.ToLower(tok)
		}
		out = append(out, tok)
	}
	return out
}

func getenvBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

// defaultMaxTokensShare : part de LLM_CONTEXT_TOKENS utilisée comme
// LLM_MAX_TOKENS quand celui-ci n'est pas défini.
const defaultMaxTokensShare = 0.5

// Load construit la configuration à partir des variables d'environnement.
func Load() Config {
	pollInterval, err := time.ParseDuration(getenv("POLL_INTERVAL", "60s"))
	if err != nil {
		pollInterval = 60 * time.Second
	}

	contextMaxTokens, err := strconv.Atoi(getenv("LLM_CONTEXT_TOKENS", "8192"))
	if err != nil || contextMaxTokens <= 0 {
		contextMaxTokens = 8192
	}

	// Comme TOOLS_SHELL_NOTIFY_THRESHOLD, 0 est une valeur valide (pas de
	// limite) : seule une erreur de parsing ou une valeur négative retombe
	// sur le défaut.
	llmTimeout, err := time.ParseDuration(getenv("LLM_TIMEOUT", "10m"))
	if err != nil || llmTimeout < 0 {
		llmTimeout = 10 * time.Minute
	}

	// Comme LLM_TIMEOUT, 0 est une valeur valide (pas de limite). Le défaut
	// suit la taille du contexte plutôt qu'une valeur fixe, qui pouvait
	// dépasser la fenêtre entière d'un petit contexte ou brider un grand.
	defaultMaxTokens := max(1, int(float64(contextMaxTokens)*defaultMaxTokensShare))
	llmMaxTokens, err := strconv.Atoi(getenv("LLM_MAX_TOKENS", strconv.Itoa(defaultMaxTokens)))
	if err != nil || llmMaxTokens < 0 {
		llmMaxTokens = defaultMaxTokens
	}

	llmMaxRetries, err := strconv.Atoi(getenv("LLM_MAX_RETRIES", "5"))
	if err != nil || llmMaxRetries < 0 {
		llmMaxRetries = 5
	}

	ontologyIdle, err := time.ParseDuration(getenv("ONTOLOGY_IDLE", "5m"))
	if err != nil || ontologyIdle < 0 {
		ontologyIdle = 5 * time.Minute
	}

	contextCompactAt, err := strconv.ParseFloat(getenv("CONTEXT_COMPACT_AT", "0.95"), 64)
	if err != nil || contextCompactAt <= 0 || contextCompactAt > 1 {
		contextCompactAt = 0.95
	}

	contextKeepLast, err := strconv.Atoi(getenv("CONTEXT_KEEP_LAST", "6"))
	if err != nil || contextKeepLast < 0 {
		contextKeepLast = 6
	}

	mailMaxBodyChars, err := strconv.Atoi(getenv("MAIL_MAX_BODY_CHARS", "12000"))
	if err != nil || mailMaxBodyChars <= 0 {
		mailMaxBodyChars = 12000
	}

	mailAllowFrom := parseList(getenv("MAIL_ALLOW_FROM", ""), true)

	toolsShellTimeout, err := time.ParseDuration(getenv("TOOLS_SHELL_TIMEOUT", "30s"))
	if err != nil || toolsShellTimeout <= 0 {
		toolsShellTimeout = 30 * time.Second
	}
	toolsShellMaxTimeout, err := time.ParseDuration(getenv("TOOLS_SHELL_MAX_TIMEOUT", "10m"))
	if err != nil || toolsShellMaxTimeout <= 0 {
		toolsShellMaxTimeout = 10 * time.Minute
	}
	// Contrairement aux timeouts ci-dessus, 0 est ici une valeur valide et
	// intentionnelle (désactive les notifications) : seule une erreur de
	// parsing retombe sur le défaut, pas une valeur <= 0 explicitement posée.
	toolsShellNotifyThreshold, err := time.ParseDuration(getenv("TOOLS_SHELL_NOTIFY_THRESHOLD", "20s"))
	if err != nil {
		toolsShellNotifyThreshold = 20 * time.Second
	}
	toolsHTTPTimeout, err := time.ParseDuration(getenv("TOOLS_HTTP_TIMEOUT", "20s"))
	if err != nil || toolsHTTPTimeout <= 0 {
		toolsHTTPTimeout = 20 * time.Second
	}
	toolsBrowserFetchTimeout, err := time.ParseDuration(getenv("TOOLS_BROWSER_FETCH_TIMEOUT", "45s"))
	if err != nil || toolsBrowserFetchTimeout <= 0 {
		toolsBrowserFetchTimeout = 45 * time.Second
	}
	toolsClaudeTimeout, err := time.ParseDuration(getenv("TOOLS_CLAUDE_TIMEOUT", "10m"))
	if err != nil || toolsClaudeTimeout <= 0 {
		toolsClaudeTimeout = 10 * time.Minute
	}
	toolsClaudeMaxTimeout, err := time.ParseDuration(getenv("TOOLS_CLAUDE_MAX_TIMEOUT", "30m"))
	if err != nil || toolsClaudeMaxTimeout <= 0 {
		toolsClaudeMaxTimeout = 30 * time.Minute
	}
	agentMaxSteps, err := strconv.Atoi(getenv("AGENT_MAX_STEPS", "8"))
	if err != nil || agentMaxSteps <= 0 {
		agentMaxSteps = 8
	}
	// Pas de repli sur une valeur par défaut si la variable est absente et
	// que la conversion échoue pour une AUTRE raison qu'une valeur non
	// numérique volontaire : contrairement à agentMaxSteps ci-dessus (dont 0
	// n'a pas de sens et doit retomber sur le défaut), 0 est une valeur
	// valide ici (désactive la fonctionnalité) — seule une valeur vraiment
	// non numérique retombe sur le défaut.
	agentMaxConsecutiveShellFailures, err := strconv.Atoi(getenv("AGENT_MAX_CONSECUTIVE_SHELL_FAILURES", "5"))
	if err != nil {
		agentMaxConsecutiveShellFailures = 5
	}
	agentRealUserWindow, err := time.ParseDuration(getenv("AGENT_REAL_USER_WINDOW", "5m"))
	if err != nil || agentRealUserWindow <= 0 {
		agentRealUserWindow = 5 * time.Minute
	}
	voiceMaxDuration, err := time.ParseDuration(getenv("VOICE_MAX_DURATION", "60s"))
	if err != nil || voiceMaxDuration <= 0 {
		voiceMaxDuration = 60 * time.Second
	}
	// 0 est valide (désactive l'arrêt sur silence).
	voiceSilenceStop, err := time.ParseDuration(getenv("VOICE_SILENCE_STOP", "2s"))
	if err != nil || voiceSilenceStop < 0 {
		voiceSilenceStop = 2 * time.Second
	}
	voiceSilenceThreshold, err := strconv.ParseFloat(getenv("VOICE_SILENCE_THRESHOLD", "500"), 64)
	if err != nil || voiceSilenceThreshold < 0 {
		voiceSilenceThreshold = 500
	}

	return Config{
		LLMBaseURL:    getenv("LLM_BASE_URL", "http://localhost:8080/v1"),
		LLMAPIKey:     getenvAllowEmpty("LLM_API_KEY", "sk-local"),
		LLMModel:      getenv("LLM_MODEL", "local-model"),
		SystemPrompt:  getenv("SYSTEM_PROMPT", "Tu es un assistant utile et concis."),
		LLMTimeout:    llmTimeout,
		LLMMaxTokens:  llmMaxTokens,
		LLMMaxRetries: llmMaxRetries,

		ContextMaxTokens:   contextMaxTokens,
		ContextCompactAt:   contextCompactAt,
		ContextKeepLastMsg: contextKeepLast,

		IMAPHost:     getenv("IMAP_HOST", ""),
		IMAPPort:     getenv("IMAP_PORT", "993"),
		IMAPUser:     getenv("IMAP_USER", ""),
		IMAPPassword: getenv("IMAP_PASSWORD", ""),
		IMAPMailbox:  getenv("IMAP_MAILBOX", "INBOX"),
		IMAPTLS:      getenvBool("IMAP_TLS", true),

		SMTPHost:     getenv("SMTP_HOST", ""),
		SMTPPort:     getenv("SMTP_PORT", "587"),
		SMTPUser:     getenv("SMTP_USER", ""),
		SMTPPassword: getenv("SMTP_PASSWORD", ""),
		SMTPFrom:     getenv("SMTP_FROM", ""),
		SMTPTLSMode:  getenv("SMTP_TLS_MODE", "starttls"),

		PollInterval: pollInterval,

		MailAllowFrom:    mailAllowFrom,
		MailMaxBodyChars: mailMaxBodyChars,

		ChatToolsEnabled: getenvBool("CHAT_TOOLS_ENABLED", true),
		MailToolsEnabled: getenvBool("MAIL_TOOLS_ENABLED", false),

		SandboxUserEnabled: getenvBool("SANDBOX_USER_ENABLED", true),
		SandboxSSHKey:      getenv("SANDBOX_SSH_KEY", defaultSandboxSSHKey()),

		ToolsShellTimeout:                toolsShellTimeout,
		ToolsShellMaxTimeout:             toolsShellMaxTimeout,
		ToolsShellNotifyThreshold:        toolsShellNotifyThreshold,
		ToolsHTTPTimeout:                 toolsHTTPTimeout,
		ToolsBrowserFetchTimeout:         toolsBrowserFetchTimeout,
		ToolsClaudeEnabled:               getenvBool("TOOLS_CLAUDE_ENABLED", true),
		ToolsClaudeBin:                   getenv("TOOLS_CLAUDE_BIN", "claude"),
		ToolsClaudePermissionMode:        getenv("TOOLS_CLAUDE_PERMISSION_MODE", "auto"),
		ToolsClaudeTimeout:               toolsClaudeTimeout,
		ToolsClaudeMaxTimeout:            toolsClaudeMaxTimeout,
		AgentMaxSteps:                    agentMaxSteps,
		AgentMaxConsecutiveShellFailures: agentMaxConsecutiveShellFailures,
		AgentRealUserWindow:              agentRealUserWindow,

		WorkspaceDir: getenv("WORKSPACE_DIR", defaultWorkspaceDir()),

		OntologyEnabled: getenvBool("ONTOLOGY_ENABLED", true),
		OntologyIdle:    ontologyIdle,

		VoiceEnabled:          getenvBool("VOICE_ENABLED", false),
		VoiceRecordCmd:        strings.Fields(getenv("VOICE_RECORD_CMD", "")),
		VoiceMaxDuration:      voiceMaxDuration,
		VoiceSilenceStop:      voiceSilenceStop,
		VoiceSilenceThreshold: voiceSilenceThreshold,
		VoiceTTSEnabled:       getenvBool("VOICE_TTS_ENABLED", true),
		VoiceTTSCmd:           strings.Fields(getenv("VOICE_TTS_CMD", "espeak-ng -v fr+robosoft8 -p 30 -s 130")),
		STTModel:              getenv("STT_MODEL", "large-v3-turbo"),
		STTLanguage:           getenv("STT_LANGUAGE", "fr"),
		STTEngine:             getenvAllowEmpty("STT_ENGINE", "gguf"),
		STTDevice:             getenvAllowEmpty("STT_DEVICE", "cpu"),
	}
}

// RequireMail vérifie que les variables nécessaires au mode mail sont présentes.
func (c Config) RequireMail() error {
	missing := []string{}
	if c.IMAPHost == "" {
		missing = append(missing, "IMAP_HOST")
	}
	if c.IMAPUser == "" {
		missing = append(missing, "IMAP_USER")
	}
	if c.IMAPPassword == "" {
		missing = append(missing, "IMAP_PASSWORD")
	}
	if c.SMTPHost == "" {
		missing = append(missing, "SMTP_HOST")
	}
	if c.SMTPUser == "" {
		missing = append(missing, "SMTP_USER")
	}
	if c.SMTPPassword == "" {
		missing = append(missing, "SMTP_PASSWORD")
	}
	if c.SMTPFrom == "" {
		missing = append(missing, "SMTP_FROM")
	}
	if len(missing) > 0 {
		return fmt.Errorf("variables d'environnement manquantes pour le mode mail: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Package config charge la configuration du harnais depuis les variables
// d'environnement (et un éventuel fichier .env local).
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// LLM (API compatible OpenAI, endpoint local)
	LLMBaseURL   string // ex: http://localhost:8000/v1
	LLMAPIKey    string // souvent une valeur factice pour un serveur local
	LLMModel     string
	SystemPrompt string

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
	// 0 = désactivé.
	ToolsShellNotifyThreshold time.Duration
	ToolsHTTPTimeout          time.Duration
	// ToolsBrowserFetchTimeout : voir internal/tools.BrowserFetchTool.Timeout
	// — plus long que ToolsHTTPTimeout par défaut, le démarrage d'un
	// navigateur (et de geckodriver pour Firefox) prenant plus de temps
	// qu'une simple requête HTTP.
	ToolsBrowserFetchTimeout time.Duration
	AgentMaxSteps            int

	// WorkspaceDir : répertoire toujours accessible en lecture/écriture pour
	// read_file/write_file (voir internal/tools.DirPermissions.AlwaysAllow),
	// que ce soit en mode chat ou mail, sans passer par
	// request_directory_access. Défaut : "bot-workspace" dans le dossier
	// personnel de l'utilisateur courant.
	WorkspaceDir string
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
	return filepath.Join(getenv("WORKSPACE_DIR", defaultWorkspaceDir()), ".env")
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
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
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
	agentMaxSteps, err := strconv.Atoi(getenv("AGENT_MAX_STEPS", "8"))
	if err != nil || agentMaxSteps <= 0 {
		agentMaxSteps = 8
	}

	return Config{
		LLMBaseURL:   getenv("LLM_BASE_URL", "http://localhost:8080/v1"),
		LLMAPIKey:    getenvAllowEmpty("LLM_API_KEY", "sk-local"),
		LLMModel:     getenv("LLM_MODEL", "local-model"),
		SystemPrompt: getenv("SYSTEM_PROMPT", "Tu es un assistant utile et concis."),

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

		ToolsShellTimeout:         toolsShellTimeout,
		ToolsShellMaxTimeout:      toolsShellMaxTimeout,
		ToolsShellNotifyThreshold: toolsShellNotifyThreshold,
		ToolsHTTPTimeout:          toolsHTTPTimeout,
		ToolsBrowserFetchTimeout:  toolsBrowserFetchTimeout,
		AgentMaxSteps:             agentMaxSteps,

		WorkspaceDir: getenv("WORKSPACE_DIR", defaultWorkspaceDir()),
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

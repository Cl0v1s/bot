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

	// ShellSandboxEnabled : si vrai, run_shell (mode chat uniquement) est
	// exécuté sous le compte système restreint "llm" (voir internal/sandbox)
	// plutôt que sous l'identité de l'utilisateur courant. La configuration
	// (utilisateur/groupe système, règle sudo) est vérifiée à chaque
	// lancement et effectuée si besoin, avec confirmation explicite.
	ShellSandboxEnabled bool

	ToolsShellTimeout time.Duration
	ToolsHTTPTimeout  time.Duration
	AgentMaxSteps     int

	// WorkspaceDir : répertoire toujours accessible en lecture/écriture pour
	// read_file/write_file (voir internal/tools.DirPermissions.AlwaysAllow),
	// que ce soit en mode chat ou mail, sans passer par
	// request_directory_access. Défaut : "bot-workspace" dans le dossier
	// personnel de l'utilisateur courant.
	WorkspaceDir string
}

// SkillsDir retourne le sous-répertoire "skills" du workspace, où les skills
// sont déclarées (voir internal/skills).
func (c Config) SkillsDir() string {
	return filepath.Join(c.WorkspaceDir, "skills")
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

	contextCompactAt, err := strconv.ParseFloat(getenv("CONTEXT_COMPACT_AT", "0.9"), 64)
	if err != nil || contextCompactAt <= 0 || contextCompactAt > 1 {
		contextCompactAt = 0.9
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
	toolsHTTPTimeout, err := time.ParseDuration(getenv("TOOLS_HTTP_TIMEOUT", "20s"))
	if err != nil || toolsHTTPTimeout <= 0 {
		toolsHTTPTimeout = 20 * time.Second
	}
	agentMaxSteps, err := strconv.Atoi(getenv("AGENT_MAX_STEPS", "8"))
	if err != nil || agentMaxSteps <= 0 {
		agentMaxSteps = 8
	}

	return Config{
		LLMBaseURL:   getenv("LLM_BASE_URL", "http://localhost:8080/v1"),
		LLMAPIKey:    getenv("LLM_API_KEY", "sk-local"),
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

		ShellSandboxEnabled: getenvBool("SHELL_SANDBOX_USER_ENABLED", true),

		ToolsShellTimeout: toolsShellTimeout,
		ToolsHTTPTimeout:  toolsHTTPTimeout,
		AgentMaxSteps:     agentMaxSteps,

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

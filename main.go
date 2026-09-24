package main

import (
	"context"
	_ "embed" // pour go:embed defaultEnvContent ci-dessous
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"bot/internal/agent"
	"bot/internal/chat"
	"bot/internal/config"
	"bot/internal/convo"
	"bot/internal/llm"
	"bot/internal/mailbot"
	"bot/internal/sandbox"
	"bot/internal/skills"
	"bot/internal/smtpclient"
	"bot/internal/tools"
)

// defaultEnvContent : contenu du fichier de config créé au tout premier
// lancement (voir config.EnsureConfigFile) — le même .env.example que celui
// versionné à la racine du dépôt, embarqué dans le binaire pour qu'il soit
// disponible même une fois le dépôt source absent de la machine cible (le
// binaire est censé pouvoir tourner seul).
//
//go:embed .env.example
var defaultEnvContent []byte

func main() {
	// Toujours dans le workspace, jamais relatif au répertoire courant (voir
	// config.ConfigFilePath) : lancer ./bot depuis un dossier différent d'une
	// fois sur l'autre ne doit pas faire "perdre" la configuration. Créé avec
	// des valeurs d'exemple si c'est la toute première fois qu'il est cherché
	// à cet emplacement.
	configPath := config.ConfigFilePath()
	if err := config.EnsureConfigFile(configPath, defaultEnvContent); err != nil {
		log.Fatalf("initialisation de la configuration (%s): %v", configPath, err)
	}
	if err := config.LoadDotEnv(configPath); err != nil {
		log.Fatalf("chargement de %s: %v", configPath, err)
	}
	cfg := config.Load()

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	// Le workspace (et son sous-répertoire skills/) est créé au démarrage et
	// toujours accessible en lecture/écriture au modèle (voir
	// tools.DirPermissions.AlwaysAllow ci-dessous), qu'il s'agisse du mode
	// chat ou du mode mail, sans passer par request_directory_access.
	if err := os.MkdirAll(cfg.SkillsDir(), 0o755); err != nil {
		log.Fatalf("création du workspace %q: %v", cfg.WorkspaceDir, err)
	}
	// scratchpad/ : fichiers de travail du modèle lui-même (voir
	// agent.WorkspacePrompt) — créé dès le démarrage comme skills/, pour
	// qu'il existe avant même que le modèle y écrive quoi que ce soit.
	if err := os.MkdirAll(cfg.ScratchpadDir(), 0o755); err != nil {
		log.Fatalf("création du scratchpad %q: %v", cfg.ScratchpadDir(), err)
	}
	// Le dossier skills/ contient toujours d'office une skill expliquant
	// comment en déclarer de nouvelles (voir skills.EnsureDefaults).
	if err := skills.EnsureDefaults(cfg.SkillsDir()); err != nil {
		log.Printf("création de la skill par défaut (%s): %v", cfg.SkillsDir(), err)
	}
	// MEMORY.md : mémoire long terme du modèle (voir agent.WorkspacePrompt
	// et convo.Conversation.MemoryFile), créée avec un en-tête minimal si
	// absente — jamais réécrite si déjà présente, pour ne rien perdre de ce
	// que le modèle (ou l'utilisateur) y aurait déjà consigné.
	if err := ensureMemoryFile(cfg.MemoryFile()); err != nil {
		log.Printf("création de la mémoire (%s): %v", cfg.MemoryFile(), err)
	}
	memoryContent, err := readMemorySnippet(cfg.MemoryFile(), maxMemoryPromptBytes)
	if err != nil {
		log.Printf("lecture de la mémoire (%s): %v", cfg.MemoryFile(), err)
	}
	loadedSkills, err := skills.Load(cfg.SkillsDir())
	if err != nil {
		log.Printf("chargement des skills (%s): %v", cfg.SkillsDir(), err)
	}
	systemPrompt := cfg.SystemPrompt
	if summary := skills.Summary(loadedSkills); summary != "" {
		systemPrompt += "\n\n" + summary
	}
	// Ajoutés systématiquement (indépendant des outils, contrairement à
	// agent.ToolUsagePrompt qui n'est ajouté que si des outils sont
	// effectivement proposés) : voir agent.ReflectionPrompt/WorkspacePrompt.
	systemPrompt += "\n\n" + agent.ReflectionPrompt
	systemPrompt += "\n\n" + agent.WorkspacePrompt(cfg.WorkspaceDir, cfg.SkillsDir(), cfg.ScratchpadDir(), cfg.MemoryFile(), memoryContent)

	client := llm.New(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
	client.HTTPClient.Timeout = cfg.LLMTimeout
	client.ContextTokens = cfg.ContextMaxTokens


	// Dans le workspace, pas relatif au répertoire depuis lequel ./bot est
	// lancé : sinon, lancer le programme depuis un dossier différent d'une
	// fois sur l'autre fait "oublier" les répertoires déjà accordés (rien à
	// voir avec le workspace lui-même, qui a un chemin absolu stable).
	allowedDirsFile := filepath.Join(cfg.WorkspaceDir, tools.DefaultAllowedDirsFile)

	switch os.Args[1] {
	case "chat":
		conv := convo.New(systemPrompt, cfg.ContextMaxTokens, cfg.ContextCompactAt, cfg.ContextKeepLastMsg)
		conv.MemoryFile = cfg.MemoryFile()

		toolsCfg := chat.ToolsConfig{
			Enabled:                     cfg.ChatToolsEnabled,
			AllowedDirsFile:             allowedDirsFile,
			SandboxUserEnabled:          cfg.SandboxUserEnabled,
			ShellTimeout:                cfg.ToolsShellTimeout,
			ShellMaxTimeout:             cfg.ToolsShellMaxTimeout,
			ShellNotifyThreshold:        cfg.ToolsShellNotifyThreshold,
			ClaudeEnabled:               cfg.ToolsClaudeEnabled,
			ClaudeBin:                   cfg.ToolsClaudeBin,
			ClaudePermissionMode:        cfg.ToolsClaudePermissionMode,
			ClaudeTimeout:               cfg.ToolsClaudeTimeout,
			ClaudeMaxTimeout:            cfg.ToolsClaudeMaxTimeout,
			HTTPTimeout:                 cfg.ToolsHTTPTimeout,
			BrowserFetchTimeout:         cfg.ToolsBrowserFetchTimeout,
			MaxSteps:                    cfg.AgentMaxSteps,
			MaxConsecutiveShellFailures: cfg.AgentMaxConsecutiveShellFailures,
			RealUserWindow:              cfg.AgentRealUserWindow,
			WorkspaceDir:                cfg.WorkspaceDir,
			SandboxSSHKey:               cfg.SandboxSSHKey,
		}
		// Pas de contexte dérivé d'un signal ici : chat.Run gère lui-même
		// Ctrl+C/SIGTERM (annulation de la requête en cours si une requête
		// est en cours, sortie immédiate sinon). Un ctx pré-annulé par un
		// premier signal casserait silencieusement toutes les requêtes
		// suivantes.
		if err := chat.Run(context.Background(), client, conv, toolsCfg, os.Stdin, os.Stdout); err != nil {
			log.Fatalf("mode chat: %v", err)
		}

	case "mail":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		if err := cfg.RequireMail(); err != nil {
			log.Fatalf("configuration invalide: %v", err)
		}
		smtp := &smtpclient.Client{
			Host:     cfg.SMTPHost,
			Port:     cfg.SMTPPort,
			User:     cfg.SMTPUser,
			Password: cfg.SMTPPassword,
			From:     cfg.SMTPFrom,
			TLSMode:  cfg.SMTPTLSMode,
		}
		opts := mailbot.Options{
			IMAPHost:     cfg.IMAPHost,
			IMAPPort:     cfg.IMAPPort,
			IMAPTLS:      cfg.IMAPTLS,
			IMAPUser:     cfg.IMAPUser,
			IMAPPassword: cfg.IMAPPassword,
			IMAPMailbox:  cfg.IMAPMailbox,
			SMTP:         smtp,
			PollInterval: cfg.PollInterval,
			SystemPrompt: systemPrompt,

			ContextMaxTokens: cfg.ContextMaxTokens,
			ContextCompactAt: cfg.ContextCompactAt,
			ContextKeepLast:  cfg.ContextKeepLastMsg,
			MemoryFile:       cfg.MemoryFile(),

			AllowFrom:    cfg.MailAllowFrom,
			MaxBodyChars: cfg.MailMaxBodyChars,

			AgentMaxSteps:                    cfg.AgentMaxSteps,
			AgentMaxConsecutiveShellFailures: cfg.AgentMaxConsecutiveShellFailures,
		}
		if cfg.MailToolsEnabled {
			// Mode mail : pas d'humain pour approuver une demande d'accès en
			// direct (grant=nil) — request_directory_access y renverra donc
			// toujours un refus explicite. Les répertoires accessibles sont
			// exactement ceux accordés dynamiquement ailleurs (typiquement
			// via le mode chat), relus depuis le fichier partagé à chaque
			// cycle de poll (voir pollOnce) : le mode mail ne peut jamais lui
			// -même en ajouter. ATTENTION : le corps du mail est un contenu
			// non fiable (voir MAIL_ALLOW_FROM, lui-même vulnérable au
			// spoofing de l'en-tête From) — exposer run_shell/write_file ici
			// est un vecteur d'exécution de code par injection de prompt,
			// accepté en connaissance de cause à la demande explicite de
			// l'opérateur de ce bot.
			perms := tools.NewDirPermissions(nil)
			if err := perms.WithPersistence(allowedDirsFile); err != nil {
				log.Printf("mode mail: lecture des répertoires autorisés: %v", err)
			}
			// Le workspace (et /tmp, pour les fichiers vraiment éphémères —
			// voir agent.WorkspacePrompt) reste accessible même après un
			// Refresh() (voir pollOnce), contrairement aux répertoires
			// accordés dynamiquement.
			// "/tmp" explicitement : sur macOS, os.TempDir() est $TMPDIR
			// (/var/folders/..., privé à l'utilisateur), pas /tmp.
			perms.AlwaysAllow(cfg.WorkspaceDir, "/tmp", os.TempDir())

			toolList := []tools.Tool{
				&tools.ReadFileTool{Perms: perms},
				&tools.HTTPGetTool{Timeout: cfg.ToolsHTTPTimeout},
				&tools.BrowserFetchTool{Timeout: cfg.ToolsBrowserFetchTimeout},
				&tools.RequestDirectoryAccessTool{Perms: perms},
				// list_dir n'est soumis à aucune permission (voir son
				// commentaire) : n'expose que des noms d'entrées, jamais de
				// contenu, jugé acceptable même piloté par un mail entrant
				// non fiable (voir l'avertissement MAIL_TOOLS_ENABLED
				// ci-dessus).
				&tools.ListDirTool{},
			}

			// run_shell en mode mail : jamais de repli silencieux vers une
			// exécution non isolée. Si SANDBOX_USER_ENABLED est actif,
			// le sandbox doit déjà avoir été configuré (typiquement en ayant
			// lancé le mode chat au moins une fois) — mode mail n'ayant pas
			// de console, il ne peut pas demander le mot de passe sudo requis
			// pour le mettre en place lui-même. Sinon, run_shell n'est pas
			// proposé du tout cette session.
			shellAvailable := !cfg.SandboxUserEnabled
			sandboxReady := false
			if cfg.SandboxUserEnabled {
				if sandbox.Ready() {
					sandboxReady = true
					shellAvailable = true
				} else {
					log.Printf("mode mail: sandbox %q non configuré (lancez le mode chat au moins une fois pour le mettre en place), run_shell non proposé", sandbox.User)
				}
			}
			gitConfigPath := ""
			homeDir := ""
			if sandboxReady {
				if cfg.SandboxSSHKey != "" {
					if err := sandbox.EnsureSSHKeyAccess(cfg.SandboxSSHKey); err != nil {
						log.Printf("mode mail: échec de l'octroi d'accès à la clé SSH (%s) : %v", cfg.SandboxSSHKey, err)
					}
				}
				if err := sandbox.EnsureGitConfig(cfg.WorkspaceDir, cfg.SandboxSSHKey); err != nil {
					log.Printf("mode mail: échec de la préparation de la config git (%s) : %v", cfg.WorkspaceDir, err)
				}
				// Doit être créé AVANT GrantDirectory : voir le commentaire
				// équivalent dans repl.go.
				if err := sandbox.EnsureSandboxHome(cfg.WorkspaceDir); err != nil {
					log.Printf("mode mail: échec de la préparation du HOME sandbox (%s) : %v", cfg.WorkspaceDir, err)
				}
				if err := sandbox.GrantDirectory(cfg.WorkspaceDir); err != nil {
					log.Printf("mode mail: échec de l'ouverture du workspace (%s) au compte %q : %v", cfg.WorkspaceDir, sandbox.User, err)
				} else {
					gitConfigPath = sandbox.GitConfigPath(cfg.WorkspaceDir)
					homeDir = sandbox.SandboxHomeDir(cfg.WorkspaceDir)
				}
			}
			if shellAvailable {
				// Pas de NotifyThreshold en mode mail : le bot tourne sans
				// utilisateur devant l'écran, une notification de bureau n'a
				// pas de sens (0 = notifications désactivées).
				toolList = append(toolList, &tools.ShellTool{Timeout: cfg.ToolsShellTimeout, MaxTimeout: cfg.ToolsShellMaxTimeout, Sandboxed: sandboxReady, Perms: perms, GitConfigPath: gitConfigPath, HomeDir: homeDir})
			}
			// run_claude tourne toujours sous l'identité réelle (voir
			// tools.ClaudeTool) : jamais proposé si le sandbox est requis,
			// pour la même raison que run_shell ci-dessus — et pas de
			// confirmation possible, personne n'étant là pour répondre.
			if cfg.ToolsClaudeEnabled && !cfg.SandboxUserEnabled {
				if _, err := exec.LookPath(cfg.ToolsClaudeBin); err != nil {
					log.Printf("mode mail: %q introuvable, run_claude non proposé", cfg.ToolsClaudeBin)
				} else {
					toolList = append(toolList, &tools.ClaudeTool{Bin: cfg.ToolsClaudeBin, PermissionMode: cfg.ToolsClaudePermissionMode, Timeout: cfg.ToolsClaudeTimeout, MaxTimeout: cfg.ToolsClaudeMaxTimeout, Perms: perms})
				}
			}

			opts.Tools = tools.NewRegistry(toolList...)
			opts.ToolsPerms = perms
		}
		if err := mailbot.Run(ctx, client, opts); err != nil {
			log.Fatalf("mode mail: %v", err)
		}

	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bot <chat|mail>")
	fmt.Fprintln(os.Stderr, "  chat  démarre une session interactive dans la console")
	fmt.Fprintln(os.Stderr, "  mail  démarre la boucle de lecture/réponse automatique aux mails")
}

// ensureMemoryFile crée path (MEMORY.md du workspace) avec un en-tête
// minimal s'il est absent. N'écrase jamais un fichier déjà présent (y
// compris modifié ou vidé par le modèle ou l'utilisateur) : seule son
// absence déclenche la création, même principe que skills.EnsureDefaults.
func ensureMemoryFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	const header = "# Mémoire\n\nFaits, préférences et contraintes à retenir d'une conversation à l'autre.\n"
	return os.WriteFile(path, []byte(header), 0o644)
}

// maxMemoryPromptBytes borne la taille du contenu de MEMORY.md injecté
// directement dans le prompt système (voir agent.WorkspacePrompt) : un
// fichier alimenté au fil de nombreuses compactions/sessions pourrait sinon
// finir par peser significativement sur le contexte à chaque tour.
const maxMemoryPromptBytes = 8000

// readMemorySnippet lit le contenu de path (MEMORY.md), tronqué à maxBytes
// si besoin (avec une note invitant à le relire directement pour la suite).
// Un fichier absent n'est pas une erreur : "" (mémoire vide), pas un échec —
// ensureMemoryFile est censé l'avoir déjà créé de toute façon.
func readMemorySnippet(path string, maxBytes int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	content := string(data)
	if len(content) > maxBytes {
		content = content[:maxBytes] + "\n[... mémoire tronquée ici, lis le fichier directement (read_file) pour la suite ...]"
	}
	return strings.TrimSpace(content), nil
}

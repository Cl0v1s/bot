package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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

func main() {
	if err := config.LoadDotEnv(".env"); err != nil {
		log.Fatalf("chargement .env: %v", err)
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
	// Le dossier skills/ contient toujours d'office une skill expliquant
	// comment en déclarer de nouvelles (voir skills.EnsureDefaults).
	if err := skills.EnsureDefaults(cfg.SkillsDir()); err != nil {
		log.Printf("création de la skill par défaut (%s): %v", cfg.SkillsDir(), err)
	}
	loadedSkills, err := skills.Load(cfg.SkillsDir())
	if err != nil {
		log.Printf("chargement des skills (%s): %v", cfg.SkillsDir(), err)
	}
	systemPrompt := cfg.SystemPrompt
	if summary := skills.Summary(loadedSkills); summary != "" {
		systemPrompt += "\n\n" + summary
	}

	client := llm.New(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)

	// Dans le workspace, pas relatif au répertoire depuis lequel ./bot est
	// lancé : sinon, lancer le programme depuis un dossier différent d'une
	// fois sur l'autre fait "oublier" les répertoires déjà accordés (rien à
	// voir avec le workspace lui-même, qui a un chemin absolu stable).
	allowedDirsFile := filepath.Join(cfg.WorkspaceDir, tools.DefaultAllowedDirsFile)

	switch os.Args[1] {
	case "chat":
		conv := convo.New(systemPrompt, cfg.ContextMaxTokens, cfg.ContextCompactAt, cfg.ContextKeepLastMsg)

		toolsCfg := chat.ToolsConfig{
			Enabled:             cfg.ChatToolsEnabled,
			AllowedDirsFile:     allowedDirsFile,
			ShellSandboxEnabled: cfg.ShellSandboxEnabled,
			ShellTimeout:        cfg.ToolsShellTimeout,
			HTTPTimeout:         cfg.ToolsHTTPTimeout,
			MaxSteps:            cfg.AgentMaxSteps,
			WorkspaceDir:        cfg.WorkspaceDir,
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

			AllowFrom:    cfg.MailAllowFrom,
			MaxBodyChars: cfg.MailMaxBodyChars,

			AgentMaxSteps: cfg.AgentMaxSteps,
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
			// Le workspace reste accessible même après un Refresh() (voir
			// pollOnce), contrairement aux répertoires accordés dynamiquement.
			perms.AlwaysAllow(cfg.WorkspaceDir)

			toolList := []tools.Tool{
				&tools.ReadFileTool{Perms: perms},
				&tools.HTTPGetTool{Timeout: cfg.ToolsHTTPTimeout},
				&tools.RequestDirectoryAccessTool{Perms: perms},
			}

			// run_shell en mode mail : jamais de repli silencieux vers une
			// exécution non isolée. Si SHELL_SANDBOX_USER_ENABLED est actif,
			// le sandbox doit déjà avoir été configuré (typiquement en ayant
			// lancé le mode chat au moins une fois) — mode mail n'ayant pas
			// de console, il ne peut pas demander le mot de passe sudo requis
			// pour le mettre en place lui-même. Sinon, run_shell n'est pas
			// proposé du tout cette session.
			shellAvailable := !cfg.ShellSandboxEnabled
			sandboxReady := false
			if cfg.ShellSandboxEnabled {
				if sandbox.Ready() {
					sandboxReady = true
					shellAvailable = true
				} else {
					log.Printf("mode mail: sandbox %q non configuré (lancez le mode chat au moins une fois pour le mettre en place), run_shell non proposé", sandbox.User)
				}
			}
			if sandboxReady {
				if err := sandbox.GrantDirectory(cfg.WorkspaceDir); err != nil {
					log.Printf("mode mail: échec de l'ouverture du workspace (%s) au compte %q : %v", cfg.WorkspaceDir, sandbox.User, err)
				}
			}
			if shellAvailable {
				toolList = append(toolList, &tools.ShellTool{Timeout: cfg.ToolsShellTimeout, Sandboxed: sandboxReady, Perms: perms})
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

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// claudeNonInteractivePrompt est ajouté au prompt système de Claude Code
// (--append-system-prompt) à chaque appel : en mode -p, personne n'est là
// pour répondre à une question, qui terminerait simplement l'exécution sans
// que la tâche soit faite.
const claudeNonInteractivePrompt = "Tu es exécuté en mode non interactif, piloté par un autre agent : personne ne peut répondre à tes questions. Ne pose AUCUNE question et ne demande aucune confirmation ni clarification. Fais directement la tâche demandée jusqu'au bout, en faisant toi-même les choix raisonnables quand quelque chose est ambigu. Termine par un résumé concis de ce que tu as fait, des choix que tu as faits, et de ce qui a éventuellement échoué ou reste à faire."

// ClaudeTool délègue une tâche à Claude Code en mode non interactif
// (`claude -p`), dans un répertoire de travail donné.
//
// Toujours exécuté sous l'identité réelle, jamais sous le compte sandbox :
// Claude Code a besoin des identifiants de l'utilisateur (dans son vrai
// HOME), absents du HOME sandbox. C'est donc un contournement du sandbox au
// même titre que run_shell "as_real_user" — d'où Confirm, et le fait que le
// mode mail ne le propose pas quand le sandbox est requis (voir main.go).
type ClaudeTool struct {
	// Bin : exécutable Claude Code. "" = "claude" (recherché dans le PATH).
	Bin string

	// PermissionMode : valeur de --permission-mode (voir `claude --help`).
	// "" = "auto". Combiné à --permission-prompts none : tout ce qui
	// nécessiterait une confirmation est refusé automatiquement plutôt que
	// de bloquer l'exécution en attendant une réponse qui ne viendra pas.
	PermissionMode string

	Timeout        time.Duration // <= 0 = 10 min
	MaxTimeout     time.Duration // plafond de "timeout_seconds", <= 0 = 30 min
	MaxOutputBytes int           // <= 0 = 20000
	Dir            string        // répertoire de travail par défaut (défaut: cwd du processus)

	// Perms : si non nil, vérifié avant chaque exécution pour le répertoire
	// de travail, comme run_shell.
	Perms *DirPermissions

	// Confirm : si non nil, appelé avant CHAQUE exécution avec le prompt et
	// le répertoire de travail ; false = exécution refusée. nil = pas de
	// confirmation (ex: mode mail sans sandbox, où run_shell tourne déjà
	// sous l'identité réelle sans confirmation).
	Confirm func(ctx context.Context, prompt, workDir string) (bool, error)
}

func (t *ClaudeTool) Name() string { return "run_claude" }

func (t *ClaudeTool) bin() string {
	if t.Bin != "" {
		return t.Bin
	}
	return "claude"
}

func (t *ClaudeTool) permissionMode() string {
	if t.PermissionMode != "" {
		return t.PermissionMode
	}
	return "auto"
}

func (t *ClaudeTool) effectiveTimeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return 10 * time.Minute
}

func (t *ClaudeTool) effectiveMaxTimeout() time.Duration {
	if t.MaxTimeout > 0 {
		return t.MaxTimeout
	}
	return 30 * time.Minute
}

func (t *ClaudeTool) Description() string {
	return fmt.Sprintf("Délègue une tâche de développement complète (modifier du code, corriger un bug, lancer des tests, explorer un dépôt...) à Claude Code, un agent de code autonome, en mode non interactif dans un répertoire donné. Claude Code ne peut PAS te poser de question : il fait la tâche directement et retourne un résumé de ce qu'il a fait. Donne-lui donc dans \"prompt\" tout le contexte nécessaire (objectif, contraintes, fichiers concernés, critère de réussite) — il ne voit rien de ta conversation. Exécuté sous l'identité réelle de l'utilisateur, hors sandbox (mode de permissions %q). Timeout par défaut %s, ajustable via \"timeout_seconds\" jusqu'à %s. Chaque appel repart de zéro, sans mémoire des appels précédents.", t.permissionMode(), t.effectiveTimeout(), t.effectiveMaxTimeout())
}

func (t *ClaudeTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"prompt": {"type": "string", "description": "Tâche à réaliser, autoporteuse : objectif, contexte, contraintes et critère de réussite. Claude Code n'a accès à rien d'autre de ta conversation."},
			"working_dir": {"type": "string", "description": "Chemin ABSOLU du répertoire dans lequel Claude Code travaille (typiquement la racine du dépôt concerné). Optionnel : par défaut, le répertoire de travail du bot."},
			"timeout_seconds": {"type": "integer", "description": "Timeout pour cette tâche, en secondes. Optionnel, plafonné par la configuration.", "minimum": 1}
		},
		"required": ["prompt"],
		"additionalProperties": false
	}`)
}

type claudeArgs struct {
	Prompt         string `json:"prompt"`
	WorkingDir     string `json:"working_dir"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// commandArgs retourne les arguments passés à Claude Code. Le prompt n'en
// fait pas partie : il est transmis sur l'entrée standard (voir Call), ce
// qui évite qu'un prompt commençant par "-" soit pris pour une option.
func (t *ClaudeTool) commandArgs() []string {
	return []string{
		"-p",
		"--output-format", "text",
		"--permission-mode", t.permissionMode(),
		"--permission-prompts", "none",
		"--append-system-prompt", claudeNonInteractivePrompt,
	}
}

func (t *ClaudeTool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args claudeArgs
	if err := decodeArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return "", fmt.Errorf(`paramètre "prompt" requis`)
	}

	workDir := args.WorkingDir
	if workDir != "" {
		if err := requireAbsolutePath("working_dir", workDir); err != nil {
			return "", err
		}
	} else if workDir = t.Dir; workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("détermination du répertoire de travail: %w", err)
		}
		workDir = wd
	}
	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("répertoire de travail %q introuvable ou invalide", workDir)
	}

	if t.Perms != nil {
		granted, err := t.Perms.RequestAccess(ctx, workDir, "exécution de Claude Code (run_claude) dans ce répertoire")
		if err != nil {
			return "", fmt.Errorf("vérification de l'accès à %q: %w", workDir, err)
		}
		if !granted {
			return "", fmt.Errorf("accès refusé au répertoire de travail %q — Claude Code non lancé", workDir)
		}
	}

	if t.Confirm != nil {
		granted, err := t.Confirm(ctx, args.Prompt, workDir)
		if err != nil {
			return "", fmt.Errorf("confirmation du lancement de Claude Code: %w", err)
		}
		if !granted {
			return "", fmt.Errorf("lancement de Claude Code refusé par l'utilisateur")
		}
	}

	timeout := t.effectiveTimeout()
	if args.TimeoutSeconds > 0 {
		requested := time.Duration(args.TimeoutSeconds) * time.Second
		if max := t.effectiveMaxTimeout(); requested > max {
			requested = max
		}
		timeout = requested
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, t.bin(), t.commandArgs()...)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(args.Prompt)
	// Même traitement que run_shell (voir ses commentaires) : détaché du
	// terminal, et tout le groupe de processus tué au timeout — Claude Code
	// lance lui-même des sous-processus (shell, serveurs MCP...).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error {
		if pid := cmd.Process.Pid; pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		return cmd.Process.Kill()
	}

	output, runErr := cmd.CombinedOutput()

	maxBytes := t.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = 20000
	}
	var b strings.Builder
	if len(output) > maxBytes {
		// Garde la FIN plutôt que le début : le résumé final de Claude Code
		// est ce qui compte.
		b.WriteString("[... début de la sortie tronqué ...]\n")
		output = output[len(output)-maxBytes:]
	}
	b.Write(output)
	if cctx.Err() == context.DeadlineExceeded {
		b.WriteString(fmt.Sprintf("\n[Claude Code interrompu après %s (timeout) — des modifications partielles ont pu être faites dans %s]", timeout, workDir))
	} else if runErr != nil {
		b.WriteString(fmt.Sprintf("\n[Claude Code terminé avec erreur: %v]", runErr))
	}
	return b.String(), nil
}

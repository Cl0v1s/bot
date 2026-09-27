package tools

import (
	"bytes"
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
	return fmt.Sprintf("N'UTILISE CET OUTIL QUE si l'utilisateur te demande EXPLICITEMENT, dans son message, de faire appel à Claude Code (ex: \"demande à Claude\", \"lance Claude Code\", \"utilise run_claude\") — JAMAIS de ta propre initiative, même pour une tâche de code qui semble s'y prêter, même si tes autres outils échouent : dans ce cas, fais la tâche toi-même avec tes autres outils, ou explique le problème à l'utilisateur. Une demande de code ordinaire n'est PAS une demande d'utiliser Claude Code. Transmet la demande de l'utilisateur à Claude Code, un agent de code autonome, en mode non interactif dans un répertoire donné : il fait la tâche directement, sans poser de question, et retourne sa réponse (ce qu'il a fait). \"prompt\" doit être la demande EXACTE de l'utilisateur, recopiée mot pour mot : ne la reformule pas, ne la résume pas, n'y ajoute ni contexte, ni instructions, ni précisions de ton cru. Une fois le résultat reçu, affiche à l'utilisateur la réponse de Claude Code telle quelle, en entier, sans la reformuler ni la résumer. Exécuté sous l'identité réelle de l'utilisateur, hors sandbox (mode de permissions %q). Timeout par défaut %s, ajustable via \"timeout_seconds\" jusqu'à %s. Chaque appel repart de zéro, sans mémoire des appels précédents.", t.permissionMode(), t.effectiveTimeout(), t.effectiveMaxTimeout())
}

func (t *ClaudeTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"prompt": {"type": "string", "description": "Demande EXACTE de l'utilisateur, recopiée mot pour mot, sans reformulation, résumé ni ajout de contexte ou d'instructions. N'appelle cet outil que si l'utilisateur a explicitement demandé de passer par Claude Code."},
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

	// stdout (la réponse de Claude Code) et stderr séparés : stderr n'est
	// renvoyé qu'en cas d'échec, pour ne pas mêler du bruit technique à la
	// réponse que le modèle doit afficher telle quelle.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	maxBytes := t.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = 20000
	}
	output := bytes.TrimSpace(stdout.Bytes())
	var b strings.Builder
	b.WriteString("Réponse de Claude Code (à afficher telle quelle à l'utilisateur, en entier, sans reformulation) :\n\n")
	if len(output) > maxBytes {
		// Garde la FIN plutôt que le début : le résumé final de Claude Code
		// est ce qui compte.
		b.WriteString("[... début de la réponse tronqué ...]\n")
		output = output[len(output)-maxBytes:]
	}
	if len(output) == 0 {
		b.WriteString("[aucune réponse]")
	}
	b.Write(output)
	if runErr != nil {
		if errOut := bytes.TrimSpace(stderr.Bytes()); len(errOut) > 0 {
			if len(errOut) > 4000 {
				errOut = errOut[len(errOut)-4000:]
			}
			b.WriteString("\n\n[sortie d'erreur de Claude Code]\n")
			b.Write(errOut)
		}
	}
	if cctx.Err() == context.DeadlineExceeded {
		b.WriteString(fmt.Sprintf("\n[Claude Code interrompu après %s (timeout) — des modifications partielles ont pu être faites dans %s]", timeout, workDir))
	} else if runErr != nil {
		b.WriteString(fmt.Sprintf("\n[Claude Code terminé avec erreur: %v]", runErr))
	}
	return b.String(), nil
}

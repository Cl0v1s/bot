package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"bot/internal/sandbox"
)

// ShellTool exécute une commande shell locale via `sh -c`. La commande
// complète est passée comme un unique argument de processus (aucune
// reconstruction/interpolation de chaîne côté Go) : c'est sh qui interprète
// guillemets, pipes, etc. exactement comme si elle avait été tapée dans un
// terminal.
type ShellTool struct {
	Timeout        time.Duration
	MaxOutputBytes int
	Dir            string // répertoire de travail, optionnel (défaut: cwd du processus)

	// Sandboxed : si vrai, la commande est exécutée sous le compte système
	// restreint "llm" (internal/sandbox) plutôt que sous l'identité de la
	// personne qui a lancé le harnais. Ne doit être activé qu'après un
	// sandbox.Ensure() réussi.
	Sandboxed bool

	// Perms : si non nil, vérifié avant chaque exécution pour le répertoire
	// de travail (Dir, ou le cwd du processus si vide) — mêmes permissions
	// que read_file/write_file. Si l'accès n'est pas encore accordé, il est
	// demandé (RequestAccess), ce qui déclenche en mode chat la même
	// question interactive que request_directory_access.
	Perms *DirPermissions
}

func (t *ShellTool) Name() string { return "run_shell" }

func (t *ShellTool) Description() string {
	base := "Exécute une commande shell locale (via `sh -c`) et retourne sa sortie standard et d'erreur combinées."
	if t.Sandboxed {
		base += fmt.Sprintf(" Exécutée sous le compte système restreint %q, pas sous celui de l'utilisateur.", sandbox.User)
	} else {
		base += " Accès complet au système : à utiliser avec prudence."
	}
	return base + " Les commandes manifestement destructrices (rm -rf/-f, formatage, écriture disque brute, arrêt machine, bombe fork...) sont bloquées avant exécution."
}

func (t *ShellTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "Commande shell à exécuter, interprétée par sh -c."}
		},
		"required": ["command"],
		"additionalProperties": false
	}`)
}

type shellArgs struct {
	Command string `json:"command"`
}

func (t *ShellTool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args shellArgs
	if err := decodeArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Command) == "" {
		return "", fmt.Errorf(`paramètre "command" requis`)
	}

	// Verrou appliqué dans le code, pas seulement suggéré dans le prompt :
	// voir le commentaire de dangerousShellPatterns (shell_guard.go) sur les
	// limites de ce filtre heuristique.
	if dangerous, label, segment := checkDangerousShellCommand(args.Command); dangerous {
		return "", fmt.Errorf("commande bloquée : motif dangereux détecté (%s) dans %q — non exécutée", label, segment)
	}

	workDir := t.Dir
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("détermination du répertoire de travail: %w", err)
		}
		workDir = wd
	}

	if t.Perms != nil {
		granted, err := t.Perms.RequestAccess(workDir, "exécution d'une commande shell (run_shell) dans ce répertoire")
		if err != nil {
			return "", fmt.Errorf("vérification de l'accès à %q: %w", workDir, err)
		}
		if !granted {
			return "", fmt.Errorf("accès refusé au répertoire de travail %q — commande non exécutée", workDir)
		}
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if t.Sandboxed {
		cmd = sandbox.WrapCommand(cctx, args.Command)
	} else {
		cmd = exec.CommandContext(cctx, "sh", "-c", args.Command)
	}
	cmd.Dir = workDir

	output, runErr := cmd.CombinedOutput()

	maxBytes := t.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = 20000
	}
	truncated := false
	if len(output) > maxBytes {
		output = output[:maxBytes]
		truncated = true
	}

	var b strings.Builder
	b.Write(output)
	if truncated {
		b.WriteString("\n[... sortie tronquée ...]")
	}
	if cctx.Err() == context.DeadlineExceeded {
		b.WriteString(fmt.Sprintf("\n[commande interrompue après %s (timeout)]", timeout))
	} else if runErr != nil {
		b.WriteString(fmt.Sprintf("\n[commande terminée avec erreur: %v]", runErr))
	}
	return b.String(), nil
}

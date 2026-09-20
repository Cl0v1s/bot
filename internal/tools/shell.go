package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
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

	// MaxTimeout : borne supérieure du timeout que le modèle peut demander
	// via le paramètre "timeout_seconds" (voir ParametersSchema/Call) — sans
	// plafond, une commande interactive ou bloquante par erreur pourrait
	// monopoliser le sandbox indéfiniment. <= 0 = valeur par défaut (10 min).
	MaxTimeout time.Duration

	// NotifyThreshold : au-delà de cette durée réelle d'exécution, une
	// notification de bureau (voir notify.go) est envoyée à la fin de la
	// commande — utile pour une commande longue (build, installation...)
	// lancée pendant qu'on fait autre chose. <= 0 = désactivé (aucune
	// notification, quelle que soit la durée).
	NotifyThreshold time.Duration

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

	// GitConfigPath : si non vide (et Sandboxed actif), transmis à
	// sandbox.WrapCommand pour donner à User une configuration git
	// "globale" utilisable (voir sandbox.EnsureGitConfig).
	GitConfigPath string

	// HomeDir : si non vide (et Sandboxed actif), transmis à
	// sandbox.WrapCommand comme HOME pour la commande — User n'ayant pas de
	// répertoire personnel réel, tout outil qui a besoin d'y écrire une
	// config/un cache (ex: `glab auth login`) échoue sinon (voir
	// sandbox.SandboxHomeDir).
	HomeDir string
}

func (t *ShellTool) Name() string { return "run_shell" }

func (t *ShellTool) Description() string {
	base := "Exécute une commande shell locale (via `sh -c`) et retourne sa sortie standard et d'erreur combinées."
	if t.Sandboxed {
		base += fmt.Sprintf(" Exécutée sous le compte système restreint %q, pas sous celui de l'utilisateur.", sandbox.User)
	} else {
		base += " Accès complet au système : à utiliser avec prudence."
	}
	base += fmt.Sprintf(
		" Timeout par défaut %s, ajustable par commande via \"timeout_seconds\" (utile pour une commande normalement longue, ex: build, installation de paquets) jusqu'à %s maximum — au-delà, la commande est interrompue et sa sortie déjà produite est renvoyée.",
		t.effectiveTimeout(), t.effectiveMaxTimeout(),
	)
	return base + " Les commandes manifestement destructrices (rm -rf/-f, formatage, écriture disque brute, arrêt machine, bombe fork...) sont bloquées avant exécution : ne les tente pas, elles échoueront systématiquement — pour supprimer un fichier ou un dossier vide, utilise `rm`/`rmdir` sans -f ni -r plutôt que `rm -f`/`rm -rf`."
}

func (t *ShellTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "Commande shell à exécuter, interprétée par sh -c."},
			"timeout_seconds": {"type": "integer", "description": "Timeout pour cette commande, en secondes. Optionnel : par défaut, timeout standard du tool. Plafonné à une valeur maximale fixée par la configuration.", "minimum": 1}
		},
		"required": ["command"],
		"additionalProperties": false
	}`)
}

type shellArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// effectiveTimeout retourne t.Timeout, ou 30s par défaut si non configuré.
func (t *ShellTool) effectiveTimeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return 30 * time.Second
}

// effectiveMaxTimeout retourne t.MaxTimeout, ou 10 minutes par défaut si non
// configuré.
func (t *ShellTool) effectiveMaxTimeout() time.Duration {
	if t.MaxTimeout > 0 {
		return t.MaxTimeout
	}
	return 10 * time.Minute
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
		granted, err := t.Perms.RequestAccess(ctx, workDir, "exécution d'une commande shell (run_shell) dans ce répertoire")
		if err != nil {
			return "", fmt.Errorf("vérification de l'accès à %q: %w", workDir, err)
		}
		if !granted {
			return "", fmt.Errorf("accès refusé au répertoire de travail %q — commande non exécutée", workDir)
		}
	}

	if t.Sandboxed {
		// Resynchronise avant chaque exécution, pas seulement une fois au
		// moment de l'octroi initial : un fichier créé ou modifié depuis en
		// dehors du harnais (édition manuelle, git pull/checkout lancé hors
		// de run_shell, etc. — write_file, lui, se resynchronise déjà après
		// coup à chaque appel, voir son commentaire) garde les droits
		// classiques de l'utilisateur réel (souvent non accessibles en
		// écriture au groupe), invisibles pour le compte sandbox tant que
		// personne ne relance GrantDirectory. Best-effort : un échec ici ne
		// doit pas empêcher la commande de s'exécuter (elle échouera d'elle-
		// même si l'accès manque réellement, avec une erreur normale).
		if err := sandbox.GrantDirectory(workDir); err != nil {
			log.Printf("run_shell: échec de la resynchronisation sandbox de %q: %v", workDir, err)
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

	var cmd *exec.Cmd
	if t.Sandboxed {
		cmd = sandbox.WrapCommand(cctx, args.Command, t.GitConfigPath, t.HomeDir)
	} else {
		cmd = exec.CommandContext(cctx, "sh", "-c", args.Command)
	}
	cmd.Dir = workDir
	// Détache la commande du terminal de contrôle (nouvelle session) : sans
	// ça, une commande qui ouvre /dev/tty directement (un outil interactif
	// ignorant sciemment que son entrée standard est /dev/null — vim, less,
	// top, ssh...) peut passer le terminal en mode brut/sans echo pour sa
	// propre interface, et l'y laisser si elle ne se termine pas proprement
	// au timeout — en particulier en mode sandboxé, où tuer le "sudo" de
	// tête ne tue pas nécessairement l'enfant qui tourne sous l'autre uid
	// (voir le commentaire de WaitDelay ci-dessous) : ce petit-fils peut
	// alors survivre au bot lui-même, terminal cassé y compris après avoir
	// quitté. Après setsid, /dev/tty n'a plus de terminal de contrôle à
	// ouvrir : impossible d'atteindre le nôtre, orpheline ou non.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Sans WaitDelay, un timeout ne tue que le process de tête (sh, ou sudo
	// en mode sandboxé) : si un petit-fils garde stdout/stderr ouverts (job
	// en arrière-plan, ou — cas sandboxé — l'enfant de sudo qui tourne sous
	// un autre uid et n'est pas atteint par le kill de sudo), Wait() ne
	// revient jamais et CombinedOutput() bloque indéfiniment, au-delà du
	// timeout demandé. WaitDelay force la fermeture des pipes après ce délai
	// pour que la sortie déjà capturée soit quand même renvoyée.
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	output, runErr := cmd.CombinedOutput()
	elapsed := time.Since(start)

	if t.NotifyThreshold > 0 && elapsed >= t.NotifyThreshold {
		title := "run_shell terminé"
		switch {
		case cctx.Err() == context.DeadlineExceeded:
			title = "run_shell : timeout"
		case runErr != nil:
			title = "run_shell : erreur"
		}
		body := fmt.Sprintf("%s (%s)", truncateForNotify(args.Command, 100), elapsed.Round(time.Second))
		go notifyOS(title, body)
	}

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

// truncateForNotify raccourcit s à max caractères, pour le corps d'une
// notification de bureau (jamais destinée à afficher une commande complète).
func truncateForNotify(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

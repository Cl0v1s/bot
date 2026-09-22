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

	// ConfirmRealUser : si non nil (et Sandboxed actif), permet au modèle de
	// demander, via le paramètre "as_real_user" (voir ParametersSchema), à
	// exécuter UNE commande précise sous l'identité réelle plutôt que sous
	// le compte sandbox — pour un outil interactif (ex: `glab auth login`)
	// que la synchronisation de permissions entre les deux identités ne
	// suffit pas toujours à satisfaire (ex: un fichier de config que l'outil
	// exige en 600, incompatible avec un accès groupe partagé — voir
	// sandbox.GrantDirectory). Reçoit la commande exacte à exécuter, appelé
	// à CHAQUE commande "as_real_user" (jamais court-circuité par ShellTool
	// lui-même) — c'est à l'implémentation de décider si/combien de temps
	// une confirmation reste valable sans redemander (ex: le mode chat
	// accorde une fenêtre de quelques minutes après un "oui", plutôt que de
	// reprompter à chaque appel). nil (ex: mode mail, aucun humain
	// disponible pour confirmer) désactive purement et simplement ce mode.
	ConfirmRealUser func(ctx context.Context, command string) (granted bool, err error)
}

func (t *ShellTool) Name() string { return "run_shell" }

func (t *ShellTool) Description() string {
	base := "Exécute une commande shell locale (via `sh -c`) et retourne sa sortie standard et d'erreur combinées."
	if t.Sandboxed {
		base += fmt.Sprintf(" Exécutée sous le compte système restreint %q, pas sous celui de l'utilisateur.", sandbox.User)
		if t.ConfirmRealUser != nil {
			base += " Le paramètre \"as_real_user\" existe pour un cas précis (identité réelle plutôt que le compte sandbox, ex: un outil qui exige des permissions de fichier strictes incompatibles avec un accès partagé, comme `glab auth login`) — mais NE L'UTILISE QUE si l'utilisateur te le demande explicitement, ou si les instructions d'une skill (SKILL.md) le demandent explicitement pour cet outil précis. Ne l'utilise JAMAIS de ta propre initiative simplement parce qu'une commande sandboxée échoue ou que ça semble plus simple : une commande sandboxée qui échoue a presque toujours une autre cause (répertoire non accordé, syntaxe, outil manquant...) à diagnostiquer normalement d'abord."
		}
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
			"timeout_seconds": {"type": "integer", "description": "Timeout pour cette commande, en secondes. Optionnel : par défaut, timeout standard du tool. Plafonné à une valeur maximale fixée par la configuration.", "minimum": 1},
			"as_real_user": {"type": "boolean", "description": "Si vrai, exécute cette commande précise sous l'identité réelle plutôt que sous le compte sandbox (aucun effet si le tool n'est pas sandboxé) — ne change AUCUN droit de fichier/répertoire, juste l'identité de ce seul appel. Demande une confirmation interactive à l'utilisateur (valable ensuite quelques minutes pour les appels suivants, pas reprompée systématiquement) ; refusé sans confirmation possible (ex: mode mail). N'UTILISE CE PARAMÈTRE QUE si l'utilisateur te le demande explicitement, ou si les instructions d'une skill l'exigent explicitement pour cet outil précis (ex: un outil dont l'authentification/les identifiants sont liés à l'identité réelle, comme glab). Ne l'active JAMAIS simplement parce qu'une commande sandboxée a échoué ou par réflexe — diagnostique d'abord la vraie cause de l'échec."}
		},
		"required": ["command"],
		"additionalProperties": false
	}`)
}

type shellArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	AsRealUser     bool   `json:"as_real_user"`
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

	// runAsRealUser : cette commande précise s'exécute sous l'identité réelle
	// plutôt que sous le compte sandbox (voir ConfirmRealUser) — jamais vrai
	// si le tool n'est pas sandboxé (rien à contourner, déjà l'identité
	// réelle par défaut). Toujours reconfirmé ici, jamais mémorisé d'un appel
	// à l'autre : voir le commentaire de ConfirmRealUser.
	runAsRealUser := false
	if args.AsRealUser && t.Sandboxed {
		if t.ConfirmRealUser == nil {
			return "", fmt.Errorf(`"as_real_user" demandé mais indisponible : confirmation interactive requise, aucun humain disponible pour la donner dans ce contexte`)
		}
		granted, err := t.ConfirmRealUser(ctx, args.Command)
		if err != nil {
			return "", fmt.Errorf("confirmation pour l'exécution sous l'identité réelle: %w", err)
		}
		if !granted {
			return "", fmt.Errorf("exécution sous l'identité réelle refusée par l'utilisateur — commande non exécutée")
		}
		runAsRealUser = true
	}

	if t.Sandboxed && !runAsRealUser {
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
		// Inutile en mode runAsRealUser : cette commande précise n'utilise
		// pas le compte sandbox, rien à synchroniser pour elle.
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
	if t.Sandboxed && !runAsRealUser {
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

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

	// RealUserWindow : purement descriptif (voir Description/ParametersSchema)
	// — la durée réelle de la fenêtre accordée par "as_real_user_window" est
	// décidée par l'implémentation de ConfirmRealUser, pas ici. <= 0 = valeur
	// affichée par défaut (5 min).
	RealUserWindow time.Duration

	// ConfirmRealUser : si non nil (et Sandboxed actif), permet au modèle de
	// demander, via le paramètre "as_real_user" (voir ParametersSchema), à
	// exécuter UNE commande précise sous l'identité réelle plutôt que sous
	// le compte sandbox — pour un outil interactif (ex: `glab auth login`)
	// que la synchronisation de permissions entre les deux identités ne
	// suffit pas toujours à satisfaire (ex: un fichier de config que l'outil
	// exige en 600, incompatible avec un accès groupe partagé — voir
	// sandbox.GrantDirectory). Reçoit la commande exacte à exécuter et window
	// (voir le paramètre "as_real_user_window") ; appelé à CHAQUE commande
	// "as_real_user" (jamais court-circuité par ShellTool lui-même) — c'est à
	// l'implémentation de décider si/combien de temps une confirmation reste
	// valable sans redemander (ex: le mode chat, quand window est vrai,
	// accorde une fenêtre de quelques minutes pour les appels suivants —
	// oneshot ou window — plutôt que de reprompter à chaque appel ; quand
	// window est faux, la confirmation ne vaut que pour cette commande
	// précise). nil (ex: mode mail, aucun humain disponible pour confirmer)
	// désactive purement et simplement ce mode.
	ConfirmRealUser func(ctx context.Context, command string, window bool) (granted bool, err error)
}

func (t *ShellTool) Name() string { return "run_shell" }

// effectiveRealUserWindow retourne t.RealUserWindow, ou 5 minutes par défaut
// si non configuré — purement pour l'afficher dans Description/
// ParametersSchema (voir le commentaire de RealUserWindow).
func (t *ShellTool) effectiveRealUserWindow() time.Duration {
	if t.RealUserWindow > 0 {
		return t.RealUserWindow
	}
	return 5 * time.Minute
}

func (t *ShellTool) Description() string {
	base := "Exécute une commande shell locale (via `sh -c`) et retourne sa sortie standard et d'erreur combinées."
	if t.Sandboxed {
		base += fmt.Sprintf(" Exécutée sous le compte système restreint %q, pas sous celui de l'utilisateur.", sandbox.User)
		if t.ConfirmRealUser != nil {
			base += fmt.Sprintf(" Le paramètre \"as_real_user\" existe pour un cas précis (identité réelle plutôt que le compte sandbox, ex: un outil qui exige des permissions de fichier strictes incompatibles avec un accès partagé, comme `glab auth login`) — mais NE L'UTILISE QUE si l'utilisateur te le demande explicitement, ou si les instructions d'une skill (SKILL.md) le demandent explicitement pour cet outil précis. Ne l'utilise JAMAIS de ta propre initiative simplement parce qu'une commande sandboxée échoue ou que ça semble plus simple : une commande sandboxée qui échoue a presque toujours une autre cause (répertoire non accordé, syntaxe, outil manquant...) à diagnostiquer normalement d'abord. Par défaut (\"as_real_user_window\" omis ou faux), la confirmation ne vaut que pour CETTE commande précise (mode \"oneshot\") : le prochain appel \"as_real_user\", même juste après, redemande. Si une skill (ou l'utilisateur) prévoit PLUSIEURS commandes sous l'identité réelle à la suite, passe \"as_real_user_window\": true sur le PREMIER appel pour ouvrir une fenêtre de %s pendant laquelle les appels \"as_real_user\" suivants (oneshot ou window) ne redemandent pas — n'active ce mode que si tu sais déjà que tu en auras besoin plusieurs fois, pas par précaution.", t.effectiveRealUserWindow())
		} else {
			base += " Le paramètre \"as_real_user\" (identité réelle plutôt que le compte sandbox) existe mais est INDISPONIBLE dans cette session : il exige qu'un shell interactif soit accessible pour qu'un humain confirme, ce qui n'est pas le cas ici (ex: mode mail, sans personne pour répondre) — toute tentative échouera explicitement avec une erreur claire. N'essaie pas de contourner ça autrement."
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
	asRealUserDesc := "Si vrai, exécute cette commande précise sous l'identité réelle plutôt que sous le compte sandbox (aucun effet si le tool n'est pas sandboxé) — ne change AUCUN droit de fichier/répertoire, juste l'identité de ce seul appel. Demande une confirmation interactive à l'utilisateur. N'UTILISE CE PARAMÈTRE QUE si l'utilisateur te le demande explicitement, ou si les instructions d'une skill l'exigent explicitement pour cet outil précis (ex: un outil dont l'authentification/les identifiants sont liés à l'identité réelle, comme glab). Ne l'active JAMAIS simplement parce qu'une commande sandboxée a échoué ou par réflexe — diagnostique d'abord la vraie cause de l'échec. Voir \"as_real_user_window\" pour plusieurs commandes à la suite."
	asRealUserWindowDesc := fmt.Sprintf("Sans effet si \"as_real_user\" n'est pas vrai. Par défaut (faux) : confirmation \"oneshot\", valable seulement pour cette commande précise, le prochain appel \"as_real_user\" redemande. Si vrai : ouvre, une fois confirmé, une fenêtre de %s pendant laquelle les appels \"as_real_user\" suivants ne redemandent pas — à réserver au cas où TU SAIS déjà que plusieurs commandes sous l'identité réelle vont suivre (typiquement précisé par une skill), jamais par défaut/précaution.", t.effectiveRealUserWindow())
	if t.Sandboxed && t.ConfirmRealUser == nil {
		asRealUserDesc = "INDISPONIBLE dans cette session : exige qu'un shell interactif soit accessible pour qu'un humain confirme (ex: mode mail, personne pour répondre) — ce n'est pas le cas ici. Toute valeur true échouera explicitement, ne l'utilise pas."
		asRealUserWindowDesc = "INDISPONIBLE dans cette session, voir \"as_real_user\"."
	}
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":             map[string]any{"type": "string", "description": "Commande shell à exécuter, interprétée par sh -c."},
			"timeout_seconds":     map[string]any{"type": "integer", "description": "Timeout pour cette commande, en secondes. Optionnel : par défaut, timeout standard du tool. Plafonné à une valeur maximale fixée par la configuration.", "minimum": 1},
			"as_real_user":        map[string]any{"type": "boolean", "description": asRealUserDesc},
			"as_real_user_window": map[string]any{"type": "boolean", "description": asRealUserWindowDesc},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	})
	if err != nil {
		// Ne peut arriver qu'en cas d'erreur de programmation (valeur non
		// sérialisable dans la map ci-dessus, jamais le cas ici) : panic
		// plutôt qu'un schéma silencieusement vide/cassé.
		panic(fmt.Sprintf("shell: construction du schéma de paramètres: %v", err))
	}
	return schema
}

type shellArgs struct {
	Command          string `json:"command"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
	AsRealUser       bool   `json:"as_real_user"`
	AsRealUserWindow bool   `json:"as_real_user_window"`
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
	if args.AsRealUserWindow && !args.AsRealUser {
		return "", fmt.Errorf(`"as_real_user_window" n'a de sens qu'avec "as_real_user": true`)
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
			return "", fmt.Errorf(`"as_real_user" demandé mais indisponible : ça nécessite qu'un shell interactif soit accessible pour qu'un humain confirme, ce qui n'est pas le cas dans cette session (ex: mode mail) — commande non exécutée`)
		}
		granted, err := t.ConfirmRealUser(ctx, args.Command, args.AsRealUserWindow)
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

	script := noTTYScript(args.Command)
	var cmd *exec.Cmd
	if t.Sandboxed && !runAsRealUser {
		cmd = sandbox.WrapCommand(cctx, script, t.GitConfigPath, t.HomeDir)
	} else {
		cmd = exec.CommandContext(cctx, "sh", "-c", script)
	}
	cmd.Dir = workDir
	// Stdin laissé à nil = /dev/null (voir exec.Cmd) : jamais le terminal
	// du harnais, même en mode chat. Voir aussi noTTYScript.
	cmd.Stdin = nil
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
	// Cancel (appelé par exec au timeout/annulation, avant WaitDelay) :
	// remplace le comportement par défaut (cmd.Process.Kill(), qui ne tue
	// QUE le process de tête) par un SIGKILL envoyé à tout le groupe de
	// processus — Setsid ci-dessus en fait le chef d'un nouveau groupe, donc
	// son propre pid est aussi le pgid. Sans ça, un descendant survit
	// indéfiniment au timeout (job en arrière-plan lancé par la commande,
	// pipeline...). Best-effort (erreurs ignorées) : le process a pu
	// terminer entre-temps, ou ne plus exister.
	//
	// Cas sandboxé (sudo de tête) : un SIGKILL envoyé par l'identité réelle
	// n'atteint que les processus du groupe appartenant à cette identité —
	// kill(2) ignore silencieusement, sans faire échouer l'appel, les
	// membres du groupe appartenant à un autre uid (vérifié empiriquement :
	// tuer le groupe du "sudo" de tête laisse orphelin et actif son enfant
	// tournant sous User). Il faut donc un second SIGKILL, explicitement
	// envoyé SOUS User via sudo, exactement comme WrapCommand redemande
	// sudo pour l'exécution elle-même.
	cmd.Cancel = func() error {
		if pid := cmd.Process.Pid; pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			if t.Sandboxed && !runAsRealUser {
				_ = exec.Command("sudo", "-n", "-u", sandbox.User, "kill", "-KILL", "--", fmt.Sprintf("-%d", pid)).Run()
			}
		}
		return cmd.Process.Kill()
	}

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

// noTTYEnv : variables posées avant chaque commande pour que les outils
// usuels renoncent d'eux-mêmes à toute interaction terminal (demande
// d'identifiants git, pager, questions debconf...) au lieu d'échouer — ou
// d'attendre jusqu'au timeout — en cherchant un tty qu'ils n'auront pas.
var noTTYEnv = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GCM_INTERACTIVE=never",
	"DEBIAN_FRONTEND=noninteractive",
	"TERM=dumb",
	"PAGER=cat",
	"GIT_PAGER=cat",
}

// noTTYScript enveloppe command pour qu'elle ne puisse jamais obtenir de
// terminal, en complément de Setsid (voir Call) :
//
//   - Setsid seul laisse une faille : le process lancé (sh) est chef de
//     session sans terminal de contrôle, et sous Linux un chef de session
//     qui ouvre un tty sans O_NOCTTY (ex: `echo x > /dev/pts/0`) en fait
//     son terminal de contrôle. Le sh externe ne fait donc QUE lancer la
//     commande dans un sh enfant (en arrière-plan puis wait, ce qui
//     empêche sh de remplacer le chef de session par la commande via
//     exec) : un process qui n'est pas chef de session ne peut jamais
//     acquérir de terminal de contrôle, quoi qu'il ouvre. Même groupe de
//     processus, donc toujours atteint par le SIGKILL de cmd.Cancel ; le
//     code de sortie est celui de la commande (wait).
//   - noTTYEnv est exporté (via le script plutôt que cmd.Env pour
//     traverser aussi le sudo du mode sandboxé, qui réinitialise
//     l'environnement), GPG_TTY retiré.
func noTTYScript(command string) string {
	return "export " + strings.Join(noTTYEnv, " ") + "; unset GPG_TTY; " +
		"sh -c " + sandbox.ShellQuote(command) + " </dev/null & wait $!"
}

// truncateForNotify raccourcit s à max caractères, pour le corps d'une
// notification de bureau (jamais destinée à afficher une commande complète).
func truncateForNotify(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

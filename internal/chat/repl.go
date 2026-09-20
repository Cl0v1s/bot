// Package chat implémente le mode interactif console du harnais.
package chat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"bot/internal/agent"
	"bot/internal/convo"
	"bot/internal/llm"
	"bot/internal/sandbox"
	"bot/internal/tools"
)

// ToolsConfig paramètre les tools disponibles en mode chat.
type ToolsConfig struct {
	Enabled bool
	// AllowedDirsFile : fichier partagé de répertoires accordés dynamiquement
	// (voir tools.DirPermissions.WithPersistence). Chaque nouvel accès
	// accordé en direct par l'utilisateur y est ajouté, et devient ainsi
	// aussi disponible pour le mode mail (qui relit ce même fichier, en
	// lecture seule).
	AllowedDirsFile string
	// SandboxUserEnabled : exécute run_shell sous le compte système
	// restreint "llm" (internal/sandbox) plutôt que sous l'utilisateur
	// courant. La configuration est vérifiée (et effectuée si besoin, avec
	// confirmation) à chaque lancement.
	SandboxUserEnabled bool
	ShellTimeout       time.Duration
	// ShellMaxTimeout : borne supérieure du timeout que le modèle peut
	// demander pour une commande run_shell précise (voir
	// tools.ShellTool.MaxTimeout).
	ShellMaxTimeout time.Duration
	// ShellNotifyThreshold : voir tools.ShellTool.NotifyThreshold.
	ShellNotifyThreshold time.Duration
	HTTPTimeout          time.Duration
	// BrowserFetchTimeout : voir tools.BrowserFetchTool.Timeout.
	BrowserFetchTimeout time.Duration
	MaxSteps            int
	// WorkspaceDir : répertoire toujours accessible en lecture/écriture pour
	// read_file/write_file (voir tools.DirPermissions.AlwaysAllow), sans
	// passer par request_directory_access.
	WorkspaceDir string
	// SandboxSSHKey : chemin d'une clé privée SSH de l'utilisateur réel,
	// rendue lisible par le compte sandbox pour que git (via run_shell)
	// puisse s'authentifier sur un dépôt distant — voir
	// config.SandboxSSHKey et sandbox.EnsureSSHKeyAccess. "" = désactivé.
	SandboxSSHKey string
}

// chatLine est le résultat d'une lecture de ligne au clavier, transmis par
// la goroutine de lecture du terminal (voir plus bas) à la boucle
// principale. eof=true signale une fin de saisie (Ctrl+D ou erreur de
// lecture).
type chatLine struct {
	text string
	eof  bool
}

// Run lance une boucle de lecture-évaluation-affichage sur la console.
// Commandes spéciales : /exit, /reset, /stats, /compact.
//
// Si toolsCfg.Enabled, chaque tour passe par la boucle agentique
// (internal/agent) : les appels d'outils et leurs résultats sont affichés
// au fur et à mesure, nettement séparés (encadrés) du texte de réponse du
// modèle. Par défaut, read_file/write_file n'ont accès à aucun répertoire :
// le modèle doit appeler request_directory_access, ce qui déclenche ici une
// question interactive (o/N) à l'utilisateur — le verrouillage est appliqué
// dans les tools eux-mêmes (internal/tools.DirPermissions), pas par une
// simple instruction de prompt.
//
// Tapez ahead : chaque tour (réponse du modèle, y compris les éventuels
// appels d'outils) s'exécute en tâche de fond, ce qui permet de continuer à
// taper pendant qu'il tourne. Une ligne tapée pendant qu'un tour est en
// cours est soit mise en file d'attente (traitée automatiquement, dans
// l'ordre, dès que le tour courant se termine), soit — si le modèle demande
// une confirmation (permission de répertoire, mise en place du sandbox) —
// utilisée comme réponse à cette confirmation : voir terminalInput. Un seul
// tour à la fois est jamais exécuté, donc conv (*convo.Conversation) reste
// toujours accédée séquentiellement, sans verrou dédié. Un Ctrl+C pendant un
// tour annule ce tour et vide la file d'attente (voir doneCh ci-dessous) :
// il n'y a pas de façon d'annuler un seul message en file sans tout vider.
func Run(ctx context.Context, client *llm.Client, conv *convo.Conversation, toolsCfg ToolsConfig, in io.Reader, out io.Writer) error {
	// color est calculé sur la sortie d'origine : colorsEnabled fait un
	// type-assert vers *os.File, qui échouerait sur le syncWriter ci-dessous.
	color := colorizer(out)
	// À partir d'ici, plusieurs goroutines (streaming en tâche de fond, écho
	// clavier, boucle principale) écrivent potentiellement en parallèle vers
	// out : on le sérialise pour éviter des écritures entrelacées/coupées.
	out = &syncWriter{out: out}

	// L'éditeur de ligne (édition + historique haut/bas) n'est activé que
	// si in est un vrai terminal interactif : sur une entrée redirigée
	// (pipe, fichier, tests), on garde bufio.Scanner tel quel. readLine()
	// unifie les deux : c'est la seule façon de lire une ligne dans tout le
	// reste de la fonction.
	var editor *lineEditor
	if f, ok := in.(*os.File); ok && isTerminalFile(f) {
		if ed, err := newLineEditor(f, out); err == nil {
			editor = ed
		}
		// Si stty échoue (terminal exotique), on retombe silencieusement
		// sur bufio.Scanner ci-dessous : pas d'historique/édition avancée,
		// mais le chat reste utilisable.
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var scanErr error

	// readLine ne reçoit jamais de texte de prompt à afficher : l'affichage
	// du prompt est découplé de la lecture (voir plus bas, terminalInput) car
	// une même ligne tapée peut aussi bien répondre à une confirmation
	// qu'alimenter le prochain message de chat, selon ce qui l'attend au
	// moment où elle arrive.
	readLine := func() (string, bool) {
		if editor != nil {
			return editor.ReadLine("")
		}
		if !scanner.Scan() {
			scanErr = scanner.Err()
			return "", false
		}
		return scanner.Text(), true
	}

	// term arbitre, pour chaque ligne tapée, si elle répond à une
	// confirmation en attente (askYesNo, potentiellement appelée depuis la
	// tâche de fond d'un tour en cours) ou si elle doit être traitée comme
	// un message de chat (voir la goroutine de lecture plus bas).
	term := &terminalInput{}

	// askYesNo affiche prompt puis attend la prochaine ligne tapée comme
	// réponse, via term : sûr à appeler aussi bien depuis la boucle
	// principale (confirmations avant le début de la boucle, ex. sandbox,
	// avec le ctx de fond de Run — jamais annulé pendant cette fenêtre) que
	// depuis la tâche de fond d'un tour en cours (permission de répertoire
	// demandée par un appel d'outil, avec le reqCtx de ce tour). Le prompt
	// est affiché en violet : toute demande d'interaction utilisateur
	// (permission, confirmation) partage ce même code couleur.
	//
	// Respecte ctx : sans ça, un Ctrl+C pendant l'affichage de cette
	// question n'aurait aucun effet (annule bien reqCtx, mais la lecture
	// bloquante de term.Ask() ne s'en aperçoit jamais) — on resterait
	// bloqué là jusqu'à ce qu'une réponse soit tapée, sans aucun moyen de
	// s'en sortir au clavier.
	askYesNo := func(ctx context.Context, prompt string) bool {
		fmt.Fprint(out, color(ansiMagenta, prompt))
		ch := term.Ask()
		select {
		case line := <-ch:
			answer := strings.ToLower(strings.TrimSpace(line))
			return answer == "o" || answer == "oui" || answer == "y" || answer == "yes"
		case <-ctx.Done():
			term.Cancel(ch)
			fmt.Fprintln(out, color(ansiYellow, "\n[demande annulée (Ctrl+C)]"))
			return false
		}
	}

	var registry *tools.Registry
	if toolsCfg.Enabled {
		// shellAvailable : run_shell n'est proposé que si le sandbox n'est pas
		// requis, ou s'il a été mis en place avec succès — jamais de repli
		// silencieux vers une exécution non isolée quand l'utilisateur a
		// explicitement demandé le sandbox.
		shellAvailable := !toolsCfg.SandboxUserEnabled
		sandboxReady := false
		if toolsCfg.SandboxUserEnabled {
			if err := sandbox.Ensure(out, func(prompt string) bool { return askYesNo(ctx, prompt) }); err != nil {
				fmt.Fprintln(out, color(ansiRed, fmt.Sprintf("[sandbox] indisponible, run_shell ne sera pas proposé cette session : %v", err)))
			} else {
				sandboxReady = true
				shellAvailable = true
			}
		}

		perms := tools.NewDirPermissions(func(ctx context.Context, dir, reason string) (bool, error) {
			fmt.Fprintln(out, color(ansiMagenta, fmt.Sprintf("\n[permission] le modèle demande l'accès au répertoire : %s", dir)))
			if reason != "" {
				fmt.Fprintln(out, color(ansiMagenta, fmt.Sprintf("  raison : %s", reason)))
			}
			if !askYesNo(ctx, "  autoriser ? [o/N] ") {
				return false, nil
			}
			if sandboxReady {
				if err := sandbox.GrantDirectory(dir); err != nil {
					return false, fmt.Errorf("échec de l'ouverture du répertoire au compte %q : %w", sandbox.User, err)
				}
			}
			return true, nil
		})
		if err := perms.WithPersistence(toolsCfg.AllowedDirsFile); err != nil {
			fmt.Fprintln(out, color(ansiRed, fmt.Sprintf("[avertissement: lecture des répertoires autorisés (%s): %v]", toolsCfg.AllowedDirsFile, err)))
		}
		// Le workspace du bot (et /tmp, pour les fichiers vraiment
		// éphémères — voir agent.WorkspacePrompt) sont toujours accessibles,
		// indépendamment des accès accordés en direct par l'utilisateur
		// (voir AlwaysAllow).
		gitConfigPath := ""
		homeDir := ""
		if toolsCfg.WorkspaceDir != "" {
			perms.AlwaysAllow(toolsCfg.WorkspaceDir, os.TempDir())
			if sandboxReady {
				if toolsCfg.SandboxSSHKey != "" {
					if err := sandbox.EnsureSSHKeyAccess(toolsCfg.SandboxSSHKey); err != nil {
						fmt.Fprintln(out, color(ansiRed, fmt.Sprintf("[sandbox] échec de l'octroi d'accès à la clé SSH (%s) : %v", toolsCfg.SandboxSSHKey, err)))
					}
				}
				if err := sandbox.EnsureGitConfig(toolsCfg.WorkspaceDir, toolsCfg.SandboxSSHKey); err != nil {
					fmt.Fprintln(out, color(ansiRed, fmt.Sprintf("[sandbox] échec de la préparation de la config git (%s) : %v", toolsCfg.WorkspaceDir, err)))
				}
				// Doit être créé AVANT GrantDirectory : c'est ce dernier qui
				// pose rétroactivement les droits groupe nécessaires sur tout
				// ce qui existe déjà sous le workspace au moment où il
				// s'exécute (voir son commentaire) — un répertoire créé après
				// coup n'en bénéficierait pas.
				if err := sandbox.EnsureSandboxHome(toolsCfg.WorkspaceDir); err != nil {
					fmt.Fprintln(out, color(ansiRed, fmt.Sprintf("[sandbox] échec de la préparation du HOME sandbox (%s) : %v", toolsCfg.WorkspaceDir, err)))
				}
				if err := sandbox.GrantDirectory(toolsCfg.WorkspaceDir); err != nil {
					fmt.Fprintln(out, color(ansiRed, fmt.Sprintf("[sandbox] échec de l'ouverture du workspace (%s) au compte %q : %v", toolsCfg.WorkspaceDir, sandbox.User, err)))
				} else {
					gitConfigPath = sandbox.GitConfigPath(toolsCfg.WorkspaceDir)
					homeDir = sandbox.SandboxHomeDir(toolsCfg.WorkspaceDir)
				}
			}
		}

		toolList := []tools.Tool{
			&tools.ReadFileTool{Perms: perms},
			&tools.WriteFileTool{Perms: perms, Sandboxed: sandboxReady},
			&tools.HTTPGetTool{Timeout: toolsCfg.HTTPTimeout},
			&tools.BrowserFetchTool{Timeout: toolsCfg.BrowserFetchTimeout},
			&tools.RequestDirectoryAccessTool{Perms: perms},
			&tools.ListDirTool{},
		}
		if shellAvailable {
			toolList = append(toolList, &tools.ShellTool{Timeout: toolsCfg.ShellTimeout, MaxTimeout: toolsCfg.ShellMaxTimeout, NotifyThreshold: toolsCfg.ShellNotifyThreshold, Sandboxed: sandboxReady, Perms: perms, GitConfigPath: gitConfigPath, HomeDir: homeDir})
		}
		registry = tools.NewRegistry(toolList...)
		if !registry.Empty() {
			agent.AppendToolUsagePrompt(conv)
		}
	}

	promptIdle := color(ansiBold+ansiCyan, "> ")
	errorLine := func(format string, a ...any) {
		fmt.Fprintln(out, color(ansiRed, fmt.Sprintf(format, a...)))
	}
	// reqErrorLine distingue une requête annulée par Ctrl+C (attendu, pas
	// une vraie erreur) d'une erreur normale.
	reqErrorLine := func(err error) {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(out, color(ansiRed, "\n[requête annulée (Ctrl+C)]"))
			return
		}
		errorLine("\n[erreur LLM: %v]", err)
	}

	// editor.restore (no-op si editor est nil) doit s'exécuter sur TOUT
	// chemin de sortie, y compris l'arrêt immédiat par Ctrl+C (os.Exit saute
	// les defer, d'où le passage explicite à newInterruptController).
	defer editor.restore()
	interrupt := newInterruptController(editor.restore)

	// runCommand traite line si c'est une commande spéciale. Retourne
	// handled=true si line était une commande (à ne donc pas traiter comme
	// un message de chat), et exit=true si le programme doit se terminer.
	// N'est appelée que quand aucun tour n'est en cours (conv accédée sans
	// verrou dédié, voir le commentaire de Run).
	runCommand := func(line string) (handled, exit bool) {
		switch line {
		case "/exit", "/quit":
			return true, true
		case "/new", "/reset":
			conv.Reset()
			fmt.Fprintln(out, "[historique vidé]")
			return true, false
		case "/stats":
			basis := "estimé (~4 car./token)"
			if conv.TokensAreExact() {
				basis = "exact (usage API)"
			} else if conv.LastKnownTokens > 0 {
				basis = "exact + estimation du dernier message"
			}
			fmt.Fprintf(out, "[messages=%d tokens~=%d/%d (%s)]\n", len(conv.Messages), conv.EstimateTokens(), conv.MaxContextTokens, basis)
			return true, false
		}
		return false, false
	}

	// doneCh signale la fin du tour en cours (succès, erreur, ou annulation
	// par Ctrl+C — auquel cas la valeur transmise est true). Bufferisé à 1 :
	// dispatchTurn n'est jamais rappelée avant que la précédente n'ait
	// signalé sa fin (voir la boucle principale), donc jamais plus d'un
	// envoi en attente.
	doneCh := make(chan bool, 1)

	// dispatchTurn ajoute line à la conversation et exécute le tour
	// (réponse du modèle, avec ou sans outils) en tâche de fond, pour ne pas
	// bloquer la boucle principale : elle reste ainsi libre de continuer à
	// recevoir des lignes tapées (mise en file d'attente, ou réponse à une
	// confirmation demandée par un appel d'outil de ce tour). L'appelant
	// doit s'assurer qu'aucun autre tour n'est déjà en cours.
	dispatchTurn := func(line string) {
		go func() {
			cancelled := false
			defer func() { doneCh <- cancelled }()

			conv.AddUser(line)
			// reqCtx est propre à ce tour : un Ctrl+C pendant son exécution
			// n'annule que lui (endReq désarme l'annulation dès qu'il se
			// termine, pour qu'un Ctrl+C ultérieur au prompt quitte le
			// programme au lieu de casser silencieusement les tours
			// suivants).
			reqCtx, endReq := interrupt.begin(ctx)
			defer endReq()

			if !registry.Empty() {
				reply, _, err := agent.Run(reqCtx, client, conv, registry, toolsCfg.MaxSteps, func(e agent.Event) {
					code := ansiYellow // appel d'outil
					switch {
					case e.Kind == agent.EventReasoning:
						code = ansiGray // commentaire du modèle avant ses tool_calls
					case e.Kind == agent.EventToolResult:
						code = ansiGreen // résultat d'outil
						if e.Err != nil {
							code = ansiRed // résultat en erreur
						}
					}
					fmt.Fprint(out, color(code, e.Format()))
				})
				if err != nil {
					cancelled = errors.Is(err, context.Canceled)
					reqErrorLine(err)
					return
				}
				fmt.Fprintln(out, "\n"+reply)
				return
			}

			if compacted, err := conv.CompactIfNeeded(reqCtx, client); err != nil {
				errorLine("[avertissement: échec de la compaction du contexte: %v]", err)
			} else if compacted {
				fmt.Fprintln(out, "[contexte compacté automatiquement]")
			}
			// Filet de sécurité de dernier recours : voir le commentaire de
			// convo.Conversation.EnsureFitsContext.
			if dropped := conv.EnsureFitsContext(); dropped > 0 {
				errorLine("[avertissement: contexte encore trop grand après compaction : %d message(s) le plus ancien(s) supprimé(s) sans résumé]", dropped)
			}

			reply, usage, err := client.ChatCompletionStream(reqCtx, conv.Full(), func(delta string) {
				fmt.Fprint(out, delta)
			})
			if err != nil {
				cancelled = errors.Is(err, context.Canceled)
				reqErrorLine(err)
				return
			}
			fmt.Fprintln(out)

			conv.AddAssistant(reply)
			conv.RecordUsage(usage)
		}()
	}

	// dispatchCompact force une compaction (résumé + extraction mémoire, en
	// parallèle — voir convo.Conversation.Compact) comme si le seuil venait
	// d'être atteint, puis le filet de sécurité de dernier recours (voir
	// EnsureFitsContext), en tâche de fond comme dispatchTurn : un appel LLM,
	// ne doit donc pas bloquer la boucle principale. Contrairement à
	// dispatchTurn, ne touche jamais conv.Messages via AddUser : "/compact"
	// n'est pas un message envoyé au modèle.
	dispatchCompact := func() {
		go func() {
			cancelled := false
			defer func() { doneCh <- cancelled }()

			reqCtx, endReq := interrupt.begin(ctx)
			defer endReq()

			compacted, err := conv.Compact(reqCtx, client)
			if err != nil {
				cancelled = errors.Is(err, context.Canceled)
				reqErrorLine(err)
				return
			}
			if compacted {
				fmt.Fprintln(out, "[contexte compacté manuellement]")
			} else {
				fmt.Fprintln(out, "[rien à compacter : historique trop court, ou tout fait partie d'un même groupe d'appel d'outil]")
			}
			// Même filet de sécurité qu'après une compaction automatique :
			// voir le commentaire de convo.Conversation.EnsureFitsContext.
			if dropped := conv.EnsureFitsContext(); dropped > 0 {
				errorLine("[avertissement: contexte encore trop grand après compaction : %d message(s) le plus ancien(s) supprimé(s) sans résumé]", dropped)
			}
		}()
	}

	// dispatchOrHandle traite line comme une commande spéciale si elle en
	// est une, sinon lance un tour via dispatchTurn. dispatched=true signale
	// qu'un tour a été lancé (donc que la boucle principale ne doit pas
	// réafficher le prompt tout de suite : il faut attendre doneCh).
	dispatchOrHandle := func(line string) (dispatched, exit bool) {
		if line == "/compact" {
			dispatchCompact()
			return true, false
		}
		if handled, ex := runCommand(line); handled {
			return false, ex
		}
		dispatchTurn(line)
		return true, false
	}

	// chatLinesCh reçoit les lignes tapées qui ne répondent pas à une
	// confirmation en attente (voir term). Bufferisé pour ne jamais bloquer
	// la goroutine de lecture, y compris si la boucle principale est
	// momentanément occupée à autre chose (ex. les confirmations affichées
	// avant même le début de la boucle, plus haut).
	chatLinesCh := make(chan chatLine, 64)
	go func() {
		for {
			rawLine, ok := readLine()
			if !ok {
				// Débloque une éventuelle confirmation en attente (réponse
				// "non" par défaut, comme le faisait déjà l'ancien
				// readLine() sur ok=false) avant de signaler la fin de
				// saisie à la boucle principale.
				term.Dispatch("")
				chatLinesCh <- chatLine{eof: true}
				return
			}
			if term.Dispatch(rawLine) {
				chatLinesCh <- chatLine{text: rawLine}
			}
		}
	}()

	fmt.Fprintln(out, "Harnais LLM — mode interactif. Tapez /exit pour quitter, /new (ou /reset) pour vider l'historique, /compact pour compacter maintenant.")
	fmt.Fprint(out, promptIdle)

	var queue []string
	busy := false
	eofPending := false

	for {
		select {
		case cl := <-chatLinesCh:
			if cl.eof {
				eofPending = true
				if !busy {
					if scanErr != nil {
						return scanErr
					}
					return nil
				}
				continue
			}
			line := strings.TrimSpace(cl.text)
			if line == "" {
				if !busy {
					fmt.Fprint(out, promptIdle)
				}
				continue
			}
			if busy {
				queue = append(queue, line)
				fmt.Fprintln(out, color(ansiCyan, "  [mis en file d'attente, traité après la réponse en cours]"))
				continue
			}
			dispatched, exit := dispatchOrHandle(line)
			if exit {
				return nil
			}
			busy = dispatched
			if !dispatched {
				fmt.Fprint(out, promptIdle)
			}

		case cancelled := <-doneCh:
			busy = false
			if cancelled && len(queue) > 0 {
				queue = nil
				fmt.Fprintln(out, color(ansiYellow, "[file d'attente vidée après annulation]"))
			}
			for len(queue) > 0 {
				next := queue[0]
				queue = queue[1:]
				// Le message tapé peut être loin en arrière dans le
				// défilement du terminal (le tour précédent a pu produire
				// beaucoup de sortie) : on le rappelle en gris au moment où
				// son traitement démarre, pour qu'il soit clair lequel des
				// messages en file est en cours.
				fmt.Fprintln(out, color(ansiGray, "> "+next))
				dispatched, exit := dispatchOrHandle(next)
				if exit {
					return nil
				}
				if dispatched {
					busy = true
					break
				}
			}
			if !busy {
				if eofPending {
					if scanErr != nil {
						return scanErr
					}
					return nil
				}
				fmt.Fprint(out, promptIdle)
			}
		}
	}
}

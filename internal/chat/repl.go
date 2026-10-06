// Package chat implémente le mode interactif console du harnais.
package chat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"bot/internal/agent"
	"bot/internal/convo"
	"bot/internal/llm"
	"bot/internal/sandbox"
	"bot/internal/tools"
	"bot/internal/voice"
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
	// WhitelistedCommandsFile : fichier de persistance des commandes
	// "as_real_user" autorisées une fois pour toutes (voir
	// tools.CommandWhitelist, paramètre "white_list" de run_shell). "" =
	// liste en mémoire seulement, pour la session.
	WhitelistedCommandsFile string
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
	// Claude* : voir tools.ClaudeTool (run_claude).
	ClaudeEnabled        bool
	ClaudeBin            string
	ClaudePermissionMode string
	ClaudeTimeout        time.Duration
	ClaudeMaxTimeout     time.Duration
	HTTPTimeout          time.Duration
	// BrowserFetchTimeout : voir tools.BrowserFetchTool.Timeout.
	BrowserFetchTimeout time.Duration
	MaxSteps            int
	// MaxConsecutiveShellFailures : voir agent.Run — <= 0 désactive.
	MaxConsecutiveShellFailures int
	// RealUserWindow : durée de la fenêtre ouverte par "as_real_user_window"
	// (voir tools.ShellTool.ConfirmRealUser) une fois confirmée — <= 0 =
	// valeur par défaut (5 min).
	RealUserWindow time.Duration
	// WorkspaceDir : répertoire toujours accessible en lecture/écriture pour
	// read_file/write_file (voir tools.DirPermissions.AlwaysAllow), sans
	// passer par request_directory_access.
	WorkspaceDir string
	// SandboxSSHKey : chemin d'une clé privée SSH de l'utilisateur réel,
	// rendue lisible par le compte sandbox pour que git (via run_shell)
	// puisse s'authentifier sur un dépôt distant — voir
	// config.SandboxSSHKey et sandbox.EnsureSSHKeyAccess. "" = désactivé.
	SandboxSSHKey string
	// Voice : commande vocale (voir internal/voice). Les transcriptions sont
	// traitées exactement comme des lignes tapées au clavier.
	Voice voice.Config
}

// chatLine est le résultat d'une lecture de ligne au clavier, transmis par
// la goroutine de lecture du terminal (voir plus bas) à la boucle
// principale. eof=true signale une fin de saisie (Ctrl+D ou erreur de
// lecture).
type chatLine struct {
	text string
	eof  bool
	// voice : la ligne vient de la commande vocale (voir internal/voice) ;
	// la réponse finale est alors lue à voix haute si la synthèse est active.
	voice bool
}

// queuedLine est un message mis en file d'attente pendant un tour, avec
// l'origine vocale de la ligne (voir chatLine.voice).
type queuedLine struct {
	text  string
	voice bool
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
	// liveCursor : même calcul, pour savoir si des séquences ANSI de
	// repositionnement de curseur (indicateur "réflexion", voir
	// dispatchTurn) peuvent être utilisées sans polluer une sortie non
	// terminale (fichier, pipe, tests).
	liveCursor := colorsEnabled(out)
	// L'éditeur de ligne (édition + historique haut/bas) n'est activé que
	// si in est un vrai terminal interactif : sur une entrée redirigée
	// (pipe, fichier, tests), on garde bufio.Scanner tel quel. readLine()
	// unifie les deux : c'est la seule façon de lire une ligne dans tout le
	// reste de la fonction.
	//
	// À partir d'ici, plusieurs goroutines (streaming en tâche de fond, écho
	// clavier, boucle principale) écrivent potentiellement en parallèle vers
	// out : on le sérialise pour éviter des écritures entrelacées/coupées —
	// via console avec l'éditeur de ligne (qui garde en plus la saisie en
	// cours affichée en bas, sous la sortie, voir console.go), syncWriter
	// sinon.
	var editor *lineEditor
	if f, ok := in.(*os.File); ok && isTerminalFile(f) {
		con := newConsole(out, terminalWidth(f))
		if liveCursor {
			con.promptColor, con.inputColor = ansiInput, ansiInput
		}
		if ed, err := newLineEditor(f, con); err == nil {
			editor = ed
			out = con
		}
		// Si stty échoue (terminal exotique), on retombe silencieusement
		// sur bufio.Scanner ci-dessous : pas d'historique/édition avancée,
		// mais le chat reste utilisable.
	}
	if editor == nil {
		out = &syncWriter{out: out}
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var scanErr error

	// Avec l'éditeur de ligne, la zone de saisie (prompt "> " compris) est
	// affichée en permanence en bas de l'écran par console, que le modèle
	// soit en train de répondre ou non : une même ligne tapée peut aussi bien
	// répondre à une confirmation qu'alimenter le prochain message de chat
	// ou la file d'attente (voir plus bas, terminalInput). Sans éditeur, le
	// prompt est affiché par la boucle principale (showPrompt).
	readLine := func() (string, bool) {
		if editor != nil {
			return editor.ReadLine("> ")
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
	//
	// detail résume la demande pour la notification de bureau envoyée quand
	// la commande vocale est active (voiceSess) : on peut alors parler au bot
	// sans regarder le terminal, où s'affiche le détail complet.
	var voiceSess *voice.Session
	askYesNo := func(ctx context.Context, detail, prompt string) bool {
		fmt.Fprint(out, color(ansiMagenta, prompt))
		ch := term.Ask()
		if voiceSess != nil {
			voiceSess.AskNotify(detail)
		}
		select {
		case line := <-ch:
			return isYes(line)
		case <-ctx.Done():
			term.Cancel(ch)
			fmt.Fprintln(out, color(ansiYellow, "\n[demande annulée (Ctrl+C)]"))
			return false
		}
	}

	// realUserGrantWindow : durée de la fenêtre ouverte quand le modèle
	// demande explicitement "as_real_user_window" et que l'utilisateur
	// confirme (voir confirmRealUser) — une tâche qui enchaîne plusieurs
	// commandes sous l'identité réelle (ex: `glab auth login` puis une
	// vérification juste après) ne redemande pas à chaque appel tant que la
	// fenêtre est ouverte. Configurable (ToolsConfig.RealUserWindow) ;
	// volontairement court par défaut plutôt qu'une mémorisation permanente
	// comme DirPermissions : l'accès accordé ici est bien plus large
	// (n'importe quelle commande, pas un répertoire précis).
	realUserGrantWindow := toolsCfg.RealUserWindow
	if realUserGrantWindow <= 0 {
		realUserGrantWindow = 5 * time.Minute
	}
	var realUserGrantedUntil time.Time

	// confirmRealUser : voir tools.ShellTool.ConfirmRealUser. window indique
	// si le modèle demande l'ouverture d'une fenêtre de réutilisation
	// (as_real_user_window) ou une autorisation ponctuelle valable pour
	// cette seule commande.
	confirmRealUser := func(ctx context.Context, command string, window, whiteList bool) (bool, error) {
		if time.Now().Before(realUserGrantedUntil) {
			return true, nil
		}
		fmt.Fprintln(out, color(ansiMagenta, "\n[run_shell] le modèle demande à exécuter cette commande sous l'identité réelle (hors sandbox, accès complet) :"))
		fmt.Fprintln(out, color(ansiMagenta, "  "+command))
		if whiteList {
			fmt.Fprintln(out, color(ansiMagenta, "  (liste blanche demandée : une fois autorisée, CETTE commande exacte ne redemandera plus jamais, y compris aux prochains lancements)"))
		}
		if window {
			fmt.Fprintln(out, color(ansiMagenta, fmt.Sprintf("  (fenêtre demandée : une fois autorisé, valable %s pour les commandes suivantes sous l'identité réelle, sans redemander)", realUserGrantWindow)))
		} else {
			fmt.Fprintln(out, color(ansiMagenta, "  (autorisation ponctuelle : uniquement pour cette commande)"))
		}
		if !askYesNo(ctx, "Commande sous l'identité réelle : "+command, "  autoriser ? [o/N] ") {
			return false, nil
		}
		if window {
			realUserGrantedUntil = time.Now().Add(realUserGrantWindow)
		}
		return true, nil
	}

	// confirmClaude : voir tools.ClaudeTool.Confirm — demandé à chaque
	// appel, Claude Code tournant hors sandbox sous l'identité réelle.
	confirmClaude := func(ctx context.Context, prompt, workDir string) (bool, error) {
		fmt.Fprintln(out, color(ansiMagenta, fmt.Sprintf("\n[run_claude] le modèle demande à lancer Claude Code (hors sandbox, identité réelle) dans %s avec la tâche :", workDir)))
		for _, line := range strings.Split(prompt, "\n") {
			fmt.Fprintln(out, color(ansiMagenta, "  "+line))
		}
		return askYesNo(ctx, fmt.Sprintf("Lancer Claude Code dans %s : %s", workDir, prompt), "  autoriser ? [o/N] "), nil
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
			if err := sandbox.Ensure(out, func(prompt string) bool { return askYesNo(ctx, "Mise en place du sandbox", prompt) }); err != nil {
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
			if !askYesNo(ctx, "Accès au répertoire "+dir, "  autoriser ? [o/N] ") {
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
			// "/tmp" explicitement : sur macOS, os.TempDir() est $TMPDIR
			// (/var/folders/..., privé à l'utilisateur), pas /tmp.
			perms.AlwaysAllow(toolsCfg.WorkspaceDir, "/tmp", os.TempDir())
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
			&tools.EditFileTool{Perms: perms, Sandboxed: sandboxReady},
			&tools.HTTPGetTool{Timeout: toolsCfg.HTTPTimeout, Perms: perms, Sandboxed: sandboxReady},
			&tools.BrowserFetchTool{Timeout: toolsCfg.BrowserFetchTimeout, Perms: perms, Sandboxed: sandboxReady},
			&tools.RequestDirectoryAccessTool{Perms: perms},
			&tools.ListDirTool{},
			&tools.MathTool{},
		}
		if shellAvailable {
			whitelist, err := tools.NewCommandWhitelist(toolsCfg.WhitelistedCommandsFile)
			if err != nil {
				fmt.Fprintln(out, color(ansiRed, fmt.Sprintf("[avertissement: %v]", err)))
			}
			toolList = append(toolList, &tools.ShellTool{Timeout: toolsCfg.ShellTimeout, MaxTimeout: toolsCfg.ShellMaxTimeout, NotifyThreshold: toolsCfg.ShellNotifyThreshold, Sandboxed: sandboxReady, Perms: perms, GitConfigPath: gitConfigPath, HomeDir: homeDir, RealUserWindow: realUserGrantWindow, ConfirmRealUser: confirmRealUser, Whitelist: whitelist})
		}
		if toolsCfg.ClaudeEnabled {
			if _, err := exec.LookPath(toolsCfg.ClaudeBin); err != nil {
				fmt.Fprintln(out, color(ansiYellow, fmt.Sprintf("[run_claude] %q introuvable dans le PATH, run_claude ne sera pas proposé cette session", toolsCfg.ClaudeBin)))
			} else {
				toolList = append(toolList, &tools.ClaudeTool{Bin: toolsCfg.ClaudeBin, PermissionMode: toolsCfg.ClaudePermissionMode, Timeout: toolsCfg.ClaudeTimeout, MaxTimeout: toolsCfg.ClaudeMaxTimeout, Perms: perms, Confirm: confirmClaude})
			}
		}
		registry = tools.NewRegistry(toolList...)
		if !registry.Empty() {
			agent.AppendToolUsagePrompt(conv)
		}
	}

	promptIdle := color(ansiInput, "> ")
	// showPrompt : sans éditeur de ligne uniquement — avec, console affiche
	// déjà le prompt en permanence (voir readLine).
	showPrompt := func() {
		if editor == nil {
			fmt.Fprint(out, promptIdle)
		}
	}
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
	dispatchTurn := func(line string, spoken bool) {
		// speak lit la réponse FINALE à voix haute, seulement pour un message
		// dicté : ni le raisonnement ni les appels d'outils n'y passent.
		speak := func(reply string) {
			if spoken && voiceSess != nil {
				voiceSess.Speak(reply)
			}
		}
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

			// thinking : indicateur affiché dès l'envoi de la requête au
			// modèle, effacé dès la toute première sortie produite (premier
			// événement d'outil/réflexion, premier fragment de réponse en
			// streaming, ou message d'erreur) — comble le silence entre
			// l'envoi de la requête et sa première sortie visible, pendant
			// lequel rien n'indiquait avant que le modèle travaillait
			// effectivement. Seulement sur un vrai terminal (liveCursor) :
			// les séquences de repositionnement de curseur polluent une
			// sortie non terminale (fichier, pipe, tests).
			thinkingShown := liveCursor
			if thinkingShown {
				fmt.Fprint(out, color(ansiGray, "…réflexion"))
			}
			clearThinking := func() {
				if thinkingShown {
					fmt.Fprint(out, "\r\x1b[K")
					thinkingShown = false
				}
			}

			// streamed : état de l'affichage en streaming — dernier type de
			// fragment affiché (raisonnement ou réponse), pour n'imprimer
			// l'en-tête "┄ réflexion" qu'au début d'un bloc de raisonnement
			// et séparer celui-ci de la réponse qui suit. streamAfterEvent
			// : dernier affichage = un bloc d'outil/avertissement, dont la
			// réponse qui suit est séparée par une ligne vide.
			const (
				streamNone = iota
				streamAfterEvent
				streamReasoning
				streamContent
			)
			streamed := streamNone
			onReasoning := func(d string) {
				clearThinking()
				if streamed != streamReasoning {
					fmt.Fprint(out, color(ansiGray, "\n┄ réflexion\n┆ "))
					streamed = streamReasoning
				}
				fmt.Fprint(out, color(ansiGray, strings.ReplaceAll(d, "\n", "\n┆ ")))
			}
			onContent := func(d string) {
				clearThinking()
				switch streamed {
				case streamReasoning:
					fmt.Fprint(out, "\n\n")
				case streamAfterEvent:
					fmt.Fprint(out, "\n")
				}
				streamed = streamContent
				fmt.Fprint(out, d)
			}
			// endStream termine la ligne en cours après un streaming, pour
			// que la suite (prompt, message d'erreur) démarre proprement.
			endStream := func() {
				if streamed == streamReasoning || streamed == streamContent {
					fmt.Fprintln(out)
				}
				streamed = streamNone
			}

			if !registry.Empty() {
				reply, _, err := agent.Run(reqCtx, client, conv, registry, toolsCfg.MaxSteps, toolsCfg.MaxConsecutiveShellFailures, true, func(e agent.Event) {
					switch e.Kind {
					case agent.EventReasoningDelta:
						onReasoning(e.Result)
						return
					case agent.EventContentDelta:
						onContent(e.Result)
						return
					}
					clearThinking()
					endStream()
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
					streamed = streamAfterEvent
				})
				clearThinking()
				endStream()
				if err != nil {
					cancelled = errors.Is(err, context.Canceled)
					reqErrorLine(err)
					return
				}
				speak(reply)
				return
			}

			if compacted, err := conv.CompactIfNeeded(reqCtx, client); err != nil {
				clearThinking()
				errorLine("[avertissement: échec de la compaction du contexte: %v]", err)
			} else if compacted {
				clearThinking()
				fmt.Fprintln(out, "[contexte compacté automatiquement]")
			}
			// Filet de sécurité de dernier recours : voir le commentaire de
			// convo.Conversation.EnsureFitsContext.
			if dropped := conv.EnsureFitsContext(); dropped > 0 {
				clearThinking()
				errorLine("[avertissement: contexte encore trop grand après compaction : %d message(s) le plus ancien(s) supprimé(s) sans résumé]", dropped)
			}

			// Même signalement des relances automatiques que agent.Run
			// (EventWarning) : le texte partiel déjà affiché va être suivi
			// d'une nouvelle réponse complète.
			streamCtx := llm.WithRetryNotifier(reqCtx, func(attempt, max int, err error) {
				clearThinking()
				endStream()
				fmt.Fprint(out, color(ansiYellow, agent.Event{Kind: agent.EventWarning, Result: agent.RetryWarning(attempt, max, err)}.Format()))
				streamed = streamAfterEvent
			})
			msg, usage, err := client.ChatCompletionStream(streamCtx, conv.Full(), nil, llm.StreamCallbacks{OnContent: onContent, OnReasoning: onReasoning})
			clearThinking()
			endStream()
			if err != nil {
				cancelled = errors.Is(err, context.Canceled)
				reqErrorLine(err)
				return
			}

			reply := msg.Content
			if msg.FinishReason == llm.FinishReasonLength {
				errorLine("[avertissement: réponse interrompue par la limite de tokens de sortie (max_tokens=%d, voir LLM_MAX_TOKENS) : le modèle est peut-être parti en boucle]", client.MaxTokens)
				if strings.TrimSpace(reply) == "" {
					reply = "[réponse interrompue : limite de tokens de sortie atteinte]"
				}
			}
			conv.AddAssistant(reply)
			conv.RecordUsage(usage)
			speak(reply)
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
	dispatchOrHandle := func(line string, spoken bool) (dispatched, exit bool) {
		if line == "/compact" {
			dispatchCompact()
			return true, false
		}
		if handled, ex := runCommand(line); handled {
			return false, ex
		}
		dispatchTurn(line, spoken)
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

	// Commande vocale : chaque transcription suit le même chemin qu'une
	// ligne tapée (réponse à une confirmation en attente, sinon message de
	// chat ou commande, mis en file si un tour est en cours).
	if toolsCfg.Voice.Enabled {
		sess, err := voice.Start(ctx, toolsCfg.Voice)
		if err != nil {
			fmt.Fprintln(out, color(ansiRed, fmt.Sprintf("[voix] désactivée : %v", err)))
		} else {
			voiceSess = sess
			defer sess.Close()
			fmt.Fprintln(out, color(ansiCyan, "[voix] active : `bot voice toggle` (raccourci global) pour parler."))
			if tts := toolsCfg.Voice.TTS; tts != nil {
				if _, err := exec.LookPath(tts.Cmd[0]); err != nil {
					fmt.Fprintln(out, color(ansiYellow, fmt.Sprintf("[voix] lecture des réponses indisponible : %q introuvable dans le PATH (voir VOICE_TTS_CMD)", tts.Cmd[0])))
				} else {
					fmt.Fprintln(out, color(ansiCyan, "[voix] les réponses aux messages dictés sont lues à voix haute (`bot voice stop` pour couper, VOICE_TTS_ENABLED=false pour désactiver)."))
				}
			}
			go func() {
				for {
					select {
					case text := <-sess.Transcripts():
						fmt.Fprintln(out, color(ansiInput, "🎙 "+text))
						if term.Dispatch(text) {
							select {
							case chatLinesCh <- chatLine{text: text, voice: true}:
							case <-sess.Done():
								return
							}
						}
					case err := <-sess.Errors():
						fmt.Fprintln(out, color(ansiRed, "[voix] "+err.Error()))
					case <-sess.Done():
						return
					}
				}
			}()
		}
	}

	fmt.Fprintln(out, "Harnais LLM — mode interactif. Tapez /exit pour quitter, /new (ou /reset) pour vider l'historique, /compact pour compacter maintenant.")
	showPrompt()

	var queue []queuedLine
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
					showPrompt()
				}
				continue
			}
			if busy {
				queue = append(queue, queuedLine{text: line, voice: cl.voice})
				fmt.Fprintln(out, color(ansiCyan, "  [mis en file d'attente, traité après la réponse en cours]"))
				continue
			}
			dispatched, exit := dispatchOrHandle(line, cl.voice)
			if exit {
				return nil
			}
			busy = dispatched
			if !dispatched {
				showPrompt()
			}

		case cancelled := <-doneCh:
			busy = false
			if cancelled && len(queue) > 0 {
				queue = nil
				fmt.Fprintln(out, color(ansiYellow, "[file d'attente vidée après annulation]"))
			}
			for len(queue) > 0 {
				// Une commande en file (/new, /compact, /exit...) est traitée
				// seule, à sa place : fusionner des messages de part et
				// d'autre d'un /new, par exemple, changerait le sens.
				if isQueueCommand(queue[0].text) {
					next := queue[0]
					queue = queue[1:]
					fmt.Fprintln(out, color(ansiInput, "> "+next.text))
					dispatched, exit := dispatchOrHandle(next.text, next.voice)
					if exit {
						return nil
					}
					if dispatched {
						busy = true
						break
					}
					continue
				}
				// Messages de chat consécutifs en file : fusionnés en un seul
				// envoi, plutôt qu'un tour complet par message — typiquement
				// des précisions ajoutées au fil de l'eau pendant la réponse
				// précédente, que le modèle doit lire ensemble.
				n := 1
				for n < len(queue) && !isQueueCommand(queue[n].text) {
					n++
				}
				batch := queue[:n]
				queue = queue[n:]
				// Les messages tapés peuvent être loin en arrière dans le
				// défilement du terminal (le tour précédent a pu produire
				// beaucoup de sortie) : on les rappelle au moment où leur
				// traitement démarre (même couleur que la saisie).
				texts := make([]string, len(batch))
				spoken := false
				for i, l := range batch {
					fmt.Fprintln(out, color(ansiInput, "> "+l.text))
					texts[i] = l.text
					spoken = spoken || l.voice
				}
				if n > 1 {
					fmt.Fprintln(out, color(ansiCyan, fmt.Sprintf("  [%d messages en attente fusionnés en un seul envoi]", n)))
				}
				dispatchTurn(strings.Join(texts, "\n\n"), spoken)
				busy = true
				break
			}
			if !busy {
				if eofPending {
					if scanErr != nil {
						return scanErr
					}
					return nil
				}
				showPrompt()
			}
		}
	}
}

// isYes indique si line répond oui à une confirmation. Tolérant à la
// sortie d'une transcription vocale (« Oui. », « Ouais ! »).
func isYes(line string) bool {
	answer := strings.ToLower(strings.TrimFunc(line, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	}))
	switch answer {
	case "o", "oui", "ouais", "y", "yes":
		return true
	}
	return false
}

// isQueueCommand indique si line (déjà nettoyée des espaces) est une
// commande spéciale du mode chat (voir runCommand/dispatchOrHandle) plutôt
// qu'un message pour le modèle — utilisé pour ne jamais la fusionner avec
// des messages en file d'attente. À tenir à jour avec runCommand.
func isQueueCommand(line string) bool {
	switch line {
	case "/exit", "/quit", "/new", "/reset", "/stats", "/compact":
		return true
	}
	return false
}

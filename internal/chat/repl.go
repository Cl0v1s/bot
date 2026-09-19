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
	// ShellSandboxEnabled : exécute run_shell sous le compte système
	// restreint "llm" (internal/sandbox) plutôt que sous l'utilisateur
	// courant. La configuration est vérifiée (et effectuée si besoin, avec
	// confirmation) à chaque lancement.
	ShellSandboxEnabled bool
	ShellTimeout        time.Duration
	HTTPTimeout         time.Duration
	MaxSteps            int
	// WorkspaceDir : répertoire toujours accessible en lecture/écriture pour
	// read_file/write_file (voir tools.DirPermissions.AlwaysAllow), sans
	// passer par request_directory_access.
	WorkspaceDir string
}

// Run lance une boucle de lecture-évaluation-affichage sur la console.
// Commandes spéciales : /exit, /reset, /stats.
//
// Si toolsCfg.Enabled, chaque tour passe par la boucle agentique
// (internal/agent) : les appels d'outils et leurs résultats sont affichés
// au fur et à mesure, nettement séparés (encadrés) du texte de réponse du
// modèle. Par défaut, read_file/write_file n'ont accès à aucun répertoire :
// le modèle doit appeler request_directory_access, ce qui déclenche ici une
// question interactive (o/N) à l'utilisateur — le verrouillage est appliqué
// dans les tools eux-mêmes (internal/tools.DirPermissions), pas par une
// simple instruction de prompt.
func Run(ctx context.Context, client *llm.Client, conv *convo.Conversation, toolsCfg ToolsConfig, in io.Reader, out io.Writer) error {
	color := colorizer(out)

	// L'éditeur de ligne (édition + historique haut/bas) n'est activé que
	// si in est un vrai terminal interactif : sur une entrée redirigée
	// (pipe, fichier, tests), on garde bufio.Scanner tel quel. readLine()
	// unifie les deux : c'est la seule façon de lire une ligne dans tout le
	// reste de la fonction (prompt principal, confirmations o/N...).
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

	readLine := func(prompt string) (string, bool) {
		if editor != nil {
			return editor.ReadLine(prompt)
		}
		fmt.Fprint(out, prompt)
		if !scanner.Scan() {
			scanErr = scanner.Err()
			return "", false
		}
		return scanner.Text(), true
	}

	// askYesNo lit la ligne suivante saisie par l'utilisateur via readLine :
	// c'est volontaire et sûr, les appels sont toujours séquentiels, jamais
	// concurrents (agent.Run bloque la boucle principale pendant son tour).
	// Le prompt est affiché en violet : toute demande d'interaction
	// utilisateur (permission, confirmation) partage ce même code couleur.
	askYesNo := func(prompt string) bool {
		line, ok := readLine(color(ansiMagenta, prompt))
		if !ok {
			return false
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "o" || answer == "oui" || answer == "y" || answer == "yes"
	}

	var registry *tools.Registry
	if toolsCfg.Enabled {
		// shellAvailable : run_shell n'est proposé que si le sandbox n'est pas
		// requis, ou s'il a été mis en place avec succès — jamais de repli
		// silencieux vers une exécution non isolée quand l'utilisateur a
		// explicitement demandé le sandbox.
		shellAvailable := !toolsCfg.ShellSandboxEnabled
		sandboxReady := false
		if toolsCfg.ShellSandboxEnabled {
			if err := sandbox.Ensure(out, askYesNo); err != nil {
				fmt.Fprintln(out, color(ansiRed, fmt.Sprintf("[sandbox] indisponible, run_shell ne sera pas proposé cette session : %v", err)))
			} else {
				sandboxReady = true
				shellAvailable = true
			}
		}

		perms := tools.NewDirPermissions(func(dir, reason string) (bool, error) {
			fmt.Fprintln(out, color(ansiMagenta, fmt.Sprintf("\n[permission] le modèle demande l'accès au répertoire : %s", dir)))
			if reason != "" {
				fmt.Fprintln(out, color(ansiMagenta, fmt.Sprintf("  raison : %s", reason)))
			}
			if !askYesNo("  autoriser ? [o/N] ") {
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
		// Le workspace du bot est toujours accessible, indépendamment des
		// accès accordés en direct par l'utilisateur (voir AlwaysAllow).
		if toolsCfg.WorkspaceDir != "" {
			perms.AlwaysAllow(toolsCfg.WorkspaceDir)
			if sandboxReady {
				if err := sandbox.GrantDirectory(toolsCfg.WorkspaceDir); err != nil {
					fmt.Fprintln(out, color(ansiRed, fmt.Sprintf("[sandbox] échec de l'ouverture du workspace (%s) au compte %q : %v", toolsCfg.WorkspaceDir, sandbox.User, err)))
				}
			}
		}

		toolList := []tools.Tool{
			&tools.ReadFileTool{Perms: perms},
			&tools.WriteFileTool{Perms: perms},
			&tools.HTTPGetTool{Timeout: toolsCfg.HTTPTimeout},
			&tools.RequestDirectoryAccessTool{Perms: perms},
		}
		if shellAvailable {
			toolList = append(toolList, &tools.ShellTool{Timeout: toolsCfg.ShellTimeout, Sandboxed: sandboxReady, Perms: perms})
		}
		registry = tools.NewRegistry(toolList...)
		if !registry.Empty() {
			agent.AppendToolUsagePrompt(conv)
		}
	}

	prompt := color(ansiBold+ansiCyan, "> ")
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

	fmt.Fprintln(out, "Harnais LLM — mode interactif. Tapez /exit pour quitter, /new (ou /reset) pour vider l'historique.")

	for {
		rawLine, ok := readLine(prompt)
		if !ok {
			break
		}
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		switch line {
		case "/exit", "/quit":
			return nil
		case "/new", "/reset":
			conv.Reset()
			fmt.Fprintln(out, "[historique vidé]")
			continue
		case "/stats":
			basis := "estimé (~4 car./token)"
			if conv.TokensAreExact() {
				basis = "exact (usage API)"
			} else if conv.LastKnownTokens > 0 {
				basis = "exact + estimation du dernier message"
			}
			fmt.Fprintf(out, "[messages=%d tokens~=%d/%d (%s)]\n", len(conv.Messages), conv.EstimateTokens(), conv.MaxContextTokens, basis)
			continue
		}

		conv.AddUser(line)

		// reqCtx est propre à ce tour : un Ctrl+C pendant son exécution
		// n'annule que lui (endReq désarme l'annulation dès qu'il se
		// termine, pour qu'un Ctrl+C ultérieur au prompt quitte le
		// programme au lieu de casser silencieusement les tours suivants).
		reqCtx, endReq := interrupt.begin(ctx)

		if !registry.Empty() {
			reply, _, err := agent.Run(reqCtx, client, conv, registry, toolsCfg.MaxSteps, func(e agent.Event) {
				code := ansiYellow // appel d'outil
				if e.Kind == agent.EventToolResult {
					code = ansiGreen // résultat d'outil
					if e.Err != nil {
						code = ansiRed // résultat en erreur
					}
				}
				fmt.Fprint(out, color(code, e.Format()))
			})
			endReq()
			if err != nil {
				reqErrorLine(err)
				continue
			}
			fmt.Fprintln(out, "\n"+reply)
			continue
		}

		if compacted, err := conv.CompactIfNeeded(reqCtx, client); err != nil {
			errorLine("[avertissement: échec de la compaction du contexte: %v]", err)
		} else if compacted {
			fmt.Fprintln(out, "[contexte compacté automatiquement]")
		}

		reply, usage, err := client.ChatCompletionStream(reqCtx, conv.Full(), func(delta string) {
			fmt.Fprint(out, delta)
		})
		endReq()
		if err != nil {
			reqErrorLine(err)
			continue
		}
		fmt.Fprintln(out)

		conv.AddAssistant(reply)
		conv.RecordUsage(usage)
	}

	if scanErr != nil {
		return scanErr
	}
	return nil
}

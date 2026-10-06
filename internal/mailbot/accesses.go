package mailbot

import (
	"encoding/json"
	"fmt"
	"strings"

	"bot/internal/agent"
)

// access : un accès (fichier, web, commande...) réalisé par un outil
// pendant le tour, à lister en fin de mail.
type access struct {
	tool   string
	target string
	failed bool
}

// accessLog collecte les accès réalisés via les événements de l'agent.
type accessLog struct {
	items []access
}

// handle enregistre les appels d'outils (EventToolCall) et marque en échec
// l'accès correspondant quand le résultat revient en erreur.
func (l *accessLog) handle(e agent.Event) {
	switch e.Kind {
	case agent.EventToolCall:
		l.items = append(l.items, access{tool: e.Tool, target: describeAccess(e.Tool, e.Args)})
	case agent.EventToolResult:
		if e.Err == nil {
			return
		}
		for i := len(l.items) - 1; i >= 0; i-- {
			if l.items[i].tool == e.Tool && !l.items[i].failed {
				l.items[i].failed = true
				break
			}
		}
	}
}

// describeAccess extrait des arguments JSON de l'outil la cible de l'accès
// (chemin, URL, commande...).
func describeAccess(tool, argsJSON string) string {
	var a map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return strings.TrimSpace(argsJSON)
	}
	str := func(k string) string {
		s, _ := a[k].(string)
		return s
	}
	var target string
	switch tool {
	case "read_file", "write_file", "edit_file", "list_dir":
		target = str("path")
	case "http_get", "browser_fetch":
		target = str("url")
		if s := str("save_to"); s != "" {
			target += " → " + s
		}
	case "run_shell":
		target = str("command")
	case "request_directory_access":
		target = str("directory")
	case "run_claude":
		target = str("working_dir")
	}
	if target == "" {
		target = strings.TrimSpace(argsJSON)
	}
	return strings.Join(strings.Fields(target), " ")
}

// format retourne le bloc à ajouter en fin de mail ("" si aucun accès).
func (l *accessLog) format() string {
	if len(l.items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n--\nAccès réalisés pendant ce tour :\n")
	for _, it := range l.items {
		suffix := ""
		if it.failed {
			suffix = " (échec)"
		}
		fmt.Fprintf(&b, "- %s : %s%s\n", it.tool, truncateAccess(it.target, 300), suffix)
	}
	return strings.TrimRight(b.String(), "\n")
}

func truncateAccess(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

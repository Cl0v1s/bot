package tools

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// notifyOS envoie une notification de bureau, en tâche de fond et sans
// jamais faire échouer l'appelant : ni binaire de notification manquant, ni
// absence de session graphique (mode mail, environnement sans DISPLAY/
// DBUS_SESSION_BUS_ADDRESS...) ne doivent faire échouer le tool call qui
// déclenche la notification, qui n'est qu'un confort. Minimal, sans
// dépendance externe : uniquement des binaires déjà présents sur un
// environnement de bureau standard (notify-send sous Linux, osascript sous
// macOS). Aucun effet sur les autres systèmes.
func notifyOS(title, body string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("notify-send", "--", title, body)
	case "darwin":
		script := fmt.Sprintf("display notification %s with title %s", quoteAppleScript(body), quoteAppleScript(title))
		cmd = exec.Command("osascript", "-e", script)
	default:
		return
	}
	_ = cmd.Run()
}

// quoteAppleScript échappe s pour l'insérer comme chaîne littérale
// AppleScript (entre guillemets doubles).
func quoteAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

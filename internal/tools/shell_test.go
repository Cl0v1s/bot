package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withFakeNotifySend place, en tête du PATH pour la durée du test, un faux
// "notify-send" qui journalise ses arguments dans un fichier — pour vérifier
// que le mécanisme de notification (best-effort, en tâche de fond) se
// déclenche bien, sans dépendre d'une vraie session graphique.
func withFakeNotifySend(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "notify.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	fake := filepath.Join(dir, "notify-send")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("écriture du faux notify-send: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// waitForFile attend jusqu'à timeout que path existe et contienne du
// contenu, pour laisser le temps à la notification (déclenchée dans une
// goroutine détachée) de s'exécuter.
func waitForFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("notification jamais reçue (fichier %q vide ou absent après %s)", path, timeout)
	return ""
}

// Sans "timeout_seconds", la commande utilise le timeout par défaut du tool.
func TestShellDefaultTimeout(t *testing.T) {
	tool := &ShellTool{Timeout: 200 * time.Millisecond}
	out, err := tool.Call(context.Background(), `{"command":"sleep 5"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	want := fmt.Sprintf("[commande interrompue après %s (timeout)]", 200*time.Millisecond)
	if !strings.Contains(out, want) {
		t.Fatalf("sortie = %q, attendu contenant %q", out, want)
	}
}

// "timeout_seconds" doit pouvoir raccourcir le timeout effectif en dessous
// du défaut du tool (utile pour une commande dont on attend un échec rapide).
func TestShellTimeoutSecondsOverridesDefault(t *testing.T) {
	tool := &ShellTool{Timeout: 5 * time.Second, MaxTimeout: 5 * time.Second}
	out, err := tool.Call(context.Background(), `{"command":"sleep 5","timeout_seconds":1}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	want := "[commande interrompue après 1s (timeout)]"
	if !strings.Contains(out, want) {
		t.Fatalf("sortie = %q, attendu contenant %q (timeout_seconds ignoré ?)", out, want)
	}
}

// Une demande de timeout au-delà de MaxTimeout doit être plafonnée, pas
// honorée telle quelle : sans ça, le modèle pourrait monopoliser le sandbox
// avec une commande bloquante en demandant un timeout arbitrairement long.
func TestShellTimeoutSecondsClampedToMax(t *testing.T) {
	tool := &ShellTool{Timeout: 200 * time.Millisecond, MaxTimeout: 500 * time.Millisecond}
	out, err := tool.Call(context.Background(), `{"command":"sleep 5","timeout_seconds":100}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	want := fmt.Sprintf("[commande interrompue après %s (timeout)]", 500*time.Millisecond)
	if !strings.Contains(out, want) {
		t.Fatalf("sortie = %q, attendu contenant %q (plafond MaxTimeout non appliqué ?)", out, want)
	}
}

// Une commande dont la durée réelle dépasse NotifyThreshold doit déclencher
// une notification de bureau à la fin.
func TestShellNotifiesWhenOverThreshold(t *testing.T) {
	logPath := withFakeNotifySend(t)
	tool := &ShellTool{Timeout: 2 * time.Second, NotifyThreshold: 100 * time.Millisecond}

	out, err := tool.Call(context.Background(), `{"command":"sleep 0.3"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Contains(out, "timeout") {
		t.Fatalf("la commande n'aurait pas dû timeout: %q", out)
	}

	got := waitForFile(t, logPath, 2*time.Second)
	if !strings.Contains(got, "run_shell") || !strings.Contains(got, "sleep 0.3") {
		t.Fatalf("notification = %q, attendu un titre run_shell et la commande dans le corps", got)
	}
}

// Une commande plus rapide que NotifyThreshold ne doit déclencher aucune
// notification.
func TestShellDoesNotNotifyUnderThreshold(t *testing.T) {
	logPath := withFakeNotifySend(t)
	tool := &ShellTool{Timeout: 2 * time.Second, NotifyThreshold: 2 * time.Second}

	if _, err := tool.Call(context.Background(), `{"command":"echo bonjour"}`); err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Laisse une marge pour qu'une notification déclenchée à tort (bug) ait
	// le temps d'écrire le fichier avant qu'on ne vérifie son absence.
	time.Sleep(150 * time.Millisecond)
	if data, err := os.ReadFile(logPath); err == nil && len(data) > 0 {
		t.Fatalf("notification envoyée alors que sous le seuil: %q", string(data))
	}
}

// NotifyThreshold <= 0 désactive complètement les notifications, quelle que
// soit la durée de la commande.
func TestShellNotifyDisabledByDefault(t *testing.T) {
	logPath := withFakeNotifySend(t)
	tool := &ShellTool{Timeout: 2 * time.Second} // NotifyThreshold laissé à zéro

	if _, err := tool.Call(context.Background(), `{"command":"sleep 0.2"}`); err != nil {
		t.Fatalf("Call: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	if data, err := os.ReadFile(logPath); err == nil && len(data) > 0 {
		t.Fatalf("notification envoyée alors que NotifyThreshold est désactivé (0): %q", string(data))
	}
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// withFakeNotifySend place, en tête du PATH pour la durée du test, un faux
// "notify-send" (Linux) et un faux "osascript" (macOS, voir notifyOS) qui
// journalisent leurs arguments dans un fichier — pour vérifier que le
// mécanisme de notification (best-effort, en tâche de fond) se déclenche
// bien, sans dépendre d'une vraie session graphique.
func withFakeNotifySend(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "notify.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	for _, name := range []string{"notify-send", "osascript"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("écriture du faux %s: %v", name, err)
		}
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

// Un timeout doit tuer TOUT l'arbre de processus, pas seulement le process
// de tête (sh) — un job mis en arrière-plan par la commande elle-même
// (`sleep 30 &`) ne doit pas survivre indéfiniment au timeout juste parce
// que le process de tête, lui, a bien été tué (régression : cmd.Process.
// Kill() par défaut n'atteint jamais un tel descendant détaché).
func TestShellTimeoutKillsBackgroundedChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	tool := &ShellTool{Timeout: 300 * time.Millisecond}
	command := fmt.Sprintf("sleep 30 & echo $! > %s; sleep 30", pidFile)
	argsJSON, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatalf("construction des arguments: %v", err)
	}
	if _, err := tool.Call(context.Background(), string(argsJSON)); err != nil {
		t.Fatalf("Call: %v", err)
	}

	pidData := waitForFile(t, pidFile, 2*time.Second)
	pid, err := strconv.Atoi(strings.TrimSpace(pidData))
	if err != nil {
		t.Fatalf("pid invalide dans %q: %q (%v)", pidFile, pidData, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // process introuvable : bien tué
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL) // nettoyage best-effort avant de faire échouer le test
	t.Fatalf("job en arrière-plan (pid %d) toujours vivant après le timeout de run_shell", pid)
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

// "as_real_user" n'a aucun effet particulier quand le tool n'est pas
// sandboxé (déjà l'identité réelle par défaut) : ConfirmRealUser ne doit
// même pas être consulté.
func TestShellAsRealUserNoOpWhenNotSandboxed(t *testing.T) {
	called := false
	tool := &ShellTool{
		Timeout:         2 * time.Second,
		ConfirmRealUser: func(ctx context.Context, command string, window bool) (bool, error) { called = true; return true, nil },
	}
	out, err := tool.Call(context.Background(), `{"command":"echo bonjour","as_real_user":true}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if called {
		t.Fatal("ConfirmRealUser appelé alors que le tool n'est pas sandboxé")
	}
	if !strings.Contains(out, "bonjour") {
		t.Fatalf("sortie = %q, attendu qu'elle contienne bonjour", out)
	}
}

// Sandboxé, mais sans ConfirmRealUser configuré (ex: mode mail, aucun humain
// disponible) : "as_real_user" doit échouer explicitement, pas se rabattre
// silencieusement sur le compte sandbox ni sur l'identité réelle sans
// confirmation.
func TestShellAsRealUserFailsWithoutConfirmCallback(t *testing.T) {
	tool := &ShellTool{Timeout: 2 * time.Second, Sandboxed: true}
	_, err := tool.Call(context.Background(), `{"command":"echo bonjour","as_real_user":true}`)
	if err == nil {
		t.Fatal("attendu une erreur (ConfirmRealUser non configuré)")
	}
	if !strings.Contains(err.Error(), "shell interactif") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne l'absence de shell interactif accessible", err.Error())
	}
}

// La description et le schéma doivent refléter que "as_real_user" est
// indisponible quand ConfirmRealUser est nil (ex: mode mail) — pour que le
// modèle le sache AVANT d'essayer, pas seulement après un échec.
func TestShellDescriptionReflectsUnavailableRealUser(t *testing.T) {
	tool := &ShellTool{Sandboxed: true} // ConfirmRealUser laissé nil
	if !strings.Contains(tool.Description(), "shell interactif") {
		t.Fatalf("Description() = %q, attendu qu'elle mentionne l'indisponibilité (shell interactif)", tool.Description())
	}
	if !strings.Contains(string(tool.ParametersSchema()), "INDISPONIBLE") {
		t.Fatalf("ParametersSchema() = %s, attendu qu'il mentionne l'indisponibilité", tool.ParametersSchema())
	}
}

// Un refus explicite de ConfirmRealUser doit faire échouer l'appel avec un
// message clair, sans jamais retomber sur le compte sandbox à la place.
func TestShellAsRealUserFailsWhenConfirmationDenied(t *testing.T) {
	tool := &ShellTool{
		Timeout:         2 * time.Second,
		Sandboxed:       true,
		ConfirmRealUser: func(ctx context.Context, command string, window bool) (bool, error) { return false, nil },
	}
	_, err := tool.Call(context.Background(), `{"command":"echo bonjour","as_real_user":true}`)
	if err == nil {
		t.Fatal("attendu une erreur (confirmation refusée)")
	}
	if !strings.Contains(err.Error(), "refusée") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne le refus", err.Error())
	}
}

// ConfirmRealUser doit recevoir exactement la commande demandée.
func TestShellAsRealUserPassesExactCommandToConfirm(t *testing.T) {
	var gotCommand string
	tool := &ShellTool{
		Timeout:   2 * time.Second,
		Sandboxed: true,
		ConfirmRealUser: func(ctx context.Context, command string, window bool) (bool, error) {
			gotCommand = command
			return true, nil
		},
	}
	if _, err := tool.Call(context.Background(), `{"command":"echo bonjour","as_real_user":true}`); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotCommand != "echo bonjour" {
		t.Fatalf("commande reçue par ConfirmRealUser = %q, attendu %q", gotCommand, "echo bonjour")
	}
}

// "as_real_user_window" sans "as_real_user" est une erreur de validation,
// pas un no-op silencieux.
func TestShellAsRealUserWindowRequiresAsRealUser(t *testing.T) {
	tool := &ShellTool{
		Timeout:         2 * time.Second,
		Sandboxed:       true,
		ConfirmRealUser: func(ctx context.Context, command string, window bool) (bool, error) { return true, nil },
	}
	_, err := tool.Call(context.Background(), `{"command":"echo bonjour","as_real_user_window":true}`)
	if err == nil {
		t.Fatal("attendu une erreur (as_real_user_window sans as_real_user)")
	}
	if !strings.Contains(err.Error(), "as_real_user_window") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne as_real_user_window", err.Error())
	}
}

// ConfirmRealUser doit recevoir exactement la valeur de "as_real_user_window"
// demandée par le modèle : faux par défaut (mode oneshot), vrai seulement si
// explicitement demandé.
func TestShellAsRealUserPassesWindowFlagToConfirm(t *testing.T) {
	var gotWindow bool
	tool := &ShellTool{
		Timeout:   2 * time.Second,
		Sandboxed: true,
		ConfirmRealUser: func(ctx context.Context, command string, window bool) (bool, error) {
			gotWindow = window
			return true, nil
		},
	}
	if _, err := tool.Call(context.Background(), `{"command":"echo bonjour","as_real_user":true}`); err != nil {
		t.Fatalf("Call (oneshot): %v", err)
	}
	if gotWindow {
		t.Fatal("window = true alors que as_real_user_window était omis (défaut attendu: oneshot)")
	}
	if _, err := tool.Call(context.Background(), `{"command":"echo bonjour","as_real_user":true,"as_real_user_window":true}`); err != nil {
		t.Fatalf("Call (window): %v", err)
	}
	if !gotWindow {
		t.Fatal("window = false alors que as_real_user_window:true était demandé")
	}
}

// Une fois la confirmation accordée, la commande doit s'exécuter comme un
// run_shell non sandboxé (sh -c direct) — pas via sandbox.WrapCommand (qui
// exigerait un compte sandbox réellement configuré sur la machine de test,
// absent ici) : une régression qui continuerait à passer par WrapCommand
// malgré la confirmation ferait échouer cet appel plutôt que de renvoyer la
// sortie attendue.
func TestShellAsRealUserRunsCommandDirectlyOnceConfirmed(t *testing.T) {
	tool := &ShellTool{
		Timeout:         2 * time.Second,
		Sandboxed:       true,
		ConfirmRealUser: func(ctx context.Context, command string, window bool) (bool, error) { return true, nil },
	}
	out, err := tool.Call(context.Background(), `{"command":"echo bonjour","as_real_user":true}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "bonjour") {
		t.Fatalf("sortie = %q, attendu qu'elle contienne bonjour (commande exécutée directement)", out)
	}
}

// La commande ne voit jamais de terminal : stdin n'en est pas un, et les
// variables de noTTYEnv sont bien exportées.
func TestShellNoTTYEnvironment(t *testing.T) {
	tool := &ShellTool{Timeout: 5 * time.Second}
	out, err := tool.Call(context.Background(), `{"command":"tty; echo \"GTP=$GIT_TERMINAL_PROMPT\""}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "not a tty") && !strings.Contains(out, "pas un tty") {
		t.Fatalf("stdin semble être un tty : %q", out)
	}
	if !strings.Contains(out, "GTP=0") {
		t.Fatalf("GIT_TERMINAL_PROMPT non exporté : %q", out)
	}
}

// L'enveloppe noTTYScript ne masque pas le code de sortie de la commande.
func TestShellNoTTYPreservesExitStatus(t *testing.T) {
	tool := &ShellTool{Timeout: 5 * time.Second}
	out, err := tool.Call(context.Background(), `{"command":"echo 'a b'; exit 3"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "a b") || !strings.Contains(out, "exit status 3") {
		t.Fatalf("sortie = %q, attendu \"a b\" et \"exit status 3\"", out)
	}
}

// Ouvrir un tty (sans O_NOCTTY, comme le fait une redirection sh) ne doit
// jamais en faire le terminal de contrôle de la commande : celle-ci n'est
// pas chef de session (voir noTTYScript).
func TestShellCannotAcquireControllingTTY(t *testing.T) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("pas de /dev/ptmx : %v", err)
	}
	defer ptmx.Close()
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Skipf("unlockpt: %v", e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Skipf("ptsname: %v", e)
	}
	pts := fmt.Sprintf("/dev/pts/%d", n)

	tool := &ShellTool{Timeout: 5 * time.Second}
	cmd := fmt.Sprintf(`: <>%s; cut -d' ' -f7 /proc/$$/stat`, pts)
	argsJSON, _ := json.Marshal(map[string]string{"command": cmd})
	out, err := tool.Call(context.Background(), string(argsJSON))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.TrimSpace(out) != "0" {
		t.Fatalf("tty_nr = %q après ouverture de %s, attendu 0 (aucun terminal de contrôle)", out, pts)
	}
}

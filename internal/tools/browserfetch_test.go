package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFakeExecutable crée, dans dir, un script shell exécutable nommé name
// qui exécute body, et retourne son chemin. Utilisé pour tester la
// découverte/l'invocation de "navigateurs" sans dépendre d'un vrai Firefox
// ou Chrome installé sur la machine qui fait tourner les tests.
func writeFakeExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("script shell non applicable sous Windows")
	}
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBrowserFetchRejectsInvalidURL(t *testing.T) {
	tool := &BrowserFetchTool{}
	for _, u := range []string{"", "ftp://example.com", "pas-une-url"} {
		_, err := tool.Call(context.Background(), fmt.Sprintf(`{"url":%q}`, u))
		if err == nil {
			t.Fatalf("url=%q: attendu une erreur", u)
		}
	}
}

// findChromeBinary doit respecter l'ordre de préférence déclaré
// (chromium avant chromium-browser, par ex.) et ignorer les noms absents.
func TestFindChromeBinaryPrefersEarlierNames(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "chromium-browser", "exit 0")
	writeFakeExecutable(t, dir, "google-chrome", "exit 0")
	t.Setenv("PATH", dir)

	got := findChromeBinary()
	want := filepath.Join(dir, "chromium-browser")
	if got != want {
		t.Fatalf("findChromeBinary() = %q, want %q (premier nom présent dans l'ordre de préférence)", got, want)
	}
}

func TestFindChromeBinaryEmptyWhenNoneInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	withIsolatedPlaywrightCache(t)
	if got := findChromeBinary(); got != "" {
		t.Fatalf("findChromeBinary() = %q, want \"\"", got)
	}
}

// withIsolatedPlaywrightCache isole findPlaywrightChromium ($HOME) de
// l'état réel de la machine qui exécute les tests — qui peut très bien
// avoir un Chromium Playwright déjà installé.
func withIsolatedPlaywrightCache(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func writeFakePlaywrightChromium(t *testing.T, home string, build int) string {
	t.Helper()
	dir := filepath.Join(home, playwrightCacheSubdir, fmt.Sprintf("chromium-%d", build), "chrome-linux64")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "chrome")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Sans navigateur Chromium natif sur le PATH, findChromeBinary doit se
// rabattre sur le Chromium téléchargé par Playwright (npx playwright
// install chromium), pas géré via Flatpak (voir le commentaire de package).
func TestFindChromeBinaryFallsBackToPlaywright(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	withIsolatedPlaywrightCache(t)
	home, _ := os.UserHomeDir()

	fake := writeFakePlaywrightChromium(t, home, 1243)

	got := findChromeBinary()
	if got != fake {
		t.Fatalf("findChromeBinary() = %q, want %q (repli Playwright)", got, fake)
	}
}

// Avec plusieurs versions Playwright installées (installations successives),
// le build le plus élevé doit être préféré.
func TestFindChromeBinaryPrefersLatestPlaywrightBuild(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	withIsolatedPlaywrightCache(t)
	home, _ := os.UserHomeDir()

	writeFakePlaywrightChromium(t, home, 1228)
	latest := writeFakePlaywrightChromium(t, home, 1243)

	got := findChromeBinary()
	if got != latest {
		t.Fatalf("findChromeBinary() = %q, want %q (build le plus récent)", got, latest)
	}
}

// firefoxBinary ne doit jamais détecter un Firefox installé en Flatpak
// (voir l'avertissement "Flatpak" dans le commentaire de package) : même
// avec un faux binaire nommé comme l'export Flatpak standard présent sur le
// PATH, seuls les noms natifs ("firefox"/"firefox-esr") doivent compter.
func TestFirefoxBinaryIgnoresFlatpakStyleName(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "org.mozilla.firefox", "exit 0")
	t.Setenv("PATH", dir)

	if got := firefoxBinary(); got != "" {
		t.Fatalf("firefoxBinary() = %q, want \"\" (un nom de style Flatpak sur le PATH ne doit pas être reconnu)", got)
	}
}

func TestFirefoxHeadlessBinaryRequiresBothBinaries(t *testing.T) {
	dir := t.TempDir()

	t.Run("aucun des deux", func(t *testing.T) {
		t.Setenv("PATH", dir)
		if firefoxHeadlessBinary() != "" {
			t.Fatal("attendu false sans firefox ni geckodriver")
		}
	})

	firefoxOnly := t.TempDir()
	writeFakeExecutable(t, firefoxOnly, "firefox", "exit 0")
	t.Run("firefox seul, sans geckodriver", func(t *testing.T) {
		t.Setenv("PATH", firefoxOnly)
		if firefoxHeadlessBinary() != "" {
			t.Fatal("attendu false sans geckodriver, même si firefox est présent")
		}
	})

	both := t.TempDir()
	writeFakeExecutable(t, both, "firefox", "exit 0")
	writeFakeExecutable(t, both, "geckodriver", "exit 0")
	t.Run("les deux présents", func(t *testing.T) {
		t.Setenv("PATH", both)
		if firefoxHeadlessBinary() == "" {
			t.Fatal("attendu un binaire avec firefox et geckodriver tous les deux présents")
		}
	})
}

// fetchWithChrome doit passer --dump-dom et l'URL cible, puis renvoyer
// exactement la sortie standard du binaire.
func TestFetchWithChromeCapturesStdout(t *testing.T) {
	dir := t.TempDir()
	// Vérifie la présence de --dump-dom et de l'URL parmi les arguments
	// reçus, pour détecter une régression dans leur construction, pas
	// seulement que "quelque chose" a été renvoyé.
	bin := writeFakeExecutable(t, dir, "fake-chrome", `
case " $* " in
  *" --dump-dom "*) ;;
  *) echo "dump-dom manquant: $*" >&2; exit 1 ;;
esac
case " $* " in
  *" https://example.com/page "*) ;;
  *) echo "url manquante: $*" >&2; exit 1 ;;
esac
case " $* " in
  *" --virtual-time-budget=8000 "*) ;;
  *) echo "virtual-time-budget manquant: $*" >&2; exit 1 ;;
esac
echo "<html><body>contenu rendu</body></html>"
`)

	got, err := fetchWithChrome(context.Background(), bin, "https://example.com/page")
	if err != nil {
		t.Fatalf("fetchWithChrome: %v", err)
	}
	if !strings.Contains(got, "contenu rendu") {
		t.Fatalf("got %q, attendu qu'il contienne la sortie du faux binaire", got)
	}
}

func TestFetchWithChromePropagatesStderrOnFailure(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeExecutable(t, dir, "fake-chrome", `echo "boom" >&2; exit 1`)

	_, err := fetchWithChrome(context.Background(), bin, "https://example.com")
	if err == nil {
		t.Fatal("attendu une erreur")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("erreur = %q, attendu qu'elle contienne le stderr du binaire", err.Error())
	}
}

// fakeGeckodriver simule les seuls endpoints WebDriver dont ce fichier a
// besoin, pour tester newFirefoxSession/navigateFirefox/firefoxPageSource
// sans dépendre d'un vrai geckodriver installé.
func fakeGeckodriver(t *testing.T, source string, navigateFails bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"value":{"ready":true}}`))
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"value": map[string]any{"sessionId": "s1"}})
	})
	mux.HandleFunc("/session/s1/url", func(w http.ResponseWriter, r *http.Request) {
		if navigateFails {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{"value": map[string]any{"error": "unknown error", "message": "navigation impossible"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"value": nil})
	})
	mux.HandleFunc("/session/s1/source", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"value": source})
	})
	mux.HandleFunc("/session/s1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"value": nil})
	})
	return httptest.NewServer(mux)
}

func TestFirefoxWebDriverFlowSuccess(t *testing.T) {
	server := fakeGeckodriver(t, "<html><body>page rendue</body></html>", false)
	defer server.Close()
	client := server.Client()
	base := server.URL

	if err := waitForGeckodriver(context.Background(), client, base); err != nil {
		t.Fatalf("waitForGeckodriver: %v", err)
	}
	sessionID, err := newFirefoxSession(context.Background(), client, base, "fake-firefox-binary")
	if err != nil {
		t.Fatalf("newFirefoxSession: %v", err)
	}
	if sessionID != "s1" {
		t.Fatalf("sessionID = %q, want %q", sessionID, "s1")
	}
	if err := navigateFirefox(context.Background(), client, base, sessionID, "https://example.com"); err != nil {
		t.Fatalf("navigateFirefox: %v", err)
	}
	got, err := firefoxPageSource(context.Background(), client, base, sessionID)
	if err != nil {
		t.Fatalf("firefoxPageSource: %v", err)
	}
	if !strings.Contains(got, "page rendue") {
		t.Fatalf("got %q, attendu le contenu source simulé", got)
	}
}

func TestNavigateFirefoxSurfacesWebDriverError(t *testing.T) {
	server := fakeGeckodriver(t, "", true)
	defer server.Close()

	err := navigateFirefox(context.Background(), server.Client(), server.URL, "s1", "https://example.com")
	if err == nil {
		t.Fatal("attendu une erreur")
	}
	if !strings.Contains(err.Error(), "navigation impossible") {
		t.Fatalf("erreur = %q, attendu qu'elle contienne le message WebDriver", err.Error())
	}
}

// Cas observé en pratique (Cloudflare, starwars.fandom.com) : la page
// rendue est un écran de vérification anti-bot, pas le contenu demandé —
// looksLikeBotChallenge doit le reconnaître pour que Call échoue
// explicitement plutôt que de renvoyer ce texte comme s'il s'agissait de la
// page réelle.
func TestLooksLikeBotChallenge(t *testing.T) {
	challenge := `<html><body>Un instant…Vérification de sécurité en cours. Enable JavaScript and cookies to continue. Ray ID: a3e0638fa898e15e</body></html>`
	if !looksLikeBotChallenge(challenge) {
		t.Fatal("attendu que cette page de vérification Cloudflare soit reconnue")
	}

	real := `<html><body><h1>Protocol Droid</h1><p>A protocol droid is a droid designed for translation and etiquette.</p></body></html>`
	if looksLikeBotChallenge(real) {
		t.Fatal("une page de contenu normal ne doit pas être prise pour une vérification anti-bot")
	}
}

// Bout en bout : Call doit échouer explicitement sur une page de
// vérification anti-bot plutôt que de renvoyer ce texte comme si c'était le
// contenu réel de la page demandée.
func TestBrowserFetchFailsExplicitlyOnBotChallenge(t *testing.T) {
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "chromium", `echo "<html><body>Vérification de sécurité en cours. Checking your browser before accessing the site.</body></html>"`)
	t.Setenv("PATH", dir)
	withIsolatedPlaywrightCache(t)

	tool := &BrowserFetchTool{}
	_, err := tool.Call(context.Background(), `{"url":"https://example.com"}`)
	if err == nil {
		t.Fatal("attendu une erreur pour une page de vérification anti-bot")
	}
	if !strings.Contains(err.Error(), "anti-bot") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne la vérification anti-bot", err.Error())
	}
}

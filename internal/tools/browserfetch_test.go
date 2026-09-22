package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestBrowserFetchRejectsInvalidFormat(t *testing.T) {
	tool := &BrowserFetchTool{}
	_, err := tool.Call(context.Background(), `{"url":"https://example.com","format":"markdown"}`)
	if err == nil {
		t.Fatal(`attendu une erreur pour format="markdown"`)
	}
	if !strings.Contains(err.Error(), "format") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne le paramètre format", err.Error())
	}
}

// Bout en bout, avec un vrai Chrome (voir realChromeBinaryOrSkip) :
// format:"html" doit renvoyer le DOM brut (balises et attributs compris),
// pas le texte nettoyé — nécessaire pour extraire une URL logée dans un
// attribut (ex: <img src="...">, perdu par la conversion en texte).
func TestBrowserFetchHTMLFormatReturnsRawDOM(t *testing.T) {
	realChromeBinaryOrSkip(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><p>texte visible</p><img src="https://example.com/photo.jpg"></body></html>`)
	}))
	defer server.Close()

	tool := &BrowserFetchTool{}

	textOut, err := tool.Call(context.Background(), fmt.Sprintf(`{"url":%q}`, server.URL))
	if err != nil {
		t.Fatalf("Call (text): %v", err)
	}
	if strings.Contains(textOut, "photo.jpg") {
		t.Fatalf("format texte (défaut) = %q, ne devrait PAS contenir l'URL de l'attribut src", textOut)
	}
	if !strings.Contains(textOut, "texte visible") {
		t.Fatalf("format texte (défaut) = %q, attendu qu'il contienne le texte visible", textOut)
	}

	htmlOut, err := tool.Call(context.Background(), fmt.Sprintf(`{"url":%q,"format":"html"}`, server.URL))
	if err != nil {
		t.Fatalf("Call (html): %v", err)
	}
	if !strings.Contains(htmlOut, `src="https://example.com/photo.jpg"`) {
		t.Fatalf("format html = %q, attendu qu'il contienne l'attribut src brut", htmlOut)
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

// realChromeBinaryOrSkip retourne un binaire Chromium/Chrome utilisable SUR
// LA VRAIE MACHINE qui exécute les tests (PATH/HOME réels, pas isolés comme
// dans le reste de ce fichier), ou passe le test si aucun n'est disponible.
// Utilisé uniquement pour les tests qui pilotent réellement fetchWithChromeCDP
// de bout en bout (voir son commentaire) : contrairement à l'ancienne
// implémentation --dump-dom, remplacer Chrome par un faux exécutable n'est
// plus possible ici (il faudrait aussi simuler tout son port de debug CDP en
// HTTP+WebSocket) — un vrai Chrome/Chromium est donc requis pour ces tests
// précis, qui passent silencieusement (pas d'échec CI) quand aucun n'est
// installé.
func realChromeBinaryOrSkip(t *testing.T) string {
	t.Helper()
	bin := findChromeBinary()
	if bin == "" {
		t.Skip("aucun Chromium/Chrome réel disponible (PATH ou cache Playwright) : test ignoré")
	}
	return bin
}

// Bout en bout, avec un vrai Chrome : Call doit échouer explicitement sur
// une page de vérification anti-bot plutôt que de renvoyer ce texte comme
// si c'était le contenu réel de la page demandée.
func TestBrowserFetchFailsExplicitlyOnBotChallenge(t *testing.T) {
	realChromeBinaryOrSkip(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>Vérification de sécurité en cours. Checking your browser before accessing the site.</body></html>`)
	}))
	defer server.Close()

	tool := &BrowserFetchTool{}
	_, err := tool.Call(context.Background(), fmt.Sprintf(`{"url":%q}`, server.URL))
	if err == nil {
		t.Fatal("attendu une erreur pour une page de vérification anti-bot")
	}
	if !strings.Contains(err.Error(), "anti-bot") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne la vérification anti-bot", err.Error())
	}
}

func TestIsMediaContentType(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"image/png", true},
		{"image/jpeg; charset=binary", true},
		{"video/mp4", true},
		{"audio/mpeg", true},
		{"application/pdf", true},
		{"text/html", false},
		{"text/html; charset=utf-8", false},
		{"application/json", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isMediaContentType(c.ct); got != c.want {
			t.Errorf("isMediaContentType(%q) = %v, attendu %v", c.ct, got, c.want)
		}
	}
}

// mediaFileName doit reprendre le nom de base de l'URL (reconnaissable),
// avec une extension déduite du Content-Type plutôt que de celle
// (éventuellement absente ou trompeuse) présente dans l'URL elle-même.
func TestMediaFileNameUsesContentTypeExtension(t *testing.T) {
	u, err := url.Parse("https://example.com/photos/vacances.jpeg?resize=800")
	if err != nil {
		t.Fatal(err)
	}
	got := mediaFileName(u, "image/jpeg")
	if !strings.HasPrefix(got, "vacances-") || !strings.HasSuffix(got, ".jpg") {
		t.Fatalf("mediaFileName = %q, attendu un nom du style vacances-<horodatage>.jpg", got)
	}
}

// Sans nom de fichier exploitable dans l'URL (ou un Content-Type inconnu),
// mediaFileName doit quand même produire un nom de fichier valide plutôt
// que planter ou produire une chaîne vide.
func TestMediaFileNameFallsBackWhenURLHasNoUsefulName(t *testing.T) {
	u, err := url.Parse("https://example.com/img?id=42")
	if err != nil {
		t.Fatal(err)
	}
	got := mediaFileName(u, "application/octet-stream")
	if !strings.HasPrefix(got, "img-") || !strings.HasSuffix(got, ".bin") {
		t.Fatalf("mediaFileName = %q, attendu un nom du style img-<horodatage>.bin", got)
	}
}

// saveMediaBytes écrit exactement les octets fournis, avec un nom dérivé de
// l'URL/du Content-Type (voir mediaFileName) — pas de requête réseau
// impliquée (les octets sont déjà en main, voir son commentaire).
func TestSaveMediaBytesWritesExactContent(t *testing.T) {
	u, err := url.Parse("https://example.com/cover.png")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("contenu-image-simule")

	path, err := saveMediaBytes(u, body, "image/png")
	if err != nil {
		t.Fatalf("saveMediaBytes: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("lecture de %q: %v", path, err)
	}
	if string(got) != string(body) {
		t.Fatalf("contenu = %q, attendu %q", got, body)
	}
	if !strings.HasSuffix(path, ".png") {
		t.Fatalf("chemin = %q, attendu une extension .png", path)
	}
}

// Bout en bout, avec un vrai Chrome (voir realChromeBinaryOrSkip) : une URL
// qui répond avec un Content-Type image doit être enregistrée telle quelle
// via Network.getResponseBody (voir fetchWithChromeCDP), sans qu'aucune
// requête HTTP supplémentaire ne soit faite pour l'obtenir — le fichier
// enregistré doit contenir exactement les octets servis.
func TestBrowserFetchCallDownloadsDetectedMedia(t *testing.T) {
	realChromeBinaryOrSkip(t)

	imageBytes := []byte("contenu-image-simule")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(imageBytes)
	}))
	defer server.Close()
	target := server.URL + "/cover.png"

	tool := &BrowserFetchTool{}
	out, err := tool.Call(context.Background(), fmt.Sprintf(`{"url":%q}`, target))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "image/png") {
		t.Fatalf("sortie = %q, attendu qu'elle mentionne le Content-Type détecté", out)
	}

	idx := strings.Index(out, mediaDownloadDir())
	if idx < 0 {
		t.Fatalf("sortie = %q, attendu qu'elle contienne un chemin sous %q", out, mediaDownloadDir())
	}
	filePath := strings.TrimSpace(out[idx:])
	t.Cleanup(func() { os.Remove(filePath) })

	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("lecture du fichier téléchargé (%q): %v", filePath, err)
	}
	if string(got) != string(imageBytes) {
		t.Fatalf("contenu du fichier téléchargé = %q, attendu %q", got, imageBytes)
	}
}

// Un média détecté mais dépassant MaxMediaBytes doit échouer explicitement
// (message clair sur la taille), pas se rabattre silencieusement sur un
// rendu en texte qui n'aurait de toute façon aucun sens pour un binaire déjà
// identifié comme tel.
func TestBrowserFetchCallFailsExplicitlyOnOversizedMedia(t *testing.T) {
	realChromeBinaryOrSkip(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "100")
		w.Write(make([]byte, 100))
	}))
	defer server.Close()
	target := server.URL + "/cover.png"

	tool := &BrowserFetchTool{MaxMediaBytes: 10}
	_, err := tool.Call(context.Background(), fmt.Sprintf(`{"url":%q}`, target))
	if err == nil {
		t.Fatal("attendu une erreur (média trop volumineux)")
	}
	if !strings.Contains(err.Error(), "volumineux") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne la taille excessive", err.Error())
	}
}

package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cdpMessage : forme générique d'un message JSON du protocole DevTools
// (CDP) — soit la réponse à une commande envoyée (ID + Result/Error), soit
// un événement (Method + Params) : les deux arrivent entrelacés sur la même
// connexion WebSocket, sans ordre garanti entre eux.
type cdpMessage struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// cdpSession pilote une session CDP le temps d'une navigation : envoi de
// commandes (chacune avec un id croissant) et lecture séquentielle des
// messages qui arrivent.
type cdpSession struct {
	ws     *wsConn
	nextID int
}

func newCDPSession(ws *wsConn) *cdpSession { return &cdpSession{ws: ws} }

func (s *cdpSession) send(method string, params any) (int, error) {
	s.nextID++
	id := s.nextID
	payload, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return 0, err
	}
	if err := s.ws.WriteText(payload); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *cdpSession) next() (cdpMessage, error) {
	raw, err := s.ws.ReadMessage()
	if err != nil {
		return cdpMessage{}, err
	}
	var msg cdpMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return cdpMessage{}, fmt.Errorf("message CDP invalide: %w", err)
	}
	return msg, nil
}

// call envoie method/params et attend SPÉCIFIQUEMENT sa réponse (même id),
// en relayant à onEvent tout événement reçu entre-temps (voir le
// commentaire de cdpMessage sur l'entrelacement) — onEvent ne doit jamais
// être nil ; utiliser une closure no-op si les événements reçus pendant cet
// appel précis n'ont pas besoin d'être traités.
func (s *cdpSession) call(method string, params any, onEvent func(cdpMessage)) (json.RawMessage, error) {
	id, err := s.send(method, params)
	if err != nil {
		return nil, err
	}
	for {
		msg, err := s.next()
		if err != nil {
			return nil, err
		}
		if msg.ID == id {
			if msg.Error != nil {
				return nil, fmt.Errorf("%s: %s", method, msg.Error.Message)
			}
			return msg.Result, nil
		}
		if msg.Method != "" {
			onEvent(msg)
		}
	}
}

// browserFetchResult décrit ce qu'une navigation pilotée par CDP (voir
// fetchWithChromeCDP) a produit : soit du HTML rendu (page normale), soit
// un média identifié via le Content-Type réel de la réponse réseau
// (mediaBody/mediaContentType) — jamais les deux à la fois.
type browserFetchResult struct {
	html             string
	mediaBody        []byte
	mediaContentType string
}

// cdpTarget représente une entrée de la liste JSON exposée par le port de
// debug HTTP de Chrome (voir chromeDebugTargets).
type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// fetchWithChromeCDP pilote Chrome via le protocole DevTools (CDP, en
// WebSocket — voir wsConn) plutôt que via `--dump-dom` (voir fetchWithChrome,
// utilisé jusqu'ici) : seule cette voie donne accès au CONTENU RÉEL d'un
// fichier média (image, vidéo, audio, PDF) — sa réponse réseau exacte, via
// Network.getResponseBody — plutôt qu'à son rendu HTML.
//
// Nécessaire empiriquement : Chrome affiche ces contenus INLINE lors d'une
// navigation directe (testé avec Browser.setDownloadBehavior, y compris en
// behavior "allow" — aucun événement Page.downloadWillBegin ne se déclenche
// pour une image/vidéo/audio/PDF, seulement pour un contenu qu'il ne sait
// PAS afficher lui-même). Il n'existe donc pas de "téléchargement" au sens
// propre à intercepter pour ces types : la seule façon d'obtenir les octets
// exacts que le navigateur a reçus, sans repasser par une requête HTTP
// séparée, est de les lire directement depuis sa pile réseau via CDP —
// l'équivalent, pour un contexte headless sans aucune interface (donc sans
// "Ctrl+S" ni boîte de dialogue possibles), de ce qu'un "Enregistrer sous"
// ferait dans un navigateur avec interface : récupérer ce que le navigateur
// a déjà lui-même obtenu, jamais le redemander de son propre chef.
//
// Client WebSocket écrit à la main (voir websocket.go) plutôt que via une
// dépendance Go existante : cohérent avec le reste du fichier (fetchWithFirefox
// est, de la même façon, sans dépendance externe au-delà de la bibliothèque
// standard).
func fetchWithChromeCDP(ctx context.Context, bin, target string, maxMediaBytes int) (browserFetchResult, error) {
	port, err := freeLocalPort()
	if err != nil {
		return browserFetchResult{}, fmt.Errorf("recherche d'un port libre pour le debug Chrome: %w", err)
	}

	// --user-data-dir dédié et jetable : requis pour que --remote-debugging-port
	// fonctionne de façon fiable (Chrome le refuse silencieusement avec le
	// profil par défaut dans certaines versions récentes, pour des raisons de
	// sécurité — un profil existant pourrait être partagé avec une session
	// utilisateur réelle), et évite au passage tout conflit avec une autre
	// invocation concurrente de ce même tool.
	userDataDir, err := os.MkdirTemp("", "bot-chrome-profile-")
	if err != nil {
		return browserFetchResult{}, fmt.Errorf("création du profil Chrome temporaire: %w", err)
	}
	defer os.RemoveAll(userDataDir)

	cmd := exec.CommandContext(ctx, bin,
		"--headless=new",
		"--disable-gpu",
		// Le sandbox interne de Chrome nécessite des primitives noyau (user
		// namespaces...) pas toujours disponibles selon l'environnement dans
		// lequel le harnais tourne : sans ce drapeau, Chrome refuse purement
		// et simplement de démarrer dans un tel environnement. Compromis
		// assumé, cohérent avec l'absence de protection SSRF déjà documentée
		// pour ce tool : le risque visé (JS d'une page web arbitraire) est
		// le même que dans un navigateur normal, pas un contenu local
		// sensible.
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--user-agent="+chromeUserAgent,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+userDataDir,
		"about:blank",
	)
	// Nouveau groupe de processus (comme ShellTool, voir son commentaire sur
	// Setsid) : Chrome démarre plusieurs sous-processus (zygote, GPU,
	// utilitaire réseau...) qui ne sont pas nécessairement tués avec lui —
	// observé empiriquement : sans ça, ces sous-processus restent parfois en
	// vie assez longtemps après le Kill() ci-dessous pour empêcher
	// RemoveAll(userDataDir) de vraiment vider le dossier (fichiers recréés
	// entre le listing et la suppression), laissant une coquille vide
	// s'accumuler dans /tmp à chaque appel. Tuer le groupe entier plutôt que
	// le seul PID de tête évite ça.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return browserFetchResult{}, fmt.Errorf("démarrage de %s: %w", bin, err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = cmd.Wait()
	}()

	httpClient := &http.Client{}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitForChromeDebugPort(ctx, httpClient, base); err != nil {
		return browserFetchResult{}, fmt.Errorf("port de debug Chrome (%s) indisponible: %w", base, err)
	}

	targetID, wsURL, err := chromePageTarget(ctx, httpClient, base)
	if err != nil {
		return browserFetchResult{}, err
	}

	ws, err := dialWebSocket(ctx, wsURL)
	if err != nil {
		return browserFetchResult{}, fmt.Errorf("connexion WebSocket à Chrome (%s): %w", wsURL, err)
	}
	defer ws.Close()

	// Budget large pour couvrir à la fois le chargement réseau et le rendu
	// JS différé (voir plus bas) — browserRenderBudget seul suffit pour le
	// second, mais pas nécessairement pour une page lente à charger.
	deadline := time.Now().Add(browserRenderBudget + 20*time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := ws.conn.SetDeadline(deadline); err != nil {
		return browserFetchResult{}, err
	}

	return driveChromeNavigation(newCDPSession(ws), targetID, target, maxMediaBytes)
}

// waitForChromeDebugPort patiente jusqu'à ce que le port de debug distant de
// Chrome réponde sur son endpoint /json/version (démarrage du processus, pas
// instantané), ou que ctx expire — même logique que waitForGeckodriver.
func waitForChromeDebugPort(ctx context.Context, client *http.Client, base string) error {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/version", nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// chromePageTarget retourne l'id et l'URL WebSocket de l'unique cible de
// type "page" listée par le port de debug de Chrome — la page "about:blank"
// avec laquelle il a été lancé (voir fetchWithChromeCDP). Les autres entrées
// que Chrome liste par défaut (service workers, popups internes
// chrome://...) sont d'un autre type, jamais "page" : aucune ambiguïté
// attendue (vérifié empiriquement).
func chromePageTarget(ctx context.Context, client *http.Client, base string) (id, wsURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/list", nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("liste des cibles Chrome: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	var targets []cdpTarget
	if err := json.Unmarshal(data, &targets); err != nil {
		return "", "", fmt.Errorf("réponse de liste des cibles Chrome invalide: %w", err)
	}
	for _, t := range targets {
		if t.Type == "page" {
			return t.ID, t.WebSocketDebuggerURL, nil
		}
	}
	return "", "", fmt.Errorf("aucune cible de type \"page\" trouvée parmi %d cible(s)", len(targets))
}

// driveChromeNavigation navigue vers target dans la page identifiée par
// targetID (voir chromePageTarget) et retourne soit son HTML rendu, soit le
// média détecté (voir browserFetchResult) — decidé uniquement à partir du
// Content-Type RÉEL de la réponse réseau de la navigation elle-même
// (Network.responseReceived), jamais deviné.
//
// targetID sert directement de frameId attendu (vérifié empiriquement : le
// frameId du cadre principal d'une cible Chrome, tel que rapporté par les
// événements Page/Network, est exactement l'id de cette cible listée via
// /json/list) — plutôt que d'attendre la réponse de Page.navigate pour
// l'apprendre, ce qui laisserait une fenêtre où un événement arrivé avant
// cette réponse (l'ordre entre eux n'est pas garanti, voir cdpMessage)
// serait ignoré à tort faute de savoir encore à quel frameId le rattacher.
func driveChromeNavigation(sess *cdpSession, targetID, target string, maxMediaBytes int) (browserFetchResult, error) {
	if maxMediaBytes <= 0 {
		maxMediaBytes = 25 << 20 // 25 Mio
	}

	noop := func(cdpMessage) {}
	if _, err := sess.call("Network.enable", map[string]any{}, noop); err != nil {
		return browserFetchResult{}, fmt.Errorf("Network.enable: %w", err)
	}
	if _, err := sess.call("Page.enable", map[string]any{}, noop); err != nil {
		return browserFetchResult{}, fmt.Errorf("Page.enable: %w", err)
	}

	var (
		docRequestID     string
		docMimeType      string
		docContentLength int64 = -1
		docReady         bool
		pageLoaded       bool
	)
	onEvent := func(msg cdpMessage) {
		switch msg.Method {
		case "Network.responseReceived":
			var p struct {
				FrameID   string `json:"frameId"`
				Type      string `json:"type"`
				RequestID string `json:"requestId"`
				Response  struct {
					MimeType string            `json:"mimeType"`
					Headers  map[string]string `json:"headers"`
				} `json:"response"`
			}
			if json.Unmarshal(msg.Params, &p) == nil && p.FrameID == targetID && p.Type == "Document" {
				// Reprend à zéro à chaque nouvelle réponse "Document" pour ce
				// cadre (ex: une redirection produit plusieurs de ces
				// événements successifs pour le même requestId, avant la
				// réponse finale) : docReady, lui, n'est (re)mis à true que
				// par Network.loadingFinished pour CE requestId précis.
				docRequestID = p.RequestID
				docMimeType = p.Response.MimeType
				docContentLength = headerContentLength(p.Response.Headers)
				docReady = false
			}
		case "Network.loadingFinished":
			var p struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(msg.Params, &p) == nil && docRequestID != "" && p.RequestID == docRequestID {
				docReady = true
			}
		case "Page.loadEventFired":
			pageLoaded = true
		}
	}

	navRaw, err := sess.call("Page.navigate", map[string]any{"url": target}, onEvent)
	if err != nil {
		return browserFetchResult{}, fmt.Errorf("Page.navigate: %w", err)
	}
	var navResult struct {
		ErrorText string `json:"errorText"`
	}
	if err := json.Unmarshal(navRaw, &navResult); err != nil {
		return browserFetchResult{}, fmt.Errorf("réponse Page.navigate invalide: %w", err)
	}
	if navResult.ErrorText != "" {
		return browserFetchResult{}, fmt.Errorf("navigation échouée: %s", navResult.ErrorText)
	}

	for !docReady {
		msg, err := sess.next()
		if err != nil {
			return browserFetchResult{}, fmt.Errorf("attente de la réponse réseau de %q: %w", target, err)
		}
		if msg.Method != "" {
			onEvent(msg)
		}
	}

	if isMediaContentType(docMimeType) {
		// Rejeté avant même d'appeler Network.getResponseBody quand la taille
		// est déjà connue via Content-Length : contrairement à une requête
		// HTTP normale (io.LimitReader), CDP ne permet pas d'interrompre en
		// cours de route un corps déjà entièrement reçu par le navigateur —
		// getResponseBody le renvoie toujours intégralement d'un coup. Autant
		// éviter de le rapatrier pour rien quand on sait déjà qu'il est trop
		// gros.
		if docContentLength > int64(maxMediaBytes) {
			return browserFetchResult{}, fmt.Errorf("média (%s) trop volumineux (%d octets > %d), non téléchargé", docMimeType, docContentLength, maxMediaBytes)
		}

		bodyRaw, err := sess.call("Network.getResponseBody", map[string]any{"requestId": docRequestID}, onEvent)
		if err != nil {
			return browserFetchResult{}, fmt.Errorf("Network.getResponseBody: %w", err)
		}
		var body struct {
			Body          string `json:"body"`
			Base64Encoded bool   `json:"base64Encoded"`
		}
		if err := json.Unmarshal(bodyRaw, &body); err != nil {
			return browserFetchResult{}, fmt.Errorf("réponse Network.getResponseBody invalide: %w", err)
		}
		mediaBody := []byte(body.Body)
		if body.Base64Encoded {
			mediaBody, err = base64.StdEncoding.DecodeString(body.Body)
			if err != nil {
				return browserFetchResult{}, fmt.Errorf("décodage du corps du média (base64): %w", err)
			}
		}
		// Garde-fou pour le cas où Content-Length était absent ou mensonger
		// (ex: réponse en chunked-encoding) : docContentLength ci-dessus ne
		// couvre alors rien.
		if len(mediaBody) > maxMediaBytes {
			return browserFetchResult{}, fmt.Errorf("média (%s) trop volumineux (%d octets > %d), non téléchargé", docMimeType, len(mediaBody), maxMediaBytes)
		}
		return browserFetchResult{mediaBody: mediaBody, mediaContentType: docMimeType}, nil
	}

	for !pageLoaded {
		msg, err := sess.next()
		if err != nil {
			return browserFetchResult{}, fmt.Errorf("attente du chargement de la page %q: %w", target, err)
		}
		if msg.Method != "" {
			onEvent(msg)
		}
	}
	// Laisse le JavaScript différé de la page (redirection JS, contenu
	// chargé en différé...) tourner avant de capturer le DOM — équivalent
	// du --virtual-time-budget de l'ancienne implémentation CLI (voir
	// fetchWithChrome), qui n'a pas d'équivalent direct exposé par CDP.
	time.Sleep(browserRenderBudget)

	evalRaw, err := sess.call("Runtime.evaluate", map[string]any{
		"expression":    "document.documentElement.outerHTML",
		"returnByValue": true,
	}, onEvent)
	if err != nil {
		return browserFetchResult{}, fmt.Errorf("Runtime.evaluate: %w", err)
	}
	var evalResult struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(evalRaw, &evalResult); err != nil {
		return browserFetchResult{}, fmt.Errorf("réponse Runtime.evaluate invalide: %w", err)
	}
	if evalResult.ExceptionDetails != nil {
		return browserFetchResult{}, fmt.Errorf("Runtime.evaluate: %s", evalResult.ExceptionDetails.Text)
	}
	return browserFetchResult{html: evalResult.Result.Value}, nil
}

// headerContentLength cherche un en-tête "Content-Length" dans headers
// (recherche insensible à la casse : HTTP/2, notamment, les transmet en
// minuscules) et retourne sa valeur, ou -1 si absent ou invalide — jamais 0,
// pour ne pas confondre "absent" avec "un média de taille nulle" dans la
// comparaison de driveChromeNavigation (docContentLength > maxMediaBytes
// serait toujours fausse pour -1 comme pour 0, mais -1 documente mieux
// l'intention pour un futur appelant).
func headerContentLength(headers map[string]string) int64 {
	for name, value := range headers {
		if strings.EqualFold(name, "Content-Length") {
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				return n
			}
		}
	}
	return -1
}

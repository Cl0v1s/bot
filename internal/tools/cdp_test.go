package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// dialFakeCDPSession ouvre une session cdpSession vers un fakeWSServer déjà
// accepté (voir websocket_test.go) — réutilisé par tous les tests de ce
// fichier pour ne pas dupliquer la poignée de main WebSocket.
func dialFakeCDPSession(t *testing.T) (*cdpSession, *fakeWSServer) {
	t.Helper()
	addr, accept := newFakeWSServer(t)

	dialDone := make(chan *wsConn, 1)
	go func() {
		ws, err := dialWebSocket(context.Background(), "ws://"+addr+"/")
		if err != nil {
			t.Errorf("dialWebSocket: %v", err)
			dialDone <- nil
			return
		}
		dialDone <- ws
	}()
	server := accept()
	t.Cleanup(server.Close)
	ws := <-dialDone
	if ws == nil {
		t.Fatal("dialWebSocket a échoué")
	}
	t.Cleanup(func() { ws.Close() })

	return newCDPSession(ws), server
}

// call envoie une commande et attend sa réponse : doit ignorer tout
// événement (message avec "method" mais sans "id" correspondant) reçu entre
// temps, et ne retourner qu'au message dont l'id correspond réellement —
// pas au premier message qui arrive, quel qu'il soit.
func TestCDPSessionCallIgnoresInterleavedEvents(t *testing.T) {
	sess, server := dialFakeCDPSession(t)

	var gotEvents []string
	done := make(chan struct{})
	var callErr error
	go func() {
		_, callErr = sess.call("Page.navigate", map[string]any{"url": "http://example.com"}, func(msg cdpMessage) {
			gotEvents = append(gotEvents, msg.Method)
		})
		close(done)
	}()

	// La requête envoyée par sess.call doit être lisible côté serveur avant
	// que celui-ci ne réponde quoi que ce soit.
	raw := server.readClientFrame()
	var req struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("décodage de la commande envoyée: %v", err)
	}
	if req.Method != "Page.navigate" {
		t.Fatalf("method = %q, attendu Page.navigate", req.Method)
	}

	// Deux événements arrivent AVANT la réponse à la commande : call ne doit
	// pas s'arrêter dessus.
	server.writeTextFrame([]byte(`{"method":"Page.frameStartedLoading","params":{}}`))
	server.writeTextFrame([]byte(`{"method":"Network.responseReceived","params":{}}`))
	server.writeTextFrame([]byte(`{"id":` + strconv.Itoa(req.ID) + `,"result":{"frameId":"f1"}}`))

	<-done
	if callErr != nil {
		t.Fatalf("call: %v", callErr)
	}
	if len(gotEvents) != 2 || gotEvents[0] != "Page.frameStartedLoading" || gotEvents[1] != "Network.responseReceived" {
		t.Fatalf("événements relayés = %v, attendu les deux événements interlacés avant la réponse", gotEvents)
	}
}

// Une réponse CDP portant un champ "error" doit se traduire par une erreur
// Go explicite, pas un résultat vide silencieusement accepté.
func TestCDPSessionCallReturnsErrorFromCDPErrorResponse(t *testing.T) {
	sess, server := dialFakeCDPSession(t)

	done := make(chan struct{})
	var callErr error
	go func() {
		_, callErr = sess.call("Network.getResponseBody", map[string]any{"requestId": "x"}, func(cdpMessage) {})
		close(done)
	}()

	raw := server.readClientFrame()
	var req struct {
		ID int `json:"id"`
	}
	json.Unmarshal(raw, &req)

	server.writeTextFrame([]byte(`{"id":` + strconv.Itoa(req.ID) + `,"error":{"code":-32000,"message":"No resource with given identifier found"}}`))

	<-done
	if callErr == nil {
		t.Fatal("attendu une erreur")
	}
	if got := callErr.Error(); !strings.Contains(got, "No resource with given identifier found") {
		t.Fatalf("erreur = %q, attendu qu'elle contienne le message CDP", got)
	}
}

func TestHeaderContentLength(t *testing.T) {
	cases := []struct {
		headers map[string]string
		want    int64
	}{
		{map[string]string{"Content-Length": "1234"}, 1234},
		{map[string]string{"content-length": "42"}, 42}, // HTTP/2 : en-têtes en minuscules
		{map[string]string{"Content-Type": "image/png"}, -1},
		{nil, -1},
		{map[string]string{"Content-Length": "pas-un-nombre"}, -1},
	}
	for _, c := range cases {
		if got := headerContentLength(c.headers); got != c.want {
			t.Errorf("headerContentLength(%v) = %d, attendu %d", c.headers, got, c.want)
		}
	}
}

// chromePageTarget doit choisir l'entrée de type "page" parmi plusieurs
// (service workers, popups internes...) — jamais la première entrée listée
// sans en vérifier le type.
func TestChromePageTargetPicksPageType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]cdpTarget{
			{ID: "sw1", Type: "service_worker", WebSocketDebuggerURL: "ws://x/sw1"},
			{ID: "page1", Type: "page", WebSocketDebuggerURL: "ws://x/page1"},
			{ID: "popup1", Type: "browser_ui", WebSocketDebuggerURL: "ws://x/popup1"},
		})
	}))
	defer server.Close()

	id, wsURL, err := chromePageTarget(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatalf("chromePageTarget: %v", err)
	}
	if id != "page1" || wsURL != "ws://x/page1" {
		t.Fatalf("id=%q wsURL=%q, attendu id=page1 wsURL=ws://x/page1", id, wsURL)
	}
}

func TestChromePageTargetErrorsWhenNoPageType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]cdpTarget{
			{ID: "sw1", Type: "service_worker", WebSocketDebuggerURL: "ws://x/sw1"},
		})
	}))
	defer server.Close()

	if _, _, err := chromePageTarget(context.Background(), server.Client(), server.URL); err == nil {
		t.Fatal("attendu une erreur quand aucune cible de type \"page\" n'existe")
	}
}

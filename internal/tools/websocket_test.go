package tools

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// fakeWSServer accepte une unique connexion WebSocket entrante (poignée de
// main RFC 6455 côté serveur faite à la main, sans dépendance externe — même
// esprit que wsConn côté client), et expose readFrame/writeFrame pour que le
// test pilote l'échange dans les deux sens. Utilisé pour tester dialWebSocket
// et wsConn.{WriteText,ReadMessage} sans dépendre d'un vrai serveur
// WebSocket (Chrome ou autre).
type fakeWSServer struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func newFakeWSServer(t *testing.T) (addr string, accept func() *fakeWSServer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("écoute TCP: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	return ln.Addr().String(), func() *fakeWSServer {
		conn, err := ln.Accept()
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		s := &fakeWSServer{t: t, conn: conn, br: bufio.NewReader(conn)}
		s.handshake()
		return s
	}
}

func (s *fakeWSServer) handshake() {
	s.t.Helper()
	tp := textproto.NewReader(s.br)
	if _, err := tp.ReadLine(); err != nil {
		s.t.Fatalf("lecture de la ligne de requête: %v", err)
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		s.t.Fatalf("lecture des en-têtes: %v", err)
	}
	key := hdr.Get("Sec-Websocket-Key")
	if key == "" {
		s.t.Fatal("Sec-WebSocket-Key absent de la requête d'upgrade")
	}
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := s.conn.Write([]byte(resp)); err != nil {
		s.t.Fatalf("envoi de la réponse d'upgrade: %v", err)
	}
}

// writeTextFrame écrit payload comme un unique frame texte NON masqué —
// conforme à RFC 6455 §5.1 (un serveur ne masque jamais).
func (s *fakeWSServer) writeTextFrame(payload []byte) {
	s.t.Helper()
	header := []byte{0x81} // FIN + opcode texte
	n := len(payload)
	switch {
	case n <= 125:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126)
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header = append(header, 127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	if _, err := s.conn.Write(header); err != nil {
		s.t.Fatalf("écriture de l'en-tête de frame: %v", err)
	}
	if _, err := s.conn.Write(payload); err != nil {
		s.t.Fatalf("écriture du payload de frame: %v", err)
	}
}

// readClientFrame lit un frame envoyé par le client et retourne son payload
// démasqué (un client DOIT masquer, voir RFC 6455 §5.3 — vérifié ici).
func (s *fakeWSServer) readClientFrame() []byte {
	s.t.Helper()
	head := make([]byte, 2)
	if _, err := readFull(s.br, head); err != nil {
		s.t.Fatalf("lecture de l'en-tête de frame client: %v", err)
	}
	masked := head[1]&0x80 != 0
	if !masked {
		s.t.Fatal("frame client non masqué (violation RFC 6455 §5.3)")
	}
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		ext := make([]byte, 2)
		readFull(s.br, ext)
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		readFull(s.br, ext)
		length = binary.BigEndian.Uint64(ext)
	}
	var maskKey [4]byte
	readFull(s.br, maskKey[:])
	payload := make([]byte, length)
	readFull(s.br, payload)
	for i := range payload {
		payload[i] ^= maskKey[i%4]
	}
	return payload
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (s *fakeWSServer) Close() { s.conn.Close() }

func TestDialWebSocketPerformsValidHandshake(t *testing.T) {
	addr, accept := newFakeWSServer(t)

	done := make(chan *wsConn, 1)
	go func() {
		ws, err := dialWebSocket(context.Background(), "ws://"+addr+"/devtools/page/abc")
		if err != nil {
			t.Errorf("dialWebSocket: %v", err)
			done <- nil
			return
		}
		done <- ws
	}()

	server := accept()
	defer server.Close()

	ws := <-done
	if ws == nil {
		t.Fatal("dialWebSocket a échoué (voir l'erreur ci-dessus)")
	}
	defer ws.Close()
}

func TestWebSocketWriteTextIsMaskedAndReceivedIntact(t *testing.T) {
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
	defer server.Close()
	ws := <-dialDone
	if ws == nil {
		t.Fatal("dialWebSocket a échoué")
	}
	defer ws.Close()

	payload := []byte(`{"id":1,"method":"Page.enable","params":{}}`)
	if err := ws.WriteText(payload); err != nil {
		t.Fatalf("WriteText: %v", err)
	}

	got := server.readClientFrame()
	if string(got) != string(payload) {
		t.Fatalf("frame reçu par le serveur = %q, attendu %q", got, payload)
	}
}

func TestWebSocketReadMessageDecodesServerFrame(t *testing.T) {
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
	defer server.Close()
	ws := <-dialDone
	if ws == nil {
		t.Fatal("dialWebSocket a échoué")
	}
	defer ws.Close()

	want := []byte(`{"id":1,"result":{}}`)
	server.writeTextFrame(want)

	got, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("message reçu = %q, attendu %q", got, want)
	}
}

// Un message dont le payload dépasse 125 octets doit passer par l'encodage
// de longueur étendue (126, sur 2 octets) — vérifié dans les deux sens.
func TestWebSocketHandlesExtendedLength(t *testing.T) {
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
	defer server.Close()
	ws := <-dialDone
	if ws == nil {
		t.Fatal("dialWebSocket a échoué")
	}
	defer ws.Close()

	big := []byte(strings.Repeat("x", 5000))

	if err := ws.WriteText(big); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	got := server.readClientFrame()
	if len(got) != len(big) || string(got) != string(big) {
		t.Fatalf("frame reçu par le serveur: longueur %d, attendu %d", len(got), len(big))
	}

	server.writeTextFrame(big)
	gotMsg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(gotMsg) != string(big) {
		t.Fatalf("message reçu: longueur %d, attendu %d", len(gotMsg), len(big))
	}
}

// Un message fragmenté sur plusieurs frames (continuation, FIN=0 puis FIN=1)
// doit être réassemblé intégralement par ReadMessage.
func TestWebSocketReassemblesFragmentedMessage(t *testing.T) {
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
	defer server.Close()
	ws := <-dialDone
	if ws == nil {
		t.Fatal("dialWebSocket a échoué")
	}
	defer ws.Close()

	// Frame 1 : opcode texte, FIN=0. Frame 2 : opcode continuation, FIN=1.
	part1 := []byte(`{"id":1,`)
	part2 := []byte(`"result":{}}`)

	server.conn.Write([]byte{0x01, byte(len(part1))}) // FIN=0, opcode=texte
	server.conn.Write(part1)
	server.conn.Write([]byte{0x80, byte(len(part2))}) // FIN=1, opcode=continuation
	server.conn.Write(part2)

	got, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	want := string(part1) + string(part2)
	if string(got) != want {
		t.Fatalf("message réassemblé = %q, attendu %q", got, want)
	}
}

// Un frame close doit se traduire par une erreur (io.EOF), pas un blocage
// indéfini ni un message vide silencieusement accepté.
func TestWebSocketCloseFrameEndsRead(t *testing.T) {
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
	defer server.Close()
	ws := <-dialDone
	if ws == nil {
		t.Fatal("dialWebSocket a échoué")
	}
	defer ws.Close()

	server.conn.Write([]byte{0x88, 0x00}) // FIN=1, opcode=close, payload vide

	if _, err := ws.ReadMessage(); err == nil {
		t.Fatal("attendu une erreur après un frame close")
	}
}

func TestDialWebSocketRejectsWrongScheme(t *testing.T) {
	if _, err := dialWebSocket(context.Background(), "http://127.0.0.1:1/"); err == nil {
		t.Fatal("attendu une erreur pour un schéma non-ws")
	}
}

func TestDialWebSocketRejectsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	// 10.255.255.1 : adresse non routable côté test (RFC 5737-like, choisie
	// pour ne jamais répondre ni refuser immédiatement) — juste assez pour
	// vérifier que le ctx est bien respecté, pas pour tester un vrai réseau.
	_, err := dialWebSocket(ctx, "ws://198.51.100.1:1/")
	if err == nil {
		t.Fatal("attendu une erreur (timeout ou connexion refusée)")
	}
}

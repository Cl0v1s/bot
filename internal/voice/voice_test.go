package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Pas de vraies notifications ni de sons pendant les tests.
	notifySendBin, soundBin = "", ""
	os.Exit(m.Run())
}

func TestEncodeWAVHeader(t *testing.T) {
	pcm := make([]byte, 3200)
	wav := encodeWAV(pcm)
	if len(wav) != 44+len(pcm) {
		t.Fatalf("taille = %d", len(wav))
	}
	le := binary.LittleEndian
	if string(wav[0:4]) != "RIFF" || string(wav[8:16]) != "WAVEfmt " || string(wav[36:40]) != "data" {
		t.Fatalf("marqueurs invalides : %q", wav[:44])
	}
	if got := le.Uint32(wav[4:]); got != uint32(36+len(pcm)) {
		t.Errorf("taille RIFF = %d", got)
	}
	if le.Uint16(wav[22:]) != 1 || le.Uint32(wav[24:]) != 16000 || le.Uint16(wav[34:]) != 16 {
		t.Errorf("format inattendu : canaux %d, fréquence %d, bits %d", le.Uint16(wav[22:]), le.Uint32(wav[24:]), le.Uint16(wav[34:]))
	}
	if le.Uint32(wav[40:]) != uint32(len(pcm)) {
		t.Errorf("taille data = %d", le.Uint32(wav[40:]))
	}
}

// tone retourne d de PCM à amplitude constante amp (onde carrée).
func tone(d time.Duration, amp int16) []byte {
	n := int(d.Seconds() * sampleRate)
	out := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		v := amp
		if i%2 == 1 {
			v = -amp
		}
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

func TestRMS(t *testing.T) {
	if got := rms(tone(100*time.Millisecond, 1000)); got < 999 || got > 1001 {
		t.Errorf("rms = %v, attendu 1000", got)
	}
	if got := rms(nil); got != 0 {
		t.Errorf("rms(nil) = %v", got)
	}
}

// writeSource écrit pcm dans un fichier et retourne une commande qui le
// recrache puis reste ouverte (comme un micro), pour simuler pw-record.
func writeSource(t *testing.T, pcm []byte) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.raw")
	if err := os.WriteFile(path, pcm, 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{"sh", "-c", `cat "$0"; exec sleep 30`, path}
}

func TestRecordStopsOnSilence(t *testing.T) {
	pcm := append(tone(500*time.Millisecond, 3000), tone(2*time.Second, 10)...)
	r := Recorder{Cmd: writeSource(t, pcm), SilenceStop: 500 * time.Millisecond, SilenceThreshold: 500, MaxDuration: 10 * time.Second}
	start := time.Now()
	got, heard, err := r.Record(context.Background(), make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	if !heard {
		t.Error("voix non détectée")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("pas d'arrêt sur silence")
	}
	// 0,5 s de voix + ~0,5 s de silence avant l'arrêt.
	if d := time.Duration(len(got)/bytesPerSample) * time.Second / sampleRate; d < 900*time.Millisecond || d > 1500*time.Millisecond {
		t.Errorf("durée enregistrée = %v", d)
	}
}

func TestRecordSilenceOnlyNotHeard(t *testing.T) {
	r := Recorder{Cmd: writeSource(t, tone(300*time.Millisecond, 10)), SilenceStop: 200 * time.Millisecond, SilenceThreshold: 500}
	stop := make(chan struct{})
	time.AfterFunc(500*time.Millisecond, func() { close(stop) })
	got, heard, err := r.Record(context.Background(), stop)
	if err != nil {
		t.Fatal(err)
	}
	if heard || len(got) == 0 {
		t.Errorf("heard=%v len=%d : un silence seul ne doit ni déclencher l'arrêt automatique ni compter comme de la voix", heard, len(got))
	}
}

func TestRecordCommandFailure(t *testing.T) {
	r := Recorder{Cmd: []string{"sh", "-c", "echo pas de micro >&2; exit 1"}}
	_, _, err := r.Record(context.Background(), make(chan struct{}))
	if err == nil || !strings.Contains(err.Error(), "pas de micro") {
		t.Fatalf("err = %v", err)
	}
}

func TestSTTTranscribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			http.Error(w, "chemin "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(f)
		if string(data[:4]) != "RIFF" || r.FormValue("model") != "m" || r.FormValue("language") != "fr" || r.FormValue("response_format") != "json" {
			http.Error(w, "champs", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"text":"  Bonjour le bot. "}`))
	}))
	defer srv.Close()

	s := STT{BaseURL: srv.URL + "/v1/", APIKey: "k", Model: "m", Language: "fr"}
	text, err := s.Transcribe(context.Background(), encodeWAV(tone(100*time.Millisecond, 1000)))
	if err != nil {
		t.Fatal(err)
	}
	if text != "Bonjour le bot." {
		t.Errorf("text = %q", text)
	}

	s.APIKey = "mauvaise"
	if _, err := s.Transcribe(context.Background(), encodeWAV(nil)); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}

func TestControlSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.sock")
	ln, err := listen(path)
	if err != nil {
		t.Fatal(err)
	}
	go serve(ln, func(cmd string) string { return "ok " + cmd })

	resp, err := Send(path, "toggle")
	if err != nil || resp != "ok toggle" {
		t.Fatalf("resp=%q err=%v", resp, err)
	}
	if _, err := listen(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("seconde écoute : err = %v", err)
	}
	ln.Close()
}

func TestControlSocketStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.sock")
	// Socket laissé par une session tuée : le fichier existe, personne
	// n'écoute.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("le socket périmé devrait exister : %v", err)
	}
	ln2, err := listen(path)
	if err != nil {
		t.Fatalf("socket périmé non récupéré : %v", err)
	}
	ln2.Close()
}

func TestSessionEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"text":"oui"}`))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "v.sock")
	src := writeSource(t, tone(300*time.Millisecond, 3000))
	sess, err := Start(context.Background(), Config{
		SocketPath: path,
		Recorder:   Recorder{Cmd: src, SilenceThreshold: 500},
		STT:        STT{BaseURL: srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if resp, _ := Send(path, "toggle"); resp != "ok recording" {
		t.Fatalf("toggle 1 = %q", resp)
	}
	time.Sleep(400 * time.Millisecond)
	if resp, _ := Send(path, "toggle"); resp != "ok recording" {
		// La réponse reflète l'état au moment de la commande : l'arrêt est
		// asynchrone.
		t.Fatalf("toggle 2 = %q", resp)
	}
	select {
	case text := <-sess.Transcripts():
		if text != "oui" {
			t.Errorf("text = %q", text)
		}
	case err := <-sess.Errors():
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("pas de transcription")
	}
	if resp, _ := Send(path, "bidule"); !strings.HasPrefix(resp, "err") {
		t.Errorf("commande inconnue : %q", resp)
	}
}

// Unsloth Studio répond 500 le temps de relancer le modèle de dictée
// déchargé : la requête est rejouée, une erreur 4xx ne l'est pas.
func TestSTTRetriesServerErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case r.FormValue("model") == "absent":
			http.Error(w, `{"error":"not downloaded"}`, http.StatusConflict)
		case calls < 3:
			http.Error(w, `{"error":{"message":"Could not reach an upstream service."}}`, http.StatusInternalServerError)
		default:
			w.Write([]byte(`{"text":"ok"}`))
		}
	}))
	defer srv.Close()

	var retries []int
	s := STT{BaseURL: srv.URL, RetryBackoff: time.Millisecond, OnRetry: func(a, max int, err error) { retries = append(retries, a) }}
	text, err := s.Transcribe(context.Background(), encodeWAV(nil))
	if err != nil || text != "ok" || calls != 3 || len(retries) != 2 {
		t.Fatalf("text=%q err=%v calls=%d retries=%v", text, err, calls, retries)
	}

	calls = 0
	s.Model = "absent"
	if _, err := s.Transcribe(context.Background(), encodeWAV(nil)); err == nil || calls != 1 {
		t.Errorf("4xx rejouée : calls=%d err=%v", calls, err)
	}
}

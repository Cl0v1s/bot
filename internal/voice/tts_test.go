package voice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanForSpeech(t *testing.T) {
	in := "<think>je réfléchis</think># Titre\n\n- **Gras** et `code`\n\n```go\nfmt.Println(1)\n```\nVoir [la doc](https://x.io/a) ou https://y.io 🎉\n"
	got := CleanForSpeech(in)
	for _, bad := range []string{"réfléchis", "#", "**", "`", "fmt.Println", "http", "🎉", "(https"} {
		if strings.Contains(got, bad) {
			t.Errorf("CleanForSpeech contient encore %q : %q", bad, got)
		}
	}
	for _, want := range []string{"Titre", "Gras et code", "Voir la doc ou"} {
		if !strings.Contains(got, want) {
			t.Errorf("CleanForSpeech perd %q : %q", want, got)
		}
	}
}

func TestCleanForSpeechTruncatesAtSentence(t *testing.T) {
	long := strings.Repeat("Une phrase assez courte. ", 200)
	got := CleanForSpeech(long)
	if n := len([]rune(got)); n > maxSpeechRunes {
		t.Errorf("longueur = %d, max %d", n, maxSpeechRunes)
	}
	if !strings.HasSuffix(got, ".") {
		t.Errorf("coupure hors fin de phrase : %q", got[len(got)-20:])
	}
}

func TestSpeakerSendsTextOnStdin(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.txt")
	sp := &Speaker{Cmd: []string{"sh", "-c", "cat > " + out}}
	if err := sp.Speak(context.Background(), "**Bonjour** Clovis"); err != nil {
		t.Fatalf("Speak: %v", err)
	}
	if got, _ := os.ReadFile(out); string(got) != "Bonjour Clovis" {
		t.Fatalf("stdin = %q", got)
	}
}

func TestSpeakerStopInterruptsWithoutError(t *testing.T) {
	sp := &Speaker{Cmd: []string{"sleep", "30"}}
	done := make(chan error, 1)
	go func() { done <- sp.Speak(context.Background(), "texte") }()
	time.Sleep(200 * time.Millisecond)
	sp.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Speak interrompu : %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop n'a pas interrompu la lecture")
	}
}

func TestSpeakerReportsFailureAndNilIsSafe(t *testing.T) {
	if err := (&Speaker{Cmd: []string{"sh", "-c", "echo boom >&2; exit 3"}}).Speak(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("erreur = %v", err)
	}
	var nilSp *Speaker
	if err := nilSp.Speak(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	nilSp.Stop()
}

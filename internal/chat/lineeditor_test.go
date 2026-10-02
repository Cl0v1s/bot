package chat

import (
	"bytes"
	"os"
	"testing"
)

// newTestLineEditor construit un lineEditor lisant input via un pipe, sans
// passer par stty (newLineEditor exige un vrai terminal).
func newTestLineEditor(t *testing.T, input string) *lineEditor {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	go func() {
		_, _ = w.WriteString(input)
		w.Close()
	}()
	con := newConsole(&bytes.Buffer{}, func() int { return 80 })
	return &lineEditor{f: r, con: con}
}

func TestReadLinePasteMultiline(t *testing.T) {
	e := newTestLineEditor(t, "voici \x1b[200~ligne 1\rligne 2\r\nligne 3\x1b[201~ fin\r")
	line, ok := e.ReadLine("> ")
	if !ok {
		t.Fatal("ReadLine: ok=false")
	}
	if want := "voici ligne 1\nligne 2\nligne 3 fin"; line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
	if got := e.history[0]; got != "voici [Contenu collé #1 +2 lignes] fin" {
		t.Fatalf("history = %q", got)
	}
}

func TestReadLinePasteSingleLineInline(t *testing.T) {
	e := newTestLineEditor(t, "\x1b[200~bonjour\x1b[201~\r")
	line, _ := e.ReadLine("> ")
	if line != "bonjour" || len(e.pastes) != 0 {
		t.Fatalf("line = %q, pastes = %d", line, len(e.pastes))
	}
}

func TestReadLinePasteBackspaceRemovesLabel(t *testing.T) {
	e := newTestLineEditor(t, "a\x1b[200~x\ny\x1b[201~\x7fb\r")
	line, _ := e.ReadLine("> ")
	if line != "ab" {
		t.Fatalf("line = %q, want %q", line, "ab")
	}
}

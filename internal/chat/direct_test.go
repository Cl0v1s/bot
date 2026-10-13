package chat

import (
	"context"
	"strings"
	"testing"
)

func TestIsDirectCommand(t *testing.T) {
	for line, want := range map[string]bool{"!ls": true, "! ls": true, "!": false, "!  ": false, "ls": false, "/new": false} {
		if got := isDirectCommand(line); got != want {
			t.Errorf("isDirectCommand(%q)=%v, want %v", line, got, want)
		}
		if want && !isQueueCommand(line) {
			t.Errorf("isQueueCommand(%q) devrait être vrai", line)
		}
	}
}

func TestParseDirectCommand(t *testing.T) {
	name, args := parseDirectCommand("!git  status -s")
	if name != "git" || args != "status -s" {
		t.Errorf("got %q %q", name, args)
	}
}

func TestRunDirectCommand(t *testing.T) {
	out, err := runDirectCommand(context.Background(), "!help")
	if err != nil || !strings.Contains(out, "!help") {
		t.Errorf("help: %q %v", out, err)
	}
	out, err = runDirectCommand(context.Background(), "!echo hi; rm -rf /nonexistent-dir-xyz")
	if err != nil || strings.TrimSpace(out) != "hi" {
		t.Errorf("echo: %q %v", out, err)
	}
	if _, err = runDirectCommand(context.Background(), "!exit 3"); err == nil {
		t.Error("erreur attendue pour un code de sortie non nul")
	}
}

func TestFormatDirectContext(t *testing.T) {
	got := formatDirectContext("!echo hi", "hi\n", nil)
	if !strings.Contains(got, "!echo hi\nhi") {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(formatDirectContext("!x", "", nil), "aucune sortie") {
		t.Error("sortie vide non signalée")
	}
}

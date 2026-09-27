package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude écrit un faux exécutable "claude" qui affiche ses arguments puis
// son entrée standard, pour vérifier ce que ClaudeTool lui transmet.
func fakeClaude(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nfor a in \"$@\"; do echo \"ARG:$a\"; done\necho \"STDIN:$(cat)\"\necho \"PWD:$(pwd)\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestClaudeToolPassesNonInteractiveFlagsAndPrompt(t *testing.T) {
	dir := t.TempDir()
	tool := &ClaudeTool{Bin: fakeClaude(t), PermissionMode: "acceptEdits"}
	out, err := tool.Call(context.Background(), `{"prompt":"-corrige le bug","working_dir":"`+dir+`"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for _, want := range []string{
		"ARG:-p",
		"ARG:acceptEdits",
		"ARG:none",
		"ARG:" + claudeNonInteractivePrompt,
		"STDIN:-corrige le bug",
		"PWD:" + dir,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sortie sans %q:\n%s", want, out)
		}
	}
}

func TestClaudeToolReturnsStdoutOnlyOnSuccess(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\ncat >/dev/null\necho 'J ai fait X.'\necho 'bruit' >&2\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := (&ClaudeTool{Bin: bin}).Call(context.Background(), `{"prompt":"x","working_dir":"`+t.TempDir()+`"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "J ai fait X.") || strings.Contains(out, "bruit") {
		t.Errorf("sortie inattendue:\n%s", out)
	}
}

func TestClaudeToolIncludesStderrOnFailure(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\ncat >/dev/null\necho 'non authentifié' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := (&ClaudeTool{Bin: bin}).Call(context.Background(), `{"prompt":"x","working_dir":"`+t.TempDir()+`"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "non authentifié") || !strings.Contains(out, "terminé avec erreur") {
		t.Errorf("sortie inattendue:\n%s", out)
	}
}

func TestClaudeToolConfirmRefused(t *testing.T) {
	tool := &ClaudeTool{
		Bin:     fakeClaude(t),
		Confirm: func(context.Context, string, string) (bool, error) { return false, nil },
	}
	if _, err := tool.Call(context.Background(), `{"prompt":"x","working_dir":"`+t.TempDir()+`"}`); err == nil {
		t.Fatal("attendu une erreur quand la confirmation est refusée")
	}
}

func TestClaudeToolRejectsRelativeDir(t *testing.T) {
	tool := &ClaudeTool{Bin: fakeClaude(t)}
	if _, err := tool.Call(context.Background(), `{"prompt":"x","working_dir":"relatif"}`); err == nil {
		t.Fatal("attendu une erreur pour un chemin relatif")
	}
}

// Le modèle ne doit lancer Claude Code que sur demande explicite de
// l'utilisateur : la consigne doit figurer en tête de la description.
func TestClaudeDescriptionRequiresExplicitUserRequest(t *testing.T) {
	desc := (&ClaudeTool{}).Description()
	if !strings.HasPrefix(desc, "N'UTILISE CET OUTIL QUE si l'utilisateur te demande EXPLICITEMENT") {
		t.Fatalf("description sans la restriction en tête : %q", desc)
	}
	if !strings.Contains(desc, "JAMAIS de ta propre initiative") {
		t.Fatalf("description sans l'interdiction d'initiative : %q", desc)
	}
}

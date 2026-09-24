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

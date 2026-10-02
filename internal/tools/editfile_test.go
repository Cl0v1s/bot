package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newEditableFile(t *testing.T, content string) (*EditFileTool, string) {
	t.Helper()
	withSandboxReady(t, false)
	dir := t.TempDir()
	perms := NewDirPermissions(nil)
	perms.AlwaysAllow(dir)
	path := filepath.Join(dir, "fichier.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return &EditFileTool{Perms: perms}, path
}

func callEditFile(t *testing.T, tool *EditFileTool, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Call(context.Background(), string(raw))
}

func readBack(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestEditFileReplacesUniquePassage(t *testing.T) {
	tool, path := newEditableFile(t, "a\nb\nc\n")
	if _, err := callEditFile(t, tool, map[string]any{"path": path, "old_string": "b", "new_string": "B1\nB2"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := readBack(t, path); got != "a\nB1\nB2\nc\n" {
		t.Fatalf("contenu = %q", got)
	}
}

func TestEditFileRejectsAmbiguousOldString(t *testing.T) {
	tool, path := newEditableFile(t, "x\nx\n")
	_, err := callEditFile(t, tool, map[string]any{"path": path, "old_string": "x", "new_string": "y"})
	if err == nil || !strings.Contains(err.Error(), "2 fois") {
		t.Fatalf("erreur = %v, attendu un refus mentionnant 2 occurrences", err)
	}
	if got := readBack(t, path); got != "x\nx\n" {
		t.Fatalf("fichier modifié malgré le refus : %q", got)
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	tool, path := newEditableFile(t, "x\nx\n")
	out, err := callEditFile(t, tool, map[string]any{"path": path, "old_string": "x", "new_string": "y", "replace_all": true})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "2 remplacement") {
		t.Fatalf("résultat = %q", out)
	}
	if got := readBack(t, path); got != "y\ny\n" {
		t.Fatalf("contenu = %q", got)
	}
}

func TestEditFileOldStringNotFound(t *testing.T) {
	tool, path := newEditableFile(t, "abc")
	_, err := callEditFile(t, tool, map[string]any{"path": path, "old_string": "zzz", "new_string": "y"})
	if err == nil || !strings.Contains(err.Error(), "introuvable") {
		t.Fatalf("erreur = %v", err)
	}
}

func TestEditFileMissingFileAndRelativePath(t *testing.T) {
	tool, path := newEditableFile(t, "abc")
	if _, err := callEditFile(t, tool, map[string]any{"path": path + ".absent", "old_string": "a", "new_string": "b"}); err == nil || !strings.Contains(err.Error(), "n'existe pas") {
		t.Fatalf("erreur = %v", err)
	}
	if _, err := callEditFile(t, tool, map[string]any{"path": "rel.txt", "old_string": "a", "new_string": "b"}); err == nil || !strings.Contains(err.Error(), "absolu") {
		t.Fatalf("erreur = %v", err)
	}
}

func TestEditFileRefusesUnauthorizedDirAndBinary(t *testing.T) {
	withSandboxReady(t, false)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	denied := &EditFileTool{Perms: NewDirPermissions(nil)}
	if _, err := callEditFile(t, denied, map[string]any{"path": path, "old_string": "a", "new_string": "b"}); err == nil {
		t.Fatal("attendu un refus pour un répertoire non autorisé")
	}

	tool, bin := newEditableFile(t, "a\x00b")
	if _, err := callEditFile(t, tool, map[string]any{"path": bin, "old_string": "a", "new_string": "c"}); err == nil || !strings.Contains(err.Error(), "binaire") {
		t.Fatalf("erreur = %v", err)
	}
}

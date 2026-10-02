package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Un chemin relatif doit être refusé, sans jamais résoudre contre le
// répertoire de travail du processus (ambigu, non garanti côté modèle).
func TestWriteFileRejectsRelativePath(t *testing.T) {
	tool := &WriteFileTool{Perms: NewDirPermissions(nil)}
	_, err := tool.Call(context.Background(), `{"path":"notes/scenario.txt","content":"x"}`)
	if err == nil {
		t.Fatalf("attendu une erreur pour un chemin relatif")
	}
	if !strings.Contains(err.Error(), "absolu") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne le caractère absolu requis", err.Error())
	}
}

// newWritableFile crée un fichier existant (ou pas, si content=="") dans un
// répertoire accordé, et retourne un WriteFileTool prêt à l'éditer.
func newWritableFile(t *testing.T, content string) (*WriteFileTool, string) {
	t.Helper()
	withSandboxReady(t, false)
	dir := t.TempDir()
	perms := NewDirPermissions(nil)
	perms.AlwaysAllow(dir)
	path := filepath.Join(dir, "fichier.txt")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &WriteFileTool{Perms: perms}, path
}

func callWriteFile(t *testing.T, tool *WriteFileTool, path, content string) (string, error) {
	t.Helper()
	req := map[string]any{"path": path, "content": content}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Call(context.Background(), string(raw))
}

// "content" remplace intégralement le fichier existant.
func TestWriteFileReplacesWholeFile(t *testing.T) {
	tool, path := newWritableFile(t, "ancien contenu\n")
	if _, err := callWriteFile(t, tool, path, "nouveau contenu"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "nouveau contenu" {
		t.Fatalf("contenu = %q, attendu remplacement intégral", got)
	}
}

func TestTopmostMissing(t *testing.T) {
	dir := t.TempDir()
	if got := topmostMissing(dir); got != dir {
		t.Errorf("topmostMissing(existant) = %q, attendu %q", got, dir)
	}
	want := filepath.Join(dir, "a")
	if got := topmostMissing(filepath.Join(dir, "a", "b", "c")); got != want {
		t.Errorf("topmostMissing = %q, attendu %q (jamais l'ancêtre existant lui-même)", got, want)
	}
}

// write_file ne doit pas réécrire un fichier existant que le compte sandbox
// ne peut pas écrire lui-même (voir checkExistingFile).
func TestWriteFileRefusesFileNotWritableBySandbox(t *testing.T) {
	withSandboxReady(t, true)
	dir := canonicalTempDir(t)
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	prevCanAccess := sandboxCanAccess
	t.Cleanup(func() { sandboxCanAccess = prevCanAccess })
	sandboxCanAccess = func(p string, write bool) bool { return p == dir }

	tool := &WriteFileTool{Perms: NewDirPermissions(nil)}
	if _, err := callWriteFile(t, tool, path, "nouveau"); err == nil {
		t.Fatal("attendu un refus")
	}
	if data, _ := os.ReadFile(path); string(data) != "original" {
		t.Fatalf("fichier modifié malgré le refus : %q", data)
	}
}

// "append" ajoute à la fin sans toucher au début, et crée le fichier absent.
func TestWriteFileAppend(t *testing.T) {
	tool, path := newWritableFile(t, "debut\n")
	raw, _ := json.Marshal(map[string]any{"path": path, "content": "fin\n", "append": true})
	if _, err := tool.Call(context.Background(), string(raw)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "debut\nfin\n" {
		t.Fatalf("contenu = %q", got)
	}

	tool2, path2 := newWritableFile(t, "")
	raw, _ = json.Marshal(map[string]any{"path": path2, "content": "neuf", "append": true})
	if _, err := tool2.Call(context.Background(), string(raw)); err != nil {
		t.Fatalf("Call (création): %v", err)
	}
	if got, _ := os.ReadFile(path2); string(got) != "neuf" {
		t.Fatalf("contenu = %q", got)
	}
}

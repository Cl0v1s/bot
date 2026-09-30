package tools

import (
	"context"
	"encoding/json"
	"fmt"
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

// Sans "offset", le comportement historique (remplacement intégral, y
// compris création) doit rester inchangé.
func TestWriteFileWithoutOffsetReplacesWholeFile(t *testing.T) {
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

func callWriteFileOldText(t *testing.T, tool *WriteFileTool, path, oldText, content string) (string, error) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"path": path, "old_text": oldText, "content": content})
	if err != nil {
		t.Fatal(err)
	}
	return tool.Call(context.Background(), string(raw))
}

// Scénario observé : le modèle voulait modifier la ligne "Blocage", l'a
// crue en ligne 23 au lieu de 20 et a écrasé "Problème Signature". Avec
// old_text, aucune ligne n'est comptée.
func TestWriteFileOldTextReplacesExactPassage(t *testing.T) {
	orig := "# Notes\n\n- **Blocage** : en désactivant la signature.\n- **Logout** : non implémenté.\n- **Signature** : certificat manuel.\n"
	tool, path := newWritableFile(t, orig)
	out, err := callWriteFileOldText(t, tool, path,
		"- **Blocage** : en désactivant la signature.",
		"- **Blocage** : (⚠️ debug uniquement) en désactivant la signature.")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	got, _ := os.ReadFile(path)
	want := "# Notes\n\n- **Blocage** : (⚠️ debug uniquement) en désactivant la signature.\n- **Logout** : non implémenté.\n- **Signature** : certificat manuel.\n"
	if string(got) != want {
		t.Fatalf("contenu = %q", got)
	}
	if !strings.Contains(out, "3> - **Blocage** : (⚠️") {
		t.Errorf("zone modifiée absente ou mal numérotée : %s", out)
	}
}

func TestWriteFileOldTextMultiLineAndDelete(t *testing.T) {
	tool, path := newWritableFile(t, "a\nb\nc\nd\n")
	if _, err := callWriteFileOldText(t, tool, path, "b\nc\n", ""); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a\nd\n" {
		t.Fatalf("contenu = %q", got)
	}
}

func TestWriteFileOldTextNotFoundLeavesFileUntouched(t *testing.T) {
	orig := "- **Blocage** : en désactivant la signature.\n- suite\n"
	tool, path := newWritableFile(t, orig)
	_, err := callWriteFileOldText(t, tool, path, "- **Blocage** : en désactivant la signature.\n- suite modifiée", "x")
	if err == nil || !strings.Contains(err.Error(), "introuvable") || !strings.Contains(err.Error(), "ligne 1") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != orig {
		t.Fatalf("fichier modifié malgré l'erreur : %q", got)
	}
}

func TestWriteFileOldTextAmbiguousRefused(t *testing.T) {
	orig := "x = 1\ny\nx = 1\n"
	tool, path := newWritableFile(t, orig)
	_, err := callWriteFileOldText(t, tool, path, "x = 1", "x = 2")
	if err == nil || !strings.Contains(err.Error(), "2 fois") || !strings.Contains(err.Error(), "lignes 1, 3") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != orig {
		t.Fatalf("fichier modifié malgré l'erreur : %q", got)
	}
}

func TestWriteFileOldTextCRLF(t *testing.T) {
	tool, path := newWritableFile(t, "a\r\nb\r\nc\r\n")
	if _, err := callWriteFileOldText(t, tool, path, "a\nb", "A\nB"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "A\r\nB\r\nc\r\n" {
		t.Fatalf("contenu = %q", got)
	}
}

// L'ancien mode par numéros de ligne est refusé avec une consigne claire.
func TestWriteFileRejectsOffset(t *testing.T) {
	tool, path := newWritableFile(t, "a\n")
	raw := fmt.Sprintf(`{"path":%q,"content":"b","offset":1,"length":1}`, path)
	_, err := tool.Call(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "old_text") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a\n" {
		t.Fatalf("fichier modifié : %q", got)
	}
}

// Insertion : la ligne d'ancrage est reprise dans old_text et content.
func TestWriteFileOldTextInsertAfterAnchor(t *testing.T) {
	tool, path := newWritableFile(t, "# Titre\nfin\n")
	if _, err := callWriteFileOldText(t, tool, path, "# Titre", "# Titre\n- nouveau point"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "# Titre\n- nouveau point\nfin\n" {
		t.Fatalf("contenu = %q", got)
	}
}

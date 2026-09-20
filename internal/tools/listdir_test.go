package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListDirBasics(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	tool := &ListDirTool{}
	out, err := tool.Call(context.Background(), fmt.Sprintf(`{"path":%q}`, dir))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	want := "a.txt\nb.txt\nsub/"
	if out != want {
		t.Fatalf("out = %q, attendu %q (ordre alphabétique, / pour les dossiers)", out, want)
	}
}

func TestListDirEmpty(t *testing.T) {
	dir := t.TempDir()
	tool := &ListDirTool{}
	out, err := tool.Call(context.Background(), fmt.Sprintf(`{"path":%q}`, dir))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out != "[répertoire vide]" {
		t.Fatalf("out = %q", out)
	}
}

// list_dir ne doit passer par aucune vérification DirPermissions : un
// répertoire jamais accordé doit tout de même être listable.
func TestListDirNotGatedByPermissions(t *testing.T) {
	dir := t.TempDir() // jamais accordé via aucun DirPermissions
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &ListDirTool{}
	out, err := tool.Call(context.Background(), fmt.Sprintf(`{"path":%q}`, dir))
	if err != nil {
		t.Fatalf("Call: %v (list_dir ne devrait requérir aucune permission)", err)
	}
	if out != "secret.txt" {
		t.Fatalf("out = %q", out)
	}
}

// "path" est requis : plus de repli implicite sur le répertoire de travail
// du processus (ambigu, non garanti côté modèle).
func TestListDirRequiresPath(t *testing.T) {
	tool := &ListDirTool{}
	_, err := tool.Call(context.Background(), `{}`)
	if err == nil {
		t.Fatalf("attendu une erreur sans \"path\"")
	}
}

// Un chemin relatif doit être refusé, pas résolu contre le répertoire de
// travail du processus.
func TestListDirRejectsRelativePath(t *testing.T) {
	tool := &ListDirTool{}
	_, err := tool.Call(context.Background(), `{"path":"documents/notes"}`)
	if err == nil {
		t.Fatalf("attendu une erreur pour un chemin relatif")
	}
	if !strings.Contains(err.Error(), "absolu") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne le caractère absolu requis", err.Error())
	}
}

func TestListDirTruncatesLargeDirectories(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := &ListDirTool{MaxEntries: 3}
	out, err := tool.Call(context.Background(), fmt.Sprintf(`{"path":%q}`, dir))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Count(out, ".txt") != 3 {
		t.Fatalf("out = %q, attendu exactement 3 entrées listées", out)
	}
	if !strings.Contains(out, "3/10") {
		t.Fatalf("out = %q, attendu une mention de troncature 3/10", out)
	}
}

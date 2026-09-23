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

func callWriteFile(t *testing.T, tool *WriteFileTool, path, content string, offset, length int) (string, error) {
	t.Helper()
	req := map[string]any{"path": path, "content": content}
	if offset > 0 {
		req["offset"] = offset
	}
	if length > 0 {
		req["length"] = length
	}
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
	if _, err := callWriteFile(t, tool, path, "nouveau contenu", 0, 0); err != nil {
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

// offset avec length=0 doit INSÉRER sans rien supprimer.
func TestWriteFileOffsetInsertsWithoutRemoving(t *testing.T) {
	tool, path := newWritableFile(t, "l1\nl2\nl3\n")
	if _, err := callWriteFile(t, tool, path, "NOUVELLE", 2, 0); err != nil {
		t.Fatalf("Call: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "l1\nNOUVELLE\nl2\nl3\n" {
		t.Fatalf("contenu = %q, attendu l'insertion avant la ligne 2", got)
	}
}

// offset avec length>0 doit REMPLACER exactement ces lignes.
func TestWriteFileOffsetReplacesGivenLength(t *testing.T) {
	tool, path := newWritableFile(t, "l1\nl2\nl3\nl4\n")
	if _, err := callWriteFile(t, tool, path, "X\nY", 2, 2); err != nil {
		t.Fatalf("Call: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "l1\nX\nY\nl4\n" {
		t.Fatalf("contenu = %q, attendu le remplacement des lignes 2-3 par X/Y", got)
	}
}

// offset au numéro de la dernière ligne + 1 doit ajouter à la fin du
// fichier (append), pas échouer.
func TestWriteFileOffsetAppendsAtEnd(t *testing.T) {
	tool, path := newWritableFile(t, "l1\nl2\n")
	if _, err := callWriteFile(t, tool, path, "l3", 3, 0); err != nil {
		t.Fatalf("Call: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "l1\nl2\nl3\n" {
		t.Fatalf("contenu = %q, attendu l'ajout de l3 en fin de fichier", got)
	}
}

// La convention de saut de ligne final du fichier ORIGINAL doit être
// préservée (ici : ABSENCE de saut de ligne final).
func TestWriteFileOffsetPreservesNoTrailingNewline(t *testing.T) {
	tool, path := newWritableFile(t, "l1\nl2") // pas de \n final
	if _, err := callWriteFile(t, tool, path, "X", 1, 1); err != nil {
		t.Fatalf("Call: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "X\nl2" {
		t.Fatalf("contenu = %q, attendu l'absence de \\n final préservée", got)
	}
}

// offset sur un fichier qui n'existe pas encore doit échouer clairement,
// pas créer un fichier partiel ou planter.
func TestWriteFileOffsetOnMissingFileFails(t *testing.T) {
	tool, path := newWritableFile(t, "") // fichier jamais créé
	_, err := callWriteFile(t, tool, path, "X", 1, 0)
	if err == nil {
		t.Fatal("attendu une erreur (fichier inexistant)")
	}
	if !strings.Contains(err.Error(), "n'existe pas encore") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne l'absence du fichier", err.Error())
	}
}

// offset/length au-delà de la fin du fichier doit échouer clairement,
// jamais silencieusement tronquer/étendre au mauvais endroit.
func TestWriteFileOffsetPastEndOfFileFails(t *testing.T) {
	tool, path := newWritableFile(t, "l1\nl2\n")
	_, err := callWriteFile(t, tool, path, "X", 10, 1)
	if err == nil {
		t.Fatal("attendu une erreur (plage hors limites)")
	}
	if !strings.Contains(err.Error(), "dépasse la fin du fichier") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne le dépassement de fin de fichier", err.Error())
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "l1\nl2\n" {
		t.Fatalf("le fichier a été modifié malgré l'échec : %q", got)
	}
}

func TestWriteFileRejectsNegativeOffsetOrLength(t *testing.T) {
	tool, path := newWritableFile(t, "l1\n")
	if _, err := tool.Call(context.Background(), fmt.Sprintf(`{"path":%q,"content":"x","offset":-1}`, path)); err == nil {
		t.Fatal("attendu une erreur pour offset négatif")
	}
	if _, err := tool.Call(context.Background(), fmt.Sprintf(`{"path":%q,"content":"x","offset":1,"length":-1}`, path)); err == nil {
		t.Fatal("attendu une erreur pour length négatif")
	}
}

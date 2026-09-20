package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newReadableFile crée un fichier de contenu content dans un répertoire
// temporaire accordé (via la liste JSON, pas le sandbox réel — voir
// withSandboxReady dans permissions_test.go), et retourne un ReadFileTool
// prêt à le lire.
func newReadableFile(t *testing.T, content string) (*ReadFileTool, string) {
	t.Helper()
	withSandboxReady(t, false)
	dir := t.TempDir()
	perms := NewDirPermissions(nil)
	perms.AlwaysAllow(dir)
	path := filepath.Join(dir, "fichier.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return &ReadFileTool{Perms: perms}, path
}

func callReadFile(t *testing.T, tool *ReadFileTool, path string, offset, length int) string {
	t.Helper()
	args := fmt.Sprintf(`{"path":%q`, path)
	if offset > 0 {
		args += fmt.Sprintf(`,"offset":%d`, offset)
	}
	if length > 0 {
		args += fmt.Sprintf(`,"length":%d`, length)
	}
	args += "}"
	out, err := tool.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call(%s): %v", args, err)
	}
	return out
}

// Sans offset/length, un petit fichier doit revenir intégralement, sans
// aucune annotation de pagination.
func TestReadFileWholeSmallFile(t *testing.T) {
	tool, path := newReadableFile(t, "ligne1\nligne2\nligne3\n")
	got := callReadFile(t, tool, path, 0, 0)
	want := "ligne1\nligne2\nligne3"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// offset/length paginent par NUMÉRO DE LIGNE (1 = première ligne), pas par
// octet : le point central de cette réécriture.
func TestReadFilePaginatesByLineNumber(t *testing.T) {
	content := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10"
	tool, path := newReadableFile(t, content)

	first := callReadFile(t, tool, path, 1, 3)
	if !strings.HasPrefix(first, "l1\nl2\nl3\n") {
		t.Fatalf("premier appel = %q, attendu qu'il commence par les lignes 1-3", first)
	}
	if !strings.Contains(first, "suite disponible avec offset=4") {
		t.Fatalf("premier appel = %q, attendu la suite à l'offset 4 (ligne), pas un offset en octets", first)
	}

	second := callReadFile(t, tool, path, 4, 3)
	if !strings.HasPrefix(second, "l4\nl5\nl6\n") {
		t.Fatalf("second appel (offset=4) = %q, attendu qu'il reprenne à la ligne 4", second)
	}
	if !strings.Contains(second, "suite disponible avec offset=7") {
		t.Fatalf("second appel = %q, attendu la suite à l'offset 7", second)
	}

	last := callReadFile(t, tool, path, 10, 3)
	if !strings.Contains(last, "l10") || strings.Contains(last, "suite disponible") {
		t.Fatalf("dernier appel (offset=10) = %q, attendu la dernière ligne sans \"suite disponible\"", last)
	}
	if !strings.Contains(last, "fin du fichier atteinte") {
		t.Fatalf("dernier appel = %q, attendu la mention de fin de fichier", last)
	}
}

// Un offset au-delà du nombre de lignes du fichier doit le dire clairement,
// en nombre de lignes (pas d'octets).
func TestReadFileOffsetPastEndOfFile(t *testing.T) {
	tool, path := newReadableFile(t, "l1\nl2\nl3")
	got := callReadFile(t, tool, path, 100, 0)
	if !strings.Contains(got, "3 ligne(s)") || !strings.Contains(got, "offset 100 au-delà de la fin") {
		t.Fatalf("got %q, attendu une mention du nombre de lignes et de l'offset demandé", got)
	}
}

func TestReadFileEmptyFile(t *testing.T) {
	tool, path := newReadableFile(t, "")
	got := callReadFile(t, tool, path, 0, 0)
	if got != "[fichier vide]" {
		t.Fatalf("got %q, want %q", got, "[fichier vide]")
	}
}

// read_file appelé sur un répertoire doit échouer explicitement en renvoyant
// vers list_dir, plutôt que de laisser le comportement erratique de la
// lecture d'un répertoire comme s'il s'agissait d'un fichier.
func TestReadFileOnDirectoryRedirectsToListDir(t *testing.T) {
	dir := t.TempDir()
	perms := NewDirPermissions(nil)
	perms.AlwaysAllow(dir)
	tool := &ReadFileTool{Perms: perms}

	_, err := tool.Call(context.Background(), fmt.Sprintf(`{"path":%q}`, dir))
	if err == nil {
		t.Fatalf("attendu une erreur pour un chemin qui est un répertoire")
	}
	if !strings.Contains(err.Error(), "list_dir") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne list_dir", err.Error())
	}
}

// Le renvoi vers list_dir doit avoir lieu AVANT le contrôle de permission :
// un répertoire jamais accordé ne doit jamais produire une erreur d'accès
// refusé, seulement le renvoi vers list_dir (qui, lui, n'a besoin d'aucune
// permission).
func TestReadFileOnUngrantedDirectorySkipsPermissionCheck(t *testing.T) {
	dir := t.TempDir() // jamais accordé via AlwaysAllow/RequestAccess
	perms := NewDirPermissions(nil)
	tool := &ReadFileTool{Perms: perms}

	_, err := tool.Call(context.Background(), fmt.Sprintf(`{"path":%q}`, dir))
	if err == nil {
		t.Fatalf("attendu une erreur pour un chemin qui est un répertoire")
	}
	if !strings.Contains(err.Error(), "list_dir") {
		t.Fatalf("erreur = %q, attendu le renvoi vers list_dir plutôt qu'un refus de permission", err.Error())
	}
}

// Un chemin relatif doit être refusé, même vers un fichier existant dans un
// répertoire par ailleurs autorisé : la résolution contre le répertoire de
// travail du processus est une source d'ambiguïté à éviter, pas une
// commodité à offrir.
func TestReadFileRejectsRelativePath(t *testing.T) {
	tool := &ReadFileTool{Perms: NewDirPermissions(nil)}
	_, err := tool.Call(context.Background(), `{"path":"notes/scenario.txt"}`)
	if err == nil {
		t.Fatalf("attendu une erreur pour un chemin relatif")
	}
	if !strings.Contains(err.Error(), "absolu") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne le caractère absolu requis", err.Error())
	}
}

// Cas fréquent en pratique : un nom de fichier mal mémorisé/deviné (ex:
// renommé depuis). L'erreur doit orienter vers list_dir pour vérifier le nom
// exact, pas se contenter du message système brut ("no such file or
// directory") qui n'aide pas le modèle à se corriger.
func TestReadFileOnMissingFileSuggestsListDir(t *testing.T) {
	tool, existing := newReadableFile(t, "contenu")
	missing := filepath.Join(filepath.Dir(existing), "Nom_Different.md")

	_, err := tool.Call(context.Background(), fmt.Sprintf(`{"path":%q}`, missing))
	if err == nil {
		t.Fatalf("attendu une erreur pour un fichier inexistant")
	}
	if !strings.Contains(err.Error(), "list_dir") {
		t.Fatalf("erreur = %q, attendu qu'elle oriente vers list_dir", err.Error())
	}
	if !strings.Contains(err.Error(), filepath.Dir(existing)) {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne le répertoire à lister", err.Error())
	}
}

// Cas observé en pratique : un PDF de plusieurs Mo lu comme "texte" remplit
// le contexte d'un coup avec du bruit binaire illisible. read_file doit
// refuser explicitement un fichier binaire plutôt que d'en renvoyer les
// octets bruts.
func TestReadFileRejectsBinaryFile(t *testing.T) {
	// Amorce d'en-tête PDF réaliste (contient un octet NUL très tôt, comme
	// un vrai PDF) plutôt qu'un simple "\x00" isolé, pour que le test vaille
	// aussi comme documentation du cas réel visé.
	content := "%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< /Type /Catalog \x00\x01\x02 >>\nendobj\n"
	tool, path := newReadableFile(t, content)

	_, err := tool.Call(context.Background(), fmt.Sprintf(`{"path":%q}`, path))
	if err == nil {
		t.Fatalf("attendu une erreur pour un fichier binaire")
	}
	if !strings.Contains(err.Error(), "binaire") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne le caractère binaire du fichier", err.Error())
	}
}

// À l'inverse, un fichier texte légitime (même avec des caractères
// accentués/UTF-8) ne doit jamais être pris pour du binaire.
func TestReadFileAcceptsTextFileWithAccents(t *testing.T) {
	tool, path := newReadableFile(t, "Résumé du scénario : les héros s'infiltrent à Coruscant.\n")
	got := callReadFile(t, tool, path, 0, 0)
	if !strings.Contains(got, "Résumé du scénario") {
		t.Fatalf("got %q, attendu le contenu texte inchangé", got)
	}
}

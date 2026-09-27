package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadTrustedFileMissingIsNotExist(t *testing.T) {
	_, err := ReadTrustedFile(filepath.Join(t.TempDir(), "absent"))
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, attendu une erreur os.IsNotExist", err)
	}
}

// Un lien symbolique (ex: posé par le compte sandbox à la place du vrai
// fichier, vers un fichier de l'utilisateur) ne doit jamais être suivi.
func TestReadTrustedFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cible")
	if err := os.WriteFile(target, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "lien")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrustedFile(link); err == nil || os.IsNotExist(err) {
		t.Fatalf("err = %v, attendu un refus explicite du lien symbolique", err)
	}
}

func TestReadTrustedFileRefusesDirectory(t *testing.T) {
	if _, err := ReadTrustedFile(t.TempDir()); err == nil {
		t.Fatal("attendu un refus pour un répertoire")
	}
}

// Cas réel des workspaces accordés avant cette protection : fichier à
// l'utilisateur mais en 0660 (groupe llm) — lu, et remis en 0600.
func TestReadTrustedFileRepairsGroupAccessibleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	data, err := ReadTrustedFile(path)
	if err != nil {
		t.Fatalf("ReadTrustedFile: %v", err)
	}
	if string(data) != "A=1\n" {
		t.Fatalf("contenu = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %v, attendu 0600 après réparation", perm)
	}
}

// WriteTrustedFile remplace un lien symbolique présent à path par un vrai
// fichier, sans jamais écrire dans sa cible.
func TestWriteTrustedFileNeverWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cible")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, WhitelistedCommandsFileName)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := WriteTrustedFile(path, []byte("[]")); err != nil {
		t.Fatalf("WriteTrustedFile: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "original" {
		t.Fatalf("cible du lien modifiée : %q", data)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, attendu un fichier régulier en 0600", info.Mode())
	}
	if _, err := ReadTrustedFile(path); err != nil {
		t.Fatalf("relecture: %v", err)
	}
}

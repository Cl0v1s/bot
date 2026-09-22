package sandbox

import (
	"io/fs"
	"testing"
	"time"
)

// fakeFileInfo implémente fs.FileInfo juste assez pour tester
// needsGroupWriteAsUser (seul Mode() est lu).
type fakeFileInfo struct {
	mode fs.FileMode
}

func (f fakeFileInfo) Name() string       { return "x" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

// needsGroupWriteAsUser évite de retraiter (via sudo, coûteux en masse — voir
// son commentaire) un fichier déjà corrigé par un appel précédent de
// GrantDirectory.
func TestNeedsGroupWriteAsUser(t *testing.T) {
	cases := []struct {
		mode fs.FileMode
		want bool
	}{
		{0o644, true},  // rw-r--r-- : pas d'écriture groupe, à corriger
		{0o664, false}, // rw-rw-r-- : déjà bon
		{0o600, true},  // rw------- : pas d'écriture groupe, à corriger
		{0o775, false}, // rwxrwxr-x (répertoire déjà bon)
	}
	for _, c := range cases {
		got := needsGroupWriteAsUser(fakeFileInfo{mode: c.mode})
		if got != c.want {
			t.Errorf("needsGroupWriteAsUser(mode=%v) = %v, attendu %v", c.mode, got, c.want)
		}
	}
}

// GrantDirectory ne doit jamais accorder au compte sandbox l'accès à un
// ".env" — voir le commentaire d'isProtectedFromGrant. Testé isolément
// (plutôt que via GrantDirectory + un vrai chown) : celui-ci dépend de
// privilèges/appartenance de groupe réels, non portables en test.
func TestIsProtectedFromGrant(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{".env", true},
		{"Scénario.md", false},
		{".env.example", false},
		{"env", false},
		{".sandbox-gitconfig", false},
	}
	for _, c := range cases {
		if got := isProtectedFromGrant(c.name); got != c.want {
			t.Errorf("isProtectedFromGrant(%q) = %v, attendu %v", c.name, got, c.want)
		}
	}
}

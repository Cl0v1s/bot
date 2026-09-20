package sandbox

import "testing"

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

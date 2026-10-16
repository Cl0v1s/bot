package sandbox

import (
	"os"
	"testing"
)

func TestResolveSSHKey(t *testing.T) {
	dir := t.TempDir()
	if got, _ := ResolveSSHKey(dir, "/x/key"); got != "/x/key" {
		t.Fatalf("clé explicite ignorée: %q", got)
	}
	p, err := ResolveSSHKey(dir, "")
	if err != nil || p != DedicatedSSHKeyPath(dir) {
		t.Fatalf("p=%q err=%v", p, err)
	}
	for _, f := range []string{p, p + ".pub"} {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(p)
	if _, err := EnsureDedicatedSSHKey(dir); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(p); string(before) != string(after) {
		t.Fatal("clé écrasée")
	}
}

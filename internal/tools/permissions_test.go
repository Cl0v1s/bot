package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// withSandboxReady force, pour la durée du test, la valeur retournée par
// sandboxReady() (voir permissions.go) — sans ça, ces tests dépendraient de
// l'état réel du compte "llm" sur la machine qui les exécute (peut très bien
// être déjà configuré), au lieu du chemin qu'ils veulent précisément
// exercer.
func withSandboxReady(t *testing.T, ready bool) {
	t.Helper()
	prev := sandboxReady
	sandboxReady = func() bool { return ready }
	t.Cleanup(func() { sandboxReady = prev })
}

// Reproduit un système où un chemin usuel est en réalité un lien symbolique
// vers un autre emplacement (ex: /home -> /var/home sur les systèmes
// ostree/Silverblue/uCore), et le cas réel signalé : un répertoire accordé
// en mode chat via un alias doit être reconnu comme déjà autorisé en mode
// mail (grant=nil, donc incapable de "regranter" via un callback) quand il
// est redemandé via l'autre alias du même répertoire réel — sans quoi un
// accès pourtant déjà donné est refusé à tort avec "aucun utilisateur
// disponible".
//
// Important : le second DirPermissions ci-dessous a délibérément grant=nil
// (comme le mode mail) — s'il obtenait quand même granted=true via un
// callback qui accepte tout, le test passerait même sans la résolution des
// liens symboliques, sans rien vérifier d'utile.
func TestDirPermissionsRecognizeSymlinkAliasesAcrossSharedPersistence(t *testing.T) {
	withSandboxReady(t, false) // exerce le repli JSON, pas la vérification OS réelle
	root := t.TempDir()
	real := filepath.Join(root, "var-home")
	target := filepath.Join(real, "Notes", "JDR")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "home")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	aliasTarget := filepath.Join(alias, "Notes", "JDR")

	allowedFile := filepath.Join(t.TempDir(), "allowed.json")

	// Mode chat : accorde via l'alias symlinké.
	chatPerms := NewDirPermissions(func(ctx context.Context, abs, reason string) (bool, error) {
		return true, nil
	})
	if err := chatPerms.WithPersistence(allowedFile); err != nil {
		t.Fatal(err)
	}
	if ok, err := chatPerms.RequestAccess(context.Background(), aliasTarget, "test"); err != nil || !ok {
		t.Fatalf("octroi initial via l'alias: ok=%v err=%v", ok, err)
	}

	// Mode mail : grant=nil, relit le fichier partagé, redemande via le
	// chemin réel (l'autre alias). Ne peut réussir QUE via la
	// reconnaissance "déjà autorisé".
	mailPerms := NewDirPermissions(nil)
	if err := mailPerms.WithPersistence(allowedFile); err != nil {
		t.Fatal(err)
	}
	if ok, err := mailPerms.RequestAccess(context.Background(), target, "test"); err != nil || !ok {
		t.Fatalf("le chemin réel devrait déjà être reconnu autorisé en mode mail après un octroi via l'alias: ok=%v err=%v", ok, err)
	}
}

// Même vérification pour CheckFile (read_file/write_file), pas seulement
// RequestAccess.
func TestCheckFileRecognizesSymlinkAliases(t *testing.T) {
	withSandboxReady(t, false) // exerce le repli JSON, pas la vérification OS réelle
	root := t.TempDir()
	real := filepath.Join(root, "var-home")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "home")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(real, "note.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	perms := NewDirPermissions(nil)
	perms.AlwaysAllow(real) // autorisé via le chemin réel

	// Lu via l'alias symlinké : doit passer.
	if _, err := perms.CheckFileRead(filepath.Join(alias, "note.txt")); err != nil {
		t.Fatalf("CheckFileRead via l'alias: %v", err)
	}
}

// Quand le sandbox est prêt, CheckFileRead/CheckFileWrite doivent vérifier
// l'accès réel du compte sandbox (sandboxCanAccess), pas la liste JSON — y
// compris pour refuser un répertoire présent dans cette liste (via
// AlwaysAllow) si l'accès réel n'est plus au rendez-vous (ex: chgrp défait
// manuellement entre-temps).
func TestCheckFileUsesRealSandboxAccessWhenReady(t *testing.T) {
	withSandboxReady(t, true)
	dir := t.TempDir()
	file := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	prevCanAccess := sandboxCanAccess
	t.Cleanup(func() { sandboxCanAccess = prevCanAccess })

	perms := NewDirPermissions(nil)
	perms.AlwaysAllow(dir) // présent dans la liste JSON...

	sandboxCanAccess = func(path string, write bool) bool { return false } // ...mais refusé par l'OS
	if _, err := perms.CheckFileRead(file); err == nil {
		t.Fatal("attendu un refus : sandbox prêt mais sandboxCanAccess refuse, la liste JSON ne doit pas suffire")
	}

	sandboxCanAccess = func(path string, write bool) bool { return path == dir && !write }
	if _, err := perms.CheckFileRead(file); err != nil {
		t.Fatalf("CheckFileRead: %v", err)
	}
	if _, err := perms.CheckFileWrite(file); err == nil {
		t.Fatal("attendu un refus en écriture : sandboxCanAccess ne l'autorise qu'en lecture")
	}
}

// CheckFileWrite cible un fichier qui n'existe pas encore (cas normal :
// write_file le crée) : la vérification doit alors porter sur le plus proche
// ancêtre existant (voir nearestExisting), jamais échouer simplement parce
// que le fichier (ou ses répertoires parents) n'existent pas encore.
func TestCheckFileWriteChecksNearestExistingAncestor(t *testing.T) {
	withSandboxReady(t, true)
	dir := t.TempDir()
	newFile := filepath.Join(dir, "sous-dossier", "encore", "note.txt")

	prevCanAccess := sandboxCanAccess
	t.Cleanup(func() { sandboxCanAccess = prevCanAccess })
	sandboxCanAccess = func(path string, write bool) bool { return path == dir && write }

	if _, err := (&DirPermissions{}).checkFile(newFile, true); err != nil {
		t.Fatalf("checkFile: %v", err)
	}
}

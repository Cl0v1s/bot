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
	dir := canonicalTempDir(t)
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

	sandboxCanAccess = func(path string, write bool) bool { return (path == dir || path == file) && !write }
	if _, err := perms.CheckFileRead(file); err != nil {
		t.Fatalf("CheckFileRead: %v", err)
	}
	if _, err := perms.CheckFileWrite(file); err == nil {
		t.Fatal("attendu un refus en écriture : sandboxCanAccess ne l'autorise qu'en lecture")
	}
}

// RequestAccess ne doit jamais annoncer un octroi réussi si l'accès réel du
// compte sandbox échoue toujours juste après (voir le commentaire de
// RequestAccess sur la vérification post-octroi) — cas observé en pratique :
// le callback de grant (sandbox.GrantDirectory dans le mode chat réel)
// retourne nil sans que ça se traduise par un accès effectif, laissant
// passer un "accès accordé" trompeur suivi d'un échec confus au premier
// read_file/write_file/run_shell.
func TestRequestAccessFailsWhenPostGrantVerificationFails(t *testing.T) {
	withSandboxReady(t, true)
	dir := t.TempDir()

	prevCanAccess := sandboxCanAccess
	t.Cleanup(func() { sandboxCanAccess = prevCanAccess })
	sandboxCanAccess = func(path string, write bool) bool { return false } // l'octroi n'a rien changé en réalité

	perms := NewDirPermissions(func(ctx context.Context, abs, reason string) (bool, error) {
		return true, nil // le callback de grant affirme réussir...
	})
	ok, err := perms.RequestAccess(context.Background(), dir, "test")
	if ok {
		t.Fatal("attendu ok=false : la vérification post-octroi doit détecter l'incohérence")
	}
	if err == nil {
		t.Fatal("attendu une erreur (pas un simple refus) : l'octroi a réussi mais l'accès réel ne suit pas, ça mérite d'être signalé distinctement")
	}
	if _, err := perms.CheckFileRead(filepath.Join(dir, "x")); err == nil {
		t.Fatal("le répertoire ne doit pas avoir été ajouté à la liste autorisée malgré l'échec de la vérification")
	}
}

// Symétrique du test précédent : quand la vérification post-octroi réussit
// bien, RequestAccess doit continuer à fonctionner normalement (pas de faux
// positif introduit par le nouveau contrôle).
func TestRequestAccessSucceedsWhenPostGrantVerificationPasses(t *testing.T) {
	withSandboxReady(t, true)
	dir := t.TempDir()

	prevCanAccess := sandboxCanAccess
	t.Cleanup(func() { sandboxCanAccess = prevCanAccess })
	// granted ne devient vrai qu'après l'appel du callback de grant (comme
	// le ferait sandbox.GrantDirectory en pratique) : sans cet état, le test
	// ne distinguerait pas "la vérification post-octroi a réussi" de "elle
	// n'a jamais été exercée" (le contrôle initial de RequestAccess, au tout
	// début, aurait déjà court-circuité tout le reste si sandboxCanAccess
	// avait renvoyé true dès le départ).
	granted := false
	sandboxCanAccess = func(path string, write bool) bool { return granted && path == dir }

	perms := NewDirPermissions(func(ctx context.Context, abs, reason string) (bool, error) {
		granted = true // simule l'effet réel de sandbox.GrantDirectory
		return true, nil
	})
	ok, err := perms.RequestAccess(context.Background(), dir, "test")
	if err != nil || !ok {
		t.Fatalf("RequestAccess: ok=%v err=%v", ok, err)
	}
}

// CheckFileWrite cible un fichier qui n'existe pas encore (cas normal :
// write_file le crée) : la vérification doit alors porter sur le plus proche
// ancêtre existant (voir nearestExisting), jamais échouer simplement parce
// que le fichier (ou ses répertoires parents) n'existent pas encore.
func TestCheckFileWriteChecksNearestExistingAncestor(t *testing.T) {
	withSandboxReady(t, true)
	dir := canonicalTempDir(t)
	newFile := filepath.Join(dir, "sous-dossier", "encore", "note.txt")

	prevCanAccess := sandboxCanAccess
	t.Cleanup(func() { sandboxCanAccess = prevCanAccess })
	sandboxCanAccess = func(path string, write bool) bool { return path == dir && write }

	if _, err := (&DirPermissions{}).checkFile(newFile, true); err != nil {
		t.Fatalf("checkFile: %v", err)
	}
}

// canonicalTempDir retourne t.TempDir() liens symboliques résolus : sur
// macOS, il est sous /var/folders, alias de /private/var/folders — or
// sandboxCanAccess reçoit toujours le chemin canonique (voir canonicalPath).
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// En mode sandbox, le répertoire accessible ne suffit pas : read_file
// s'exécute sous l'identité réelle, il ne doit pas lire un fichier que le
// compte sandbox lui-même ne peut pas lire (ex: fichier 600 de
// l'utilisateur dans /tmp).
func TestCheckFileReadChecksFileItselfWhenSandboxed(t *testing.T) {
	withSandboxReady(t, true)
	dir := canonicalTempDir(t)
	file := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	prevCanAccess := sandboxCanAccess
	t.Cleanup(func() { sandboxCanAccess = prevCanAccess })
	sandboxCanAccess = func(path string, write bool) bool { return path == dir }

	if _, err := (&DirPermissions{}).CheckFileRead(file); err == nil {
		t.Fatal("attendu un refus : fichier inaccessible au compte sandbox malgré un répertoire accessible")
	}
	// Fichier à créer : seul le répertoire compte.
	if _, err := (&DirPermissions{}).CheckFileRead(filepath.Join(dir, "absent.txt")); err != nil {
		t.Fatalf("CheckFileRead (fichier absent): %v", err)
	}
}

// .env et les fichiers d'état du harnais vivent dans le workspace, toujours
// accessible : ils doivent quand même rester hors de portée de read_file/
// write_file, avec ou sans sandbox.
func TestCheckFileRefusesProtectedFiles(t *testing.T) {
	for _, ready := range []bool{false, true} {
		withSandboxReady(t, ready)
		prevCanAccess := sandboxCanAccess
		sandboxCanAccess = func(string, bool) bool { return true }

		dir := canonicalTempDir(t)
		perms := NewDirPermissions(nil)
		perms.AlwaysAllow(dir)
		for _, name := range []string{".env", DefaultAllowedDirsFile, DefaultWhitelistedCommandsFile, ".ssh/id_ed25519", ".git-credentials"} {
			path := filepath.Join(dir, name)
			if _, err := perms.CheckFileRead(path); err == nil {
				t.Errorf("sandbox=%v: lecture de %q acceptée, attendu un refus", ready, name)
			}
			if _, err := perms.CheckFileWrite(path); err == nil {
				t.Errorf("sandbox=%v: écriture de %q acceptée, attendu un refus", ready, name)
			}
		}
		if _, err := perms.CheckFileRead(filepath.Join(dir, "MEMORY.md")); err != nil {
			t.Errorf("sandbox=%v: MEMORY.md refusé: %v", ready, err)
		}
		sandboxCanAccess = prevCanAccess
	}
}

// Un octroi trop large (racine, répertoire personnel ou l'un de ses
// parents) est refusé sans même solliciter l'humain.
func TestRequestAccessRefusesTooBroadDirectories(t *testing.T) {
	withSandboxReady(t, false)
	home := t.TempDir()
	t.Setenv("HOME", home)
	asked := false
	perms := NewDirPermissions(func(ctx context.Context, abs, reason string) (bool, error) {
		asked = true
		return true, nil
	})
	for _, dir := range []string{"/", "/etc", home, filepath.Dir(home)} {
		ok, err := perms.RequestAccess(context.Background(), dir, "test")
		if ok || err == nil {
			t.Errorf("RequestAccess(%q): ok=%v err=%v, attendu un refus explicite", dir, ok, err)
		}
	}
	if asked {
		t.Fatal("l'humain ne doit pas être sollicité pour un répertoire trop large")
	}
	sub := filepath.Join(home, "projet")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := perms.RequestAccess(context.Background(), sub, "test"); !ok || err != nil {
		t.Fatalf("RequestAccess(sous-répertoire): ok=%v err=%v", ok, err)
	}
}

// Un fichier de persistance qui n'est plus un fichier de confiance (ici un
// lien symbolique, comme pourrait en poser le compte sandbox) est refusé.
func TestWithPersistenceRefusesSymlinkedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cible.json")
	if err := os.WriteFile(target, []byte(`["/"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, DefaultAllowedDirsFile)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := NewDirPermissions(nil).WithPersistence(path); err == nil {
		t.Fatal("attendu une erreur pour un fichier de persistance en lien symbolique")
	}
}

// withSandboxGrant remplace sandbox.GrantDirectory pour la durée du test, et
// retourne la liste des répertoires sur lesquels il a été appelé.
func withSandboxGrant(t *testing.T, err error) *[]string {
	t.Helper()
	var calls []string
	prev := sandboxGrant
	sandboxGrant = func(dir string) error {
		calls = append(calls, dir)
		return err
	}
	t.Cleanup(func() { sandboxGrant = prev })
	return &calls
}

// Un répertoire déjà autorisé redemandé via request_directory_access : pas
// de nouvelle confirmation, mais les droits du groupe sandbox sont
// réappliqués (fichiers ajoutés depuis sans g+rw).
func TestRequestAccessResyncsAlreadyGrantedDirectory(t *testing.T) {
	withSandboxReady(t, true)
	dir := canonicalTempDir(t)
	prevCanAccess := sandboxCanAccess
	t.Cleanup(func() { sandboxCanAccess = prevCanAccess })
	sandboxCanAccess = func(path string, write bool) bool { return path == dir }
	calls := withSandboxGrant(t, nil)

	perms := NewDirPermissions(func(ctx context.Context, abs, reason string) (bool, error) {
		t.Fatal("aucune confirmation attendue pour un répertoire déjà autorisé")
		return false, nil
	})
	ok, err := perms.RequestAccess(context.Background(), dir, "")
	if err != nil || !ok {
		t.Fatalf("RequestAccess: ok=%v err=%v", ok, err)
	}
	if len(*calls) != 1 || (*calls)[0] != dir {
		t.Fatalf("GrantDirectory appelé sur %v, attendu [%s]", *calls, dir)
	}
}

// Un échec de la réapplication est remonté au modèle, pas masqué derrière
// un "accès accordé".
func TestRequestAccessReportsResyncFailure(t *testing.T) {
	withSandboxReady(t, true)
	dir := canonicalTempDir(t)
	prevCanAccess := sandboxCanAccess
	t.Cleanup(func() { sandboxCanAccess = prevCanAccess })
	sandboxCanAccess = func(path string, write bool) bool { return path == dir }
	withSandboxGrant(t, os.ErrPermission)

	if ok, err := NewDirPermissions(nil).RequestAccess(context.Background(), dir, ""); err == nil || ok {
		t.Fatalf("attendu une erreur, reçu ok=%v err=%v", ok, err)
	}
}

// Jamais de parcours récursif d'un répertoire trop large (ex: /tmp, toujours
// accessible via AlwaysAllow), ni hors sandbox.
func TestRequestAccessDoesNotResyncTooBroadOrUnsandboxed(t *testing.T) {
	withSandboxReady(t, true)
	prevCanAccess := sandboxCanAccess
	t.Cleanup(func() { sandboxCanAccess = prevCanAccess })
	sandboxCanAccess = func(path string, write bool) bool { return true }
	calls := withSandboxGrant(t, nil)

	if ok, err := NewDirPermissions(nil).RequestAccess(context.Background(), "/tmp", ""); err != nil || !ok {
		t.Fatalf("RequestAccess(/tmp): ok=%v err=%v", ok, err)
	}
	withSandboxReady(t, false)
	perms := NewDirPermissions(nil)
	dir := canonicalTempDir(t)
	perms.AlwaysAllow(dir)
	if ok, err := perms.RequestAccess(context.Background(), dir, ""); err != nil || !ok {
		t.Fatalf("RequestAccess hors sandbox: ok=%v err=%v", ok, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("GrantDirectory ne doit pas être appelé, appelé sur %v", *calls)
	}
}

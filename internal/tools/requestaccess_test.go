package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// En mode mail (grant=nil : aucun humain disponible), un répertoire déjà
// autorisé (ex: accordé plus tôt via le mode chat) doit répondre comme si
// l'autorisation venait d'être donnée, sans jamais atteindre le message
// "aucun utilisateur disponible" — ce message ne doit s'appliquer qu'à un
// répertoire réellement pas encore autorisé.
func TestRequestDirectoryAccessAlreadyAllowedInNonInteractiveMode(t *testing.T) {
	withSandboxReady(t, false) // exerce le repli JSON (AlwaysAllow), pas la vérification OS réelle
	dir := t.TempDir()
	perms := NewDirPermissions(nil) // grant=nil : simule le mode mail
	perms.AlwaysAllow(dir)

	tool := &RequestDirectoryAccessTool{Perms: perms}
	out, err := tool.Call(context.Background(), fmt.Sprintf(`{"directory":%q}`, dir))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "accès accordé") {
		t.Fatalf("résultat = %q, attendu un accès accordé pour un répertoire déjà autorisé", out)
	}
}

// Même vérification via le vrai mécanisme de persistance partagée
// chat/mail : un répertoire accordé "en mode chat" (grant non-nil) et relu
// depuis le fichier partagé "en mode mail" (grant nil, Refresh) doit lui
// aussi être immédiatement considéré comme autorisé.
func TestRequestDirectoryAccessAlreadyAllowedViaSharedPersistence(t *testing.T) {
	withSandboxReady(t, false) // exerce le repli JSON partagé, pas la vérification OS réelle
	granted := t.TempDir()
	allowedFile := filepath.Join(t.TempDir(), "allowed.json")

	chatPerms := NewDirPermissions(func(ctx context.Context, abs, reason string) (bool, error) {
		return true, nil
	})
	if err := chatPerms.WithPersistence(allowedFile); err != nil {
		t.Fatalf("WithPersistence (chat): %v", err)
	}
	if ok, err := chatPerms.RequestAccess(context.Background(), granted, "test"); err != nil || !ok {
		t.Fatalf("octroi initial: ok=%v err=%v", ok, err)
	}

	mailPerms := NewDirPermissions(nil)
	if err := mailPerms.WithPersistence(allowedFile); err != nil {
		t.Fatalf("WithPersistence (mail): %v", err)
	}

	tool := &RequestDirectoryAccessTool{Perms: mailPerms}
	out, err := tool.Call(context.Background(), fmt.Sprintf(`{"directory":%q}`, granted))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "accès accordé") {
		t.Fatalf("résultat = %q, attendu un accès accordé pour un répertoire déjà autorisé via le fichier partagé", out)
	}
}

// À l'inverse, un répertoire réellement jamais autorisé en mode mail doit
// bien produire le message explicite "aucun utilisateur disponible".
func TestRequestDirectoryAccessDeniedWhenNonInteractiveAndNotAllowed(t *testing.T) {
	dir := t.TempDir()
	perms := NewDirPermissions(nil)

	tool := &RequestDirectoryAccessTool{Perms: perms}
	out, err := tool.Call(context.Background(), fmt.Sprintf(`{"directory":%q}`, dir))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "aucun utilisateur disponible") {
		t.Fatalf("résultat = %q, attendu le message d'indisponibilité en mode non interactif", out)
	}
}

// Un chemin relatif doit être refusé, sans jamais résoudre contre le
// répertoire de travail du processus (ambigu, non garanti côté modèle).
func TestRequestDirectoryAccessRejectsRelativePath(t *testing.T) {
	tool := &RequestDirectoryAccessTool{Perms: NewDirPermissions(nil)}
	_, err := tool.Call(context.Background(), `{"directory":"Documents/Notes/JDR"}`)
	if err == nil {
		t.Fatalf("attendu une erreur pour un chemin relatif")
	}
	if !strings.Contains(err.Error(), "absolu") {
		t.Fatalf("erreur = %q, attendu qu'elle mentionne le caractère absolu requis", err.Error())
	}
}

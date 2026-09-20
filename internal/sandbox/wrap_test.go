package sandbox

import (
	"context"
	"strings"
	"testing"
)

// Sans ce umask, un mkdir/une redirection faits par une commande sandboxée
// (propriétaire User) redevient illisible en écriture pour l'utilisateur
// réel dès qu'il y touche à son tour via read_file/write_file — cause
// observée d'un "permission denied" au write_file suivant un mkdir fait par
// run_shell.
func TestWrapCommandSetsPermissiveUmask(t *testing.T) {
	cmd := WrapCommand(context.Background(), "mkdir -p sous-dossier", "", "")

	found := false
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "umask 002;") && strings.Contains(arg, "mkdir -p sous-dossier") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Args = %v, attendu un argument \"umask 002; mkdir -p sous-dossier\"", cmd.Args)
	}
}

func TestWrapCommandPassesGitConfigPath(t *testing.T) {
	cmd := WrapCommand(context.Background(), "git status", "/tmp/gitconfig", "")

	found := false
	for _, arg := range cmd.Args {
		if arg == "GIT_CONFIG_GLOBAL=/tmp/gitconfig" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Args = %v, attendu GIT_CONFIG_GLOBAL=/tmp/gitconfig", cmd.Args)
	}
}

// User n'a pas de répertoire personnel réel : sans HOME réglé explicitement,
// un outil comme `glab auth login` échoue en tentant d'y créer sa config
// (voir le commentaire de WrapCommand sur homeDir). HOME ne peut pas passer
// comme les autres variables (affectation avant la commande sur la ligne
// sudo) à cause de l'option sudoers "always_set_home" : il doit être
// exporté à l'intérieur même du script exécuté par sh -c.
func TestWrapCommandPassesHomeDir(t *testing.T) {
	cmd := WrapCommand(context.Background(), "glab auth login", "", "/tmp/workspace/.sandbox-home")

	found := false
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "export HOME='/tmp/workspace/.sandbox-home';") && strings.Contains(arg, "glab auth login") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Args = %v, attendu un argument exportant HOME='/tmp/workspace/.sandbox-home'", cmd.Args)
	}
}

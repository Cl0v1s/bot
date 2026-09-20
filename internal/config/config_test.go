package config

import (
	"os"
	"path/filepath"
	"testing"
)

// ConfigFilePath ne doit jamais dépendre du répertoire courant : c'est
// précisément ce qu'on corrige (avant, ./bot cherchait ".env" relatif au
// cwd, donc "perdait" la config selon d'où il était lancé).
func TestConfigFilePathIgnoresWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WORKSPACE_DIR", "") // pas de variable réelle : repli sur le défaut

	want := filepath.Join(home, "bot-workspace", ".env")
	if got := ConfigFilePath(); got != want {
		t.Fatalf("ConfigFilePath() = %q, want %q", got, want)
	}
}

// Une variable d'environnement WORKSPACE_DIR déjà présente au niveau du
// process (pas dans le fichier .env lui-même, réglée avant même de le
// chercher) doit être respectée — sans quoi il n'y aurait aucun moyen de
// redéfinir l'emplacement du fichier de config, sa propre localisation ne
// pouvant pas dépendre de son propre contenu.
func TestConfigFilePathRespectsRealWorkspaceDirEnvVar(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WORKSPACE_DIR", dir)

	want := filepath.Join(dir, ".env")
	if got := ConfigFilePath(); got != want {
		t.Fatalf("ConfigFilePath() = %q, want %q", got, want)
	}
}

func TestEnsureConfigFileCreatesWithDefaultContentOnlyIfAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sous-dossier", ".env")

	if err := EnsureConfigFile(path, []byte("DEFAUT=1\n")); err != nil {
		t.Fatalf("EnsureConfigFile (création): %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("lecture après création: %v", err)
	}
	if string(got) != "DEFAUT=1\n" {
		t.Fatalf("contenu = %q, want %q", got, "DEFAUT=1\n")
	}

	// N'écrase jamais un fichier déjà présent, même avec un contenu différent
	// du défaut fourni cette fois — y compris modifié/vidé par l'utilisateur.
	if err := os.WriteFile(path, []byte("REEL=vrai_reglage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureConfigFile(path, []byte("DEFAUT=1\n")); err != nil {
		t.Fatalf("EnsureConfigFile (déjà présent): %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "REEL=vrai_reglage\n" {
		t.Fatalf("le fichier existant a été écrasé : %q", got)
	}
}

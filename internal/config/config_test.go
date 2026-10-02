package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestVoiceConfigDefaultsAndOverrides(t *testing.T) {
	for _, k := range []string{"VOICE_SILENCE_STOP", "VOICE_RECORD_CMD", "VOICE_ENABLED"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	c := Load()
	if c.VoiceEnabled || c.VoiceSilenceStop != 2*time.Second || len(c.VoiceRecordCmd) != 0 {
		t.Errorf("défauts : enabled=%v silence=%v cmd=%v", c.VoiceEnabled, c.VoiceSilenceStop, c.VoiceRecordCmd)
	}

	t.Setenv("VOICE_SILENCE_STOP", "0")
	t.Setenv("VOICE_RECORD_CMD", "arecord -q -t raw")
	c = Load()
	if c.VoiceSilenceStop != 0 {
		t.Errorf("silence = %v", c.VoiceSilenceStop)
	}
	if strings.Join(c.VoiceRecordCmd, "|") != "arecord|-q|-t|raw" {
		t.Errorf("cmd = %v", c.VoiceRecordCmd)
	}
}

func TestLLMMaxTokensDefaultsToContextShare(t *testing.T) {
	for _, k := range []string{"LLM_MAX_TOKENS", "LLM_CONTEXT_TOKENS"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if c := Load(); c.LLMMaxTokens != 4096 {
		t.Errorf("défaut (contexte 8192) = %d, attendu 4096", c.LLMMaxTokens)
	}
	t.Setenv("LLM_CONTEXT_TOKENS", "32768")
	if c := Load(); c.LLMMaxTokens != 16384 {
		t.Errorf("contexte 32768 = %d, attendu 16384", c.LLMMaxTokens)
	}
	t.Setenv("LLM_MAX_TOKENS", "1000")
	if c := Load(); c.LLMMaxTokens != 1000 {
		t.Errorf("valeur explicite ignorée : %d", c.LLMMaxTokens)
	}
}

func TestVoiceTTSConfigDefaultsAndOverrides(t *testing.T) {
	for _, k := range []string{"VOICE_TTS_ENABLED", "VOICE_TTS_CMD"} {
		t.Setenv(k, "")
	}
	c := Load()
	if !c.VoiceTTSEnabled || strings.Join(c.VoiceTTSCmd, " ") != "espeak-ng -v fr+robosoft8 -p 30 -s 130" {
		t.Errorf("défauts : enabled=%v cmd=%v", c.VoiceTTSEnabled, c.VoiceTTSCmd)
	}
	t.Setenv("VOICE_TTS_ENABLED", "false")
	t.Setenv("VOICE_TTS_CMD", "espeak-ng -v fr+UniversalRobot")
	c = Load()
	if c.VoiceTTSEnabled || strings.Join(c.VoiceTTSCmd, "|") != "espeak-ng|-v|fr+UniversalRobot" {
		t.Errorf("surcharges : enabled=%v cmd=%v", c.VoiceTTSEnabled, c.VoiceTTSCmd)
	}
}

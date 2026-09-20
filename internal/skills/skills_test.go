package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadIgnoresFlatMarkdownFile(t *testing.T) {
	dir := t.TempDir()
	// Un fichier .md directement dans dir n'est plus une skill valide :
	// chaque skill doit être dans son propre sous-répertoire.
	if err := os.WriteFile(filepath.Join(dir, "plate.md"), []byte("---\nname: plate\n---\ncontenu"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Load() = %+v, attendu aucune skill (fichier plat ignoré)", got)
	}
}

func TestLoadFindsSkillInOwnSubdirectory(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "ma-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: ma-skill\ndescription: fait un truc\n---\ninstructions"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Load() = %+v, attendu une skill", got)
	}
	if got[0].Name != "ma-skill" || got[0].Description != "fait un truc" {
		t.Fatalf("skill chargée = %+v", got[0])
	}
}

// Sans en-tête, le nom du sous-répertoire sert de nom de la skill.
func TestLoadDerivesNameFromDirectoryWithoutHeader(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "sans-entete")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("juste des instructions, pas d'en-tête"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Name != "sans-entete" {
		t.Fatalf("Load() = %+v, attendu le nom dérivé du sous-répertoire", got)
	}
}

// EnsureDefaults doit créer la skill par défaut dans son propre
// sous-répertoire, pas comme fichier plat.
func TestEnsureDefaultsCreatesOwnSubdirectory(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureDefaults(dir); err != nil {
		t.Fatalf("EnsureDefaults: %v", err)
	}

	path := filepath.Join(dir, defaultSkillDir, "SKILL.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("SKILL.md attendu à %q: %v", path, err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Name != defaultSkillDir {
		t.Fatalf("Load() = %+v, attendu la skill par défaut", got)
	}
}

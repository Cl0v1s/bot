package ontology

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"bot/internal/llm"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "onto.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sample() Extraction {
	return Extraction{
		Entities: []Entity{
			{Name: "Synapse", Type: "projet", Description: "Projet médical"},
			{Name: "Medicine", Type: "groupe"},
			{Name: "React", Type: "Technologie"},
		},
		Relations: []Relation{
			{Source: "Synapse", Relation: "appartient à", Target: "Medicine"},
			{Source: "React", Relation: "est une sous-catégorie de", Target: "Framework Frontend"},
			{Source: "synapse", Relation: "Appartient  à", Target: "MEDICINE"}, // doublon
			{Source: "Synapse", Relation: "x", Target: "Synapse"},              // boucle
		},
	}
}

func TestApplyDedupesAndCreatesMissingEntities(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	st, err := s.Apply(ctx, sample())
	if err != nil {
		t.Fatal(err)
	}
	if st.NewEntities != 4 || st.NewRelations != 2 {
		t.Fatalf("stats = %+v, attendu 4 concepts et 2 relations", st)
	}

	// Second passage identique : rien de nouveau, mentions incrémentées.
	st, err = s.Apply(ctx, sample())
	if err != nil {
		t.Fatal(err)
	}
	if st.NewEntities != 0 || st.NewRelations != 0 || st.UpdatedEntities != 4 {
		t.Fatalf("stats 2e passage = %+v", st)
	}
	n, e, _ := s.Counts(ctx)
	if n != 4 || e != 2 {
		t.Fatalf("counts = %d/%d, attendu 4/2", n, e)
	}
}

func TestDescribe(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if out, err := s.Describe(ctx, "x", 1, ""); !strings.Contains(out, "vide") {
		t.Log(err)
		t.Fatalf("base vide: %q", out)
	}
	if _, err := s.Apply(ctx, sample()); err != nil {
		t.Fatal(err)
	}

	out, err := s.Describe(ctx, "SYNAP", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Synapse [Projet]", "Projet médical", "--APPARTIENT_À--> Medicine [Groupe]"} {
		if !strings.Contains(out, want) {
			t.Errorf("manque %q dans:\n%s", want, out)
		}
	}

	// Voisin entrant, et profondeur 2 depuis React.
	out, _ = s.Describe(ctx, "medicine", 1, "")
	if !strings.Contains(out, "<--APPARTIENT_À-- Synapse") {
		t.Errorf("lien entrant manquant:\n%s", out)
	}
	out, _ = s.Describe(ctx, "react", 2, "")
	if !strings.Contains(out, "## Framework Frontend") {
		t.Errorf("voisin à 2 niveaux attendu:\n%s", out)
	}

	// Filtre de relation.
	out, _ = s.Describe(ctx, "synapse", 1, "est une sous-catégorie de")
	if strings.Contains(out, "Medicine") {
		t.Errorf("le filtre de relation devrait exclure Medicine:\n%s", out)
	}

	if out, _ = s.Describe(ctx, "inconnu", 1, ""); !strings.Contains(out, "Aucun concept") {
		t.Errorf("sans résultat: %q", out)
	}
	if out, _ = s.Describe(ctx, "", 1, ""); !strings.Contains(out, "Types de relations") || !strings.Contains(out, "APPARTIENT_À (1)") {
		t.Errorf("aperçu: %q", out)
	}
}

func TestDescribeInjectionSafe(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.Apply(ctx, sample()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Describe(ctx, `'; DROP TABLE nodes; --`, 1, `"'`); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := s.Counts(ctx); n != 4 {
		t.Fatalf("nodes = %d", n)
	}
}

func TestNormalization(t *testing.T) {
	if got := cleanType(" projet  logiciel "); got != "ProjetLogiciel" {
		t.Errorf("cleanType = %q", got)
	}
	if got := cleanType("  "); got != "Concept" {
		t.Errorf("cleanType vide = %q", got)
	}
	if got := cleanRelation(" est-une sous catégorie de "); got != "EST_UNE_SOUS_CATÉGORIE_DE" {
		t.Errorf("cleanRelation = %q", got)
	}
}

func TestParseExtraction(t *testing.T) {
	ext, err := parseExtraction("Voici :\n```json\n{\"entities\":[{\"name\":\"A\",\"type\":\"T\",\"description\":\"d\"}],\"relations\":[]}\n```")
	if err != nil || len(ext.Entities) != 1 || ext.Entities[0].Name != "A" {
		t.Fatalf("ext=%+v err=%v", ext, err)
	}
	if _, err := parseExtraction("rien"); err == nil {
		t.Error("erreur attendue sans JSON")
	}
	if _, err := parseExtraction("{pas du json}"); err == nil {
		t.Error("erreur attendue sur JSON invalide")
	}
}

func TestSince(t *testing.T) {
	msgs := []llm.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}, {Role: "user", Content: "c"}}
	if got := Since(msgs, nil); len(got) != 3 {
		t.Errorf("nil => tout, got %d", len(got))
	}
	last := msgs[1]
	if got := Since(msgs, &last); len(got) != 1 || got[0].Content != "c" {
		t.Errorf("got %+v", got)
	}
	gone := llm.Message{Role: "user", Content: "disparu"}
	if got := Since(msgs, &gone); len(got) != 3 {
		t.Errorf("message introuvable => tout, got %d", len(got))
	}
}

func TestTranscriptSkipsToolsAndSystem(t *testing.T) {
	got := transcript([]llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "bonjour"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{}}},
		{Role: "tool", Content: "résultat bruyant"},
		{Role: "assistant", Content: "salut"},
	})
	if got != "Utilisateur : bonjour\n\nAssistant : salut" {
		t.Errorf("transcript = %q", got)
	}
}

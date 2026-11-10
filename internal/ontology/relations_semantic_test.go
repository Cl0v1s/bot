package ontology

import (
	"context"
	"errors"
	"testing"
)

type fakeEmbedder struct {
	vecs map[string][]float32
	err  error
}

func (f fakeEmbedder) Embed(_ context.Context, in []string) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(in))
	for i, s := range in {
		out[i] = f.vecs[s]
	}
	return out, nil
}

func TestSemanticRelationAliases(t *testing.T) {
	emb := fakeEmbedder{vecs: map[string][]float32{
		"utilise":         {1, 0, 0},
		"emploie":         {0.98, 0.1, 0},
		"est utilisé par": {0.97, 0.1, 0.1},
		"héberge":         {0, 1, 0},
		"n'utilise pas":   {0.99, 0.05, 0},
		"dépend de":       {0, 0, 1},
	}}
	counts := map[string]int{"UTILISE": 5, "EMPLOIE": 2, "EST_UTILISÉ_PAR": 1, "HÉBERGE": 3, "N'UTILISE_PAS": 1, "DÉPEND_DE": 4}
	got, err := semanticRelationAliases(context.Background(), emb, counts, 0.9)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["EMPLOIE"] != "UTILISE" {
		t.Fatalf("aliases = %v, attendu seulement EMPLOIE → UTILISE (sens inverse et négation exclus)", got)
	}
}

func TestSemanticRelationAliasesError(t *testing.T) {
	_, err := semanticRelationAliases(context.Background(), fakeEmbedder{err: errors.New("hs")}, map[string]int{"A": 1, "B": 1}, 0.9)
	if err == nil {
		t.Fatal("attendu l'erreur de l'embedder")
	}
}

func TestCleanMergesSemanticRelations(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustApply(t, s, Extraction{Relations: []Relation{
		{Source: "A", Relation: "UTILISE", Target: "B"},
		{Source: "A", Relation: "UTILISE", Target: "C"},
		{Source: "A", Relation: "UTILISE", Target: "E"},
		{Source: "A", Relation: "S'APPUIE_SUR", Target: "B"}, // doublon une fois fusionné
		{Source: "A", Relation: "S'APPUIE_SUR", Target: "D"},
	}})
	s.Embedder = fakeEmbedder{vecs: map[string][]float32{
		"utilise":      {1, 0},
		"s appuie sur": {0.99, 0.05},
	}}

	rep, err := s.Clean(ctx, CleanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied || len(rep.RelationMerges) != 1 || rep.RelationMerges[0] != "S_APPUIE_SUR → UTILISE" {
		t.Fatalf("plan = %+v", rep)
	}

	rep, err = s.Clean(ctx, CleanOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.RelationsRenamed != 1 || rep.EdgesRemoved != 1 {
		t.Fatalf("rapport = %+v", rep)
	}
	if _, e, _ := s.Counts(ctx); e != 4 {
		t.Fatalf("%d relations, attendu 4", e)
	}

	// Serveur d'embeddings en panne : le reste du nettoyage n'est pas bloqué.
	mustApply(t, s, Extraction{Relations: []Relation{{Source: "A", Relation: "HÉBERGE", Target: "B"}}})
	s.Embedder = fakeEmbedder{err: errors.New("hs")}
	rep, err = s.Clean(ctx, CleanOptions{})
	if err != nil || rep.EmbeddingErr == "" {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
}

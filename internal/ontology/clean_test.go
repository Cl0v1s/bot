package ontology

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	graph "github.com/justintout/go-sqlite-graph"
)

func mustApply(t *testing.T, s *Store, ext Extraction) {
	t.Helper()
	if _, err := s.Apply(context.Background(), ext); err != nil {
		t.Fatal(err)
	}
}

func TestNormKey(t *testing.T) {
	for _, tc := range [][2]string{
		{"Les Bases de données", "base de donnee"},
		{"L'équipe Produit", "equipe produit"},
		{"Synapse.", "synapse"},
		{"Kubernetes", "kubernete"}, // approximatif mais stable : sert à comparer
	} {
		if got := normKey(tc[0]); got != tc[1] {
			t.Errorf("normKey(%q) = %q, attendu %q", tc[0], got, tc[1])
		}
	}
	if normKey("Kubernetes") != normKey("kubernete") {
		t.Error("pluriel non ramené")
	}
}

func TestCleanMergesDuplicatesAndAliases(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustApply(t, s, Extraction{
		Entities: []Entity{
			{Name: "Base de données", Type: "Technologie", Description: "stockage"},
			{Name: "Bases de donnees", Type: "Notion", Description: "stockage relationnel et documents"},
			{Name: "Postgres"},
			{Name: "Équipe"},
		},
		Relations: []Relation{
			{Source: "Postgres", Relation: "est une sous-catégorie de", Target: "Base de données"},
			{Source: "Postgres", Relation: "est une sous-catégorie de", Target: "Bases de donnees"}, // devient doublon
			{Source: "Équipe", Relation: "utilise", Target: "Bases de donnees"},
			{Source: "Bases de donnees", Relation: "fait partie de", Target: "Base de données"}, // devient boucle
		},
	})

	// Dry-run : rien ne change.
	rep, err := s.Clean(ctx, CleanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Merges) != 1 || rep.Applied {
		t.Fatalf("plan = %+v", rep)
	}
	if n, _, _ := s.Counts(ctx); n != 4 {
		t.Fatalf("dry-run a modifié la base : %d concepts", n)
	}

	rep, err = s.Clean(ctx, CleanOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || len(rep.Merges) != 1 {
		t.Fatalf("rapport = %+v", rep)
	}
	n, e, _ := s.Counts(ctx)
	if n != 3 || e != 2 {
		t.Fatalf("après fusion : %d concepts / %d relations, attendu 3 / 2", n, e)
	}
	if _, err := os.Stat(rep.BackupPath); err != nil {
		t.Fatalf("sauvegarde absente : %v", err)
	}

	out, err := s.Describe(ctx, "base", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "stockage relationnel et documents") {
		t.Errorf("description la plus riche perdue :\n%s", out)
	}

	// Le nom fusionné doit désormais retomber sur le concept conservé.
	st, err := s.Apply(ctx, Extraction{Entities: []Entity{{Name: "bases de donnees"}, {Name: "Base de données"}}})
	if err != nil {
		t.Fatal(err)
	}
	if st.NewEntities != 0 {
		t.Errorf("alias ignoré : %+v", st)
	}
	// Idempotent.
	rep, _ = s.Clean(ctx, CleanOptions{Apply: true})
	if rep.Changed() {
		t.Errorf("second nettoyage non vide : %+v", rep)
	}
}

func TestCleanOrphans(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustApply(t, s, Extraction{
		Entities:  []Entity{{Name: "Vieux"}, {Name: "Récent"}, {Name: "Cité"}, {Name: "Lié"}, {Name: "Autre"}},
		Relations: []Relation{{Source: "Lié", Relation: "utilise", Target: "Autre"}},
	})
	mustApply(t, s, Extraction{Entities: []Entity{{Name: "Cité"}}}) // mentions = 2

	old := time.Now().Add(-200 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for _, name := range []string{"Vieux", "Cité", "Lié"} {
		n, err := s.findByKey(ctx, entityKey(name))
		if err != nil || n == nil {
			t.Fatal(name, err)
		}
		n.Properties["last_seen"] = old
		if err := s.g.UpdateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}

	if rep, _ := s.Clean(ctx, CleanOptions{Apply: true}); rep.Changed() {
		t.Fatalf("purge sans OrphanMaxAge : %+v", rep)
	}
	rep, err := s.Clean(ctx, CleanOptions{Apply: true, OrphanMaxAge: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Orphans) != 1 || rep.Orphans[0] != "Vieux" {
		t.Fatalf("orphelins = %v, attendu [Vieux] (Récent est frais, Cité est répété, Lié a un lien)", rep.Orphans)
	}
	if n, _, _ := s.Counts(ctx); n != 4 {
		t.Fatalf("%d concepts, attendu 4", n)
	}
}

func TestCleanRelations(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustApply(t, s, Extraction{Relations: []Relation{
		{Source: "A", Relation: "uses", Target: "B"},
		{Source: "A", Relation: "se sert de", Target: "C"},
	}})
	// Une extraction ultérieure utilise un synonyme : Apply le canonise déjà.
	mustApply(t, s, Extraction{Relations: []Relation{{Source: "A", Relation: "utilise", Target: "C"}}})
	// Base héritée : relation non canonique et doublon posés directement.
	a, _ := s.findByKey(ctx, "a")
	b, _ := s.findByKey(ctx, "b")
	for _, typ := range []string{"EMPLOIE", "UTILISE"} {
		if err := s.g.CreateEdge(ctx, &graph.Edge{SourceID: a.ID, TargetID: b.ID, Type: typ}); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := s.Clean(ctx, CleanOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.RelationsRenamed != 1 || rep.EdgesRemoved != 1 {
		t.Fatalf("rapport = %+v", rep)
	}
	if _, e, _ := s.Counts(ctx); e != 3 { // A-USES-B, A-UTILISE-B, A-UTILISE-C
		t.Fatalf("%d relations, attendu 3", e)
	}
}

func TestCandidatesAndDecisions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustApply(t, s, Extraction{Entities: []Entity{
		{Name: "Synapse"}, {Name: "Projet Synapse"}, {Name: "Postgresql"}, {Name: "Postgresqk"}, {Name: "Redis"},
	}})
	rep, err := s.Clean(ctx, CleanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Merges) != 0 || len(rep.Candidates) != 2 {
		t.Fatalf("rapport = %+v", rep)
	}
	ops := mergesFromDecisions(rep.Candidates, []judgeDecision{
		{N: 1, Same: true, Keep: "B"},
		{N: 2, Same: false},
		{N: 9, Same: true, Keep: "A"}, // numéro inconnu
	})
	if len(ops) != 1 || ops[0].Keep != rep.Candidates[0].B.Name {
		t.Fatalf("ops = %+v", ops)
	}
	if n, err := s.ApplyMerges(ctx, ops); err != nil || n != 1 {
		t.Fatalf("ApplyMerges = %d, %v", n, err)
	}
}

func TestBackupRotation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustApply(t, s, Extraction{Entities: []Entity{{Name: "X"}}})
	for i := 0; i < keepBackups+2; i++ {
		if _, err := s.backup(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	got, _ := filepath.Glob(s.path + ".bak-*")
	if len(got) != keepBackups {
		t.Fatalf("%d sauvegardes gardées, attendu %d", len(got), keepBackups)
	}
}

// Un concept mis à jour (2e mention) doit rester retrouvable : sinon la 3e
// mention en créait un doublon.
func TestRepeatedMentionsDoNotDuplicate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		mustApply(t, s, Extraction{Entities: []Entity{{Name: "Synapse", Type: "Projet"}}})
	}
	if n, _, _ := s.Counts(ctx); n != 1 {
		t.Fatalf("%d concepts après 4 mentions, attendu 1", n)
	}
}

func TestCleanRepairsLostLabel(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustApply(t, s, Extraction{Entities: []Entity{{Name: "Synapse"}}})
	n, _ := s.findByKey(ctx, "synapse")
	if err := s.g.RemoveLabels(ctx, n.ID, conceptLabel); err != nil { // état laissé par l'ancien bug
		t.Fatal(err)
	}
	if got, _ := s.findByKey(ctx, "synapse"); got != nil {
		t.Fatal("préparation : le concept devrait être introuvable")
	}
	rep, err := s.Clean(ctx, CleanOptions{Apply: true})
	if err != nil || rep.LabelsRepaired != 1 {
		t.Fatalf("rapport = %+v, %v", rep, err)
	}
	if got, _ := s.findByKey(ctx, "synapse"); got == nil {
		t.Fatal("concept toujours introuvable après réparation")
	}
}

func TestExploreAndRenderSVG(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustApply(t, s, Extraction{
		Entities: []Entity{{Name: "Synapse <Med>", Type: "Projet", Description: "a & b"}, {Name: "Seul"}},
		Relations: []Relation{
			{Source: "Synapse <Med>", Relation: "utilise", Target: "Go"},
			{Source: "Go", Relation: "utilise", Target: "SQLite"},
			{Source: "SQLite", Relation: "utilise", Target: "WAL"},
		},
	})
	sg, err := s.Explore(ctx, "synapse", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sg.Nodes) != 3 || len(sg.Edges) != 2 { // Synapse, Go, SQLite (WAL à 3 sauts)
		t.Fatalf("sous-graphe = %d nœuds / %d liens", len(sg.Nodes), len(sg.Edges))
	}
	all, _ := s.Explore(ctx, "", 2, 0)
	if len(all.Nodes) != 4 { // "Seul", sans lien, est omis
		t.Fatalf("vue globale : %d nœuds, attendu 4", len(all.Nodes))
	}
	if capped, _ := s.Explore(ctx, "", 2, 2); len(capped.Nodes) != 2 || !capped.Truncated {
		t.Fatalf("plafond non appliqué : %+v", capped)
	}
	svg := string(RenderSVG(sg, "Test"))
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(strings.TrimSpace(svg), "</svg>") {
		t.Fatalf("SVG mal formé : %.80s", svg)
	}
	if strings.Contains(svg, "<Med>") || !strings.Contains(svg, "Synapse &lt;Med&gt;") {
		t.Error("noms non échappés")
	}
	if none, _ := s.Explore(ctx, "inconnu", 1, 0); len(none.Nodes) != 0 {
		t.Error("requête sans correspondance : attendu vide")
	}
}

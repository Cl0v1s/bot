package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"bot/internal/ontology"
)

func TestQueryOntologyTool(t *testing.T) {
	s, err := ontology.Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.Apply(context.Background(), ontology.Extraction{
		Relations: []ontology.Relation{{Source: "Synapse", Relation: "appartient à", Target: "Medicine"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tool := &QueryOntologyTool{Store: s}
	out, err := tool.Call(context.Background(), `{"query":"synapse","depth":1}`)
	if err != nil || !strings.Contains(out, "--APPARTIENT_À--> Medicine") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if _, err := tool.Call(context.Background(), `{"inconnu":1}`); err == nil {
		t.Error("argument inconnu devrait être refusé")
	}
	if !NewRegistry(tool).Has(QueryOntologyToolName) {
		t.Error("Has")
	}
}

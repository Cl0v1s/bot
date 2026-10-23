package ontology

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bot/internal/llm"
)

func TestDistillerRun(t *testing.T) {
	var sawPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sawPrompt = string(body)
		content, _ := json.Marshal(`{"entities":[{"name":"Synapse","type":"Projet","description":"d"}],"relations":[{"source":"Synapse","relation":"appartient à","target":"Medicine"}]}`)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":` + string(content) + `},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := openTest(t)
	d := &Distiller{Store: s, Client: llm.New(srv.URL, "k", "m")}
	ctx := context.Background()

	// Rien d'analysable : aucun appel LLM.
	if st, err := d.Run(ctx, []llm.Message{{Role: "tool", Content: "x"}}); err != nil || !st.Empty() || sawPrompt != "" {
		t.Fatalf("st=%+v err=%v prompt=%q", st, err, sawPrompt)
	}

	st, err := d.Run(ctx, []llm.Message{{Role: "user", Content: "Le projet Synapse appartient au groupe Medicine"}})
	if err != nil {
		t.Fatal(err)
	}
	if st.NewEntities != 2 || st.NewRelations != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if !strings.Contains(sawPrompt, "projet Synapse") {
		t.Errorf("la conversation devrait être dans le prompt: %s", sawPrompt)
	}

	// Le second passage fournit les noms connus au modèle.
	if _, err := d.Run(ctx, []llm.Message{{Role: "user", Content: "encore"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sawPrompt, "Concepts déjà connus") {
		t.Errorf("noms connus absents du prompt: %s", sawPrompt)
	}
}

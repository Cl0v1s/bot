package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bot/internal/convo"
	"bot/internal/llm"
)

// fakeStreamingServer répond en streaming SSE avec "ack:" puis le contenu du
// dernier message utilisateur, avec un délai entre les deux morceaux : assez
// long pour que, dans TestRun_QueuesLinesTypedWhileBusy, la ligne suivante ait
// le temps d'être tapée (lue depuis in) pendant que ce tour est encore en
// cours.
func fakeStreamingServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		last := ""
		if len(req.Messages) > 0 {
			last = req.Messages[len(req.Messages)-1].Content
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter ne supporte pas le flush (requis pour le streaming)")
		}

		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ack:\"}}]}\n\n")
		flusher.Flush()
		time.Sleep(150 * time.Millisecond)
		chunk, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"delta": map[string]string{"content": last}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

// TestRun_QueuesLinesTypedWhileBusy vérifie le comportement central de la
// file d'attente : une ligne tapée pendant qu'un tour est en cours n'est ni
// perdue ni traitée immédiatement, mais mise en file et traitée
// automatiquement, dans l'ordre, une fois ce tour terminé.
func TestRun_QueuesLinesTypedWhileBusy(t *testing.T) {
	srv := fakeStreamingServer(t)
	defer srv.Close()

	client := llm.New(srv.URL, "", "test-model")
	conv := convo.New("", 100000, 0.9, 10)

	// "second" et "/exit" arrivent depuis in quasi instantanément (pas de
	// vraie latence de terminal) : largement avant la fin du délai de 150ms
	// du premier tour, donc forcément mis en file.
	in := strings.NewReader("premier\nsecond\n/exit\n")
	var out bytes.Buffer

	runErr := make(chan error, 1)
	go func() { runErr <- Run(context.Background(), client, conv, ToolsConfig{}, in, &out) }()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run n'a pas terminé à temps (deadlock ?)")
	}

	// La correction du contenu échangé se vérifie sur conv, pas sur le texte
	// brut affiché : les écritures de plusieurs tours/messages peuvent
	// s'entrelacer dans out (voir le commentaire de Run sur "tapez ahead"),
	// alors que conv.Messages reflète l'état final, non ambigu.
	wantRoles := []string{"user", "assistant", "user", "assistant"}
	if len(conv.Messages) != len(wantRoles) {
		t.Fatalf("attendu %d messages (2 tours), got %d: %+v", len(wantRoles), len(conv.Messages), conv.Messages)
	}
	for i, role := range wantRoles {
		if conv.Messages[i].Role != role {
			t.Errorf("message %d: rôle=%q, attendu %q", i, conv.Messages[i].Role, role)
		}
	}
	if conv.Messages[0].Content != "premier" || conv.Messages[1].Content != "ack:premier" {
		t.Errorf("premier tour incorrect: %+v / %+v", conv.Messages[0], conv.Messages[1])
	}
	if conv.Messages[2].Content != "second" || conv.Messages[3].Content != "ack:second" {
		t.Errorf("second tour (mis en file) incorrect: %+v / %+v", conv.Messages[2], conv.Messages[3])
	}

	if !strings.Contains(out.String(), "mis en file d'attente") {
		t.Errorf("attendu un message de mise en file d'attente dans la sortie, got: %q", out.String())
	}
}

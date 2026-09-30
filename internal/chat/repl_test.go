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

// fakeMixedServer répond en streaming SSE (comme fakeStreamingServer) pour
// une requête "stream":true (un tour de chat normal), et en JSON classique
// pour une requête non-streaming (les appels de résumé/extraction mémoire
// de convo.Conversation.Compact) — distingués entre eux par une sous-chaîne
// caractéristique de leur system prompt respectif.
func fakeMixedServer(t *testing.T, summaryReply, memoryReply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Fatal("ResponseWriter ne supporte pas le flush")
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ack\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		sys := ""
		if len(req.Messages) > 0 {
			sys = req.Messages[0].Content
		}
		reply := summaryReply
		if strings.Contains(sys, "mémoire") {
			reply = memoryReply
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": reply}}},
		})
	}))
}

// /compact doit forcer une compaction immédiate (résumé + extraction
// mémoire), sans attendre que le seuil normal soit atteint.
func TestRun_CompactCommandForcesCompaction(t *testing.T) {
	server := fakeMixedServer(t, "résumé forcé du contexte", "RIEN")
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	// KeepLast=0 : tout l'historique existant est compactable, sans devoir
	// atteindre le seuil normal (CompactAt) au préalable.
	conv := convo.New("sys", 100000, 0.9, 0)
	conv.AddUser("premier message")
	conv.AddAssistant("première réponse")

	in := strings.NewReader("/compact\n/exit\n")
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

	if len(conv.Messages) != 1 || conv.Messages[0].Role != "system" {
		t.Fatalf("attendu un historique compacté à 1 message système, got %+v", conv.Messages)
	}
	if !strings.Contains(conv.Messages[0].Content, "résumé forcé du contexte") {
		t.Fatalf("message compacté = %q, attendu qu'il contienne le résumé", conv.Messages[0].Content)
	}
	if !strings.Contains(out.String(), "contexte compacté manuellement") {
		t.Errorf("attendu la confirmation de compaction manuelle dans la sortie, got: %q", out.String())
	}
}

// /compact sur un historique trop court pour être compacté (rien à
// résumer) doit le dire clairement, pas planter ni bloquer.
func TestRun_CompactCommandOnEmptyHistory(t *testing.T) {
	server := fakeMixedServer(t, "résumé forcé du contexte", "RIEN")
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	conv := convo.New("sys", 100000, 0.9, 6) // KeepLast=6, aucun message : rien à compacter

	in := strings.NewReader("/compact\n/exit\n")
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

	if !strings.Contains(out.String(), "rien à compacter") {
		t.Errorf("attendu un message \"rien à compacter\", got: %q", out.String())
	}
}

// runQueued exécute Run sur input (lignes arrivant toutes pendant le premier
// tour, donc mises en file) et retourne la conversation finale.
func runQueued(t *testing.T, input string) (*convo.Conversation, string) {
	t.Helper()
	srv := fakeStreamingServer(t)
	defer srv.Close()
	client := llm.New(srv.URL, "", "test-model")
	conv := convo.New("", 100000, 0.9, 10)
	var out bytes.Buffer
	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(context.Background(), client, conv, ToolsConfig{}, strings.NewReader(input), &out)
	}()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run n'a pas terminé à temps (deadlock ?)")
	}
	return conv, out.String()
}

func userContents(conv *convo.Conversation) []string {
	var got []string
	for _, m := range conv.Messages {
		if m.Role == "user" {
			got = append(got, m.Content)
		}
	}
	return got
}

// Plusieurs messages en file d'attente sont envoyés en un seul tour.
func TestRun_MergesQueuedMessages(t *testing.T) {
	conv, out := runQueued(t, "premier\nsecond\ntroisième\n/exit\n")
	want := []string{"premier", "second\n\ntroisième"}
	if got := userContents(conv); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("messages envoyés = %q, attendu %q", got, want)
	}
	if !strings.Contains(out, "2 messages en attente fusionnés") {
		t.Errorf("attendu l'annonce de la fusion dans la sortie, got: %q", out)
	}
}

// Une commande en file sépare les messages : jamais de fusion par-dessus.
func TestRun_QueuedCommandSplitsMerge(t *testing.T) {
	conv, _ := runQueued(t, "premier\nA\nB\n/stats\nC\n/exit\n")
	want := []string{"premier", "A\n\nB", "C"}
	if got := userContents(conv); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("messages envoyés = %q, attendu %q", got, want)
	}
}

// Les transcriptions vocales arrivent avec majuscule et ponctuation.
func TestIsYes(t *testing.T) {
	for _, s := range []string{"o", "Oui.", " oui ! ", "Ouais", "yes", "Y"} {
		if !isYes(s) {
			t.Errorf("isYes(%q) = false", s)
		}
	}
	for _, s := range []string{"", "non", "Non.", "oui mais non", "n", "peut-être"} {
		if isYes(s) {
			t.Errorf("isYes(%q) = true", s)
		}
	}
}

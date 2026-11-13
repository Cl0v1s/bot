package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRepeatTail(t *testing.T) {
	unit := "Wait, \"prepore\" ...\nWhat if \"prepore\" is \"pré-procure\"?\nNo.\n"
	text := "Début normal de réflexion. " + strings.Repeat(unit, 10)
	cut, ok := repeatTail(text)
	if !ok {
		t.Fatal("boucle non détectée")
	}
	got := text[:len(text)-cut]
	if !strings.HasPrefix(got, "Début normal de réflexion. Wait") || strings.Count(got, "No.") > 2 {
		t.Fatalf("troncature inattendue: %q", got)
	}
	for _, s := range []string{
		strings.Repeat("a", 100),
		"| a | b |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |\n",
		strings.Repeat("ligne différente numéro 1\n", 1) + "autre texte assez long pour dépasser le seuil " + strings.Repeat("x", 50),
	} {
		if _, ok := repeatTail(s); ok {
			t.Errorf("faux positif: %q", s)
		}
	}
}

func TestStreamLoopCut(t *testing.T) {
	unit := "Wait, maybe it is something else entirely? No.\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for i := 0; i < 2000; i++ {
			if r.Context().Err() != nil {
				return
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q}}]}\n\n", unit)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New(srv.URL, "", "m")
	msg, _, err := c.ChatCompletionStream(context.Background(), nil, nil, StreamCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.FinishReason != FinishReasonLoop {
		t.Fatalf("finish_reason = %q", msg.FinishReason)
	}
	if n := strings.Count(msg.ReasoningContent, "No."); n > 2 {
		t.Fatalf("répétition non retirée: %d occurrences", n)
	}
}

func TestStreamLoopRecovery(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fl := w.(http.Flusher)
		if calls == 1 {
			for i := 0; i < 2000 && r.Context().Err() == nil; i++ {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q}}]}\n\n", "Wait, maybe it is something else entirely? No.\n")
				fl.Flush()
			}
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c := New(srv.URL, "", "m")
	notified := 0
	ctx := WithRetryNotifier(context.Background(), func(int, int, error) { notified++ })
	msg, _, err := c.ChatCompletionStream(ctx, []Message{{Role: "user", Content: "hi"}}, nil, StreamCallbacks{})
	if err != nil || msg.Content != "ok" || msg.FinishReason == FinishReasonLoop || notified != 1 {
		t.Fatalf("msg=%+v err=%v notified=%d", msg, err, notified)
	}
}

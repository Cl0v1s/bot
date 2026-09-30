package llm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newRetryTestClient(url string) *Client {
	c := New(url+"/v1", "", "m")
	c.RetryBackoff = time.Millisecond
	return c
}

// Flux coupé en pleine génération (connexion fermée sans "[DONE]" ni
// finish_reason) : la requête est rejouée, le notifier est prévenu avant la
// relance, et la réponse retournée est celle de la relance seule — pas une
// concaténation avec le texte partiel du premier essai.
func TestChatCompletionStreamRetriesOnTruncatedStream(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partiel\"}}]}\n\n")
		if calls == 1 {
			return // fin de flux prématurée
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" complet\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var notified []int
	ctx := WithRetryNotifier(context.Background(), func(attempt, max int, err error) {
		notified = append(notified, attempt)
	})
	var streamed string
	msg, _, err := newRetryTestClient(srv.URL).ChatCompletionStream(ctx, []Message{{Role: "user", Content: "salut"}}, nil, StreamCallbacks{
		OnContent: func(d string) { streamed += d },
	})
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	if msg.Content != "partiel complet" {
		t.Fatalf("content = %q, attendu %q", msg.Content, "partiel complet")
	}
	if calls != 2 || len(notified) != 1 || notified[0] != 1 {
		t.Fatalf("appels = %d, notifications = %v ; attendu 2 appels et une notification", calls, notified)
	}
	if streamed != "partielpartiel complet" {
		t.Fatalf("fragments transmis = %q", streamed)
	}
}

// Connexion coupée brutalement par le serveur en plein flux (EOF inattendu
// côté client) : relancée elle aussi.
func TestChatCompletionStreamRetriesOnConnectionDrop(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n")
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	msg, _, err := newRetryTestClient(srv.URL).ChatCompletionStream(context.Background(), []Message{{Role: "user", Content: "salut"}}, nil, StreamCallbacks{})
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	if msg.Content != "ok" || calls != 2 {
		t.Fatalf("content = %q, appels = %d", msg.Content, calls)
	}
}

// Serveur indisponible (503, ex: modèle en cours de chargement) puis
// disponible : ChatCompletion finit par réussir.
func TestChatCompletionRetriesOnUnavailable(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"Loading model"}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"bonjour"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	msg, _, err := newRetryTestClient(srv.URL).ChatCompletion(context.Background(), []Message{{Role: "user", Content: "salut"}}, nil)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if msg.Content != "bonjour" || calls != 3 {
		t.Fatalf("content = %q, appels = %d", msg.Content, calls)
	}
}

// Serveur injoignable (connexion refusée) : autant de relances que
// MaxRetries, puis l'erreur d'origine est remontée.
func TestChatCompletionGivesUpAfterMaxRetries(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	c := newRetryTestClient("http://" + addr)
	c.MaxRetries = 2
	notified := 0
	ctx := WithRetryNotifier(context.Background(), func(int, int, error) { notified++ })
	if _, _, err := c.ChatCompletion(ctx, []Message{{Role: "user", Content: "salut"}}, nil); err == nil {
		t.Fatal("erreur attendue")
	}
	if notified != 2 {
		t.Fatalf("notifications = %d, attendu 2", notified)
	}
}

// Une erreur non transitoire (400) n'est jamais relancée.
func TestChatCompletionDoesNotRetryClientError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"bad request"}}`)
	}))
	defer srv.Close()

	if _, _, err := newRetryTestClient(srv.URL).ChatCompletion(context.Background(), []Message{{Role: "user", Content: "salut"}}, nil); err == nil {
		t.Fatal("erreur attendue")
	}
	if calls != 1 {
		t.Fatalf("appels = %d, attendu 1", calls)
	}
}

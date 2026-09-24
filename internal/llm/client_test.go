package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Reproduit le cas réel signalé : un serveur d'inférence local (ex: Unsloth
// Studio) répond "No model loaded. Call POST /inference/load first." tant
// qu'aucun modèle n'est en mémoire — ChatCompletion doit charger le modèle
// (via /load) puis rejouer la requête, sans jamais remonter cette erreur à
// l'appelant si le rechargement + la relance réussissent.
func TestChatCompletionAutoLoadsModelOnNoModelLoadedError(t *testing.T) {
	var chatCalls, loadCalls int
	var gotLoadBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			chatCalls++
			if chatCalls == 1 {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"error":{"message":"No model loaded. Call POST /inference/load first."}}`))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"bonjour"}}]}`))
		case "/v1/load":
			loadCalls++
			_ = json.NewDecoder(r.Body).Decode(&gotLoadBody)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Fatalf("chemin inattendu: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "", "org/modele:UD-Q4_K_XL")
	client.ContextTokens = 65000

	msg, _, err := client.ChatCompletion(context.Background(), []Message{{Role: "user", Content: "salut"}}, nil)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if msg.Content != "bonjour" {
		t.Fatalf("content = %q, attendu %q", msg.Content, "bonjour")
	}
	if chatCalls != 2 {
		t.Fatalf("appels /chat/completions = %d, attendu 2 (échec puis relance réussie)", chatCalls)
	}
	if loadCalls != 1 {
		t.Fatalf("appels /load = %d, attendu 1", loadCalls)
	}
	if gotLoadBody["model_path"] != "org/modele" {
		t.Fatalf("model_path envoyé à /load = %v, attendu %q (sans le suffixe quant)", gotLoadBody["model_path"], "org/modele")
	}
	if gotLoadBody["gguf_variant"] != "UD-Q4_K_XL" {
		t.Fatalf("gguf_variant envoyé à /load = %v, attendu %q", gotLoadBody["gguf_variant"], "UD-Q4_K_XL")
	}
	if gotLoadBody["max_seq_length"] != float64(65000) {
		t.Fatalf("max_seq_length envoyé à /load = %v, attendu 65000 (ContextTokens)", gotLoadBody["max_seq_length"])
	}
}

// Si le modèle n'a pas de suffixe ":quant", /load ne doit recevoir aucun
// "gguf_variant" (le serveur doit alors choisir lui-même le quant) plutôt
// qu'une chaîne vide envoyée explicitement.
func TestChatCompletionAutoLoadOmitsVariantWhenModelHasNone(t *testing.T) {
	var gotLoadBody map[string]any
	chatCalls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			chatCalls++
			if chatCalls == 1 {
				_, _ = w.Write([]byte(`{"error":{"message":"No model loaded. Call POST /inference/load first."}}`))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		case "/v1/load":
			_ = json.NewDecoder(r.Body).Decode(&gotLoadBody)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "", "org/modele-sans-quant")
	if _, _, err := client.ChatCompletion(context.Background(), []Message{{Role: "user", Content: "salut"}}, nil); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if _, present := gotLoadBody["gguf_variant"]; present {
		t.Fatalf("gguf_variant présent (%v) alors que le modèle ne porte aucun suffixe quant", gotLoadBody["gguf_variant"])
	}
	if gotLoadBody["model_path"] != "org/modele-sans-quant" {
		t.Fatalf("model_path = %v, attendu %q", gotLoadBody["model_path"], "org/modele-sans-quant")
	}
}

// Si le chargement automatique échoue à son tour (serveur sans cet endpoint,
// modèle introuvable...), l'erreur d'ORIGINE ("aucun modèle chargé") doit
// être remontée telle quelle, pas masquée derrière l'échec du chargement —
// et surtout pas de boucle de relance infinie.
func TestChatCompletionFallsBackToOriginalErrorWhenAutoLoadFails(t *testing.T) {
	chatCalls, loadCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			chatCalls++
			_, _ = w.Write([]byte(`{"error":{"message":"No model loaded. Call POST /inference/load first."}}`))
		case "/v1/load":
			loadCalls++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"not found"}`))
		}
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "", "org/modele")
	_, _, err := client.ChatCompletion(context.Background(), []Message{{Role: "user", Content: "salut"}}, nil)
	if err == nil {
		t.Fatal("attendu une erreur (chargement automatique et requête d'origine tous deux en échec)")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "no model loaded") {
		t.Fatalf("erreur = %q, attendu qu'elle contienne l'erreur d'origine (no model loaded)", err.Error())
	}
	if chatCalls != 1 {
		t.Fatalf("appels /chat/completions = %d, attendu 1 (pas de relance après un échec du chargement)", chatCalls)
	}
	if loadCalls != 1 {
		t.Fatalf("appels /load = %d, attendu 1 (pas de boucle)", loadCalls)
	}
}

// Une erreur normale (rien à voir avec "aucun modèle chargé") ne doit
// déclencher aucune tentative de chargement automatique.
func TestChatCompletionDoesNotAutoLoadOnUnrelatedError(t *testing.T) {
	loadCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"error":{"message":"context length exceeded"}}`))
		case "/v1/load":
			loadCalls++
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "", "org/modele")
	if _, _, err := client.ChatCompletion(context.Background(), []Message{{Role: "user", Content: "salut"}}, nil); err == nil {
		t.Fatal("attendu une erreur")
	}
	if loadCalls != 0 {
		t.Fatalf("appels /load = %d, attendu 0 (erreur sans rapport avec l'absence de modèle chargé)", loadCalls)
	}
}

// Même mécanisme côté streaming.
func TestChatCompletionStreamAutoLoadsModel(t *testing.T) {
	chatCalls, loadCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			chatCalls++
			if chatCalls == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"message":"No model loaded. Call POST /inference/load first."}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"bon\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"jour\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/v1/load":
			loadCalls++
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "", "org/modele")
	var got strings.Builder
	full, _, err := client.ChatCompletionStream(context.Background(), []Message{{Role: "user", Content: "salut"}}, func(d string) { got.WriteString(d) })
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	if full != "bonjour" || got.String() != "bonjour" {
		t.Fatalf("texte = %q / onDelta = %q, attendu %q", full, got.String(), "bonjour")
	}
	if loadCalls != 1 {
		t.Fatalf("appels /load = %d, attendu 1", loadCalls)
	}
}

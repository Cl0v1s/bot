package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"bot/internal/convo"
	"bot/internal/llm"
	"bot/internal/tools"
)

// Bug critique observé en production : un modèle qui échappe systématiquement
// sa tentative d'appel d'outil en texte brut ("<tool_call>...") lors de
// l'appel de compaction (aucun outil pourtant proposé à cet appel) faisait
// échouer Compact à CHAQUE tour, et Run propageait cet échec comme une
// erreur fatale du tour entier — bloquant définitivement la conversation
// (chaque nouveau message retentait la même compaction, échouait de la même
// façon, sans qu'aucune réponse ne puisse plus jamais aboutir). La
// compaction est une optimisation, pas un prérequis : son échec doit être
// signalé (EventWarning) puis ignoré, jamais bloquant.
func TestRunSurvivesCompactionFailure(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := "réponse finale"
		if atomic.AddInt32(&calls, 1) == 1 {
			// Premier appel = la tentative de résumé de compaction.
			reply = "<tool_call>\n<function=run_shell>\n</function>\n</tool_call>"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": reply}}},
		})
	}))
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	// MaxContextTokens=1, CompactAt=0 : seuil de compaction toujours atteint,
	// pour que Run tente systématiquement de compacter avant de répondre.
	conv := convo.New("sys", 1, 0, 0)
	conv.AddUser("bonjour")

	var warnings []string
	reply, _, err := Run(context.Background(), client, conv, tools.NewRegistry(), 4, func(e Event) {
		if e.Kind == EventWarning {
			warnings = append(warnings, e.Result)
		}
	})
	if err != nil {
		t.Fatalf("Run: %v (la conversation ne doit jamais rester bloquée sur un échec de compaction)", err)
	}
	if reply != "réponse finale" {
		t.Fatalf("reply = %q, want %q", reply, "réponse finale")
	}
	if len(warnings) != 1 {
		t.Fatalf("attendu exactement un avertissement de compaction, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "échec de la compaction") {
		t.Fatalf("warning = %q, attendu qu'il mentionne l'échec de compaction", warnings[0])
	}
}

// Filet de sécurité : quand la compaction échoue ET que l'historique dépasse
// quand même MaxContextTokens, Run ne doit jamais envoyer une requête vouée
// à être rejetée par le serveur pour dépassement de contexte — il doit
// couper les plus vieux messages en dernier recours (EnsureFitsContext)
// avant l'appel de complétion final.
func TestRunTruncatesOversizedHistoryAfterCompactionFailure(t *testing.T) {
	var gotFinalRequestMessageCount int
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		reply := "réponse finale"
		if atomic.AddInt32(&calls, 1) == 1 {
			reply = "<tool_call><function=run_shell></function></tool_call>" // échec de compaction
		} else {
			gotFinalRequestMessageCount = len(req.Messages)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": reply}}},
		})
	}))
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	// MaxContextTokens minuscule : après l'échec de compaction, l'historique
	// (plusieurs gros messages) doit dépasser cette limite, déclenchant
	// EnsureFitsContext.
	conv := convo.New("sys", 50, 0, 0)
	for i := 0; i < 10; i++ {
		conv.AddUser(strings.Repeat("x", 100))
	}

	var warnings []string
	_, _, err := Run(context.Background(), client, conv, tools.NewRegistry(), 4, func(e Event) {
		if e.Kind == EventWarning {
			warnings = append(warnings, e.Result)
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if conv.EstimateTokens() > conv.MaxContextTokens {
		t.Fatalf("EstimateTokens()=%d toujours au-dessus de MaxContextTokens=%d après Run", conv.EstimateTokens(), conv.MaxContextTokens)
	}
	if gotFinalRequestMessageCount >= 11 { // system + 10 messages originaux, non tronqués
		t.Fatalf("la requête finale envoyée au serveur contient %d messages, attendu qu'elle ait été tronquée", gotFinalRequestMessageCount)
	}

	foundTruncationWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "supprimé") {
			foundTruncationWarning = true
		}
	}
	if !foundTruncationWarning {
		t.Fatalf("attendu un avertissement mentionnant la troncature, got %v", warnings)
	}
}

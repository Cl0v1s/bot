package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	reply, _, err := Run(context.Background(), client, conv, tools.NewRegistry(), 4, 0, func(e Event) {
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
	_, _, err := Run(context.Background(), client, conv, tools.NewRegistry(), 4, 0, func(e Event) {
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

// La fonctionnalité demandée : après N échecs run_shell d'affilée (ici
// N=2), Run doit injecter un message poussant le modèle à changer
// d'approche — avant que le modèle ne produise sa réponse finale, pas
// après. Utilise le vrai ShellTool (non sandboxé, "exit 1" échoue
// réellement) plutôt qu'un faux tool : le point exact vérifié est la
// détection par agent.shellCallFailed d'un vrai résultat de ShellTool.Call.
func TestRunInjectsNudgeAfterConsecutiveShellFailures(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var msg map[string]any
		if n <= 2 {
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":   "call" + string(rune('0'+n)),
					"type": "function",
					"function": map[string]any{
						"name":      "run_shell",
						"arguments": `{"command":"exit 1"}`,
					},
				}},
			}
		} else {
			msg = map[string]any{"role": "assistant", "content": "fini"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg}},
		})
	}))
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	conv := convo.New("sys", 100000, 0.9, 10)
	conv.AddUser("fais un truc qui échoue")

	registry := tools.NewRegistry(&tools.ShellTool{Timeout: 2 * time.Second})

	var warnings []string
	reply, _, err := Run(context.Background(), client, conv, registry, 5, 2, func(e Event) {
		if e.Kind == EventWarning {
			warnings = append(warnings, e.Result)
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "fini" {
		t.Fatalf("reply = %q, want %q", reply, "fini")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("appels API = %d, attendu exactement 3 (2 échecs run_shell + 1 réponse finale, le nudge ne doit pas déclencher d'étape en plus)", got)
	}

	foundNudge := false
	for _, m := range conv.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "2 appels run_shell") {
			foundNudge = true
		}
	}
	if !foundNudge {
		t.Fatalf("aucun message \"user\" de nudge trouvé dans conv.Messages: %+v", conv.Messages)
	}

	foundWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "run_shell") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatalf("attendu un EventWarning mentionnant run_shell, got %v", warnings)
	}
}

// maxConsecutiveShellFailures <= 0 doit désactiver la fonctionnalité :
// aucun message injecté, quel que soit le nombre d'échecs.
func TestRunDoesNotInjectNudgeWhenDisabled(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var msg map[string]any
		if n <= 5 {
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":       "call" + string(rune('0'+n)),
					"type":     "function",
					"function": map[string]any{"name": "run_shell", "arguments": `{"command":"exit 1"}`},
				}},
			}
		} else {
			msg = map[string]any{"role": "assistant", "content": "fini"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg}},
		})
	}))
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	conv := convo.New("sys", 100000, 0.9, 10)
	conv.AddUser("fais un truc qui échoue")
	registry := tools.NewRegistry(&tools.ShellTool{Timeout: 2 * time.Second})

	_, _, err := Run(context.Background(), client, conv, registry, 8, 0, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range conv.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "run_shell") && strings.Contains(m.Content, "harnais") {
			t.Fatalf("nudge injecté malgré maxConsecutiveShellFailures=0 (désactivé): %+v", conv.Messages)
		}
	}
}

// Un run_shell RÉUSSI entre deux échecs doit remettre le compteur à zéro :
// pas de nudge si les échecs ne sont pas consécutifs.
func TestRunResetsCounterOnShellSuccess(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var msg map[string]any
		switch {
		case n == 1, n == 3:
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":       "callf" + string(rune('0'+n)),
					"type":     "function",
					"function": map[string]any{"name": "run_shell", "arguments": `{"command":"exit 1"}`},
				}},
			}
		case n == 2:
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":       "calls2",
					"type":     "function",
					"function": map[string]any{"name": "run_shell", "arguments": `{"command":"true"}`},
				}},
			}
		default:
			msg = map[string]any{"role": "assistant", "content": "fini"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg}},
		})
	}))
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	conv := convo.New("sys", 100000, 0.9, 10)
	conv.AddUser("fais un truc")
	registry := tools.NewRegistry(&tools.ShellTool{Timeout: 2 * time.Second})

	// Seuil 2 : échec, succès, échec -> jamais 2 échecs D'AFFILÉE.
	_, _, err := Run(context.Background(), client, conv, registry, 8, 2, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range conv.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "harnais") {
			t.Fatalf("nudge injecté alors que les échecs n'étaient pas consécutifs (succès entre les deux): %+v", conv.Messages)
		}
	}
}

package convo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bot/internal/llm"
)

// Un message assistant porteur d'un tool_call a un Content vide : sans
// compter aussi Function.Arguments, ce message serait estimé à quasiment 0
// token quel que soit le volume réel de ses arguments (ex: un write_file
// avec un gros contenu), sous-estimant fortement le contexte pendant les
// tours à base d'outils.
func TestMessageEstimateTokensIncludesToolCallArguments(t *testing.T) {
	bigArgs := `{"path":"foo.txt","content":"` + strings.Repeat("x", 4000) + `"}`
	m := llm.Message{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{
			{Function: llm.ToolCallFunction{Name: "write_file", Arguments: bigArgs}},
		},
	}

	got := messageEstimateTokens(m)
	if got < 900 {
		t.Fatalf("messageEstimateTokens(%d octets d'arguments) = %d, attendu >= ~900 (arguments ignorés ?)", len(bigArgs), got)
	}
}

// EstimateTokens doit tenir compte des tool_calls des messages ajoutés
// depuis le dernier usage réel connu, pas seulement de leur Content.
func TestEstimateTokensAccountsForToolCallsSinceLastKnownUsage(t *testing.T) {
	c := New("", 8192, 0.9, 6)
	c.LastKnownTokens = 100
	c.LastKnownMessageCount = 0

	bigArgs := `{"command":"` + strings.Repeat("y", 4000) + `"}`
	c.AppendRaw(llm.Message{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{
			{Function: llm.ToolCallFunction{Name: "run_shell", Arguments: bigArgs}},
		},
	})

	got := c.EstimateTokens()
	if got < 1000 {
		t.Fatalf("EstimateTokens() = %d, attendu >= ~1000 (arguments du tool_call non comptés depuis le dernier usage connu ?)", got)
	}
}

// Compact ne doit jamais couper au milieu d'un groupe [assistant à
// tool_calls + ses résultats d'outils] : sinon, soit le message envoyé au
// LLM pour le résumé se termine par un assistant à tool_calls sans ses
// résultats (rejeté par le serveur : "Cannot continue an assistant message
// that contains tool calls"), soit l'historique conservé commence par un
// message "tool" orphelin (rejeté de la même façon au tour suivant).
func TestCompactNeverSplitsAToolCallGroup(t *testing.T) {
	var gotSummarizeReq []llm.Message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("décodage de la requête de résumé: %v", err)
		}
		gotSummarizeReq = req.Messages
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "résumé"}},
			},
		})
	}))
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	c := New("", 8192, 0.9, 1) // KeepLast=1 : la coupure naïve tombe en plein milieu du 2e groupe d'outils
	c.AddUser("bonjour")
	c.AppendRaw(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "t1", Function: llm.ToolCallFunction{Name: "run_shell", Arguments: `{"command":"a"}`}}}})
	c.AppendRaw(llm.Message{Role: "tool", ToolCallID: "t1", Content: "résultat 1"})
	c.AddAssistant("voilà le résultat de la première commande")
	c.AddUser("fais une autre commande")
	c.AppendRaw(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "t2", Function: llm.ToolCallFunction{Name: "run_shell", Arguments: `{"command":"b"}`}}}})
	c.AppendRaw(llm.Message{Role: "tool", ToolCallID: "t2", Content: "résultat 2"})

	compacted, err := c.Compact(context.Background(), client)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !compacted {
		t.Fatalf("Compact aurait dû compacter")
	}

	if len(gotSummarizeReq) == 0 {
		t.Fatalf("aucune requête de résumé envoyée")
	}
	last := gotSummarizeReq[len(gotSummarizeReq)-1]
	if last.Role == "assistant" && len(last.ToolCalls) > 0 {
		t.Fatalf("la requête de résumé se termine par un message assistant à tool_calls sans ses résultats : %+v", last)
	}

	if len(c.Messages) == 0 {
		t.Fatalf("historique conservé vide")
	}
	if c.Messages[0].Role == "tool" {
		t.Fatalf("l'historique conservé commence par un message \"tool\" orphelin : %+v", c.Messages[0])
	}
}

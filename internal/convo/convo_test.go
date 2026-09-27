package convo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bot/internal/llm"
)

// fakeCompactionServer route la réponse selon le system prompt reçu : celui
// de memoryMaintenanceSystemPrompt renvoie memoryReply, tout le reste (le
// résumé de compaction) renvoie summaryReply — pour pouvoir tester les deux
// appels séparément sans dépendre de leur ordre d'exécution.
func fakeCompactionServer(t *testing.T, summaryReply, memoryReply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("décodage de la requête: %v", err)
		}
		reply := summaryReply
		if len(req.Messages) > 0 && req.Messages[0].Content == memoryMaintenanceSystemPrompt {
			reply = memoryReply
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": reply}}},
		})
	}))
}

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
// tool_calls + ses résultats d'outils] : l'historique CONSERVÉ (pas résumé)
// commencerait alors par un message "tool" orphelin, rejeté par l'API au
// tour suivant ("Cannot continue an assistant message that contains tool
// calls", faute du message assistant qui précède normalement un rôle
// "tool"). Le message envoyé pour le résumé lui-même n'a plus cette
// contrainte (voir serializeForSummary : converti en texte, jamais rejoué
// avec de vrais rôles) — seul l'historique conservé y reste soumis.
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

	if len(gotSummarizeReq) != 2 || gotSummarizeReq[1].Role != "user" {
		t.Fatalf("requête de résumé = %+v, attendu exactement [system, user] (voir serializeForSummary)", gotSummarizeReq)
	}
	if !strings.Contains(gotSummarizeReq[1].Content, "résultat 1") {
		t.Fatalf("le journal envoyé pour résumé ne contient pas le premier groupe d'outils coupé : %q", gotSummarizeReq[1].Content)
	}

	if len(c.Messages) == 0 {
		t.Fatalf("historique conservé vide")
	}
	if c.Messages[0].Role == "tool" {
		t.Fatalf("l'historique conservé commence par un message \"tool\" orphelin : %+v", c.Messages[0])
	}
}

// Quand MemoryFile est configuré et que la maintenance mémoire produit un
// contenu à garder, Compact doit l'écrire dans le fichier — sans toucher au
// résumé de compaction, produit par un appel séparé.
func TestCompactPersistsMemoryWhenSomethingWorthKeeping(t *testing.T) {
	server := fakeCompactionServer(t, "résumé de la conversation", "- préfère les réponses en français")
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	memoryFile := filepath.Join(t.TempDir(), "MEMORY.md")
	c := New("", 8192, 0.9, 0)
	c.MemoryFile = memoryFile
	c.AddUser("bonjour")
	c.AddAssistant("salut")

	if _, err := c.Compact(context.Background(), client); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	data, err := os.ReadFile(memoryFile)
	if err != nil {
		t.Fatalf("lecture de %q: %v", memoryFile, err)
	}
	// Remplacement intégral du fichier par la réponse du modèle (voir
	// persistMemory), pas un ajout dans un gabarit : contenu attendu
	// exactement égal, pas seulement "contient".
	if got := strings.TrimSpace(string(data)); got != "- préfère les réponses en français" {
		t.Fatalf("MEMORY.md = %q, attendu exactement le contenu renvoyé par la maintenance mémoire", got)
	}
	if !strings.Contains(c.Messages[0].Content, "résumé de la conversation") {
		t.Fatalf("résumé de compaction = %q, attendu le résumé (pas la maintenance mémoire)", c.Messages[0].Content)
	}
}

// persistMemory doit transmettre le contenu déjà présent dans MEMORY.md au
// modèle (voir memoryMaintenanceSystemPrompt) — c'est ce qui lui permet de
// juger quoi garder, fusionner ou retirer, pas seulement d'éviter un
// doublon exact avec ce qu'il ajouterait.
func TestCompactPassesExistingMemoryForMaintenance(t *testing.T) {
	var gotMemoryReqUser string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("décodage de la requête: %v", err)
		}
		reply := "résumé"
		if len(req.Messages) > 0 && req.Messages[0].Content == memoryMaintenanceSystemPrompt {
			gotMemoryReqUser = req.Messages[1].Content
			reply = memoryNothingSentinel
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": reply}}},
		})
	}))
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	memoryFile := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(memoryFile, []byte("## Préférences\n- préfère les réponses en français\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := New("", 8192, 0.9, 0)
	c.MemoryFile = memoryFile
	c.AddUser("bonjour")
	c.AddAssistant("salut")

	if _, err := c.Compact(context.Background(), client); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if !strings.Contains(gotMemoryReqUser, "préfère les réponses en français") {
		t.Fatalf("requête de maintenance mémoire = %q, attendu qu'elle contienne le contenu déjà enregistré", gotMemoryReqUser)
	}
}

// Le cœur du changement demandé : la maintenance mémoire doit pouvoir
// RETIRER une entrée existante devenue inutile, pas seulement ajouter sans
// dupliquer — remplace tout le fichier par ce que le modèle renvoie, y
// compris quand ça laisse de côté une partie de ce qui existait avant.
func TestCompactMemoryMaintenanceCanPruneExistingContent(t *testing.T) {
	// Le modèle ne garde qu'UNE des deux entrées existantes : simule une
	// vraie révision qui retire quelque chose devenu obsolète/inutile.
	server := fakeCompactionServer(t, "résumé", "- préfère les réponses en français")
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	memoryFile := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(memoryFile, []byte(
		"- préfère les réponses en français\n- détail obsolète d'une tâche ponctuelle passée, à retirer\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	c := New("", 8192, 0.9, 0)
	c.MemoryFile = memoryFile
	c.AddUser("bonjour")
	c.AddAssistant("salut")

	if _, err := c.Compact(context.Background(), client); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	data, err := os.ReadFile(memoryFile)
	if err != nil {
		t.Fatalf("lecture de %q: %v", memoryFile, err)
	}
	if strings.Contains(string(data), "obsolète") {
		t.Fatalf("MEMORY.md = %q, attendu que l'entrée obsolète ait été retirée par la maintenance", data)
	}
	if !strings.Contains(string(data), "préfère les réponses en français") {
		t.Fatalf("MEMORY.md = %q, attendu que l'entrée toujours utile soit conservée", data)
	}
}

// Une mémoire existante entièrement vidée par la maintenance (sentinel) doit
// se traduire par un fichier vide, pas laisser l'ancien contenu en place.
func TestCompactMemoryMaintenanceCanEmptyExistingFile(t *testing.T) {
	server := fakeCompactionServer(t, "résumé", memoryNothingSentinel)
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	memoryFile := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(memoryFile, []byte("- plus rien d'utile ici\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := New("", 8192, 0.9, 0)
	c.MemoryFile = memoryFile
	c.AddUser("bonjour")
	c.AddAssistant("salut")

	if _, err := c.Compact(context.Background(), client); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	data, err := os.ReadFile(memoryFile)
	if err != nil {
		t.Fatalf("lecture de %q: %v", memoryFile, err)
	}
	if strings.TrimSpace(string(data)) != "" {
		t.Fatalf("MEMORY.md = %q, attendu vide (sentinel \"rien à garder\")", data)
	}
}

// Quand l'appel d'extraction ne trouve rien à retenir (sentinel
// memoryNothingSentinel), Compact ne doit rien écrire du tout — y compris
// ne pas créer le fichier s'il n'existait pas encore.
func TestCompactWritesNothingWhenExtractionFindsNothing(t *testing.T) {
	server := fakeCompactionServer(t, "résumé", memoryNothingSentinel)
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	memoryFile := filepath.Join(t.TempDir(), "MEMORY.md")
	c := New("", 8192, 0.9, 0)
	c.MemoryFile = memoryFile
	c.AddUser("bonjour")
	c.AddAssistant("salut")

	if _, err := c.Compact(context.Background(), client); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if _, err := os.Stat(memoryFile); !os.IsNotExist(err) {
		t.Fatalf("MEMORY.md ne devrait pas avoir été créé (extraction = sentinel \"rien\"), stat err=%v", err)
	}
}

// Un serveur qui renvoie un contenu vide (cas observé : un modèle qui
// répond par un tool_call plutôt que du texte, alors qu'aucun outil n'est
// proposé à cet appel) ne doit jamais produire un résumé vide silencieux :
// Compact doit échouer explicitement plutôt que de remplacer l'historique
// par un message creux.
func TestCompactFailsOnEmptySummaryInsteadOfCorruptingHistory(t *testing.T) {
	server := fakeCompactionServer(t, "", memoryNothingSentinel)
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	c := New("", 8192, 0.9, 0)
	c.AddUser("bonjour")
	c.AddAssistant("salut")
	before := append([]llm.Message(nil), c.Messages...)

	if _, err := c.Compact(context.Background(), client); err == nil {
		t.Fatal("attendu une erreur pour un résumé vide")
	}

	if len(c.Messages) != len(before) {
		t.Fatalf("historique modifié malgré l'échec: %+v", c.Messages)
	}
}

// La maintenance mémoire a lieu APRÈS le résumé, jamais en parallèle (les
// serveurs locaux partagent une même réserve de contexte entre générations
// simultanées, voir Compact), et seulement si le résumé a réussi.
func TestCompactPersistsMemoryOnlyAfterSuccessfulSummary(t *testing.T) {
	memoryFile := filepath.Join(t.TempDir(), "MEMORY.md")

	failing := fakeCompactionServer(t, "", "- préfère les réponses en français")
	defer failing.Close()
	c := New("", 8192, 0.9, 0)
	c.MemoryFile = memoryFile
	c.AddUser("bonjour")
	c.AddAssistant("salut")
	if _, err := c.Compact(context.Background(), llm.New(failing.URL, "", "test-model")); err == nil {
		t.Fatal("attendu une erreur (résumé vide dans ce test)")
	}
	if _, err := os.Stat(memoryFile); !os.IsNotExist(err) {
		t.Fatalf("MEMORY.md ne devrait pas être écrit quand le résumé échoue (err=%v)", err)
	}

	ok := fakeCompactionServer(t, "résumé", "- préfère les réponses en français")
	defer ok.Close()
	if _, err := c.Compact(context.Background(), llm.New(ok.URL, "", "test-model")); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	data, err := os.ReadFile(memoryFile)
	if err != nil || !strings.Contains(string(data), "préfère les réponses en français") {
		t.Fatalf("MEMORY.md = %q (err=%v), attendu l'extraction après un résumé réussi", data, err)
	}
}

// overflowServer refuse (erreur de dépassement de contexte) toute requête de
// résumé dont le journal dépasse maxChars caractères, et répond "résumé"
// sinon. Compte les requêtes reçues.
func overflowServer(t *testing.T, maxChars int, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("décodage de la requête: %v", err)
		}
		*calls++
		size := 0
		for _, m := range req.Messages {
			size += len(m.Content)
		}
		if size > maxChars {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": "The model ran out of context space while generating."},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "résumé"}}},
		})
	}))
}

// Un échec de compaction pour dépassement de contexte n'est pas définitif :
// Compact supprime les plus vieux messages et retente, jusqu'à réussir.
func TestCompactRetriesDroppingOldestOnContextOverflow(t *testing.T) {
	calls := 0
	server := overflowServer(t, 6000, &calls)
	defer server.Close()

	c := New("", 0, 0.9, 2) // MaxContextTokens=0 : pas de pré-découpe, seule la boucle de réessai joue
	for i := 0; i < 20; i++ {
		c.AddUser(strings.Repeat("u", 400))
		c.AddAssistant(strings.Repeat("a", 400))
	}
	last := c.Messages[len(c.Messages)-1]

	compacted, err := c.Compact(context.Background(), llm.New(server.URL, "", "test-model"))
	if err != nil || !compacted {
		t.Fatalf("Compact = %v, %v ; attendu un succès après réessais", compacted, err)
	}
	if calls < 2 {
		t.Fatalf("%d requête(s) : attendu au moins un réessai", calls)
	}
	if c.LastCompactionDropped == 0 {
		t.Fatal("LastCompactionDropped = 0, attendu des messages supprimés sans résumé")
	}
	if len(c.Messages) != 3 || c.Messages[0].Role != "system" || c.Messages[2].Content != last.Content {
		t.Fatalf("historique inattendu après compaction : %d messages", len(c.Messages))
	}
}

// Même si toutes les tentatives échouent, les messages supprimés le restent :
// la conversation rétrécit au lieu de rester bloquée.
func TestCompactShrinksConversationEvenWhenAllAttemptsFail(t *testing.T) {
	calls := 0
	server := overflowServer(t, 0, &calls) // refuse tout
	defer server.Close()

	c := New("", 0, 0.9, 2)
	for i := 0; i < 20; i++ {
		c.AddUser("question")
		c.AddAssistant("réponse")
	}
	before := len(c.Messages)

	_, err := c.Compact(context.Background(), llm.New(server.URL, "", "test-model"))
	if err == nil && len(c.Messages) != 2 {
		t.Fatalf("sans erreur, attendu que tout l'ancien historique ait été supprimé (reste %d)", len(c.Messages))
	}
	if len(c.Messages) >= before {
		t.Fatalf("historique non réduit (%d -> %d)", before, len(c.Messages))
	}
	if calls > maxCompactionAttempts {
		t.Fatalf("%d requêtes, attendu au plus %d", calls, maxCompactionAttempts)
	}
}

// La requête de résumé est bornée d'emblée (summaryInputShare) : elle ne doit
// pas contenir tout le contexte.
func TestCompactBoundsSummaryRequestUpFront(t *testing.T) {
	calls := 0
	// 1000 tokens de contexte => budget ~600 tokens ~ 2400 caractères.
	server := overflowServer(t, 3200, &calls)
	defer server.Close()

	c := New("", 1000, 0.9, 1)
	for i := 0; i < 20; i++ {
		c.AddUser(strings.Repeat("u", 200))
		c.AddAssistant(strings.Repeat("a", 200))
	}
	if _, err := c.Compact(context.Background(), llm.New(server.URL, "", "test-model")); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if calls != 1 {
		t.Fatalf("%d requêtes, attendu une seule (requête déjà bornée avant envoi)", calls)
	}
}

// Une erreur qui n'est PAS un dépassement de contexte ne déclenche aucune
// suppression.
func TestCompactDoesNotDropOnOtherErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "model not loaded"}})
	}))
	defer server.Close()

	c := New("", 0, 0.9, 2)
	for i := 0; i < 5; i++ {
		c.AddUser("q")
		c.AddAssistant("r")
	}
	before := len(c.Messages)
	if _, err := c.Compact(context.Background(), llm.New(server.URL, "", "test-model")); err == nil {
		t.Fatal("attendu une erreur")
	}
	if len(c.Messages) != before || c.LastCompactionDropped != 0 {
		t.Fatalf("historique modifié (%d -> %d) sur une erreur sans rapport avec le contexte", before, len(c.Messages))
	}
}

func TestIsContextOverflow(t *testing.T) {
	for _, msg := range []string{
		"erreur LLM: Message too long: 65230 tokens exceeds the 65024-token context window.",
		"erreur LLM: The model ran out of context space while generating.",
		"This model's maximum context length is 8192 tokens",
	} {
		if !IsContextOverflow(errString(msg)) {
			t.Errorf("IsContextOverflow(%q) = false", msg)
		}
	}
	for _, msg := range []string{"model not loaded", "connection refused"} {
		if IsContextOverflow(errString(msg)) {
			t.Errorf("IsContextOverflow(%q) = true", msg)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// DropOldest ne coupe jamais un groupe tool_calls et garde le dernier message.
func TestDropOldestKeepsToolGroupsAndLastMessage(t *testing.T) {
	c := New("", 0, 0.9, 0)
	c.AddUser("q1")
	c.AppendRaw(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Function: llm.ToolCallFunction{Name: "run_shell"}}}})
	c.AppendRaw(llm.Message{Role: "tool", Content: "out", ToolCallID: "1"})
	c.AddAssistant("r1")
	c.AddUser("q2")

	if n := c.DropOldest(0.4); n != 3 { // 0.4*5=2 -> étendu au-delà du "tool"
		t.Fatalf("DropOldest = %d, attendu 3", n)
	}
	if c.Messages[0].Role == "tool" {
		t.Fatal("un message tool se retrouve en tête")
	}
	c.DropOldest(1)
	if len(c.Messages) != 1 || c.Messages[0].Content != "q2" {
		t.Fatalf("attendu de garder le dernier message, reste %+v", c.Messages)
	}
}

// Cas observé en pratique : un modèle qui échappe sa tentative d'appel
// d'outil en texte brut ("<tool_call><function=run_shell>...") plutôt que
// via tool_calls structuré, alors qu'aucun outil n'est proposé à cet appel
// de résumé. Content n'est alors PAS vide (contrairement au cas couvert par
// TestCompactFailsOnEmptySummaryInsteadOfCorruptingHistory) : Compact doit
// quand même refuser ce contenu plutôt que de le prendre pour un résumé.
func TestCompactFailsOnToolCallArtifactInsteadOfCorruptingHistory(t *testing.T) {
	escaped := "<tool_call>\n<function=run_shell>\n<parameter=command>\ncat > /tmp/x.md\nEOF\n</parameter>\n</function>\n</tool_call>"
	server := fakeCompactionServer(t, escaped, memoryNothingSentinel)
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	c := New("", 8192, 0.9, 0)
	c.AddUser("bonjour")
	c.AddAssistant("salut")
	before := append([]llm.Message(nil), c.Messages...)

	if _, err := c.Compact(context.Background(), client); err == nil {
		t.Fatal("attendu une erreur pour un résumé qui est en réalité un appel d'outil échappé en texte")
	}

	if len(c.Messages) != len(before) {
		t.Fatalf("historique modifié malgré l'échec: %+v", c.Messages)
	}
}

// Même défense côté extraction mémoire : un appel d'outil échappé en texte
// ne doit jamais être écrit dans MEMORY.md.
func TestCompactSkipsMemoryOnToolCallArtifact(t *testing.T) {
	escaped := "<tool_call>\n<function=run_shell>\n<parameter=command>\ncat > /tmp/x.md\nEOF\n</parameter>\n</function>\n</tool_call>"
	server := fakeCompactionServer(t, "résumé", escaped)
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	memoryFile := filepath.Join(t.TempDir(), "MEMORY.md")
	c := New("", 8192, 0.9, 0)
	c.MemoryFile = memoryFile
	c.AddUser("bonjour")
	c.AddAssistant("salut")

	if _, err := c.Compact(context.Background(), client); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if _, err := os.Stat(memoryFile); !os.IsNotExist(err) {
		t.Fatalf("MEMORY.md ne devrait pas avoir été créé (extraction = appel d'outil échappé), stat err=%v", err)
	}
}

// serializeForSummary ne doit jamais mentionner "Utilisateur"/"Assistant"
// (voir son commentaire : vérifié empiriquement comme la cause probable
// d'un modèle qui continue la conversation au lieu de la résumer), et doit
// représenter chaque rôle avec ses étiquettes neutres.
func TestSerializeForSummaryOmitsChatRoleWords(t *testing.T) {
	got := serializeForSummary([]llm.Message{
		{Role: "user", Content: "bonjour"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{Function: llm.ToolCallFunction{Name: "run_shell", Arguments: `{"command":"ls"}`}}}},
		{Role: "tool", Content: "a.txt"},
		{Role: "assistant", Content: "voilà le fichier"},
	})

	for _, forbidden := range []string{"Utilisateur", "Assistant"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("serializeForSummary contient %q, attendu qu'il soit banni : %q", forbidden, got)
		}
	}
	for _, want := range []string{"Demande : bonjour", "Action effectuée : run_shell(", "Résultat obtenu : a.txt", "Réponse donnée : voilà le fichier"} {
		if !strings.Contains(got, want) {
			t.Fatalf("serializeForSummary = %q, attendu qu'il contienne %q", got, want)
		}
	}
}

// Filet de sécurité de dernier recours (voir agent.Run/chat/mailbot, qui
// l'appellent après une compaction ratée ou insuffisante) : si l'historique
// dépasse encore MaxContextTokens, EnsureFitsContext doit supprimer les plus
// anciens messages jusqu'à repasser sous la limite, SANS jamais vider
// complètement l'historique.
func TestEnsureFitsContextDropsOldestMessages(t *testing.T) {
	c := New("", 200, 0.9, 0) // ~200 tokens de budget dur
	for i := 0; i < 20; i++ {
		c.AddUser(strings.Repeat("x", 40)) // chaque message pèse bien plus qu'un tour de boucle
	}
	last := c.Messages[len(c.Messages)-1]

	dropped := c.EnsureFitsContext()
	if dropped == 0 {
		t.Fatal("attendu que des messages soient supprimés (historique largement au-dessus du budget)")
	}
	if c.EstimateTokens() > c.MaxContextTokens {
		t.Fatalf("EstimateTokens()=%d toujours au-dessus de MaxContextTokens=%d après EnsureFitsContext", c.EstimateTokens(), c.MaxContextTokens)
	}
	if len(c.Messages) == 0 {
		t.Fatal("l'historique ne doit jamais être entièrement vidé")
	}
	if got := c.Messages[len(c.Messages)-1]; got.Content != last.Content {
		t.Fatalf("le tout dernier message a été supprimé, attendu qu'il soit toujours conservé : %+v", got)
	}
}

// Ne coupe jamais au milieu d'un groupe [assistant à tool_calls + ses
// résultats] — même précaution que Compact.
func TestEnsureFitsContextNeverOrphansToolResult(t *testing.T) {
	c := New("", 50, 0.9, 0) // budget minuscule : force une coupe systématique
	c.AddUser(strings.Repeat("x", 200))
	c.AppendRaw(llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "t1", Function: llm.ToolCallFunction{Name: "run_shell", Arguments: `{"command":"a"}`}}}})
	c.AppendRaw(llm.Message{Role: "tool", ToolCallID: "t1", Content: strings.Repeat("y", 200)})
	c.AddUser("dernier message")

	c.EnsureFitsContext()

	if len(c.Messages) > 0 && c.Messages[0].Role == "tool" {
		t.Fatalf("l'historique commence par un message \"tool\" orphelin après EnsureFitsContext : %+v", c.Messages[0])
	}
}

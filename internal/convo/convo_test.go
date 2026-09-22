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

// Depuis que persistMemory est lancée en parallèle du résumé (pas après),
// l'extraction mémoire doit quand même aboutir même si le résumé échoue
// ensuite — contrepartie assumée de la parallélisation (voir le commentaire
// de Compact). Vérifie aussi que Compact attend bien la fin de
// l'extraction (wg.Wait()) avant de retourner, sans quoi ce test serait
// intrinsèquement flaky (lecture du fichier avant qu'il soit écrit).
func TestCompactPersistsMemoryEvenWhenSummaryFails(t *testing.T) {
	server := fakeCompactionServer(t, "", "- préfère les réponses en français")
	defer server.Close()
	client := llm.New(server.URL, "", "test-model")

	memoryFile := filepath.Join(t.TempDir(), "MEMORY.md")
	c := New("", 8192, 0.9, 0)
	c.MemoryFile = memoryFile
	c.AddUser("bonjour")
	c.AddAssistant("salut")

	if _, err := c.Compact(context.Background(), client); err == nil {
		t.Fatal("attendu une erreur (résumé vide dans ce test)")
	}

	data, err := os.ReadFile(memoryFile)
	if err != nil {
		t.Fatalf("lecture de %q: %v (l'extraction mémoire aurait dû aboutir malgré l'échec du résumé)", memoryFile, err)
	}
	if !strings.Contains(string(data), "préfère les réponses en français") {
		t.Fatalf("MEMORY.md = %q, attendu qu'il contienne l'extraction", data)
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

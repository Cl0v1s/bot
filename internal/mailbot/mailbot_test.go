package mailbot

import (
	"testing"

	"bot/internal/convo"
)

func TestNormalizeSubjectStripsReplyAndForwardPrefixes(t *testing.T) {
	cases := map[string]string{
		"Question sur le scénario":      "question sur le scénario",
		"Re: Question sur le scénario":  "question sur le scénario",
		"RE : Question sur le scénario": "question sur le scénario", // espace avant ":"
		"Re: Fwd: Question":             "question",
		"Fw: TR: Re: Question":          "question",
		"  Question  ":                  "question",
		"":                              "",
	}
	for in, want := range cases {
		if got := normalizeSubject(in); got != want {
			t.Errorf("normalizeSubject(%q) = %q, attendu %q", in, got, want)
		}
	}
}

// Deux personnes différentes écrivant avec le même sujet (ou sans sujet) ne
// doivent jamais partager la même conversation : ce serait une fuite de
// contexte de l'une vers l'autre.
func TestGetOrCreateConversationScopedPerSender(t *testing.T) {
	opts := Options{
		SystemPrompt:  "prompt",
		Conversations: make(map[string]*convo.Conversation),
	}

	c1 := opts.getOrCreateConversation("alice@example.com", "Question")
	c2 := opts.getOrCreateConversation("bob@example.com", "Question")
	if c1 == c2 {
		t.Fatalf("alice et bob ne devraient pas partager la même conversation")
	}

	c1.AddUser("secret d'alice")
	for _, m := range c2.Messages {
		if m.Content == "secret d'alice" {
			t.Fatalf("le contexte d'alice a fuité vers la conversation de bob")
		}
	}
}

// Un même expéditeur qui répond dans le même fil (même sujet, avec ou sans
// préfixe Re:) doit retrouver la même conversation, avec l'historique
// précédent déjà présent.
func TestGetOrCreateConversationReusedAcrossReplies(t *testing.T) {
	opts := Options{
		SystemPrompt:  "prompt",
		Conversations: make(map[string]*convo.Conversation),
	}

	first := opts.getOrCreateConversation("alice@example.com", "Question sur le scénario")
	first.AddUser("premier message")

	second := opts.getOrCreateConversation("alice@example.com", "Re: Question sur le scénario")
	if second != first {
		t.Fatalf("attendu la même conversation pour une réponse dans le même fil (Re:)")
	}
	if len(second.Messages) != 1 || second.Messages[0].Content != "premier message" {
		t.Fatalf("l'historique du fil n'a pas été conservé: %+v", second.Messages)
	}
}

// Un même expéditeur avec un sujet différent doit obtenir une conversation
// séparée : pas de mélange entre deux fils distincts.
func TestGetOrCreateConversationSeparatePerSubject(t *testing.T) {
	opts := Options{
		SystemPrompt:  "prompt",
		Conversations: make(map[string]*convo.Conversation),
	}

	a := opts.getOrCreateConversation("alice@example.com", "Question A")
	b := opts.getOrCreateConversation("alice@example.com", "Question B")
	if a == b {
		t.Fatalf("deux sujets différents ne devraient pas partager la même conversation")
	}
}

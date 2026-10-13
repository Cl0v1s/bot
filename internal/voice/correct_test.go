package voice

import (
	"testing"

	"bot/internal/llm"
)

func TestRecentContext(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "bonjour"},
		{Role: "assistant", Content: "salut"},
		{Role: "tool", Content: "ignoré"},
		{Role: "user", Content: "abcdefghij"},
	}
	got := RecentContext(msgs, 2, 4)
	want := "Assistant : salu…\nUtilisateur : abcd…"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

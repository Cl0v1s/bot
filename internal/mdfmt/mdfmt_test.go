package mdfmt

import (
	"strings"
	"testing"
)

const sample = "Intro\n\n| # | Projet | Titre |\n|---|:------:|------:|\n| !1 | keycloak | fix: a |\n| !22 | k8s | feat: b |\n\n**Résumé**\n"

func TestFormatTextBox(t *testing.T) {
	got := FormatText(sample, Options{Unicode: true, Width: func() int { return 80 }})
	want := "Intro\n\n" +
		"┌─────┬──────────┬─────────┐\n" +
		"│ #   │  Projet  │   Titre │\n" +
		"├─────┼──────────┼─────────┤\n" +
		"│ !1  │ keycloak │  fix: a │\n" +
		"│ !22 │   k8s    │ feat: b │\n" +
		"└─────┴──────────┴─────────┘\n\n**Résumé**\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestStreamChunkedSameAsWhole(t *testing.T) {
	opt := Options{Unicode: true, Width: func() int { return 80 }}
	var b strings.Builder
	sw := NewStreamWriter(&b, opt)
	for _, r := range sample {
		_, _ = sw.Write([]byte(string(r)))
	}
	_ = sw.Flush()
	if b.String() != FormatText(sample, opt) {
		t.Fatalf("streaming ≠ complet:\n%s", b.String())
	}
}

func TestPassthroughBeforeTable(t *testing.T) {
	var b strings.Builder
	sw := NewStreamWriter(&b, Options{})
	_, _ = sw.Write([]byte("du texte sans retour"))
	if b.String() != "du texte sans retour" {
		t.Fatalf("le texte doit passer immédiatement, got %q", b.String())
	}
}

func TestFenceAndInvalidTableUntouched(t *testing.T) {
	in := "```\n| a | b |\n|---|---|\n| 1 | 2 |\n```\n| pas | un tableau |\nfin\n"
	if got := FormatText(in, Options{}); got != in {
		t.Fatalf("got %q", got)
	}
}

func TestTableAtEndWithoutNewline(t *testing.T) {
	got := FormatText("| a | b |\n|---|---|\n| 1 | 2 |", Options{})
	if !strings.HasPrefix(got, "+---+---+\n| a | b |") {
		t.Fatalf("got %q", got)
	}
}

func TestWrapsThenCards(t *testing.T) {
	in := "| Titre | Lien |\n|---|---|\n| un titre assez long pour être coupé | https://example.com/une/tres/longue/url/qui/ne/tient/pas |\n"
	// Largeur moyenne : la colonne titre se replie.
	got := FormatText(in, Options{Width: func() int { return 90 }})
	if !strings.Contains(got, "+--") || strings.Contains(got, " : ") {
		t.Fatalf("attendu un tableau, got:\n%s", got)
	}
	// Très étroit : fiches, URL intacte.
	got = FormatText(in, Options{Width: func() int { return 40 }})
	if !strings.Contains(got, "Lien  : https://example.com/une/tres/longue/url/qui/ne/tient/pas") {
		t.Fatalf("attendu des fiches, got:\n%s", got)
	}
}

func TestHTMLTableStyledAndSafe(t *testing.T) {
	h, err := HTML("| a | b |\n|:--|--:|\n| 1 | <script>x</script> |\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h, "<table style=") || !strings.Contains(h, "<th style=") || !strings.Contains(h, "<td style=") {
		t.Fatalf("styles manquants: %s", h)
	}
	if strings.Contains(h, "<script>") {
		t.Fatalf("HTML brut non filtré: %s", h)
	}
}

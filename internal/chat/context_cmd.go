package chat

import (
	"fmt"
	"io"
	"strings"

	"bot/internal/convo"
)

// runContextCommand traite /context : répartition (estimée, ~4 car./token)
// du contexte courant par catégorie, avec une jauge d'occupation.
func runContextCommand(out io.Writer, conv *convo.Conversation) {
	b := conv.Breakdown()
	max := conv.MaxContextTokens
	used := conv.EstimateTokens()

	fmt.Fprintf(out, "[contexte : %s]\n", usageLine(used, max))
	if max > 0 {
		fmt.Fprintln(out, contextBar(used, max, 40))
	}
	rows := []struct {
		name string
		n    int
	}{
		{"prompt système", b.System},
		{"messages utilisateur", b.User},
		{"réponses assistant", b.Assistant},
		{"appels d'outils", b.ToolCalls},
		{"résultats d'outils", b.ToolResults},
		{"résumés (system)", b.Summaries},
	}
	for _, r := range rows {
		fmt.Fprintf(out, "  %-22s ~%d tokens%s\n", r.name, r.n, pct(r.n, max))
	}
	fmt.Fprintf(out, "  messages : %d — compaction à %.0f%%\n", len(conv.Messages), conv.CompactAt*100)
}

func usageLine(used, max int) string {
	if max <= 0 {
		return fmt.Sprintf("~%d tokens", used)
	}
	return fmt.Sprintf("~%d/%d tokens (%d%%)", used, max, used*100/max)
}

func pct(n, max int) string {
	if max <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%.1f%%)", float64(n)*100/float64(max))
}

func contextBar(used, max, width int) string {
	filled := used * width / max
	if filled > width {
		filled = width
	}
	return "  [" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

// contextStatus : ligne d'état compacte de l'occupation du contexte,
// affichée en permanence au-dessus du prompt, et sa couleur (jaune à
// l'approche du seuil de compaction, rouge au-delà).
func contextStatus(conv *convo.Conversation) (text, colorCode string) {
	used, max := conv.EstimateTokens(), conv.MaxContextTokens
	if max <= 0 {
		return fmt.Sprintf("contexte ~%s tokens", humanTokens(used)), ansiGray
	}
	colorCode = ansiGray
	ratio := float64(used) / float64(max)
	switch {
	case ratio >= conv.CompactAt:
		colorCode = ansiRed
	case ratio >= conv.CompactAt*0.8:
		colorCode = ansiYellow
	}
	bar := strings.TrimSpace(contextBar(used, max, 20))
	return fmt.Sprintf("contexte %s %d%% ~%s/%s", bar, used*100/max, humanTokens(used), humanTokens(max)), colorCode
}

func humanTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

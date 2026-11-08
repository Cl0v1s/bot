// Package mdfmt met en forme le markdown produit par le modèle pour les
// sorties où il n'est pas interprété : tableaux alignés dans le terminal
// (REPL), texte brut et HTML pour les mails.
package mdfmt

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Options règle le rendu des tableaux.
type Options struct {
	// Width donne la largeur disponible (colonnes). Nil ou <= 0 : 100.
	Width func() int
	// Unicode : cadre en caractères de dessin (┌─┬┐) plutôt qu'en ASCII.
	Unicode bool
	// Bold : met l'en-tête en gras (séquences ANSI) — terminal couleur seulement.
	Bold bool
}

func (o Options) width() int {
	if o.Width != nil {
		if w := o.Width(); w > 0 {
			return w
		}
	}
	return 100
}

type align int

const (
	alignLeft align = iota
	alignRight
	alignCenter
)

var (
	sepCellRe = regexp.MustCompile(`^:?-+:?$`)
	linkRe    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
)

// isTableLine : la ligne (hors indentation) commence par '|'.
func isTableLine(l string) bool {
	return strings.HasPrefix(strings.TrimLeft(l, " \t"), "|")
}

// splitRow découpe une ligne de tableau en cellules, en respectant « \| » et
// les portions entre backticks.
func splitRow(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	var cells []string
	var cur strings.Builder
	inCode := false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\\' && i+1 < len(rs) && rs[i+1] == '|':
			cur.WriteRune('|')
			i++
		case r == '`':
			inCode = !inCode
			cur.WriteRune(r)
		case r == '|' && !inCode:
			cells = append(cells, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if rest := strings.TrimSpace(cur.String()); rest != "" {
		cells = append(cells, cur.String())
	}
	for i := range cells {
		cells[i] = cleanInline(strings.TrimSpace(cells[i]))
	}
	return cells
}

// cleanInline retire les marqueurs markdown inline qui fausseraient la
// largeur affichée : gras, code, liens [texte](url) → « texte (url) ».
func cleanInline(s string) string {
	s = linkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := linkRe.FindStringSubmatch(m)
		if sub[1] == sub[2] {
			return sub[2]
		}
		return sub[1] + " (" + sub[2] + ")"
	})
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	s = strings.ReplaceAll(s, "`", "")
	return s
}

// parseSeparator renvoie les alignements si line est une ligne « |---|:-:| ».
func parseSeparator(line string) ([]align, bool) {
	cells := splitRow(line)
	if len(cells) == 0 {
		return nil, false
	}
	al := make([]align, len(cells))
	for i, c := range cells {
		if !sepCellRe.MatchString(c) {
			return nil, false
		}
		l, r := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		switch {
		case l && r:
			al[i] = alignCenter
		case r:
			al[i] = alignRight
		}
	}
	return al, true
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// RenderTable met en forme un tableau markdown (lignes brutes, en-tête +
// séparateur + corps). ok=false si ce n'est pas un tableau valide : l'appelant
// réémet alors les lignes telles quelles.
func RenderTable(lines []string, opt Options) (string, bool) {
	if len(lines) < 2 {
		return "", false
	}
	al, ok := parseSeparator(lines[1])
	if !ok {
		return "", false
	}
	header := splitRow(lines[0])
	if len(header) == 0 {
		return "", false
	}
	rows := [][]string{header}
	for _, l := range lines[2:] {
		rows = append(rows, splitRow(l))
	}
	ncols := len(header)
	for _, r := range rows {
		if len(r) > ncols {
			ncols = len(r)
		}
	}
	for i := range rows {
		for len(rows[i]) < ncols {
			rows[i] = append(rows[i], "")
		}
	}
	for len(al) < ncols {
		al = append(al, alignLeft)
	}

	avail := opt.width()
	widths := make([]int, ncols)
	minW := make([]int, ncols)
	for _, r := range rows {
		for c, cell := range r {
			if n := runeLen(cell); n > widths[c] {
				widths[c] = n
			}
			for _, w := range strings.Fields(cell) {
				if n := runeLen(w); n > minW[c] {
					minW[c] = n
				}
			}
		}
	}
	total := func() int {
		t := 1
		for _, w := range widths {
			t += w + 3
		}
		return t
	}
	// Réduit la plus large colonne encore compressible (retour à la ligne
	// aux espaces) jusqu'à tenir ; sinon, présentation « fiches ».
	for total() > avail {
		best := -1
		for c, w := range widths {
			if w > minW[c] && (best < 0 || w > widths[best]) {
				best = c
			}
		}
		if best < 0 {
			return renderCards(rows, opt), true
		}
		widths[best]--
	}
	return renderBox(rows, al, widths, opt), true
}

type box struct{ h, v, tl, tm, tr, ml, mm, mr, bl, bm, br string }

var (
	boxUnicode = box{"─", "│", "┌", "┬", "┐", "├", "┼", "┤", "└", "┴", "┘"}
	boxASCII   = box{"-", "|", "+", "+", "+", "+", "+", "+", "+", "+", "+"}
)

// wrap coupe s aux espaces en lignes d'au plus w runes (un mot plus long que w
// reste entier : w est borné par le plus long mot, sauf cas dégénéré).
func wrap(s string, w int) []string {
	if runeLen(s) <= w {
		return []string{s}
	}
	var out []string
	cur := ""
	for _, word := range strings.Fields(s) {
		switch {
		case cur == "":
			cur = word
		case runeLen(cur)+1+runeLen(word) <= w:
			cur += " " + word
		default:
			out = append(out, cur)
			cur = word
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func pad(s string, w int, a align) string {
	gap := w - runeLen(s)
	if gap <= 0 {
		return s
	}
	switch a {
	case alignRight:
		return strings.Repeat(" ", gap) + s
	case alignCenter:
		l := gap / 2
		return strings.Repeat(" ", l) + s + strings.Repeat(" ", gap-l)
	}
	return s + strings.Repeat(" ", gap)
}

func renderBox(rows [][]string, al []align, widths []int, opt Options) string {
	b := boxASCII
	if opt.Unicode {
		b = boxUnicode
	}
	rule := func(l, m, r string) string {
		var sb strings.Builder
		sb.WriteString(l)
		for i, w := range widths {
			if i > 0 {
				sb.WriteString(m)
			}
			sb.WriteString(strings.Repeat(b.h, w+2))
		}
		sb.WriteString(r)
		return sb.String()
	}
	var out []string
	out = append(out, rule(b.tl, b.tm, b.tr))
	for ri, row := range rows {
		wrapped := make([][]string, len(row))
		height := 1
		for c, cell := range row {
			wrapped[c] = wrap(cell, widths[c])
			if len(wrapped[c]) > height {
				height = len(wrapped[c])
			}
		}
		for k := 0; k < height; k++ {
			var sb strings.Builder
			sb.WriteString(b.v)
			for c := range row {
				part := ""
				if k < len(wrapped[c]) {
					part = wrapped[c][k]
				}
				cell := pad(part, widths[c], al[c])
				if ri == 0 && opt.Bold && part != "" {
					cell = strings.Replace(cell, part, "\x1b[1m"+part+"\x1b[0m", 1)
				}
				sb.WriteString(" " + cell + " " + b.v)
			}
			out = append(out, sb.String())
		}
		if ri == 0 {
			out = append(out, rule(b.ml, b.mm, b.mr))
		}
	}
	out = append(out, rule(b.bl, b.bm, b.br))
	return strings.Join(out, "\n")
}

// renderCards : un bloc par ligne « Colonne : valeur », pour les tableaux
// trop larges (URL longues…) — les valeurs restent entières et copiables.
func renderCards(rows [][]string, opt Options) string {
	header := rows[0]
	lw := 0
	for _, h := range header {
		if n := runeLen(h); n > lw {
			lw = n
		}
	}
	bullet := "-"
	if opt.Unicode {
		bullet = "●"
	}
	var blocks []string
	for _, row := range rows[1:] {
		var sb strings.Builder
		for c, cell := range row {
			label := ""
			if c < len(header) {
				label = header[c]
			}
			if c == 0 {
				sb.WriteString(bullet + " ")
			} else {
				sb.WriteString("  ")
			}
			l := pad(label, lw, alignLeft)
			if opt.Bold && label != "" {
				l = strings.Replace(l, label, "\x1b[1m"+label+"\x1b[0m", 1)
			}
			sb.WriteString(l + " : " + cell + "\n")
		}
		blocks = append(blocks, strings.TrimRight(sb.String(), "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

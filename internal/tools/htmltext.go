package tools

import (
	"html"
	"regexp"
	"strings"
)

// noiseBlockRe retire un bloc <script>, <style>, <nav>, <header>, <footer>
// ou <aside> entièrement (contenu compris), pas seulement ses balises : un
// simple retrait de balises laisserait tout le JS/CSS, et tout le menu de
// navigation/pied de page d'un site, comme "texte" — largement plus
// polluant qu'utile, et une part significative du budget de troncature
// gaspillée sur du contenu de navigation plutôt que l'article lui-même
// (ex: Wikipédia, dont le menu principal précède le contenu réel).
var noiseBlockRe = regexp.MustCompile(`(?is)<(script|style|nav|header|footer|aside)\b[^>]*>.*?</(script|style|nav|header|footer|aside)>`)

// mainTagRe capture le contenu d'une balise <main> : convention HTML5 pour
// "le contenu principal de la page, hors navigation/bandeaux/pied de page"
// (utilisée par de nombreux sites, dont Wikipédia) — quand elle est
// présente, se concentrer sur son seul contenu élimine d'un coup tout le
// reste du bruit de mise en page, bien au-delà de ce que noiseBlockRe cible
// nommément.
var mainTagRe = regexp.MustCompile(`(?is)<main\b[^>]*>(.*)</main>`)

// htmlToText convertit grossièrement une page HTML en texte lisible : si
// une balise <main> est présente, seul son contenu est conservé (voir
// mainTagRe) ; scripts/styles/nav/header/footer/aside retirés en bloc ;
// balises restantes retirées ; entités HTML décodées ; lignes vides
// répétées réduites à une seule (le balisage retiré en laisse souvent
// beaucoup).
func htmlToText(raw string) string {
	if m := mainTagRe.FindStringSubmatch(raw); m != nil {
		raw = m[1]
	}
	raw = noiseBlockRe.ReplaceAllString(raw, "")

	// Suit aussi si on est dans une valeur d'attribut entre guillemets tant
	// qu'on est dans une balise : sans ça, un simple ">" à l'intérieur d'un
	// attribut (ex: l'attribut data-mw de Wikipédia/MediaWiki, qui embarque
	// du wikitexte JSON pouvant contenir des ">") termine à tort le mode
	// "dans une balise", et fait fuiter le reste de l'attribut comme texte
	// visible jusqu'au ">" suivant.
	var b strings.Builder
	inTag := false
	var quote byte
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case inTag && quote != 0:
			if c == quote {
				quote = 0
			}
		case inTag && (c == '"' || c == '\''):
			quote = c
		case c == '<':
			inTag = true
		case c == '>':
			inTag = false
		case !inTag:
			b.WriteByte(c)
		}
	}
	text := html.UnescapeString(b.String())

	var lines []string
	blank := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

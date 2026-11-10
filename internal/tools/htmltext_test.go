package tools

import (
	"strings"
	"testing"
)

func TestHTMLToTextStripsTagsScriptsAndStyles(t *testing.T) {
	raw := `<html><head><style>body{color:red}</style><script>alert("x")</script></head>
<body>
<nav>Menu</nav>
<h1>Padmé Amidala</h1>
<p>Sénatrice de Naboo, elle a servi la République.</p>


<p>Deuxième paragraphe.</p>
</body></html>`

	got := htmlToText(raw)

	if strings.Contains(got, "alert") || strings.Contains(got, "color:red") {
		t.Fatalf("le script/style n'a pas été retiré: %q", got)
	}
	if strings.Contains(got, "Menu") {
		t.Fatalf("le contenu de <nav> n'a pas été retiré: %q", got)
	}
	if !strings.Contains(got, "Padmé Amidala") || !strings.Contains(got, "Sénatrice de Naboo") {
		t.Fatalf("le texte utile a disparu: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("des lignes vides répétées subsistent: %q", got)
	}
}

// header/footer/aside (menus, pied de page, encarts) sont autant de bruit de
// navigation que nav/script/style : à retirer en bloc de la même façon, pas
// seulement leurs balises.
func TestHTMLToTextStripsHeaderFooterAside(t *testing.T) {
	raw := `<header>Aller au contenu | Rechercher | Se connecter</header>
<article><p>Contenu réel de l'article.</p></article>
<aside>Voir aussi : autres articles</aside>
<footer>© 2026 — Mentions légales</footer>`

	got := htmlToText(raw)
	for _, noise := range []string{"Aller au contenu", "Se connecter", "Voir aussi", "Mentions légales"} {
		if strings.Contains(got, noise) {
			t.Fatalf("bruit de navigation non retiré (%q): %q", noise, got)
		}
	}
	if !strings.Contains(got, "Contenu réel de l'article.") {
		t.Fatalf("le contenu réel a disparu: %q", got)
	}
}

// Quand une balise <main> est présente, tout ce qui est en dehors (barre
// latérale, menu de comptes, bandeau supérieur...) doit être ignoré, même
// si ce n'est pas nommément un <nav>/<header>/<footer>/<aside> (ex: des
// <div> de mise en page, comme sur Wikipédia).
func TestHTMLToTextUsesMainTagWhenPresent(t *testing.T) {
	raw := `<div id="siteHeader"><a href="#">Créer un compte</a></div>
<main id="content"><h1>Padmé Amidala</h1><p>Sénatrice de Naboo.</p></main>
<div id="siteFooter">Mentions légales</div>`

	got := htmlToText(raw)
	if strings.Contains(got, "Créer un compte") || strings.Contains(got, "Mentions légales") {
		t.Fatalf("contenu hors de <main> non ignoré: %q", got)
	}
	if !strings.Contains(got, "Padmé Amidala") || !strings.Contains(got, "Sénatrice de Naboo") {
		t.Fatalf("le contenu de <main> a disparu: %q", got)
	}
}

// Un ">" à l'intérieur d'une valeur d'attribut entre guillemets (ex:
// data-mw de MediaWiki, qui embarque du wikitexte JSON) ne doit pas être
// pris pour la fin de la balise : sans ça, le reste de l'attribut fuite
// comme texte visible, jusqu'au prochain vrai ">".
func TestHTMLToTextIgnoresGreaterThanInsideQuotedAttribute(t *testing.T) {
	raw := `<span data-mw='{"parts":[{"template":{"params":{"a":{"wt":"1 > 0"}}}}]}'>Padmé Amidala</span>`

	got := htmlToText(raw)
	if strings.Contains(got, "parts") || strings.Contains(got, "template") || strings.Contains(got, `"wt"`) {
		t.Fatalf("le contenu de l'attribut a fuité comme texte: %q", got)
	}
	if got != "Padmé Amidala" {
		t.Fatalf("got = %q, attendu \"Padmé Amidala\"", got)
	}
}

func TestHTMLToTextDecodesEntities(t *testing.T) {
	got := htmlToText("<p>Padm&eacute; Amidala &amp; la R&eacute;publique</p>")
	if !strings.Contains(got, "Padmé Amidala & la République") {
		t.Fatalf("entités HTML non décodées: %q", got)
	}
}

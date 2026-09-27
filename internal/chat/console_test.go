package chat

import (
	"strconv"
	"strings"
	"testing"
)

// vt est un émulateur de terminal minimal (juste ce qu'utilise console) :
// retour à la ligne automatique avec l'attente en dernière colonne des vrais
// terminaux, \r, \n (ONLCR : aussi retour en colonne 0), CSI A/C/D/G/J/K,
// SGR ignoré. Permet de vérifier ce qui est réellement AFFICHÉ, pas juste
// les séquences émises.
type vt struct {
	w           int
	rows        [][]rune
	r, c        int
	pendingWrap bool
}

func newVT(w int) *vt { return &vt{w: w} }

func (t *vt) row(r int) []rune {
	for len(t.rows) <= r {
		t.rows = append(t.rows, []rune(strings.Repeat(" ", t.w)))
	}
	return t.rows[r]
}

func (t *vt) Write(p []byte) (int, error) {
	s := []rune(string(p))
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; ch {
		case '\r':
			t.c, t.pendingWrap = 0, false
		case '\n':
			t.r, t.c, t.pendingWrap = t.r+1, 0, false
		case 0x1b:
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			n, err := strconv.Atoi(string(s[i+2 : j]))
			if err != nil {
				n = 1
			}
			switch s[j] {
			case 'A':
				t.r = max(0, t.r-n)
			case 'C':
				t.c = min(t.w-1, t.c+n)
			case 'D':
				t.c = max(0, t.c-n)
			case 'G':
				t.c = n - 1
			case 'K':
				row := t.row(t.r)
				for k := t.c; k < t.w; k++ {
					row[k] = ' '
				}
			case 'J':
				row := t.row(t.r)
				for k := t.c; k < t.w; k++ {
					row[k] = ' '
				}
				t.rows = t.rows[:t.r+1]
			}
			if s[j] != 'm' && s[j] != 'K' && s[j] != 'J' {
				t.pendingWrap = false
			}
			i = j
		default:
			if t.pendingWrap {
				t.r, t.c, t.pendingWrap = t.r+1, 0, false
			}
			t.row(t.r)[t.c] = ch
			if t.c == t.w-1 {
				t.pendingWrap = true
			} else {
				t.c++
			}
		}
	}
	return len(p), nil
}

func (t *vt) screen() string {
	var lines []string
	for _, r := range t.rows {
		lines = append(lines, strings.TrimRight(string(r), " "))
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func newTestConsole(w int) (*console, *vt) {
	term := newVT(w)
	return newConsole(term, func() int { return w }), term
}

func expectScreen(t *testing.T, term *vt, want string) {
	t.Helper()
	if got := term.screen(); got != want {
		t.Fatalf("écran :\n%s\n--- attendu :\n%s", got, want)
	}
}

// Cas signalé : une saisie plus longue que la largeur du terminal était
// réimprimée en double à chaque retour arrière.
func TestConsoleMultiRowInputBackspaceDoesNotDuplicate(t *testing.T) {
	c, term := newTestConsole(10)
	c.beginInput("> ")
	text := []rune(strings.Repeat("a", 20) + "bcdef")
	for i := 1; i <= len(text); i++ {
		c.setInput(text[:i], i)
	}
	for i := len(text) - 1; i >= 20; i-- {
		c.setInput(text[:i], i) // retour arrière
	}
	expectScreen(t, term, "> aaaaaaaa\naaaaaaaaaa\naa")
	if term.r != 2 || term.c != 2 {
		t.Fatalf("curseur en %d,%d, attendu 2,2", term.r, term.c)
	}
}

// Édition au milieu d'une saisie sur plusieurs lignes : curseur au bon
// endroit, rien en double.
func TestConsoleMultiRowInputCursorInMiddle(t *testing.T) {
	c, term := newTestConsole(10)
	c.beginInput("> ")
	text := []rune(strings.Repeat("x", 15))
	c.setInput(text, 3)
	c.setInput(append([]rune("xxxY"), text[3:]...), 4)
	expectScreen(t, term, "> xxxYxxxx\nxxxxxxxx")
	if term.r != 0 || term.c != 6 {
		t.Fatalf("curseur en %d,%d, attendu 0,6", term.r, term.c)
	}
}

// Saisie remplissant pile une ligne d'écran (attente de retour à la ligne
// du terminal) : le curseur doit rester cohérent pour la suite.
func TestConsoleInputExactlyFillingRow(t *testing.T) {
	c, term := newTestConsole(10)
	c.beginInput("> ")
	text := []rune("12345678")
	c.setInput(text, len(text))
	c.setInput(text[:7], 7)
	expectScreen(t, term, "> 1234567")
}

// Cas signalé : ce qui est tapé pendant que le modèle répond ne doit pas
// être découpé par le flux de la réponse — la saisie reste en bas, intacte,
// et la réponse continue au-dessus, sans coupure.
func TestConsoleOutputStreamsAboveInput(t *testing.T) {
	c, term := newTestConsole(40)
	c.beginInput("> ")
	c.Write([]byte("Bonjour"))
	c.setInput([]rune("ma question"), 11)
	c.Write([]byte(" le monde,"))
	c.Write([]byte(" suite\nligne 2"))
	expectScreen(t, term, "Bonjour le monde, suite\nligne 2\n> ma question")
	if term.r != 2 || term.c != 13 {
		t.Fatalf("curseur en %d,%d, attendu 2,13", term.r, term.c)
	}
}

// Ligne de sortie plus longue que l'écran, en cours de streaming : la suite
// doit reprendre exactement où elle s'était arrêtée.
func TestConsoleOutputWrappedPartialLine(t *testing.T) {
	c, term := newTestConsole(10)
	c.beginInput("> ")
	c.setInput([]rune("q"), 1)
	c.Write([]byte("abcdefghijklm"))
	c.Write([]byte("nop"))
	c.Write([]byte("qrst")) // remplit pile la 2e ligne
	c.Write([]byte("u"))
	expectScreen(t, term, "abcdefghij\nklmnopqrst\nu\n> q")
}

// Validation (Entrée) pendant une réponse : la ligne validée s'affiche sur
// sa propre ligne, la réponse reprend en dessous, et la nouvelle saisie
// reste en bas.
func TestConsoleCommitDuringStream(t *testing.T) {
	c, term := newTestConsole(40)
	c.beginInput("> ")
	c.Write([]byte("début de réponse"))
	c.setInput([]rune("suivante"), 8)
	c.commitInput()
	c.beginInput("> ")
	c.Write([]byte("[mis en file]\n"))
	c.Write([]byte("fin"))
	expectScreen(t, term, "début de réponse\n> suivante\n[mis en file]\nfin\n>")
}

// Les séquences ANSI (couleurs, effacement de "…réflexion") ne comptent pas
// dans la position de fin de ligne.
func TestConsoleTrackIgnoresEscapes(t *testing.T) {
	c, term := newTestConsole(40)
	c.beginInput("> ")
	c.Write([]byte("\x1b[90m…réflexion\x1b[0m"))
	c.Write([]byte("\r\x1b[K"))
	c.Write([]byte("\x1b[33mok\x1b[0m"))
	c.Write([]byte(" !"))
	expectScreen(t, term, "ok !\n>")
}

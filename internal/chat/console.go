package chat

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

// console remplace syncWriter quand l'éditeur de ligne est actif (vrai
// terminal) : en plus de sérialiser les écritures concurrentes, il garde la
// saisie en cours (prompt + texte tapé) toujours affichée EN BAS, sous la
// sortie du programme. Avant chaque écriture (réponse du modèle en
// streaming, événements d'outils...), la zone de saisie est effacée, la
// sortie écrite à sa place, puis la saisie redessinée en dessous — sans quoi
// le texte tapé pendant une réponse se retrouvait découpé au milieu du flux.
//
// La zone de saisie peut s'étendre sur plusieurs lignes d'écran (texte plus
// long que la largeur du terminal) : console calcule elle-même le nombre de
// lignes occupées et la position du curseur, pour toujours revenir au début
// de la zone avant de la redessiner — un simple "\r" ne remonte qu'au début
// de la ligne d'écran courante, et le texte était alors réimprimé en double
// à chaque retour arrière.
//
// Simplification assumée : un caractère compte pour une colonne (les
// caractères "larges" — CJK, certains emoji — décaleront l'affichage).
type console struct {
	mu    sync.Mutex
	out   io.Writer
	width func() int

	// Saisie en cours (voir beginInput/setInput/commitInput).
	active      bool
	prompt      string // texte visible du prompt
	promptColor string // code ANSI du prompt ("" = sans couleur)
	inputColor  string // code ANSI du texte tapé ("" = sans couleur)
	buf         []rune
	pos         int

	shown    bool // zone de saisie actuellement dessinée à l'écran
	curRow   int  // ligne d'écran du curseur, relative au début de la zone de saisie
	sepAdded bool // un saut de ligne a été ajouté sous une ligne de sortie inachevée pour dessiner la saisie

	// outCol : colonnes écrites depuis le dernier saut de ligne de la
	// sortie (séquences ANSI exclues) — pour revenir exactement à la fin
	// d'une ligne de sortie inachevée (streaming en cours) avant d'y
	// ajouter la suite.
	outCol int
}

func newConsole(out io.Writer, width func() int) *console {
	return &console{out: out, width: width}
}

// terminalWidth retourne la largeur actuelle du terminal f (TIOCGWINSZ),
// ou 80 si elle ne peut pas être déterminée. Relue à chaque dessin : suit
// un redimensionnement de la fenêtre.
func terminalWidth(f *os.File) func() int {
	return func() int {
		var ws struct{ Row, Col, X, Y uint16 }
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
		if errno != 0 || ws.Col == 0 {
			return 80
		}
		return int(ws.Col)
	}
}

func (c *console) w() int {
	if w := c.width(); w > 0 {
		return w
	}
	return 80
}

// Write écrit p comme sortie du programme, au-dessus de la saisie en cours.
func (c *console) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b bytes.Buffer
	c.hide(&b)
	b.Write(p)
	c.track(p)
	if c.active {
		c.draw(&b)
	}
	if _, err := c.out.Write(b.Bytes()); err != nil {
		return 0, err
	}
	return len(p), nil
}

// beginInput affiche une nouvelle zone de saisie vide avec prompt.
func (c *console) beginInput(prompt string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b bytes.Buffer
	c.hide(&b)
	c.active, c.prompt, c.buf, c.pos = true, prompt, nil, 0
	c.draw(&b)
	_, _ = c.out.Write(b.Bytes())
}

// setInput met à jour le texte saisi et la position du curseur, et
// redessine la zone de saisie.
func (c *console) setInput(buf []rune, pos int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b bytes.Buffer
	c.hide(&b)
	c.buf, c.pos = append(c.buf[:0], buf...), pos
	if c.active {
		c.draw(&b)
	}
	_, _ = c.out.Write(b.Bytes())
}

// commitInput termine la saisie (Entrée) : la ligne validée est écrite dans
// la sortie, sur sa propre ligne, et la zone de saisie disparaît jusqu'au
// prochain beginInput.
func (c *console) commitInput() {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b bytes.Buffer
	c.hide(&b)
	if c.outCol != 0 {
		b.WriteString("\r\n")
	}
	b.WriteString(c.paint(c.promptColor, c.prompt))
	b.WriteString(c.paint(c.inputColor, string(c.buf)))
	b.WriteString("\r\n")
	c.outCol = 0
	c.active, c.buf, c.pos = false, nil, 0
	_, _ = c.out.Write(b.Bytes())
}

// endInput retire la zone de saisie sans rien valider (fin de saisie).
func (c *console) endInput() {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b bytes.Buffer
	c.hide(&b)
	c.active = false
	_, _ = c.out.Write(b.Bytes())
}

func (c *console) paint(code, s string) string {
	if code == "" || s == "" {
		return s
	}
	return code + s + ansiReset
}

// draw dessine la zone de saisie à la position courante (fin de la sortie)
// et place le curseur sur la position d'édition.
func (c *console) draw(b *bytes.Buffer) {
	w := c.w()
	// Ligne de sortie inachevée (réponse en cours de streaming) : la saisie
	// va sur la ligne suivante, on y reviendra dans hide. outCol multiple
	// non nul de w : curseur en attente de retour à la ligne automatique,
	// le prochain caractère ira de toute façon au début de la ligne
	// suivante — pas de saut de ligne à ajouter.
	if c.outCol%w != 0 {
		b.WriteString("\r\n")
		c.sepAdded = true
	}
	promptW := utf8.RuneCountInString(c.prompt)
	b.WriteString(c.paint(c.promptColor, c.prompt))
	b.WriteString(c.paint(c.inputColor, string(c.buf)))
	total := promptW + len(c.buf)
	if total > 0 && total%w == 0 {
		// Ligne d'écran pile remplie : le terminal laisse le curseur sur la
		// dernière colonne, en attente. On force le passage à la ligne
		// suivante pour que la position réelle corresponde au calcul.
		b.WriteString(" \r")
	}
	endRow := total / w
	cursor := promptW + c.pos
	row, col := cursor/w, cursor%w
	if endRow > row {
		fmt.Fprintf(b, "\x1b[%dA", endRow-row)
	}
	fmt.Fprintf(b, "\r\x1b[%dG", col+1)
	c.curRow = row
	c.shown = true
}

// hide efface la zone de saisie et replace le curseur exactement à la fin
// de la sortie, là où la prochaine écriture doit continuer.
func (c *console) hide(b *bytes.Buffer) {
	if !c.shown {
		return
	}
	if c.curRow > 0 {
		fmt.Fprintf(b, "\x1b[%dA", c.curRow)
	}
	b.WriteString("\r\x1b[J")
	if c.sepAdded {
		fmt.Fprintf(b, "\x1b[A\x1b[%dG", c.outCol%c.w()+1)
		c.sepAdded = false
	}
	c.shown = false
}

// track met à jour outCol d'après p : saut de ligne/retour chariot remettent
// à zéro, séquences ANSI (CSI) ignorées, un caractère UTF-8 = une colonne.
func (c *console) track(p []byte) {
	for i := 0; i < len(p); i++ {
		switch ch := p[i]; {
		case ch == '\n' || ch == '\r':
			c.outCol = 0
		case ch == '\b':
			if c.outCol > 0 {
				c.outCol--
			}
		case ch == '\t':
			c.outCol = (c.outCol/8 + 1) * 8
		case ch == 0x1b:
			if i+1 < len(p) && p[i+1] == '[' {
				i += 2
				for i < len(p) && (p[i] < 0x40 || p[i] > 0x7e) {
					i++
				}
			} else {
				i++
			}
		case ch < 0x20:
		case ch&0xc0 == 0x80: // octet de continuation UTF-8
		default:
			c.outCol++
		}
	}
}

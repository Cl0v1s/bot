package chat

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// lineEditor est un éditeur de ligne minimal, sans dépendance externe,
// activé uniquement quand l'entrée est un vrai terminal interactif (sinon
// on garde bufio.Scanner, voir repl.go — important pour les pipes/tests).
// Gère l'édition de base (curseur gauche/droite, début/fin, retour arrière,
// suppression) et la navigation dans l'historique des prompts de CETTE
// session via les flèches haut/bas — pas de persistance entre sessions.
//
// Ctrl+C reste géré par le signal SIGINT habituel (internal/chat/interrupt.go) :
// on passe le terminal en mode "cbreak" (pas de ligne canonique, pas d'écho
// local — c'est nous qui réaffichons) mais on laisse ISIG actif, donc le
// pilote du terminal continue de générer SIGINT lui-même sur Ctrl+C, sans
// rien changer à ce mécanisme existant.
type lineEditor struct {
	f       *os.File
	out     io.Writer
	saved   string // état stty d'origine (sortie de `stty -g`), pour restauration
	history []string
}

// newLineEditor bascule f en mode cbreak via `stty` (sous-processus, même
// approche que le reste du projet pour internal/sandbox — aucune
// dépendance ajoutée). Si stty échoue (terminal exotique, sandboxé...),
// retourne une erreur : l'appelant doit alors retomber sur bufio.Scanner.
func newLineEditor(f *os.File, out io.Writer) (*lineEditor, error) {
	saved, err := runStty(f, "-g")
	if err != nil {
		return nil, err
	}
	if _, err := runStty(f, "-icanon", "-echo"); err != nil {
		return nil, err
	}
	return &lineEditor{f: f, out: out, saved: strings.TrimSpace(saved)}, nil
}

// restore rétablit l'état stty d'origine. Sûr à appeler plusieurs fois ou
// sur un éditeur nil. Doit être appelé sur TOUT chemin de sortie du
// programme, y compris os.Exit (voir interrupt.go) qui saute les defer.
func (e *lineEditor) restore() {
	if e == nil || e.saved == "" {
		return
	}
	_, _ = runStty(e.f, e.saved)
}

func runStty(f *os.File, args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = f
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("stty %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(out.String()), err)
	}
	return out.String(), nil
}

func (e *lineEditor) readByteRaw() (byte, error) {
	var b [1]byte
	if _, err := e.f.Read(b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// readRune lit un caractère complet, potentiellement multi-octets (UTF-8 :
// accents, etc.), à partir de son premier octet déjà lu.
func (e *lineEditor) readRune(first byte) (rune, error) {
	if first < 0x80 {
		return rune(first), nil
	}
	var n int
	switch {
	case first&0xE0 == 0xC0:
		n = 1
	case first&0xF0 == 0xE0:
		n = 2
	case first&0xF8 == 0xF0:
		n = 3
	default:
		return utf8.RuneError, nil
	}
	buf := make([]byte, n+1)
	buf[0] = first
	for i := 0; i < n; i++ {
		b, err := e.readByteRaw()
		if err != nil {
			return utf8.RuneError, err
		}
		buf[i+1] = b
	}
	r, _ := utf8.DecodeRune(buf)
	return r, nil
}

// readEscapeSeq lit la suite d'une séquence d'échappement ANSI (flèches,
// Origine/Fin, Suppr...) après un octet ESC déjà consommé. Utilise un court
// délai pour distinguer une touche Échap pressée seule (rien ne suit) du
// début d'une séquence — sans quoi Échap seule bloquerait la lecture en
// attendant un octet suivant qui n'arrive jamais.
func (e *lineEditor) readEscapeSeq() (seq string, ok bool) {
	_ = e.f.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	defer e.f.SetReadDeadline(time.Time{})

	b1, err := e.readByteRaw()
	if err != nil || (b1 != '[' && b1 != 'O') {
		return "", false
	}
	var sb strings.Builder
	sb.WriteByte(b1)
	for sb.Len() < 8 {
		b, err := e.readByteRaw()
		if err != nil {
			return "", false
		}
		sb.WriteByte(b)
		if b >= 0x40 && b <= 0x7E { // octet final d'une séquence CSI
			return sb.String(), true
		}
	}
	return "", false
}

// ReadLine affiche prompt puis lit une ligne au clavier avec édition de
// base et historique (flèches haut/bas). ok=false signifie fin de saisie
// (Ctrl+D sur ligne vide) ou erreur de lecture.
func (e *lineEditor) ReadLine(prompt string) (line string, ok bool) {
	buf := []rune{}
	pos := 0
	histIdx := len(e.history)
	pending := "" // ligne en cours de frappe, sauvegardée en remontant l'historique

	redraw := func() {
		fmt.Fprint(e.out, "\r", prompt, string(buf), "\x1b[K")
		if back := len(buf) - pos; back > 0 {
			fmt.Fprintf(e.out, "\x1b[%dD", back)
		}
	}

	fmt.Fprint(e.out, prompt)

	for {
		b, err := e.readByteRaw()
		if err != nil {
			return "", false
		}

		switch {
		case b == '\r' || b == '\n':
			fmt.Fprint(e.out, "\r\n")
			s := string(buf)
			if s != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != s) {
				e.history = append(e.history, s)
			}
			return s, true

		case b == 0x7f || b == 0x08: // retour arrière
			if pos > 0 {
				buf = append(buf[:pos-1], buf[pos:]...)
				pos--
				redraw()
			}

		case b == 0x04: // Ctrl+D
			if len(buf) == 0 {
				return "", false
			}

		case b == 0x01: // Ctrl+A : début de ligne
			pos = 0
			redraw()

		case b == 0x05: // Ctrl+E : fin de ligne
			pos = len(buf)
			redraw()

		case b == 0x1b:
			seq, isSeq := e.readEscapeSeq()
			if !isSeq {
				continue // Échap seule : ignorée
			}
			switch seq {
			case "[A": // haut : entrée précédente de l'historique
				if histIdx > 0 {
					if histIdx == len(e.history) {
						pending = string(buf)
					}
					histIdx--
					buf = []rune(e.history[histIdx])
					pos = len(buf)
					redraw()
				}
			case "[B": // bas : entrée suivante, ou ligne en cours au bout
				if histIdx < len(e.history) {
					histIdx++
					if histIdx == len(e.history) {
						buf = []rune(pending)
					} else {
						buf = []rune(e.history[histIdx])
					}
					pos = len(buf)
					redraw()
				}
			case "[C": // droite
				if pos < len(buf) {
					pos++
					fmt.Fprint(e.out, "\x1b[C")
				}
			case "[D": // gauche
				if pos > 0 {
					pos--
					fmt.Fprint(e.out, "\x1b[D")
				}
			case "[H", "OH", "[1~": // début
				pos = 0
				redraw()
			case "[F", "OF", "[4~": // fin
				pos = len(buf)
				redraw()
			case "[3~": // suppr (efface le caractère sous le curseur)
				if pos < len(buf) {
					buf = append(buf[:pos], buf[pos+1:]...)
					redraw()
				}
			}

		case b < 0x20:
			// autre caractère de contrôle : ignoré

		default:
			r, err := e.readRune(b)
			if err != nil {
				return "", false
			}
			buf = append(buf, 0)
			copy(buf[pos+1:], buf[pos:])
			buf[pos] = r
			pos++
			if pos == len(buf) {
				fmt.Fprint(e.out, string(r))
			} else {
				redraw()
			}
		}
	}
}

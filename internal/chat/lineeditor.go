package chat

import (
	"bytes"
	"fmt"
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
//
// Le mode "bracketed paste" du terminal est activé : un contenu collé
// arrive encadré par ESC[200~ … ESC[201~, ce qui permet de le distinguer
// d'une frappe. Ses sauts de ligne ne valident donc PAS la saisie : un
// collage multi-lignes est remplacé dans la zone de saisie par un
// marqueur compact ("[Contenu collé #1 +12 lignes]"), effacé d'un bloc au
// retour arrière, et remplacé par le contenu réel à la validation.
//
// L'affichage de la saisie est entièrement délégué à console (voir
// console.go) : c'est elle qui la garde en bas de l'écran, sous la sortie
// du modèle qui peut s'afficher en parallèle de la frappe, et qui gère une
// saisie plus longue qu'une ligne d'écran.
type lineEditor struct {
	f       *os.File
	con     *console
	saved   string // état stty d'origine (sortie de `stty -g`), pour restauration
	history []string
	// pastes : contenus collés de la session, référencés dans la saisie (et
	// dans l'historique) par leur marqueur. Conservés tout au long de la
	// session pour que rappeler une entrée de l'historique la redéveloppe.
	pastes []pastedContent
}

type pastedContent struct {
	label   string // marqueur affiché dans la saisie
	content string
}

const (
	bracketedPasteOn  = "\x1b[?2004h"
	bracketedPasteOff = "\x1b[?2004l"
	pasteEnd          = "\x1b[201~"
)

// newLineEditor bascule f en mode cbreak via `stty` (sous-processus, même
// approche que le reste du projet pour internal/sandbox — aucune
// dépendance ajoutée). Si stty échoue (terminal exotique, sandboxé...),
// retourne une erreur : l'appelant doit alors retomber sur bufio.Scanner.
func newLineEditor(f *os.File, con *console) (*lineEditor, error) {
	saved, err := runStty(f, "-g")
	if err != nil {
		return nil, err
	}
	if _, err := runStty(f, "-icanon", "-echo"); err != nil {
		return nil, err
	}
	con.writeRaw(bracketedPasteOn)
	return &lineEditor{f: f, con: con, saved: strings.TrimSpace(saved)}, nil
}

// restore rétablit l'état stty d'origine. Sûr à appeler plusieurs fois ou
// sur un éditeur nil. Doit être appelé sur TOUT chemin de sortie du
// programme, y compris os.Exit (voir interrupt.go) qui saute les defer.
func (e *lineEditor) restore() {
	if e == nil || e.saved == "" {
		return
	}
	e.con.writeRaw(bracketedPasteOff)
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

// readPaste lit le contenu d'un collage, après la séquence ESC[200~ déjà
// consommée, jusqu'à la séquence de fin ESC[201~ (exclue). Les fins de
// ligne sont normalisées en "\n" (les terminaux envoient "\r").
func (e *lineEditor) readPaste() (string, error) {
	var b []byte
	for !bytes.HasSuffix(b, []byte(pasteEnd)) {
		c, err := e.readByteRaw()
		if err != nil {
			return "", err
		}
		b = append(b, c)
	}
	s := string(b[:len(b)-len(pasteEnd)])
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n"), nil
}

// pasteRunes retourne ce qu'il faut insérer dans la saisie pour le contenu
// collé s : le texte lui-même s'il tient sur une ligne sans caractère de
// contrôle, sinon un marqueur (enregistré dans e.pastes) qui le
// représente jusqu'à la validation.
func (e *lineEditor) pasteRunes(s string) []rune {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return []rune(s)
	}
	label := fmt.Sprintf("[Contenu collé #%d", len(e.pastes)+1)
	if n := strings.Count(strings.TrimRight(s, "\n"), "\n"); n > 0 {
		label += fmt.Sprintf(" +%d lignes", n)
	}
	label += "]"
	e.pastes = append(e.pastes, pastedContent{label: label, content: s})
	return []rune(label)
}

// expandPastes remplace dans s chaque marqueur de collage par son contenu.
func (e *lineEditor) expandPastes(s string) string {
	for _, p := range e.pastes {
		s = strings.ReplaceAll(s, p.label, p.content)
	}
	return s
}

// pasteLabelBefore/pasteLabelAfter retournent la longueur (en runes) du
// marqueur de collage qui se termine juste avant / commence juste après
// pos dans buf, ou 0 : retour arrière et Suppr l'effacent d'un bloc.
func (e *lineEditor) pasteLabelBefore(buf []rune, pos int) int {
	s := string(buf[:pos])
	for _, p := range e.pastes {
		if strings.HasSuffix(s, p.label) {
			return utf8.RuneCountInString(p.label)
		}
	}
	return 0
}

func (e *lineEditor) pasteLabelAfter(buf []rune, pos int) int {
	s := string(buf[pos:])
	for _, p := range e.pastes {
		if strings.HasPrefix(s, p.label) {
			return utf8.RuneCountInString(p.label)
		}
	}
	return 0
}

// ReadLine affiche prompt puis lit une ligne au clavier avec édition de
// base et historique (flèches haut/bas). ok=false signifie fin de saisie
// (Ctrl+D sur ligne vide) ou erreur de lecture.
func (e *lineEditor) ReadLine(prompt string) (line string, ok bool) {
	buf := []rune{}
	pos := 0
	histIdx := len(e.history)
	pending := "" // ligne en cours de frappe, sauvegardée en remontant l'historique

	redraw := func() { e.con.setInput(buf, pos) }

	e.con.beginInput(prompt)

	for {
		b, err := e.readByteRaw()
		if err != nil {
			e.con.endInput()
			return "", false
		}

		switch {
		case b == '\r' || b == '\n':
			e.con.commitInput()
			s := string(buf)
			if s != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != s) {
				e.history = append(e.history, s)
			}
			return e.expandPastes(s), true

		case b == 0x7f || b == 0x08: // retour arrière
			if pos > 0 {
				n := max(e.pasteLabelBefore(buf, pos), 1)
				buf = append(buf[:pos-n], buf[pos:]...)
				pos -= n
				redraw()
			}

		case b == 0x04: // Ctrl+D
			if len(buf) == 0 {
				e.con.endInput()
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
					redraw()
				}
			case "[D": // gauche
				if pos > 0 {
					pos--
					redraw()
				}
			case "[H", "OH", "[1~": // début
				pos = 0
				redraw()
			case "[F", "OF", "[4~": // fin
				pos = len(buf)
				redraw()
			case "[3~": // suppr (efface le caractère sous le curseur)
				if pos < len(buf) {
					n := max(e.pasteLabelAfter(buf, pos), 1)
					buf = append(buf[:pos], buf[pos+n:]...)
					redraw()
				}
			case "[200~": // début d'un collage (bracketed paste)
				s, err := e.readPaste()
				if err != nil {
					e.con.endInput()
					return "", false
				}
				ins := e.pasteRunes(s)
				buf = append(buf[:pos], append(ins, buf[pos:]...)...)
				pos += len(ins)
				redraw()
			}

		case b < 0x20:
			// autre caractère de contrôle : ignoré

		default:
			r, err := e.readRune(b)
			if err != nil {
				e.con.endInput()
				return "", false
			}
			buf = append(buf, 0)
			copy(buf[pos+1:], buf[pos:])
			buf[pos] = r
			pos++
			redraw()
		}
	}
}

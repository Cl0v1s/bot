package mdfmt

import (
	"io"
	"strings"
)

const (
	stPassStart  = iota // début de ligne hors tableau (indentation en attente)
	stPass              // milieu de ligne hors tableau : passage direct
	stTableLine         // ligne de tableau en cours de lecture
	stTableStart        // début de ligne dans un tableau (indentation en attente)
)

// StreamWriter laisse passer le texte au fil de l'eau, mais retient les
// tableaux markdown (lignes commençant par '|') pour les réémettre alignés.
// Les blocs de code (```) ne sont jamais touchés. Appeler Flush en fin de
// flux. Non concurrent.
type StreamWriter struct {
	w   io.Writer
	opt Options

	state   int
	inFence bool
	head    string // premiers caractères non blancs de la ligne (détection ```)
	lead    strings.Builder
	cur     strings.Builder
	table   []string
}

func NewStreamWriter(w io.Writer, opt Options) *StreamWriter {
	return &StreamWriter{w: w, opt: opt}
}

func (s *StreamWriter) Write(p []byte) (int, error) {
	var out strings.Builder
	for _, r := range string(p) {
		s.step(r, &out)
	}
	if out.Len() > 0 {
		if _, err := io.WriteString(s.w, out.String()); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (s *StreamWriter) step(r rune, out *strings.Builder) {
	switch s.state {
	case stPassStart:
		switch {
		case r == ' ' || r == '\t':
			s.lead.WriteRune(r)
		case r == '\n':
			out.WriteString(s.lead.String() + "\n")
			s.lead.Reset()
			s.head = ""
		case r == '|' && !s.inFence:
			s.cur.WriteString(s.lead.String() + "|")
			s.lead.Reset()
			s.state = stTableLine
		default:
			out.WriteString(s.lead.String())
			s.lead.Reset()
			s.head = ""
			s.state = stPass
			s.step(r, out)
		}
	case stPass:
		out.WriteRune(r)
		if r == '\n' {
			s.state = stPassStart
			s.head = ""
			return
		}
		if len(s.head) < 3 {
			s.head += string(r)
			if s.head == "```" {
				s.inFence = !s.inFence
			}
		}
	case stTableLine:
		if r == '\n' {
			s.table = append(s.table, s.cur.String())
			s.cur.Reset()
			s.state = stTableStart
			return
		}
		if r != '\r' {
			s.cur.WriteRune(r)
		}
	case stTableStart:
		switch {
		case r == ' ' || r == '\t':
			s.lead.WriteRune(r)
		case r == '|':
			s.cur.WriteString(s.lead.String() + "|")
			s.lead.Reset()
			s.state = stTableLine
		default:
			s.flushTable(out)
			s.state = stPassStart
			s.lead.Reset()
			s.step(r, out)
		}
	}
}

func (s *StreamWriter) flushTable(out *strings.Builder) {
	if len(s.table) == 0 {
		return
	}
	if txt, ok := RenderTable(s.table, s.opt); ok {
		out.WriteString(txt + "\n")
	} else {
		out.WriteString(strings.Join(s.table, "\n") + "\n")
	}
	s.table = nil
}

// Flush émet ce qui est en attente (tableau en cours, ligne partielle).
func (s *StreamWriter) Flush() error {
	var out strings.Builder
	switch s.state {
	case stTableLine:
		s.table = append(s.table, s.cur.String())
		s.cur.Reset()
		s.flushTable(&out)
	case stTableStart:
		s.flushTable(&out)
		out.WriteString(s.lead.String())
		s.lead.Reset()
	case stPassStart:
		out.WriteString(s.lead.String())
		s.lead.Reset()
	}
	s.state = stPassStart
	s.head = ""
	if out.Len() == 0 {
		return nil
	}
	_, err := io.WriteString(s.w, out.String())
	return err
}

// FormatText met en forme tous les tableaux de text (rendu complet, sans
// streaming) — pour le corps texte brut des mails.
func FormatText(text string, opt Options) string {
	var b strings.Builder
	sw := NewStreamWriter(&b, opt)
	_, _ = sw.Write([]byte(text))
	_ = sw.Flush()
	return b.String()
}

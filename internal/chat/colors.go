package chat

import (
	"io"
	"os"
)

// Codes ANSI minimaux, sans dépendance externe. Volontairement limités à
// quelques couleurs pour distinguer les grandes catégories de sortie :
// saisie utilisateur, erreurs, appels d'outils, résultats d'outils.
const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiCyan    = "\x1b[36m"
	ansiMagenta = "\x1b[35m" // demandes d'interaction utilisateur (permission, confirmation)
)

// colorsEnabled désactive la couleur si NO_COLOR est défini (convention
// standard, https://no-color.org) ou si out n'est pas un terminal (fichier,
// pipe, redirection) — pour ne jamais injecter de codes ANSI dans une sortie
// non interactive.
func colorsEnabled(out io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	return isTerminalFile(f)
}

// isTerminalFile indique si f est un vrai terminal interactif (et non un
// fichier, un pipe ou une redirection) — utilisé pour la couleur et pour
// décider d'activer l'éditeur de ligne (internal/chat/lineeditor.go).
func isTerminalFile(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// colorizer retourne une fonction qui encadre s des codes ANSI code, ou
// renvoie s tel quel si les couleurs sont désactivées.
func colorizer(out io.Writer) func(code, s string) string {
	enabled := colorsEnabled(out)
	return func(code, s string) string {
		if !enabled || code == "" {
			return s
		}
		return code + s + ansiReset
	}
}

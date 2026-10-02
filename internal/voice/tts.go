package voice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

// maxSpeechRunes borne la longueur lue à voix haute : une longue réponse
// reste intégralement affichée dans le terminal, mais la lire en entier
// serait pénible.
const maxSpeechRunes = 1500

// Speaker lit un texte à voix haute en lançant une commande de synthèse
// vocale (par défaut espeak-ng, voir config.VoiceTTSCmd) qui reçoit le texte
// sur son entrée standard. Une seule lecture à la fois : en lancer une
// nouvelle interrompt la précédente. Sûr pour un usage concurrent ; un
// *Speaker nil est valide et ne fait rien.
type Speaker struct {
	// Cmd : commande et arguments (ex: espeak-ng -v fr+robosoft8 -p 30 -s 130).
	Cmd []string

	mu     sync.Mutex
	cancel context.CancelFunc
}

// Speak lit text (nettoyé du balisage, voir CleanForSpeech) et ne retourne
// qu'à la fin de la lecture. Retourne nil si la lecture a été interrompue
// (Stop, nouvelle lecture, ctx annulé) ou si text est vide une fois nettoyé.
func (sp *Speaker) Speak(ctx context.Context, text string) error {
	if sp == nil {
		return nil
	}
	text = CleanForSpeech(text)
	if text == "" {
		return nil
	}
	if len(sp.Cmd) == 0 {
		return errors.New("commande de synthèse vocale vide")
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sp.mu.Lock()
	if sp.cancel != nil {
		sp.cancel()
	}
	sp.cancel = cancel
	sp.mu.Unlock()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(cctx, sp.Cmd[0], sp.Cmd[1:]...)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stderr = &stderr
	err := cmd.Run()
	if cctx.Err() != nil {
		return nil
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s : %w (%s)", sp.Cmd[0], err, firstLine(msg))
		}
		return fmt.Errorf("%s : %w", sp.Cmd[0], err)
	}
	return nil
}

// Stop interrompt la lecture en cours, s'il y en a une.
func (sp *Speaker) Stop() {
	if sp == nil {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.cancel != nil {
		sp.cancel()
		sp.cancel = nil
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

var (
	thinkBlockRe = regexp.MustCompile(`(?s)<think>.*?</think>`)
	codeFenceRe  = regexp.MustCompile("(?s)```.*?(?:```|$)")
	mdLinkRe     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	urlRe        = regexp.MustCompile(`https?://\S+`)
	lineMarkerRe = regexp.MustCompile(`(?m)^[ \t]{0,3}(?:#{1,6}|>|[-*+]|\d+[.)])[ \t]+`)
	blankLinesRe = regexp.MustCompile(`\n{3,}`)
	spacesRe     = regexp.MustCompile(`[ \t]+`)
)

// CleanForSpeech retire ce qui ne se lit pas bien à voix haute : blocs de
// raisonnement et de code, URL, marqueurs Markdown (titres, puces, gras,
// liens), émojis. Tronque aussi à maxSpeechRunes, de préférence en fin de
// phrase.
func CleanForSpeech(text string) string {
	text = thinkBlockRe.ReplaceAllString(text, "")
	text = codeFenceRe.ReplaceAllString(text, "")
	text = mdLinkRe.ReplaceAllString(text, "$1")
	text = urlRe.ReplaceAllString(text, "")
	text = lineMarkerRe.ReplaceAllString(text, "")
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '*', r == '`', r == '~', r == '|':
			return -1
		case r >= 0x1F000, r >= 0x2600 && r <= 0x27BF, r == 0xFE0F, r == 0x200D:
			return -1
		}
		return r
	}, text)
	text = spacesRe.ReplaceAllString(text, " ")
	text = blankLinesRe.ReplaceAllString(text, "\n\n")
	text = strings.TrimSpace(text)

	if runes := []rune(text); len(runes) > maxSpeechRunes {
		cut := runes[:maxSpeechRunes]
		if i := lastSentenceEnd(cut); i > maxSpeechRunes/2 {
			cut = cut[:i+1]
		}
		text = strings.TrimSpace(string(cut))
	}
	return text
}

func lastSentenceEnd(r []rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		switch r[i] {
		case '.', '!', '?', '\n':
			return i
		}
	}
	return -1
}

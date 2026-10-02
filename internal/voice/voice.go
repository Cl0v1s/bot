// Package voice ajoute une entrée vocale locale au mode chat : un
// raccourci clavier global du bureau lance `bot voice toggle`, qui pilote
// via un socket unix la session `bot chat` en cours ; celle-ci enregistre
// le micro (pw-record), fait transcrire l'audio par un serveur STT local
// au format OpenAI, et injecte le texte obtenu comme une ligne tapée au
// clavier. Aucun focus du terminal n'est nécessaire : l'état est affiché
// en notifications de bureau.
package voice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
)

// Config paramètre une Session.
type Config struct {
	Enabled bool
	// SocketPath : "" = SocketPath().
	SocketPath string
	Recorder   Recorder
	STT        STT
	// TTS : lit à voix haute les réponses aux messages dictés (voir
	// Session.Speak). nil = pas de synthèse vocale.
	TTS *Speaker
}

type state int

const (
	stateIdle state = iota
	stateRecording
	stateTranscribing
)

func (s state) String() string {
	switch s {
	case stateRecording:
		return "recording"
	case stateTranscribing:
		return "transcribing"
	}
	return "idle"
}

// Session écoute les commandes du socket de contrôle et produit une
// transcription par enregistrement (Transcripts). Un seul enregistrement
// ou transcription à la fois.
type Session struct {
	cfg    Config
	ln     net.Listener
	path   string
	ctx    context.Context
	cancel context.CancelFunc
	notif  notifier

	transcripts chan string
	errs        chan error

	mu      sync.Mutex
	state   state
	stop    chan struct{} // fermé pour arrêter l'enregistrement en cours
	discard bool          // enregistrement en cours annulé (cancel) plutôt qu'arrêté
}

// Start ouvre le socket de contrôle et commence à écouter les commandes.
// Retourne ErrAlreadyRunning si une autre session l'utilise déjà.
func Start(ctx context.Context, cfg Config) (*Session, error) {
	path := cfg.SocketPath
	if path == "" {
		path = SocketPath()
	}
	ln, err := listen(path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Session{
		cfg:         cfg,
		ln:          ln,
		path:        path,
		ctx:         ctx,
		cancel:      cancel,
		transcripts: make(chan string, 8),
		errs:        make(chan error, 8),
	}
	go serve(ln, s.handle)
	return s, nil
}

// Transcripts : un texte par enregistrement transcrit avec succès (jamais
// vide).
func (s *Session) Transcripts() <-chan string { return s.transcripts }

// Errors : échecs d'enregistrement ou de transcription, à afficher.
func (s *Session) Errors() <-chan error { return s.errs }

// Done est fermé à la fermeture de la session.
func (s *Session) Done() <-chan struct{} { return s.ctx.Done() }

// Close arrête l'écoute, abandonne un éventuel enregistrement en cours et
// supprime le socket.
func (s *Session) Close() {
	s.cancel()
	s.ln.Close()
	_ = os.Remove(s.path)
}

// AskNotify signale en notification qu'une réponse oui/non est attendue :
// on n'a pas forcément le terminal sous les yeux quand on parle au bot.
func (s *Session) AskNotify(question string) {
	s.notif.show("bot : confirmation demandée", question+"\n\nRéponds « oui » ou « non » (raccourci vocal ou terminal).", false, true)
}

// Speak lit text à voix haute en tâche de fond (sans effet si la synthèse
// vocale n'est pas configurée). Un échec est signalé sur Errors().
func (s *Session) Speak(text string) {
	if s.cfg.TTS == nil {
		return
	}
	go func() {
		if err := s.cfg.TTS.Speak(s.ctx, text); err != nil {
			select {
			case s.errs <- fmt.Errorf("synthèse vocale : %w", err):
			default:
			}
		}
	}()
}

func (s *Session) handle(cmd string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch cmd {
	case "toggle":
		switch s.state {
		case stateIdle:
			s.startLocked()
		case stateRecording:
			close(s.stop)
		case stateTranscribing:
			s.notif.show("bot : occupé", "Transcription en cours, patiente un instant.", true, false)
		}
	case "start":
		if s.state == stateIdle {
			s.startLocked()
		}
	case "stop":
		if s.state == stateRecording {
			close(s.stop)
		} else {
			s.cfg.TTS.Stop()
		}
	case "cancel":
		if s.state == stateRecording {
			s.discard = true
			close(s.stop)
		} else {
			s.cfg.TTS.Stop()
		}
	case "status":
	default:
		return fmt.Sprintf("err commande inconnue %q (toggle, start, stop, cancel, status)", cmd)
	}
	return "ok " + s.state.String()
}

func (s *Session) startLocked() {
	s.state = stateRecording
	s.stop = make(chan struct{})
	s.discard = false
	// Sinon le micro enregistrerait la voix du bot.
	s.cfg.TTS.Stop()
	sound("audio-volume-change", 0)
	s.notif.show("bot : écoute…", "Parle, puis rappuie sur le raccourci (ou tais-toi).", true, false)
	go s.run(s.stop)
}

func (s *Session) setState(st state) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

func (s *Session) run(stop <-chan struct{}) {
	pcm, heard, err := s.cfg.Recorder.Record(s.ctx, stop)
	s.mu.Lock()
	discard := s.discard
	if err == nil && !discard && heard {
		s.state = stateTranscribing
	} else {
		s.state = stateIdle
	}
	s.mu.Unlock()
	switch {
	case errors.Is(err, context.Canceled):
		return
	case err != nil:
		s.fail(fmt.Errorf("enregistrement : %w", err))
		return
	case discard:
		s.notif.show("bot : annulé", "Enregistrement abandonné.", true, false)
		return
	case !heard:
		s.notif.show("bot : rien entendu", "Aucune voix détectée (voir VOICE_SILENCE_THRESHOLD si le micro est faible).", true, false)
		return
	}
	// Son de fin atténué : joué à plein volume, il était trop fort.
	sound("complete", -25)
	s.notif.show("bot : transcription…", "", true, false)
	stt := s.cfg.STT
	stt.OnRetry = func(attempt, max int, err error) {
		s.notif.show("bot : transcription…", fmt.Sprintf("Serveur indisponible, nouvel essai (%d/%d)…", attempt, max), true, false)
	}
	text, err := stt.Transcribe(s.ctx, encodeWAV(pcm))
	s.setState(stateIdle)
	if err != nil {
		if s.ctx.Err() == nil {
			s.fail(fmt.Errorf("transcription : %w", err))
		}
		return
	}
	if text == "" {
		s.notif.show("bot : rien compris", "La transcription est vide.", true, false)
		return
	}
	s.notif.show("bot : envoyé", text, true, false)
	select {
	case s.transcripts <- text:
	case <-s.ctx.Done():
	}
}

func (s *Session) fail(err error) {
	s.notif.show("bot : erreur vocale", err.Error(), false, false)
	select {
	case s.errs <- err:
	default:
	}
}

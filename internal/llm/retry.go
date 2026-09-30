package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// DefaultMaxRetries / DefaultRetryBackoff : nombre de relances d'une requête
// après une erreur transitoire (voir isTransient) et délai avant la première
// d'entre elles, doublé à chaque relance suivante (plafonné à
// maxRetryBackoff) — de quoi couvrir le redémarrage d'un serveur
// d'inférence local (1+2+4+8+16 s ≈ 30 s au total) sans faire attendre
// indéfiniment un serveur réellement arrêté.
const (
	DefaultMaxRetries   = 5
	DefaultRetryBackoff = time.Second
	maxRetryBackoff     = 30 * time.Second
)

// errStreamTruncated : le flux SSE s'est terminé sans "[DONE]" ni
// finish_reason — connexion fermée proprement par le serveur (ou un proxy)
// en pleine génération, que bufio.Scanner voit comme une fin de flux
// normale et non comme une erreur. Enveloppe io.ErrUnexpectedEOF pour être
// traitée comme tel par isTransient.
var errStreamTruncated = fmt.Errorf("flux LLM interrompu avant la fin de la réponse: %w", io.ErrUnexpectedEOF)

// statusError : réponse HTTP non-200 du serveur, dont le code permet de
// distinguer une indisponibilité passagère (502/503/504 : serveur en cours
// de (re)démarrage ou de chargement du modèle, proxy sans backend) d'un
// refus définitif de la requête.
type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("LLM a répondu avec le status %d: %s", e.status, e.body)
}

func isUnavailableStatus(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// isTransient indique si err justifie de rejouer la requête : connexion au
// serveur refusée/réinitialisée/coupée (y compris en plein streaming : EOF
// inattendu) ou indisponibilité HTTP passagère. Jamais pour une annulation
// ou un dépassement de délai (ctx, ou http.Client.Timeout — relancer ferait
// attendre à nouveau toute la durée du timeout).
func isTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		return isUnavailableStatus(se.status)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// RetryNotifier est appelé avant chaque relance d'une requête après une
// erreur transitoire : attempt (à partir de 1) sur max, et l'erreur qui l'a
// provoquée. Voir WithRetryNotifier.
type RetryNotifier func(attempt, max int, err error)

type retryNotifierKey struct{}

// WithRetryNotifier attache fn au contexte des requêtes : ChatCompletion et
// ChatCompletionStream l'appellent avant chaque relance, pour que
// l'appelant puisse le signaler (en streaming notamment, où le texte
// partiel déjà affiché va être suivi d'une nouvelle génération complète).
func WithRetryNotifier(ctx context.Context, fn RetryNotifier) context.Context {
	return context.WithValue(ctx, retryNotifierKey{}, fn)
}

// withRetry exécute do, puis le rejoue tant qu'il échoue sur une erreur
// transitoire, dans la limite de c.MaxRetries relances espacées d'un délai
// exponentiel. La requête est rejouée à l'identique (même historique) : la
// génération interrompue est perdue et reprise depuis le début, aucun
// serveur compatible OpenAI ne permettant de reprendre un flux en cours.
func (c *Client) withRetry(ctx context.Context, do func() (Message, Usage, error)) (Message, Usage, error) {
	res, usage, err := do()
	notify, _ := ctx.Value(retryNotifierKey{}).(RetryNotifier)
	backoff := c.RetryBackoff
	for attempt := 1; attempt <= c.MaxRetries && isTransient(err) && ctx.Err() == nil; attempt++ {
		if notify != nil {
			notify(attempt, c.MaxRetries, err)
		}
		select {
		case <-ctx.Done():
			return res, usage, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxRetryBackoff)
		res, usage, err = do()
	}
	return res, usage, err
}

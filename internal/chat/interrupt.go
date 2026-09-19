package chat

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// interruptController donne au Ctrl+C (et à SIGTERM) un comportement à deux
// vitesses, propre au mode interactif :
//   - reçu PENDANT une requête en cours (entre begin() et l'appel à sa
//     fonction de fin), il annule seulement cette requête ;
//   - reçu EN DEHORS (au prompt, en attente de saisie), il termine
//     immédiatement le programme.
//
// Nécessaire car signal.NotifyContext ne gère qu'un seul signal pour toute
// la durée du programme : une fois consommé pour annuler une requête, un
// second Ctrl+C ne faisait plus rien (le contexte déjà annulé cassait aussi
// silencieusement toutes les requêtes suivantes).
type interruptController struct {
	mu     sync.Mutex
	cancel context.CancelFunc // non nil pendant une requête en cours
}

// newInterruptController installe le gestionnaire de signaux. onExit (peut
// être nil) est appelé juste avant os.Exit dans le cas "arrêt immédiat" —
// utilisé pour restaurer le terminal (voir lineEditor.restore) puisque
// os.Exit saute les defer.
func newInterruptController(onExit func()) *interruptController {
	c := &interruptController{}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range ch {
			c.mu.Lock()
			cancel := c.cancel
			c.mu.Unlock()
			if cancel != nil {
				cancel()
				continue
			}
			if onExit != nil {
				onExit()
			}
			os.Exit(130) // 128+SIGINT, convention shell standard
		}
	}()
	return c
}

// begin dérive de parent un contexte pour la requête qui démarre, et arme
// son annulation par Ctrl+C. La fonction retournée doit toujours être
// appelée (sans defer dans une boucle : voir l'appelant) une fois la requête
// terminée, avant tout `continue`.
func (c *interruptController) begin(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	return ctx, func() {
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
		cancel()
	}
}

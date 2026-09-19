package chat

import "sync"

// terminalInput coordonne l'unique goroutine qui lit le terminal (voir la
// goroutine de lecture dans repl.go) avec les points du programme qui ont
// ponctuellement besoin de "voler" la prochaine ligne tapée comme réponse à
// une question précise (confirmation sandbox, permission de répertoire)
// plutôt que de la laisser partir dans la file d'attente des messages de
// chat. Un seul lecteur du terminal à la fois est indispensable : deux
// goroutines lisant le même fd en parallèle se voleraient des octets l'une à
// l'autre.
type terminalInput struct {
	mu      sync.Mutex
	waiting chan string // non-nil si quelqu'un attend la prochaine ligne comme réponse
}

// Ask enregistre une attente de réponse et retourne le canal sur lequel elle
// sera livrée (taille 1 : jamais bloquant côté émetteur, y compris si
// personne ne le lit finalement).
func (t *terminalInput) Ask() <-chan string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch := make(chan string, 1)
	t.waiting = ch
	return ch
}

// Dispatch reçoit une ligne fraîchement tapée. Si une réponse était
// attendue (Ask), elle lui est livrée et Dispatch retourne false : la ligne
// ne doit pas être traitée une deuxième fois. Sinon, retourne true : la
// ligne doit être traitée comme un message de chat normal par la boucle
// principale (immédiatement, ou mise en file d'attente si occupée).
func (t *terminalInput) Dispatch(line string) bool {
	t.mu.Lock()
	ch := t.waiting
	t.waiting = nil
	t.mu.Unlock()
	if ch == nil {
		return true
	}
	ch <- line
	return false
}

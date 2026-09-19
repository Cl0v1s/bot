package chat

import (
	"io"
	"sync"
)

// syncWriter sérialise les écritures concurrentes vers out. En mode
// interactif avec file d'attente (voir repl.go), la réponse en cours de
// streaming (tâche de fond), l'écho des caractères tapés (goroutine de
// lecture du terminal) et les messages de la boucle principale écrivent tous
// potentiellement en parallèle vers la même sortie : sans ce verrou, deux
// écritures pourraient s'entrelacer au milieu l'une de l'autre et corrompre
// l'affichage (par ex. une séquence ANSI coupée en deux).
type syncWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

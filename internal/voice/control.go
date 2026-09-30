package voice

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrAlreadyRunning : un autre processus (typiquement une autre session
// `bot chat`) écoute déjà sur le socket de contrôle.
var ErrAlreadyRunning = errors.New("une autre session écoute déjà les commandes vocales")

// SocketPath retourne le chemin du socket de contrôle, par lequel
// `bot voice <commande>` (lancé par exemple depuis un raccourci clavier
// global du bureau) pilote la session `bot chat` en cours : dans
// $XDG_RUNTIME_DIR (privé à l'utilisateur, vidé à la déconnexion), ou le
// répertoire temporaire à défaut.
func SocketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "bot-voice.sock")
}

// listen ouvre le socket de contrôle en path. Un fichier déjà présent
// auquel personne ne répond est un reste d'une session précédente mal
// terminée (kill -9, crash) : il est supprimé. S'il répond, une autre
// session est active : ErrAlreadyRunning, plutôt que de lui voler le socket.
func listen(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, ErrAlreadyRunning
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("suppression du socket périmé %s : %w", path, err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// serve traite les connexions de ln jusqu'à sa fermeture : une commande
// d'une ligne par connexion, à laquelle handle répond d'une ligne.
func serve(ln net.Listener, handle func(cmd string) string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			line, err := bufio.NewReader(c).ReadString('\n')
			if err != nil && line == "" {
				return
			}
			fmt.Fprintln(c, handle(strings.TrimSpace(line)))
		}(c)
	}
}

// Send envoie cmd (toggle, start, stop, cancel, status) à la session qui
// écoute sur path et retourne sa réponse.
func Send(path, cmd string) (string, error) {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintln(c, cmd); err != nil {
		return "", err
	}
	resp, err := bufio.NewReader(c).ReadString('\n')
	if err != nil && resp == "" {
		return "", err
	}
	return strings.TrimSpace(resp), nil
}

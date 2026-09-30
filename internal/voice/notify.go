package voice

import (
	"os/exec"
	"strings"
	"sync"
)

// Binaires des notifications et des sons ; "" = désactivé (tests).
var (
	notifySendBin = "notify-send"
	soundBin      = "canberra-gtk-play"
)

// notifier affiche l'état de la commande vocale en notifications de
// bureau (notify-send), en remplaçant toujours la même bulle plutôt que
// d'en empiler une par étape. Best-effort comme tools.notifyOS : ni
// notify-send absent ni absence de session graphique ne font échouer quoi
// que ce soit.
type notifier struct {
	mu sync.Mutex
	id string
}

// show remplace la bulle d'état. transient : ne pas la garder dans
// l'historique des notifications (étapes intermédiaires) ; critical :
// reste affichée jusqu'à ce qu'on la ferme (question en attente).
func (n *notifier) show(summary, body string, transient, critical bool) {
	if notifySendBin == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	args := []string{"-p", "-a", "bot", "-i", "audio-input-microphone"}
	if n.id != "" {
		args = append(args, "-r", n.id)
	}
	if transient {
		args = append(args, "-e")
	}
	if critical {
		args = append(args, "-u", "critical")
	}
	args = append(args, "--", summary, body)
	out, err := exec.Command(notifySendBin, args...).Output()
	if err != nil {
		return
	}
	if id := strings.TrimSpace(string(out)); id != "" {
		n.id = id
	}
}

// sound joue un son d'événement du thème système, s'il y a de quoi.
func sound(event string) {
	if soundBin == "" {
		return
	}
	if _, err := exec.LookPath(soundBin); err != nil {
		return
	}
	go func() { _ = exec.Command(soundBin, "-i", event).Run() }()
}

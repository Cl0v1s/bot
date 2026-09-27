package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"bot/internal/sandbox"
)

// DefaultWhitelistedCommandsFile est le chemin (relatif au workspace, comme
// DefaultAllowedDirsFile) du fichier de persistance des commandes
// "as_real_user" mises en liste blanche (voir CommandWhitelist). Protégé
// contre toute modification par le compte sandbox (voir
// sandbox.ReadTrustedFile) : sinon une commande sandboxée pourrait y ajouter
// ce qu'elle veut exécuter ensuite sous l'identité réelle.
const DefaultWhitelistedCommandsFile = sandbox.WhitelistedCommandsFileName

// CommandWhitelist mémorise, de façon persistante, des commandes run_shell
// EXACTES (chaîne complète, arguments compris, comparée octet pour octet)
// que l'utilisateur a autorisées une fois pour toutes à s'exécuter sous
// l'identité réelle (voir le paramètre "white_list" de ShellTool) : une
// commande qui diffère d'un seul caractère (argument, espace...) redemande
// confirmation normalement.
type CommandWhitelist struct {
	mu   sync.Mutex
	cmds map[string]bool
	path string // "" = pas de persistance (mémoire seule)
}

// NewCommandWhitelist charge la liste depuis path (fichier absent = liste
// vide). path vide = liste en mémoire seulement, pour la session.
func NewCommandWhitelist(path string) (*CommandWhitelist, error) {
	w := &CommandWhitelist{cmds: map[string]bool{}, path: path}
	return w, w.Refresh()
}

// Refresh recharge la liste depuis le fichier (no-op sans persistance) et la
// remplace intégralement : c'est le fichier qui fait foi, comme pour
// DirPermissions.Refresh. Permet au mode mail de voir les commandes ajoutées
// entre-temps en mode chat (et les révocations faites à la main dans le
// fichier). En cas d'erreur, la liste en mémoire est conservée.
func (w *CommandWhitelist) Refresh() error {
	if w == nil || w.path == "" {
		return nil
	}
	data, err := sandbox.ReadTrustedFile(w.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("lecture de la liste blanche %q: %w", w.path, err)
	}
	var cmds []string
	if strings.TrimSpace(string(data)) != "" {
		if err := json.Unmarshal(data, &cmds); err != nil {
			return fmt.Errorf("lecture de la liste blanche %q: contenu invalide: %w", w.path, err)
		}
	}
	m := make(map[string]bool, len(cmds))
	for _, c := range cmds {
		m[c] = true
	}
	w.mu.Lock()
	w.cmds = m
	w.mu.Unlock()
	return nil
}

// List retourne les commandes en liste blanche, triées. Sûr sur un receveur
// nil.
func (w *CommandWhitelist) List() []string {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	cmds := make([]string, 0, len(w.cmds))
	for c := range w.cmds {
		cmds = append(cmds, c)
	}
	w.mu.Unlock()
	sort.Strings(cmds)
	return cmds
}

// Contains indique si command (exacte) est en liste blanche. Sûr sur un
// receveur nil (toujours faux).
func (w *CommandWhitelist) Contains(command string) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cmds[command]
}

// Add ajoute command et persiste la liste. En cas d'échec d'écriture, la
// commande reste autorisée pour la session (seule la persistance est
// perdue) et l'erreur est retournée pour être journalisée.
func (w *CommandWhitelist) Add(command string) error {
	w.mu.Lock()
	w.cmds[command] = true
	cmds := make([]string, 0, len(w.cmds))
	for c := range w.cmds {
		cmds = append(cmds, c)
	}
	path := w.path
	w.mu.Unlock()
	if path == "" {
		return nil
	}
	// Même format et même écriture atomique que les répertoires autorisés.
	sort.Strings(cmds)
	return saveAllowedDirsFile(path, cmds)
}

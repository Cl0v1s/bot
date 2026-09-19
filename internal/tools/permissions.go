package tools

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DirGrantFunc demande à un humain l'autorisation d'accéder au répertoire
// abs (chemin absolu), avec une raison optionnelle fournie par le modèle.
// Retourne granted=true si l'accès est accordé. nil = pas d'humain
// disponible pour répondre (ex: mode mail non interactif) : toute nouvelle
// demande est automatiquement refusée, et rien n'est jamais ajouté à la
// liste.
//
// err doit être non nil UNIQUEMENT en cas d'échec technique pendant
// l'octroi (ex: échec du chgrp vers le groupe du sandbox) — jamais pour un
// simple refus de l'utilisateur (granted=false, err=nil). Cette distinction
// est essentielle : un échec technique doit remonter au modèle comme une
// erreur à comprendre/éventuellement réessayer, pas comme "l'utilisateur a
// refusé", qui est une tout autre situation.
type DirGrantFunc func(abs, reason string) (granted bool, err error)

// DefaultAllowedDirsFile est le chemin (relatif au répertoire de travail du
// programme) du fichier de persistance partagé entre le mode chat et le
// mode mail. Non configurable : ce n'est pas un réglage utilisateur, juste
// l'emplacement fixe de cet état partagé entre les deux invocations.
const DefaultAllowedDirsFile = ".bot_allowed_dirs.json"

// DirPermissions est le point d'application réel (dans le code, pas dans le
// prompt) du contrôle d'accès aux répertoires pour les tools fichiers. Par
// défaut, aucun répertoire n'est autorisé : un accès doit avoir été accordé
// explicitement via RequestAccess (ou déjà présent dans le fichier de
// persistance chargé au démarrage), ou faire partie des répertoires
// permanents (AlwaysAllow).
//
// La liste est dynamique et peut être partagée entre plusieurs invocations
// du programme via un fichier de persistance (voir WithPersistence) : le
// mode chat y ajoute les répertoires que l'utilisateur accorde en direct, et
// le mode mail relit ce même fichier à chaque cycle pour bénéficier des
// mêmes autorisations — mais ne peut jamais lui-même en ajouter (son
// DirGrantFunc est nil).
type DirPermissions struct {
	mu          sync.Mutex
	allowed     []string // chemins absolus, nettoyés — dynamique, remplacé par Refresh()
	always      []string // chemins absolus, nettoyés — permanents, jamais touchés par Refresh()
	grant       DirGrantFunc
	persistPath string
}

func NewDirPermissions(grant DirGrantFunc) *DirPermissions {
	return &DirPermissions{grant: grant}
}

// Interactive indique si un humain peut être sollicité pour accorder de
// nouveaux accès (true en mode chat, false en mode mail).
func (p *DirPermissions) Interactive() bool {
	return p != nil && p.grant != nil
}

// WithPersistence relie ces permissions à un fichier partagé entre les
// invocations du programme : la liste actuellement persistée y est chargée
// immédiatement, et tout accès accordé ultérieurement via RequestAccess
// (donc uniquement possible si un DirGrantFunc est configuré) y sera ajouté.
// Un appelant en lecture seule (ex: mode mail) doit utiliser Refresh()
// périodiquement pour voir les accès accordés ailleurs (ex: par le mode
// chat) sans jamais en écrire lui-même.
func (p *DirPermissions) WithPersistence(path string) error {
	if p == nil || path == "" {
		return nil
	}
	p.mu.Lock()
	p.persistPath = path
	p.mu.Unlock()
	return p.Refresh()
}

// Refresh recharge la liste des répertoires autorisés depuis le fichier de
// persistance (no-op si aucun n'est configuré) et la remplace intégralement
// : c'est le fichier qui fait foi. Sûr à appeler périodiquement (ex: avant
// chaque cycle de traitement des mails) pour capter les accès accordés par
// une autre invocation (ex: le mode chat) sans jamais en accorder soi-même.
func (p *DirPermissions) Refresh() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	path := p.persistPath
	p.mu.Unlock()
	if path == "" {
		return nil
	}

	dirs, err := loadAllowedDirsFile(path)
	if err != nil {
		return fmt.Errorf("lecture du fichier de permissions %q: %w", path, err)
	}

	p.mu.Lock()
	p.allowed = dirs
	p.mu.Unlock()
	return nil
}

// Preallow autorise des répertoires immédiatement, sans passer par le
// callback de confirmation ni par la persistance (utilisé pour des tests ou
// des cas d'usage ponctuels).
func (p *DirPermissions) Preallow(dirs ...string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if abs, err := filepath.Abs(d); err == nil {
			p.allowed = append(p.allowed, filepath.Clean(abs))
		}
	}
}

func (p *DirPermissions) isAllowedLocked(abs string) bool {
	for _, a := range p.allowed {
		if abs == a || strings.HasPrefix(abs, a+string(filepath.Separator)) {
			return true
		}
	}
	for _, a := range p.always {
		if abs == a || strings.HasPrefix(abs, a+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// AlwaysAllow accorde un accès permanent à dirs (ex: le workspace du bot),
// indépendant de la liste dynamique : contrairement à Preallow, ces chemins
// survivent à Refresh() (qui remplace intégralement la liste dynamique
// chargée depuis le fichier partagé) et ne sont jamais écrits dans ce
// fichier. À utiliser pour les répertoires que le bot doit pouvoir utiliser
// dans toutes les invocations (chat comme mail), sans dépendre d'une
// autorisation accordée en direct par l'utilisateur.
func (p *DirPermissions) AlwaysAllow(dirs ...string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if abs, err := filepath.Abs(d); err == nil {
			p.always = append(p.always, filepath.Clean(abs))
		}
	}
}

// CheckFile vérifie que path (fichier à lire ou écrire) se trouve dans un
// répertoire autorisé, et retourne son chemin absolu. Ne déclenche jamais de
// demande d'autorisation elle-même : c'est le rôle exclusif du tool
// request_directory_access, pour que la décision reste toujours explicite.
func (p *DirPermissions) CheckFile(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("chemin invalide %q: %w", path, err)
	}
	abs = filepath.Clean(abs)

	p.mu.Lock()
	ok := p.isAllowedLocked(abs)
	p.mu.Unlock()
	if !ok {
		return "", fmt.Errorf(
			`accès refusé : le répertoire %q n'est pas autorisé. Utilise l'outil "request_directory_access" pour demander l'accès à l'utilisateur avant de réessayer.`,
			filepath.Dir(abs),
		)
	}
	return abs, nil
}

// RequestAccess est le seul moyen d'ajouter un répertoire à la liste
// autorisée après le démarrage. Si le répertoire est déjà autorisé, retourne
// true immédiatement. Sinon, sollicite le callback de confirmation (s'il y
// en a un) ; toute demande sans callback disponible (mode mail) est
// automatiquement refusée, sans jamais toucher à la liste ni au fichier
// partagé.
func (p *DirPermissions) RequestAccess(dir, reason string) (bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, fmt.Errorf("chemin invalide %q: %w", dir, err)
	}
	abs = filepath.Clean(abs)

	p.mu.Lock()
	already := p.isAllowedLocked(abs)
	p.mu.Unlock()
	if already {
		return true, nil
	}

	if p.grant == nil {
		return false, nil
	}

	granted, grantErr := p.grant(abs, reason)
	if grantErr != nil {
		// Échec technique (ex: chgrp vers le groupe du sandbox) : distinct
		// d'un simple refus, remonté comme une vraie erreur au modèle.
		return false, grantErr
	}
	if !granted {
		return false, nil
	}

	p.mu.Lock()
	p.allowed = append(p.allowed, abs)
	dirsCopy := append([]string(nil), p.allowed...)
	path := p.persistPath
	p.mu.Unlock()

	if path != "" {
		if err := saveAllowedDirsFile(path, dirsCopy); err != nil {
			// L'octroi reste valable pour cette session même si la
			// persistance échoue (ex: disque en lecture seule) ; seul le
			// partage avec d'autres invocations (ex: le mode mail) est perdu.
			log.Printf("tools: échec de l'enregistrement du répertoire autorisé %q dans %q: %v", abs, path, err)
		}
	}

	return true, nil
}

func loadAllowedDirsFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	var dirs []string
	if err := json.Unmarshal(data, &dirs); err != nil {
		return nil, fmt.Errorf("contenu invalide: %w", err)
	}
	return dirs, nil
}

// saveAllowedDirsFile écrit atomiquement (fichier temporaire + rename) pour
// limiter le risque de lecture partielle par une autre invocation en cours
// (ex: le mode mail qui relit périodiquement ce même fichier).
func saveAllowedDirsFile(path string, dirs []string) error {
	if dirs == nil {
		dirs = []string{}
	}
	data, err := json.MarshalIndent(dirs, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

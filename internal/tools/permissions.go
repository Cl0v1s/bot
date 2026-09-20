package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"bot/internal/sandbox"
)

// sandboxReady/sandboxCanAccess indirectent sandbox.Ready/sandbox.CanAccess :
// des variables plutôt que des appels directs, pour que les tests puissent
// simuler un sandbox prêt ou non sans dépendre de l'état réel de la machine
// qui les exécute (qui peut très bien avoir un compte "llm" déjà configuré).
var (
	sandboxReady     = sandbox.Ready
	sandboxCanAccess = sandbox.CanAccess
)

// DirGrantFunc demande à un humain l'autorisation d'accéder au répertoire
// abs (chemin absolu), avec une raison optionnelle fournie par le modèle.
// Retourne granted=true si l'accès est accordé. nil = pas d'humain
// disponible pour répondre (ex: mode mail non interactif) : toute nouvelle
// demande est automatiquement refusée, et rien n'est jamais ajouté à la
// liste.
//
// ctx est celui de la requête en cours (voir Registry.Call) : une
// implémentation interactive doit le respecter (ex: abandonner l'attente de
// réponse si ctx est annulé par un Ctrl+C) plutôt que de bloquer
// indéfiniment sur une confirmation qui ne viendra jamais — sans quoi
// Ctrl+C n'a plus aucun effet tant qu'une telle confirmation est affichée.
//
// err doit être non nil UNIQUEMENT en cas d'échec technique pendant
// l'octroi (ex: échec du chgrp vers le groupe du sandbox) — jamais pour un
// simple refus de l'utilisateur (granted=false, err=nil). Cette distinction
// est essentielle : un échec technique doit remonter au modèle comme une
// erreur à comprendre/éventuellement réessayer, pas comme "l'utilisateur a
// refusé", qui est une tout autre situation.
type DirGrantFunc func(ctx context.Context, abs, reason string) (granted bool, err error)

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
		if abs, err := canonicalPath(d); err == nil {
			p.allowed = append(p.allowed, abs)
		}
	}
}

// canonicalPath retourne le chemin absolu de path, liens symboliques résolus
// sur la plus grande partie existante du chemin (comme `realpath -m`) —
// nécessaire sur un système où un chemin usuel (ex: /home/<user>) est en
// réalité un lien symbolique vers un autre emplacement (ex: /var/home/<user>,
// cas des systèmes ostree/Silverblue/uCore) : sans ça, un même répertoire
// réel accordé via un alias (ex: /var/home/... en mode chat) n'est pas
// reconnu comme déjà autorisé quand il est redemandé via l'autre alias (ex:
// /home/... depuis un mail), et inversement — refusant à tort un accès déjà
// donné. Le suffixe qui n'existe pas encore (ex: un nouveau fichier pour
// write_file) est conservé tel quel, non résolu.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)

	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}

	dir := filepath.Dir(abs)
	base := filepath.Base(abs)
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, base), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Racine atteinte sans rien pouvoir résoudre (chemin totalement
			// inexistant) : retombe sur le chemin nettoyé tel quel plutôt
			// que d'échouer.
			return abs, nil
		}
		base = filepath.Join(filepath.Base(dir), base)
		dir = parent
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

// checkAccess vérifie si dir (un répertoire, éventuellement pas encore créé
// — voir nearestExisting) est effectivement accessible. Si le sandbox est
// prêt, la vérité vient du système : sandboxCanAccess teste réellement, sous
// l'identité du compte sandbox, l'accès à dir — pas d'un fichier séparé
// censé refléter ce qui a été accordé, qui pourrait avoir divergé (accès
// révoqué manuellement, répertoire recréé, fichier corrompu...). Le
// répertoire accordé au sandbox l'est en lecture+écriture d'un coup (voir
// sandbox.GrantDirectory), donc write ne change que le bit testé, jamais le
// résultat en pratique.
//
// Sans sandbox prêt (désactivé, ou pas encore configuré), aucune identité
// séparée à interroger : on retombe sur la liste des répertoires
// explicitement accordés (fichier JSON partagé, voir isAllowedLocked) —
// seul mécanisme disponible dans ce cas.
func (p *DirPermissions) checkAccess(dir string, write bool) bool {
	if sandboxReady() {
		return sandboxCanAccess(nearestExisting(dir), write)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isAllowedLocked(dir)
}

// nearestExisting retourne path s'il existe, sinon son plus proche ancêtre
// existant — nécessaire pour tester un chemin que write_file s'apprête à
// créer (fichier et/ou répertoires parents), puisque tester l'accès à
// quelque chose qui n'existe pas encore échoue toujours.
func nearestExisting(path string) string {
	dir := path
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir // racine atteinte sans rien trouver ; le test échouera, ce qui est correct
		}
		dir = parent
	}
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
		if abs, err := canonicalPath(d); err == nil {
			p.always = append(p.always, abs)
		}
	}
}

// CheckFileRead vérifie que path (fichier à lire) se trouve dans un
// répertoire autorisé, et retourne son chemin absolu. Ne déclenche jamais de
// demande d'autorisation elle-même : c'est le rôle exclusif du tool
// request_directory_access, pour que la décision reste toujours explicite.
func (p *DirPermissions) CheckFileRead(path string) (string, error) {
	return p.checkFile(path, false)
}

// CheckFileWrite est l'équivalent de CheckFileRead pour un fichier à écrire
// (créer ou remplacer) : path lui-même n'a donc pas besoin d'exister déjà,
// contrairement à CheckFileRead — seul son répertoire (ou le plus proche
// ancêtre existant, voir nearestExisting) doit être autorisé.
func (p *DirPermissions) CheckFileWrite(path string) (string, error) {
	return p.checkFile(path, true)
}

func (p *DirPermissions) checkFile(path string, write bool) (string, error) {
	abs, err := canonicalPath(path)
	if err != nil {
		return "", fmt.Errorf("chemin invalide %q: %w", path, err)
	}

	if !p.checkAccess(filepath.Dir(abs), write) {
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
func (p *DirPermissions) RequestAccess(ctx context.Context, dir, reason string) (bool, error) {
	abs, err := canonicalPath(dir)
	if err != nil {
		return false, fmt.Errorf("chemin invalide %q: %w", dir, err)
	}

	if p.checkAccess(abs, false) {
		return true, nil
	}

	if p.grant == nil {
		return false, nil
	}

	granted, grantErr := p.grant(ctx, abs, reason)
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

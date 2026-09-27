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

// DefaultAllowedDirsFile est le nom (relatif au workspace, voir main.go) du
// fichier de persistance partagé entre le mode chat et le mode mail. Non
// configurable : ce n'est pas un réglage utilisateur, juste l'emplacement
// fixe de cet état partagé entre les deux invocations. Protégé contre toute
// modification par le compte sandbox, voir sandbox.ReadTrustedFile.
const DefaultAllowedDirsFile = sandbox.AllowedDirsFileName

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

	if err := checkNotProtected(abs); err != nil {
		return "", err
	}

	if !p.checkAccess(filepath.Dir(abs), write) {
		return "", fmt.Errorf(
			`accès refusé : le répertoire %q n'est pas autorisé. Utilise l'outil "request_directory_access" pour demander l'accès à l'utilisateur avant de réessayer.`,
			filepath.Dir(abs),
		)
	}

	// En écriture, le contrôle au niveau du fichier est fait par l'appelant
	// APRÈS resynchronisation (voir writeFileSynced) : un fichier de
	// l'utilisateur fraîchement créé hors du harnais n'est accessible au
	// compte sandbox qu'une fois son répertoire resynchronisé.
	if !write {
		if err := checkExistingFile(abs, false); err != nil {
			return "", err
		}
	}
	return abs, nil
}

// checkNotProtected refuse tout chemin dont un composant est protégé (voir
// sandbox.IsProtectedName : .env, fichiers d'état du harnais, ~/.ssh...),
// quel que soit le mode (sandbox ou non) et même dans un répertoire
// autorisé — y compris le workspace, qui les contient. Sans ça, read_file
// (qui s'exécute sous l'identité réelle) lirait .env alors que
// GrantDirectory le refuse justement au compte sandbox, et write_file
// pourrait modifier la liste blanche "as_real_user" ou les répertoires
// autorisés sans aucune confirmation humaine.
func checkNotProtected(abs string) error {
	for _, part := range strings.Split(abs, string(filepath.Separator)) {
		if sandbox.IsProtectedName(part) {
			return fmt.Errorf("accès refusé : %q est un fichier protégé (configuration, état du harnais ou secret de l'utilisateur), jamais accessible aux outils du modèle — ne réessaie pas", abs)
		}
	}
	return nil
}

// checkExistingFile vérifie, en mode sandbox uniquement, que le fichier abs
// lui-même (s'il existe) est accessible au compte sandbox, pas seulement son
// répertoire : read_file/write_file s'exécutent sous l'identité réelle, et
// sans ce contrôle un fichier de l'utilisateur en 0600 situé dans un
// répertoire lisible par le compte sandbox (/tmp, /etc, un dossier 755...)
// serait lu ou réécrit alors que le compte sandbox lui-même n'y a pas accès.
// Sans sandbox, pas d'identité séparée à interroger : no-op.
func checkExistingFile(abs string, write bool) error {
	if !sandboxReady() {
		return nil
	}
	if _, err := os.Lstat(abs); err != nil {
		return nil // fichier à créer : seul le répertoire compte
	}
	if !sandboxCanAccess(abs, write) {
		mode := "lecture"
		if write {
			mode = "écriture"
		}
		return fmt.Errorf("accès refusé : le fichier %q n'est pas accessible en %s au compte sandbox %q (droits du fichier lui-même, ex: 600), même si son répertoire l'est", abs, mode, sandbox.User)
	}
	return nil
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

	if err := checkNotProtected(abs); err != nil {
		return false, err
	}
	if err := checkNotTooBroad(abs); err != nil {
		return false, err
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

	// Vérification post-octroi, en mode sandboxé uniquement : p.grant (via
	// sandbox.GrantDirectory, dans l'implémentation du mode chat) peut
	// annoncer un succès (err == nil) sans que l'accès réel du compte
	// sandbox à abs en résulte effectivement — constaté en pratique : le
	// répertoire restait inaccessible en écriture juste après un "accès
	// accordé", pour une cause qui n'a pas pu être identifiée avec
	// certitude (pas une erreur reproductible de GrantDirectory lui-même).
	// Sans ce contrôle, l'incohérence n'apparaît qu'au prochain read_file/
	// write_file/run_shell, avec un message qui ne permet pas de deviner
	// que l'octroi qui semblait avoir réussi n'a en fait rien changé. Pas de
	// vérification équivalente en mode non sandboxé : là, checkAccess ne
	// teste rien d'autre que l'appartenance à p.allowed, qu'on est justement
	// en train de construire — il n'y a rien d'indépendant à vérifier.
	if sandboxReady() {
		nearest := nearestExisting(abs)
		if !sandboxCanAccess(nearest, false) || !sandboxCanAccess(nearest, true) {
			return false, fmt.Errorf(
				"octroi accepté mais l'accès réel (lecture/écriture) du compte sandbox à %q échoue toujours juste après — incohérence à diagnostiquer plutôt qu'un octroi silencieusement inefficace ; réessayer peut suffire si c'était transitoire",
				abs,
			)
		}
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

// tooBroadDirs : répertoires système jamais accordables tels quels (voir
// checkNotTooBroad) — leurs sous-répertoires restent accordables.
var tooBroadDirs = []string{
	"/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt",
	"/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/tmp", "/usr", "/var",
	"/var/home", "/Users", "/Applications", "/Library", "/System", "/private",
}

// checkNotTooBroad refuse d'accorder la racine, un répertoire système, le
// répertoire personnel de l'utilisateur ou l'un de ses ancêtres : en mode
// sandbox, un octroi rend TOUT le contenu accessible au compte sandbox
// (chgrp + g+rw récursif, voir sandbox.GrantDirectory) — ~/.ssh, ~/.config,
// ~/.gnupg... Cas typique : run_shell demande l'accès au répertoire de
// travail du bot, lancé depuis $HOME. Une seule réponse "o" ne doit jamais
// pouvoir avoir cette portée ; le modèle doit demander un sous-répertoire
// précis.
func checkNotTooBroad(abs string) error {
	broad := false
	for _, d := range tooBroadDirs {
		if c, err := canonicalPath(d); err == nil && c == abs {
			broad = true
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if h, err := canonicalPath(home); err == nil && (h == abs || strings.HasPrefix(h, abs+string(filepath.Separator))) {
			broad = true
		}
	}
	if !broad {
		return nil
	}
	return fmt.Errorf("accès refusé sans demander : %q est trop large (racine, répertoire système, répertoire personnel ou l'un de ses parents) — demande l'accès à un sous-répertoire précis (ex: le dossier du projet concerné). Pour run_shell, le bot doit être lancé depuis ce sous-répertoire", abs)
}

func loadAllowedDirsFile(path string) ([]string, error) {
	data, err := sandbox.ReadTrustedFile(path)
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

// saveAllowedDirsFile écrit atomiquement (voir sandbox.WriteTrustedFile) pour
// limiter le risque de lecture partielle par une autre invocation en cours
// (ex: le mode mail qui relit périodiquement ce même fichier), et sans
// jamais suivre un fichier/lien préparé par le compte sandbox.
func saveAllowedDirsFile(path string, dirs []string) error {
	if dirs == nil {
		dirs = []string{}
	}
	data, err := json.MarshalIndent(dirs, "", "  ")
	if err != nil {
		return err
	}
	return sandbox.WriteTrustedFile(path, data)
}

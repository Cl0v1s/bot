package sandbox

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// GrantDirectory change le GROUPE de dir et de tout son contenu vers Group
// (récursivement), et ajoute les droits de lecture/écriture/exécution pour
// ce groupe, afin qu'une commande exécutée en tant que User (voir
// WrapCommand) puisse y accéder. Le bit setgid est posé sur dir lui-même
// pour que les fichiers créés ultérieurement à l'intérieur héritent
// automatiquement du groupe.
//
// Ne touche JAMAIS au propriétaire (chown) : seul os.Chown(..., -1, gid) est
// utilisé, uid=-1 signifiant explicitement "ne pas changer". Ne nécessite
// aucun privilège particulier tant que l'appelant est déjà propriétaire de
// ces fichiers et membre du groupe Group (mis en place par Ensure).
//
// Un fichier possédé par quelqu'un d'autre que l'appelant n'est ni chgrp ni
// chmod directement (ça exigerait d'être root ou propriétaire) — mais si ce
// propriétaire est justement User (cas concret et quasi unique ici : un
// fichier créé ou réécrit par run_shell, voir WrapCommand), on peut lui
// demander de s'ouvrir lui-même le bit d'écriture groupe, via le même sudo
// déjà utilisé pour WrapCommand (voir ensureGroupWritableAsUser) : sans ça,
// un tel fichier resterait accessible à User mais pas à l'utilisateur réel
// via write_file — un umask permissif (voir le commentaire de WrapCommand)
// n'aide pas quand l'outil qui a écrit ce fichier a demandé un mode
// explicite (ex: 0644, comme le fait couramment un éditeur ou un outil qui
// recrée un fichier plutôt que d'en modifier le contenu en place) : il n'y a
// alors aucun bit d'écriture groupe à laisser passer, umask ou pas.
// Tout autre propriétaire (ni l'appelant, ni User — cas rare, ex: un fichier
// système croisé en travaillant dans un dossier partagé) reste ignoré comme
// avant : ni chgrp/chmod possible, ni sudo applicable, mais aussi sans
// conséquence pour cet appelant précis.
//
// Plus généralement, AUCUNE erreur sur un fichier précis (chown/chmod qui
// échoue, propriétaire inattendu...) n'est jamais remontée comme faisant
// échouer tout GrantDirectory : elle est journalisée et le parcours
// continue. Nécessaire y compris pour un fichier qu'on POSSÈDE et qu'on
// pourrait légitimement modifier : filepath.WalkDir s'arrête net à la
// première erreur renvoyée par son callback, et un chemin comme /tmp est
// partagé avec potentiellement de nombreux autres processus qui créent et
// suppriment sans arrêt des fichiers sans rapport avec nous (observé en
// pratique : une course avec un fichier temporaire d'une application tierce
// a fait échouer, avant ce correctif, la synchronisation d'un fichier qui
// nous intéressait vraiment, situé plus loin dans le même parcours).
func GrantDirectory(dir string) error {
	grp, err := user.LookupGroup(Group)
	if err != nil {
		return fmt.Errorf("groupe %q introuvable (le sandbox a-t-il été configuré ?): %w", Group, err)
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		return fmt.Errorf("gid invalide pour le groupe %q: %w", Group, err)
	}

	// Étape faite en premier et sans effet de bord sur dir lui-même : si elle
	// échoue, on ne veut pas avoir déjà modifié dir (chgrp/setgid) tout en
	// annonçant un refus — voir le commentaire d'ensureGroupTraverse. Ouvrir
	// dir à Group ne suffit pas : le processus qui tourne en tant que User
	// doit aussi pouvoir *traverser* chaque répertoire parent pour y arriver
	// (ex: getcwd()/chdir() échoue avec "Permission denied" si un répertoire
	// ancêtre comme le home de l'utilisateur est en 750). On ajoute donc
	// uniquement le droit de traversée (exécution, jamais lecture/écriture)
	// pour Group sur chaque ancêtre, sans changer leur propriétaire ni leur
	// groupe principal.
	for _, ancestor := range ancestors(dir) {
		if err := ensureGroupTraverse(ancestor); err != nil {
			return fmt.Errorf("octroi du droit de traversée sur %q: %w", ancestor, err)
		}
	}

	// Rempli pendant le parcours (voir plus bas), traité en une poignée
	// d'appels sudo groupés APRÈS le parcours plutôt qu'un par fichier
	// pendant celui-ci — voir needsGroupWriteAsUser.
	var ownedByUserNeedingFix []string
	sandboxUID, sandboxUIDErr := lookupSandboxUID()

	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Seule une erreur sur dir lui-même fait échouer l'octroi : pour
			// tout autre chemin (sous-répertoire illisible, entrée disparue
			// entre le listing et l'appel...), on journalise et on continue,
			// comme documenté plus haut — sinon WalkDir s'arrête net et laisse
			// tout le reste de l'arborescence sans les droits nécessaires
			// (observé sur macOS : octroi de ~/Documents interrompu à mi-
			// parcours, sans setgid posé sur la racine).
			if path == dir {
				return err
			}
			log.Printf("sandbox: %q ignoré (best-effort) : %v", path, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if isProtectedFromGrant(d.Name()) {
			return nil
		}

		// Un lien symbolique n'est jamais suivi : os.Chown/os.Chmod
		// agiraient sur sa CIBLE, potentiellement hors du répertoire accordé
		// (ex: node_modules -> ~/.meteor/..., fréquent dans un projet JS),
		// ouvrant au compte sandbox un chemin que l'utilisateur n'a jamais
		// accordé. Seul le groupe du lien lui-même est changé (Lchown) ; ses
		// droits n'ont aucun effet sur macOS comme sur Linux.
		if d.Type()&fs.ModeSymlink != 0 {
			if !ownedByMe(d) {
				return nil
			}
			if err := os.Lchown(path, -1, gid); err != nil {
				log.Printf("sandbox: changement de groupe du lien %q ignoré (best-effort) : %v", path, err)
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			log.Printf("sandbox: %q ignoré (best-effort) : %v", path, err)
			return nil
		}

		// .sandbox-home est le HOME dédié de User (voir home.go) : lui-même
		// (ce répertoire) a besoin du droit d'écriture groupe pour que User
		// puisse y créer des fichiers/sous-répertoires — mais SON CONTENU,
		// lui, ne doit JAMAIS être touché : des outils comme glab refusent
		// purement et simplement de fonctionner si leur fichier de
		// config/identifiants a des permissions plus larges que 600
		// (observé en pratique avec ~/.config/glab-cli/config.yml, remis en
		// 660 par ce parcours avant ce correctif) — même logique que ssh ou
		// gpg pour leurs propres fichiers sensibles. L'utilisateur réel n'a
		// de toute façon aucun besoin d'accéder au contenu du HOME de User
		// (configs/caches internes à SES outils à lui, jamais consultés via
		// write_file/read_file).
		skipContent := d.IsDir() && d.Name() == sandboxHomeDirName

		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
			if sandboxUIDErr == nil && int(st.Uid) == sandboxUID && needsGroupWriteAsUser(info) {
				ownedByUserNeedingFix = append(ownedByUserNeedingFix, path)
			}
			if skipContent {
				return fs.SkipDir
			}
			return nil
		}

		// best-effort à partir d'ici, pour CE chemin précis : un chown/chmod
		// qui échoue (ex: le fichier vient de disparaître entre le listing
		// et l'appel — TOCTOU banal dans un dossier partagé et volatil comme
		// /tmp, où d'autres processus créent/suppriment sans arrêt des
		// fichiers qui n'ont rien à voir avec nous) ne doit jamais
		// interrompre tout le parcours (filepath.WalkDir s'arrête net à la
		// première erreur remontée par le callback) : ça laisserait le
		// reste de l'arborescence — tout ce qui vient après, dans l'ordre
		// de parcours, potentiellement le fichier qu'on voulait vraiment
		// accorder — sans les droits nécessaires, à cause d'un fichier sans
		// rapport. Observé en pratique : une course avec un cookie temporaire
		// Steam dans /tmp a empêché la resynchronisation d'un fichier
		// fraîchement écrit par write_file juste après lui dans le parcours.
		if err := os.Chown(path, -1, gid); err != nil {
			log.Printf("sandbox: changement de groupe de %q ignoré (best-effort) : %v", path, err)
			if skipContent {
				return fs.SkipDir
			}
			return nil
		}

		mode := info.Mode().Perm()
		if d.IsDir() {
			mode |= 0o070 // rwx pour le groupe
		} else {
			mode |= 0o060        // rw pour le groupe
			if mode&0o100 != 0 { // exécutable pour le propriétaire -> aussi pour le groupe
				mode |= 0o010
			}
		}
		if err := os.Chmod(path, mode); err != nil {
			log.Printf("sandbox: changement des droits de %q ignoré (best-effort) : %v", path, err)
		}
		if skipContent {
			return fs.SkipDir
		}
		return nil
	}); err != nil {
		return err
	}

	// Best-effort, comme documenté plus haut : un échec ici ne doit jamais
	// faire échouer tout GrantDirectory.
	_ = ensureGroupWritableAsUser(ownedByUserNeedingFix)

	// setgid sur le répertoire racine accordé : les fichiers créés dedans
	// par la suite héritent automatiquement du groupe Group.
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if err := os.Chmod(dir, info.Mode().Perm()|os.ModeSetgid); err != nil {
		return fmt.Errorf("pose du bit setgid sur %q: %w", dir, err)
	}

	return nil
}

// ownedByMe indique si d (sans suivre un éventuel lien symbolique)
// appartient à l'utilisateur effectif du processus courant.
func ownedByMe(d fs.DirEntry) bool {
	info, err := d.Info()
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// lookupSandboxUID retourne l'UID numérique de User (voir sandbox.go), pour
// distinguer, dans GrantDirectory, "possédé par User lui-même" (cas où
// ensureGroupWritableAsUser peut aider) de "possédé par un tiers
// quelconque" (cas où rien n'est possible sans privilège particulier).
func lookupSandboxUID() (int, error) {
	u, err := user.Lookup(User)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(u.Uid)
}

// needsGroupWriteAsUser indique si info (déjà connu comme possédé par User,
// voir l'appelant) manque du bit d'écriture groupe — évite de retraiter à
// chaque appel de GrantDirectory des fichiers déjà corrigés par un appel
// précédent (voir ensureGroupWritableAsUser : un dépôt git à l'intérieur
// d'un dossier accordé, par exemple, peut compter des milliers d'objets
// possédés par User une fois cloné via run_shell — sans ce filtre, chaque
// futur appel à GrantDirectory sur ce même dossier retraiterait tous ces
// objets pour rien).
func needsGroupWriteAsUser(info os.FileInfo) bool {
	return info.Mode().Perm()&0o020 == 0
}

// ensureGroupWritableAsUser demande à User (via le même sudo -n -u utilisé
// pour WrapCommand, sans mot de passe) d'ajouter lui-même le bit d'écriture
// groupe sur chacun de paths, des fichiers/répertoires qu'il possède déjà :
// GrantDirectory, lui, tourne sous l'identité réelle de l'utilisateur et ne
// peut PAS le faire directement (chmod exige d'être propriétaire du fichier,
// ou root) — mais User, propriétaire, l'est. No-op si paths est vide.
//
// Groupé en un minimum d'appels sudo plutôt qu'un par fichier — nécessaire
// en pratique : un dépôt git cloné via run_shell à l'intérieur d'un dossier
// accordé compte facilement plusieurs milliers d'objets possédés par User,
// et lancer un sous-processus sudo par fichier rendait GrantDirectory (donc
// write_file et run_shell, qui l'appellent à chaque invocation) visiblement
// bloqué le temps de tous les lancer un par un. batchSize garde chaque
// ligne de commande à une taille raisonnable (bien en dessous d'ARG_MAX),
// pas pour la vitesse de sudo lui-même.
//
// "g+rwX" plutôt que "g+rw" : le X majuscule n'ajoute le bit d'exécution
// pour le groupe que si le chemin est un répertoire ou déjà exécutable pour
// au moins une catégorie — même logique que la pose de mode faite plus haut
// dans GrantDirectory pour un fichier possédé par l'appelant, appliquée ici
// en une commande plutôt que répliquée en Go.
func ensureGroupWritableAsUser(paths []string) error {
	const batchSize = 200
	for start := 0; start < len(paths); start += batchSize {
		end := min(start+batchSize, len(paths))
		batch := paths[start:end]

		args := append([]string{"-n", "-u", User, "--", "chmod", "g+rwX"}, batch...)
		out, err := exec.Command("sudo", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("chmod de %d chemin(s) en tant que %q: %s: %w", len(batch), User, strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

// isProtectedFromGrant indique si name (nom de base, pas chemin complet) ne
// doit JAMAIS être touché par GrantDirectory, quel que soit le dossier
// accordé — y compris le workspace lui-même, dont ConfigFilePath place le
// sien à sa racine. ".env" porte les vrais identifiants du bot (clé API
// LLM, identifiants mail...) : le compte sandbox est justement là pour
// isoler ce qu'une commande shell pilotée par le modèle peut atteindre, lui
// accorder l'accès à ce fichier en effet de bord d'une synchronisation de
// dossier annulerait cette isolation, sans aucune raison fonctionnelle
// (aucun outil n'a besoin d'y toucher).
func isProtectedFromGrant(name string) bool {
	return name == ".env"
}

// ancestors retourne les répertoires parents de dir, du plus proche (parent
// direct) jusqu'à la racine du système de fichiers.
func ancestors(dir string) []string {
	var out []string
	d := filepath.Dir(dir)
	for {
		out = append(out, d)
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return out
}

// alreadyTraversable indique si path est déjà traversable par tout le monde
// (bit exécution "other"), auquel cas aucune ACL supplémentaire n'est
// nécessaire — cas courant pour des répertoires comme "/" ou "/Users".
func alreadyTraversable(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0o001 != 0, nil
}

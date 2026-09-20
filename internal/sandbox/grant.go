package sandbox

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
// Un fichier déjà possédé par quelqu'un d'autre est ignoré (ni chgrp ni
// chmod), plutôt que de faire échouer tout l'appel : chgrp/chmod exigent
// d'être propriétaire du fichier (ou root), donc impossibles à appliquer
// sans privilège particulier — mais aussi inutiles, son propriétaire ayant
// déjà pleinement accès. Cas concret : une fois dir accordé, User lui-même
// (voir WrapCommand) peut y avoir écrit des fichiers via run_shell, qui lui
// appartiennent alors en propre. Sans ce contournement, un seul tel fichier
// interromprait tout le parcours (filepath.WalkDir s'arrête à la première
// erreur) et laisserait le reste de l'arborescence — tout ce qui vient
// après, dans l'ordre de parcours — sans les droits nécessaires pour User.
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

	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if isProtectedFromGrant(d.Name()) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
			return nil
		}

		if err := os.Chown(path, -1, gid); err != nil {
			return fmt.Errorf("changement de groupe de %q: %w", path, err)
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
			return fmt.Errorf("changement des droits de %q: %w", path, err)
		}
		return nil
	}); err != nil {
		return err
	}

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

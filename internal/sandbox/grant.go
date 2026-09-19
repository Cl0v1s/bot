package sandbox

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
		if err := os.Chown(path, -1, gid); err != nil {
			return fmt.Errorf("changement de groupe de %q: %w", path, err)
		}

		info, err := d.Info()
		if err != nil {
			return err
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

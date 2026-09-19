//go:build linux

package sandbox

import (
	"fmt"
	"os/exec"
	"strings"
)

// ensureGroupTraverse ajoute, via une ACL POSIX (setfacl), le droit de
// traversée (x) pour le groupe Group sur path — sans modifier son
// propriétaire, son groupe principal, ni ses droits de lecture/écriture. Ne
// nécessite pas sudo tant que l'appelant possède déjà path (cas normal :
// répertoires de l'utilisateur qui a lancé le harnais). Nécessite le paquet
// "acl" (setfacl) installé et un système de fichiers monté avec le support
// des ACL POSIX (par défaut sur la plupart des distributions récentes).
func ensureGroupTraverse(path string) error {
	ok, err := alreadyTraversable(path)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}

	out, err := exec.Command("setfacl", "-m", "g:"+Group+":x", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("setfacl (le paquet \"acl\" est-il installé ?): %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

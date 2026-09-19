//go:build darwin

package sandbox

import (
	"fmt"
	"os/exec"
	"strings"
)

// ensureGroupTraverse ajoute, via une ACL macOS (chmod +a), le droit de
// traversée ("search") pour le groupe Group sur path — sans modifier son
// propriétaire, son groupe principal, ni ses droits de lecture/écriture. Ne
// nécessite pas sudo tant que l'appelant possède déjà path (cas normal :
// répertoires de l'utilisateur qui a lancé le harnais).
func ensureGroupTraverse(path string) error {
	ok, err := alreadyTraversable(path)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}

	out, err := exec.Command("chmod", "+a", "group:"+Group+" allow search", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chmod +a: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ensureGroupReadable ajoute, via une ACL macOS (chmod +a), le droit de
// lecture ("read") pour le groupe Group sur path — sans changer son
// propriétaire ni ses droits existants. Contrairement à ensureGroupTraverse,
// jamais utilisé sur un répertoire entier : uniquement sur un fichier précis
// (voir EnsureSSHKeyAccess). Toujours (ré)appliqué, sans vérification
// préalable : idempotent.
func ensureGroupReadable(path string) error {
	out, err := exec.Command("chmod", "+a", "group:"+Group+" allow read", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chmod +a: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

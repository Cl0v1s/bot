package sandbox

import (
	"fmt"
	"path/filepath"
)

// EnsureSSHKeyAccess donne à User un accès en lecture seule à keyPath (une
// clé privée SSH de l'utilisateur réel) : traversée de chaque répertoire
// ancêtre (typiquement ~/.ssh, en 700 par défaut) puis lecture du fichier
// lui-même — jamais d'écriture, jamais de changement de propriétaire. Pas
// d'effet si keyPath est vide (voir config.SandboxSSHKey).
//
// Implication de sécurité assumée à la demande explicite de l'opérateur :
// User peut désormais s'authentifier en SSH avec l'identité réelle de
// l'utilisateur pour tout ce qu'il exécute (typiquement git push/pull sur un
// dépôt distant) — ce qui affaiblit l'isolation du sandbox spécifiquement
// pour les opérations SSH/git, au bénéfice de pouvoir effectivement
// s'authentifier auprès d'un serveur distant qui l'exige. Voir
// EnsureGitConfig, qui pointe core.sshCommand vers cette clé.
func EnsureSSHKeyAccess(keyPath string) error {
	if keyPath == "" {
		return nil
	}
	dir := filepath.Dir(keyPath)
	for _, ancestor := range append([]string{dir}, ancestors(dir)...) {
		if err := ensureGroupTraverse(ancestor); err != nil {
			return fmt.Errorf("octroi du droit de traversée sur %q: %w", ancestor, err)
		}
	}
	if err := ensureGroupReadable(keyPath); err != nil {
		return fmt.Errorf("octroi de la lecture de %q: %w", keyPath, err)
	}
	return nil
}

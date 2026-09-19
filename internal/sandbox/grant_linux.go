//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// ensureGroupTraverse ajoute, via une ACL POSIX (setfacl), le droit de
// traversée (x) pour le groupe Group sur path — sans modifier son
// propriétaire, son groupe principal, ni ses droits de lecture/écriture. Ne
// nécessite pas sudo tant que l'appelant possède déjà path (cas normal :
// répertoires de l'utilisateur qui a lancé le harnais). Nécessite le paquet
// "acl" (setfacl) installé et un système de fichiers monté avec le support
// des ACL POSIX (par défaut sur la plupart des distributions récentes).
//
// Cas particulier : un ancêtre appartenant à un autre utilisateur (ex: un
// point de montage système comme /run/media/<user>, root:root 0750, pour un
// média externe) — setfacl y échoue systématiquement avec "Opération non
// permise" puisque ni la propriété ni CAP_FOWNER ne sont réunis. Plutôt que
// de tenter puis d'échouer avec un message setfacl brut, on vérifie d'abord
// si User peut déjà le traverser tel quel (llmCanTraverse : robuste aussi à
// une ACL déjà posée manuellement), et sinon on détecte ce cas précis pour
// donner une remédiation actionnable (l'utilisateur courant, lui, a
// généralement les droits sudo pour le faire une fois lui-même).
func ensureGroupTraverse(path string) error {
	ok, err := alreadyTraversable(path)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if llmCanTraverse(path) {
		return nil
	}

	if owned, err := ownedByCurrentUser(path); err == nil && !owned && os.Geteuid() != 0 {
		return fmt.Errorf(
			"%q appartient à un autre utilisateur (probablement un point de montage système, ex: média externe) : "+
				"impossible d'y ajouter le droit de traversée pour le groupe %q sans en être propriétaire ni root. "+
				"À corriger une seule fois, avec vos propres droits sudo : sudo setfacl -m g:%s:x %s",
			path, Group, Group, path,
		)
	}

	out, err := exec.Command("setfacl", "-m", "g:"+Group+":x", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("setfacl (le paquet \"acl\" est-il installé ?): %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// llmCanTraverse indique si User peut déjà traverser path, en le testant
// directement sous son identité (via le sudo -n -u déjà en place, voir
// WrapCommand) plutôt qu'en le déduire des seuls bits classiques (voir
// alreadyTraversable) : couvre aussi bien le bit "autres" qu'une ACL déjà
// présente (posée manuellement, ou par une exécution précédente).
func llmCanTraverse(path string) bool {
	return exec.Command("sudo", "-n", "-u", User, "--", "test", "-x", path).Run() == nil
}

// ownedByCurrentUser indique si path appartient à l'utilisateur effectif du
// processus courant — utilisé uniquement pour distinguer, dans
// ensureGroupTraverse, un futur échec setfacl inévitable (ancêtre appartenant
// à quelqu'un d'autre) d'un échec pour une autre raison (ACL non supportée,
// paquet manquant...).
func ownedByCurrentUser(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true, nil
	}
	return int(st.Uid) == os.Geteuid(), nil
}

// ensureGroupReadable ajoute, via une ACL POSIX (setfacl), le droit de
// lecture (r) pour le groupe Group sur path — sans changer son propriétaire
// ni ses droits existants. Contrairement à ensureGroupTraverse, jamais
// utilisé sur un répertoire entier : uniquement sur un fichier précis (voir
// EnsureSSHKeyAccess). Toujours (ré)appliqué, sans vérification préalable :
// idempotent, et un fichier de clé privée n'est jamais déjà lisible par
// tout le monde (donc pas de raccourci équivalent à alreadyTraversable).
func ensureGroupReadable(path string) error {
	out, err := exec.Command("setfacl", "-m", "g:"+Group+":r", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("setfacl (le paquet \"acl\" est-il installé ?): %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

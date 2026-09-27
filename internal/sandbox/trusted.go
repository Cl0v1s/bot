package sandbox

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Noms des fichiers d'état du harnais, tous à la racine du workspace. Le
// workspace est accordé à User (voir GrantDirectory) : sans protection
// particulière, une commande run_shell sandboxée pourrait les réécrire — ou,
// le répertoire lui étant inscriptible, les supprimer puis les recréer — et
// ainsi s'accorder elle-même des droits réservés à l'utilisateur réel (ex:
// ajouter une commande à la liste blanche "as_real_user", exécutée ensuite
// sous l'identité réelle sans confirmation en mode mail ; ou désactiver
// SANDBOX_USER_ENABLED dans .env pour le prochain lancement). D'où
// ReadTrustedFile/WriteTrustedFile, et leur exclusion de GrantDirectory et
// des outils fichiers (voir IsProtectedName).
const (
	ConfigFileName              = ".env"
	AllowedDirsFileName         = ".bot_allowed_dirs.json"
	WhitelistedCommandsFileName = ".bot_whitelisted_commands.json"
)

// protectedNames : noms de base (fichier ou répertoire) jamais accordés au
// compte sandbox par GrantDirectory, et jamais lus/écrits par les outils
// fichiers du modèle (voir tools.DirPermissions), quel que soit le
// répertoire où ils se trouvent : fichiers d'état du harnais (voir
// ci-dessus), et secrets usuels de l'utilisateur qu'un octroi un peu large
// (ex: un dossier de projet contenant un .netrc) ne doit jamais exposer.
var protectedNames = map[string]bool{
	ConfigFileName:              true,
	AllowedDirsFileName:         true,
	WhitelistedCommandsFileName: true,
	".ssh":                      true,
	".gnupg":                    true,
	".netrc":                    true,
	".git-credentials":          true,
}

// IsProtectedName indique si name (nom de base, pas chemin complet) désigne
// un fichier ou répertoire protégé (voir protectedNames). Couvre aussi les
// fichiers temporaires d'écriture atomique des fichiers d'état
// (".bot_allowed_dirs.json.tmp", ou ceux de WriteTrustedFile).
func IsProtectedName(name string) bool {
	if protectedNames[name] {
		return true
	}
	for _, state := range []string{AllowedDirsFileName, WhitelistedCommandsFileName} {
		if strings.HasPrefix(name, state+".") {
			return true
		}
	}
	return false
}

// ReadTrustedFile lit path, un fichier d'état ou de configuration du
// harnais, en refusant tout fichier qui a pu être fabriqué ou modifié par
// quelqu'un d'autre que l'utilisateur réel (typiquement : le compte
// sandbox, qui a le droit d'écriture sur le workspace) :
//
//   - lien symbolique refusé (O_NOFOLLOW) ;
//   - fichier régulier uniquement ;
//   - propriétaire = utilisateur effectif du processus — un fichier supprimé
//     puis recréé par User lui appartient, et est donc rejeté.
//
// Un fichier qui appartient bien à l'utilisateur mais reste accessible au
// groupe ou aux autres (cas des workspaces accordés avant l'existence de
// cette protection : chgrp llm + g+rw) est réparé sur place (mode 0600,
// groupe principal de l'utilisateur) avec un avertissement : son contenu a
// pu être lu, voire modifié, par le compte sandbox jusque-là.
//
// Les vérifications portent sur le fichier effectivement ouvert (fstat), pas
// sur un chemin testé avant ouverture : pas de fenêtre entre contrôle et
// lecture. Un fichier absent renvoie une erreur satisfaisant os.IsNotExist.
func ReadTrustedFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("ouverture de %q (lien symbolique refusé): %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q n'est pas un fichier régulier, refusé", path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("%q n'appartient pas à l'utilisateur courant (uid %d) : possiblement recréé par le compte sandbox %q, refusé — vérifiez son contenu puis recréez-le vous-même", path, st.Uid, User)
	}
	if info.Mode().Perm()&0o077 != 0 {
		log.Printf("sandbox: %q était accessible au groupe/aux autres (%s) — remis en 0600 ; son contenu a pu être lu ou modifié par le compte %q, vérifiez-le", path, info.Mode().Perm(), User)
		if err := f.Chown(-1, os.Getegid()); err != nil {
			return nil, fmt.Errorf("réparation du groupe de %q: %w", path, err)
		}
		if err := f.Chmod(0o600); err != nil {
			return nil, fmt.Errorf("réparation des droits de %q: %w", path, err)
		}
	}
	return io.ReadAll(f)
}

// WriteTrustedFile écrit atomiquement data dans path, en 0600 : fichier
// temporaire à nom aléatoire créé en exclusif dans le même répertoire (donc
// jamais un fichier ou lien symbolique préparé à l'avance par le compte
// sandbox, qui a le droit d'écriture sur ce répertoire), puis rename.
func WriteTrustedFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op après un rename réussi
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

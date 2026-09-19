package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitConfigFileName est le nom, à l'intérieur du répertoire accordé à User
// (typiquement le workspace du bot), du fichier de config git dédié au
// sandbox — voir GitConfigPath.
const gitConfigFileName = ".sandbox-gitconfig"

// GitConfigPath retourne le chemin du fichier de config git dédié au
// sandbox à l'intérieur de dir. À passer à WrapCommand une fois dir accordé
// à User via GrantDirectory.
func GitConfigPath(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, gitConfigFileName)
}

// EnsureGitConfig crée, une seule fois (n'écrase jamais un fichier déjà
// présent), le fichier de config git dédié au sandbox dans dir :
//
//   - safe.directory=* : les dépôts accordés au LLM appartiennent à
//     l'utilisateur courant, pas à User — sans ça, git refuse de les traiter
//     ("dubious ownership") puisqu'il s'exécute sous une identité différente
//     de celle du propriétaire.
//   - user.name/user.email : recopiés depuis la config git --global de
//     l'utilisateur courant, s'ils y sont définis, pour que les commits
//     faits par le LLM aient une identité au lieu d'échouer ("Please tell
//     me who you are").
//
// Volontairement, ceci ne touche ni à /etc/gitconfig (qui affecterait tous
// les utilisateurs de la machine) ni ne crée de répertoire personnel pour
// User : dir doit déjà être un répertoire accordé à User (voir
// GrantDirectory), qui couvre alors aussi ce fichier.
func EnsureGitConfig(dir string) error {
	path := GitConfigPath(dir)
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("vérification de %q: %w", path, err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("création de %q: %w", dir, err)
	}

	if err := exec.Command("git", "config", "--file", path, "--add", "safe.directory", "*").Run(); err != nil {
		return fmt.Errorf("écriture de safe.directory dans %q: %w", path, err)
	}
	if name := globalGitConfig("user.name"); name != "" {
		if err := exec.Command("git", "config", "--file", path, "user.name", name).Run(); err != nil {
			return fmt.Errorf("écriture de user.name dans %q: %w", path, err)
		}
	}
	if email := globalGitConfig("user.email"); email != "" {
		if err := exec.Command("git", "config", "--file", path, "user.email", email).Run(); err != nil {
			return fmt.Errorf("écriture de user.email dans %q: %w", path, err)
		}
	}
	return nil
}

func globalGitConfig(key string) string {
	out, err := exec.Command("git", "config", "--global", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

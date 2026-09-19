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

// knownHostsFileName est le nom, à côté de gitConfigFileName, du fichier
// known_hosts dédié au sandbox : User n'ayant pas de répertoire personnel,
// il n'a pas de ~/.ssh/known_hosts, et sans ça toute commande git sur un
// dépôt distant en SSH (github.com, etc.) bloque sur l'invite interactive
// de vérification de clé d'hôte jusqu'au timeout de run_shell.
const knownHostsFileName = ".sandbox-known-hosts"

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
//   - core.sshCommand : pointé vers un known_hosts dédié (voir
//     knownHostsFileName) avec StrictHostKeyChecking=accept-new, pour que
//     git sur un dépôt distant en SSH n'attende pas indéfiniment une
//     confirmation interactive de clé d'hôte (impossible à donner : run_shell
//     n'a pas de terminal en face). "accept-new" fait confiance à la clé lors
//     du premier contact avec un hôte donné, mais refuse toujours une clé qui
//     changerait ensuite pour un hôte déjà connu. Si sshKeyPath est non vide,
//     y ajoute "-i sshKeyPath -o IdentitiesOnly=yes" pour que git s'authentifie
//     avec cette clé (celle de l'utilisateur réel, voir EnsureSSHKeyAccess,
//     à appeler séparément pour que User puisse effectivement la lire) plutôt
//     que d'échouer faute d'identité (User n'a pas de ~/.ssh à lui).
//
// Chaque réglage n'est écrit que s'il est absent, pour ne jamais écraser une
// valeur déjà présente (y compris posée à la main) : appelable sans risque à
// chaque démarrage, y compris sur un fichier créé par une version antérieure
// de cette fonction qui n'avait pas encore tel ou tel réglage. Corollaire :
// activer SandboxSSHKey après coup, sur un workspace où ce fichier existe déjà
// avec un core.sshCommand sans "-i", ne le met pas à jour automatiquement —
// supprimer la ligne (ou le fichier) à la main pour la faire régénérer.
//
// Volontairement, ceci ne touche ni à /etc/gitconfig (qui affecterait tous
// les utilisateurs de la machine) ni ne crée de répertoire personnel pour
// User : dir doit déjà être un répertoire accordé à User (voir
// GrantDirectory), qui couvre alors aussi ce fichier.
func EnsureGitConfig(dir string, sshKeyPath string) error {
	path := GitConfigPath(dir)
	if path == "" {
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("création de %q: %w", dir, err)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
			return fmt.Errorf("création de %q: %w", path, err)
		} else {
			f.Close()
		}
	} else if err != nil {
		return fmt.Errorf("vérification de %q: %w", path, err)
	}

	if gitConfigGet(path, "safe.directory") == "" {
		if err := exec.Command("git", "config", "--file", path, "--add", "safe.directory", "*").Run(); err != nil {
			return fmt.Errorf("écriture de safe.directory dans %q: %w", path, err)
		}
	}
	if gitConfigGet(path, "user.name") == "" {
		if name := globalGitConfig("user.name"); name != "" {
			if err := exec.Command("git", "config", "--file", path, "user.name", name).Run(); err != nil {
				return fmt.Errorf("écriture de user.name dans %q: %w", path, err)
			}
		}
	}
	if gitConfigGet(path, "user.email") == "" {
		if email := globalGitConfig("user.email"); email != "" {
			if err := exec.Command("git", "config", "--file", path, "user.email", email).Run(); err != nil {
				return fmt.Errorf("écriture de user.email dans %q: %w", path, err)
			}
		}
	}
	if gitConfigGet(path, "core.sshCommand") == "" {
		knownHosts := filepath.Join(dir, knownHostsFileName)
		if _, err := os.Stat(knownHosts); os.IsNotExist(err) {
			if f, err := os.OpenFile(knownHosts, os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
				return fmt.Errorf("création de %q: %w", knownHosts, err)
			} else {
				f.Close()
			}
		} else if err != nil {
			return fmt.Errorf("vérification de %q: %w", knownHosts, err)
		}
		sshCommand := fmt.Sprintf("ssh -o UserKnownHostsFile=%s -o StrictHostKeyChecking=accept-new", knownHosts)
		if sshKeyPath != "" {
			sshCommand += fmt.Sprintf(" -i %s -o IdentitiesOnly=yes", sshKeyPath)
		}
		if err := exec.Command("git", "config", "--file", path, "core.sshCommand", sshCommand).Run(); err != nil {
			return fmt.Errorf("écriture de core.sshCommand dans %q: %w", path, err)
		}
	}
	return nil
}

// gitConfigGet retourne la valeur de key dans le fichier de config file, ou
// "" si elle est absente (ou si sa lecture échoue).
func gitConfigGet(file, key string) string {
	out, err := exec.Command("git", "config", "--file", file, "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func globalGitConfig(key string) string {
	out, err := exec.Command("git", "config", "--global", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

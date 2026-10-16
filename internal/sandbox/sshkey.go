package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// sshKeyDirName est le répertoire, à l'intérieur du workspace, qui contient
// la clé SSH dédiée au bot (voir DedicatedSSHKeyPath). Volontairement pas
// ".ssh" : ce nom est protégé (voir protectedNames) et jamais accordé à User.
const sshKeyDirName = ".sandbox-ssh"

// sshKeyFileName est le nom de la clé privée dédiée (la publique est
// <nom>.pub, à enregistrer comme deploy key côté serveur distant).
const sshKeyFileName = "id_ed25519"

// DedicatedSSHKeyPath retourne le chemin de la clé privée SSH propre au bot
// à l'intérieur de workspace ("" si workspace est vide).
func DedicatedSSHKeyPath(workspace string) string {
	if workspace == "" {
		return ""
	}
	return filepath.Join(workspace, sshKeyDirName, sshKeyFileName)
}

// EnsureDedicatedSSHKey génère, une seule fois (jamais d'écrasement), une
// paire de clés ed25519 sans passphrase propre au bot dans workspace, et
// retourne le chemin de la clé privée. À enregistrer côté serveur distant
// comme « deploy key » (clé publique : <chemin>.pub) : le bot n'a ainsi
// jamais besoin des clés personnelles de l'utilisateur ni de son agent SSH.
func EnsureDedicatedSSHKey(workspace string) (string, error) {
	path := DedicatedSSHKeyPath(workspace)
	if path == "" {
		return "", nil
	}
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("vérification de %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("création de %q: %w", filepath.Dir(path), err)
	}
	out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "bot-deploy-key", "-f", path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh-keygen: %s: %w", string(out), err)
	}
	return path, nil
}

// ResolveSSHKey retourne la clé SSH à utiliser par le sandbox : configured
// (SANDBOX_SSH_KEY) si non vide, sinon la clé dédiée du bot dans workspace,
// générée au besoin. Jamais de repli implicite sur ~/.ssh.
func ResolveSSHKey(workspace, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	return EnsureDedicatedSSHKey(workspace)
}

// EnsureSSHKeyAccess donne à User un accès en lecture seule à keyPath :
// traversée de chaque répertoire ancêtre puis lecture du fichier lui-même —
// jamais d'écriture, jamais de changement de propriétaire. Pas d'effet si
// keyPath est vide. Pour la clé dédiée (dans le workspace), les ancêtres sont
// déjà traversables ; pour une clé explicite (SANDBOX_SSH_KEY, p. ex. dans
// ~/.ssh), c'est l'opérateur qui assume d'exposer cette identité à User.
// Voir EnsureGitConfig, qui pointe core.sshCommand vers cette clé.
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

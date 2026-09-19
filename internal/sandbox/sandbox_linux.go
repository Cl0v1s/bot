//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// ensureGroupActive s'assure que group est actif pour le processus courant
// (voir groupStale) ; sinon, relance le programme via "sg", qui active un
// groupe dont l'utilisateur courant est déjà membre selon /etc/group, sans
// nécessiter de déconnexion/reconnexion complète.
func ensureGroupActive(group string) error {
	stale, err := groupStale(group)
	if err != nil {
		return err
	}
	if !stale {
		return nil
	}

	sgPath, err := exec.LookPath("sg")
	if err != nil {
		return fmt.Errorf("%q pas encore actif pour cette session et \"sg\" introuvable (paquet shadow-utils) : ouvrez un nouveau terminal (ou reconnectez-vous), puis relancez le programme", group)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("relance impossible: %w", err)
	}

	args := append([]string{self}, os.Args[1:]...)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}

	// Remplace le processus courant : au retour (en cas de succès), c'est le
	// nouveau processus, avec group actif, qui reprend depuis le début du
	// programme — Ensure() y retombera immédiatement sur Ready()==true.
	return syscall.Exec(sgPath, []string{"sg", group, "-c", strings.Join(quoted, " ")}, os.Environ())
}

// createSystemUserAndGroup crée le groupe et l'utilisateur système "llm"
// (sans mot de passe, sans shell de connexion, sans répertoire personnel).
func createSystemUserAndGroup() error {
	if err := run("sudo", "groupadd", "--system", Group); err != nil {
		return err
	}
	return run("sudo", "useradd",
		"--system",
		"--no-create-home",
		"--shell", "/usr/sbin/nologin",
		"--gid", Group,
		User,
	)
}

func addUserToGroup(username, group string) error {
	return run("sudo", "usermod", "--append", "--groups", group, username)
}

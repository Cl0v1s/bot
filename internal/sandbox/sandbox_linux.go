//go:build linux

package sandbox

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

//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
)

// ensureGroupActive s'assure que group est actif pour le processus courant
// (voir groupStale). macOS n'a pas d'équivalent de "sg" pour activer un
// groupe fraîchement rejoint sans reconnexion complète ; on le signale
// clairement plutôt que de laisser échouer GrantDirectory juste après avec
// un message cryptique.
//
// groupStale seul ne suffit pas sur macOS : getgroups(2) n'y renvoie qu'au
// plus 16 groupes (le reste est résolu à la demande par le noyau via
// opendirectoryd), et un groupe tout juste rejoint via dseditgroup y est
// souvent déjà pris en compte par le noyau pour chown(2) sans y apparaître.
// Un "stale" est donc confirmé par un vrai chgrp d'essai (canChgrpTo) avant
// d'être signalé — c'est exactement l'opération dont GrantDirectory a besoin.
func ensureGroupActive(group string) error {
	stale, err := groupStale(group)
	if err != nil {
		return err
	}
	if !stale || canChgrpTo(group) {
		return nil
	}
	return fmt.Errorf("%q vient d'être rejoint mais n'est pas encore actif pour cette session : ouvrez un nouveau terminal (ou déconnectez-vous puis reconnectez-vous), puis relancez le programme", group)
}

// canChgrpTo teste, sur un fichier temporaire jetable, si le processus
// courant peut réellement en changer le groupe vers group.
func canChgrpTo(group string) bool {
	grp, err := user.LookupGroup(group)
	if err != nil {
		return false
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		return false
	}
	f, err := os.CreateTemp("", "bot-chgrp-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)
	return os.Chown(name, -1, gid) == nil
}

// createSystemUserAndGroup crée le groupe et l'utilisateur système "llm" via
// dscl (pas de useradd/groupadd sur macOS). L'utilisateur est désactivé
// (aucun mot de passe possible, AuthenticationAuthority ";DisabledUser;")
// et n'a pas de shell de connexion interactif.
func createSystemUserAndGroup() error {
	gid, err := freeID("/Groups", "PrimaryGroupID")
	if err != nil {
		return err
	}
	uid, err := freeID("/Users", "UniqueID")
	if err != nil {
		return err
	}

	steps := [][]string{
		{"dscl", ".", "-create", "/Groups/" + Group},
		{"dscl", ".", "-create", "/Groups/" + Group, "PrimaryGroupID", strconv.Itoa(gid)},
		{"dscl", ".", "-create", "/Users/" + User},
		{"dscl", ".", "-create", "/Users/" + User, "UniqueID", strconv.Itoa(uid)},
		{"dscl", ".", "-create", "/Users/" + User, "PrimaryGroupID", strconv.Itoa(gid)},
		{"dscl", ".", "-create", "/Users/" + User, "UserShell", "/usr/bin/false"},
		{"dscl", ".", "-create", "/Users/" + User, "NFSHomeDirectory", "/var/empty"},
		{"dscl", ".", "-create", "/Users/" + User, "RealName", "LLM sandbox user (bot)"},
		{"dscl", ".", "-append", "/Users/" + User, "AuthenticationAuthority", ";DisabledUser;"},
	}
	for _, args := range steps {
		if err := run("sudo", args...); err != nil {
			return err
		}
	}
	return nil
}

func addUserToGroup(username, group string) error {
	return run("sudo", "dseditgroup", "-o", "edit", "-a", username, "-t", "user", group)
}

// freeID trouve un identifiant système libre dans la plage 300-399 pour
// path ("/Users" ou "/Groups"), en listant les valeurs déjà attribuées.
func freeID(path, key string) (int, error) {
	out, err := exec.Command("dscl", ".", "-list", path, key).Output()
	if err != nil {
		return 0, fmt.Errorf("dscl -list %s %s: %w", path, key, err)
	}

	used := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if n, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
			used[n] = true
		}
	}

	for id := 300; id < 400; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("aucun identifiant système libre trouvé dans la plage 300-399")
}

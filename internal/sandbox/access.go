package sandbox

import "os/exec"

// CanAccess teste directement, sous l'identité réelle de User (llm), si path
// lui est accessible au niveau OS — en lecture (write=false) ou en écriture
// (write=true) — plutôt que de se fier à un état mémorisé ailleurs (fichier
// JSON, etc.) qui pourrait avoir divergé de la réalité (répertoire recréé,
// permissions changées manuellement...). path doit exister : l'appelant est
// responsable de tester un ancêtre existant si ce n'est pas le cas (voir
// tools.DirPermissions).
//
// Un seul bit est testé (r ou w, jamais les deux, jamais x séparément) : ça
// suffit comme test de présence du octroi, puisque GrantDirectory pose
// toujours lecture+écriture+traversée ensemble sur un répertoire accordé —
// ce n'est pas une simulation complète de l'algorithme d'accès POSIX.
func CanAccess(path string, write bool) bool {
	flag := "-r"
	if write {
		flag = "-w"
	}
	return exec.Command("sudo", "-n", "-u", User, "--", "test", flag, path).Run() == nil
}

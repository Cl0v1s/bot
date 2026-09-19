package sandbox

import (
	"context"
	"os/exec"
)

// WrapCommand construit la commande exécutant shellCmd (via sh -c) sous
// l'identité User, par un appel sudo non interactif. À n'utiliser qu'après
// que Ready() (ou Ensure()) a réussi : sudo -n échoue immédiatement sinon
// (remonté comme une erreur normale de commande, jamais un blocage).
//
// gitConfigPath, si non vide, est exposé à la commande via la variable
// d'environnement GIT_CONFIG_GLOBAL (voir EnsureGitConfig) : User n'ayant
// pas de répertoire personnel, c'est le seul moyen pour lui d'avoir une
// configuration git "globale" (safe.directory, identité) sans toucher à la
// configuration système (/etc/gitconfig, qui affecterait tous les
// utilisateurs de la machine) ni lui créer de répertoire personnel. Passer
// cette variable à travers sudo malgré env_reset fonctionne sans droit
// supplémentaire (vérifié empiriquement) : une affectation VAR=valeur avant
// la commande sur la ligne sudo n'est pas soumise à env_keep.
func WrapCommand(ctx context.Context, shellCmd string, gitConfigPath string) *exec.Cmd {
	args := []string{"-n", "-u", User}
	if gitConfigPath != "" {
		args = append(args, "GIT_CONFIG_GLOBAL="+gitConfigPath)
	}
	args = append(args, "--", "sh", "-c", shellCmd)
	return exec.CommandContext(ctx, "sudo", args...)
}

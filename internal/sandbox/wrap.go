package sandbox

import (
	"context"
	"os/exec"
)

// WrapCommand construit la commande exécutant shellCmd (via sh -c) sous
// l'identité User, par un appel sudo non interactif. À n'utiliser qu'après
// que Ready() (ou Ensure()) a réussi : sudo -n échoue immédiatement sinon
// (remonté comme une erreur normale de commande, jamais un blocage).
func WrapCommand(ctx context.Context, shellCmd string) *exec.Cmd {
	return exec.CommandContext(ctx, "sudo", "-n", "-u", User, "--", "sh", "-c", shellCmd)
}

package sandbox

import (
	"context"
	"os/exec"
	"strings"
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
//
// homeDir, si non vide, devient le HOME de la commande — sans ça, User (qui
// n'a pas de répertoire personnel créé sur disque, son entrée passwd pointe
// vers un chemin que lui-même ne peut pas créer, parent appartenant à root)
// fait échouer tout outil ayant besoin d'écrire une config/un cache sous
// $HOME (ex: `glab auth login`, pas seulement git — voir SandboxHomeDir).
// Contrairement à gitConfigPath, ceci NE PEUT PAS passer par une affectation
// VAR=valeur avant la commande sur la ligne sudo : l'option sudoers
// "always_set_home" (vérifiée active ici, et par défaut sur la plupart des
// systèmes) écrase HOME inconditionnellement après coup, quoi que la ligne
// de commande ait demandé — c'est spécifique à cette variable, pas un effet
// général d'env_reset (gitConfigPath, lui, passe sans problème). D'où
// l'export fait à l'intérieur même du script exécuté par sh -c : sudo a
// déjà fini d'imposer son propre HOME au moment où ce script démarre, donc
// le réécrire depuis l'intérieur n'est plus de son ressort.
func WrapCommand(ctx context.Context, shellCmd string, gitConfigPath string, homeDir string) *exec.Cmd {
	args := []string{"-n", "-u", User}
	if gitConfigPath != "" {
		args = append(args, "GIT_CONFIG_GLOBAL="+gitConfigPath)
	}
	// umask 002 (au lieu du 022 usuel) : tout fichier/répertoire créé par
	// cette commande (mkdir, redirection, touch...) reste accessible en
	// ÉCRITURE au groupe Group, pas seulement en lecture. Sans ça, un
	// répertoire créé ici (propriétaire User) redevient illisible en
	// écriture pour l'utilisateur réel dès qu'il y touche à son tour via
	// read_file/write_file — qui, eux, ne s'exécutent JAMAIS sous User,
	// toujours sous l'identité réelle, simple membre du groupe Group, pas
	// propriétaire (cause observée d'un "permission denied" au write_file
	// suivant un mkdir fait par run_shell). Symétrique à GrantDirectory, qui
	// pose les mêmes bits rétroactivement sur un répertoire accordé.
	script := "umask 002; " + shellCmd
	if homeDir != "" {
		script = "export HOME=" + ShellQuote(homeDir) + "; " + script
	}
	args = append(args, "--", "sh", "-c", script)
	return exec.CommandContext(ctx, "sudo", args...)
}

// ShellQuote entoure s de guillemets simples pour un usage sûr dans un
// script sh -c, en échappant les guillemets simples déjà présents (forme
// standard 'x'\''y' pour un s contenant x'y).
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

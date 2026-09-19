// Package sandbox provisionne (une fois) et utilise un compte système dédié
// et restreint ("llm") sous lequel les commandes run_shell demandées par le
// modèle sont exécutées — plutôt que sous l'identité de la personne qui a
// lancé le harnais.
//
// Rien ici ne change jamais le PROPRIÉTAIRE d'un fichier (chown) : les
// répertoires accordés au modèle via request_directory_access n'ont que
// leur GROUPE changé vers "llm" (chgrp), ce qui ne nécessite aucun
// privilège particulier dès lors que l'utilisateur courant est propriétaire
// de ces répertoires et membre du groupe "llm" (mis en place par Ensure).
package sandbox

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strings"
)

const (
	User  = "llm"
	Group = "llm"
)

// Ready indique si l'utilisateur courant peut déjà exécuter des commandes
// en tant que User sans mot de passe (sudo -n). Rapide et sûr à appeler à
// chaque lancement : ne déclenche jamais de prompt.
func Ready() bool {
	return exec.Command("sudo", "-n", "-u", User, "--", "true").Run() == nil
}

// Explain décrit ce qu'Ensure va faire, à afficher avant toute exécution.
func Explain(currentUser string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Le mode chat veut isoler les commandes shell (run_shell) demandées par le LLM sous un compte système dédié et restreint, %q — plutôt que de les exécuter avec vos propres droits.\n\n", User)
	b.WriteString("Cela nécessite, une seule fois :\n")
	fmt.Fprintf(&b, "  1. Créer le groupe système %q, s'il n'existe pas déjà.\n", Group)
	fmt.Fprintf(&b, "  2. Créer l'utilisateur système %q (membre du groupe %q, connexion interactive désactivée, sans mot de passe).\n", User, Group)
	fmt.Fprintf(&b, "  3. Vous ajouter (%q) au groupe %q, pour pouvoir ouvrir par groupe les répertoires que vous accorderez ensuite au LLM (chgrp), sans jamais en changer le propriétaire.\n", currentUser, Group)
	fmt.Fprintf(&b, "  4. Ajouter une règle sudo limitée à votre compte (%q), autorisant uniquement \"sudo -u %s\" sans mot de passe — cela ne vous donne aucun droit supplémentaire, ça vous permet seulement de *descendre* vers le compte restreint %q.\n", currentUser, User, User)
	b.WriteString("\nCes étapes demandent votre mot de passe (sudo vous le demandera directement). Aucun chown n'est jamais effectué : seul le groupe des répertoires explicitement accordés au LLM sera modifié, plus tard, au moment de chaque autorisation.\n")
	return b.String()
}

// Ensure vérifie que le sandbox est prêt et, sinon, explique la
// configuration nécessaire via out, demande confirmation via confirm, puis
// l'effectue. Ne fait rien (et ne demande rien) si Ready() est déjà vrai.
func Ensure(out io.Writer, confirm func(prompt string) bool) error {
	if Ready() {
		return nil
	}

	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("utilisateur courant introuvable: %w", err)
	}

	fmt.Fprintln(out, Explain(u.Username))
	if !confirm("Configurer maintenant ? [o/N] ") {
		return fmt.Errorf("configuration refusée par l'utilisateur")
	}

	if !userExists(User) {
		fmt.Fprintf(out, "[sandbox] création du groupe et de l'utilisateur système %q…\n", User)
		if err := createSystemUserAndGroup(); err != nil {
			return fmt.Errorf("création de l'utilisateur système %q: %w", User, err)
		}
	}

	fmt.Fprintf(out, "[sandbox] ajout de %q au groupe %q…\n", u.Username, Group)
	if err := addUserToGroup(u.Username, Group); err != nil {
		return fmt.Errorf("ajout de %q au groupe %q: %w", u.Username, Group, err)
	}

	fmt.Fprintf(out, "[sandbox] ajout de la règle sudo pour %q…\n", u.Username)
	if err := addSudoersRule(u.Username); err != nil {
		return fmt.Errorf("ajout de la règle sudoers: %w", err)
	}

	if !Ready() {
		return fmt.Errorf("configuration effectuée mais la vérification a échoué (sudo -n -u %s toujours refusé)", User)
	}
	fmt.Fprintln(out, "[sandbox] configuration terminée avec succès.")
	return nil
}

func userExists(name string) bool {
	return exec.Command("id", name).Run() == nil
}

// addSudoersRule installe une règle limitée à username, systématiquement
// validée avec `visudo -c` avant toute installation pour ne jamais risquer
// de corrompre la configuration sudo du système.
func addSudoersRule(username string) error {
	path := "/etc/sudoers.d/bot-" + sanitizeForFilename(username)
	rule := fmt.Sprintf("%s ALL=(%s) NOPASSWD: ALL\n", username, User)

	tmp, err := os.CreateTemp("", "bot-sudoers-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(rule); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	if out, err := exec.Command("visudo", "-c", "-f", tmpPath).CombinedOutput(); err != nil {
		return fmt.Errorf("règle sudoers invalide, non installée: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// root (via sudo) crée le fichier, il appartient donc naturellement à
	// root — ce n'est pas un chown fait par ce programme sur un fichier
	// utilisateur, seulement l'effet standard de toute installation via sudo.
	if out, err := exec.Command("sudo", "install", "-m", "0440", tmpPath, path).CombinedOutput(); err != nil {
		return fmt.Errorf("installation de %s: %s: %w", path, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func sanitizeForFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '_' || r == '-':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

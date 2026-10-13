package chat

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// directPrefix introduit une commande directe : la ligne n'est pas envoyée au
// modèle, la commande est exécutée tout de suite et sa sortie est ajoutée à
// la conversation (voir formatDirectContext) pour la requête suivante.
const directPrefix = "!"

// isDirectCommand indique si line (déjà nettoyée des espaces) est une
// commande directe. Un "!" seul n'en est pas une.
func isDirectCommand(line string) bool {
	return strings.HasPrefix(line, directPrefix) && strings.TrimSpace(line[len(directPrefix):]) != ""
}

// parseDirectCommand sépare le nom de la commande de ses arguments.
func parseDirectCommand(line string) (name, args string) {
	body := strings.TrimSpace(strings.TrimPrefix(line, directPrefix))
	name, args, _ = strings.Cut(body, " ")
	return name, strings.TrimSpace(args)
}

// directHelp est la sortie de "!help".
const directHelp = `Commandes directes (non envoyées au modèle, sortie ajoutée au contexte) :
  !help              cette aide
  !<commande shell>  exécute la commande comme dans votre terminal, sous votre
                     identité réelle (hors sandbox), directement : aucune
                     confirmation, aucun filtre de sécurité, ex. : !ls -la`

// directMaxOutput borne la sortie conservée (affichage et contexte).
const directMaxOutput = 64 * 1024

// runDirectCommand exécute la commande directe line et retourne sa sortie
// (stdout+stderr). Les commandes internes (help) sont traitées ici ; tout le
// reste est passé à "bash -c" via os/exec sous l'identité réelle du processus,
// sans run_shell : ni confirmation, ni blocage de commandes, ni vérification
// des répertoires — c'est l'utilisateur lui-même qui tape la commande.
func runDirectCommand(ctx context.Context, line string) (string, error) {
	name, args := parseDirectCommand(line)
	if name == "help" && args == "" {
		return directHelp, nil
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", strings.TrimSpace(strings.TrimPrefix(line, directPrefix)))
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	output := string(out)
	if len(output) > directMaxOutput {
		output = output[:directMaxOutput] + "\n[sortie tronquée]"
	}
	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return output, err
}

// formatDirectContext met en forme la commande et son résultat (ou son
// erreur) en un message utilisateur pour la conversation.
func formatDirectContext(line string, output string, err error) string {
	var b strings.Builder
	b.WriteString("[commande directe exécutée par l'utilisateur, sans passer par le modèle]\n")
	b.WriteString(line)
	b.WriteString("\n")
	if err != nil {
		fmt.Fprintf(&b, "Erreur : %v\n", err)
	}
	if out := strings.TrimRight(output, "\n"); out != "" {
		b.WriteString(out)
		b.WriteString("\n")
	} else if err == nil {
		b.WriteString("(aucune sortie)\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

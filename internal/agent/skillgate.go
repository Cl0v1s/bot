package agent

import (
	"context"
	"strings"
	"unicode"

	"bot/internal/llm"
	"bot/internal/skills"
)

type skillsCtxKey struct{}

// stopwords : mots génériques, présents dans quasiment toutes les
// descriptions de skills et demandes ("quand on te demande de...") : ils ne
// doivent pas suffire à déclarer une skill pertinente.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`skill skills utiliser utilise quand demande demandes pour dans avec sans cette cela fichier fichiers commande commandes faire fais fait veux voudrais peux peut explique comment depuis entre vers tout tous toute toutes plus moins aussi mais donc alors ainsi comme chaque autre autres exemple format ce qui que quoi dont ainsi where when with that this from your`) {
		stopwords[w] = true
	}
}

// stems découpe s en mots significatifs (>= 4 lettres, hors stopwords),
// tronqués à 5 caractères pour absorber pluriels et conjugaisons.
func stems(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) < 4 || stopwords[w] {
			continue
		}
		r := []rune(w)
		if len(r) > 5 {
			r = r[:5]
		}
		out[string(r)] = true
	}
	return out
}

// relevantSkills garde les skills non encore lues dont le nom ou la
// description recoupe la demande de l'utilisateur : au moins un mot du nom,
// ou deux mots de la description. Évite de harceler le modèle pour des
// commandes sans rapport avec une skill.
func relevantSkills(list []skills.Skill, request string, history []llm.Message) []skills.Skill {
	req := stems(request)
	var out []skills.Skill
	for _, sk := range list {
		if skillAlreadyRead(sk, history) {
			continue
		}
		hit := false
		for w := range stems(strings.ReplaceAll(sk.Name, "-", " ")) {
			if req[w] {
				hit = true
			}
		}
		if !hit {
			n := 0
			for w := range stems(sk.Description) {
				if req[w] {
					n++
				}
			}
			hit = n >= 2
		}
		if hit {
			out = append(out, sk)
		}
	}
	return out
}

// skillAlreadyRead : la skill a déjà été lue dans l'historique courant.
func skillAlreadyRead(sk skills.Skill, history []llm.Message) bool {
	for _, m := range history {
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "read_file" && strings.Contains(tc.Function.Arguments, sk.Path) {
				return true
			}
		}
	}
	return false
}

// lastUserMessage : contenu du dernier message "user" de l'historique.
func lastUserMessage(history []llm.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			return history[i].Content
		}
	}
	return ""
}

// WithSkills transmet à Run la liste des skills déclarées, pour activer le
// garde-fou skillGate. Passée par le contexte pour ne pas alourdir la
// signature de Run (appelée depuis le REPL et le mailbot). Sans skills, Run
// se comporte comme avant.
func WithSkills(ctx context.Context, list []skills.Skill) context.Context {
	if len(list) == 0 {
		return ctx
	}
	return context.WithValue(ctx, skillsCtxKey{}, list)
}

func skillsFromContext(ctx context.Context) []skills.Skill {
	l, _ := ctx.Value(skillsCtxKey{}).([]skills.Skill)
	return l
}

// skillReminder : texte ajouté à la demande de l'utilisateur quand elle
// ressemble à une skill non encore lue. Les modèles locaux ignorent
// régulièrement la consigne « lis la skill d'abord » noyée dans le prompt
// système ; placée dans la demande elle-même, juste avant qu'ils agissent,
// elle est bien plus suivie. Un refus d'appel a posteriori a été écarté :
// il gaspille une étape et se déclenchait sur des commandes sans rapport.
func skillReminder(list []skills.Skill) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Rappel système, ne vient pas de l'utilisateur : cette demande relève probablement de ")
	if len(list) == 1 {
		b.WriteString("la skill suivante. ")
	} else {
		b.WriteString("l'une des skills suivantes. ")
	}
	b.WriteString("Avant tout autre outil, lis-la avec read_file sur le chemin indiqué, puis applique exactement ce qu'elle dit.")
	for _, s := range list {
		b.WriteString("\n- " + s.Name)
		if s.Description != "" {
			b.WriteString(" : " + s.Description)
		}
		b.WriteString(" [" + s.Path + "]")
	}
	b.WriteString("]")
	return b.String()
}

// commandNotFound : le résultat d'un run_shell indique une commande
// inexistante (exit 127 / message de l'interpréteur).
func commandNotFound(result string) bool {
	l := strings.ToLower(result)
	return strings.Contains(l, "exit status 127") ||
		strings.Contains(l, "commande introuvable") ||
		strings.Contains(l, "command not found")
}

// commandNotFoundHint complète un « commande introuvable » : un modèle qui
// invente une commande (ex: « ddg » pour chercher sur le web) a presque
// toujours une skill ou un outil dédié qu'il n'a pas lu. Pas de test de
// pertinence ici : l'échec lui-même prouve que le modèle improvise.
func commandNotFoundHint(list []skills.Skill) string {
	var b strings.Builder
	b.WriteString("\n\nCette commande n'existe pas : ne l'invente pas et ne la réessaie pas. Si la tâche est couverte par un outil du bot (browser_fetch...) ou par l'une des skills ci-dessous, lis la skill concernée avec read_file et suis-la à la lettre.")
	if len(list) > 0 {
		b.WriteString("\n")
		b.WriteString(skills.Summary(list))
	}
	return b.String()
}

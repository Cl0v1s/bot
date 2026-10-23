package chat

import (
	"fmt"
	"io"
	"strings"

	"bot/internal/llm"
)

// isThinkCommand : "/think" seul ou suivi d'un argument.
func isThinkCommand(line string) bool {
	f := strings.Fields(line)
	return len(f) > 0 && f[0] == "/think"
}

// runThinkCommand traite /think :
//
//	/think                   état courant
//	/think on|off            active/désactive le raisonnement (Qwen3.x : enable_thinking)
//	/think low|medium|high   niveau de réflexion (gpt-oss et templates à niveaux ;
//	                         sans effet sur les Qwen3.x, qui n'ont que on/off)
//	/think auto              retour au défaut du serveur (ni enable_thinking ni niveau)
func runThinkCommand(out io.Writer, client *llm.Client, args []string) {
	if len(args) == 1 {
		switch strings.ToLower(args[0]) {
		case "on":
			t := true
			client.EnableThinking = &t
		case "off":
			f := false
			client.EnableThinking = &f
		case "auto":
			client.EnableThinking, client.ReasoningEffort = nil, ""
		default:
			effort, ok := llm.ParseReasoningEffort(args[0])
			if !ok {
				fmt.Fprintln(out, "usage : /think on|off|low|medium|high|auto")
				return
			}
			client.ReasoningEffort = effort
		}
	} else if len(args) > 1 {
		fmt.Fprintln(out, "usage : /think on|off|low|medium|high|auto")
		return
	}
	fmt.Fprintf(out, "[réflexion : %s]\n", thinkingState(client))
}

func thinkingState(c *llm.Client) string {
	parts := []string{"défaut serveur"}
	if c.EnableThinking != nil {
		parts[0] = map[bool]string{true: "on", false: "off"}[*c.EnableThinking]
	}
	if c.ReasoningEffort != "" {
		parts = append(parts, "niveau "+c.ReasoningEffort)
	}
	return strings.Join(parts, ", ")
}

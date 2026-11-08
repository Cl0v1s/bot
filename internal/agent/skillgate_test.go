package agent

import (
	"strings"
	"testing"

	"bot/internal/convo"
	"bot/internal/llm"
	"bot/internal/skills"
)

func call(name, args string) llm.ToolCall {
	var tc llm.ToolCall
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

func TestSkillReminder(t *testing.T) {
	if skillReminder(nil) != "" {
		t.Error("pas de skill : pas de rappel")
	}
	r := skillReminder([]skills.Skill{{Name: "gitlab-activity-reporter", Description: "rapport", Path: "/ws/skills/g/SKILL.md"}})
	if !strings.Contains(r, "/ws/skills/g/SKILL.md") || !strings.Contains(r, "read_file") {
		t.Errorf("rappel = %q", r)
	}
	c := convo.New("", 1000, 0.8, 2)
	if c.AppendToLastUser(r) {
		t.Error("conversation vide : rien à compléter")
	}
	c.AddUser("rapport d'activité du jour")
	if !c.AppendToLastUser(r) || !strings.Contains(c.Messages[0].Content, "rapport d'activité du jour\n\n[Rappel") {
		t.Errorf("message = %q", c.Messages[0].Content)
	}
}

func TestRelevantSkills(t *testing.T) {
	list := []skills.Skill{
		{Name: "gitlab-review-mr", Description: "Review a GitLab merge request", Path: "/ws/skills/gitlab-review-mr/SKILL.md"},
		{Name: "configure-mcp", Description: "Configurer un serveur MCP", Path: "/ws/skills/configure-mcp/SKILL.md"},
	}
	got := relevantSkills(list, "fais la review de https://gitlab.com/a/b/-/merge_requests/3", nil)
	if len(got) != 1 || got[0].Name != "gitlab-review-mr" {
		t.Errorf("got %v", got)
	}
	if got := relevantSkills(list, "liste les fichiers du dossier courant", nil); len(got) != 0 {
		t.Errorf("faux positif : %v", got)
	}
	read := []llm.Message{{ToolCalls: []llm.ToolCall{call("read_file", `{"path":"/ws/skills/configure-mcp/SKILL.md"}`)}}}
	if got := relevantSkills(list, "configure un serveur MCP", read); len(got) != 0 {
		t.Errorf("skill déjà lue : %v", got)
	}
}

func TestCommandNotFound(t *testing.T) {
	for _, r := range []string{"sh: ligne 1: ddg: commande introuvable\n[commande terminée avec erreur: exit status 127]", "bash: foo: command not found"} {
		if !commandNotFound(r) {
			t.Errorf("non détecté : %q", r)
		}
	}
	if commandNotFound("ok\n[commande terminée avec erreur: exit status 1]") {
		t.Error("faux positif sur exit 1")
	}
	h := commandNotFoundHint([]skills.Skill{{Name: "web-researcher", Path: "/ws/skills/web-researcher/SKILL.md"}})
	if !strings.Contains(h, "web-researcher") || !strings.Contains(h, "browser_fetch") {
		t.Errorf("hint = %q", h)
	}
}

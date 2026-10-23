package ontology

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"bot/internal/llm"
)

// maxJudged borne le nombre de paires soumises au modèle par appel ; le reste
// sera vu au nettoyage suivant.
const maxJudged = 20

const judgeSystemPrompt = `Tu arbitres des doublons dans un graphe de connaissances. Pour chaque paire numérotée (A, B), dis si A et B désignent EXACTEMENT le même concept (même personne, même outil, même projet...) ou deux concepts distincts, même proches (ex: "Synapse" et "Synapse Medicine" peuvent être distincts : un projet et son organisation ; "React" et "React Native" le sont). Dans le doute, réponds false. Si c'est le même, "keep" désigne le nom le plus canonique ("A" ou "B").

Réponds UNIQUEMENT par un objet JSON, sans texte autour : {"decisions":[{"n":1,"same":true,"keep":"A"}]}`

type judgeDecision struct {
	N    int    `json:"n"`
	Same bool   `json:"same"`
	Keep string `json:"keep"`
}

// JudgeCandidates soumet les paires candidates au modèle et retourne les
// fusions qu'il confirme. Ne modifie pas la base (voir Store.Merge).
func (d *Distiller) JudgeCandidates(ctx context.Context, cands []Candidate) ([]MergeOp, error) {
	if len(cands) == 0 {
		return nil, nil
	}
	if len(cands) > maxJudged {
		cands = cands[:maxJudged]
	}
	var b strings.Builder
	describe := func(c Concept) string {
		s := fmt.Sprintf("%q [%s]", c.Name, c.Type)
		if c.Description != "" {
			s += " — " + c.Description
		}
		return s
	}
	for i, c := range cands {
		fmt.Fprintf(&b, "%d. A: %s\n   B: %s\n", i+1, describe(c.A), describe(c.B))
	}
	client := *d.Client
	if client.MaxTokens == 0 || client.MaxTokens > maxExtractionTokens {
		client.MaxTokens = maxExtractionTokens
	}
	// En streaming : le serveur ne remarque l'annulation (nouveau message de
	// l'utilisateur) qu'à l'écriture du token suivant ; une requête non
	// streamée continue de générer et retarde la requête de l'utilisateur.
	msg, _, err := client.ChatCompletionStream(ctx, []llm.Message{
		{Role: "system", Content: judgeSystemPrompt},
		{Role: "user", Content: b.String() + "\nProduis l'objet JSON."},
	}, nil, llm.StreamCallbacks{})
	if err != nil {
		return nil, fmt.Errorf("arbitrage: %w", err)
	}
	s := msg.Content
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return nil, fmt.Errorf("arbitrage: pas d'objet JSON dans la réponse : %q", truncateRunes(strings.TrimSpace(s), 200))
	}
	var out struct {
		Decisions []judgeDecision `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("arbitrage: JSON invalide : %w", err)
	}
	return mergesFromDecisions(cands, out.Decisions), nil
}

// mergesFromDecisions traduit les décisions en fusions, en ignorant les
// numéros inconnus et les paires dont un concept est déjà fusionné par une
// décision précédente (chaînes A=B, B=C).
func mergesFromDecisions(cands []Candidate, decisions []judgeDecision) []MergeOp {
	var ops []MergeOp
	gone := map[int64]bool{}
	for _, dec := range decisions {
		if !dec.Same || dec.N < 1 || dec.N > len(cands) {
			continue
		}
		c := cands[dec.N-1]
		keep, drop := c.A, c.B
		if strings.EqualFold(strings.TrimSpace(dec.Keep), "B") {
			keep, drop = c.B, c.A
		}
		if gone[keep.ID] || gone[drop.ID] {
			continue
		}
		gone[drop.ID] = true
		ops = append(ops, MergeOp{KeepID: keep.ID, DropID: drop.ID, Keep: keep.Name, Drop: drop.Name})
	}
	return ops
}

// ApplyMerges exécute des fusions, en s'arrêtant à la première erreur ; retourne
// le nombre de fusions faites.
func (s *Store) ApplyMerges(ctx context.Context, ops []MergeOp) (int, error) {
	for i, m := range ops {
		if err := s.Merge(ctx, m.KeepID, m.DropID); err != nil {
			return i, err
		}
	}
	return len(ops), nil
}

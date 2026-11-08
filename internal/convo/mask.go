package convo

import (
	"fmt"
	"strings"
)

const (
	// DefaultMaskAt : fraction (0-1) du contexte à partir de laquelle les
	// anciens résultats d'outils sont masqués — volontairement bien en
	// dessous de CompactAt : masquer est gratuit (aucun appel LLM) et
	// retarde, voire évite, une compaction coûteuse et avec perte.
	DefaultMaskAt = 0.5
	// DefaultKeepToolResults : nombre de résultats d'outils les plus récents
	// toujours conservés intégralement.
	DefaultKeepToolResults = 4
	// maskMinChars : un résultat plus court que ça n'est pas masqué (le
	// placeholder ne ferait guère gagner de place).
	maskMinChars = 400

	maskedPrefix = "[résultat masqué"
)

// maskedPlaceholder remplace le contenu d'un ancien résultat d'outil. Garde
// le nom de l'outil et la taille d'origine, et dit au modèle quoi faire s'il
// a encore besoin de l'information.
func maskedPlaceholder(toolName string, size int) string {
	if toolName == "" {
		toolName = "outil"
	}
	return fmt.Sprintf("%s de %s, %d caractères, pour libérer du contexte : relance l'outil si tu as encore besoin de cette information]", maskedPrefix, toolName, size)
}

// NeedsMasking indique si le contexte estimé a atteint le seuil MaskAt.
func (c *Conversation) NeedsMasking() bool {
	if c.MaxContextTokens <= 0 || c.MaskAt <= 0 {
		return false
	}
	return c.EstimateTokens() >= int(float64(c.MaxContextTokens)*c.MaskAt)
}

// MaskOldToolResultsIfNeeded masque les anciens résultats d'outils si le
// seuil MaskAt est atteint (voir MaskOldToolResults). Retourne le nombre de
// résultats masqués.
func (c *Conversation) MaskOldToolResultsIfNeeded() int {
	if !c.NeedsMasking() {
		return 0
	}
	return c.MaskOldToolResults(c.KeepToolResults)
}

// MaskOldToolResults remplace le contenu des résultats d'outils (messages
// role="tool") par un court placeholder, en conservant intacts les keep plus
// récents. Seul le contenu change : le message, son tool_call_id et son
// ordre restent, donc le contrat « une réponse tool par tool_call » reste
// respecté. Les résultats courts ou déjà masqués sont ignorés. Retourne le
// nombre de messages masqués.
func (c *Conversation) MaskOldToolResults(keep int) int {
	if keep < 0 {
		keep = 0
	}
	var toolIdx []int
	for i, m := range c.Messages {
		if m.Role == "tool" {
			toolIdx = append(toolIdx, i)
		}
	}
	if len(toolIdx) <= keep {
		return 0
	}

	masked, savedTokens := 0, 0
	for _, i := range toolIdx[:len(toolIdx)-keep] {
		m := c.Messages[i]
		if len(m.Content) < maskMinChars || strings.HasPrefix(m.Content, maskedPrefix) {
			continue
		}
		placeholder := maskedPlaceholder(c.toolNameFor(i), len(m.Content))
		savedTokens += estimateTokens(m.Content) - estimateTokens(placeholder)
		c.Messages[i].Content = placeholder
		masked++
	}
	if masked > 0 && c.LastKnownTokens > 0 {
		// Le compte exact reste valable moyennant la réduction estimée (les
		// messages n'ont ni changé de nombre ni d'ordre).
		c.LastKnownTokens -= savedTokens
		if c.LastKnownTokens < 1 {
			c.LastKnownTokens = 1
		}
	}
	return masked
}

// toolNameFor retrouve le nom de l'outil du message "tool" d'indice i, via
// le tool_call de l'assistant qui l'a demandé.
func (c *Conversation) toolNameFor(i int) string {
	id := c.Messages[i].ToolCallID
	for j := i - 1; j >= 0; j-- {
		for _, tc := range c.Messages[j].ToolCalls {
			if tc.ID == id {
				return tc.Function.Name
			}
		}
		if c.Messages[j].Role != "tool" {
			break
		}
	}
	return ""
}

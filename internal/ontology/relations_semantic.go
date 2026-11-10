package ontology

// Dédoublonnage sémantique des types de relation par embeddings : « UTILISE »,
// « SE_SERT_DE » et « EMPLOIE » désignent la même chose, mais seule une table
// d'alias écrite à la main (relationAliases) le sait. Ici, chaque type de
// relation distinct est converti en vecteur, et ceux dont la similarité
// cosinus dépasse un seuil sont regroupés sous le plus utilisé du groupe
// (comme graph_find_similar_vocab / graph_merge_predicates dans
// claude-ontology, mais par sens plutôt que par orthographe).

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Embedder convertit des textes en vecteurs (llm.Client.Embed en production).
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// DefaultRelationSimilarity : seuil de similarité cosinus au-dessus duquel
// deux types de relation sont considérés comme synonymes. Volontairement
// haut : fusionner à tort deux relations distinctes dégrade le graphe, rater
// un synonyme ne coûte presque rien (il sera repris au nettoyage suivant).
const DefaultRelationSimilarity = 0.9

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Les embeddings rapprochent « utilise » de « est utilisé par » et « a » de
// « n'a pas » : les relations de sens inverse ou niées ne doivent jamais être
// fusionnées. Un marqueur présent d'un seul côté suffit à l'interdire.
var (
	passiveMarkers  = map[string]bool{"par": true, "by": true}
	negationMarkers = map[string]bool{"ne": true, "pas": true, "not": true, "non": true, "sans": true, "without": true, "no": true}
)

func relationWords(rel string) []string { return nameTokens(strings.ReplaceAll(rel, "_", " ")) }

func hasMarker(words []string, set map[string]bool) bool {
	for _, w := range words {
		if set[w] {
			return true
		}
	}
	return false
}

func compatibleRelations(a, b string) bool {
	wa, wb := relationWords(a), relationWords(b)
	return hasMarker(wa, passiveMarkers) == hasMarker(wb, passiveMarkers) &&
		hasMarker(wa, negationMarkers) == hasMarker(wb, negationMarkers)
}

// semanticRelationAliases regroupe les types de relation (déjà ramenés à leur
// forme canonique) par similarité d'embedding. Retourne alias → type retenu
// (le plus utilisé du groupe, à égalité l'ordre alphabétique), sans les types
// qui restent seuls.
func semanticRelationAliases(ctx context.Context, emb Embedder, counts map[string]int, threshold float64) (map[string]string, error) {
	if threshold <= 0 {
		threshold = DefaultRelationSimilarity
	}
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, t)
	}
	if len(types) < 2 {
		return nil, nil
	}
	sort.Slice(types, func(i, j int) bool {
		if counts[types[i]] != counts[types[j]] {
			return counts[types[i]] > counts[types[j]]
		}
		return types[i] < types[j]
	})
	texts := make([]string, len(types))
	for i, t := range types {
		texts[i] = strings.ToLower(strings.ReplaceAll(t, "_", " "))
	}
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(types) {
		return nil, fmt.Errorf("embeddings: %d vecteur(s) pour %d type(s) de relation", len(vecs), len(types))
	}

	var reps []int // indices (dans types) des représentants de groupe
	aliases := map[string]string{}
	for i, t := range types {
		best, bestSim := -1, threshold
		for _, r := range reps {
			if !compatibleRelations(t, types[r]) {
				continue
			}
			if sim := cosine(vecs[i], vecs[r]); sim >= bestSim {
				best, bestSim = r, sim
			}
		}
		if best >= 0 {
			aliases[t] = types[best]
		} else {
			reps = append(reps, i)
		}
	}
	return aliases, nil
}

// relationCounts : nombre de liens par type, une fois canonicalRelation appliqué.
func relationCounts(snap *snapshot) map[string]int {
	counts := map[string]int{}
	for _, e := range snap.edges {
		counts[canonicalRelation(e.typ)]++
	}
	return counts
}

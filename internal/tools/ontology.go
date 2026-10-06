package tools

import (
	"context"
	"encoding/json"

	"bot/internal/ontology"
)

// QueryOntologyToolName : nom du tool, exposé pour que d'autres paquets
// (prompts, mode mail) puissent savoir s'il est proposé.
const QueryOntologyToolName = "query_ontology"

// QueryOntologyTool interroge le graphe de connaissances (concepts et liens
// extraits automatiquement des conversations, voir internal/ontology).
// Lecture seule, sur une base locale du workspace : aucune permission ni
// confirmation requise.
type QueryOntologyTool struct {
	Store *ontology.Store
}

func (t *QueryOntologyTool) Name() string { return QueryOntologyToolName }

func (t *QueryOntologyTool) Description() string {
	return "Interroge ta base de connaissances (ontologie) : un graphe de concepts (personnes, projets, outils, méthodes, préférences...) et de liens entre eux (\"appartient à\", \"utilise\", \"est une sous-catégorie de\"...), construit automatiquement à partir des conversations passées avec l'utilisateur. À consulter pendant ta réflexion dès qu'une question touche au contexte de l'utilisateur (ses projets, son équipe, ses outils, ses habitudes) : plus rapide et plus fiable que de relire l'historique ou de deviner. \"query\" = nom (ou fragment de nom) d'un concept ; la réponse donne sa description et ses liens, et ceux de ses voisins jusqu'à \"depth\" sauts. Sans \"query\" : aperçu global (types de relations, concepts les plus connectés). La base peut être incomplète : l'absence d'un concept ne prouve pas qu'il n'existe pas."
}

func (t *QueryOntologyTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Nom ou fragment de nom d'un concept (insensible à la casse), ex: \"synapse\". Vide ou absent : aperçu global."},
			"depth": {"type": "integer", "minimum": 1, "maximum": 3, "description": "Nombre de sauts à parcourir depuis les concepts trouvés (défaut 1 : leurs liens directs)."},
			"relation": {"type": "string", "description": "Optionnel : ne montrer que les liens de ce type, ex: \"appartient à\" (casse et espaces ignorés)."}
		},
		"additionalProperties": false
	}`)
}

type queryOntologyArgs struct {
	Query    string `json:"query"`
	Depth    int    `json:"depth"`
	Relation string `json:"relation"`
}

func (t *QueryOntologyTool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args queryOntologyArgs
	if err := decodeArgs(argsJSON, &args); err != nil {
		return "", err
	}
	return t.Store.Describe(ctx, args.Query, args.Depth, args.Relation)
}

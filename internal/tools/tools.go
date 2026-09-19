// Package tools fournit des tools (function calling) exécutables localement
// et exposables au LLM : shell, lecture/écriture de fichiers, requête HTTP.
//
// Les arguments de chaque appel arrivent du LLM sous forme d'une chaîne JSON
// (le champ "arguments" d'un tool_call). Ils sont systématiquement décodés
// via encoding/json dans une struct typée, jamais par découpage de chaîne :
// cela gère nativement guillemets doubles, apostrophes, échappements et
// unicode sans code de parsing fragile.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"bot/internal/llm"
)

// Tool est un outil exécutable localement et exposable au LLM.
type Tool interface {
	Name() string
	Description() string
	// ParametersSchema retourne le JSON Schema (objet) décrivant les
	// arguments attendus, tel qu'exigé par l'API "function tools".
	ParametersSchema() json.RawMessage
	// Call exécute l'outil avec les arguments bruts (JSON sérialisé en
	// chaîne, exactement comme fourni par le LLM) et retourne un texte à
	// renvoyer au modèle.
	Call(ctx context.Context, argsJSON string) (string, error)
}

// Registry regroupe les tools disponibles pour un appel de conversation donné.
type Registry struct {
	tools map[string]Tool
	order []string
}

func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{tools: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		r.tools[t.Name()] = t
		r.order = append(r.order, t.Name())
	}
	return r
}

func (r *Registry) Empty() bool {
	return r == nil || len(r.tools) == 0
}

// Specs retourne les descriptions des tools au format attendu par l'API
// (champ "tools" de la requête chat completions).
func (r *Registry) Specs() []llm.Tool {
	if r.Empty() {
		return nil
	}
	specs := make([]llm.Tool, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		specs = append(specs, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.ParametersSchema(),
			},
		})
	}
	return specs
}

// Call exécute l'outil nommé name avec les arguments JSON bruts.
func (r *Registry) Call(ctx context.Context, name, argsJSON string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("outil %q inconnu (aucun outil enregistré)", name)
	}
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("outil %q inconnu", name)
	}
	return t.Call(ctx, argsJSON)
}

// decodeArgs décode strictement les arguments JSON d'un tool_call dans dst.
// L'utilisation systématique de encoding/json garantit que guillemets,
// apostrophes et caractères échappés dans les valeurs (chemins, contenus,
// commandes...) sont interprétés correctement, quels que soient les
// caractères qu'ils contiennent.
func decodeArgs(argsJSON string, dst any) error {
	if argsJSON == "" {
		argsJSON = "{}"
	}
	dec := json.NewDecoder(strings.NewReader(argsJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("arguments invalides: %w", err)
	}
	return nil
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ListDirTool liste les entrées d'un répertoire local (comme `ls`), sans
// jamais lire le contenu d'un fichier ni en modifier quoi que ce soit.
//
// Volontairement PAS soumis à DirPermissions (contrairement à read_file/
// write_file/run_shell) ni exécuté sous le compte sandbox restreint : lister
// des NOMS d'entrées n'expose aucun contenu et ne permet aucune écriture, un
// risque jugé assez faible pour ne nécessiter ni confirmation humaine ni
// isolation — à la différence de lire le contenu d'un fichier ou d'exécuter
// une commande. Sert notamment à explorer une arborescence avant de savoir
// quel répertoire précis demander via request_directory_access.
type ListDirTool struct {
	MaxEntries int // nombre max d'entrées retournées par appel (défaut 500)
}

func (t *ListDirTool) Name() string { return "list_dir" }

func (t *ListDirTool) Description() string {
	return "Liste les fichiers et sous-répertoires d'un répertoire local (comme `ls`), sans lire leur contenu ni les modifier. Contrairement aux autres outils fichiers, accessible pour n'importe quel répertoire lisible par le processus, sans permission ni confirmation préalable : utile pour explorer une arborescence avant de demander l'accès à un répertoire précis (request_directory_access) pour le lire ou y écrire."
}

func (t *ListDirTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin ABSOLU du répertoire à lister. Un chemin relatif est refusé."}
		},
		"required": ["path"],
		"additionalProperties": false
	}`)
}

type listDirArgs struct {
	Path string `json:"path"`
}

func (t *ListDirTool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args listDirArgs
	if err := decodeArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", fmt.Errorf(`paramètre "path" requis`)
	}
	if err := requireAbsolutePath("path", args.Path); err != nil {
		return "", err
	}

	dir := args.Path

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("lecture de %q: %w", dir, err)
	}
	if len(entries) == 0 {
		return "[répertoire vide]", nil
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	maxEntries := t.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 500
	}
	total := len(entries)
	truncated := total > maxEntries
	if truncated {
		entries = entries[:maxEntries]
	}

	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		b.WriteString(name)
		b.WriteString("\n")
	}
	if truncated {
		fmt.Fprintf(&b, "[... tronqué : %d/%d entrées affichées ...]\n", maxEntries, total)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

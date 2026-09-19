package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileTool écrit (crée ou remplace) un fichier texte local. L'accès
// n'est autorisé que si le répertoire du fichier a été préautorisé ou
// accordé via RequestDirectoryAccessTool — voir DirPermissions.
type WriteFileTool struct {
	Perms    *DirPermissions
	MaxBytes int
}

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Description() string {
	return "Écrit (crée ou remplace intégralement) un fichier texte local, dans un répertoire déjà autorisé (voir request_directory_access). Crée les répertoires parents si besoin."
}

func (t *WriteFileTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin du fichier à écrire, dans un répertoire déjà autorisé."},
			"content": {"type": "string", "description": "Contenu texte complet à écrire dans le fichier (remplace tout contenu existant)."}
		},
		"required": ["path", "content"],
		"additionalProperties": false
	}`)
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *WriteFileTool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args writeFileArgs
	// decodeArgs passe par encoding/json : les guillemets, apostrophes et
	// sauts de ligne présents dans "content" sont décodés correctement,
	// aucune reconstruction manuelle de chaîne n'est faite.
	if err := decodeArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", fmt.Errorf(`paramètre "path" requis`)
	}

	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 200000
	}
	if len(args.Content) > maxBytes {
		return "", fmt.Errorf("contenu trop volumineux (%d octets, max %d)", len(args.Content), maxBytes)
	}

	resolved, err := t.Perms.CheckFile(args.Path)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", fmt.Errorf("création du répertoire parent: %w", err)
	}
	if err := os.WriteFile(resolved, []byte(args.Content), 0o644); err != nil {
		return "", fmt.Errorf("écriture de %q: %w", args.Path, err)
	}

	return fmt.Sprintf("fichier %q écrit (%d octets)", args.Path, len(args.Content)), nil
}

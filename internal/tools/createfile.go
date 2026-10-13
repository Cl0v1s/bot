package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// CreateFileTool crée un nouveau fichier texte local. Refuse d'écraser un
// fichier existant (voir EditFileTool pour le modifier). L'accès n'est
// autorisé que si le répertoire du fichier a été préautorisé ou accordé via
// RequestDirectoryAccessTool — voir DirPermissions.
type CreateFileTool struct {
	Perms    *DirPermissions
	MaxBytes int

	// Sandboxed : voir writesync.go (resynchronisation des droits avec le
	// compte sandbox avant et après l'écriture).
	Sandboxed bool
}

func (t *CreateFileTool) Name() string { return "create_file" }

func (t *CreateFileTool) Description() string {
	return "Crée un nouveau fichier texte local avec le contenu donné, dans un répertoire déjà autorisé (voir request_directory_access). Crée les répertoires parents si besoin. Échoue si le fichier existe déjà : pour le modifier, utilise edit_file (remplacement ciblé, ou \"append\" pour ajouter à la fin)."
}

func (t *CreateFileTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin ABSOLU du fichier à créer, dans un répertoire déjà autorisé. Un chemin relatif est refusé."},
			"content": {"type": "string", "description": "Contenu texte complet du nouveau fichier."}
		},
		"required": ["path", "content"],
		"additionalProperties": false
	}`)
}

type createFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *CreateFileTool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args createFileArgs
	if err := decodeArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", fmt.Errorf(`paramètre "path" requis`)
	}
	if err := requireAbsolutePath("path", args.Path); err != nil {
		return "", err
	}

	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 200000
	}
	if len(args.Content) > maxBytes {
		return "", fmt.Errorf("contenu trop volumineux (%d octets, max %d)", len(args.Content), maxBytes)
	}

	resolved, err := t.Perms.CheckFileWrite(args.Path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(resolved); err == nil {
		return "", fmt.Errorf("%q existe déjà : create_file ne crée que de nouveaux fichiers (utilise edit_file pour le modifier)", args.Path)
	}

	if err := syncBeforeWrite(t.Sandboxed, resolved, "create_file"); err != nil {
		return "", err
	}
	if err := writeAndSync(t.Sandboxed, resolved, []byte(args.Content), "create_file"); err != nil {
		return "", err
	}
	return fmt.Sprintf("fichier %q créé (%d octets)", args.Path, len(args.Content)), nil
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// RequestDirectoryAccessTool est l'unique moyen pour le modèle d'obtenir
// l'accès à un répertoire pour read_file/write_file : par défaut ces deux
// tools n'ont accès à rien, et RequestAccess (verrou appliqué dans le code,
// pas seulement demandé dans le prompt) refuse tout chemin non couvert par
// un répertoire déjà autorisé.
type RequestDirectoryAccessTool struct {
	Perms *DirPermissions
}

func (t *RequestDirectoryAccessTool) Name() string { return "request_directory_access" }

func (t *RequestDirectoryAccessTool) Description() string {
	return "Demande à l'utilisateur la permission d'accéder (lecture/écriture) à un répertoire local. Obligatoire avant tout read_file/write_file sur un répertoire pas encore autorisé : ces outils échoueront sinon."
}

func (t *RequestDirectoryAccessTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"directory": {"type": "string", "description": "Chemin ABSOLU du répertoire pour lequel demander l'accès. Un chemin relatif est refusé."},
			"reason": {"type": "string", "description": "Explication brève, montrée à l'utilisateur, de la raison de cette demande."}
		},
		"required": ["directory"],
		"additionalProperties": false
	}`)
}

type requestDirArgs struct {
	Directory string `json:"directory"`
	Reason    string `json:"reason"`
}

func (t *RequestDirectoryAccessTool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args requestDirArgs
	if err := decodeArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if args.Directory == "" {
		return "", fmt.Errorf(`paramètre "directory" requis`)
	}
	if err := requireAbsolutePath("directory", args.Directory); err != nil {
		return "", err
	}

	granted, err := t.Perms.RequestAccess(ctx, args.Directory, args.Reason)
	if err != nil {
		return "", err
	}
	if granted {
		return fmt.Sprintf("accès accordé au répertoire %q pour le reste de la session.", args.Directory), nil
	}
	if !t.Perms.Interactive() {
		return fmt.Sprintf(
			"accès refusé au répertoire %q : aucun utilisateur disponible pour répondre en mode automatique. Seuls les répertoires préconfigurés par l'administrateur sont accessibles ici.",
			args.Directory,
		), nil
	}
	return fmt.Sprintf("accès refusé au répertoire %q par l'utilisateur.", args.Directory), nil
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"bot/internal/sandbox"
)

// WriteFileTool écrit (crée ou remplace) un fichier texte local. L'accès
// n'est autorisé que si le répertoire du fichier a été préautorisé ou
// accordé via RequestDirectoryAccessTool — voir DirPermissions.
type WriteFileTool struct {
	Perms    *DirPermissions
	MaxBytes int

	// Sandboxed : si vrai, resynchronise après coup (via
	// sandbox.GrantDirectory) l'accès du compte sandbox sur tout ce que cet
	// appel vient de créer — write_file s'exécute toujours sous l'identité
	// réelle de l'utilisateur (jamais sous le compte sandbox, contrairement
	// à run_shell), donc un nouveau fichier/répertoire créé ici garde par
	// défaut des droits classiques (umask du processus), pas forcément
	// accessibles en écriture au compte sandbox — symétrique au correctif
	// de umask dans sandbox.WrapCommand (qui traite le sens inverse : du
	// contenu créé par run_shell, pas toujours accessible en écriture à
	// l'utilisateur réel).
	Sandboxed bool
}

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Description() string {
	return "Écrit (crée ou remplace intégralement) un fichier texte local, dans un répertoire déjà autorisé (voir request_directory_access). Crée les répertoires parents si besoin."
}

func (t *WriteFileTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin ABSOLU du fichier à écrire, dans un répertoire déjà autorisé. Un chemin relatif est refusé."},
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

	// Capturé AVANT MkdirAll : le plus proche ancêtre déjà existant, pour
	// resynchroniser seulement à partir de là ensuite (voir plus bas), sans
	// remonter plus haut que nécessaire.
	existingAncestor := nearestExisting(filepath.Dir(resolved))

	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", fmt.Errorf("création du répertoire parent: %w", err)
	}
	if err := os.WriteFile(resolved, []byte(args.Content), 0o644); err != nil {
		return "", fmt.Errorf("écriture de %q: %w", args.Path, err)
	}

	if t.Sandboxed {
		// Best-effort : le fichier est déjà écrit avec succès à ce stade,
		// un échec ici ne doit pas faire échouer l'appel (juste laisser le
		// compte sandbox potentiellement sans accès à ce contenu tout
		// neuf, comme avant ce correctif).
		if err := sandbox.GrantDirectory(existingAncestor); err != nil {
			log.Printf("write_file: échec de la resynchronisation sandbox de %q: %v", existingAncestor, err)
		}
	}

	return fmt.Sprintf("fichier %q écrit (%d octets)", args.Path, len(args.Content)), nil
}

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

	// Sandboxed : si vrai, resynchronise (via sandbox.GrantDirectory) l'accès
	// entre l'utilisateur réel et le compte sandbox, dans les deux sens :
	//   - AVANT l'écriture : un fichier déjà présent au chemin visé peut avoir
	//     été créé ou réécrit depuis par run_shell (donc appartenir au compte
	//     sandbox, pas à l'utilisateur réel) avec un mode qui n'accorde pas
	//     l'écriture au groupe — l'écriture ci-dessous échouerait sinon avec
	//     un "permission denied" pourtant surprenant pour l'utilisateur réel
	//     sur SON PROPRE système de fichiers (observé en pratique).
	//   - APRÈS l'écriture : write_file s'exécute toujours sous l'identité
	//     réelle (jamais sous le compte sandbox, contrairement à run_shell),
	//     donc un nouveau fichier/répertoire créé ici garde par défaut des
	//     droits classiques (umask du processus), pas forcément accessibles
	//     en écriture au compte sandbox.
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
	// resynchroniser à partir de là (voir plus bas, dans les deux sens), sans
	// remonter plus haut que nécessaire.
	existingAncestor := nearestExisting(filepath.Dir(resolved))

	if t.Sandboxed {
		// Voir le commentaire de Sandboxed : un fichier déjà présent à ce
		// chemin peut appartenir au compte sandbox (créé/réécrit depuis par
		// run_shell) sans que l'écriture groupe soit accordée. Best-effort,
		// avant même de tenter l'écriture : un échec ici ne doit pas
		// empêcher d'essayer quand même (l'écriture elle-même échouera
		// alors normalement si l'accès manque réellement).
		if err := sandbox.GrantDirectory(existingAncestor); err != nil {
			log.Printf("write_file: échec de la resynchronisation sandbox de %q: %v", existingAncestor, err)
		}
	}

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

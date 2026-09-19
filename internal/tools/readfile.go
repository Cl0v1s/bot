package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// ReadFileTool lit un fichier texte local. L'accès n'est autorisé que si le
// répertoire du fichier a été préautorisé ou accordé via
// RequestDirectoryAccessTool — voir DirPermissions.
type ReadFileTool struct {
	Perms    *DirPermissions
	MaxBytes int
}

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Description() string {
	return "Lit le contenu d'un fichier texte local, dans un répertoire déjà autorisé (voir request_directory_access). " +
		"Une limite de taille interne s'applique à chaque appel : pour un gros fichier, utilise \"offset\" (octet de départ, défaut 0) et \"length\" (nombre d'octets, plafonné par cette limite) pour le lire par portions, en t'appuyant sur l'offset de suite indiqué en fin de résultat tant que le fichier n'est pas entièrement lu."
}

func (t *ReadFileTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin du fichier à lire, dans un répertoire déjà autorisé."},
			"offset": {"type": "integer", "minimum": 0, "description": "Octet à partir duquel lire (défaut 0). Reprendre à l'offset de suite indiqué par l'appel précédent pour continuer la lecture d'un gros fichier."},
			"length": {"type": "integer", "minimum": 1, "description": "Nombre d'octets à lire au maximum (défaut : la limite interne de l'outil, qui plafonne aussi toute valeur plus grande)."}
		},
		"required": ["path"],
		"additionalProperties": false
	}`)
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}

func (t *ReadFileTool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args readFileArgs
	if err := decodeArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", fmt.Errorf(`paramètre "path" requis`)
	}
	if args.Offset < 0 {
		return "", fmt.Errorf(`paramètre "offset" invalide : doit être positif ou nul`)
	}
	if args.Length < 0 {
		return "", fmt.Errorf(`paramètre "length" invalide : doit être positif`)
	}

	resolved, err := t.Perms.CheckFile(args.Path)
	if err != nil {
		return "", err
	}

	f, err := os.Open(resolved)
	if err != nil {
		return "", fmt.Errorf("lecture de %q: %w", args.Path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("lecture de %q: %w", args.Path, err)
	}
	size := info.Size()

	if args.Offset >= size {
		return fmt.Sprintf("[fichier de %d octet(s) ; offset %d au-delà de la fin, rien à lire]", size, args.Offset), nil
	}

	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 20000
	}
	length := args.Length
	if length <= 0 || length > maxBytes {
		length = maxBytes
	}

	if _, err := f.Seek(args.Offset, io.SeekStart); err != nil {
		return "", fmt.Errorf("déplacement dans %q: %w", args.Path, err)
	}

	buf := make([]byte, length)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", fmt.Errorf("lecture de %q: %w", args.Path, err)
	}
	buf = buf[:n]
	end := args.Offset + int64(n)

	// Portion couvrant tout le fichier depuis le tout début : rien à
	// signaler, comportement identique à un read_file sans pagination.
	if args.Offset == 0 && end >= size {
		return string(buf), nil
	}
	if end >= size {
		return fmt.Sprintf("%s\n[octets %d-%d/%d : fin du fichier atteinte]", buf, args.Offset, end, size), nil
	}
	return fmt.Sprintf("%s\n[octets %d-%d/%d ; suite disponible avec offset=%d]", buf, args.Offset, end, size, end), nil
}

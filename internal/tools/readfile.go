package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ReadFileTool lit un fichier texte local. L'accès n'est autorisé que si le
// répertoire du fichier a été préautorisé ou accordé via
// RequestDirectoryAccessTool — voir DirPermissions.
type ReadFileTool struct {
	Perms *DirPermissions
	// MaxLines : nombre maximal de lignes renvoyées par appel (défaut 500,
	// voir "offset"/"length" pour lire un fichier par portions).
	MaxLines int
	// MaxBytes : garde-fou supplémentaire sur la taille totale renvoyée
	// (défaut 200000), au cas où la fenêtre de MaxLines lignes contiendrait
	// quand même un volume de texte excessif (ex: quelques lignes très
	// longues, fichier minifié...). Coupe la fenêtre avant MaxLines si
	// cette limite est atteinte en premier.
	MaxBytes int
}

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Description() string {
	return "Lit le contenu d'un fichier texte local, dans un répertoire déjà autorisé (voir request_directory_access). Pour lister le contenu d'un répertoire (équivalent de `ls`), utilise l'outil list_dir, pas celui-ci : appelé sur un répertoire, il échoue explicitement plutôt que de rien lister. " +
		"Une limite de nombre de lignes s'applique à chaque appel : pour un gros fichier, utilise \"offset\" (numéro de la première ligne à lire, 1 = début du fichier) et \"length\" (nombre de lignes, plafonné par cette limite) pour le lire par portions, en reprenant avec l'offset de suite indiqué en fin de résultat tant que le fichier n'est pas entièrement lu."
}

func (t *ReadFileTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin ABSOLU du fichier à lire, dans un répertoire déjà autorisé. Un chemin relatif est refusé."},
			"offset": {"type": "integer", "minimum": 1, "description": "Numéro de la première ligne à lire (1 = début du fichier, défaut 1). Reprendre à l'offset de suite indiqué par l'appel précédent pour continuer la lecture d'un gros fichier."},
			"length": {"type": "integer", "minimum": 1, "description": "Nombre de lignes à lire au maximum (défaut : la limite interne de l'outil, qui plafonne aussi toute valeur plus grande)."}
		},
		"required": ["path"],
		"additionalProperties": false
	}`)
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
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
	if err := requireAbsolutePath("path", args.Path); err != nil {
		return "", err
	}
	if args.Offset < 0 {
		return "", fmt.Errorf(`paramètre "offset" invalide : doit être positif ou nul`)
	}
	if args.Length < 0 {
		return "", fmt.Errorf(`paramètre "length" invalide : doit être positif`)
	}

	// Vérifié avant toute chose, y compris avant le contrôle de permission :
	// un répertoire n'a besoin d'aucune autorisation pour ça, puisque rien de
	// son contenu n'est lu ni exposé ici — list_dir (jamais soumis à
	// permission, voir son commentaire) est le bon outil, quel que soit le
	// répertoire visé, y compris un répertoire jamais accordé.
	if info, err := os.Stat(args.Path); err == nil && info.IsDir() {
		return "", fmt.Errorf("%q est un répertoire, pas un fichier : utilise l'outil list_dir pour en lister le contenu", args.Path)
	}

	resolved, err := t.Perms.CheckFileRead(args.Path)
	if err != nil {
		return "", err
	}

	f, err := os.Open(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			// Message actionnable plutôt que l'erreur système brute : le cas
			// le plus fréquent en pratique est un nom de fichier mal
			// mémorisé/deviné (ex: renommé depuis) plutôt qu'un vrai chemin
			// inconnu — list_dir (jamais soumis à permission) permet de
			// vérifier le nom exact sans autre appel avant de réessayer.
			return "", fmt.Errorf("%q n'existe pas. Vérifie le nom exact avec list_dir sur %q avant de réessayer : le nom que tu utilises n'est peut-être plus le bon (fichier renommé, par exemple)", args.Path, filepath.Dir(args.Path))
		}
		return "", fmt.Errorf("lecture de %q: %w", args.Path, err)
	}
	defer f.Close()

	if binary, err := isProbablyBinary(f); err != nil {
		return "", fmt.Errorf("lecture de %q: %w", args.Path, err)
	} else if binary {
		// Un PDF/image/exécutable/archive lu comme "texte" produirait un
		// résultat illisible (octets bruts, flux compressés...) tout en
		// consommant une part significative — parfois la totalité — du
		// contexte pour rien : observé en pratique avec un PDF de plusieurs
		// Mo. Mieux vaut le refuser explicitement que de renvoyer ce bruit.
		return "", fmt.Errorf("%q semble être un fichier binaire (contenu non textuel : PDF, image, exécutable, archive...), pas un fichier texte : read_file ne peut pas en extraire un contenu lisible tel quel, et le lire gaspillerait le contexte en octets bruts. Pour un PDF, essaie de le convertir d'abord en texte via run_shell (ex: pdftotext), si l'outil est disponible", args.Path)
	}

	startLine := args.Offset
	if startLine <= 0 {
		startLine = 1
	}

	maxLines := t.MaxLines
	if maxLines <= 0 {
		maxLines = 500
	}
	length := args.Length
	if length <= 0 || length > maxLines {
		length = maxLines
	}

	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 200000
	}

	// Lignes jusqu'à 1 Mo chacune : au-delà, scanner.Err() renverrait
	// bufio.ErrTooLong plutôt que de simplement tronquer — acceptable, un
	// fichier "texte" avec des lignes plus longues que ça n'en est
	// vraisemblablement pas un.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		lineNum   int
		lines     []string
		byteCount int
		hasMore   bool
	)
	for scanner.Scan() {
		lineNum++
		text := scanner.Text()
		if lineNum < startLine {
			continue
		}
		if len(lines) >= length || byteCount+len(text)+1 > maxBytes {
			hasMore = true
			break
		}
		lines = append(lines, text)
		byteCount += len(text) + 1
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("lecture de %q: %w", args.Path, err)
	}

	if lineNum == 0 {
		return "[fichier vide]", nil
	}
	if len(lines) == 0 {
		return fmt.Sprintf("[fichier de %d ligne(s) ; offset %d au-delà de la fin, rien à lire]", lineNum, startLine), nil
	}

	content := strings.Join(lines, "\n")
	endLine := startLine + len(lines) - 1

	// Portion couvrant tout le fichier depuis le tout début : rien à
	// signaler, comportement identique à un read_file sans pagination.
	if startLine == 1 && !hasMore {
		return content, nil
	}
	if !hasMore {
		return fmt.Sprintf("%s\n[lignes %d-%d : fin du fichier atteinte]", content, startLine, endLine), nil
	}
	return fmt.Sprintf("%s\n[lignes %d-%d ; suite disponible avec offset=%d]", content, startLine, endLine, endLine+1), nil
}

// isProbablyBinary lit un préfixe de f pour détecter un octet NUL —
// heuristique standard (celle de git/grep -I) pour distinguer un fichier
// texte d'un format binaire (PDF, image, exécutable, archive...), qui
// contient presque toujours au moins un NUL dans ses tout premiers octets,
// contrairement à un texte légitime. Remet f au tout début avant de
// retourner, pour que l'appelant puisse le lire depuis le début ensuite.
func isProbablyBinary(f *os.File) (bool, error) {
	const sniffLen = 8000
	buf := make([]byte, sniffLen)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	return bytes.IndexByte(buf[:n], 0) != -1, nil
}

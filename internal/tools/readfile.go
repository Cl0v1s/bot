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
	"regexp"
	"strings"
)

// ReadFileTool lit un fichier texte local. L'accès n'est autorisé que si le
// répertoire du fichier a été préautorisé ou accordé via
// RequestDirectoryAccessTool — voir DirPermissions.
type ReadFileTool struct {
	Perms *DirPermissions
	// MaxLines : nombre maximal de lignes renvoyées par appel (défaut 200,
	// voir "offset"/"length" pour lire un fichier par portions, ou "search"
	// pour n'en extraire que les passages pertinents plutôt que de tout lire).
	MaxLines int
	// MaxBytes : garde-fou supplémentaire sur la taille totale renvoyée
	// (défaut 40000), au cas où la fenêtre de MaxLines lignes contiendrait
	// quand même un volume de texte excessif (ex: quelques lignes très
	// longues, fichier minifié...). Coupe la fenêtre avant MaxLines si
	// cette limite est atteinte en premier.
	MaxBytes int
	// MaxSearchLines : nombre maximal de lignes lues du fichier pour une
	// recherche ("search", voir Call) avant d'abandonner — garde-fou contre
	// un fichier pathologiquement gros, indépendant de MaxLines/MaxBytes qui
	// bornent la SORTIE, pas la quantité lue en entrée pour trouver les
	// correspondances. Défaut 200000.
	MaxSearchLines int
}

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Description() string {
	return "Lit le contenu d'un fichier texte local, dans un répertoire déjà autorisé (voir request_directory_access). Pour lister le contenu d'un répertoire (équivalent de `ls`), utilise l'outil list_dir, pas celui-ci : appelé sur un répertoire, il échoue explicitement plutôt que de rien lister. " +
		"Une limite de nombre de lignes s'applique à chaque appel : pour un gros fichier, utilise \"offset\" (numéro de la première ligne à lire, 1 = début du fichier) et \"length\" (nombre de lignes, plafonné par cette limite) pour le lire par portions, en reprenant avec l'offset de suite indiqué en fin de résultat tant que le fichier n'est pas entièrement lu. " +
		"Si tu cherches quelque chose de précis plutôt que de vouloir parcourir tout le fichier, préfère \"search\" (motif — syntaxe d'expression régulière RE2/Go, ex: \"func\\\\s+ResolveProject\") : ne renvoie que les lignes correspondantes avec quelques lignes de contexte autour, bien plus économe en contexte qu'une lecture complète. \"search\" et \"offset\"/\"length\" sont mutuellement exclusifs."
}

func (t *ReadFileTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin ABSOLU du fichier à lire, dans un répertoire déjà autorisé. Un chemin relatif est refusé."},
			"offset": {"type": "integer", "minimum": 1, "description": "Numéro de la première ligne à lire (1 = début du fichier, défaut 1). Reprendre à l'offset de suite indiqué par l'appel précédent pour continuer la lecture d'un gros fichier. Incompatible avec \"search\"."},
			"length": {"type": "integer", "minimum": 1, "description": "Nombre de lignes à lire au maximum (défaut : la limite interne de l'outil, qui plafonne aussi toute valeur plus grande). Incompatible avec \"search\"."},
			"search": {"type": "string", "description": "Motif de recherche (expression régulière, syntaxe RE2/Go — ex: \"TODO|FIXME\", insensible à la casse via le préfixe \"(?i)\"). Quand fourni, ne renvoie que les lignes correspondantes (avec du contexte autour, voir \"context\") au lieu du fichier entier — à préférer à une lecture complète dès que tu cherches quelque chose de précis plutôt que de vouloir tout parcourir. Incompatible avec \"offset\"/\"length\"."},
			"context": {"type": "integer", "minimum": 0, "description": "Avec \"search\" : nombre de lignes de contexte à inclure avant/après chaque ligne correspondante (défaut 2). Sans effet sans \"search\"."}
		},
		"required": ["path"],
		"additionalProperties": false
	}`)
}

type readFileArgs struct {
	Path    string `json:"path"`
	Offset  int    `json:"offset"`
	Length  int    `json:"length"`
	Search  string `json:"search"`
	Context int    `json:"context"`
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
	if args.Context < 0 {
		return "", fmt.Errorf(`paramètre "context" invalide : doit être positif ou nul`)
	}
	if args.Search != "" && (args.Offset > 0 || args.Length > 0) {
		return "", fmt.Errorf(`"search" et "offset"/"length" sont mutuellement exclusifs : utilise l'un ou l'autre, pas les deux`)
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

	maxLines := t.MaxLines
	if maxLines <= 0 {
		maxLines = 200
	}
	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 40000
	}

	if args.Search != "" {
		return t.searchFile(f, args)
	}
	return t.readRange(f, args, maxLines, maxBytes)
}

func (t *ReadFileTool) readRange(f *os.File, args readFileArgs, maxLines, maxBytes int) (string, error) {
	startLine := args.Offset
	if startLine <= 0 {
		startLine = 1
	}
	length := args.Length
	if length <= 0 || length > maxLines {
		length = maxLines
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

// searchBlock regroupe une ou plusieurs lignes correspondantes proches (leurs
// fenêtres de contexte se chevauchent ou se touchent) en un seul passage
// contigu du fichier, pour ne pas répéter deux fois une même ligne partagée
// entre deux correspondances voisines.
type searchBlock struct {
	start, end int          // indices 0-based dans allLines, inclusifs
	matches    map[int]bool // sous-ensemble de [start,end] : lignes réellement correspondantes (pas juste du contexte)
}

// searchFile implémente le paramètre "search" de Call : ne renvoie que les
// lignes correspondant au motif, avec quelques lignes de contexte autour,
// plutôt que le fichier entier — voir la description du paramètre.
func (t *ReadFileTool) searchFile(f *os.File, args readFileArgs) (string, error) {
	re, err := regexp.Compile(args.Search)
	if err != nil {
		return "", fmt.Errorf("motif de recherche invalide (syntaxe RE2/Go attendue) : %w", err)
	}

	maxSearchLines := t.MaxSearchLines
	if maxSearchLines <= 0 {
		maxSearchLines = 200000
	}
	contextLines := args.Context
	if args.Context == 0 {
		contextLines = 2
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var allLines []string
	for scanner.Scan() {
		if len(allLines) >= maxSearchLines {
			return "", fmt.Errorf("%q dépasse %d lignes : trop volumineux pour \"search\" (qui doit lire tout le fichier pour trouver les correspondances) — utilise run_shell (ex: grep) directement à la place", args.Path, maxSearchLines)
		}
		allLines = append(allLines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("lecture de %q: %w", args.Path, err)
	}

	if len(allLines) == 0 {
		return "[fichier vide]", nil
	}

	var blocks []searchBlock
	matchCount := 0
	for i, line := range allLines {
		if !re.MatchString(line) {
			continue
		}
		matchCount++
		start := max(0, i-contextLines)
		end := min(len(allLines)-1, i+contextLines)
		if n := len(blocks); n > 0 && start <= blocks[n-1].end+1 {
			if end > blocks[n-1].end {
				blocks[n-1].end = end
			}
			blocks[n-1].matches[i] = true
		} else {
			blocks = append(blocks, searchBlock{start: start, end: end, matches: map[int]bool{i: true}})
		}
	}

	if matchCount == 0 {
		return fmt.Sprintf("[0 correspondance pour %q dans %q (%d ligne(s) lues)]", args.Search, args.Path, len(allLines)), nil
	}

	maxLines := t.MaxLines
	if maxLines <= 0 {
		maxLines = 200
	}
	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 40000
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d correspondance(s) pour %q dans %q :\n", matchCount, args.Search, args.Path)
	emittedLines := 0
	blocksShown := 0
	for _, block := range blocks {
		blockLines := block.end - block.start + 1
		if emittedLines+blockLines > maxLines || b.Len() > maxBytes {
			break
		}
		fmt.Fprintf(&b, "\n--- lignes %d-%d ---\n", block.start+1, block.end+1)
		for i := block.start; i <= block.end; i++ {
			marker := " "
			if block.matches[i] {
				marker = ">"
			}
			fmt.Fprintf(&b, "%d%s %s\n", i+1, marker, allLines[i])
		}
		emittedLines += blockLines
		blocksShown++
	}

	if blocksShown < len(blocks) {
		fmt.Fprintf(&b, "\n[%d/%d passage(s) affiché(s) — motif trop fréquent pour tout montrer, précise-le pour réduire les correspondances]", blocksShown, len(blocks))
	}

	return strings.TrimRight(b.String(), "\n"), nil
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

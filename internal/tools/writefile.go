package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// WriteFileTool écrit (crée ou remplace) un fichier texte local, ou en
// modifie un passage repéré par son texte exact ("old_text"). L'accès n'est
// autorisé que si le répertoire du fichier a été préautorisé ou accordé via
// RequestDirectoryAccessTool — voir DirPermissions.
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
	return "Écrit (crée ou remplace intégralement) un fichier texte local, dans un répertoire déjà autorisé (voir request_directory_access). Crée les répertoires parents si besoin. " +
		"Pour MODIFIER un fichier EXISTANT, utilise \"old_text\" : recopie EXACTEMENT le passage actuel tel que read_file l'affiche (une ou plusieurs lignes entières, espaces et ponctuation compris), et \"content\" = sa nouvelle version ; seul ce passage est remplacé, sans compter aucune ligne. \"old_text\" doit apparaître une seule fois dans le fichier : s'il est ambigu, inclus une ligne voisine de plus. \"content\" vide = suppression du passage. Pour INSÉRER sans rien supprimer, prends comme \"old_text\" la ligne après laquelle insérer, et comme \"content\" cette même ligne suivie des nouvelles. " +
		"Le résultat d'une modification affiche la zone modifiée, numérotée : vérifie-la. Sans \"old_text\", \"content\" remplace tout le fichier (ou le crée) : à réserver à un nouveau fichier ou à une réécriture complète voulue."
}

func (t *WriteFileTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin ABSOLU du fichier à écrire, dans un répertoire déjà autorisé. Un chemin relatif est refusé."},
			"content": {"type": "string", "description": "Contenu texte. Avec \"old_text\" : la nouvelle version de ce passage (vide = le supprimer). Sans \"old_text\" : remplace tout le fichier (ou le crée)."},
			"old_text": {"type": "string", "description": "Passage EXISTANT à remplacer par \"content\", recopié exactement depuis read_file (lignes entières, sans numéros de ligne). Doit apparaître une seule fois dans le fichier. À utiliser pour toute modification d'un fichier existant."}
		},
		"required": ["path", "content"],
		"additionalProperties": false
	}`)
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	OldText string `json:"old_text"`
	// Offset/Length : ancien mode d'édition par numéros de ligne, supprimé
	// (les modèles comptaient mal les lignes d'une lecture non numérotée).
	// Toujours décodés pour répondre par une erreur explicite plutôt que
	// "unknown field", un modèle pouvant reprendre une habitude vue plus
	// tôt dans la conversation.
	Offset int `json:"offset"`
	Length int `json:"length"`
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
	if args.Offset != 0 || args.Length != 0 {
		return "", fmt.Errorf(`"offset"/"length" n'existent pas pour write_file : pour modifier un passage, passe-le tel quel dans "old_text" et sa nouvelle version dans "content"`)
	}

	resolved, err := t.Perms.CheckFileWrite(args.Path)
	if err != nil {
		return "", err
	}

	// Voir writesync.go : resynchronisation limitée au répertoire du
	// fichier, puis contrôle d'accès au fichier lui-même.
	if err := syncBeforeWrite(t.Sandboxed, resolved, "write_file"); err != nil {
		return "", err
	}

	finalContent, summary, err := t.computeFinalContent(resolved, args)
	if err != nil {
		return "", err
	}

	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 200000
	}
	if len(finalContent) > maxBytes {
		return "", fmt.Errorf("résultat trop volumineux (%d octets, max %d)", len(finalContent), maxBytes)
	}

	if err := writeAndSync(t.Sandboxed, resolved, []byte(finalContent), "write_file"); err != nil {
		return "", err
	}

	if summary != "" {
		return fmt.Sprintf("fichier %q modifié (%d octets au total) : %s", args.Path, len(finalContent), summary), nil
	}
	return fmt.Sprintf("fichier %q écrit (%d octets)", args.Path, len(finalContent)), nil
}

// computeFinalContent retourne le contenu complet à écrire dans resolved :
// args.Content tel quel sans "old_text" (remplacement intégral ou
// création), sinon le contenu actuel avec le passage remplacé (voir
// replaceOldText). summary (modification uniquement) décrit ce qui a été
// fait, avec un extrait numéroté de la zone modifiée pour que le modèle
// puisse vérifier le résultat.
func (t *WriteFileTool) computeFinalContent(resolved string, args writeFileArgs) (final, summary string, err error) {
	if args.OldText != "" {
		return replaceOldText(resolved, args)
	}
	return args.Content, "", nil
}

// replaceOldText implémente "old_text" : remplacement d'un passage repéré
// par son texte exact plutôt que par des numéros de ligne, que les modèles
// comptent mal sur une lecture non numérotée (observé : la ligne 23 visée à
// la place de la 20, écrasant une ligne sans rapport). Le passage doit être
// unique, pour qu'aucune autre occurrence ne soit modifiée par erreur.
func replaceOldText(resolved string, args writeFileArgs) (final, summary string, err error) {
	data, err := os.ReadFile(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("%q n'existe pas : \"old_text\" ne peut modifier qu'un fichier existant — omets-le pour créer ce fichier avec \"content\" comme contenu complet", args.Path)
		}
		return "", "", fmt.Errorf("lecture de %q avant modification: %w", args.Path, err)
	}
	if isLikelyBinaryContent(data) {
		return "", "", fmt.Errorf("%q semble être un fichier binaire : \"old_text\" ne s'applique qu'à du texte", args.Path)
	}
	existing := string(data)
	oldText, content := args.OldText, args.Content
	// Fichier aux fins de ligne Windows : le modèle recopie des "\n".
	if !strings.Contains(existing, oldText) && strings.Contains(existing, "\r\n") {
		oldText = strings.ReplaceAll(oldText, "\n", "\r\n")
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	if oldText == content {
		return "", "", fmt.Errorf(`"content" est identique à "old_text" : rien à modifier`)
	}

	switch n := strings.Count(existing, oldText); {
	case n == 0:
		return "", "", fmt.Errorf("\"old_text\" introuvable dans %q : fichier non modifié. Il doit être recopié EXACTEMENT (espaces, ponctuation, retours à la ligne) depuis une lecture récente du fichier, sans numéros de ligne%s", args.Path, oldTextHint(existing, args.OldText))
	case n > 1:
		return "", "", fmt.Errorf("\"old_text\" apparaît %d fois dans %q (lignes %s) : fichier non modifié. Ajoute une ligne voisine à \"old_text\" (et à \"content\") pour désigner une seule occurrence", n, args.Path, occurrenceLines(existing, oldText))
	}

	idx := strings.Index(existing, oldText)
	final = existing[:idx] + content + existing[idx+len(oldText):]

	lines, _ := splitFileLines([]byte(final))
	startLine := strings.Count(final[:idx], "\n")
	newLines := 0
	if content != "" {
		newLines = strings.Count(strings.TrimSuffix(content, "\n"), "\n") + 1
	}
	oldLines := strings.Count(strings.TrimSuffix(oldText, "\n"), "\n") + 1
	action := fmt.Sprintf("passage de %d ligne(s) remplacé par %d ligne(s), à partir de la ligne %d", oldLines, newLines, startLine+1)
	return final, action + "\n" + editExcerpt(lines, startLine, newLines), nil
}

// oldTextHint aide à corriger un "old_text" introuvable : si sa première
// ligne significative existe dans le fichier (le passage a été mal recopié
// plus loin), indique où, avec son contenu exact.
func oldTextHint(existing, oldText string) string {
	var first string
	for _, l := range strings.Split(oldText, "\n") {
		if isSignificantLine(l) {
			first = strings.TrimSpace(l)
			break
		}
	}
	if first == "" {
		return ""
	}
	for i, l := range strings.Split(existing, "\n") {
		if strings.TrimSpace(l) == first {
			return fmt.Sprintf(". Sa première ligne existe bien (ligne %d) : c'est la suite du passage qui diffère ; relis le fichier (read_file avec \"offset\" %d) et recopie-le tel quel", i+1, i+1)
		}
	}
	return ". Relis le fichier (read_file) pour recopier le passage tel qu'il est actuellement"
}

// occurrenceLines liste les numéros de ligne (1-based) où commence chaque
// occurrence de sub dans s.
func occurrenceLines(s, sub string) string {
	var nums []string
	for off := 0; ; {
		i := strings.Index(s[off:], sub)
		if i < 0 {
			break
		}
		nums = append(nums, fmt.Sprint(strings.Count(s[:off+i], "\n")+1))
		off += i + len(sub)
	}
	return strings.Join(nums, ", ")
}

// isSignificantLine : au moins 3 lettres ou chiffres — exclut lignes vides,
// accolades, séparateurs ("---", "*/"...). Voir oldTextHint.
func isSignificantLine(line string) bool {
	n := 0
	for _, r := range line {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n >= 3
}

// editExcerpt affiche, numérotée (même format que read_file "search" : ">"
// pour les lignes nouvelles), la zone modifiée de result avec 2 lignes de
// contexte de part et d'autre. Un long bloc nouveau n'est montré que par ses
// premières et dernières lignes.
func editExcerpt(result []string, start, n int) string {
	const ctxLines, headTail = 2, 5
	var b strings.Builder
	b.WriteString("[zone modifiée, état actuel du fichier — vérifie qu'il n'y a ni doublon ni ligne mal placée]\n")
	line := func(i int) {
		marker := " "
		if i >= start && i < start+n {
			marker = ">"
		}
		fmt.Fprintf(&b, "%d%s %s\n", i+1, marker, result[i])
	}
	from := max(0, start-ctxLines)
	to := min(len(result), start+n+ctxLines) // exclusif
	for i := from; i < to; i++ {
		if n > 2*headTail && i == start+headTail {
			fmt.Fprintf(&b, "[... %d ligne(s) nouvelles non affichées ...]\n", n-2*headTail)
			i = start + n - headTail - 1
			continue
		}
		line(i)
	}
	if n == 0 {
		b.WriteString("[aucune ligne nouvelle : suppression]\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// splitFileLines découpe data en lignes, en détectant séparément si le
// fichier se terminait par un saut de ligne — distinction perdue par un
// simple bufio.Scanner (qui traite "a\nb\n" et "a\nb" de façon identique) et
// nécessaire pour numéroter correctement la zone modifiée (voir
// editExcerpt). Un fichier vide (0 octet) a 0 ligne, pas une ligne vide.
func splitFileLines(data []byte) (lines []string, trailingNewline bool) {
	if len(data) == 0 {
		return nil, false
	}
	s := string(data)
	trailingNewline = strings.HasSuffix(s, "\n")
	if trailingNewline {
		s = s[:len(s)-1]
	}
	return strings.Split(s, "\n"), trailingNewline
}

// isLikelyBinaryContent applique la même heuristique que
// isProbablyBinary (readfile.go, octet NUL dans les premiers octets) mais
// sur un contenu déjà entièrement lu en mémoire plutôt qu'un *os.File.
func isLikelyBinaryContent(data []byte) bool {
	const sniffLen = 8000
	n := len(data)
	if n > sniffLen {
		n = sniffLen
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

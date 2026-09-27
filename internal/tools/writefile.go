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
// modifie une portion ciblée (voir "offset"/"length"). L'accès n'est
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
		"Pour modifier une portion ciblée d'un fichier EXISTANT sans regénérer tout son contenu, utilise \"offset\" (et éventuellement \"length\") — repère d'abord la plage de lignes concernée avec read_file en mode \"search\" (qui numérote chaque ligne affichée), plutôt que de compter toi-même les lignes d'une lecture complète (non numérotée) : \"length\" > 0 remplace ces lignes par \"content\", \"length\" omis/0 insère \"content\" avant la ligne \"offset\" sans rien supprimer. Pour MODIFIER des lignes existantes, utilise toujours \"length\" (remplacement) : une insertion laisse l'ancienne version en place, en doublon. \"content\" ne doit contenir que les lignes nouvelles ou de remplacement, jamais les lignes voisines qui restent en place. Le résultat affiche la zone modifiée, numérotée : vérifie-la. Sans \"offset\", \"content\" remplace tout le fichier (ou le crée)."
}

func (t *WriteFileTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin ABSOLU du fichier à écrire, dans un répertoire déjà autorisé. Un chemin relatif est refusé."},
			"content": {"type": "string", "description": "Contenu texte. Sans \"offset\" : remplace tout le fichier (ou le crée). Avec \"offset\" : les lignes à insérer/substituer, découpées sur les sauts de ligne (un saut de ligne final éventuel est ignoré ; vide avec \"length\" > 0 = suppression de ces lignes). Uniquement les lignes nouvelles ou de remplacement, jamais les lignes voisines qui restent en place."},
			"offset": {"type": "integer", "minimum": 1, "description": "Numéro de la ligne (1 = première ligne) à partir de laquelle appliquer \"content\", sur un fichier qui doit déjà exister. Sans \"length\" (ou length=0) : insère \"content\" avant cette ligne, sans rien supprimer (offset = nombre de lignes + 1 pour ajouter à la fin). Avec \"length\" : voir ce paramètre. Omis = remplace tout le fichier avec \"content\"."},
			"length": {"type": "integer", "minimum": 0, "description": "Avec \"offset\" : nombre de lignes existantes à remplacer par \"content\", à partir de la ligne \"offset\" incluse. 0 ou omis = insertion pure (rien supprimé). Sans effet sans \"offset\"."}
		},
		"required": ["path", "content"],
		"additionalProperties": false
	}`)
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Offset  int    `json:"offset"`
	Length  int    `json:"length"`
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
	if args.Offset < 0 {
		return "", fmt.Errorf(`paramètre "offset" invalide : doit être positif ou nul`)
	}
	if args.Length < 0 {
		return "", fmt.Errorf(`paramètre "length" invalide : doit être positif ou nul`)
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

	if args.Offset > 0 {
		return fmt.Sprintf("fichier %q modifié (%d octets au total) : %s", args.Path, len(finalContent), summary), nil
	}
	return fmt.Sprintf("fichier %q écrit (%d octets)", args.Path, len(finalContent)), nil
}

// computeFinalContent retourne le contenu complet à écrire dans resolved :
// args.Content tel quel si args.Offset est omis (remplacement intégral —
// comportement historique, fonctionne aussi pour créer un nouveau fichier),
// sinon le contenu ACTUEL de resolved avec args.Content inséré/substitué à
// partir de la ligne args.Offset (voir Description). summary (mode offset
// uniquement) décrit ce qui a été fait, avec un extrait numéroté de la zone
// modifiée : sans ce retour, le modèle n'a aucun moyen de voir qu'il a
// inséré au mauvais endroit, ou inséré une nouvelle version d'un passage au
// lieu de le remplacer.
func (t *WriteFileTool) computeFinalContent(resolved string, args writeFileArgs) (final, summary string, err error) {
	if args.Offset == 0 {
		return args.Content, "", nil
	}

	existing, err := os.ReadFile(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("%q n'existe pas encore : \"offset\" ne peut modifier qu'un fichier existant — omets \"offset\" pour créer ce fichier avec \"content\" comme contenu complet", args.Path)
		}
		return "", "", fmt.Errorf("lecture de %q avant modification: %w", args.Path, err)
	}
	if len(existing) > 0 && isLikelyBinaryContent(existing) {
		return "", "", fmt.Errorf("%q semble être un fichier binaire : \"offset\" (édition ligne à ligne) ne s'applique qu'à du texte", args.Path)
	}

	lines, trailingNewline := splitFileLines(existing)
	newLines := splitContentLines(args.Content)

	insertIdx := args.Offset - 1      // 0-based
	endIdx := insertIdx + args.Length // exclusif, 0-based ; = insertIdx en insertion pure
	if args.Length > 0 {
		if insertIdx > len(lines) || endIdx > len(lines) {
			return "", "", fmt.Errorf(
				"la plage demandée (lignes %d à %d) dépasse la fin du fichier (%d ligne(s)) : relis le fichier (read_file) pour un offset/length à jour avant de réessayer",
				args.Offset, args.Offset+args.Length-1, len(lines),
			)
		}
	} else {
		if insertIdx > len(lines) {
			return "", "", fmt.Errorf(
				"offset %d dépasse la fin du fichier (%d ligne(s) ; offset max pour insérer en fin de fichier : %d) : relis le fichier (read_file) pour un offset à jour avant de réessayer",
				args.Offset, len(lines), len(lines)+1,
			)
		}
		if len(newLines) == 0 {
			return "", "", fmt.Errorf(`"content" vide : rien à insérer (pour supprimer des lignes, passe "length" avec un "content" vide)`)
		}
	}

	if err := checkBoundaryDuplicate(lines, newLines, insertIdx, endIdx); err != nil {
		return "", "", err
	}

	result := make([]string, 0, len(lines)-args.Length+len(newLines))
	result = append(result, lines[:insertIdx]...)
	result = append(result, newLines...)
	result = append(result, lines[endIdx:]...)

	final = strings.Join(result, "\n")
	if trailingNewline && len(result) > 0 {
		final += "\n"
	}

	var action string
	if args.Length > 0 {
		action = fmt.Sprintf("lignes %d-%d (%d) remplacées par %d ligne(s)", args.Offset, endIdx, args.Length, len(newLines))
	} else {
		action = fmt.Sprintf("%d ligne(s) insérée(s) avant l'ancienne ligne %d, aucune supprimée", len(newLines), args.Offset)
	}
	return final, action + "\n" + editExcerpt(result, insertIdx, len(newLines)), nil
}

// splitContentLines découpe le "content" d'une édition par offset en lignes.
// Un saut de ligne final est ignoré : un modèle termine presque toujours son
// contenu par "\n", ce qui ajoutait sinon une ligne vide parasite à chaque
// édition. "" = aucune ligne (suppression pure avec "length") ; "\n" = une
// ligne vide.
func splitContentLines(content string) []string {
	if content == "" {
		return nil
	}
	content = strings.TrimSuffix(content, "\n")
	content = strings.TrimSuffix(content, "\r")
	return strings.Split(content, "\n")
}

// checkBoundaryDuplicate refuse une édition dont le contenu commence par les
// lignes qui restent juste avant la zone modifiée, ou se termine par celles
// qui restent juste après : ces lignes se retrouveraient en double. Deux
// erreurs typiques d'un modèle : inclure dans "content" la ligne d'ancrage
// (ex: le titre sous lequel il insère), ou relancer une insertion déjà faite
// par un appel précédent. Seul un recouvrement comportant au moins une ligne
// significative (voir isSignificantLine) compte : une accolade ou une ligne
// vide identique de part et d'autre est banale.
func checkBoundaryDuplicate(lines, newLines []string, insertIdx, endIdx int) error {
	before := lines[:insertIdx]
	after := lines[endIdx:]
	for m := len(newLines); m >= 1; m-- {
		if m <= len(before) && equalLines(newLines[:m], before[len(before)-m:]) && anySignificant(newLines[:m]) {
			return fmt.Errorf("les %d première(s) ligne(s) de \"content\" sont identiques aux lignes %d-%d, juste avant la zone modifiée, qui restent en place : elles seraient en double — fichier non modifié. Retire-les de \"content\" (ou étends la plage avec offset/length pour les remplacer). Si ce contenu a déjà été inséré par un appel précédent, n'insère rien de plus", m, insertIdx-m+1, insertIdx)
		}
		if m <= len(after) && equalLines(newLines[len(newLines)-m:], after[:m]) && anySignificant(newLines[len(newLines)-m:]) {
			return fmt.Errorf("les %d dernière(s) ligne(s) de \"content\" sont identiques aux lignes %d-%d, juste après la zone modifiée, qui restent en place : elles seraient en double — fichier non modifié. Retire-les de \"content\" (ou étends \"length\" pour les remplacer). Si ce contenu a déjà été inséré par un appel précédent, n'insère rien de plus", m, endIdx+1, endIdx+m)
		}
	}
	return nil
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimRight(a[i], " \t\r") != strings.TrimRight(b[i], " \t\r") {
			return false
		}
	}
	return true
}

func anySignificant(lines []string) bool {
	for _, l := range lines {
		if isSignificantLine(l) {
			return true
		}
	}
	return false
}

// isSignificantLine : au moins 3 lettres ou chiffres — exclut lignes vides,
// accolades, séparateurs ("---", "*/"...).
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
// nécessaire ici pour reproduire fidèlement la même convention en sortie
// (voir Call). Un fichier vide (0 octet) a 0 ligne, pas une ligne vide.
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

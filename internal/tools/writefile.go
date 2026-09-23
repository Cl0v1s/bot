package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"bot/internal/sandbox"
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
		"Pour modifier une portion ciblée d'un fichier EXISTANT sans regénérer tout son contenu, utilise \"offset\" (et éventuellement \"length\") — repère d'abord la plage de lignes concernée avec read_file (qui indique les numéros de ligne dans sa sortie), plutôt que de relire puis réécrire le fichier entier pour un petit changement : \"length\" > 0 remplace ces lignes par \"content\", \"length\" omis/0 insère \"content\" avant la ligne \"offset\" sans rien supprimer. Sans \"offset\", \"content\" remplace tout le fichier (ou le crée)."
}

func (t *WriteFileTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin ABSOLU du fichier à écrire, dans un répertoire déjà autorisé. Un chemin relatif est refusé."},
			"content": {"type": "string", "description": "Contenu texte. Sans \"offset\" : remplace tout le fichier (ou le crée). Avec \"offset\" : les lignes à insérer/substituer — inséré tel quel, découpé sur les sauts de ligne (n'ajoute pas de saut de ligne final sauf s'il est explicitement présent dans cette valeur)."},
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

	finalContent, err := t.computeFinalContent(resolved, args)
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

	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", fmt.Errorf("création du répertoire parent: %w", err)
	}
	if err := os.WriteFile(resolved, []byte(finalContent), 0o644); err != nil {
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

	if args.Offset > 0 {
		return fmt.Sprintf("fichier %q modifié à partir de la ligne %d (%d octets au total)", args.Path, args.Offset, len(finalContent)), nil
	}
	return fmt.Sprintf("fichier %q écrit (%d octets)", args.Path, len(finalContent)), nil
}

// computeFinalContent retourne le contenu complet à écrire dans resolved :
// args.Content tel quel si args.Offset est omis (remplacement intégral —
// comportement historique, fonctionne aussi pour créer un nouveau fichier),
// sinon le contenu ACTUEL de resolved avec args.Content inséré/substitué à
// partir de la ligne args.Offset (voir Description).
func (t *WriteFileTool) computeFinalContent(resolved string, args writeFileArgs) (string, error) {
	if args.Offset == 0 {
		return args.Content, nil
	}

	existing, err := os.ReadFile(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%q n'existe pas encore : \"offset\" ne peut modifier qu'un fichier existant — omets \"offset\" pour créer ce fichier avec \"content\" comme contenu complet", args.Path)
		}
		return "", fmt.Errorf("lecture de %q avant modification: %w", args.Path, err)
	}
	if len(existing) > 0 && isLikelyBinaryContent(existing) {
		return "", fmt.Errorf("%q semble être un fichier binaire : \"offset\" (édition ligne à ligne) ne s'applique qu'à du texte", args.Path)
	}

	lines, trailingNewline := splitFileLines(existing)
	newLines := strings.Split(args.Content, "\n")

	insertIdx := args.Offset - 1 // 0-based
	var result []string
	if args.Length > 0 {
		endIdx := insertIdx + args.Length // exclusif, 0-based
		if insertIdx > len(lines) || endIdx > len(lines) {
			return "", fmt.Errorf(
				"la plage demandée (lignes %d à %d) dépasse la fin du fichier (%d ligne(s)) : relis le fichier (read_file) pour un offset/length à jour avant de réessayer",
				args.Offset, args.Offset+args.Length-1, len(lines),
			)
		}
		result = make([]string, 0, len(lines)-args.Length+len(newLines))
		result = append(result, lines[:insertIdx]...)
		result = append(result, newLines...)
		result = append(result, lines[endIdx:]...)
	} else {
		if insertIdx > len(lines) {
			return "", fmt.Errorf(
				"offset %d dépasse la fin du fichier (%d ligne(s) ; offset max pour insérer en fin de fichier : %d) : relis le fichier (read_file) pour un offset à jour avant de réessayer",
				args.Offset, len(lines), len(lines)+1,
			)
		}
		result = make([]string, 0, len(lines)+len(newLines))
		result = append(result, lines[:insertIdx]...)
		result = append(result, newLines...)
		result = append(result, lines[insertIdx:]...)
	}

	final := strings.Join(result, "\n")
	if trailingNewline {
		final += "\n"
	}
	return final, nil
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

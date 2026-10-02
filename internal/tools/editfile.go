package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// EditFileTool modifie un fichier texte local existant en remplaçant un
// passage exact par un autre, sans avoir à relire ni à renvoyer le fichier
// entier (contrairement à write_file) : bien plus économe en contexte pour
// un changement ponctuel. Mêmes permissions et même resynchronisation
// sandbox que WriteFileTool — voir ses commentaires.
type EditFileTool struct {
	Perms *DirPermissions
	// MaxBytes : taille maximale du fichier édité (défaut 2 Mo) — le fichier
	// est chargé en mémoire pour le remplacement.
	MaxBytes  int
	Sandboxed bool
}

func (t *EditFileTool) Name() string { return "edit_file" }

func (t *EditFileTool) Description() string {
	return "Modifie un fichier texte existant en remplaçant un passage exact (\"old_string\") par un autre (\"new_string\"), sans relire ni réécrire le reste du fichier : à préférer à write_file pour tout changement ponctuel, bien plus économe en contexte. " +
		"\"old_string\" doit correspondre EXACTEMENT au texte du fichier (espaces et indentation compris) et être unique dans le fichier : ajoute quelques lignes de contexte autour si besoin, ou utilise \"replace_all\" pour remplacer toutes les occurrences. Si tu ne connais pas le texte exact, repère-le d'abord avec read_file (\"search\")."
}

func (t *EditFileTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin ABSOLU du fichier existant à modifier, dans un répertoire déjà autorisé. Un chemin relatif est refusé."},
			"old_string": {"type": "string", "description": "Texte exact à remplacer (espaces, indentation et sauts de ligne compris). Doit être unique dans le fichier, sauf avec \"replace_all\"."},
			"new_string": {"type": "string", "description": "Texte de remplacement (peut être vide pour supprimer le passage)."},
			"replace_all": {"type": "boolean", "description": "Remplace toutes les occurrences de \"old_string\" au lieu d'exiger qu'elle soit unique (défaut false)."}
		},
		"required": ["path", "old_string", "new_string"],
		"additionalProperties": false
	}`)
}

type editFileArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (t *EditFileTool) Call(ctx context.Context, argsJSON string) (string, error) {
	var args editFileArgs
	if err := decodeArgs(argsJSON, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", fmt.Errorf(`paramètre "path" requis`)
	}
	if err := requireAbsolutePath("path", args.Path); err != nil {
		return "", err
	}
	if args.OldString == "" {
		return "", fmt.Errorf(`paramètre "old_string" requis et non vide (pour créer un fichier ou en ajouter la fin, utilise write_file)`)
	}
	if args.OldString == args.NewString {
		return "", fmt.Errorf(`"old_string" et "new_string" sont identiques : rien à modifier`)
	}

	resolved, err := t.Perms.CheckFileWrite(args.Path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%q n'existe pas : edit_file ne modifie qu'un fichier existant (utilise write_file pour le créer)", args.Path)
		}
		return "", fmt.Errorf("lecture de %q: %w", args.Path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%q est un répertoire, pas un fichier", args.Path)
	}
	maxBytes := t.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	if info.Size() > int64(maxBytes) {
		return "", fmt.Errorf("%q est trop volumineux pour edit_file (%d octets, max %d)", args.Path, info.Size(), maxBytes)
	}

	if err := syncBeforeWrite(t.Sandboxed, resolved, "edit_file"); err != nil {
		return "", err
	}
	if err := checkExistingFile(resolved, false); err != nil {
		return "", err
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("lecture de %q: %w", args.Path, err)
	}
	if isLikelyBinaryContent(data) {
		return "", fmt.Errorf("%q semble être un fichier binaire : edit_file ne modifie que du texte", args.Path)
	}

	content := string(data)
	count := strings.Count(content, args.OldString)
	switch {
	case count == 0:
		return "", fmt.Errorf(`"old_string" introuvable dans %q : le texte doit correspondre exactement (espaces et indentation compris). Repère-le avec read_file ("search") avant de réessayer`, args.Path)
	case count > 1 && !args.ReplaceAll:
		return "", fmt.Errorf(`"old_string" apparaît %d fois dans %q : ajoute du contexte autour pour le rendre unique, ou utilise "replace_all"`, count, args.Path)
	}

	n := 1
	if args.ReplaceAll {
		n = -1
	} else {
		count = 1
	}
	updated := strings.Replace(content, args.OldString, args.NewString, n)

	if err := writeAndSync(t.Sandboxed, resolved, []byte(updated), "edit_file"); err != nil {
		return "", err
	}
	return fmt.Sprintf("fichier %q modifié (%d remplacement(s))", args.Path, count), nil
}

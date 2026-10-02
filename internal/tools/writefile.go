package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// WriteFileTool écrit (crée ou remplace intégralement) un fichier texte
// local. L'accès n'est autorisé que si le répertoire du fichier a été
// préautorisé ou accordé via RequestDirectoryAccessTool — voir
// DirPermissions.
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
	return "Écrit un fichier texte local en entier (le crée, ou remplace intégralement son contenu), dans un répertoire déjà autorisé (voir request_directory_access). Crée les répertoires parents si besoin. Pour modifier un fichier existant, préfère edit_file (remplacement ciblé, sans relire ni renvoyer le fichier entier : bien plus économe en contexte) ; n'utilise write_file pour un fichier existant que pour le réécrire presque entièrement, auquel cas tout ce qui n'est pas dans \"content\" est perdu. Pour ajouter du texte à la FIN d'un fichier sans le relire (mémoire, journal, fichier long écrit par morceaux), utilise \"append\": true."
}

func (t *WriteFileTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Chemin ABSOLU du fichier à écrire, dans un répertoire déjà autorisé. Un chemin relatif est refusé."},
			"content": {"type": "string", "description": "Contenu texte COMPLET du fichier : remplace intégralement son contenu actuel (ou le crée). Avec \"append\", texte à ajouter à la fin du fichier existant."},
			"append": {"type": "boolean", "description": "Si vrai, ajoute \"content\" à la fin du fichier (créé s'il n'existe pas) au lieu de le remplacer. À utiliser pour compléter un fichier sans le relire (défaut false)."}
		},
		"required": ["path", "content"],
		"additionalProperties": false
	}`)
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append"`
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

	// Voir writesync.go : resynchronisation limitée au répertoire du
	// fichier, puis contrôle d'accès au fichier lui-même.
	if err := syncBeforeWrite(t.Sandboxed, resolved, "write_file"); err != nil {
		return "", err
	}

	if args.Append {
		existing, err := os.ReadFile(resolved)
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("lecture de %q: %w", args.Path, err)
		}
		if err := writeAndSync(t.Sandboxed, resolved, append(existing, args.Content...), "write_file"); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d octets ajoutés à la fin de %q", len(args.Content), args.Path), nil
	}

	if err := writeAndSync(t.Sandboxed, resolved, []byte(args.Content), "write_file"); err != nil {
		return "", err
	}
	return fmt.Sprintf("fichier %q écrit (%d octets)", args.Path, len(args.Content)), nil
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

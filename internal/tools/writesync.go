package tools

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"bot/internal/sandbox"
)

// Écriture de fichier sous l'identité réelle (write_file, "save_to" de
// http_get/browser_fetch), resynchronisée avec le compte sandbox quand il
// est actif — voir WriteFileTool.Sandboxed. Séquence commune aux deux
// outils, en deux temps :
//
//  1. syncBeforeWrite, juste après DirPermissions.CheckFileWrite ;
//  2. writeAndSync, pour l'écriture elle-même.
//
// La resynchronisation ne porte jamais que sur le répertoire du fichier (et
// les répertoires créés pour lui), jamais sur un ancêtre existant plus haut :
// écrire /tmp/nouveau/x ne doit pas parcourir tout /tmp (chgrp + g+rw de
// fichiers d'autres applications de l'utilisateur sans aucun rapport). Elle
// est aussi sautée pour un répertoire appartenant à quelqu'un d'autre (ex:
// /tmp lui-même, ou un dossier créé par run_shell donc par le compte
// sandbox) : GrantDirectory y échouerait à coup sûr, et c'est inutile.

// syncBeforeWrite resynchronise le répertoire de resolved s'il existe déjà
// (un fichier présent peut appartenir au compte sandbox sans écriture groupe
// — l'écriture échouerait sinon), puis vérifie que le fichier lui-même, s'il
// existe, est bien inscriptible par le compte sandbox (voir
// checkExistingFile). Best-effort pour la resynchronisation, pas pour le
// contrôle.
func syncBeforeWrite(sandboxed bool, resolved, tool string) error {
	dir := filepath.Dir(resolved)
	if sandboxed && !dirOwnedByOther(dir) {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			if err := sandbox.GrantDirectory(dir); err != nil {
				log.Printf("%s: échec de la resynchronisation sandbox de %q: %v", tool, dir, err)
			}
		}
	}
	return checkExistingFile(resolved, true)
}

// writeAndSync crée les répertoires parents manquants, écrit content dans
// resolved (0644), puis resynchronise ce qui vient d'être créé : le
// répertoire du fichier, ou le plus haut des répertoires créés pour lui.
func writeAndSync(sandboxed bool, resolved string, content []byte, tool string) error {
	dir := filepath.Dir(resolved)
	syncTarget := topmostMissing(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("création du répertoire parent: %w", err)
	}
	if err := os.WriteFile(resolved, content, 0o644); err != nil {
		return fmt.Errorf("écriture de %q: %w", resolved, err)
	}
	// Best-effort : le fichier est déjà écrit avec succès à ce stade.
	if sandboxed && !dirOwnedByOther(syncTarget) {
		if err := sandbox.GrantDirectory(syncTarget); err != nil {
			log.Printf("%s: échec de la resynchronisation sandbox de %q: %v", tool, syncTarget, err)
		}
	}
	return nil
}

// topmostMissing retourne dir s'il existe, sinon le plus haut de ses
// ancêtres qui n'existe pas encore (celui que MkdirAll créera en premier).
func topmostMissing(dir string) string {
	existing := nearestExisting(dir)
	if existing == dir {
		return dir
	}
	rel, err := filepath.Rel(existing, dir)
	if err != nil {
		return dir
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return filepath.Join(existing, first)
}

// dirOwnedByOther indique si dir existe et appartient à un autre utilisateur
// que celui du processus.
func dirOwnedByOther(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) != os.Geteuid()
}

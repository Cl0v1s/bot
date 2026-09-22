package sandbox

import (
	"os"
	"path/filepath"
)

// sandboxHomeDirName : sous-répertoire du workspace utilisé comme HOME pour
// les commandes exécutées via WrapCommand. User est un compte système sans
// répertoire personnel créé sur disque (son entrée passwd pointe vers un
// chemin comme /var/home/llm ou /home/llm qui n'existe pas et que User ne
// peut pas créer, son parent appartenant à root) : tout outil qui a besoin
// d'écrire une config/un cache sous $HOME échoue sinon avec un "permission
// denied" (observé avec `glab auth login`, mais générique à tout outil du
// genre — npm, docker, ou n'importe quel outil suivant la XDG Base
// Directory Spec, qui retombe sur $HOME/.config par défaut). Un HOME dédié,
// à l'intérieur du workspace déjà accordé au sandbox via GrantDirectory,
// résout ça pour n'importe quel outil, pas seulement git — qui a sa propre
// solution dédiée (voir gitconfig.go) restant en place mais devenant de ce
// fait redondante avec HOME correctement réglé.
const sandboxHomeDirName = ".sandbox-home"

// SandboxHomeDir retourne le chemin du HOME dédié au sandbox à l'intérieur
// de dir (typiquement le workspace). À passer à WrapCommand une fois dir
// accordé à User via GrantDirectory.
func SandboxHomeDir(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, sandboxHomeDirName)
}

// EnsureSandboxHome crée, si absent, le répertoire retourné par
// SandboxHomeDir. Ne le vide ni ne le recrée s'il existe déjà : son contenu
// (configs/caches écrits par les outils qui s'en servent comme HOME) doit
// persister d'une session à l'autre comme n'importe quel autre cache. dir
// doit déjà être un répertoire accordé à User (voir GrantDirectory), qui
// donne alors à User le droit d'y créer des fichiers/sous-répertoires — mais
// SANS descendre dans ce sous-répertoire lui-même (voir le commentaire de
// GrantDirectory sur sandboxHomeDirName) : son contenu reste aux permissions
// que les outils de User lui-même leur donnent, jamais élargi.
func EnsureSandboxHome(dir string) error {
	home := SandboxHomeDir(dir)
	if home == "" {
		return nil
	}
	return os.MkdirAll(home, 0o755)
}

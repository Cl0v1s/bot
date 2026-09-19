// Package skills charge les skills déclarées par l'utilisateur dans le
// sous-répertoire "skills" du workspace du bot (voir
// internal/config.Config.SkillsDir). Une skill est un fichier Markdown
// (directement dans le répertoire, ou nommé SKILL.md dans un
// sous-répertoire dédié) avec un en-tête optionnel délimité par "---" :
//
//	---
//	name: nom-de-la-skill
//	description: ce que fait la skill et quand l'utiliser
//	---
//	Instructions détaillées pour le modèle...
//
// Seuls le nom et la description sont chargés en mémoire au démarrage (et
// injectés dans le prompt système) : le corps n'est lu par le modèle que
// s'il choisit d'appeler read_file sur le chemin de la skill, pour éviter de
// saturer le contexte avec des skills non pertinentes pour la conversation
// en cours.
package skills

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill est une skill déclarée par l'utilisateur.
type Skill struct {
	Name        string
	Description string
	Path        string // chemin absolu du fichier Markdown, à lire via read_file pour les instructions complètes
}

// defaultSkillFile est la skill fournie d'office dans le workspace (voir
// EnsureDefaults) : elle explique au modèle lui-même comment déclarer de
// nouvelles skills dans ce même répertoire.
const defaultSkillFile = "creer-une-skill.md"

const defaultSkillContent = `---
name: creer-une-skill
description: Explique comment déclarer une nouvelle skill dans ce workspace (format, emplacement, en-tête). À utiliser quand on te demande de créer, ajouter ou déclarer une nouvelle skill.
---
# Créer une skill

Une skill est un fichier Markdown placé dans ce répertoire ("skills/"), avec
un en-tête optionnel délimité par des lignes "---" :

` + "```" + `
---
name: nom-de-la-skill
description: ce que fait la skill et quand l'utiliser
---
Instructions détaillées pour le modèle : étapes à suivre, exemples,
contraintes...
` + "```" + `

Deux emplacements possibles :
- directement "skills/nom-de-la-skill.md" ;
- ou "skills/nom-de-la-skill/SKILL.md", si la skill a besoin de fichiers
  annexes rangés dans son propre sous-répertoire.

Règles :
- "name" et "description" sont chargés au démarrage du bot et ajoutés au
  prompt système (liste des skills disponibles) : la description doit être
  courte et précise, c'est elle qui permet de savoir quand lire et utiliser
  la skill.
- Le corps (après l'en-tête) n'est lu par le modèle que s'il appelle
  read_file sur le chemin indiqué : les instructions détaillées peuvent donc
  être aussi longues que nécessaire, elles ne polluent pas le contexte par
  défaut.
- Sans en-tête, le nom du fichier (ou du sous-répertoire) sert de nom, et la
  description reste vide — toujours préférable de renseigner les deux
  explicitement.
- Illustre toujours les instructions par des exemples concrets (une commande
  exacte plutôt qu'une description vague, un extrait avant/après...) : une
  skill qui ne fait qu'expliquer en abstrait est plus difficile à appliquer
  correctement qu'une skill qui montre. Exemple ci-dessous.

Pour créer une nouvelle skill, écris un fichier à ce format dans ce
répertoire avec write_file (déjà accessible sans permission particulière,
c'est le workspace du bot). Une nouvelle skill n'apparaît dans la liste des
skills disponibles qu'après redémarrage du bot : le chargement se fait une
seule fois, au démarrage.

## Exemple : bonnes pratiques pour les appels de commande (run_shell)

Voici, à titre d'exemple de skill bien écrite (instructions courtes, chacune
illustrée), des bonnes pratiques pour appeler ` + "`run_shell`" + ` :

**Toujours donner un exemple concret.** Mauvais : « Je vais lister les
fichiers modifiés récemment. » Bon :

` + "```" + `
find . -maxdepth 2 -type f -mtime -1
` + "```" + `

**Explorer en lecture seule avant de modifier.** Avant un ` + "`mv`" + `, vérifie d'abord
ce que tu cibles :

` + "```" + `
ls -la ancien_nom.txt
mv ancien_nom.txt nouveau_nom.txt
` + "```" + `

**Une commande à la fois, pas de longues chaînes.** Mauvais :
` + "`grep -rl TODO . | xargs sed -i '' 's/TODO/FIXME/g' && git diff --stat`" + `
(un maillon peut échouer silencieusement). Préfère plusieurs appels courts,
en vérifiant chaque résultat avant d'enchaîner.

**Pas besoin de ` + "`cd`" + `.** ` + "`run_shell`" + ` s'exécute déjà dans le répertoire
autorisé courant : inutile d'enchaîner un ` + "`cd`" + ` vers le même endroit. Pour un
autre répertoire, la commande demandera elle-même l'accès
(` + "`request_directory_access`" + `).

**Pas de commandes interactives.** ` + "`run_shell`" + ` n'a pas d'entrée standard :
une commande qui attend une confirmation reste bloquée jusqu'au timeout.
Mauvais : ` + "`npm init`" + ` (pose des questions). Bon : ` + "`npm init -y`" + `.

**Vérifie le résultat avant d'enchaîner.** Ne suppose pas qu'une commande a
réussi : regarde la sortie réellement renvoyée avant de décrire le résultat
ou de lancer la commande suivante.

**Certaines commandes sont bloquées, pas la peine d'insister.**
` + "`rm -rf`" + `/` + "`rm -f`" + `, le formatage de disque, l'arrêt machine, etc. sont
refusés avant même de s'exécuter (garde-fou dans le code). Si une commande
revient avec « motif dangereux détecté », propose une alternative plutôt que
de la reformuler pour contourner le filtre.
`

// EnsureDefaults crée, si absente, la skill "creer-une-skill" dans dir (voir
// defaultSkillContent) : le dossier skills/ du workspace contient toujours
// d'office cette meta-skill expliquant comment en déclarer de nouvelles.
// N'écrase jamais un fichier déjà présent à ce chemin (y compris modifié ou
// vidé par l'utilisateur) : seule son absence déclenche la (re)création.
func EnsureDefaults(dir string) error {
	path := filepath.Join(dir, defaultSkillFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(defaultSkillContent), 0o644)
}

// Load scans dir à la recherche de skills : chaque fichier "*.md" trouvé
// directement dans dir, ou dans un sous-répertoire immédiat sous la forme
// "<sous-répertoire>/SKILL.md". Un dir absent n'est pas une erreur (aucune
// skill déclarée).
func Load(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []Skill
	for _, e := range entries {
		var path string
		switch {
		case !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".md"):
			path = filepath.Join(dir, e.Name())
		case e.IsDir():
			candidate := filepath.Join(dir, e.Name(), "SKILL.md")
			if _, err := os.Stat(candidate); err != nil {
				continue
			}
			path = candidate
		default:
			continue
		}

		abs, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		s, err := parseSkillFile(abs)
		if err != nil {
			continue
		}
		out = append(out, s)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parseSkillFile lit l'en-tête "name"/"description" d'un fichier de skill.
// À défaut d'en-tête, le nom du fichier (sans extension) sert de nom, et la
// description reste vide.
func parseSkillFile(path string) (Skill, error) {
	s := Skill{
		Name: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Path: path,
	}
	// Une skill sous forme de sous-répertoire (.../<nom>/SKILL.md) prend le
	// nom de son répertoire plutôt que "SKILL".
	if strings.EqualFold(filepath.Base(path), "SKILL.md") {
		s.Name = filepath.Base(filepath.Dir(path))
	}

	f, err := os.Open(path)
	if err != nil {
		return Skill{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return s, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.ToLower(key) {
		case "name":
			if value != "" {
				s.Name = value
			}
		case "description":
			s.Description = value
		}
	}
	return s, scanner.Err()
}

// Summary rend un résumé textuel des skills disponibles, destiné à être
// ajouté au prompt système : le modèle y voit le nom, la description et le
// chemin de chaque skill, et peut lire ce chemin via read_file pour obtenir
// les instructions complètes le moment venu. Retourne "" si skills est vide.
func Summary(list []Skill) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Skills disponibles (lis le fichier indiqué via read_file pour les instructions complètes avant de l'utiliser) :\n")
	for _, s := range list {
		if s.Description != "" {
			b.WriteString("- " + s.Name + " : " + s.Description + " [" + s.Path + "]\n")
		} else {
			b.WriteString("- " + s.Name + " [" + s.Path + "]\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

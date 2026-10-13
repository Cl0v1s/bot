package chat

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"bot/internal/ontology"
)

// isOntologyCommand : "/ontology" seul ou suivi d'arguments.
func isOntologyCommand(line string) bool {
	f := strings.Fields(line)
	return len(f) > 0 && f[0] == "/ontology"
}

// runOntologyCommand traite /ontology :
//
//	/ontology              taille du graphe
//	/ontology explore [q]  SVG du graphe (ou du voisinage de q) dans le scratchpad
//	/ontology clean        plan de nettoyage, sans rien modifier
//	/ontology clean apply  applique (après sauvegarde), puis fait arbitrer les
//	                       doublons possibles par le modèle et fusionne ceux
//	                       qu'il confirme
func runOntologyCommand(ctx context.Context, out io.Writer, store *ontology.Store, d *ontology.Distiller, args []string, orphanAge time.Duration, scratchDir string) error {
	if store == nil {
		fmt.Fprintln(out, "[ontologie désactivée (ONTOLOGY_ENABLED=false)]")
		return nil
	}
	switch {
	case len(args) == 0:
		nodes, edges, err := store.Counts(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "[ontologie : %d concepts, %d relations — /ontology clean pour analyser les doublons et le superflu]\n", nodes, edges)
		return nil

	case args[0] == "explore":
		return exploreOntology(ctx, out, store, args[1:], scratchDir)

	case args[0] == "clean" && (len(args) == 1 || (len(args) == 2 && args[1] == "apply")):
		apply := len(args) == 2
		rep, err := store.Clean(ctx, ontology.CleanOptions{Apply: apply, OrphanMaxAge: orphanAge})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, rep.String())
		if !apply {
			if rep.Changed() || len(rep.Candidates) > 0 {
				fmt.Fprintln(out, "[rien n'a été modifié — /ontology clean apply pour appliquer et faire arbitrer les doublons possibles par le modèle]")
			}
			return nil
		}
		if len(rep.Candidates) == 0 {
			return nil
		}
		// Les candidats ont été calculés avant les fusions : ceux dont un
		// concept a disparu depuis échoueront dans Merge ("introuvable") ;
		// on relance donc le plan sur la base nettoyée.
		after, err := store.Clean(ctx, ontology.CleanOptions{})
		if err != nil {
			return err
		}
		ops, err := d.JudgeCandidates(ctx, after.Candidates)
		if err != nil {
			return err
		}
		n, err := store.ApplyMerges(ctx, ops)
		for _, m := range ops[:n] {
			fmt.Fprintf(out, "[fusion confirmée par le modèle : %q → %q]\n", m.Drop, m.Keep)
		}
		if err != nil {
			return err
		}
		if kept := len(after.Candidates) - n; kept > 0 {
			fmt.Fprintf(out, "[%d paire(s) laissée(s) distincte(s)]\n", kept)
		}
		return nil
	}
	fmt.Fprintln(out, "usage : /ontology | /ontology explore [concept] [profondeur] | /ontology clean | /ontology clean apply")
	return nil
}

// exploreOntology écrit un SVG du graphe dans scratchDir. args : un fragment
// de nom de concept (optionnel ; sinon les concepts les plus connectés) et,
// en dernier, une profondeur en sauts (1 à 4, défaut 2).
func exploreOntology(ctx context.Context, out io.Writer, store *ontology.Store, args []string, scratchDir string) error {
	depth := 2
	if n := len(args); n > 1 {
		if d, err := strconv.Atoi(args[n-1]); err == nil && d >= 1 {
			depth, args = d, args[:n-1]
		}
	}
	query := strings.Join(args, " ")
	sg, err := store.Explore(ctx, query, depth, ontology.DefaultExploreNodes)
	if err != nil {
		return err
	}
	if len(sg.Nodes) == 0 {
		if query == "" {
			fmt.Fprintln(out, "[ontologie vide ou sans aucun lien : rien à dessiner]")
		} else {
			fmt.Fprintf(out, "[aucun concept ne correspond à %q]\n", query)
		}
		return nil
	}
	title := "Ontologie"
	slug := "ontologie"
	if query != "" {
		title = fmt.Sprintf("Ontologie autour de %q (%d saut(s))", query, depth)
		slug += "-" + slugify(query)
	}
	if scratchDir == "" {
		scratchDir = os.TempDir()
	}
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(scratchDir, fmt.Sprintf("%s-%s.svg", slug, time.Now().Format("20060102-150405")))
	if err := os.WriteFile(path, ontology.RenderSVG(sg, title), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "[SVG écrit : %s — %d concepts, %d relations%s]\n", path, len(sg.Nodes), len(sg.Edges), map[bool]string{true: ", tronqué", false: ""}[sg.Truncated])
	return nil
}

// slugify : nom de fichier sûr (lettres/chiffres, tirets), borné.
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if r := []rune(out); len(r) > 30 {
		out = string(r[:30])
	}
	return out
}

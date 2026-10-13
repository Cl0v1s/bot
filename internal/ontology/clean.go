package ontology

// Nettoyage de l'ontologie : fusion des doublons, purge des concepts
// orphelins et périmés, normalisation des relations. Le plan est calculé
// sur un instantané ; il n'est appliqué (après sauvegarde du fichier) que si
// CleanOptions.Apply est vrai.
//
// Seules les opérations sûres (déterministes) sont faites ici. Les paires
// "probablement identiques" (Candidates) sont seulement signalées : les
// trancher demande un jugement (voir Distiller.JudgeCandidates).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	graph "github.com/justintout/go-sqlite-graph"
	"zombiezen.com/go/sqlite"
)

const (
	maxCandidates   = 40
	maxCandidateSet = 4000 // au-delà, la recherche de candidats (O(n²)) est sautée
	keepBackups     = 3
	maxAliases      = 20
)

// CleanOptions règle un nettoyage.
type CleanOptions struct {
	Apply bool // false : calcule seulement le plan
	// OrphanMaxAge : un concept sans aucune relation, cité une seule fois et
	// pas revu depuis plus longtemps que cela est supprimé. 0 : pas de purge.
	OrphanMaxAge time.Duration
}

// MergeOp fusionne le concept Drop dans le concept Keep.
type MergeOp struct {
	KeepID, DropID int64
	Keep, Drop     string
}

// Candidate est une paire de concepts peut-être identiques.
type Candidate struct {
	A, B   Concept
	Reason string
}

// Concept est la vue d'un nœud utile au nettoyage.
type Concept struct {
	ID          int64
	Name        string
	Type        string
	Description string
}

// CleanReport décrit ce qu'un nettoyage a fait (Apply) ou ferait.
type CleanReport struct {
	Merges           []MergeOp
	LabelsRepaired   int // concepts qui avaient perdu leur label (voir findByKey)
	Orphans          []string
	orphanIDs        []int64
	repairIDs        []int64
	RelationsRenamed int
	EdgesRemoved     int // doublons et boucles
	Candidates       []Candidate
	BackupPath       string
	Applied          bool
}

// Changed indique si le nettoyage déterministe modifie quelque chose.
func (r CleanReport) Changed() bool {
	return len(r.Merges) > 0 || len(r.Orphans) > 0 || r.RelationsRenamed > 0 || r.LabelsRepaired > 0 || r.EdgesRemoved > 0
}

// Summary : une ligne, pour le démarrage.
func (r CleanReport) Summary() string {
	return fmt.Sprintf("%d fusion(s), %d orphelin(s) supprimé(s), %d relation(s) renommée(s), %d lien(s) en trop retiré(s), %d concept(s) réparé(s)",
		len(r.Merges), len(r.Orphans), r.RelationsRenamed, r.EdgesRemoved, r.LabelsRepaired)
}

func (r CleanReport) String() string {
	var b strings.Builder
	verb := "à faire"
	if r.Applied {
		verb = "fait"
	}
	fmt.Fprintf(&b, "Nettoyage %s : %s.\n", verb, r.Summary())
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s :\n", title)
		for _, it := range items {
			fmt.Fprintf(&b, "  - %s\n", it)
		}
	}
	var merges []string
	for _, m := range r.Merges {
		merges = append(merges, fmt.Sprintf("%q → %q", m.Drop, m.Keep))
	}
	list("Fusions", merges)
	list("Orphelins", r.Orphans)
	var cands []string
	for _, c := range r.Candidates {
		cands = append(cands, fmt.Sprintf("%q ? %q (%s)", c.A.Name, c.B.Name, c.Reason))
	}
	list("Doublons possibles (à trancher)", cands)
	if r.BackupPath != "" {
		fmt.Fprintf(&b, "Sauvegarde : %s\n", r.BackupPath)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------------------------------------------------------------------------
// Normalisation de noms et de relations
// ---------------------------------------------------------------------------

var accentFolder = strings.NewReplacer(
	"à", "a", "â", "a", "ä", "a", "é", "e", "è", "e", "ê", "e", "ë", "e",
	"î", "i", "ï", "i", "ô", "o", "ö", "o", "ù", "u", "û", "u", "ü", "u",
	"ç", "c", "œ", "oe", "æ", "ae", "’", "'",
)

var leadingArticles = map[string]bool{
	"le": true, "la": true, "les": true, "l": true, "un": true, "une": true,
	"des": true, "du": true, "de": true, "d": true, "the": true, "a": true,
}

// nameTokens : nom sans casse, accents, ponctuation ni article initial, au
// singulier approximatif ("Les Bases de données" ~ "base de donnee").
func nameTokens(name string) []string {
	s := accentFolder.Replace(strings.ToLower(name))
	toks := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for len(toks) > 1 && leadingArticles[toks[0]] {
		toks = toks[1:]
	}
	for i, t := range toks {
		if len(t) > 3 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss") {
			toks[i] = t[:len(t)-1]
		}
	}
	return toks
}

func normKey(name string) string { return strings.Join(nameTokens(name), " ") }

// relationAliases ramène les formulations courantes d'une même relation à une
// forme canonique (clés et valeurs déjà passées par cleanRelation).
var relationAliases = map[string]string{
	"SE_SERT_DE":                "UTILISE",
	"EMPLOIE":                   "UTILISE",
	"UTILISÉ_PAR_LE_PROJET":     "UTILISE",
	"APPARTIENT":                "APPARTIENT_À",
	"FAIT_PARTIE_DE":            "APPARTIENT_À",
	"EST_UN_SOUS_ENSEMBLE_DE":   "EST_UNE_SOUS_CATÉGORIE_DE",
	"EST_UNE_SOUS_CATEGORIE_DE": "EST_UNE_SOUS_CATÉGORIE_DE",
	"DEPEND_DE":                 "DÉPEND_DE",
	"HEBERGE":                   "HÉBERGE",
	"PREFERE":                   "PRÉFÈRE",
}

func canonicalRelation(r string) string {
	if c, ok := relationAliases[r]; ok {
		return c
	}
	return r
}

// ---------------------------------------------------------------------------
// Instantané
// ---------------------------------------------------------------------------

type snapNode struct {
	Concept
	props    map[string]any
	mentions int
	lastSeen time.Time
	degree   int
	noLabel  bool // sans label Concept : invisible pour Match
}

type snapEdge struct {
	id, src, dst int64
	typ          string
}

type snapshot struct {
	nodes []*snapNode
	byID  map[int64]*snapNode
	edges []snapEdge
}

func (s *Store) load(ctx context.Context) (*snapshot, error) {
	snap := &snapshot{byID: map[int64]*snapNode{}}
	err := s.query(ctx, `SELECT n.id, n.name, n.properties, n.updated_at,
			(SELECT group_concat(label, ',') FROM node_labels l WHERE l.node_id = n.id AND l.label <> ?1)
		, NOT EXISTS (SELECT 1 FROM node_labels l WHERE l.node_id = n.id AND l.label = ?1)
		FROM nodes n ORDER BY n.id`, []any{conceptLabel}, func(st *sqlite.Stmt) error {
		n := &snapNode{Concept: Concept{ID: st.ColumnInt64(0), Name: st.ColumnText(1)}}
		if err := json.Unmarshal([]byte(st.ColumnText(2)), &n.props); err != nil || n.props == nil {
			n.props = map[string]any{}
		}
		n.Description = firstString(n.props, "description")
		n.mentions = numberProp(n.props, "mentions")
		for _, ts := range []string{firstString(n.props, "last_seen"), st.ColumnText(3)} {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				n.lastSeen = t
				break
			}
		}
		n.Type = defaultType
		if labels := st.ColumnText(4); labels != "" {
			n.Type = strings.Split(labels, ",")[0]
		}
		n.noLabel = st.ColumnInt(5) != 0
		snap.nodes = append(snap.nodes, n)
		snap.byID[n.ID] = n
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = s.query(ctx, `SELECT id, source_id, target_id, type FROM edges ORDER BY id`, nil, func(st *sqlite.Stmt) error {
		e := snapEdge{id: st.ColumnInt64(0), src: st.ColumnInt64(1), dst: st.ColumnInt64(2), typ: st.ColumnText(3)}
		snap.edges = append(snap.edges, e)
		if n := snap.byID[e.src]; n != nil {
			n.degree++
		}
		if n := snap.byID[e.dst]; n != nil && e.dst != e.src {
			n.degree++
		}
		return nil
	})
	return snap, err
}

// ---------------------------------------------------------------------------
// Plan
// ---------------------------------------------------------------------------

type edgeFix struct {
	id     int64
	delete bool
	newTyp string
}

// better : le concept à conserver d'un groupe de doublons (le plus connecté,
// puis le plus cité, puis le plus ancien).
func better(a, b *snapNode) bool {
	if a.degree != b.degree {
		return a.degree > b.degree
	}
	if a.mentions != b.mentions {
		return a.mentions > b.mentions
	}
	return a.ID < b.ID
}

func plan(snap *snapshot, opts CleanOptions, now time.Time) (CleanReport, []edgeFix) {
	var rep CleanReport

	// Relations : forme canonique, puis doublons exacts.
	var fixes []edgeFix
	type edgeKey struct {
		src, dst int64
		typ      string
	}
	seen := map[edgeKey]bool{}
	for _, e := range snap.edges {
		typ := canonicalRelation(e.typ)
		k := edgeKey{e.src, e.dst, typ}
		switch {
		case e.src == e.dst || seen[k]:
			fixes = append(fixes, edgeFix{id: e.id, delete: true})
			rep.EdgesRemoved++
		case typ != e.typ:
			seen[k] = true
			fixes = append(fixes, edgeFix{id: e.id, newTyp: typ})
			rep.RelationsRenamed++
		default:
			seen[k] = true
		}
	}

	// Doublons : même nom une fois normalisé.
	groups := map[string][]*snapNode{}
	var order []string
	for _, n := range snap.nodes {
		k := normKey(n.Name)
		if k == "" {
			continue
		}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], n)
	}
	dropped := map[int64]bool{}
	for _, k := range order {
		g := groups[k]
		if len(g) < 2 {
			continue
		}
		keep := g[0]
		for _, n := range g[1:] {
			if better(n, keep) {
				keep = n
			}
		}
		for _, n := range g {
			if n != keep {
				rep.Merges = append(rep.Merges, MergeOp{KeepID: keep.ID, DropID: n.ID, Keep: keep.Name, Drop: n.Name})
				dropped[n.ID] = true
			}
		}
	}

	// Concepts à qui il manque le label commun (base abîmée par l'ancien
	// findByKey) : sans lui, ni Match ni donc l'extraction ne les retrouvent.
	for _, n := range snap.nodes {
		if n.noLabel && !dropped[n.ID] {
			rep.repairIDs = append(rep.repairIDs, n.ID)
			rep.LabelsRepaired++
		}
	}

	// Orphelins périmés.
	if opts.OrphanMaxAge > 0 {
		for _, n := range snap.nodes {
			if dropped[n.ID] || n.degree > 0 || n.mentions > 1 || n.lastSeen.IsZero() {
				continue
			}
			if now.Sub(n.lastSeen) > opts.OrphanMaxAge {
				rep.Orphans = append(rep.Orphans, n.Name)
				rep.orphanIDs = append(rep.orphanIDs, n.ID)
				dropped[n.ID] = true
			}
		}
	}

	rep.Candidates = candidates(snap, dropped)
	return rep, fixes
}

// candidates repère des paires de noms proches mais pas identiques une fois
// normalisés : l'un contient les mots de l'autre ("Synapse" / "projet
// Synapse"), ou ils ne diffèrent que d'une faute de frappe.
func candidates(snap *snapshot, dropped map[int64]bool) []Candidate {
	var live []*snapNode
	for _, n := range snap.nodes {
		if !dropped[n.ID] && normKey(n.Name) != "" {
			live = append(live, n)
		}
	}
	if len(live) > maxCandidateSet {
		return nil
	}
	type prepared struct {
		n    *snapNode
		toks []string
		key  string
	}
	ps := make([]prepared, len(live))
	for i, n := range live {
		toks := nameTokens(n.Name)
		ps[i] = prepared{n, toks, strings.Join(toks, " ")}
	}
	var out []Candidate
	for i := 0; i < len(ps) && len(out) < maxCandidates; i++ {
		for j := i + 1; j < len(ps) && len(out) < maxCandidates; j++ {
			a, b := ps[i], ps[j]
			if a.key == b.key {
				continue // déjà couvert par les fusions
			}
			reason := ""
			switch {
			case subset(a.toks, b.toks) || subset(b.toks, a.toks):
				reason = "l'un contient l'autre"
			case similarNames(a.key, b.key):
				reason = "noms très proches"
			}
			if reason != "" {
				out = append(out, Candidate{A: a.n.Concept, B: b.n.Concept, Reason: reason})
			}
		}
	}
	return out
}

// subset : tous les mots de small sont dans big, et small porte au moins un
// mot significatif (pas seulement "de", "api"...).
func subset(small, big []string) bool {
	if len(small) >= len(big) {
		return false
	}
	long := false
	for _, w := range small {
		if len(w) >= 4 {
			long = true
		}
	}
	if !long {
		return false
	}
	set := map[string]bool{}
	for _, w := range big {
		set[w] = true
	}
	for _, w := range small {
		if !set[w] {
			return false
		}
	}
	return true
}

func similarNames(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	short := min(len(ra), len(rb))
	if short < 5 || abs(len(ra)-len(rb)) > 2 {
		return false
	}
	limit := 1
	if short >= 9 {
		limit = 2
	}
	return levenshtein(ra, rb) <= limit
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// ---------------------------------------------------------------------------
// Exécution
// ---------------------------------------------------------------------------

// Clean calcule le plan de nettoyage et, si opts.Apply, l'exécute après avoir
// sauvegardé la base (seulement s'il y a quelque chose à modifier).
func (s *Store) Clean(ctx context.Context, opts CleanOptions) (CleanReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap, err := s.load(ctx)
	if err != nil {
		return CleanReport{}, err
	}
	rep, fixes := plan(snap, opts, time.Now())
	if !opts.Apply || !rep.Changed() {
		return rep, nil
	}

	if rep.BackupPath, err = s.backup(ctx); err != nil {
		return rep, fmt.Errorf("sauvegarde avant nettoyage: %w", err)
	}
	for _, f := range fixes {
		if f.delete {
			err = s.g.DeleteEdge(ctx, f.id)
		} else {
			var e *graph.Edge
			if e, err = s.g.GetEdge(ctx, f.id); err == nil {
				e.Type = f.newTyp
				err = s.g.UpdateEdge(ctx, e)
			}
		}
		if err != nil {
			return rep, err
		}
	}
	for _, id := range rep.repairIDs {
		if err := s.g.AddLabels(ctx, id, conceptLabel); err != nil {
			return rep, err
		}
	}
	for _, m := range rep.Merges {
		if err := s.merge(ctx, m.KeepID, m.DropID); err != nil {
			return rep, err
		}
	}
	// Les orphelins ont été choisis sur l'instantané : un concept orphelin
	// n'a pu gagner de lien depuis (s.mu est tenu), seulement le perdre.
	for _, id := range rep.orphanIDs {
		if err := s.g.DeleteNode(ctx, id); err != nil {
			return rep, err
		}
	}
	rep.Applied = true
	return rep, nil
}

// Merge fusionne le concept drop dans keep : liens réattribués (sans
// doublon ni boucle), mentions additionnées, description la plus riche,
// types réunis, et nom de drop retenu comme alias de keep pour que les
// prochaines extractions retombent sur lui.
func (s *Store) Merge(ctx context.Context, keepID, dropID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.merge(ctx, keepID, dropID)
}

func (s *Store) merge(ctx context.Context, keepID, dropID int64) error {
	if keepID == dropID {
		return fmt.Errorf("fusion d'un concept avec lui-même")
	}
	type ek struct {
		src, dst int64
		typ      string
	}
	type row struct {
		id  int64
		key ek
	}
	var existing = map[ek]bool{}
	var dropEdges []row
	err := s.query(ctx, `SELECT id, source_id, target_id, type FROM edges
		WHERE source_id IN (?1, ?2) OR target_id IN (?1, ?2)`, []any{keepID, dropID}, func(st *sqlite.Stmt) error {
		k := ek{st.ColumnInt64(1), st.ColumnInt64(2), st.ColumnText(3)}
		if k.src == dropID || k.dst == dropID {
			dropEdges = append(dropEdges, row{st.ColumnInt64(0), k})
		} else {
			existing[k] = true
		}
		return nil
	})
	if err != nil {
		return err
	}

	tx, err := s.g.BeginTx(ctx)
	if err != nil {
		return err
	}
	fail := func(err error) error { tx.Rollback(); return err }

	keep, err := tx.GetNode(ctx, keepID)
	if err != nil {
		return fail(err)
	}
	drop, err := tx.GetNode(ctx, dropID)
	if err != nil {
		return fail(err)
	}

	for _, r := range dropEdges {
		k := r.key
		if k.src == dropID {
			k.src = keepID
		}
		if k.dst == dropID {
			k.dst = keepID
		}
		if k.src == k.dst || existing[k] {
			continue // supprimé avec drop
		}
		existing[k] = true
		if err := tx.CreateEdge(ctx, &graph.Edge{SourceID: k.src, TargetID: k.dst, Type: k.typ}); err != nil {
			return fail(err)
		}
	}

	props := keep.Properties
	if props == nil {
		props = map[string]any{}
	}
	dprops := drop.Properties
	props["mentions"] = numberProp(props, "mentions") + numberProp(dprops, "mentions")
	if len([]rune(firstString(dprops, "description"))) > len([]rune(firstString(props, "description"))) {
		props["description"] = dprops["description"]
	}
	if firstString(dprops, "last_seen") > firstString(props, "last_seen") {
		props["last_seen"] = dprops["last_seen"]
	}
	aliases := map[string]bool{}
	var list []string
	addAlias := func(a string) {
		a = entityKey(a)
		if a != "" && a != firstString(props, "key") && !aliases[a] && len(list) < maxAliases {
			aliases[a] = true
			list = append(list, a)
		}
	}
	for _, p := range []map[string]any{props, dprops} {
		if arr, ok := p["aliases"].([]any); ok {
			for _, v := range arr {
				if str, ok := v.(string); ok {
					addAlias(str)
				}
			}
		}
	}
	addAlias(drop.Name)
	props["aliases"] = list
	keep.Properties = props

	have := map[string]bool{}
	for _, l := range keep.Labels {
		have[l] = true
	}
	for _, l := range drop.Labels {
		if !have[l] {
			keep.Labels = append(keep.Labels, l)
		}
	}
	if err := tx.UpdateNode(ctx, keep); err != nil {
		return fail(err)
	}
	if err := tx.DeleteNode(ctx, dropID); err != nil {
		return fail(err)
	}
	return tx.Commit()
}

// findByAlias retrouve le concept dont key est un alias (nom d'un doublon
// fusionné).
func (s *Store) findByAlias(ctx context.Context, key string) (*graph.Node, error) {
	id := int64(-1)
	err := s.query(ctx, `SELECT id FROM nodes
		WHERE EXISTS (SELECT 1 FROM json_each(nodes.properties, '$.aliases') WHERE value = ?1) LIMIT 1`,
		[]any{key}, func(st *sqlite.Stmt) error { id = st.ColumnInt64(0); return nil })
	if err != nil || id < 0 {
		return nil, err
	}
	return s.g.GetNode(ctx, id)
}

// backup copie la base (cohérente, même en mode WAL) à côté d'elle et ne
// garde que les keepBackups plus récentes.
func (s *Store) backup(ctx context.Context) (string, error) {
	dest := fmt.Sprintf("%s.bak-%s", s.path, time.Now().UTC().Format("20060102-150405.000"))
	if err := s.query(ctx, `VACUUM INTO ?1`, []any{dest}, nil); err != nil {
		return "", err
	}
	old, _ := filepath.Glob(s.path + ".bak-*")
	sort.Strings(old)
	for len(old) > keepBackups {
		os.Remove(old[0])
		old = old[1:]
	}
	return dest, nil
}

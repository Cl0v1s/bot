// Package ontology maintient un graphe de connaissances (concepts et liens
// entre eux) extrait des conversations, stocké dans une base SQLite du
// workspace (go-sqlite-graph), et interrogeable par le modèle (voir
// tools.QueryOntologyTool) pendant ses phases de réflexion.
//
// Les écritures passent par l'API de go-sqlite-graph ; la lecture des arêtes
// (que la bibliothèque ne sait pas énumérer) se fait en SQL direct, sur une
// connexion en lecture seule distincte.
package ontology

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	graph "github.com/justintout/go-sqlite-graph"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	// conceptLabel : label commun à tous les nœuds, en plus de leur type
	// (Projet, Personne...) — permet de tous les retrouver via Match.
	conceptLabel = "Concept"
	defaultType  = "Concept"

	maxNameLen        = 120
	maxDescriptionLen = 300
	maxRelationLen    = 40
	maxTypeLen        = 30

	// Plafonds par extraction : un modèle qui s'emballe ne doit pas pouvoir
	// inonder la base.
	maxEntitiesPerApply  = 60
	maxRelationsPerApply = 120
)

// Entity est un concept identifié dans une conversation.
type Entity struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// Relation est un lien orienté Source --Relation--> Target entre deux
// concepts désignés par leur nom.
type Relation struct {
	Source   string `json:"source"`
	Relation string `json:"relation"`
	Target   string `json:"target"`
}

// Extraction est le résultat d'une analyse de conversation.
type Extraction struct {
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
}

// Stats résume ce qu'un Apply a réellement changé.
type Stats struct {
	NewEntities     int
	UpdatedEntities int
	NewRelations    int
}

func (s Stats) Empty() bool {
	return s.NewEntities == 0 && s.UpdatedEntities == 0 && s.NewRelations == 0
}

func (s Stats) String() string {
	return fmt.Sprintf("+%d concept(s), %d mis à jour, +%d relation(s)", s.NewEntities, s.UpdatedEntities, s.NewRelations)
}

// Store est la base de graphe. Sûr pour un usage concurrent.
type Store struct {
	path string
	g    *graph.Graph
	mu   sync.Mutex // sérialise les Apply (lecture-puis-écriture non atomique)
}

// Open ouvre (ou crée) la base à path.
func Open(path string) (*Store, error) {
	g, err := graph.Open(fileURI(path), nil)
	if err != nil {
		return nil, fmt.Errorf("ouverture de l'ontologie %s: %w", path, err)
	}
	// Open est paresseux : sans cette requête, le fichier n'existerait pas
	// encore (donc inouvrable en lecture seule) tant qu'aucune écriture n'a
	// eu lieu.
	if _, err := g.Match(conceptLabel).Count(context.Background()); err != nil {
		g.Close()
		return nil, fmt.Errorf("initialisation de l'ontologie %s: %w", path, err)
	}
	return &Store{path: path, g: g}, nil
}

func (s *Store) Close() error { return s.g.Close() }

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// query exécute une lecture SQL sur une connexion en lecture seule dédiée.
func (s *Store) query(ctx context.Context, sql string, args []any, row func(*sqlite.Stmt) error) error {
	conn, err := sqlite.OpenConn(fileURI(s.path), sqlite.OpenReadOnly|sqlite.OpenURI|sqlite.OpenWAL)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetInterrupt(ctx.Done())
	return sqlitex.Execute(conn, sql, &sqlitex.ExecOptions{Args: args, ResultFunc: row})
}

// ---------------------------------------------------------------------------
// Normalisation
// ---------------------------------------------------------------------------

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func cleanName(s string) string {
	return truncateRunes(strings.Join(strings.Fields(s), " "), maxNameLen)
}

func entityKey(name string) string { return strings.ToLower(cleanName(name)) }

// cleanType : "projet logiciel" -> "ProjetLogiciel". Vide -> "Concept".
func cleanType(s string) string {
	var b strings.Builder
	upNext := true
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if upNext {
				r = unicode.ToUpper(r)
				upNext = false
			}
			b.WriteRune(r)
		case r == '_' || unicode.IsSpace(r) || r == '-':
			upNext = true
		}
	}
	if b.Len() == 0 {
		return defaultType
	}
	return truncateRunes(b.String(), maxTypeLen)
}

// cleanRelation : "appartient à" -> "APPARTIENT_À". Vide si inexploitable.
func cleanRelation(s string) string {
	var b strings.Builder
	sep := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if sep && b.Len() > 0 {
				b.WriteByte('_')
			}
			sep = false
			b.WriteRune(unicode.ToUpper(r))
		default:
			sep = true
		}
	}
	return truncateRunes(b.String(), maxRelationLen)
}

func firstString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func numberProp(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Écriture
// ---------------------------------------------------------------------------

func (s *Store) findByKey(ctx context.Context, key string) (*graph.Node, error) {
	res, err := s.g.Match(conceptLabel).WhereJSON("key", "=", key).Limit(1).Run(ctx)
	if err != nil {
		return nil, err
	}
	if res.Len() == 0 {
		return nil, nil
	}
	return res.Nodes()[0], nil
}

// upsertEntity crée ou met à jour le nœud de name, et retourne son id.
// created=true pour une création.
func (s *Store) upsertEntity(ctx context.Context, name, typ, desc string, now string) (id int64, created bool, err error) {
	name = cleanName(name)
	key := entityKey(name)
	typ = cleanType(typ)
	desc = truncateRunes(strings.Join(strings.Fields(desc), " "), maxDescriptionLen)

	node, err := s.findByKey(ctx, key)
	if err != nil {
		return 0, false, err
	}
	if node == nil {
		n := &graph.Node{
			Name:   name,
			Labels: []string{conceptLabel, typ},
			Properties: map[string]any{
				"key":         key,
				"description": desc,
				"mentions":    1,
				"last_seen":   now,
			},
		}
		if err := s.g.CreateNode(ctx, n); err != nil {
			return 0, false, err
		}
		return n.ID, true, nil
	}

	props := node.Properties
	if props == nil {
		props = map[string]any{}
	}
	// Garde la description la plus détaillée : une mention brève ultérieure
	// ne doit pas écraser une description plus riche.
	if len([]rune(desc)) > len([]rune(firstString(props, "description"))) {
		props["description"] = desc
	}
	props["mentions"] = numberProp(props, "mentions") + 1
	props["last_seen"] = now
	node.Properties = props
	if err := s.g.UpdateNode(ctx, node); err != nil {
		return 0, false, err
	}
	if typ != defaultType {
		if err := s.g.AddLabels(ctx, node.ID, typ); err != nil {
			return 0, false, err
		}
	}
	return node.ID, false, nil
}

func (s *Store) edgeExists(ctx context.Context, src, dst int64, typ string) (bool, error) {
	found := false
	err := s.query(ctx, `SELECT 1 FROM edges WHERE source_id = ?1 AND target_id = ?2 AND type = ?3 LIMIT 1`,
		[]any{src, dst, typ}, func(*sqlite.Stmt) error { found = true; return nil })
	return found, err
}

// Apply fusionne ext dans la base : concepts retrouvés par nom (insensible à
// la casse), liens dédupliqués par (source, relation, cible). Une relation
// qui cite un concept absent de ext.Entities le crée avec le type "Concept".
func (s *Store) Apply(ctx context.Context, ext Extraction) (Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var st Stats
	now := time.Now().UTC().Format(time.RFC3339)
	ids := map[string]int64{} // clé -> id, pour ce Apply

	ensure := func(name, typ, desc string) (int64, error) {
		key := entityKey(name)
		if key == "" {
			return 0, fmt.Errorf("nom vide")
		}
		if id, ok := ids[key]; ok && typ == "" && desc == "" {
			return id, nil
		}
		id, created, err := s.upsertEntity(ctx, name, typ, desc, now)
		if err != nil {
			return 0, err
		}
		if _, seen := ids[key]; !seen {
			if created {
				st.NewEntities++
			} else {
				st.UpdatedEntities++
			}
		}
		ids[key] = id
		return id, nil
	}

	for i, e := range ext.Entities {
		if i >= maxEntitiesPerApply {
			break
		}
		if entityKey(e.Name) == "" {
			continue
		}
		if _, err := ensure(e.Name, e.Type, e.Description); err != nil {
			return st, err
		}
	}

	for i, r := range ext.Relations {
		if i >= maxRelationsPerApply {
			break
		}
		typ := cleanRelation(r.Relation)
		if typ == "" || entityKey(r.Source) == "" || entityKey(r.Target) == "" {
			continue
		}
		src, err := ensure(r.Source, "", "")
		if err != nil {
			return st, err
		}
		dst, err := ensure(r.Target, "", "")
		if err != nil {
			return st, err
		}
		if src == dst {
			continue
		}
		exists, err := s.edgeExists(ctx, src, dst, typ)
		if err != nil {
			return st, err
		}
		if exists {
			continue
		}
		if err := s.g.CreateEdge(ctx, &graph.Edge{SourceID: src, TargetID: dst, Type: typ}); err != nil {
			return st, err
		}
		st.NewRelations++
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// Lecture
// ---------------------------------------------------------------------------

type edgeRow struct {
	source, target int64
	typ            string
}

func (s *Store) edgesOf(ctx context.Context, id int64, relation string) ([]edgeRow, error) {
	var out []edgeRow
	sql := `SELECT source_id, target_id, type FROM edges WHERE (source_id = ?1 OR target_id = ?1)`
	args := []any{id}
	if relation != "" {
		sql += ` AND type = ?2`
		args = append(args, relation)
	}
	sql += ` ORDER BY type, id`
	err := s.query(ctx, sql, args, func(st *sqlite.Stmt) error {
		out = append(out, edgeRow{source: st.ColumnInt64(0), target: st.ColumnInt64(1), typ: st.ColumnText(2)})
		return nil
	})
	return out, err
}

// KnownNames retourne les noms des limit concepts les plus connectés/cités,
// pour que l'extraction réutilise les noms existants au lieu de créer des
// doublons ("Synapse" / "projet Synapse").
func (s *Store) KnownNames(ctx context.Context, limit int) ([]string, error) {
	var names []string
	err := s.query(ctx, `SELECT n.name FROM nodes n
		ORDER BY (SELECT COUNT(*) FROM edges e WHERE e.source_id = n.id OR e.target_id = n.id) DESC, n.updated_at DESC, n.id DESC
		LIMIT ?1`, []any{limit}, func(st *sqlite.Stmt) error {
		names = append(names, st.ColumnText(0))
		return nil
	})
	return names, err
}

// Counts retourne le nombre de concepts et de relations.
func (s *Store) Counts(ctx context.Context) (nodes, edges int, err error) {
	err = s.query(ctx, `SELECT (SELECT COUNT(*) FROM nodes), (SELECT COUNT(*) FROM edges)`, nil, func(st *sqlite.Stmt) error {
		nodes, edges = int(st.ColumnInt64(0)), int(st.ColumnInt64(1))
		return nil
	})
	return
}

// nodeInfo : nœud mis en cache pour l'affichage.
type nodeInfo struct {
	name   string
	labels []string
	desc   string
}

func (s *Store) node(ctx context.Context, cache map[int64]nodeInfo, id int64) (nodeInfo, error) {
	if n, ok := cache[id]; ok {
		return n, nil
	}
	gn, err := s.g.GetNode(ctx, id)
	if err != nil {
		return nodeInfo{}, err
	}
	var labels []string
	for _, l := range gn.Labels {
		if l != conceptLabel {
			labels = append(labels, l)
		}
	}
	sort.Strings(labels)
	n := nodeInfo{name: gn.Name, labels: labels, desc: firstString(gn.Properties, "description")}
	cache[id] = n
	return n, nil
}

func (n nodeInfo) title() string {
	if len(n.labels) == 0 {
		return n.name
	}
	return fmt.Sprintf("%s [%s]", n.name, strings.Join(n.labels, ", "))
}

const (
	maxQueryDepth   = 3
	maxQueryMatches = 5
	maxOutputLines  = 120
)

// Describe répond à une requête du modèle : les concepts dont le nom
// contient query (insensible à la casse), leur description et leurs liens,
// puis, jusqu'à depth sauts, ceux de leurs voisins. relation (optionnel)
// restreint les liens affichés à ce type. query vide : aperçu global (types
// de relations, concepts les plus connectés).
func (s *Store) Describe(ctx context.Context, query string, depth int, relation string) (string, error) {
	if depth < 1 {
		depth = 1
	}
	if depth > maxQueryDepth {
		depth = maxQueryDepth
	}
	relation = cleanRelation(relation)
	query = strings.TrimSpace(query)

	nodes, edges, err := s.Counts(ctx)
	if err != nil {
		return "", err
	}
	if nodes == 0 {
		return "L'ontologie est vide : aucun concept n'a encore été extrait des conversations.", nil
	}
	if query == "" {
		return s.overview(ctx, nodes, edges)
	}

	pattern := "%" + strings.NewReplacer("%", "", "_", "").Replace(strings.ToLower(cleanName(query))) + "%"
	res, err := s.g.Match(conceptLabel).WhereJSON("key", "LIKE", pattern).Limit(maxQueryMatches).Run(ctx)
	if err != nil {
		return "", err
	}
	if res.Len() == 0 {
		return fmt.Sprintf("Aucun concept ne correspond à %q (l'ontologie contient %d concepts, %d relations). Essaie un fragment plus court, ou un appel sans \"query\" pour un aperçu.", query, nodes, edges), nil
	}

	var b strings.Builder
	lines := 0
	cache := map[int64]nodeInfo{}
	visited := map[int64]bool{}
	type item struct {
		id    int64
		level int
	}
	var queue []item
	for _, n := range res.Nodes() {
		queue = append(queue, item{n.ID, 1})
		visited[n.ID] = true
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		info, err := s.node(ctx, cache, it.id)
		if err != nil {
			return "", err
		}
		prefix := strings.Repeat("  ", it.level-1)
		fmt.Fprintf(&b, "%s## %s", prefix, info.title())
		if info.desc != "" {
			fmt.Fprintf(&b, " — %s", info.desc)
		}
		b.WriteByte('\n')
		lines++
		rows, err := s.edgesOf(ctx, it.id, relation)
		if err != nil {
			return "", err
		}
		for _, e := range rows {
			if lines >= maxOutputLines {
				b.WriteString("[résultat tronqué]\n")
				return b.String(), nil
			}
			other, arrow := e.target, fmt.Sprintf("--%s-->", e.typ)
			if e.target == it.id {
				other, arrow = e.source, fmt.Sprintf("<--%s--", e.typ)
			}
			oi, err := s.node(ctx, cache, other)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "%s  %s %s\n", prefix, arrow, oi.title())
			lines++
			if it.level < depth && !visited[other] {
				visited[other] = true
				queue = append(queue, item{other, it.level + 1})
			}
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (s *Store) overview(ctx context.Context, nodes, edges int) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Ontologie : %d concepts, %d relations.\n", nodes, edges)

	var rels []string
	if err := s.query(ctx, `SELECT type, COUNT(*) FROM edges GROUP BY type ORDER BY COUNT(*) DESC, type LIMIT 30`, nil, func(st *sqlite.Stmt) error {
		rels = append(rels, fmt.Sprintf("%s (%d)", st.ColumnText(0), st.ColumnInt64(1)))
		return nil
	}); err != nil {
		return "", err
	}
	if len(rels) > 0 {
		fmt.Fprintf(&b, "Types de relations : %s\n", strings.Join(rels, ", "))
	}

	b.WriteString("Concepts les plus connectés :\n")
	cache := map[int64]nodeInfo{}
	var ids []int64
	if err := s.query(ctx, `SELECT n.id FROM nodes n
		ORDER BY (SELECT COUNT(*) FROM edges e WHERE e.source_id = n.id OR e.target_id = n.id) DESC, n.id DESC LIMIT 25`,
		nil, func(st *sqlite.Stmt) error { ids = append(ids, st.ColumnInt64(0)); return nil }); err != nil {
		return "", err
	}
	for _, id := range ids {
		info, err := s.node(ctx, cache, id)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "- %s", info.title())
		if info.desc != "" {
			fmt.Fprintf(&b, " — %s", info.desc)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

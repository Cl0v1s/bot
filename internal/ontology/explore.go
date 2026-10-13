package ontology

import (
	"context"
	"fmt"
	"html"
	"math"
	"sort"
	"strings"
)

const (
	// DefaultExploreNodes : plafond de concepts dessinés (au-delà, le SVG
	// devient illisible) ; le plus connectés d'abord, ou par distance à la
	// requête.
	DefaultExploreNodes = 80
	maxExploreDepth     = 4
)

// ExploreNode et ExploreEdge : sous-graphe à dessiner.
type ExploreNode struct {
	Concept
	Degree int
	Seed   bool // correspond à la requête
}

type ExploreEdge struct {
	Src, Dst int // index dans Nodes
	Relation string
}

type Subgraph struct {
	Nodes     []ExploreNode
	Edges     []ExploreEdge
	Truncated bool // des concepts ont été omis (plafond maxNodes)
}

// Explore extrait le sous-graphe autour des concepts dont le nom contient
// query (jusqu'à depth sauts), ou, query vide, les maxNodes concepts les plus
// connectés. Lecture seule.
func (s *Store) Explore(ctx context.Context, query string, depth, maxNodes int) (Subgraph, error) {
	if depth < 1 {
		depth = 1
	}
	if depth > maxExploreDepth {
		depth = maxExploreDepth
	}
	if maxNodes < 1 {
		maxNodes = DefaultExploreNodes
	}
	snap, err := s.load(ctx)
	if err != nil {
		return Subgraph{}, err
	}
	adj := map[int64][]int64{}
	for _, e := range snap.edges {
		adj[e.src] = append(adj[e.src], e.dst)
		adj[e.dst] = append(adj[e.dst], e.src)
	}

	var chosen []int64
	picked := map[int64]bool{}
	seeds := map[int64]bool{}
	truncated := false
	add := func(id int64) bool {
		if picked[id] {
			return true
		}
		if len(chosen) >= maxNodes {
			truncated = true
			return false
		}
		picked[id] = true
		chosen = append(chosen, id)
		return true
	}

	q := strings.ToLower(cleanName(query))
	if q == "" {
		order := append([]*snapNode(nil), snap.nodes...)
		sort.SliceStable(order, func(i, j int) bool {
			if order[i].degree != order[j].degree {
				return order[i].degree > order[j].degree
			}
			return order[i].mentions > order[j].mentions
		})
		for _, n := range order {
			if n.degree == 0 {
				break // les isolés n'apportent rien au dessin
			}
			if !add(n.ID) {
				break
			}
		}
		if len(order) > len(chosen) {
			truncated = true
		}
	} else {
		var frontier []int64
		for _, n := range snap.nodes {
			if strings.Contains(strings.ToLower(n.Name), q) {
				seeds[n.ID] = true
				if add(n.ID) {
					frontier = append(frontier, n.ID)
				}
			}
		}
		for level := 1; level <= depth && len(frontier) > 0; level++ {
			var next []int64
			for _, id := range frontier {
				for _, o := range adj[id] {
					if !picked[o] && add(o) {
						next = append(next, o)
					}
				}
			}
			frontier = next
		}
	}

	var sg Subgraph
	sg.Truncated = truncated
	index := map[int64]int{}
	for _, id := range chosen {
		n := snap.byID[id]
		index[id] = len(sg.Nodes)
		sg.Nodes = append(sg.Nodes, ExploreNode{Concept: n.Concept, Degree: n.degree, Seed: seeds[id]})
	}
	for _, e := range snap.edges {
		a, okA := index[e.src]
		b, okB := index[e.dst]
		if okA && okB && a != b {
			sg.Edges = append(sg.Edges, ExploreEdge{Src: a, Dst: b, Relation: e.typ})
		}
	}
	return sg, nil
}

// ---------------------------------------------------------------------------
// Mise en page (Fruchterman-Reingold, déterministe) et rendu SVG
// ---------------------------------------------------------------------------

const (
	minW, minH = 1600.0, 1100.0
	margin     = 90.0

	// Zone réservée à la légende (colonne de gauche) et au titre (bandeau du
	// haut) : le graphe est placé dans le reste, pour qu'aucun concept
	// ni libellé ne passe dessous.
	legendW  = 240.0
	titleH   = 50.0
	legendMx = 36 // types affichés dans la légende, au-delà "… +N autres"

	spacing = 130.0 // distance idéale entre deux concepts voisins
)

type vec struct{ x, y float64 }

// layout place les concepts librement (aucun bord) puis calcule la taille du
// canevas pour les contenir tous : plus il y a de concepts, plus il est grand.
func layout(sg Subgraph) (pos []vec, w, h float64) {
	n := len(sg.Nodes)
	pos = make([]vec, n)
	if n == 0 {
		return pos, minW, minH
	}
	side := math.Max(spacing*2, spacing*math.Sqrt(float64(n))*1.2)
	midX, midY := 0.0, 0.0
	// Départ sur un cercle, déterministe.
	for i := range pos {
		a := 2 * math.Pi * float64(i) / float64(n)
		pos[i] = vec{math.Cos(a) * side / 2, math.Sin(a) * side / 2}
	}
	if n == 1 {
		return fit(pos)
	}
	// Gravité plus forte pour les petits îlots, sinon la répulsion des autres
	// les rejette tous contre les bords du cadre.
	gravity := make([]float64, n)
	for i, size := range componentSizes(sg) {
		gravity[i] = 0.3
		if size <= 6 {
			gravity[i] = 1.2
		}
	}
	k := spacing
	temp := side / 8
	for it := 0; it < 300; it++ {
		disp := make([]vec, n)
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				dx, dy := pos[i].x-pos[j].x, pos[i].y-pos[j].y
				d := math.Max(math.Hypot(dx, dy), 0.01)
				f := k * k / d
				disp[i].x += dx / d * f
				disp[i].y += dy / d * f
				disp[j].x -= dx / d * f
				disp[j].y -= dy / d * f
			}
		}
		for _, e := range sg.Edges {
			dx, dy := pos[e.Src].x-pos[e.Dst].x, pos[e.Src].y-pos[e.Dst].y
			d := math.Max(math.Hypot(dx, dy), 0.01)
			f := d * d / k
			disp[e.Src].x -= dx / d * f
			disp[e.Src].y -= dy / d * f
			disp[e.Dst].x += dx / d * f
			disp[e.Dst].y += dy / d * f
		}
		for i := range pos {
			// Légère gravité vers le centre : garde les composantes
			// déconnectées dans le cadre.
			disp[i].x += (midX - pos[i].x) * gravity[i]
			disp[i].y += (midY - pos[i].y) * gravity[i]
			d := math.Max(math.Hypot(disp[i].x, disp[i].y), 0.01)
			step := math.Min(d, temp)
			pos[i].x += disp[i].x / d * step
			pos[i].y += disp[i].y / d * step
		}
		temp *= 0.985
	}
	return fit(pos)
}

// fit translate pos pour que la boîte englobante soit centrée dans la zone
// du graphe, et retourne la taille du canevas (au moins minW x minH).
func fit(pos []vec) ([]vec, float64, float64) {
	x0, x1, y0, y1 := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for _, p := range pos {
		x0, x1 = math.Min(x0, p.x), math.Max(x1, p.x)
		y0, y1 = math.Min(y0, p.y), math.Max(y1, p.y)
	}
	w := math.Max(minW, legendW+(x1-x0)+2*margin)
	h := math.Max(minH, titleH+(y1-y0)+2*margin)
	offX := legendW + (w-legendW-(x1-x0))/2 - x0
	offY := titleH + (h-titleH-(y1-y0))/2 - y0
	for i := range pos {
		pos[i].x += offX
		pos[i].y += offY
	}
	return pos, w, h
}

// componentSizes retourne, pour chaque nœud, la taille de sa composante
// connexe.
func componentSizes(sg Subgraph) []int {
	parent := make([]int, len(sg.Nodes))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for _, e := range sg.Edges {
		parent[find(e.Src)] = find(e.Dst)
	}
	size := map[int]int{}
	for i := range parent {
		size[find(i)]++
	}
	out := make([]int, len(parent))
	for i := range out {
		out[i] = size[find(i)]
	}
	return out
}

var typePalette = []string{"#4e79a7", "#f28e2b", "#59a14f", "#e15759", "#b07aa1", "#76b7b2", "#edc948", "#9c755f", "#ff9da7", "#86bc86"}

func radius(degree int) float64 { return 7 + 3*math.Sqrt(float64(degree)) }

// RenderSVG dessine sg : nœuds colorés par type, taille selon le nombre de
// liens, relations en étiquettes sur les flèches, légende des types.
func RenderSVG(sg Subgraph, title string) []byte {
	pos, svgW, svgH := layout(sg)

	// Types par fréquence décroissante -> couleurs.
	counts := map[string]int{}
	for _, n := range sg.Nodes {
		counts[n.Type]++
	}
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool {
		if counts[types[i]] != counts[types[j]] {
			return counts[types[i]] > counts[types[j]]
		}
		return types[i] < types[j]
	})
	color := map[string]string{}
	for i, t := range types {
		color[t] = typePalette[i%len(typePalette)]
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" font-family="sans-serif">`+"\n", svgW, svgH, svgW, svgH)
	b.WriteString(`<defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="#8a8a8a"/></marker></defs>` + "\n")
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" fill="#ffffff"/>`+"\n")
	suffix := ""
	if sg.Truncated {
		suffix = " (tronqué)"
	}
	fmt.Fprintf(&b, `<text x="20" y="30" font-size="18" font-weight="bold" fill="#222">%s — %d concepts, %d relations%s</text>`+"\n",
		html.EscapeString(title), len(sg.Nodes), len(sg.Edges), suffix)

	// Liens : les paires déjà reliées sont courbées pour ne pas se superposer.
	pairSeen := map[[2]int]int{}
	for _, e := range sg.Edges {
		a, c := e.Src, e.Dst
		key := [2]int{min(a, c), max(a, c)}
		slot := pairSeen[key]
		pairSeen[key]++
		x1, y1, x2, y2 := pos[a].x, pos[a].y, pos[c].x, pos[c].y
		dx, dy := x2-x1, y2-y1
		d := math.Max(math.Hypot(dx, dy), 0.01)
		ux, uy := dx/d, dy/d
		// Départ/arrivée au bord des disques.
		sx, sy := x1+ux*radius(sg.Nodes[a].Degree), y1+uy*radius(sg.Nodes[a].Degree)
		ex, ey := x2-ux*(radius(sg.Nodes[c].Degree)+3), y2-uy*(radius(sg.Nodes[c].Degree)+3)
		off := 0.0
		if slot > 0 {
			off = float64((slot+1)/2) * 22
			if slot%2 == 0 {
				off = -off
			}
			if a > c {
				off = -off
			}
		}
		mx, my := (sx+ex)/2-uy*off, (sy+ey)/2+ux*off
		fmt.Fprintf(&b, `<path d="M%.1f,%.1f Q%.1f,%.1f %.1f,%.1f" fill="none" stroke="#b5b5b5" stroke-width="1.2" marker-end="url(#arrow)"><title>%s → %s : %s</title></path>`+"\n",
			sx, sy, mx, my, ex, ey,
			html.EscapeString(sg.Nodes[a].Name), html.EscapeString(sg.Nodes[c].Name), html.EscapeString(e.Relation))
		lx, ly := (sx+ex)/4+mx/2, (sy+ey)/4+my/2
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="9" fill="#777" text-anchor="middle" paint-order="stroke" stroke="#fff" stroke-width="3">%s</text>`+"\n",
			lx, ly, html.EscapeString(strings.ToLower(strings.ReplaceAll(e.Relation, "_", " "))))
	}

	for i, n := range sg.Nodes {
		r := radius(n.Degree)
		stroke, sw := "#ffffff", 1.5
		if n.Seed {
			stroke, sw = "#222222", 3
		}
		tip := n.Name + " [" + n.Type + "]"
		if n.Description != "" {
			tip += "\n" + n.Description
		}
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="%s" stroke="%s" stroke-width="%.1f"><title>%s</title></circle>`+"\n",
			pos[i].x, pos[i].y, r, color[n.Type], stroke, sw, html.EscapeString(tip))
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="12" fill="#222" text-anchor="middle" paint-order="stroke" stroke="#fff" stroke-width="3.5">%s</text>`+"\n",
			pos[i].x, pos[i].y+r+13, html.EscapeString(truncateRunes(n.Name, 28)))
	}

	// Légende : colonne de gauche réservée, types tronqués et plafonnés.
	fmt.Fprintf(&b, `<rect x="10" y="%.0f" width="%.0f" height="%.0f" fill="#f6f6f6" stroke="#e2e2e2" rx="6"/>`+"\n", titleH, legendW-20, svgH-titleH-10)
	y := titleH + 24
	for i, t := range types {
		if i >= legendMx {
			fmt.Fprintf(&b, `<text x="42" y="%.0f" font-size="12" fill="#777">… +%d autres types</text>`+"\n", y+4, len(types)-legendMx)
			break
		}
		fmt.Fprintf(&b, `<circle cx="28" cy="%.0f" r="6" fill="%s"/><text x="42" y="%.0f" font-size="12" fill="#222">%s (%d)</text>`+"\n",
			y, color[t], y+4, html.EscapeString(truncateRunes(t, 22)), counts[t])
		y += 20
	}
	b.WriteString("</svg>\n")
	return []byte(b.String())
}

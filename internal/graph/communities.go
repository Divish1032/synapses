package graph

import (
	"math"
	"math/rand"
	"sort"
)

// CommunityNode is a minimal node record included in community output.
type CommunityNode struct {
	ID      NodeID   `json:"id"`
	Name    string   `json:"name"`
	Type    NodeType `json:"type"`
	Package string   `json:"package"`
	File    string   `json:"file"`
}

// Community is a cluster of strongly-connected nodes detected by LPA.
type Community struct {
	// ID is a 0-based index, assigned after sorting by size desc.
	ID int `json:"id"`
	// Size is the number of member nodes.
	Size int `json:"size"`
	// DominantPackage is the most common package among the community's nodes.
	DominantPackage string `json:"dominant_package"`
	// PackageMix lists all distinct packages present (sorted). Length > 1 → IsMixed.
	PackageMix []string `json:"package_mix"`
	// IsMixed is true when the community spans more than one package.
	// Mixed communities often indicate hidden coupling across module boundaries.
	IsMixed bool `json:"is_mixed"`
	// Nodes are the member nodes, sorted by file then name.
	Nodes []CommunityNode `json:"nodes"`
}

// CommunityResult is returned by DetectCommunities.
type CommunityResult struct {
	Algorithm string `json:"algorithm"`
	// Modularity is the Newman-Girvan modularity score (0–1).
	// Higher values indicate stronger community separation.
	Modularity     float64 `json:"modularity"`
	IterationCount int     `json:"iteration_count"`
	CommunityCount int     `json:"community_count"`
	// Communities are sorted by size descending.
	Communities []Community `json:"communities"`
}

type communityNeighbor struct {
	idx    int     // index into the nodes slice
	weight float64 // edge weight
}

// DetectCommunities runs Label Propagation Algorithm (LPA) on the graph and
// returns the discovered community structure. File and package nodes are
// excluded; DEFINES edges are excluded (too low-weight to be meaningful).
//
// maxIterations controls convergence (default 10).
// minSize filters out communities smaller than this (default 2, hides singletons).
func (g *Graph) DetectCommunities(maxIterations, minSize int) (*CommunityResult, error) {
	if maxIterations <= 0 {
		maxIterations = 10
	}
	if minSize <= 0 {
		minSize = 2
	}

	// Collect eligible nodes — exclude file and package hub-nodes.
	allNodes := g.AllNodes()
	nodes := make([]*Node, 0, len(allNodes))
	for _, n := range allNodes {
		if n.Type == NodeFile || n.Type == NodePackage {
			continue
		}
		nodes = append(nodes, n)
	}

	if len(nodes) == 0 {
		return &CommunityResult{
			Algorithm:   "label_propagation",
			Communities: []Community{},
		}, nil
	}

	// Build a dense index: NodeID → position in nodes slice.
	nodeIndex := make(map[NodeID]int, len(nodes))
	for i, n := range nodes {
		nodeIndex[n.ID] = i
	}

	// Build an undirected weighted adjacency list.
	// We treat CALLS/IMPLEMENTS/EMBEDS/DEPENDS_ON/IMPORTS/EXPORTS as bidirectional;
	// DEFINES is excluded because it creates file-hub noise.
	adj := make([][]communityNeighbor, len(nodes))
	allEdges := g.AllEdges()
	for _, e := range allEdges {
		if e.Type == EdgeDefines {
			continue
		}
		w, ok := DefaultEdgeWeights[e.Type]
		if !ok {
			w = 0.5
		}
		fi, okF := nodeIndex[e.From]
		ti, okT := nodeIndex[e.To]
		if !okF || !okT {
			continue
		}
		adj[fi] = append(adj[fi], communityNeighbor{ti, w})
		adj[ti] = append(adj[ti], communityNeighbor{fi, w}) // undirected
	}

	// Initialise labels: each node is its own community.
	labels := make([]int, len(nodes))
	for i := range labels {
		labels[i] = i
	}

	// Shuffle order for each iteration — required for LPA correctness.
	rng := rand.New(rand.NewSource(42))
	order := make([]int, len(nodes))
	for i := range order {
		order[i] = i
	}

	iterCount := 0
	for iter := 0; iter < maxIterations; iter++ {
		iterCount++
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

		changed := 0
		for _, idx := range order {
			if len(adj[idx]) == 0 {
				continue // isolated node; keep its own label
			}

			// Weighted vote: accumulate weight per label across all neighbors.
			votes := make(map[int]float64, len(adj[idx]))
			for _, nb := range adj[idx] {
				votes[labels[nb.idx]] += nb.weight
			}

			// Choose label with highest total weight; break ties by lower label value
			// for determinism (same graph, same seed → same communities).
			best := labels[idx]
			bestScore := -1.0
			for label, score := range votes {
				if score > bestScore || (score == bestScore && label < best) {
					best = label
					bestScore = score
				}
			}

			if best != labels[idx] {
				labels[idx] = best
				changed++
			}
		}

		if changed == 0 {
			break // converged
		}
	}

	// Group node indices by final label.
	labelToIndices := make(map[int][]int, len(nodes)/4)
	for i, lbl := range labels {
		labelToIndices[lbl] = append(labelToIndices[lbl], i)
	}

	// Build Community structs; filter by minSize.
	communities := make([]Community, 0, len(labelToIndices))
	for _, indices := range labelToIndices {
		if len(indices) < minSize {
			continue
		}

		cNodes := make([]CommunityNode, 0, len(indices))
		pkgCount := make(map[string]int)
		for _, idx := range indices {
			n := nodes[idx]
			cNodes = append(cNodes, CommunityNode{
				ID:      n.ID,
				Name:    n.Name,
				Type:    n.Type,
				Package: n.Package,
				File:    n.File,
			})
			if n.Package != "" {
				pkgCount[n.Package]++
			}
		}

		// Dominant package: most frequent; tie-break by alphabetical order.
		dominant := ""
		maxCount := 0
		for pkg, cnt := range pkgCount {
			if cnt > maxCount || (cnt == maxCount && pkg < dominant) {
				dominant = pkg
				maxCount = cnt
			}
		}

		// Package mix: sorted list of distinct packages.
		pkgMix := make([]string, 0, len(pkgCount))
		for pkg := range pkgCount {
			pkgMix = append(pkgMix, pkg)
		}
		sort.Strings(pkgMix)

		// Sort nodes for deterministic output.
		sort.Slice(cNodes, func(i, j int) bool {
			if cNodes[i].File != cNodes[j].File {
				return cNodes[i].File < cNodes[j].File
			}
			return cNodes[i].Name < cNodes[j].Name
		})

		communities = append(communities, Community{
			Size:            len(cNodes),
			DominantPackage: dominant,
			PackageMix:      pkgMix,
			IsMixed:         len(pkgMix) > 1,
			Nodes:           cNodes,
		})
	}

	// Sort by size descending; assign sequential IDs.
	sort.Slice(communities, func(i, j int) bool {
		return communities[i].Size > communities[j].Size
	})
	for i := range communities {
		communities[i].ID = i
	}

	modularity := computeModularity(nodes, allEdges, nodeIndex, labels)

	return &CommunityResult{
		Algorithm:      "label_propagation",
		Modularity:     math.Round(modularity*1000) / 1000,
		IterationCount: iterCount,
		CommunityCount: len(communities),
		Communities:    communities,
	}, nil
}

// computeModularity computes the Newman-Girvan modularity score Q.
//
// Q = Σ_c [ L_c/m  −  (d_c / 2m)² ]
//
// where L_c = total weighted internal edge count for community c,
// d_c = total weighted degree of all nodes in c, m = total edge weight.
// Returns values in roughly [−0.5, 1.0]; values above 0.3 indicate
// meaningful community structure.
func computeModularity(nodes []*Node, edges []*Edge, nodeIndex map[NodeID]int, labels []int) float64 {
	if len(edges) == 0 || len(nodes) == 0 {
		return 0
	}

	// Compute total weight m and per-node weighted degree.
	m := 0.0
	degree := make([]float64, len(nodes))
	for _, e := range edges {
		if e.Type == EdgeDefines {
			continue
		}
		w := DefaultEdgeWeights[e.Type]
		fi, okF := nodeIndex[e.From]
		ti, okT := nodeIndex[e.To]
		if !okF || !okT {
			continue
		}
		m += w
		degree[fi] += w
		degree[ti] += w // undirected
	}

	if m == 0 {
		return 0
	}

	// Per-community aggregates.
	type commStats struct {
		internal float64 // sum of edge weights internal to the community
		degree   float64 // sum of node degrees in the community
	}
	stats := make(map[int]*commStats, len(nodes)/4)
	for i, lbl := range labels {
		if stats[lbl] == nil {
			stats[lbl] = &commStats{}
		}
		stats[lbl].degree += degree[i]
	}

	// Accumulate internal edge weights.
	for _, e := range edges {
		if e.Type == EdgeDefines {
			continue
		}
		w := DefaultEdgeWeights[e.Type]
		fi, okF := nodeIndex[e.From]
		ti, okT := nodeIndex[e.To]
		if !okF || !okT {
			continue
		}
		if labels[fi] == labels[ti] {
			stats[labels[fi]].internal += w
		}
	}

	twoM := 2 * m
	Q := 0.0
	for _, s := range stats {
		Q += s.internal/m - (s.degree/twoM)*(s.degree/twoM)
	}
	return Q
}

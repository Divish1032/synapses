package graph

import (
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// provenanceWeight returns a relevance multiplier based on node provenance.
// user-authored nodes are not penalized; generated/vendored/external nodes
// are deprioritized so they rank below user code when the token budget is applied.
// The root node is never penalized (checked by the caller).
func provenanceWeight(p ProvenanceType) float64 {
	switch p {
	case ProvenanceGenerated:
		return 0.5
	case ProvenanceVendored:
		return 0.3
	case ProvenanceExternal:
		return 0.2
	default: // ProvenanceUserAuthored or empty — no penalty
		return 1.0
	}
}

// outInEdges returns combined outgoing and incoming edges for id.
// When the columnar index is ready it reads directly from CSR flat arrays,
// avoiding map lookups and pointer chasing. Tombstoned edge endpoints are
// silently filtered. Falls back to the pointer-map when the index is not ready.
// The returned slice is always a fresh allocation — callers must not modify it.
func (g *Graph) outInEdges(id NodeID, idx *GraphIndex) []*Edge {
	if idx != nil && idx.Ready() {
		// Use idx.Seq() rather than reading idx.IDToSeq directly: Seq() acquires
		// idx.mu.RLock() which synchronises with the idx.mu.Lock() held by
		// MarkTombstone.  A direct map read without the lock is flagged as a data
		// race by the race detector even though IDToSeq is immutable after ready=1.
		seq := idx.Seq(id)
		if seq == 0 {
			return nil
		}
		outSeqs, outTypes := idx.OutNeighbours(seq)
		inSeqs, inTypes := idx.InNeighbours(seq)
		edges := make([]*Edge, 0, len(outSeqs)+len(inSeqs))
		for i, tSeq := range outSeqs {
			if idx.IsTombstoned(tSeq) {
				continue
			}
			edges = append(edges, &Edge{
				From: id,
				To:   idx.SeqIDs[tSeq],
				Type: EdgeType(idx.Pool.Value(outTypes[i])),
			})
		}
		for i, fSeq := range inSeqs {
			if idx.IsTombstoned(fSeq) {
				continue
			}
			edges = append(edges, &Edge{
				From: idx.SeqIDs[fSeq],
				To:   id,
				Type: EdgeType(idx.Pool.Value(inTypes[i])),
			})
		}
		return edges
	}
	// Fallback: pointer-map path.
	var all []*Edge
	all = append(all, g.outEdges[id]...)
	all = append(all, g.inEdges[id]...)
	return all
}

// CarveEgoGraph extracts a relevance-ranked subgraph centred on the given root node.
//
// Algorithm:
//  1. BFS outward from root, up to cfg.MaxDepth hops.
//  2. Each node is assigned a relevance score:
//     relevance = edgeTypeWeight(edge) × (cfg.DecayFactor ^ hopCount)
//  3. When a node is reachable via multiple paths the maximum score is kept.
//  4. If the estimated token cost exceeds cfg.TokenBudget, the lowest-scored
//     nodes are pruned (highest-hop, lowest-weight first).
//  5. Only edges where both endpoints survived pruning are included.
func (g *Graph) CarveEgoGraph(rootID NodeID, cfg CarveConfig) (*SubGraph, error) {
	// Acquire the read lock first so we can compute the structural fingerprint
	// of the root node.  The fingerprint is included in the cache key, which
	// means a comment-only edit (same signature, same edges) produces the same
	// fingerprint → same key → cache hit, with no explicit invalidation needed.
	// A structural change (new signature, edge added/removed) produces a
	// different fingerprint → new key → automatic cache miss → BFS recomputes.
	g.mu.RLock()
	defer g.mu.RUnlock()

	if _, ok := g.nodes[rootID]; !ok {
		return nil, ErrNodeNotFound(rootID)
	}

	fp := g.nodeFingerprintLocked(rootID)
	if sub, ok := g.cache.get(rootID, cfg, fp); ok {
		return sub, nil
	}

	// Grab index once under the read lock; nil if not yet built.
	idx := g.index

	weights := cfg.EdgeWeights
	if weights == nil {
		weights = DefaultEdgeWeights
	}
	decay := cfg.DecayFactor
	if decay <= 0 || decay > 1 {
		decay = 0.5
	}

	// BFS state.
	type qItem struct {
		id  NodeID
		hop int
	}

	visited := make(map[NodeID]float64) // nodeID → best relevance seen
	visited[rootID] = 1.0

	queue := []qItem{{rootID, 0}}

	// Struct/interface nodes have no CALLS edges — only DEFINES from their file.
	// Seed BFS with the struct's methods so the carve includes method-level context.
	if rootNode := g.nodes[rootID]; rootNode != nil &&
		(rootNode.Type == NodeStruct || rootNode.Type == NodeInterface) {
		prefix := rootNode.Name + "."
		for _, n := range g.nodes {
			if n.Type == NodeMethod && strings.HasPrefix(n.Name, prefix) {
				visited[n.ID] = 0.9 // slightly below root
				queue = append(queue, qItem{n.ID, 0})
			}
		}
	}

	var edgesInSubgraph []*Edge

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		if curr.hop >= cfg.MaxDepth {
			continue
		}

		// Traverse both outgoing and incoming edges so the carve captures
		// "what does this call" AND "what calls this".
		// When the columnar index is ready, reads from CSR arrays (cache-friendly,
		// skips tombstoned nodes). Falls back to pointer-map when not ready.
		allEdges := g.outInEdges(curr.id, idx)

		for _, e := range allEdges {
			typeWeight := edgeWeight(e.Type, weights)

			// R1: HANDLES edges are heuristically inferred (not AST-proven).
			// Scale their weight by the route node's confidence score so that
			// a 0.85-confidence inferred route ranks below a structural CALLS
			// edge. The confidence is stored in the route node's metadata.
			if e.Type == EdgeHandles {
				if routeNode := g.nodes[e.From]; routeNode != nil {
					if conf, err := strconv.ParseFloat(routeNode.Metadata["confidence"], 64); err == nil && conf > 0 {
						typeWeight *= conf
					}
				}
			}

			relevance := typeWeight * math.Pow(decay, float64(curr.hop+1))

			neighbor := e.To
			if e.To == curr.id {
				neighbor = e.From
			}

			// Directional CALLS boost — intent-aware:
			//   Positive DirectionBoost: forward edges (curr→neighbor) boosted
			//     → pruner prefers callees (what this calls). Used by "modify".
			//   Negative DirectionBoost: backward edges (neighbor→curr) boosted
			//     → pruner prefers callers (what calls this). Used by "debug".
			if cfg.DirectionBoost != 0 && e.Type == EdgeCalls {
				if cfg.DirectionBoost > 0 && e.From == curr.id {
					relevance *= (1.0 + cfg.DirectionBoost)
				} else if cfg.DirectionBoost < 0 && e.To == curr.id {
					relevance *= (1.0 - cfg.DirectionBoost) // double-neg: 1+|boost|
				}
			}

			if prev, seen := visited[neighbor]; !seen || relevance > prev {
				visited[neighbor] = relevance
				if curr.hop+1 < cfg.MaxDepth {
					queue = append(queue, qItem{neighbor, curr.hop + 1})
				}
			}

			// Collect the edge if both endpoints are in our visited set
			// (we will filter again after budget pruning).
			edgesInSubgraph = append(edgesInSubgraph, e)
		}
	}

	// Build scored node list, applying MinRelevance and ExcludeTypes filters.
	// Excluded-type nodes are still BFS-traversed above (so their edges are
	// discovered) but they are never emitted in the output.
	type scoredNode struct {
		id        NodeID
		relevance float64
		hop       int
	}
	var scored []scoredNode
	for id, rel := range visited {
		// Drop nodes below the minimum relevance threshold (kills sibling explosion
		// from file-hub nodes and low-signal package imports).
		if cfg.MinRelevance > 0 && rel < cfg.MinRelevance && id != rootID {
			continue
		}
		// Drop excluded node types from output (still traversed in BFS above).
		if n, ok := g.nodes[id]; ok && cfg.ExcludeTypes[n.Type] {
			continue
		}
		// Drop test-file nodes when requested (still traversed for edge discovery).
		if cfg.ExcludeTestFiles {
			if n, ok := g.nodes[id]; ok && strings.HasSuffix(n.File, "_test.go") {
				continue
			}
		}
		hop := hopDistance(id, rootID, rel, decay)
		scored = append(scored, scoredNode{id, rel, hop})
	}

	// R28: Apply provenance multiplier — deprioritize generated/vendored/external
	// nodes relative to user-authored code. The root node is never penalized so
	// the agent always gets the entity it asked for regardless of its provenance.
	for i := range scored {
		if scored[i].id == rootID {
			continue
		}
		if n, ok := g.nodes[scored[i].id]; ok {
			scored[i].relevance *= provenanceWeight(n.Provenance)
		}
	}

	// Sort by relevance descending so we keep the most important nodes first
	// when applying the token budget.
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].relevance != scored[j].relevance {
			return scored[i].relevance > scored[j].relevance
		}
		return scored[i].hop < scored[j].hop
	})

	// Apply token budget using per-node byte-based estimation (1 token ≈ 4 bytes).
	// Always keep at least the first (highest-relevance) node.
	truncated := false
	truncatedCount := 0
	if cfg.TokenBudget > 0 && len(scored) > 1 {
		var usedTokens int
		cutoff := len(scored)
		for i, s := range scored {
			n := g.nodes[s.id]
			if n == nil {
				continue
			}
			cost := estimateNodeTokens(n)
			if usedTokens+cost > cfg.TokenBudget && i > 0 {
				cutoff = i
				break
			}
			usedTokens += cost
		}
		if cutoff < len(scored) {
			truncated = true
			truncatedCount = len(scored) - cutoff
		}
		scored = scored[:cutoff]
	}

	// Build the final node set.
	keep := make(map[NodeID]struct{}, len(scored))
	for _, s := range scored {
		keep[s.id] = struct{}{}
	}

	// Assemble output nodes.
	outNodes := make([]CarvedNode, 0, len(scored))
	for _, s := range scored {
		n := g.nodes[s.id]
		if n == nil {
			continue
		}
		outNodes = append(outNodes, CarvedNode{
			Node:      n,
			Relevance: s.relevance,
			Hop:       s.hop,
		})
	}

	// Include only edges where both endpoints survived the budget cut.
	seen := make(map[[2]NodeID]struct{})
	var outEdges []*Edge
	for _, e := range edgesInSubgraph {
		_, fromOK := keep[e.From]
		_, toOK := keep[e.To]
		if !fromOK || !toOK {
			continue
		}
		key := [2]NodeID{e.From, e.To}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		outEdges = append(outEdges, e)
	}

	result := &SubGraph{
		Root:           rootID,
		Nodes:          outNodes,
		Edges:          outEdges,
		Truncated:      truncated,
		TruncatedCount: truncatedCount,
	}
	g.cache.put(rootID, cfg, fp, result)
	return result, nil
}

// edgeWeight returns the configured weight for an edge type, falling back to 0.5.
func edgeWeight(et EdgeType, weights map[EdgeType]float64) float64 {
	if w, ok := weights[et]; ok {
		return w
	}
	return 0.5
}

// hopDistance estimates the hop count from relevance and decay.
// relevance = weight × decay^hop → hop ≈ log(relevance/weight) / log(decay)
// This is approximate; it is only used for display, not for pruning decisions.
func hopDistance(id, root NodeID, relevance, decay float64) int {
	if id == root {
		return 0
	}
	if decay <= 0 || decay >= 1 || relevance <= 0 {
		return 1
	}
	// Assume max edge weight of 1.0 for simplicity.
	h := math.Log(relevance) / math.Log(decay)
	if h < 0 {
		h = -h
	}
	return int(math.Round(h))
}

// estimateNodeTokens estimates the token cost of serialising a node (1 token ≈ 4 bytes).
// It sums the byte lengths of all string fields and metadata key/value pairs.
func estimateNodeTokens(n *Node) int {
	b := len(n.ID) + len(string(n.Type)) + len(n.Name) + len(n.Package) + len(n.File)
	for k, v := range n.Metadata {
		b += len(k) + len(v)
	}
	return b/4 + 1
}

// maxImpactNodesPerTier caps the number of nodes returned per tier to avoid
// overwhelming the LLM context window. When a tier exceeds this limit, the
// slice is truncated and ImpactTier.Truncated is set true.
const maxImpactNodesPerTier = 50

// ImpactAnalysis performs a reverse BFS from rootID following incoming CALLS
// and IMPLEMENTS edges to find all nodes that could be affected if rootID changes.
// Results are grouped into depth tiers: direct (depth 1), indirect (depth 2),
// peripheral (depth 3+). maxDepth caps the traversal (0 uses default of 3).
func (g *Graph) ImpactAnalysis(rootID NodeID, maxDepth int) (*ImpactResult, error) {
	if maxDepth <= 0 {
		maxDepth = 3
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	root, ok := g.nodes[rootID]
	if !ok {
		return nil, ErrNodeNotFound(rootID)
	}

	// Reverse BFS: at each hop follow edges that CALL INTO or IMPLEMENT rootID.
	type entry struct {
		id    NodeID
		depth int
	}

	visited := map[NodeID]int{rootID: 0} // node → first-seen depth
	queue := []entry{{rootID, 0}}
	fileSet := map[string]struct{}{}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if cur.depth >= maxDepth {
			continue
		}

		nextDepth := cur.depth + 1
		// inEdges gives us nodes that have an edge pointing TO cur.id
		for _, e := range g.inEdges[cur.id] {
			if e.Type != EdgeCalls && e.Type != EdgeImplements {
				continue
			}
			if _, seen := visited[e.From]; seen {
				continue
			}
			visited[e.From] = nextDepth
			queue = append(queue, entry{e.From, nextDepth})
		}
	}

	// Build tiers.
	tierNodes := map[int][]EntityRef{}
	for id, depth := range visited {
		if depth == 0 {
			continue // skip root itself
		}
		n := g.nodes[id]
		if n == nil {
			continue
		}
		tierNodes[depth] = append(tierNodes[depth], EntityRef{
			ID:   id,
			Name: n.Name,
			Type: n.Type,
			File: n.File,
			Line: n.Line,
		})
		if n.File != "" {
			fileSet[n.File] = struct{}{}
		}
	}

	// Sort nodes within each tier by name for stable output.
	for d := range tierNodes {
		nodes := tierNodes[d]
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
		tierNodes[d] = nodes
	}

	tierLabel := func(depth int) (string, float64) {
		switch depth {
		case 1:
			return "direct", 1.0
		case 2:
			return "indirect", 0.6
		default:
			return "peripheral", 0.3
		}
	}

	var tiers []ImpactTier
	total := 0
	for d := 1; d <= maxDepth; d++ {
		nodes, ok := tierNodes[d]
		if !ok || len(nodes) == 0 {
			continue
		}
		label, conf := tierLabel(d)
		tier := ImpactTier{
			Depth:      d,
			Label:      label,
			Confidence: conf,
			TotalNodes: len(nodes),
			Nodes:      nodes,
		}
		if len(nodes) > maxImpactNodesPerTier {
			tier.Nodes = nodes[:maxImpactNodesPerTier]
			tier.Truncated = true
		}
		tiers = append(tiers, tier)
		total += len(nodes)
	}

	files := make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}
	sort.Strings(files)

	anyTruncated := false
	for _, t := range tiers {
		if t.Truncated {
			anyTruncated = true
			break
		}
	}

	return &ImpactResult{
		Root: EntityRef{
			ID:   rootID,
			Name: root.Name,
			Type: root.Type,
			File: root.File,
			Line: root.Line,
		},
		Tiers:         tiers,
		TotalAffected: total,
		AffectedFiles: files,
		Truncated:     anyTruncated,
	}, nil
}

// testFileSuffixes covers test file conventions for Go, TypeScript, and JavaScript.
// Python has two conventions: suffix-named (*_test.py) and prefix-named (test_*.py).
// Prefix detection is handled separately in isTestFile via filepath.Base + HasPrefix.
var testFileSuffixes = []string{
	"_test.go",
	"_test.ts", "_test.js",
	"_spec.ts", "_spec.js",
	"_test.py",       // Python suffix convention: auth_test.py
	".test.ts", ".test.js",
	".spec.ts", ".spec.js",
}

func isTestFile(file string) bool {
	base := filepath.Base(file)
	// Python prefix convention: test_auth.py, test_models.py, etc.
	if strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") {
		return true
	}
	for _, suffix := range testFileSuffixes {
		if strings.HasSuffix(file, suffix) {
			return true
		}
	}
	return false
}

// FindTestsFor returns the files of test nodes that call into the given node,
// found via reverse-BFS over CALLS edges limited to test files.
// The result is a deduplicated sorted list of test file paths.
// Returns an empty slice when no test coverage is found.
func (g *Graph) FindTestsFor(nodeID NodeID) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if _, ok := g.nodes[nodeID]; !ok {
		return nil
	}

	type entry struct {
		id    NodeID
		depth int
	}
	visited := map[NodeID]bool{nodeID: true}
	queue := []entry{{nodeID, 0}}
	testFiles := map[string]struct{}{}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= 5 { // cap at 5 hops to avoid runaway traversal
			continue
		}
		for _, e := range g.inEdges[cur.id] {
			if e.Type != EdgeCalls {
				continue
			}
			if visited[e.From] {
				continue
			}
			visited[e.From] = true
			caller := g.nodes[e.From]
			if caller == nil {
				continue
			}
			if isTestFile(caller.File) {
				testFiles[caller.File] = struct{}{}
			} else {
				// Keep traversing up — tests may call through non-test helpers.
				queue = append(queue, entry{e.From, cur.depth + 1})
			}
		}
	}

	files := make([]string, 0, len(testFiles))
	for f := range testFiles {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// ErrNodeNotFound is returned when a query targets a non-existent node.
type ErrNodeNotFound NodeID

func (e ErrNodeNotFound) Error() string {
	return "node not found: " + string(e)
}


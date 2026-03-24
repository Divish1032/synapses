package graph

import (
	"log"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/viterin/vek/vek32"
)

// bfsQueuePool reuses BFS queue slices across CarveEgoGraph calls to reduce
// allocation pressure on cache misses. The queue slice is reset to length 0
// before reuse — O(1) compared to O(N) map clearing.
var bfsQueuePool = sync.Pool{
	New: func() interface{} {
		s := make([]qItem, 0, 64)
		return &s
	},
}

// qItem is a BFS queue entry used by CarveEgoGraph.
type qItem struct {
	id  NodeID
	hop int
}

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
		// Use Unsafe* methods: the caller (CarveEgoGraph) already holds g.mu.RLock
		// and the index is immutable after ready=1. This avoids acquiring idx.mu.RLock
		// three times per BFS step (Seq + OutNeighbours + InNeighbours).
		seq := idx.UnsafeSeq(id)
		if seq == 0 {
			return nil
		}
		outSeqs, outTypes := idx.UnsafeOutNeighbours(seq)
		inSeqs, inTypes := idx.UnsafeInNeighbours(seq)
		edges := make([]*Edge, 0, len(outSeqs)+len(inSeqs))
		for i, tSeq := range outSeqs {
			if idx.UnsafeIsTombstoned(tSeq) {
				continue
			}
			edges = append(edges, &Edge{
				From: id,
				To:   idx.SeqIDs[tSeq],
				Type: EdgeType(idx.Pool.Value(outTypes[i])),
			})
		}
		for i, fSeq := range inSeqs {
			if idx.UnsafeIsTombstoned(fSeq) {
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

// pprScores computes Personalized PageRank scores using power iteration on a
// sparse candidate subgraph bounded by a BFS pre-pass.
//
// Algorithm overview:
//  1. Build a personalised teleport vector (root=1.0; struct/interface receiver
//     methods at 0.9, matching BFS method-seeding behaviour).
//  2. Collect a candidate set via BFS from the teleport targets up to
//     pprBFSHorizon hops. Only nodes in this set participate in the random
//     walk — nodes beyond the horizon have negligible PPR rank for any
//     practical alpha (effective reach ≈ 1/alpha ≈ 7 hops for alpha=0.15).
//     This keeps PPR O(K×E×I) where K is candidate size, not O(N×E×I),
//     enabling production use on large graphs without O(N) work per call.
//  3. Build undirected adjacency restricted to the candidate set.
//     DirectionBoost is applied as a transition-probability bias on CALLS edges:
//     positive boost multiplies outgoing CALLS weights; negative multiplies
//     incoming. This causes the walk to prefer callees (intent="modify") or
//     callers (intent="debug"), consistent with BFS directional behaviour.
//  4. Power iteration until L∞ convergence (epsilon=1e-6, max 100 iters).
//     Dangling nodes (zero out-weight within candidate set) redistribute mass
//     to the teleport distribution to conserve total probability mass.
//
// Must be called with g.mu.RLock held (CarveEgoGraph already holds it).
func (g *Graph) pprScores(rootID NodeID, cfg CarveConfig, idx *GraphIndex) map[NodeID]float64 {
	alpha := cfg.Alpha
	if alpha <= 0 || alpha >= 1 {
		alpha = 0.15
	}

	weights := cfg.EdgeWeights
	if weights == nil {
		weights = DefaultEdgeWeights
	}

	// ── Teleport vector ────────────────────────────────────────────────────────
	// For struct/interface roots, receiver methods are seeded at weight 0.9 so
	// they surface even when they share no direct edge with the struct node.
	teleport := make(map[NodeID]float64, 8)
	teleport[rootID] = 1.0
	if rootNode := g.nodes[rootID]; rootNode != nil &&
		(rootNode.Type == NodeStruct || rootNode.Type == NodeInterface) {
		if idx != nil && idx.Ready() {
			for _, mSeq := range idx.ReceiverMethodSeqs(rootNode.Name) {
				if !idx.UnsafeIsTombstoned(mSeq) {
					teleport[idx.SeqIDs[mSeq]] = 0.9
				}
			}
		} else {
			// Fallback: linear scan when the CSR index is not yet built.
			// This path is taken at startup and in all unit tests (no index built).
			prefix := rootNode.Name + "."
			for _, n := range g.nodes {
				if n.Type == NodeMethod && strings.HasPrefix(n.Name, prefix) {
					if _, already := teleport[n.ID]; !already {
						teleport[n.ID] = 0.9
					}
				}
			}
		}

		// For interface roots, also seed concrete implementors and their receiver
		// methods. IMPLEMENTS edges have direction concrete→interface, so incoming
		// edges on the interface node (e.To==rootID) identify the concrete types.
		// Seeding at 0.85 ensures implementing types surface even when they are
		// beyond the normal PPR BFS horizon via other edge paths.
		if rootNode.Type == NodeInterface {
			for _, e := range g.outInEdges(rootID, idx) {
				if e.Type != EdgeImplements || e.To != rootID {
					continue
				}
				implID := e.From
				if _, already := teleport[implID]; !already {
					teleport[implID] = 0.85
				}
				implNode := g.nodes[implID]
				if implNode == nil {
					continue
				}
				if idx != nil && idx.Ready() {
					for _, mSeq := range idx.ReceiverMethodSeqs(implNode.Name) {
						if !idx.UnsafeIsTombstoned(mSeq) {
							mID := idx.SeqIDs[mSeq]
							if _, alreadyM := teleport[mID]; !alreadyM {
								teleport[mID] = 0.85
							}
						}
					}
				} else {
					prefix := implNode.Name + "."
					for _, n := range g.nodes {
						if n.Type == NodeMethod && strings.HasPrefix(n.Name, prefix) {
							if _, alreadyM := teleport[n.ID]; !alreadyM {
								teleport[n.ID] = 0.85
							}
						}
					}
				}
			}
		}
	}
	// Normalise to sum=1.0 (required for a valid probability vector).
	teleportSum := 0.0
	for _, v := range teleport {
		teleportSum += v
	}
	for k := range teleport {
		teleport[k] /= teleportSum
	}

	// ── Sparse candidate set ───────────────────────────────────────────────────
	// BFS from all teleport targets up to pprBFSHorizon undirected hops.
	// Seeding from teleport targets (not just root) ensures struct methods and
	// their downstream subgraphs are included in the candidate set.
	const pprBFSHorizon = 6
	const maxPPRCandidates = 10_000
	candidate := make(map[NodeID]struct{}, 128)
	frontier := make([]NodeID, 0, len(teleport))
	for id := range teleport {
		candidate[id] = struct{}{}
		frontier = append(frontier, id)
	}
	for hop := 0; hop < pprBFSHorizon && len(frontier) > 0; hop++ {
		var next []NodeID
		for _, id := range frontier {
			if len(candidate) >= maxPPRCandidates {
				break
			}
			// FlatGraph fast path: use cache-friendly CSR adjacency when available.
			if flatNbs := g.flatGraphNeighbors(id); flatNbs != nil {
				for _, nb := range flatNbs {
					if len(candidate) >= maxPPRCandidates {
						break
					}
					if _, seen := candidate[nb]; !seen {
						candidate[nb] = struct{}{}
						next = append(next, nb)
					}
				}
				continue
			}
			for _, e := range g.outInEdges(id, idx) {
				if len(candidate) >= maxPPRCandidates {
					break
				}
				nb := e.To
				if e.To == id {
					nb = e.From
				}
				if _, seen := candidate[nb]; !seen {
					candidate[nb] = struct{}{}
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}

	// ── Adjacency (restricted to candidate set) ────────────────────────────────
	type nbEntry struct {
		id     NodeID
		weight float64
	}
	adj := make(map[NodeID][]nbEntry, len(candidate))
	outWeightSum := make(map[NodeID]float64, len(candidate))

	for id := range candidate {
		for _, e := range g.outInEdges(id, idx) {
			nb := e.To
			isOutgoing := e.From == id
			if !isOutgoing {
				nb = e.From
			}
			// Skip edges leaving the candidate set — those nodes have negligible rank.
			if _, inCand := candidate[nb]; !inCand {
				continue
			}

			w := edgeWeight(e.Type, weights)

			// Mirror the BFS confidence scaling for HANDLES edges.
			if e.Type == EdgeHandles {
				if routeNode := g.nodes[e.From]; routeNode != nil {
					if conf, err := strconv.ParseFloat(routeNode.Metadata["confidence"], 64); err == nil && conf > 0 {
						w *= conf
					}
				}
			}

			// Sprint 15 #3: apply per-edge learned weight multiplier.
			w *= learnedEdgeMult(cfg.LearnedEdgeWeights, e.From, e.To, e.Type)

			// Apply DirectionBoost to CALLS edges as a transition-probability bias.
			if cfg.DirectionBoost != 0 && e.Type == EdgeCalls {
				if cfg.DirectionBoost > 0 && isOutgoing {
					w *= (1.0 + cfg.DirectionBoost)
				} else if cfg.DirectionBoost < 0 && !isOutgoing {
					w *= (1.0 - cfg.DirectionBoost) // double-neg → 1+|boost|
				}
			}

			// Sprint 16 #4: apply cross-domain decay in PPR adjacency construction.
			// Reduces the transition weight for cross-domain edges so PPR assigns lower
			// rank to nodes in other domains relative to same-domain nodes at equal
			// structural distance. Mirrors the BFS cross-domain decay behaviour.
			if cfg.CrossDomainDecay > 0 && cfg.CrossDomainDecay < 1 {
				currNode := g.nodes[id]
				neighNode := g.nodes[nb]
				if currNode != nil && neighNode != nil {
					currDomain := currNode.Domain
					if currDomain == "" {
						currDomain = DomainCode
					}
					neighDomain := neighNode.Domain
					if neighDomain == "" {
						neighDomain = DomainCode
					}
					if currDomain != neighDomain {
						w *= cfg.CrossDomainDecay
					}
				}
			}

			adj[id] = append(adj[id], nbEntry{nb, w})
			outWeightSum[id] += w
		}
	}

	// ── Power iteration ────────────────────────────────────────────────────────
	//   rank[v] = α·teleport[v] + (1-α)·Σ_u rank[u]·w(u,v)/outDeg(u)
	//
	// Dangling nodes (outWeightSum==0 within candidate set) redistribute their
	// full mass to the teleport distribution to conserve total probability mass.
	rank := make(map[NodeID]float64, len(candidate))
	rank[rootID] = 1.0
	newRank := make(map[NodeID]float64, len(candidate))

	const pprMaxIter = 100
	const pprEpsilon = 1e-6 // L∞ convergence threshold

	for iter := 0; iter < pprMaxIter; iter++ {
		for k := range newRank {
			delete(newRank, k)
		}

		// Teleport contribution (α fraction always returns to personalised roots).
		for id, tv := range teleport {
			newRank[id] += alpha * tv
		}

		// Propagation: distribute each candidate node's rank to its neighbours.
		for id := range candidate {
			r := rank[id]
			if r == 0 {
				continue
			}
			total := outWeightSum[id]
			if total <= 0 {
				// Dangling node: redistribute to teleport distribution.
				for tid, tv := range teleport {
					newRank[tid] += (1 - alpha) * r * tv
				}
				continue
			}
			for _, nb := range adj[id] {
				newRank[nb.id] += (1 - alpha) * r * nb.weight / total
			}
		}

		// L∞ convergence: stop when the maximum per-node change is below epsilon.
		maxDelta := 0.0
		for id := range candidate {
			if d := math.Abs(newRank[id] - rank[id]); d > maxDelta {
				maxDelta = d
			}
		}
		rank, newRank = newRank, rank

		if maxDelta < pprEpsilon {
			break
		}
	}

	return rank
}

// CarveEgoGraph extracts a relevance-ranked subgraph centred on the given root node.
//
// When cfg.UsePPR is false (default), the algorithm is:
//  1. BFS outward from root, up to cfg.MaxDepth hops.
//  2. Each node is assigned a relevance score:
//     relevance = edgeTypeWeight(edge) × (cfg.DecayFactor ^ hopCount)
//  3. When a node is reachable via multiple paths the maximum score is kept.
//  4. If the estimated token cost exceeds cfg.TokenBudget, the lowest-scored
//     nodes are pruned (highest-hop, lowest-weight first).
//  5. Only edges where both endpoints survived pruning are included.
//
// When cfg.UsePPR is true, step 1-3 are replaced by Personalized PageRank
// (see pprScores). Steps 4-5 are identical. PPR captures multi-path importance
// that BFS max-score heuristic cannot represent.
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

	// decay is used by hopDistance (display-only) in the post-processing section.
	// For PPR, hop counts are approximate — PPR scores don't follow the BFS
	// decay^hop formula, but the display heuristic is acceptable for both modes.
	decay := cfg.DecayFactor
	if decay <= 0 || decay > 1 {
		decay = 0.5
	}

	// visited maps nodeID → relevance score, consumed by the post-processing
	// pipeline below (MinRelevance, centrality boost, token budget, edges).
	// Populated by PPR power iteration or BFS max-score traversal.
	var visited map[NodeID]float64
	bfsTruncated := false

	if cfg.UsePPR {
		// PPR path: power iteration scores all reachable nodes.
		// MinRelevance (applied in post-processing below) acts as the reach
		// limiter in place of MaxDepth — PPR naturally assigns near-zero scores
		// to distant nodes, which MinRelevance=0.01 (default) prunes cleanly.
		visited = g.pprScores(rootID, cfg, idx)
		// Root-pin: undirected PPR can rank high-degree neighbours above root
		// (a degree-2 neighbour accumulates random-walk mass from both root and
		// the rest of the graph). Always output root at relevance=1.0 regardless
		// of the PPR mathematical rank.
		visited[rootID] = 1.0
	} else {
		weights := cfg.EdgeWeights
		if weights == nil {
			weights = DefaultEdgeWeights
		}

		// BFS state.
		visited = make(map[NodeID]float64) // nodeID → best relevance seen
		visited[rootID] = 1.0

		qp := bfsQueuePool.Get().(*[]qItem)
		queue := (*qp)[:0]
		queue = append(queue, qItem{rootID, 0})
		defer func() { *qp = queue[:0]; bfsQueuePool.Put(qp) }()

		// Struct/interface nodes have no CALLS edges — only DEFINES from their file.
		// Seed BFS with the struct's methods so the carve includes method-level context.
		if rootNode := g.nodes[rootID]; rootNode != nil &&
			(rootNode.Type == NodeStruct || rootNode.Type == NodeInterface) {
			if idx != nil && idx.Ready() {
				// Use the receiverIndex for O(methods) instead of O(all_nodes).
				for _, mSeq := range idx.ReceiverMethodSeqs(rootNode.Name) {
					if idx.UnsafeIsTombstoned(mSeq) {
						continue
					}
					mID := idx.SeqIDs[mSeq]
					visited[mID] = 0.9 // slightly below root
					queue = append(queue, qItem{mID, 0})
				}
			} else {
				// Fallback: linear scan when index is not ready.
				prefix := rootNode.Name + "."
				for _, n := range g.nodes {
					if n.Type == NodeMethod && strings.HasPrefix(n.Name, prefix) {
						visited[n.ID] = 0.9
						queue = append(queue, qItem{n.ID, 0})
					}
				}
			}

			// For interface roots, also seed concrete implementors and their
			// receiver methods at 0.85. IMPLEMENTS edges point concrete→interface,
			// so we look for incoming edges (e.To==rootID, e.Type==EdgeImplements).
			// Without explicit seeding, implementors only surface at BFS-derived
			// relevance ~0.45; their methods at ~0.034 — too low to survive pruning.
			if rootNode.Type == NodeInterface {
				for _, e := range g.outInEdges(rootID, idx) {
					if e.Type != EdgeImplements || e.To != rootID {
						continue
					}
					implID := e.From
					// Guard: a self-loop IMPLEMENTS edge (parser bug) must never
					// downgrade the root from its pinned score of 1.0.
					if implID == rootID {
						continue
					}
					visited[implID] = 0.85
					queue = append(queue, qItem{implID, 0})
					implNode := g.nodes[implID]
					if implNode == nil {
						continue
					}
					if idx != nil && idx.Ready() {
						for _, mSeq := range idx.ReceiverMethodSeqs(implNode.Name) {
							if idx.UnsafeIsTombstoned(mSeq) {
								continue
							}
							mID := idx.SeqIDs[mSeq]
							// Max-score semantics: preserve a higher score already set
							// by the interface's own method seeding (e.g. 0.9 when the
							// interface and implementor share the same receiver name).
							// Only enqueue if this is the first visit to avoid duplicate
							// BFS expansions from the same node.
							if prev, seen := visited[mID]; !seen {
								visited[mID] = 0.85
								queue = append(queue, qItem{mID, 0})
							} else if 0.85 > prev {
								visited[mID] = 0.85
								// Already queued — score updated; BFS will use the new value.
							}
						}
					} else {
						prefix := implNode.Name + "."
						for _, n := range g.nodes {
							if n.Type == NodeMethod && strings.HasPrefix(n.Name, prefix) {
								if prev, seen := visited[n.ID]; !seen {
									visited[n.ID] = 0.85
									queue = append(queue, qItem{n.ID, 0})
								} else if 0.85 > prev {
									visited[n.ID] = 0.85
								}
							}
						}
					}
				}
			}
		}

		const maxVisited = 10_000

		for head1 := 0; head1 < len(queue); head1++ {
			curr := queue[head1]

			if len(visited) >= maxVisited {
				bfsTruncated = true
				break
			}

			if curr.hop >= cfg.MaxDepth {
				continue
			}

			// Traverse both outgoing and incoming edges so the carve captures
			// "what does this call" AND "what calls this".
			// When the columnar index is ready, reads from CSR arrays (cache-friendly,
			// skips tombstoned nodes). Falls back to pointer-map when not ready.
			allEdges := g.outInEdges(curr.id, idx)
			// Degree-normalized adaptive decay (GCN-style, Kipf & Welling ICLR 2017).
			// High-degree hubs decay children faster to prevent hub explosion;
			// low-degree nodes in narrow chains receive a relatively smaller penalty.
			//
			// BFS-path-only safeguard: this formula applies only when UsePPR=false.
			// PPR (Sprint 13 #2) assigns near-zero scores to distant nodes naturally
			// via power iteration — no explicit decay is needed. This block is the
			// BFS fallback that prevents hub explosion until PPR becomes the default.
			localDecay := decay / (1.0 + math.Log2(float64(len(allEdges)+1)))

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

				// Sprint 15 #3: apply per-edge learned weight multiplier derived
				// from historical task outcomes. Neutral (1.0) when no entry exists.
				typeWeight *= learnedEdgeMult(cfg.LearnedEdgeWeights, e.From, e.To, e.Type)

				relevance := typeWeight * localDecay * visited[curr.id]

				neighbor := e.To
				if e.To == curr.id {
					neighbor = e.From
				}

				// Sprint 16 #4: apply cross-domain decay when BFS crosses a domain boundary.
				// When a node in one domain (e.g. code) links to a node in another domain
				// (e.g. infra, api), multiply relevance by CrossDomainDecay so cross-domain
				// neighbors rank lower than same-domain neighbors at equal structural distance.
				// Only applied when CrossDomainDecay is in (0, 1) and both nodes are visible.
				if cfg.CrossDomainDecay > 0 && cfg.CrossDomainDecay < 1 {
					currNode := g.nodes[curr.id]
					neighNode := g.nodes[neighbor]
					if currNode != nil && neighNode != nil {
						currDomain := currNode.Domain
						if currDomain == "" {
							currDomain = DomainCode
						}
						neighDomain := neighNode.Domain
						if neighDomain == "" {
							neighDomain = DomainCode
						}
						if currDomain != neighDomain {
							relevance *= cfg.CrossDomainDecay
						}
					}
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
						// Only re-enqueue if not previously visited at a lower hop count.
						// This prevents exponential queue growth on dense graphs.
						// Known accuracy tradeoff: when a node is re-discovered at a
						// higher score, its subtree is NOT re-explored. Intentional —
						// re-enqueueing would degrade to Dijkstra-like complexity on
						// dense graphs. The score update still improves pruning.
						if !seen {
							queue = append(queue, qItem{neighbor, curr.hop + 1})
						}
					}
				}

				// Edge collection deferred to after budget pruning to prevent
				// unbounded memory on hub nodes with 50K+ edges.
			}
		}
	} // end BFS / PPR scoring branch

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
			if n, ok := g.nodes[id]; ok && isTestFile(n.File) {
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

	// R13: Apply eigenvector centrality boost — architecturally important nodes
	// (connected to other important nodes) get a persistent relevance boost so
	// they survive token-budget pruning over structurally similar but obscure nodes.
	// Formula: relevance × (1 + centralityBeta × centrality[node])
	// Root is never penalized or boosted here; its relevance is fixed at 1.0.
	const centralityBeta = 0.2
	if idx != nil && idx.Ready() && len(idx.EigenvectorCentrality) > 0 {
		for i := range scored {
			if scored[i].id == rootID {
				continue
			}
			seq := idx.UnsafeSeq(scored[i].id)
			if seq > 0 && int(seq) < len(idx.EigenvectorCentrality) {
				scored[i].relevance *= 1.0 + centralityBeta*idx.EigenvectorCentrality[seq]
			}
		}
	}

	// Sprint 13 #3: Semantic-structural hybrid scoring (CodexGraph / LEGO-GraphRAG).
	// Blends BFS/PPR structural scores with embedding cosine similarity to root:
	//   finalScore = (1-λ)×structural + λ×cosineSim(embed(root), embed(n))
	//
	// Since UpsertEmbedding pre-normalizes all vectors to unit length,
	// cosine similarity reduces to a dot product — no sqrt needed.
	//
	// Applied only when EmbeddingLookup is set AND HybridLambda > 0. Falls back
	// to pure structural scoring when the root has no stored embedding. Nodes
	// with no embedding are left unchanged (structural score preserved).
	// Root relevance is always pinned at 1.0 regardless of similarity.
	if cfg.EmbeddingLookup != nil && cfg.HybridLambda > 0 {
		// Batch-fetch embeddings for all scored nodes + root in one round-trip.
		batchIDs := make([]NodeID, 0, len(scored)+1)
		batchIDs = append(batchIDs, rootID)
		for i := range scored {
			if scored[i].id != rootID {
				batchIDs = append(batchIDs, scored[i].id)
			}
		}
		embeddings := cfg.EmbeddingLookup(batchIDs)
		rootVec := embeddings[rootID]

		// Only blend when we have a root embedding — without it cosine similarity
		// is undefined and we must fall through to pure structural ranking.
		// Log at debug level so operators know the semantic channel is inactive.
		if len(rootVec) == 0 {
			log.Printf("graph: hybrid scoring fallback for root %q — no stored embedding; using pure structural scoring (lambda=%.2f inactive)", rootID, cfg.HybridLambda)
		}
		if len(rootVec) > 0 {
			λ := cfg.HybridLambda
			for i := range scored {
				if scored[i].id == rootID {
					continue // root stays pinned at 1.0
				}
				nodeVec := embeddings[scored[i].id]
				if len(nodeVec) == 0 {
					continue // no embedding for this node — keep structural score
				}
				sim := dotProduct(rootVec, nodeVec)
				if sim < 0 {
					sim = 0 // negative cosine similarity (opposite meaning) → no boost
				}
				// Clamp structural score to [0, 1] before blending so the λ weights
				// hold their meaning. Centrality boost and struct-method seeding (0.9)
				// can push scores above 1.0 (e.g. 0.9 × 1.2 = 1.08), which would make
				// the (1-λ) structural term dominate and distort the blend ratio.
				// Root is always exactly 1.0 and is skipped above; no non-root node
				// should claim higher structural relevance than the root itself.
				structScore := scored[i].relevance
				if structScore > 1.0 {
					structScore = 1.0
				}
				scored[i].relevance = (1-λ)*structScore + λ*sim
			}
		}
	}

	// Sprint 15 #2: Quality score boost — entities that have consistently
	// produced helpful context rank higher; entities that caused repeated
	// corrections or session abandonment rank lower.
	//
	// Formula: relevance *= (1 + qualityBeta × tanh(score / qualityScale))
	//   tanh squashes the unbounded quality score into (-1, +1) so a very
	//   negative entity is penalised by at most qualityBeta (never zeroed).
	//   Root is always pinned at 1.0 and is never penalised or boosted.
	//   Nodes with no quality record are left unchanged.
	if cfg.QualityScoreLookup != nil {
		// Build QualityNode slice — carries ID, Name, File so the closure can
		// convert to entityWithPath without re-acquiring g.mu.RLock (held here).
		// g.nodes is accessed directly (no lock) because we are inside RLock.
		batchNodes := make([]QualityNode, 0, len(scored))
		for i := range scored {
			if scored[i].id == rootID {
				continue
			}
			qn := QualityNode{ID: scored[i].id}
			if n := g.nodes[scored[i].id]; n != nil {
				qn.Name = n.Name
				qn.File = n.File
			}
			batchNodes = append(batchNodes, qn)
		}
		if len(batchNodes) > 0 {
			const qualityBeta = 0.2
			const qualityScale = 5.0
			qualityScores := cfg.QualityScoreLookup(batchNodes)
			for i := range scored {
				if scored[i].id == rootID {
					continue
				}
				if qs, ok := qualityScores[scored[i].id]; ok {
					scored[i].relevance *= 1.0 + qualityBeta*math.Tanh(qs/qualityScale)
				}
			}
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

	// Collect edges where both endpoints survived the budget cut.
	// Deferred to after pruning to avoid unbounded memory on hub nodes
	// with 50K+ edges — only edges between kept nodes are allocated.
	type edgeDedupKey struct {
		from, to NodeID
		typ      EdgeType
	}
	seen := make(map[edgeDedupKey]struct{})
	var outEdges []*Edge
	for id := range keep {
		edges := g.outInEdges(id, idx)
		for _, e := range edges {
			_, fromOK := keep[e.From]
			_, toOK := keep[e.To]
			if !fromOK || !toOK {
				continue
			}
			key := edgeDedupKey{e.From, e.To, e.Type}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			outEdges = append(outEdges, e)
		}
	}

	result := &SubGraph{
		Root:           rootID,
		Nodes:          outNodes,
		Edges:          outEdges,
		Truncated:      truncated || bfsTruncated,
		TruncatedCount: truncatedCount,
	}
	g.cache.put(rootID, cfg, fp, result)
	return result, nil
}

// dotProduct computes the dot product of two pre-normalized float32 vectors.
// Since all node embeddings are normalized to unit length by UpsertEmbedding,
// this equals cosine similarity without any additional sqrt computation.
// Uses SIMD-accelerated vek32.Dot (3–5× faster than a scalar loop) for
// production workloads with 768/1536-dim embedding vectors.
// Returns 0 for mismatched or empty lengths.
func dotProduct(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	return float64(vek32.Dot(a, b))
}

// edgeWeight returns the configured weight for an edge type, falling back to 0.5.
func edgeWeight(et EdgeType, weights map[EdgeType]float64) float64 {
	if w, ok := weights[et]; ok {
		return w
	}
	return 0.5
}

// learnedEdgeMult returns the learned weight multiplier for a specific edge
// from the LearnedEdgeWeights map (Sprint 15 #3). Returns 1.0 (neutral) when
// the map is nil or the edge has no entry — callers need not check for nil.
func learnedEdgeMult(lew map[EdgeWeightKey]float64, from, to NodeID, et EdgeType) float64 {
	if lew == nil {
		return 1.0
	}
	if m, ok := lew[EdgeWeightKey{From: from, To: to, Type: et}]; ok {
		return m
	}
	return 1.0
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

	const maxVisited = 10_000
	visited := map[NodeID]int{rootID: 0} // node → first-seen depth
	queue := []entry{{rootID, 0}}
	fileSet := map[string]struct{}{}
	bfsTruncated := false

	for head2 := 0; head2 < len(queue); head2++ {
		if len(visited) >= maxVisited {
			bfsTruncated = true
			break
		}
		cur := queue[head2]

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

	// Sprint 16 #5: cross-domain impact analysis.
	// Collect entities reachable from root via cross-domain edges in one hop.
	// Edges have already been filtered at injection time (suppressed edges are never
	// injected; namematcher enforces confidence >= 0.6), so all edges present in
	// the in-memory graph already satisfy the confidence threshold.
	cdResult := collectCrossDomainImpact(g, rootID)

	// Add cross-domain entity files to AffectedFiles so callers (agents) see the
	// full set of files to review — including Terraform, API specs, and doc files.
	for _, ref := range cdResult.refs {
		if ref.File != "" {
			fileSet[ref.File] = struct{}{}
		}
	}
	// Rebuild files slice now that cross-domain files have been added.
	files = make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}
	sort.Strings(files)

	return &ImpactResult{
		Root: EntityRef{
			ID:   rootID,
			Name: root.Name,
			Type: root.Type,
			File: root.File,
			Line: root.Line,
		},
		Tiers:                tiers,
		TotalAffected:        total,
		AffectedFiles:        files,
		Truncated:            anyTruncated || bfsTruncated,
		CrossDomainImpact:    cdResult.refs,
		CrossDomainAffected:  len(cdResult.refs),
		CrossDomainTruncated: cdResult.truncated,
	}, nil
}

// CrossDomainImpactForNode returns the cross-domain entities directly reachable
// from nodeID via cross-domain edges. It is the public entry point used by
// the struct/interface aggregation path in handleGetImpact.
// Returns the refs and a truncated flag (true when capped at maxCrossDomainImpactNodes).
func (g *Graph) CrossDomainImpactForNode(nodeID NodeID) ([]CrossDomainRef, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	r := collectCrossDomainImpact(g, nodeID)
	return r.refs, r.truncated
}

// crossDomainCaps defines per-category node limits for collectCrossDomainImpact.
//
// Categories differ in signal strength and realistic cardinality:
//   - DEPLOYS/CONSUMES: explicit hand-linked or parser-derived, high value, low volume → 30 each
//   - CONFIGURED_BY/DOCUMENTS/MANUAL: explicit, trusted, typically very few → 20 each
//   - MENTIONS: synthetic name-match heuristic, variable confidence, potentially high volume → 15
//
// High-signal categories fill first (collection order below). MENTIONS is collected
// last so it cannot crowd out explicit relationships even when it has hundreds of matches.
// The overall cap (maxCrossDomainImpactNodes=100) is a safety net for pathological graphs.
var crossDomainCaps = map[EdgeType]int{
	EdgeDeploys:      30,
	EdgeConsumes:     30,
	EdgeConfiguredBy: 20,
	EdgeDocuments:    20,
	EdgeManual:       20,
	EdgeMentions:     15,
}

// maxCrossDomainImpactNodes is the overall safety-net cap across all categories.
// Normal operation stays well under this via per-category caps, but it prevents
// pathological graphs (e.g. 200 DEPLOYS + 200 MENTIONS) from blowing up responses.
const maxCrossDomainImpactNodes = 100

// crossDomainResult packages the output of collectCrossDomainImpact so callers
// can propagate the truncation signal without a separate call.
type crossDomainResult struct {
	refs      []CrossDomainRef
	truncated bool
}

// collectCrossDomainImpact finds entities reachable from root in one hop via
// cross-domain edges and returns them as CrossDomainRef slices.
//
// Per-category caps (crossDomainCaps) guarantee that high-volume synthetic edges
// (MENTIONS, up to 15) cannot crowd out explicit high-signal edges (DEPLOYS/CONSUMES,
// up to 30 each). Each category is bounded independently: even if a node has 200
// MENTIONS edges, DEPLOYS/CONSUMES still get their full quota.
//
// An overall safety-net cap (maxCrossDomainImpactNodes=100) prevents pathological
// graphs from producing unbounded output. truncated=true when either cap fires.
//
// Caller must hold g.mu.RLock.
// cdSeenKey deduplicates cross-domain refs by (NodeID, EdgeType) so that a
// node reachable via multiple cross-domain edge types (e.g. DEPLOYS and MENTIONS)
// appears once per relationship — preserving full edge-type diversity.
type cdSeenKey struct {
	id NodeID
	et EdgeType
}

func collectCrossDomainImpact(g *Graph, rootID NodeID) crossDomainResult {
	seen := make(map[cdSeenKey]bool)

	// perCat tracks how many nodes have been collected per edge type so we can
	// enforce per-category caps without an extra pass.
	perCat := make(map[EdgeType]int, len(crossDomainCaps))
	var refs []CrossDomainRef
	truncated := false

	// addRef attempts to add a CrossDomainRef. Returns true if added, false if
	// the per-category cap, overall cap, or deduplication prevented it.
	addRef := func(id NodeID, n *Node, et EdgeType) bool {
		k := cdSeenKey{id: id, et: et}
		if seen[k] {
			return false
		}
		if id == rootID { // never include the root itself
			return false
		}
		cap, ok := crossDomainCaps[et]
		if !ok {
			cap = 10 // conservative default for unknown future edge types
		}
		if perCat[et] >= cap {
			truncated = true
			return false
		}
		if len(refs) >= maxCrossDomainImpactNodes {
			truncated = true
			return false
		}
		seen[k] = true
		perCat[et]++
		refs = append(refs, CrossDomainRef{
			EntityRef: EntityRef{
				ID:   id,
				Name: n.Name,
				Type: n.Type,
				File: n.File,
				Line: n.Line,
			},
			EdgeType: et,
			Category: CrossDomainCategory(et),
		})
		return true
	}

	// Single-pass collection over outEdges then inEdges. Per-category counters enforce
	// the priority ordering implicitly: because we scan all edges once, a category
	// that fills its cap early still allows other categories to accumulate their quota.
	// O(E) where E = out-degree + in-degree of root (bounded in practice).
	//
	// Forward outEdges: DEPLOYS, CONSUMES, CONFIGURED_BY, MANUAL, MENTIONS.
	for _, e := range g.outEdges[rootID] {
		if !IsCrossDomainEdge(e.Type) {
			continue
		}
		n := g.nodes[e.To]
		if n == nil {
			continue
		}
		addRef(e.To, n, e.Type)
	}

	// Reverse inEdges: DOCUMENTS (docs → this entity), MENTIONS (name-match → this),
	// MANUAL (user link → this). These surface stale docs and related entities.
	for _, e := range g.inEdges[rootID] {
		if e.Type != EdgeDocuments && e.Type != EdgeMentions && e.Type != EdgeManual {
			continue
		}
		n := g.nodes[e.From]
		if n == nil {
			continue
		}
		addRef(e.From, n, e.Type)
	}

	return crossDomainResult{refs: sortCrossDomainRefs(refs), truncated: truncated}
}

// sortCrossDomainRefs sorts cross-domain refs by category then name for stable output.
func sortCrossDomainRefs(refs []CrossDomainRef) []CrossDomainRef {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Category != refs[j].Category {
			return refs[i].Category < refs[j].Category
		}
		return refs[i].Name < refs[j].Name
	})
	return refs
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
	const maxVisited = 5_000
	visited := map[NodeID]bool{nodeID: true}
	queue := []entry{{nodeID, 0}}
	testFiles := map[string]struct{}{}

	for head3 := 0; head3 < len(queue); head3++ {
		if len(visited) >= maxVisited {
			break
		}
		cur := queue[head3]
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


package benchmark

import (
	"fmt"
	"sort"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── Scenario 1: Context Completeness ────────────────────────────────────────
//
// For high-fanin nodes (≥5 callers), CarveEgoGraph should return a meaningful
// fraction of their direct callers. This validates BFS traversal quality.
//
// Ground truth: direct callers from the graph's own edge data.
// Pass threshold: 0.6 average F1 (budget-constrained BFS can't get all callers).

func scenarioContextCompleteness() Scenario {
	return Scenario{
		Name:          "context-completeness",
		Description:   "CarveEgoGraph returns direct callers for high-fanin functions",
		PassThreshold: 0.6,
		Run:           runContextCompleteness,
	}
}

func runContextCompleteness(g *graph.Graph, _ *store.Store) ([]QueryResult, error) {
	// Find up to 5 high-fanin functions/methods (≥5 callers).
	type candidate struct {
		id    graph.NodeID
		fanin int
	}
	var candidates []candidate
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		fi := g.Fanin(n.ID)
		if fi >= 5 {
			candidates = append(candidates, candidate{n.ID, fi})
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no functions with ≥5 callers found — graph too small for this scenario")
	}

	// Sort by fanin descending, take top 5.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].fanin > candidates[j].fanin
	})
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 2
	cfg.TokenBudget = 12000

	var results []QueryResult
	for _, c := range candidates {
		// Ground truth: all direct callers (incoming CALLS edges).
		expectedCallers := make(map[string]bool)
		for _, e := range g.InEdges(c.id) {
			if e.Type == graph.EdgeCalls {
				expectedCallers[string(e.From)] = true
			}
		}
		if len(expectedCallers) == 0 {
			continue
		}

		start := time.Now()
		sg, err := g.CarveEgoGraph(c.id, cfg)
		elapsed := time.Since(start)

		if err != nil {
			continue
		}

		// Returned: non-root nodes in the subgraph.
		returned := make(map[string]bool)
		for _, cn := range sg.Nodes {
			if cn.Node.ID != c.id {
				returned[string(cn.Node.ID)] = true
			}
		}

		label := fmt.Sprintf("CarveEgoGraph(%s) [fanin=%d]", nodeName(g, c.id), c.fanin)
		results = append(results, makeQueryResult(label, expectedCallers, returned, elapsed))
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no valid high-fanin candidates produced results")
	}
	return results, nil
}

// ── Scenario 2: Search Accuracy ────────────────────────────────────────────
//
// FTS search for a known function name should return that function in top-5.
// Tests the FTS5 index and BM25 ranking.
//
// Ground truth: the exact node that was queried for.
// Pass threshold: 0.8 (top-5 should almost always contain the exact match).

func scenarioSearchAccuracy() Scenario {
	return Scenario{
		Name:          "search-accuracy",
		Description:   "FTS search for known function names returns them in top-5",
		PassThreshold: 0.8,
		Run:           runSearchAccuracy,
	}
}

func runSearchAccuracy(g *graph.Graph, st *store.Store) ([]QueryResult, error) {
	// Pick up to 10 functions/methods with distinct names.
	seen := make(map[string]bool)
	var targets []*graph.Node
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		if n.Name == "" || n.Name == "main" || n.Name == "init" {
			continue
		}
		if seen[n.Name] {
			continue // skip duplicate names (ambiguous matches)
		}
		seen[n.Name] = true
		targets = append(targets, n)
		if len(targets) >= 10 {
			break
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no named functions found — graph too small")
	}

	var results []QueryResult
	for _, target := range targets {
		expected := map[string]bool{string(target.ID): true}

		start := time.Now()
		hits, err := st.SemanticSearch(target.Name, 5)
		elapsed := time.Since(start)

		if err != nil {
			continue
		}

		returned := make(map[string]bool)
		for _, h := range hits {
			returned[h.ID] = true
		}

		label := fmt.Sprintf("SemanticSearch(%q)", target.Name)
		results = append(results, makeQueryResult(label, expected, returned, elapsed))
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no search queries returned results")
	}
	return results, nil
}

// ── Scenario 3: Impact Coverage ────────────────────────────────────────────
//
// For a node with known callers, ImpactAnalysis should surface at least some
// of those callers in its result tiers.
//
// Ground truth: direct callers from graph edges.
// Pass threshold: 0.5 (impact analysis may be depth-limited).

func scenarioImpactCoverage() Scenario {
	return Scenario{
		Name:          "impact-coverage",
		Description:   "ImpactAnalysis surfaces direct callers of modified functions",
		PassThreshold: 0.5,
		Run:           runImpactCoverage,
	}
}

func runImpactCoverage(g *graph.Graph, _ *store.Store) ([]QueryResult, error) {
	// Find up to 5 functions with moderate fanin (3-20 callers).
	type candidate struct {
		id    graph.NodeID
		fanin int
	}
	var candidates []candidate
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		fi := g.Fanin(n.ID)
		if fi >= 3 && fi <= 20 {
			candidates = append(candidates, candidate{n.ID, fi})
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no functions with 3-20 callers found")
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].fanin > candidates[j].fanin
	})
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}

	var results []QueryResult
	for _, c := range candidates {
		// Ground truth: direct callers.
		expectedCallers := make(map[string]bool)
		for _, e := range g.InEdges(c.id) {
			if e.Type == graph.EdgeCalls {
				expectedCallers[string(e.From)] = true
			}
		}
		if len(expectedCallers) == 0 {
			continue
		}

		start := time.Now()
		impact, err := g.ImpactAnalysis(c.id, 3)
		elapsed := time.Since(start)

		if err != nil {
			continue
		}

		// Collect all node IDs from all impact tiers.
		returned := make(map[string]bool)
		for _, tier := range impact.Tiers {
			for _, n := range tier.Nodes {
				returned[string(n.ID)] = true
			}
		}

		label := fmt.Sprintf("ImpactAnalysis(%s) [fanin=%d]", nodeName(g, c.id), c.fanin)
		results = append(results, makeQueryResult(label, expectedCallers, returned, elapsed))
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no impact analysis candidates produced results")
	}
	return results, nil
}

// ── Scenario 4: Graph Reachability ───────────────────────────────────────────
//
// For known caller→callee edges, BFS over CALLS edges should find the target
// within a bounded depth. Tests graph connectivity and edge integrity.
//
// Ground truth: we pick edges that exist, so a path must exist.
// Pass threshold: 0.9 (direct edges should always be reachable).

func scenarioCallChainConnectivity() Scenario {
	return Scenario{
		Name:          "graph-reachability",
		Description:   "BFS over CALLS edges reaches known callees within 3 hops",
		PassThreshold: 0.9,
		Run:           runCallChainConnectivity,
	}
}

func runCallChainConnectivity(g *graph.Graph, _ *store.Store) ([]QueryResult, error) {
	// Collect CALLS edges and pick up to 10.
	type pair struct {
		from, to graph.NodeID
	}
	var pairs []pair
	for _, n := range g.AllNodes() {
		for _, e := range g.OutEdges(n.ID) {
			if e.Type == graph.EdgeCalls {
				pairs = append(pairs, pair{e.From, e.To})
				if len(pairs) >= 10 {
					break
				}
			}
		}
		if len(pairs) >= 10 {
			break
		}
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("no CALLS edges found — graph too small")
	}

	var results []QueryResult
	for _, p := range pairs {
		// BFS from p.from following CALLS edges, max 3 hops.
		start := time.Now()
		found := bfsReachable(g, p.from, p.to, 3)
		elapsed := time.Since(start)

		var precision, recall, f1 float64
		if found {
			precision, recall, f1 = 1.0, 1.0, 1.0
		}

		label := fmt.Sprintf("BFSReach(%s→%s)", nodeName(g, p.from), nodeName(g, p.to))
		results = append(results, QueryResult{
			Label:     label,
			Precision: precision,
			Recall:    recall,
			F1:        f1,
			LatencyMs: float64(elapsed.Microseconds()) / 1000.0,
			Expected:  1,
			Returned:  boolToInt(found),
			Relevant:  boolToInt(found),
		})
	}

	return results, nil
}

// bfsReachable checks if target is reachable from start via CALLS edges
// within maxDepth hops.
func bfsReachable(g *graph.Graph, start, target graph.NodeID, maxDepth int) bool {
	visited := map[graph.NodeID]bool{start: true}
	queue := []graph.NodeID{start}
	for depth := 0; depth < maxDepth && len(queue) > 0; depth++ {
		var next []graph.NodeID
		for _, id := range queue {
			for _, e := range g.OutEdges(id) {
				if e.Type != graph.EdgeCalls {
					continue
				}
				if e.To == target {
					return true
				}
				if !visited[e.To] {
					visited[e.To] = true
					next = append(next, e.To)
				}
			}
		}
		queue = next
	}
	return false
}

// ── Scenario 5: FTS Ranking Quality ──────────────────────────────────────────
//
// For each entity, searching its name should rank it #1 (exact match).
// Tests that the FTS5 BM25 scoring correctly prioritizes exact name matches.
//
// Ground truth: the target node should be rank 1.
// Pass threshold: 0.7 (some names may be ambiguous).

func scenarioFTSRanking() Scenario {
	return Scenario{
		Name:          "fts-ranking",
		Description:   "FTS search ranks exact name matches at position 1",
		PassThreshold: 0.7,
		Run:           runFTSRanking,
	}
}

func runFTSRanking(g *graph.Graph, st *store.Store) ([]QueryResult, error) {
	// Pick up to 10 functions with unique names.
	seen := make(map[string]bool)
	var targets []*graph.Node
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		if n.Name == "" || len(n.Name) < 4 {
			continue // skip very short names (likely ambiguous)
		}
		if seen[n.Name] {
			continue
		}
		seen[n.Name] = true
		targets = append(targets, n)
		if len(targets) >= 10 {
			break
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no named functions found")
	}

	var results []QueryResult
	for _, target := range targets {
		start := time.Now()
		hits, err := st.SemanticSearch(target.Name, 10)
		elapsed := time.Since(start)

		if err != nil || len(hits) == 0 {
			continue
		}

		// Check if target is rank 1.
		rank1 := hits[0].ID == string(target.ID)

		var precision, recall, f1 float64
		if rank1 {
			precision, recall, f1 = 1.0, 1.0, 1.0
		}

		label := fmt.Sprintf("FTSRank1(%q)", target.Name)
		results = append(results, QueryResult{
			Label:     label,
			Precision: precision,
			Recall:    recall,
			F1:        f1,
			LatencyMs: float64(elapsed.Microseconds()) / 1000.0,
			Expected:  1,
			Returned:  len(hits),
			Relevant:  boolToInt(rank1),
		})
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no FTS queries returned results")
	}
	return results, nil
}

// ── Scenario 6: Memory Recall ────────────────────────────────────────────────
//
// For memories inserted with known content, FTS recall should surface them
// when searching for their content keywords. Tests the memory write → search
// round-trip quality.
//
// Ground truth: memories we just inserted with known IDs.
// Pass threshold: 0.7 (FTS may rank some below top-k).

func scenarioMemoryRecall() Scenario {
	return Scenario{
		Name:          "memory-recall",
		Description:   "FTS recall surfaces memories matching their content keywords",
		PassThreshold: 0.7,
		Run:           runMemoryRecall,
	}
}

func runMemoryRecall(g *graph.Graph, st *store.Store) ([]QueryResult, error) {
	if st == nil {
		return nil, fmt.Errorf("store required for memory-recall scenario")
	}

	// Insert test memories with distinctive content.
	type testMemory struct {
		content string
		query   string // search query that should find this memory
	}
	memories := []testMemory{
		{
			content: "Authentication tokens must be rotated every 24 hours to comply with security policy",
			query:   "authentication token rotation",
		},
		{
			content: "Database connection pool size should be set to 2x CPU cores for optimal throughput",
			query:   "database connection pool",
		},
		{
			content: "Rate limiting middleware uses a sliding window algorithm with 100 requests per minute",
			query:   "rate limiting sliding window",
		},
		{
			content: "Cache invalidation follows write-through pattern to prevent stale reads in distributed nodes",
			query:   "cache invalidation write-through",
		},
		{
			content: "Error handling in the API layer must return structured JSON error responses with error codes",
			query:   "error handling API structured",
		},
	}

	// Use a short TTL so benchmark memories expire quickly after the run.
	shortExpiry := time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339)

	// Insert memories.
	insertedIDs := make([]string, len(memories))
	for i, tm := range memories {
		id := fmt.Sprintf("bench-recall-%d-%d", i, time.Now().UnixNano())
		m := store.Memory{
			ID:        id,
			Tier:      store.TierProject,
			Content:   tm.content,
			Source:    "benchmark",
			Tags:     `["benchmark"]`,
			ExpiresAt: shortExpiry,
		}
		insertedID, err := st.InsertMemory(m)
		if err != nil {
			return nil, fmt.Errorf("insert test memory %d: %w", i, err)
		}
		insertedIDs[i] = insertedID
	}

	// Search for each memory by its query.
	var results []QueryResult
	for i, tm := range memories {
		expected := map[string]bool{insertedIDs[i]: true}

		start := time.Now()
		found, err := st.SearchMemories(tm.query, 10)
		elapsed := time.Since(start)

		if err != nil {
			continue
		}

		returned := make(map[string]bool)
		for _, m := range found {
			returned[m.ID] = true
		}

		label := fmt.Sprintf("MemoryRecall(%q)", tm.query)
		results = append(results, makeQueryResult(label, expected, returned, elapsed))
	}

	// Clean up benchmark memories immediately instead of waiting for TTL expiry.
	// This prevents test data from appearing in real recall() results.
	for _, id := range insertedIDs {
		st.DeleteMemoryByID(id)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no memory recall queries returned results")
	}
	return results, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nodeName returns the name of a node by ID, or the raw ID if not found.
func nodeName(g *graph.Graph, id graph.NodeID) string {
	n := g.GetNode(id)
	if n != nil && n.Name != "" {
		return n.Name
	}
	return string(id)
}

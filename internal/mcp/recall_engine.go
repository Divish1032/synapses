package mcp

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/store"
)

// graphParentEntry records how a node was discovered during BFS traversal.
// ParentNodeID is the node from which this node was reached.
// EdgeType is the relationship type between them in the code graph.
// IsIncoming is true when this node was discovered via InEdges — meaning
// THIS node CALLS ParentNodeID in the real code (inverse of traversal direction).
// IsIncoming=false means ParentNodeID CALLS/IMPORTS/IMPLEMENTS this node.
type graphParentEntry struct {
	ParentNodeID string
	EdgeType     graph.EdgeType
	IsIncoming   bool
}

// GraphBFSResult is the output of a BFS traversal from seed nodes.
type GraphBFSResult struct {
	// Nodes: reachable node IDs, excluding seeds themselves.
	Nodes []string
	// ParentMap: maps each discovered node to the edge that found it.
	// Used to reconstruct traversal paths (anchor → seed walk).
	ParentMap map[string]graphParentEntry
	// SeedSet: original seed node IDs (for terminating path reconstruction).
	SeedSet map[string]bool
	// ActivationMap: spreading activation score for each reachable node (0.0–1.0).
	// Seeds are also present with activation 1.0.
	// Non-seed nodes receive activation proportional to edge type weight and
	// parent fan-out. Nodes reachable via multiple paths hold the maximum score.
	// Used to rank memories anchored to high-activation nodes higher in RRF.
	ActivationMap map[string]float64
}

// TraversalPath describes how a specific memory was reached via graph traversal.
type TraversalPath struct {
	MemoryID string `json:"memory_id"`
	// MemorySnippet is the first ~80 runes of memory content, for quick preview.
	MemorySnippet string `json:"memory_snippet"`
	// Path shows the directed structural path from query-matching entity to memory anchor.
	// →[EDGE]→ means the left node calls/imports/implements the right node.
	// ←[EDGE]- means the right node calls/imports/implements the left node.
	// Example: "AuthService →[CALLS]→ TokenValidator" (AuthService calls TokenValidator)
	// Example: "AuthService ←[CALLS]- TokenManager" (TokenManager calls AuthService)
	Path string `json:"path"`
	Hops int    `json:"hops"`
}

// GraphTraversalInfo describes the graph channel's multi-hop traversal for a recall() call.
// Surfaced in the response when the graph channel was active and found results.
type GraphTraversalInfo struct {
	Depth        int             `json:"depth"`
	AnchorCount  int             `json:"anchor_count"`  // query-matching seed entities
	VisitedNodes int             `json:"visited_nodes"` // graph nodes explored by BFS
	Paths        []TraversalPath `json:"paths,omitempty"`
	Note         string          `json:"note"`
}

// quadRecallSearch runs 4 parallel retrieval channels and merges via RRF.
// Returns memories ranked by fused score, plus per-memory channel attribution,
// stale embedding IDs, and optional graph traversal info.
//
// Channels:
//  1. BM25 — FTS5 full-text search on memory content (existing)
//  2. Semantic — cosine similarity on embeddings (existing, skipped if no embedder)
//  3. Graph — BFS from anchor entities of query-matching memories (skipped if no graph)
//  4. Temporal — recent memories scored by recency decay (no text filter)
//
// depth controls the graph channel's BFS hop count (0 = default 2, max 4).
// Each channel runs in its own goroutine with a 5s timeout.
// Channel errors are logged but never fail the entire recall.
// The response shape is unchanged — episodes are searched separately by the caller.
func (s *Server) quadRecallSearch(
	ctx context.Context,
	query string,
	limit int,
	includeStale bool,
	sinceDays int,
	untilTime *time.Time, // Sprint 10.5: optional upper time bound for temporal channel
	depth int,
) ([]store.Memory, map[string][]string, []string, *GraphTraversalInfo) {
	if limit <= 0 {
		limit = 5
	}
	// Clamp depth: 0 → default 2, negative → 1, >4 → 4.
	if depth <= 0 {
		depth = 2
	} else if depth > 4 {
		depth = 4
	}

	// Per-channel limit: request more than final limit so RRF has enough
	// candidates from each channel to produce meaningful fusion.
	channelLimit := limit * 3
	if channelLimit < 10 {
		channelLimit = 10
	}

	// Snapshot config once to prevent nil-pointer races if config is
	// hot-reloaded between the check and the merge branch below.
	cfg := s.config
	useConvex := cfg != nil && cfg.Recall.FusionMode == "convex"

	var (
		mu              sync.Mutex
		channels        = make(map[string][]string)
		channelScores   = make(map[string]*store.ChannelScores) // raw scores for ConvexMerge
		memMap          = make(map[string]store.Memory)          // id → full memory for hydration
		staleEmbeddings = make(map[string]bool)                  // memory IDs with stale embeddings (entity changed)
	)

	// Graph channel metadata: written only by the graph goroutine, read after
	// wg.Wait() which provides the synchronisation barrier — no mutex needed.
	var (
		graphResult    GraphBFSResult
		graphSeedCount int
		// graphAnchorMap: memID → anchor nodeID for memories returned by the
		// graph channel. Populated during activation sort, reused by traversal
		// info to avoid a second DB round-trip.
		graphAnchorMap map[string]string
	)

	collectMemories := func(name string, mems []store.Memory) {
		mu.Lock()
		defer mu.Unlock()
		ids := make([]string, len(mems))
		for i, m := range mems {
			ids[i] = m.ID
			memMap[m.ID] = m
		}
		channels[name] = ids
	}

	collectScoredMemories := func(name string, scored []store.ScoredMemory) {
		mu.Lock()
		defer mu.Unlock()
		ids := make([]string, len(scored))
		scores := make([]float64, len(scored))
		for i, sm := range scored {
			ids[i] = sm.Memory.ID
			scores[i] = sm.Score
			memMap[sm.Memory.ID] = sm.Memory
		}
		channels[name] = ids
		channelScores[name] = &store.ChannelScores{IDs: ids, Scores: scores}
	}

	collectMemoryIDs := func(name string, ids []string) {
		mu.Lock()
		defer mu.Unlock()
		channels[name] = ids
	}

	// WaitGroup for all channels. Errors are logged, never propagated.
	var wg sync.WaitGroup

	// ── Channel 1: BM25 ───────────────────────────────────────────────────
	wg.Add(1)
	go func() {
		defer wg.Done()
		chCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		// Check context before doing work — fail fast if parent was cancelled.
		select {
		case <-chCtx.Done():
			logRecallChannelError("bm25", chCtx.Err())
			return
		default:
		}

		if useConvex {
			// ConvexMerge needs raw BM25 scores for magnitude-aware fusion.
			scored, err := s.store.SearchMemoriesWithScoresCtx(chCtx, query, channelLimit, includeStale)
			if err != nil {
				logRecallChannelError("bm25", err)
				return
			}
			collectScoredMemories("bm25", scored)
		} else {
			var mems []store.Memory
			var err error
			if includeStale {
				mems, err = s.store.SearchMemoriesIncludingStaleCtx(chCtx, query, channelLimit)
			} else {
				mems, err = s.store.SearchMemoriesCtx(chCtx, query, channelLimit)
			}
			if err != nil {
				logRecallChannelError("bm25", err)
				return
			}
			collectMemories("bm25", mems)
		}
	}()

	// ── Channel 2: Semantic ───────────────────────────────────────────────
	if s.memoryEmbedder != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			queryVec, embedErr := s.memoryEmbedder.Embed(chCtx, query)
			if embedErr != nil || len(queryVec) == 0 {
				if embedErr != nil {
					logRecallChannelError("semantic", embedErr)
				}
				return
			}

			// Sprint 10.7: stale embeddings (entity changed) participate in
			// scoring — their vector is still valid (memory text unchanged).
			// MemoryVectorSearchWithThreshold no longer filters e.stale=0;
			// stale results carry StaleEmbedding=true for agent annotation.
			vecResults, vecErr := s.store.MemoryVectorSearchWithThresholdCtx(chCtx, queryVec, channelLimit, 0.3)
			if vecErr != nil {
				logRecallChannelError("semantic", vecErr)
				return
			}

			// Hydrate full Memory structs for results, preserving cosine-similarity order.
			// Track stale embedding IDs for the recall response annotation.
			ids := make([]string, len(vecResults))
			cosineScores := make([]float64, len(vecResults))
			for i, vr := range vecResults {
				ids[i] = vr.MemoryID
				cosineScores[i] = vr.Score
				if vr.StaleEmbedding {
					mu.Lock()
					staleEmbeddings[vr.MemoryID] = true
					mu.Unlock()
				}
			}
			if len(ids) > 0 {
				fullMems, hydErr := s.store.GetMemoriesByIDsCtx(chCtx, ids)
				if hydErr != nil {
					logRecallChannelError("semantic", hydErr)
					// Fall back to IDs only — RRF can still rank them.
					collectMemoryIDs("semantic", ids)
					// In convex mode, still populate channel scores from the
					// cosine similarities we already have. The scores are valid
					// even though we couldn't hydrate full Memory structs.
					if useConvex {
						mu.Lock()
						channelScores["semantic"] = &store.ChannelScores{
							IDs:    ids,
							Scores: cosineScores,
						}
						mu.Unlock()
					}
					return
				}
				// GetMemoriesByIDs returns in arbitrary SQL order.
				// Re-sort to match the original cosine-similarity ranking
				// so RRF assigns correct rank positions.
				memByID := make(map[string]store.Memory, len(fullMems))
				for _, m := range fullMems {
					memByID[m.ID] = m
				}
				ordered := make([]store.Memory, 0, len(ids))
				orderedScores := make([]float64, 0, len(ids))
				for idx, id := range ids {
					if m, ok := memByID[id]; ok {
						ordered = append(ordered, m)
						orderedScores = append(orderedScores, cosineScores[idx])
					}
				}
				collectMemories("semantic", ordered)
				if useConvex {
					mu.Lock()
					cs := &store.ChannelScores{
						IDs:    make([]string, len(ordered)),
						Scores: orderedScores,
					}
					for i, m := range ordered {
						cs.IDs[i] = m.ID
					}
					channelScores["semantic"] = cs
					mu.Unlock()
				}
			}
		}()
	}

	// ── Channel 3: Graph ──────────────────────────────────────────────────
	if s.graph != nil && s.store != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			select {
			case <-chCtx.Done():
				logRecallChannelError("graph", chCtx.Err())
				return
			default:
			}

			// Step 1: Find anchor nodes of memories matching the query.
			seedNodes, err := s.store.GetAnchorNodesByFTSQueryCtx(chCtx, query, 50)
			if err != nil {
				logRecallChannelError("graph", err)
				return
			}
			graphSeedCount = len(seedNodes)
			if len(seedNodes) == 0 {
				return // no anchor nodes match query — graph channel empty
			}

			// Step 2: BFS from seed nodes at configured depth.
			// graphBFS now returns a GraphBFSResult with parent map for path reconstruction.
			bfsRes := s.graphBFS(seedNodes, depth)
			graphResult = bfsRes
			if len(bfsRes.Nodes) == 0 {
				return
			}

			// Step 3: Find memories anchored to reachable nodes.
			mems, err := s.store.GetMemoriesByAnchorNodesCtx(chCtx, bfsRes.Nodes, channelLimit, includeStale)
			if err != nil {
				logRecallChannelError("graph", err)
				return
			}

			// Step 4: Sort memories by anchor node activation so that memories
			// attached to high-activation nodes get better RRF rank positions.
			// Uses MAX activation across all of a memory's anchors — a memory
			// anchored to both a high- and a low-activation node should rank high.
			// Also caches the first-anchor map for traversal info (avoids a second DB call).
			//
			// maxAct is scoped outside the nested if so ConvexMerge can use it.
			maxAct := make(map[string]float64, len(mems))
			if len(mems) > 0 && len(bfsRes.ActivationMap) > 0 {
				memIDs := make([]string, len(mems))
				for i, m := range mems {
					memIDs[i] = m.ID
				}
				bfsNodeSet := make(map[string]bool, len(bfsRes.Nodes))
				for _, nid := range bfsRes.Nodes {
					bfsNodeSet[nid] = true
				}
				// Fetch all anchors per memory for accurate max-activation scoring.
				allAnchors, allErr := s.store.GetAllMemoryAnchorNodeIDsInSet(memIDs, bfsNodeSet)
				if allErr != nil {
					logRecallChannelError("graph-activation", allErr)
					// Sorting fails gracefully — fall through with unsorted memories.
				} else if len(allAnchors) > 0 {
					// Pre-compute max activation score per memory.
					for memID, nodeIDs := range allAnchors {
						best := 0.0
						for _, nid := range nodeIDs {
							if a := bfsRes.ActivationMap[nid]; a > best {
								best = a
							}
						}
						maxAct[memID] = best
					}
					sort.Slice(mems, func(i, j int) bool {
						return maxAct[mems[i].ID] > maxAct[mems[j].ID]
					})
					// Cache first-anchor map for traversal info (avoids a second DB call).
					// Traversal path reconstruction uses one anchor per memory — any
					// anchor in the BFS set is valid for path tracing.
					firstAnchor := make(map[string]string, len(allAnchors))
					for memID, nodeIDs := range allAnchors {
						if len(nodeIDs) > 0 {
							firstAnchor[memID] = nodeIDs[0]
						}
					}
					graphAnchorMap = firstAnchor
				}
			}

			collectMemories("graph", mems)
			if useConvex && len(mems) > 0 && len(maxAct) > 0 {
				// Use activation scores as raw graph channel scores.
				// Only emit when maxAct is populated — otherwise all scores
				// are 0.0 which normalizes to all-1.0, giving every graph
				// result misleading maximum weight.
				hasNonZero := false
				mu.Lock()
				cs := &store.ChannelScores{
					IDs:    make([]string, len(mems)),
					Scores: make([]float64, len(mems)),
				}
				for i, m := range mems {
					cs.IDs[i] = m.ID
					cs.Scores[i] = maxAct[m.ID]
					if cs.Scores[i] > 0 {
						hasNonZero = true
					}
				}
				if hasNonZero {
					channelScores["graph"] = cs
				}
				mu.Unlock()
			}
		}()
	}

	// ── Channel 4: Temporal ───────────────────────────────────────────────
	// Temporal channel uses a lower limit than other channels. It returns
	// unfiltered recent memories (no text match), so with channelLimit it
	// would flood RRF with irrelevant noise. Capping at `limit` ensures
	// temporal only fills gaps when other channels don't have enough results.
	temporalLimit := limit
	if temporalLimit < 5 {
		temporalLimit = 5
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		chCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		select {
		case <-chCtx.Done():
			logRecallChannelError("temporal", chCtx.Err())
			return
		default:
		}

		mems, err := s.store.RecentMemoriesCtx(chCtx, temporalLimit, sinceDays, untilTime, includeStale)
		if err != nil {
			logRecallChannelError("temporal", err)
			return
		}

		// Re-rank by recency decay score (most recent first is already the order,
		// but we assign explicit rank positions for RRF).
		// The order from RecentMemories is already created_at DESC, which is
		// equivalent to recency-first ranking. No re-sort needed.
		collectMemories("temporal", mems)
		if useConvex && len(mems) > 0 {
			// Use recency decay as raw temporal channel scores.
			mu.Lock()
			cs := &store.ChannelScores{
				IDs:    make([]string, len(mems)),
				Scores: make([]float64, len(mems)),
			}
			for i, m := range mems {
				cs.IDs[i] = m.ID
				cs.Scores[i] = store.DecayedImportanceScore(m, 0)
			}
			channelScores["temporal"] = cs
			mu.Unlock()
		}
	}()

	wg.Wait()

	// ── Merge ────────────────────────────────────────────────────────────
	if len(channels) == 0 {
		return nil, nil, nil, nil
	}

	var rankedIDs []string
	var attribution map[string][]string

	if useConvex && len(channelScores) > 0 {
		// Score-aware convex fusion: uses score magnitudes, not just ranks.
		cw := store.DefaultConvexWeights
		if cfg.Recall.ConvexAlpha != nil {
			cw.Alpha = clampUnit(*cfg.Recall.ConvexAlpha)
		}
		if cfg.Recall.ConvexGraphBonus != nil {
			cw.GraphBonus = clampUnit(*cfg.Recall.ConvexGraphBonus)
		}
		if cfg.Recall.ConvexTemporalBonus != nil {
			cw.TemporalBonus = clampUnit(*cfg.Recall.ConvexTemporalBonus)
		}
		rankedIDs, attribution = store.ConvexMerge(channelScores, limit, cw)
	} else {
		// Default: Reciprocal Rank Fusion (rank-only).
		rankedIDs, attribution = store.RRFMergeWeighted(channels, limit, 60, store.DefaultRRFWeights)
	}

	if len(rankedIDs) == 0 {
		return nil, nil, nil, nil
	}

	// Hydrate results from memMap. Any missing IDs (from semantic fallback
	// path) are fetched from store.
	var missingIDs []string
	for _, id := range rankedIDs {
		if _, ok := memMap[id]; !ok {
			missingIDs = append(missingIDs, id)
		}
	}
	if len(missingIDs) > 0 {
		fetched, err := s.store.GetMemoriesByIDsCtx(ctx, missingIDs)
		if err != nil {
			logRecallChannelError("hydration", err)
		} else {
			mu.Lock()
			for _, m := range fetched {
				memMap[m.ID] = m
			}
			mu.Unlock()
		}
	}

	// Build final ordered result, applying decay visibility threshold.
	// Memories with decayed importance below the threshold are demoted (excluded)
	// unless they are pinned. Pinned memories always score 1.0 and are never removed.
	result := make([]store.Memory, 0, len(rankedIDs))
	for _, id := range rankedIDs {
		if m, ok := memMap[id]; ok {
			if store.DecayedImportanceScore(m, 0) >= store.DecayVisibilityThreshold {
				result = append(result, m)
			}
		}
	}

	// Sprint 10.7: collect stale embedding IDs that survived RRF merge AND decay filter.
	// Only include IDs that are in the final result set (post-decay-threshold).
	var staleEmbIDs []string
	for _, m := range result {
		if staleEmbeddings[m.ID] {
			staleEmbIDs = append(staleEmbIDs, m.ID)
		}
	}

	// ── Sprint 10.8: Graph traversal info ─────────────────────────────────
	// Build traversal info only when the graph channel was active (seedCount > 0).
	// Path reconstruction: for each result memory attributed to the graph channel,
	// trace the BFS parent map from its anchor node back to the seed, producing
	// a human-readable path like "AuthService -[CALLS]- TokenValidator".
	var traversalInfo *GraphTraversalInfo
	if graphSeedCount > 0 {
		ti := &GraphTraversalInfo{
			Depth:        depth,
			AnchorCount:  graphSeedCount,
			VisitedNodes: len(graphResult.Nodes),
			Note: "Paths show directed code relationships from query-matching entities to " +
				"memory anchor points. →[EDGE]→ means left calls/imports/implements right. " +
				"←[EDGE]- means right calls/imports/implements left. BFS explores both " +
				"directions from seed entities.",
		}

		// Collect memory IDs attributed to the graph channel in the final result.
		graphMemIDs := graphAttributedMemIDs(result, attribution)
		if len(graphMemIDs) > 0 && len(graphResult.ParentMap) > 0 {
			// Reuse the anchorMap populated during activation sort in the graph
			// channel goroutine. graphAnchorMap covers all graph channel memories
			// (a superset of graphMemIDs), so all lookups succeed.
			// Fall back to a fresh DB query only when the activation sort was
			// skipped (e.g., anchorErr in the goroutine or empty mems).
			anchorMap := graphAnchorMap
			if len(anchorMap) == 0 {
				// Fallback: build nodeSet and re-query with the narrower graphMemIDs.
				bfsNodeSet := make(map[string]bool, len(graphResult.ParentMap))
				for nid := range graphResult.ParentMap {
					bfsNodeSet[nid] = true
				}
				var dbErr error
				anchorMap, dbErr = s.store.GetMemoryAnchorNodeIDsInSet(graphMemIDs, bfsNodeSet)
				if dbErr != nil {
					logRecallChannelError("graph-traversal", dbErr)
					anchorMap = nil
				}
			}
			if len(anchorMap) > 0 {
				ti.Paths = s.reconstructTraversalPaths(result, attribution, graphResult, anchorMap)
			}
		}
		traversalInfo = ti
	}

	return result, attribution, staleEmbIDs, traversalInfo
}

// graphAttributedMemIDs returns memory IDs from result that have "graph" in their attribution.
func graphAttributedMemIDs(result []store.Memory, attribution map[string][]string) []string {
	var ids []string
	for _, m := range result {
		for _, ch := range attribution[m.ID] {
			if ch == "graph" {
				ids = append(ids, m.ID)
				break
			}
		}
	}
	return ids
}

// reconstructTraversalPaths builds per-memory path explanations for memories
// surfaced by the graph channel. Skips memories whose anchor is itself a seed
// (those are found by BM25 directly — no interesting multi-hop to show).
func (s *Server) reconstructTraversalPaths(
	mems []store.Memory,
	attribution map[string][]string,
	bfsResult GraphBFSResult,
	anchorMap map[string]string, // memID → first anchor nodeID
) []TraversalPath {
	var paths []TraversalPath
	for _, m := range mems {
		// Only build paths for memories attributed to the graph channel.
		graphContributed := false
		for _, ch := range attribution[m.ID] {
			if ch == "graph" {
				graphContributed = true
				break
			}
		}
		if !graphContributed {
			continue
		}

		anchorNodeID, ok := anchorMap[m.ID]
		if !ok {
			continue
		}

		pathStr, hops := s.buildGraphPath(anchorNodeID, bfsResult)
		if hops == 0 {
			continue // anchor is a seed — no multi-hop path to show
		}

		// Truncate at rune boundary to avoid splitting multi-byte UTF-8 characters.
		snippet := m.Content
		runes := []rune(snippet)
		if len(runes) > 80 {
			snippet = string(runes[:77]) + "..."
		}

		paths = append(paths, TraversalPath{
			MemoryID:      m.ID,
			MemorySnippet: snippet,
			Path:          pathStr,
			Hops:          hops,
		})
	}
	return paths
}

// buildGraphPath reconstructs the directed traversal path from a seed to the given
// anchor node using the BFS parent map. Returns the path string and hop count.
// Returns ("", 0) when:
//   - the anchor is itself a seed (no interesting path),
//   - the path cannot be traced back to a seed within maxIter hops,
//   - the parent map is empty.
//
// Path format uses directed arrows:
//
//	→[CALLS]→  means left node calls/imports/implements right node
//	←[CALLS]-  means right node calls/imports/implements left node
func (s *Server) buildGraphPath(nodeID string, bfsResult GraphBFSResult) (string, int) {
	const maxIter = 8

	if bfsResult.SeedSet[nodeID] {
		return "", 0 // anchor is a seed — surfaced by BM25 directly
	}

	type segment struct {
		nodeID     string
		edgeType   graph.EdgeType
		isIncoming bool // true = nodeID CALLS parentNodeID (inverse of BFS traversal)
	}

	// Trace from anchor back toward seed, collecting segments.
	// segs is built in anchor→seed order; reversed before display.
	var segs []segment
	current := nodeID
	for i := 0; i < maxIter; i++ {
		entry, ok := bfsResult.ParentMap[current]
		if !ok {
			// Can't trace further — path is incomplete (anchor not in BFS result).
			return "", 0
		}
		segs = append(segs, segment{
			nodeID:     current,
			edgeType:   entry.EdgeType,
			isIncoming: entry.IsIncoming,
		})
		current = entry.ParentNodeID
		if bfsResult.SeedSet[current] {
			break
		}
	}

	if len(segs) == 0 {
		return "", 0
	}
	// Verify we actually reached a seed (not just hit maxIter).
	if !bfsResult.SeedSet[current] {
		return "", 0
	}

	// Reverse to get seed → anchor order for display.
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}

	// Build directed path string starting from seed (current).
	// For each segment in seed→anchor order:
	//   IsIncoming=false: parentNodeID →[EDGE]→ nodeID (parent calls nodeID)
	//   IsIncoming=true:  nodeID ←[EDGE]- parentNodeID is WRONG order.
	//     After reversal, the segment's parent is the seed-side.
	//     IsIncoming=true means nodeID CALLS parentNodeID — so the arrow points
	//     from nodeID toward parentNodeID (the seed side), shown as ←[EDGE]-.
	var sb strings.Builder
	sb.WriteString(s.graphNodeName(current))
	for _, seg := range segs {
		if !seg.isIncoming {
			// Outgoing: seed-side parent CALLS/IMPORTS anchor-side nodeID
			sb.WriteString(" →[")
			sb.WriteString(string(seg.edgeType))
			sb.WriteString("]→ ")
		} else {
			// Incoming: anchor-side nodeID CALLS seed-side parent
			// Arrow points right-to-left from the agent's perspective
			sb.WriteString(" ←[")
			sb.WriteString(string(seg.edgeType))
			sb.WriteString("]- ")
		}
		sb.WriteString(s.graphNodeName(seg.nodeID))
	}
	return sb.String(), len(segs)
}

// graphNodeName returns the display name for a node ID.
// Falls back to extracting the entity name from the "repoID::file::EntityName"
// node ID format if the node is not present in the graph.
func (s *Server) graphNodeName(nodeID string) string {
	if s.graph != nil {
		if n := s.graph.GetNode(graph.NodeID(nodeID)); n != nil {
			return n.Name
		}
	}
	// Fallback: extract last segment from "::" separated node ID.
	if idx := strings.LastIndex(nodeID, "::"); idx >= 0 {
		return nodeID[idx+2:]
	}
	return nodeID
}

// edgeActivationWeight returns the spreading activation multiplier for the given
// edge type. CALLS (direct invocation) propagates full activation. IMPLEMENTS
// (structural contract) propagates 70%. IMPORTS (package-level dependency)
// propagates 50%. Unknown types are given 40% — conservative, as their
// semantic coupling is weaker.
//
// Based on Anderson (1983) spreading activation theory: tighter coupling
// between nodes means more shared cognitive context, so more activation flows.
func edgeActivationWeight(t graph.EdgeType) float64 {
	switch t {
	case graph.EdgeCalls:
		return 1.0
	case graph.EdgeImplements:
		return 0.7
	case graph.EdgeImports:
		return 0.5
	default:
		return 0.4
	}
}

// graphBFS performs breadth-first search from seed node IDs following
// CALLS, IMPORTS, and IMPLEMENTS edges. Returns a GraphBFSResult containing
// reachable node IDs (excluding seeds), the parent map for path reconstruction,
// the seed set for termination detection, and an activation map.
//
// Spreading activation (Anderson 1983): seeds start at 1.0. Each hop:
//
//	child_act = parent_act × edgeActivationWeight(type) / max(1, fan_out)
//
// fan_out is the count of allowed-type edges on the parent (both directions).
// Nodes reachable via multiple paths keep the maximum activation.
//
// Edge type filtering by depth:
//   - Depth 1: CALLS + IMPORTS + IMPLEMENTS (broad discovery)
//   - Depth 2+: CALLS only (focused structural relationship)
//
// Capped at 500 total reachable nodes to prevent explosion from hub nodes.
// maxDepth is configurable to support depth= parameter; 0 defaults to 2.
func (s *Server) graphBFS(seeds []string, maxDepth int) GraphBFSResult {
	if maxDepth <= 0 {
		maxDepth = 2
	}

	const maxNodes = 500

	visited := make(map[graph.NodeID]bool, len(seeds))
	frontier := make([]graph.NodeID, 0, len(seeds))
	parentMap := make(map[string]graphParentEntry, 64)
	seedSet := make(map[string]bool, len(seeds))

	// activationMap: seeds = 1.0; non-seeds = max activation across all paths.
	activationMap := make(map[string]float64, len(seeds)+64)

	for _, seed := range seeds {
		seedSet[seed] = true
		nid := graph.NodeID(seed)
		if s.graph.GetNode(nid) != nil && !visited[nid] {
			visited[nid] = true
			activationMap[seed] = 1.0
			frontier = append(frontier, nid)
		}
	}

	// Allowed edge types per depth level.
	depth1Types := map[graph.EdgeType]bool{
		graph.EdgeCalls:      true,
		graph.EdgeImports:    true,
		graph.EdgeImplements: true,
	}
	depth2Types := map[graph.EdgeType]bool{
		graph.EdgeCalls: true,
	}

	for d := 1; d <= maxDepth; d++ {
		if len(visited) >= maxNodes {
			break
		}

		allowedTypes := depth1Types
		if d >= 2 {
			allowedTypes = depth2Types
		}

		var nextFrontier []graph.NodeID
		for _, nid := range frontier {
			parentActivation := activationMap[string(nid)]

			// Fetch edges once for both fan-out counting and traversal.
			outEdges := s.graph.OutEdges(nid)
			inEdges := s.graph.InEdges(nid)

			// fan-out: count of allowed-type edges in both directions.
			// Divides parent activation so hub nodes don't flood neighbors.
			fanOut := 0
			for _, e := range outEdges {
				if allowedTypes[e.Type] {
					fanOut++
				}
			}
			for _, e := range inEdges {
				if allowedTypes[e.Type] {
					fanOut++
				}
			}
			if fanOut < 1 {
				fanOut = 1
			}

			// Follow outgoing edges (callees, imports, implements).
			// IsIncoming=false: nid (parent) CALLS/IMPORTS/IMPLEMENTS e.To.
			for _, e := range outEdges {
				if !allowedTypes[e.Type] {
					continue
				}
				act := parentActivation * edgeActivationWeight(e.Type) / float64(fanOut)
				// Update activation unconditionally — nodes reachable via
				// multiple paths keep the maximum.
				if act > activationMap[string(e.To)] {
					activationMap[string(e.To)] = act
				}
				if visited[e.To] {
					continue
				}
				visited[e.To] = true
				parentMap[string(e.To)] = graphParentEntry{
					ParentNodeID: string(nid),
					EdgeType:     e.Type,
					IsIncoming:   false,
				}
				nextFrontier = append(nextFrontier, e.To)
				if len(visited) >= maxNodes {
					break
				}
			}
			if len(visited) >= maxNodes {
				break
			}
			// Follow incoming edges (callers of this node).
			// IsIncoming=true: e.From CALLS nid — e.From is the caller, nid the callee.
			for _, e := range inEdges {
				if !allowedTypes[e.Type] {
					continue
				}
				act := parentActivation * edgeActivationWeight(e.Type) / float64(fanOut)
				if act > activationMap[string(e.From)] {
					activationMap[string(e.From)] = act
				}
				if visited[e.From] {
					continue
				}
				visited[e.From] = true
				parentMap[string(e.From)] = graphParentEntry{
					ParentNodeID: string(nid),
					EdgeType:     e.Type,
					IsIncoming:   true,
				}
				nextFrontier = append(nextFrontier, e.From)
				if len(visited) >= maxNodes {
					break
				}
			}
			if len(visited) >= maxNodes {
				break
			}
		}
		frontier = nextFrontier
	}

	// Build result slice — exclude seed nodes (their memories are covered by BM25).
	result := make([]string, 0, len(visited))
	for nid := range visited {
		if !seedSet[string(nid)] {
			result = append(result, string(nid))
		}
	}

	return GraphBFSResult{
		Nodes:         result,
		ParentMap:     parentMap,
		SeedSet:       seedSet,
		ActivationMap: activationMap,
	}
}

// clampUnit clamps a float64 to the [0, 1] range.
// Used for config validation of ConvexWeights fields.
func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// logRecallChannelError logs a non-fatal error from a recall channel.
// Channel errors never fail the entire recall — other channels continue.
func logRecallChannelError(channel string, err error) {
	logutil.Warn("synapses: recall %s channel error: %v\n", channel, err)
}

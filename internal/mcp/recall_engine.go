package mcp

import (
	"context"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/store"
)

// quadRecallSearch runs 4 parallel retrieval channels and merges via RRF.
// Returns memories ranked by fused score, plus per-memory channel attribution.
//
// Channels:
//  1. BM25 — FTS5 full-text search on memory content (existing)
//  2. Semantic — cosine similarity on embeddings (existing, skipped if no embedder)
//  3. Graph — BFS from anchor entities of query-matching memories (skipped if no graph)
//  4. Temporal — recent memories scored by recency decay (no text filter)
//
// Each channel runs in its own goroutine with a 5s timeout.
// Channel errors are logged but never fail the entire recall.
// The response shape is unchanged — episodes are searched separately by the caller.
func (s *Server) quadRecallSearch(
	ctx context.Context,
	query string,
	limit int,
	includeStale bool,
	sinceDays int,
) ([]store.Memory, map[string][]string) {
	if limit <= 0 {
		limit = 5
	}

	// Per-channel limit: request more than final limit so RRF has enough
	// candidates from each channel to produce meaningful fusion.
	channelLimit := limit * 3
	if channelLimit < 10 {
		channelLimit = 10
	}

	var (
		mu       sync.Mutex
		channels = make(map[string][]string)
		memMap   = make(map[string]store.Memory) // id → full memory for hydration
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
		_ = chCtx // context for future cancellation awareness

		var mems []store.Memory
		var err error
		if includeStale {
			mems, err = s.store.SearchMemoriesIncludingStale(query, channelLimit)
		} else {
			mems, err = s.store.SearchMemories(query, channelLimit)
		}
		if err != nil {
			logRecallChannelError("bm25", err)
			return
		}
		collectMemories("bm25", mems)
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

			vecResults, vecErr := s.store.MemoryVectorSearchWithThreshold(queryVec, channelLimit, 0.3)
			if vecErr != nil {
				logRecallChannelError("semantic", vecErr)
				return
			}

			// Hydrate full Memory structs for results, preserving cosine-similarity order.
			ids := make([]string, len(vecResults))
			for i, vr := range vecResults {
				ids[i] = vr.MemoryID
			}
			if len(ids) > 0 {
				fullMems, hydErr := s.store.GetMemoriesByIDs(ids)
				if hydErr != nil {
					logRecallChannelError("semantic", hydErr)
					// Fall back to IDs only — RRF can still rank them.
					collectMemoryIDs("semantic", ids)
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
				for _, id := range ids {
					if m, ok := memByID[id]; ok {
						ordered = append(ordered, m)
					}
				}
				collectMemories("semantic", ordered)
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
			_ = chCtx

			// Step 1: Find anchor nodes of memories matching the query.
			seedNodes, err := s.store.GetAnchorNodesByFTSQuery(query, 50)
			if err != nil {
				logRecallChannelError("graph", err)
				return
			}
			if len(seedNodes) == 0 {
				return // no anchor nodes match query — graph channel empty
			}

			// Step 2: BFS 2 hops from seed nodes.
			reachableNodes := s.graphBFS(seedNodes, 2)
			if len(reachableNodes) == 0 {
				return
			}

			// Step 3: Find memories anchored to reachable nodes.
			mems, err := s.store.GetMemoriesByAnchorNodes(reachableNodes, channelLimit, includeStale)
			if err != nil {
				logRecallChannelError("graph", err)
				return
			}
			collectMemories("graph", mems)
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
		_ = chCtx

		mems, err := s.store.RecentMemories(temporalLimit, sinceDays, includeStale)
		if err != nil {
			logRecallChannelError("temporal", err)
			return
		}

		// Re-rank by recency decay score (most recent first is already the order,
		// but we assign explicit rank positions for RRF).
		// The order from RecentMemories is already created_at DESC, which is
		// equivalent to recency-first ranking. No re-sort needed.
		collectMemories("temporal", mems)
	}()

	wg.Wait()

	// ── RRF Merge ─────────────────────────────────────────────────────────
	if len(channels) == 0 {
		return nil, nil
	}

	rankedIDs, attribution := store.RRFMergeWeighted(channels, limit, 60, store.DefaultRRFWeights)
	if len(rankedIDs) == 0 {
		return nil, nil
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
		fetched, err := s.store.GetMemoriesByIDs(missingIDs)
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

	// Build final ordered result.
	result := make([]store.Memory, 0, len(rankedIDs))
	for _, id := range rankedIDs {
		if m, ok := memMap[id]; ok {
			result = append(result, m)
		}
	}

	return result, attribution
}

// graphBFS performs breadth-first search from seed node IDs following
// CALLS, IMPORTS, and IMPLEMENTS edges. Returns all unique reachable node IDs.
//
// Edge type filtering by depth:
//   - Depth 1: CALLS + IMPORTS + IMPLEMENTS (broad discovery)
//   - Depth 2: CALLS only (focused structural relationship)
//
// Capped at 500 total reachable nodes to prevent explosion from hub nodes.
// maxDepth is configurable to support Sprint 10 #8 (multi-hop traversal).
func (s *Server) graphBFS(seeds []string, maxDepth int) []string {
	if maxDepth <= 0 {
		maxDepth = 2
	}

	const maxNodes = 500

	visited := make(map[graph.NodeID]bool, len(seeds))
	frontier := make([]graph.NodeID, 0, len(seeds))

	for _, seed := range seeds {
		nid := graph.NodeID(seed)
		if s.graph.GetNode(nid) != nil && !visited[nid] {
			visited[nid] = true
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

	for depth := 1; depth <= maxDepth; depth++ {
		if len(visited) >= maxNodes {
			break
		}

		allowedTypes := depth1Types
		if depth >= 2 {
			allowedTypes = depth2Types
		}

		var nextFrontier []graph.NodeID
		for _, nid := range frontier {
			// Follow outgoing edges.
			for _, e := range s.graph.OutEdges(nid) {
				if !allowedTypes[e.Type] || visited[e.To] {
					continue
				}
				visited[e.To] = true
				nextFrontier = append(nextFrontier, e.To)
				if len(visited) >= maxNodes {
					break
				}
			}
			if len(visited) >= maxNodes {
				break
			}
			// Follow incoming edges (callers).
			for _, e := range s.graph.InEdges(nid) {
				if !allowedTypes[e.Type] || visited[e.From] {
					continue
				}
				visited[e.From] = true
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

	// Convert to string slice, excluding seed nodes (we want structurally
	// RELATED nodes, not the seeds themselves — the seeds' memories are
	// already covered by the BM25 channel).
	seedSet := make(map[string]bool, len(seeds))
	for _, s := range seeds {
		seedSet[s] = true
	}

	result := make([]string, 0, len(visited))
	for nid := range visited {
		if !seedSet[string(nid)] {
			result = append(result, string(nid))
		}
	}
	return result
}

// logRecallChannelError logs a non-fatal error from a recall channel.
// Channel errors never fail the entire recall — other channels continue.
func logRecallChannelError(channel string, err error) {
	logutil.Warn("synapses: recall %s channel error: %v\n", channel, err)
}

package watcher

// node_embed.go — background node embedding pass.
//
// After the initial parse completes, this pass embeds all graph nodes that
// don't yet have a vector in the node_embeddings table. This ensures the HNSW
// index is populated with code entity vectors BEFORE the NL resolver runs,
// making Tier 1 embedding-based entity resolution possible.
//
// After embedding completes, runs post-embed discovery passes:
//   - DiscoverDocCodeRelations: semantic doc↔code linking via embedding similarity
//   - DiscoverEmbedRelations: knowledge-node relationship discovery
//   - DetectCommunities: label propagation over knowledge nodes
//
// Modeled after the memory embedding sweep in internal/mcp/server.go
// (embedSweepLoop / embedMemory).

import (
	"context"
	"time"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/store"
)

const (
	// nodeEmbedBatchSize is the number of un-embedded nodes fetched per iteration.
	nodeEmbedBatchSize = 500

	// nodeEmbedDelay is the sleep between individual embed calls to avoid
	// overloading the embedding server. Nodes are smaller than memories so
	// 50ms is sufficient (vs 100ms for memories).
	nodeEmbedDelay = 50 * time.Millisecond

	// nodeEmbedTimeout is the per-node budget for each Embed call.
	nodeEmbedTimeout = 5 * time.Second
)

// runNodeEmbedPass embeds all graph nodes that lack stored vectors, then
// triggers an HNSW rebuild and runs post-embed discovery passes (doc↔code
// linking, knowledge relations, community detection).
//
// Designed to run once in a background goroutine after initial parse completes.
// Safe to call multiple times; skips already-embedded nodes automatically
// (via the content_hash check in GetNodesWithoutEmbeddings).
//
// ctx is the daemon lifecycle context — cancelled on daemon shutdown.
// embedder and st must both be non-nil (callers should guard before calling).
// g may be nil — in that case post-embed discovery is skipped.
func runNodeEmbedPass(ctx context.Context, embedder embed.Embedder, st *store.Store, g *graph.Graph) {
	ids, err := st.GetNodesWithoutEmbeddings(nodeEmbedBatchSize)
	if err != nil {
		logutil.Warn("synapses/watcher: node embed pass: list nodes: %v\n", err)
		return
	}

	// Even when all nodes are embedded, we still run post-embed discovery
	// (below) to ensure semantic edges exist in the in-memory graph.
	if len(ids) == 0 {
		if g != nil {
			runPostEmbedDiscovery(ctx, embedder, st, g)
		}
		return
	}

	model := embedder.Model()
	embedded := 0

	for _, nodeID := range ids {
		// Respect daemon shutdown.
		select {
		case <-ctx.Done():
			return
		default:
		}

		text, ok := st.GetNodeTextForEmbedding(nodeID)
		if !ok || text == "" {
			continue
		}

		embedCtx, cancel := context.WithTimeout(ctx, nodeEmbedTimeout)
		vec, embedErr := embedder.Embed(embedCtx, text)
		cancel()
		if embedErr != nil {
			logutil.Warn("synapses/watcher: node embed: embed %s: %v\n", nodeID, embedErr)
			continue
		}
		if len(vec) == 0 {
			continue
		}

		if upsertErr := st.UpsertEmbedding(nodeID, model, vec); upsertErr != nil {
			logutil.Warn("synapses/watcher: node embed: upsert %s: %v\n", nodeID, upsertErr)
			continue
		}
		embedded++

		// Rate-limit to avoid saturating the embedding server.
		select {
		case <-ctx.Done():
			return
		case <-time.After(nodeEmbedDelay):
		}
	}

	if embedded > 0 {
		logutil.Info("synapses/watcher: node embed pass complete: %d nodes embedded\n", embedded)
		// Rebuild the HNSW node index so new vectors are immediately searchable.
		st.RebuildNodeHNSW()
	}

	// Post-embed discovery: run semantic linking passes now that HNSW is populated.
	// All three functions are idempotent — safe to re-run on repeated embed passes.
	// Skipped when graph is nil (e.g. standalone embed-only invocation).
	if g != nil {
		runPostEmbedDiscovery(ctx, embedder, st, g)
	}
}

// discoveryEdgeTypes are the edge types created by post-embed discovery passes.
// Used to collect and persist new edges to the store.
var discoveryEdgeTypes = map[graph.EdgeType]bool{
	graph.EdgeExplains:     true,
	graph.EdgeDocumentedBy: true,
	graph.EdgeRelatesTo:    true,
	graph.EdgeCausedBy:     true,
	graph.EdgeInstanceOf:   true,
	graph.EdgeContradicts:  true,
}

// runPostEmbedDiscovery runs the three semantic discovery passes that depend on
// populated HNSW embeddings: doc↔code linking, knowledge relations, communities.
// After creating edges in the in-memory graph, persists them to the store so
// they survive daemon restarts.
func runPostEmbedDiscovery(ctx context.Context, embedder embed.Embedder, st *store.Store, g *graph.Graph) {
	// Check for shutdown before starting potentially expensive passes.
	select {
	case <-ctx.Done():
		return
	default:
	}

	er := newStoreEmbedResolver(embedder, st)
	if er == nil {
		return
	}

	// Snapshot existing discovery edges before the passes run.
	existingEdges := make(map[[3]string]bool)
	for _, e := range g.AllEdges() {
		if discoveryEdgeTypes[e.Type] {
			existingEdges[[3]string{string(e.From), string(e.To), string(e.Type)}] = true
		}
	}

	// Phase 1: Semantic doc↔code linking — creates EXPLAINS/DOCUMENTED_BY edges
	// between doc sections and code entities based on embedding similarity.
	dcCount := resolver.DiscoverDocCodeRelations(g, er, 0.60)

	// Phase 2: Knowledge-node relationship discovery — creates RELATES_TO edges
	// between semantically similar knowledge nodes (concepts, entities, etc.).
	erCount := resolver.DiscoverEmbedRelations(g, er, 0.55)

	// Phase 3: Community detection — label propagation over knowledge nodes
	// to assign community IDs for downstream clustering/grouping.
	comCount := resolver.DetectCommunities(g, 10)

	if dcCount+erCount+comCount > 0 {
		logutil.Info("synapses/watcher: post-embed discovery: %d doc-code, %d relations, %d communities\n",
			dcCount, erCount, comCount)

		// Collect newly created edges and persist to SQLite so they survive restarts.
		var newEdges []graph.Edge
		for _, e := range g.AllEdges() {
			if discoveryEdgeTypes[e.Type] {
				key := [3]string{string(e.From), string(e.To), string(e.Type)}
				if !existingEdges[key] {
					newEdges = append(newEdges, *e)
				}
			}
		}
		if len(newEdges) > 0 {
			if err := st.SaveDiscoveryEdges(newEdges); err != nil {
				logutil.Warn("synapses/watcher: persist discovery edges: %v\n", err)
			} else {
				logutil.Info("synapses/watcher: persisted %d discovery edges\n", len(newEdges))
			}
		}
	}
}

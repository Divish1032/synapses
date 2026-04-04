package watcher

// node_embed.go — background node embedding pass.
//
// After the initial parse completes, this pass embeds all graph nodes that
// don't yet have a vector in the node_embeddings table. This ensures the HNSW
// index is populated with code entity vectors BEFORE the resolver runs,
// making Tier 1 embedding-based entity resolution possible.
//
// Modeled after the memory embedding sweep in internal/mcp/server.go
// (embedSweepLoop / embedMemory).

import (
	"context"
	"time"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/pulse"
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
// triggers an HNSW rebuild.
//
// Designed to run once in a background goroutine after initial parse completes.
// Safe to call multiple times; skips already-embedded nodes automatically
// (via the content_hash check in GetNodesWithoutEmbeddings).
//
// ctx is the daemon lifecycle context — cancelled on daemon shutdown.
// embedder and st must both be non-nil (callers should guard before calling).
func runNodeEmbedPass(ctx context.Context, embedder embed.Embedder, st *store.Store, pc *pulse.Client) {
	start := time.Now()
	ids, err := st.GetNodesWithoutEmbeddings(nodeEmbedBatchSize)
	if err != nil {
		logutil.Warn("synapses/watcher: node embed pass: list nodes: %v\n", err)
		return
	}

	logutil.Info("synapses/watcher: node embed pass: %d nodes need embedding (model=%s)\n", len(ids), embedder.Model())

	if len(ids) == 0 {
		return
	}

	model := embedder.Model()
	embedded := 0
	embedErrors := 0

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
			embedErrors++
			continue
		}
		if len(vec) == 0 {
			continue
		}

		if upsertErr := st.UpsertEmbedding(nodeID, model, vec); upsertErr != nil {
			logutil.Warn("synapses/watcher: node embed: upsert %s: %v\n", nodeID, upsertErr)
			embedErrors++
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

	// Emit pulse event for the node embed pass.
	if pc != nil {
		var poolContention int
		if be, ok := embedder.(*embed.BuiltinEmbedder); ok {
			poolContention = int(be.PoolContentions.Load())
		}
		pc.RecordEmbeddingEvent(pulse.EmbeddingEvent{
			Trigger:             "node_embed_pass",
			Count:               embedded,
			Errors:              embedErrors,
			DurationMs:          time.Since(start).Milliseconds(),
			Model:               model,
			Success:             embedErrors == 0,
			StaleCount:          len(ids) - embedded - embedErrors,
			EmbedPoolContention: poolContention,
		})
	}

}

package watcher

// embed_resolver.go — storeEmbedResolver adapts embed.Embedder + store.Store to
// satisfy the resolver.EmbedResolver interface. This keeps the resolver package
// free of direct dependencies on store or embed.

import (
	"context"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/store"
)

// storeEmbedResolver implements resolver.EmbedResolver using the store's HNSW
// node index (via VectorSearch) and an embed.Embedder for query embedding.
type storeEmbedResolver struct {
	embedder embed.Embedder
	st       *store.Store
}

// newStoreEmbedResolver returns a storeEmbedResolver or nil if either argument
// is nil (safe to pass nil er to resolver functions — they degrade gracefully).
func newStoreEmbedResolver(e embed.Embedder, st *store.Store) resolver.EmbedResolver {
	if e == nil || st == nil {
		return nil
	}
	return &storeEmbedResolver{embedder: e, st: st}
}

// EmbedText implements resolver.EmbedResolver.
func (r *storeEmbedResolver) EmbedText(ctx context.Context, text string) ([]float32, error) {
	return r.embedder.Embed(ctx, text)
}

// SearchByVector implements resolver.EmbedResolver.
// Delegates to store.VectorSearch which uses HNSW when the index is populated
// or falls back to brute-force. Returns nil when the index has no vectors yet.
func (r *storeEmbedResolver) SearchByVector(queryVec []float32, k int) []resolver.EmbedMatch {
	results, err := r.st.VectorSearch(queryVec, k)
	if err != nil || len(results) == 0 {
		return nil
	}
	out := make([]resolver.EmbedMatch, len(results))
	for i, sr := range results {
		out[i] = resolver.EmbedMatch{
			NodeID: sr.ID,
			Score:  sr.Score,
		}
	}
	return out
}

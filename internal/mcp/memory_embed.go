package mcp

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/store"
)

// embedMemory generates a vector embedding for a single memory and stores it.
// Fail-silent: errors are logged to stderr but never propagated to callers.
// This is designed to be called from goroutines on the write path.
func (s *Server) embedMemory(embedder embed.Embedder, st *store.Store, memoryID, content string) {
	if embedder == nil || st == nil || content == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vec, err := embedder.Embed(ctx, content)
	if err != nil {
		fmt.Fprintf(os.Stderr, "synapses: embed memory %s: %v\n", memoryID, err)
		return
	}
	if len(vec) == 0 {
		return
	}
	if err := st.UpsertMemoryEmbedding(memoryID, embedder.Model(), vec); err != nil {
		fmt.Fprintf(os.Stderr, "synapses: store memory embedding %s: %v\n", memoryID, err)
	}
}

// EmbedAllMemories generates embeddings for all un-embedded memories in the
// background. Rate-limited to avoid CPU contention. Called at startup and
// periodically to lazy-migrate legacy memories.
func EmbedAllMemories(ctx context.Context, embedder embed.Embedder, st *store.Store) {
	if embedder == nil || st == nil {
		return
	}

	ids, err := st.GetMemoriesWithoutEmbeddings(0) // 0 = no limit
	if err != nil || len(ids) == 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "synapses: embedding %d memories (model: %s) …\n", len(ids), embedder.Model())
	done := 0

	for _, memID := range ids {
		select {
		case <-ctx.Done():
			return
		default:
		}

		text, ok := st.GetMemoryTextForEmbedding(memID)
		if !ok || text == "" {
			continue
		}

		embedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		vec, embedErr := embedder.Embed(embedCtx, text)
		cancel()

		if embedErr != nil {
			fmt.Fprintf(os.Stderr, "synapses: embed memory %s: %v\n", memID, embedErr)
			continue
		}
		if len(vec) == 0 {
			continue
		}
		if err := st.UpsertMemoryEmbedding(memID, embedder.Model(), vec); err != nil {
			fmt.Fprintf(os.Stderr, "synapses: store memory embedding %s: %v\n", memID, err)
		}
		done++
	}

	if done > 0 {
		fmt.Fprintf(os.Stderr, "synapses: memory embedding complete (%d/%d memories indexed)\n", done, len(ids))
	}
}

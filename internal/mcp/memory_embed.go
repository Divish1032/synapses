package mcp

import (
	"context"
	"time"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/logutil"
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
		logutil.Error("synapses: embed memory %s: %v\n", memoryID, err)
		return
	}
	if len(vec) == 0 {
		return
	}
	if err := st.UpsertMemoryEmbedding(memoryID, embedder.Model(), vec); err != nil {
		logutil.Error("synapses: store memory embedding %s: %v\n", memoryID, err)
	}
}

// EmbedAllMemories generates embeddings for all un-embedded memories in the
// background. Rate-limited to ~2 embeddings/second for builtin mode to avoid
// CPU contention. Called at startup to lazy-migrate legacy memories.
func EmbedAllMemories(ctx context.Context, embedder embed.Embedder, st *store.Store) {
	if embedder == nil || st == nil {
		return
	}

	ids, err := st.GetMemoriesWithoutEmbeddings(0) // 0 = no limit
	if err != nil || len(ids) == 0 {
		return
	}

	logutil.Info("synapses: embedding %d memories (model: %s) …\n", len(ids), embedder.Model())
	done := 0
	errors := 0

	// Rate limit: pause between embeddings to avoid saturating CPU.
	// Builtin mode is CPU-bound; Ollama mode has its own throughput limits.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for i, memID := range ids {
		select {
		case <-ctx.Done():
			if done > 0 {
				logutil.Warn("synapses: memory embedding interrupted (%d/%d done)\n", done, len(ids))
			}
			return
		default:
		}

		// Rate limit: wait between embeddings (skip for first one).
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}

		text, ok := st.GetMemoryTextForEmbedding(memID)
		if !ok || text == "" {
			continue
		}

		embedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		vec, embedErr := embedder.Embed(embedCtx, text)
		cancel()

		if embedErr != nil {
			errors++
			// Log first 3 errors, then suppress to avoid log spam.
			if errors <= 3 {
				logutil.Error("synapses: embed memory %s: %v\n", memID, embedErr)
			} else if errors == 4 {
				logutil.Warn("synapses: suppressing further embedding errors (%d so far)\n", errors)
			}
			continue
		}
		if len(vec) == 0 {
			continue
		}
		if err := st.UpsertMemoryEmbedding(memID, embedder.Model(), vec); err != nil {
			logutil.Error("synapses: store memory embedding %s: %v\n", memID, err)
		}
		done++
	}

	if done > 0 || errors > 0 {
		logutil.Info("synapses: memory embedding complete (%d/%d indexed, %d errors)\n", done, len(ids), errors)
	}
}

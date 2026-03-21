package mcp

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/store"
)

// statusDetailer is satisfied by embed.BuiltinEmbedder to expose model init state.
type statusDetailer interface {
	StatusDetail() string
}

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
// pc may be nil (pulse disabled) — fire-and-forget EmbeddingEvent on completion. (P2-6)
func EmbedAllMemories(ctx context.Context, embedder embed.Embedder, st *store.Store, pc *pulse.Client) {
	if embedder == nil || st == nil {
		return
	}

	ids, err := st.GetMemoriesWithoutEmbeddings(0) // 0 = no limit
	if err != nil || len(ids) == 0 {
		return
	}

	staleCount := len(ids) // P2-15: capture before processing
	logutil.Info("synapses: embedding %d memories (model: %s) …\n", len(ids), embedder.Model())
	done := 0
	errors := 0
	start := time.Now() // P2-6: timing

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

	// P2-6: emit EmbeddingEvent on completion. Enqueue is mutex+append (O(1)) —
	// direct call, no goroutine needed. EmbedAllMemories itself is already called
	// from a goroutine by callers.
	if pc != nil {
		modelStatus := ""
		if sd, ok := embedder.(statusDetailer); ok {
			modelStatus = sd.StatusDetail()
		}
		pc.RecordEmbeddingEvent(pulse.EmbeddingEvent{
			Trigger:     "startup",
			Model:       embedder.Model(),
			ModelStatus: modelStatus,
			Count:       done,
			Errors:      errors,
			StaleCount:  staleCount,
			DurationMs:  time.Since(start).Milliseconds(),
			Success:     errors == 0,
		})
	}
}

// normalizeVec returns a unit-length copy of v.
// Used to convert raw embedding vectors into pre-normalized form so that
// dot-product == cosine-similarity (avoids magnitude division per query).
// Always returns a fresh slice — never the input buffer — so callers that
// store the result are safe even if the embedder reuses its output array.
// Returns a zero-filled copy when magnitude is zero (degenerate vector).
func normalizeVec(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		out := make([]float32, len(v))
		copy(out, v)
		return out
	}
	scale := float32(1.0 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * scale
	}
	return out
}

// EmbedToolCatalog pre-computes normalized vector embeddings for all entries in
// toolCatalog. Each tool is embedded as "Description keywords…" so that
// cosine similarity captures both the functional description and intent tags.
//
// Called as a background goroutine from SetMemoryEmbedder. Safe to call
// concurrently — results are committed atomically under toolEmbedsMu.
// Aborts on any embedding error to avoid a partial index, which would silently
// degrade ranking (partial embeddings produce misleading similarity scores).
func (s *Server) EmbedToolCatalog(ctx context.Context, embedder embed.Embedder) {
	if embedder == nil {
		return
	}
	embeddings := make([][]float32, len(toolCatalog))
	for i, tool := range toolCatalog {
		text := tool.Description
		if len(tool.Keywords) > 0 {
			text += " " + strings.Join(tool.Keywords, " ")
		}
		embedCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		vec, err := embedder.Embed(embedCtx, text)
		cancel()
		if err != nil {
			logutil.Warn("synapses: embed tool catalog %s: %v — semantic tool discovery unavailable\n", tool.Name, err)
			return // abort; partial index is worse than no index
		}
		if len(vec) == 0 {
			logutil.Warn("synapses: embed tool catalog %s: empty vector — semantic tool discovery unavailable\n", tool.Name)
			return
		}
		embeddings[i] = normalizeVec(vec)
	}

	model := embedder.Model()
	s.toolEmbedsMu.Lock()
	s.toolEmbeds = embeddings
	s.toolEmbedModel = model
	s.toolEmbedsMu.Unlock()
	logutil.Info("synapses: tool catalog embedded (%d tools, model=%s)\n", len(embeddings), model)
}

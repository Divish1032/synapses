package mcp

import (
	"context"
	"math"
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
// parentCtx is the server lifecycle context; the 30s timeout is nested under it
// so embedding work stops when the server shuts down.
func (s *Server) embedMemory(parentCtx context.Context, embedder embed.Embedder, st *store.Store, memoryID, content string) {
	if embedder == nil || st == nil || content == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
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

const maxFailedEmbedIDs = 256

// trackFailedEmbed records a memory ID that failed to embed due to a full
// background queue. Bounded to maxFailedEmbedIDs entries.
func (s *Server) trackFailedEmbed(memoryID string) {
	count := 0
	s.failedEmbedIDs.Range(func(_, _ interface{}) bool {
		count++
		return count < maxFailedEmbedIDs
	})
	if count < maxFailedEmbedIDs {
		s.failedEmbedIDs.Store(memoryID, struct{}{})
	}
}

// failedEmbedRetryLoop retries embeddings that were dropped due to a full
// background queue. Runs every 60 seconds.
func (s *Server) failedEmbedRetryLoop(embedder embed.Embedder, st *store.Store) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			var ids []string
			s.failedEmbedIDs.Range(func(k, _ interface{}) bool {
				ids = append(ids, k.(string))
				s.failedEmbedIDs.Delete(k)
				return true
			})
			if len(ids) == 0 {
				continue
			}
			logutil.Info("synapses: retrying %d dropped embed(s)\n", len(ids))
			for _, memID := range ids {
				content, ok := st.GetMemoryContent(memID)
				if !ok || content == "" {
					continue
				}
				s.goBackground(func() { s.embedMemory(s.lifecycleCtx, embedder, st, memID, content) })
			}
		case <-s.stopCh:
			return
		}
	}
}

// EmbedAllMemories generates embeddings for all un-embedded memories in the
// background. Rate-limited to ~2 embeddings/second for builtin mode to avoid
// CPU contention. Called at startup to lazy-migrate legacy memories.
//
// Model-change migration: before scanning for un-embedded memories, all
// embeddings from a previous model are marked stale. This handles the
// MiniLM → nomic upgrade path: old embeddings are in a different vector
// space and must be regenerated. The rate limiter ensures this doesn't
// saturate CPU (nomic is ~12s/embed on CPU; 1000 memories ≈ 1h8m at pool/3).
//
// pc may be nil (pulse disabled) — fire-and-forget EmbeddingEvent on completion. (P2-6)
func EmbedAllMemories(ctx context.Context, embedder embed.Embedder, st *store.Store, pc *pulse.Client) {
	if embedder == nil || st == nil {
		return
	}

	// Model-change migration: invalidate embeddings from a different model.
	// This is the critical path for embedding model upgrades — old embeddings
	// in a different vector space produce meaningless similarity scores.
	if invalidated, err := st.InvalidateEmbeddingsByModel(embedder.Model()); err != nil {
		logutil.Error("synapses: invalidate old model embeddings: %v\n", err)
	} else if invalidated > 0 {
		logutil.Info("synapses: model upgrade detected — marked %d embeddings for re-embedding (new model: %s)\n", invalidated, embedder.Model())
		// Rebuild HNSW index to purge stale old-model embeddings that were
		// loaded at startup before invalidation. The rebuilt index contains
		// only current-model embeddings (RebuildMemoryHNSW filters stale=0).
		st.RebuildMemoryHNSW()
	}

	// Process in batches of 500 so that:
	//   1. Memory usage is bounded even with millions of unembedded memories.
	//   2. Resume is incremental: already-embedded memories are committed to
	//      the DB before the next batch is fetched, so a crash mid-batch loses
	//      at most batchSize embeddings rather than the entire queue.
	const batchSize = 500

	done := 0
	errors := 0
	staleCount := 0
	start := time.Now()

	// Rate limit: pause between embeddings to avoid saturating CPU.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for ctx.Err() == nil {
		ids, err := st.GetMemoriesWithoutEmbeddings(batchSize)
		if err != nil {
			logutil.Error("synapses: get memories without embeddings: %v\n", err)
			break
		}
		if len(ids) == 0 {
			break
		}
		if staleCount == 0 {
			logutil.Info("synapses: embedding memories (model: %s) …\n", embedder.Model())
		}
		staleCount += len(ids)

		batchDone := 0
		for i, memID := range ids {
			select {
			case <-ctx.Done():
				if done > 0 {
					logutil.Warn("synapses: memory embedding interrupted (%d done so far)\n", done)
				}
				return
			default:
			}

			// Rate limit: wait between embeddings (skip for first one).
			if i > 0 || done > 0 {
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
			batchDone++
		}

		// If the entire batch made zero progress (embedder persistently unavailable),
		// stop to avoid an infinite retry loop on the same IDs.
		if batchDone == 0 {
			logutil.Warn("synapses: memory embedding stalled — embedder unavailable (%d errors total), stopping\n", errors)
			break
		}
	}

	if done > 0 || errors > 0 {
		logutil.Info("synapses: memory embedding complete (%d/%d indexed, %d errors, %.1fs)\n", done, staleCount, errors, time.Since(start).Seconds())
	}
	_ = start // used in log above

	// Phase 2: refresh stale embeddings (content changed since last embedding).
	// Loop until no stale IDs remain, mirroring the primary phase above.
	staleDone := 0
	for ctx.Err() == nil {
		staleIDs, staleErr := st.GetStaleEmbeddingMemoryIDs(500)
		if staleErr != nil || len(staleIDs) == 0 {
			break
		}
		if staleDone == 0 {
			logutil.Info("synapses: refreshing stale embedding(s) (model: %s) …\n", embedder.Model())
		}
		batchStaleDone := 0
		for i, memID := range staleIDs {
			select {
			case <-ctx.Done():
				return
			default:
			}
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
			embedCtx2, cancel2 := context.WithTimeout(ctx, 30*time.Second)
			vec2, err2 := embedder.Embed(embedCtx2, text)
			cancel2()
			if err2 != nil || len(vec2) == 0 {
				// Embed failed — leave old embedding intact so it remains queryable.
				continue
			}
			// Upsert atomically replaces stale embedding only after fresh value is ready.
			if err := st.UpsertMemoryEmbedding(memID, embedder.Model(), vec2); err != nil {
				logutil.Error("synapses: store stale embedding %s: %v\n", memID, err)
			} else {
				staleDone++
				batchStaleDone++
			}
		}
		// If the entire batch made zero progress, stop to avoid infinite loop.
		if batchStaleDone == 0 {
			logutil.Warn("synapses: stale embedding refresh stalled — embedder unavailable, stopping\n")
			break
		}
	}
	if staleDone > 0 {
		logutil.Info("synapses: stale embedding refresh complete (%d refreshed)\n", staleDone)
	}

	// P2-6: emit EmbeddingEvent on completion. Enqueue is mutex+append (O(1)) —
	// direct call, no goroutine needed. EmbedAllMemories itself is already called
	// from a goroutine by callers.
	if pc != nil {
		modelStatus := ""
		if sd, ok := embedder.(statusDetailer); ok {
			modelStatus = sd.StatusDetail()
		}
		// P8-10: compute embedding coverage percentage.
		var coveragePct float64
		totalMemories := st.CountEmbeddableMemories()
		if totalMemories > 0 {
			remainingStale := staleCount - done
			if remainingStale < 0 {
				remainingStale = 0
			}
			coveragePct = float64(totalMemories-remainingStale) / float64(totalMemories)
			if coveragePct < 0 {
				coveragePct = 0
			} else if coveragePct > 1 {
				coveragePct = 1
			}
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
			EventType:   "batch",
			CoveragePct: coveragePct,
		})
	}
}

// normalizeVec returns a unit-length copy of v.
// Used to convert raw embedding vectors into pre-normalized form so that
// dot-product == cosine-similarity (avoids magnitude division per query).
// Always returns a fresh slice — never the input buffer — so callers that
// store the result are safe even if the embedder reuses its output array.
// Returns nil when magnitude is zero (degenerate vector) so callers can
// detect and skip rather than storing a zero-vector as valid.
func normalizeVec(v []float32) []float32 {
	if len(v) == 0 {
		return nil
	}
	// Guard: reject input containing NaN or Inf values.
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil
		}
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 || math.IsNaN(sum) || math.IsInf(sum, 0) {
		return nil
	}
	scale := float32(1.0 / math.Sqrt(sum))
	// Guard: reject if scale is NaN/Inf (near-zero sum edge case).
	if math.IsNaN(float64(scale)) || math.IsInf(float64(scale), 0) {
		return nil
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * scale
	}
	return out
}

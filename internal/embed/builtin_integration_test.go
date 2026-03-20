//go:build integration

package embed_test

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Integration tests (require model download, ~23 MB) ──────────────────────
//
// Run with: go test ./internal/embed -tags integration -race -count=1 -timeout 120s
//
// These tests download the all-MiniLM-L6-v2 ONNX model on first run.
// The model is cached in the test temp directory across subtests.

func TestBuiltinEmbedder_Integration(t *testing.T) {
	// Shared embedder so model is downloaded once for all subtests.
	modelsDir := t.TempDir()
	e := embed.NewBuiltinEmbedder(modelsDir)
	t.Cleanup(func() {
		require.NoError(t, e.Close())
	})

	t.Run("Embed_HelloWorld_384Dims", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		vec, err := e.Embed(ctx, "hello world")
		require.NoError(t, err)
		require.Len(t, vec, 384, "expected 384-dimensional embedding")

		// Verify non-zero: at least some values should be non-zero.
		var sumSq float64
		for _, v := range vec {
			sumSq += float64(v) * float64(v)
		}
		assert.Greater(t, sumSq, 0.0, "embedding should have non-zero magnitude")

		// Verify normalization: L2 norm should be ~1.0 (MiniLM with normalization).
		norm := math.Sqrt(sumSq)
		assert.InDelta(t, 1.0, norm, 0.01, "normalized embedding should have L2 norm ≈ 1.0")
	})

	t.Run("IsReady_AfterEmbed", func(t *testing.T) {
		assert.True(t, e.IsReady(), "embedder should be ready after successful Embed")
	})

	t.Run("StatusDetail_AfterEmbed", func(t *testing.T) {
		assert.Equal(t, "ready", e.StatusDetail())
	})

	t.Run("Embed_DifferentTexts_DifferentVectors", func(t *testing.T) {
		ctx := context.Background()
		vec1, err := e.Embed(ctx, "the quick brown fox")
		require.NoError(t, err)

		vec2, err := e.Embed(ctx, "quantum mechanics")
		require.NoError(t, err)

		// Cosine similarity should be well below 1.0 for unrelated texts.
		var dot float64
		for i := range vec1 {
			dot += float64(vec1[i]) * float64(vec2[i])
		}
		assert.Less(t, dot, 0.95, "semantically different texts should have cosine sim < 0.95")
	})

	t.Run("Embed_EmptyString", func(t *testing.T) {
		ctx := context.Background()
		vec, err := e.Embed(ctx, "")
		require.NoError(t, err)
		assert.Len(t, vec, 384, "empty string should still produce 384-dim embedding")
	})

	t.Run("Embed_ConcurrentCalls", func(t *testing.T) {
		const n = 5
		var wg sync.WaitGroup
		results := make([][]float32, n)
		errs := make([]error, n)

		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				results[idx], errs[idx] = e.Embed(context.Background(), "concurrent test")
			}(i)
		}
		wg.Wait()

		for i := 0; i < n; i++ {
			require.NoError(t, errs[i], "goroutine %d failed", i)
			assert.Len(t, results[i], 384, "goroutine %d returned wrong dimension", i)
		}
	})

	t.Run("Close_ThenReopen", func(t *testing.T) {
		// Close the embedder, verify state reset.
		require.NoError(t, e.Close())
		assert.False(t, e.IsReady())

		// Re-initialize by calling Embed again (model is cached on disk).
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		vec, err := e.Embed(ctx, "reopen test")
		require.NoError(t, err)
		assert.Len(t, vec, 384)
		assert.True(t, e.IsReady())
	})
}

func TestBuiltinEmbedder_ContextCancellation_MidEmbed(t *testing.T) {
	modelsDir := t.TempDir()
	e := embed.NewBuiltinEmbedder(modelsDir)
	t.Cleanup(func() { _ = e.Close() })

	// First, ensure model is downloaded with a generous timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := e.Embed(ctx, "warmup")
	require.NoError(t, err)

	// Now test that a pre-cancelled context fails fast.
	cancelledCtx, cancelFn := context.WithCancel(context.Background())
	cancelFn()

	vec, err := e.Embed(cancelledCtx, "should fail")
	assert.Nil(t, vec)
	assert.ErrorIs(t, err, context.Canceled)
}

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

	t.Run("Embed_Deterministic", func(t *testing.T) {
		ctx := context.Background()
		vec1, err := e.Embed(ctx, "determinism check")
		require.NoError(t, err)

		vec2, err := e.Embed(ctx, "determinism check")
		require.NoError(t, err)

		require.Len(t, vec1, 384)
		require.Len(t, vec2, 384)
		for i := range vec1 {
			assert.InDelta(t, vec1[i], vec2[i], 1e-6,
				"same input must produce same output at index %d", i)
		}
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

	t.Run("Embed_LongText", func(t *testing.T) {
		// MiniLM has a 256-token context window. Verify long text doesn't panic
		// or return an error — the model truncates internally.
		longText := ""
		for i := 0; i < 1000; i++ {
			longText += "the quick brown fox jumps over the lazy dog "
		}
		ctx := context.Background()
		vec, err := e.Embed(ctx, longText)
		require.NoError(t, err)
		assert.Len(t, vec, 384)
	})

	t.Run("Embed_ConcurrentCalls_AllIdentical", func(t *testing.T) {
		const n = 5
		const text = "concurrent determinism test"
		var wg sync.WaitGroup
		results := make([][]float32, n)
		errs := make([]error, n)

		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				results[idx], errs[idx] = e.Embed(context.Background(), text)
			}(i)
		}
		wg.Wait()

		for i := 0; i < n; i++ {
			require.NoError(t, errs[i], "goroutine %d failed", i)
			require.Len(t, results[i], 384, "goroutine %d returned wrong dimension", i)
		}

		// All goroutines embedded the same text — verify results are identical.
		for i := 1; i < n; i++ {
			for j := range results[0] {
				assert.InDelta(t, results[0][j], results[i][j], 1e-6,
					"goroutine %d differs from goroutine 0 at index %d", i, j)
			}
		}
	})
}

// TestBuiltinEmbedder_CloseThenReopen uses its own embedder instance to
// avoid mutating state shared with other subtests.
func TestBuiltinEmbedder_CloseThenReopen(t *testing.T) {
	modelsDir := t.TempDir()
	e := embed.NewBuiltinEmbedder(modelsDir)
	t.Cleanup(func() { _ = e.Close() })

	// Initialize the embedder.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	vec, err := e.Embed(ctx, "warmup")
	require.NoError(t, err)
	require.Len(t, vec, 384)
	assert.True(t, e.IsReady())

	// Close and verify state reset.
	require.NoError(t, e.Close())
	assert.False(t, e.IsReady())
	// After close, status should show init was attempted but not ready.
	// Close sets ready=false but initAttempted stays true.
	assert.Equal(t, "unavailable", e.StatusDetail())

	// Re-initialize by calling Embed again (model is cached on disk).
	vec2, err := e.Embed(ctx, "reopen test")
	require.NoError(t, err)
	assert.Len(t, vec2, 384)
	assert.True(t, e.IsReady())
	assert.Equal(t, "ready", e.StatusDetail())
}

func TestBuiltinEmbedder_ContextCancellation_PreAndPostLock(t *testing.T) {
	modelsDir := t.TempDir()
	e := embed.NewBuiltinEmbedder(modelsDir)
	t.Cleanup(func() { _ = e.Close() })

	// Ensure model is downloaded with a generous timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := e.Embed(ctx, "warmup")
	require.NoError(t, err)

	// Test 1: Pre-cancelled context (hits the pre-lock check at builtin.go:121).
	cancelledCtx, cancelFn := context.WithCancel(context.Background())
	cancelFn()

	vec, err := e.Embed(cancelledCtx, "should fail")
	assert.Nil(t, vec)
	assert.ErrorIs(t, err, context.Canceled)

	// Test 2: Deadline-exceeded context.
	expiredCtx, expCancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
	defer expCancel()

	vec2, err2 := e.Embed(expiredCtx, "should also fail")
	assert.Nil(t, vec2)
	assert.ErrorIs(t, err2, context.DeadlineExceeded)

	// Verify embedder is still healthy after cancelled calls.
	goodVec, goodErr := e.Embed(context.Background(), "recovery")
	require.NoError(t, goodErr)
	assert.Len(t, goodVec, 384, "embedder should recover after cancelled calls")
}

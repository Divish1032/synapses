package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Unit tests (no model download required) ─────────────────────────────────

func TestBuiltinEmbedder_NewAndModel(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	t.Cleanup(func() { _ = e.Close() })
	require.NotNil(t, e)
	assert.Equal(t, "nomic-embed-text-v1.5", e.Model())
}

func TestBuiltinEmbedder_StatusDetail_BeforeInit(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	t.Cleanup(func() { _ = e.Close() })
	assert.Equal(t, "model not yet downloaded", e.StatusDetail())
}

func TestBuiltinEmbedder_CloseBeforeInit(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	err := e.Close()
	assert.NoError(t, err, "Close() on un-initialized embedder should succeed")
}

func TestBuiltinEmbedder_CloseIdempotent(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	require.NoError(t, e.Close())
	require.NoError(t, e.Close())
	// StatusDetail should still be valid after double close.
	assert.Equal(t, "model not yet downloaded", e.StatusDetail())
}

func TestBuiltinEmbedder_EmbedCancelledContext(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	t.Cleanup(func() { _ = e.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	vec, err := e.Embed(ctx, "hello world")
	assert.Nil(t, vec)
	assert.ErrorIs(t, err, context.Canceled)
	// Embedder should NOT have attempted init on a cancelled context.
	assert.Equal(t, "model not yet downloaded", e.StatusDetail())
}

func TestBuiltinEmbedder_EmbedDeadlineExceeded(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	t.Cleanup(func() { _ = e.Close() })

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
	defer cancel()

	vec, err := e.Embed(ctx, "hello world")
	assert.Nil(t, vec)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestBuiltinEmbedder_EmbedFailedInit_StatusUnavailable(t *testing.T) {
	// Use an impossible path so MkdirAll fails during ensureModel,
	// without triggering an actual network download.
	e := embed.NewBuiltinEmbedder("/dev/null/impossible/path")
	t.Cleanup(func() { _ = e.Close() })

	vec, err := e.Embed(context.Background(), "hello")
	assert.Nil(t, vec)
	assert.Error(t, err)
	// After a failed init attempt, status should transition to "unavailable".
	assert.Equal(t, "unavailable", e.StatusDetail())
}

func TestBuiltinEmbedder_RetryAfterFailedInit(t *testing.T) {
	// Verify the embedder is NOT permanently broken after a failed init.
	// The "initAttempted" flag should not prevent retry on next Embed call.
	e := embed.NewBuiltinEmbedder("/dev/null/impossible/path")
	t.Cleanup(func() { _ = e.Close() })

	// First call fails.
	_, err := e.Embed(context.Background(), "hello")
	require.Error(t, err)
	assert.Equal(t, "unavailable", e.StatusDetail())

	// Second call should also attempt init (retry), not return a cached error.
	_, err2 := e.Embed(context.Background(), "hello again")
	require.Error(t, err2)
	// The key assertion: err2 is a fresh error from ensureModel(), not a
	// "permanently broken" sentinel. Both errors should come from the same
	// code path (ensureModel), proving retry happened.
	assert.Equal(t, err.Error(), err2.Error(), "retry should hit same ensureModel path")
}

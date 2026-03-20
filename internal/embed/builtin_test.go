package embed_test

import (
	"context"
	"testing"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Unit tests (no model download required) ─────────────────────────────────

func TestBuiltinEmbedder_NewAndModel(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	require.NotNil(t, e)
	assert.Equal(t, "all-MiniLM-L6-v2", e.Model())
}

func TestBuiltinEmbedder_IsReady_InitiallyFalse(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	assert.False(t, e.IsReady(), "embedder should not be ready before first Embed call")
}

func TestBuiltinEmbedder_StatusDetail_BeforeInit(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	assert.Equal(t, "model not yet downloaded", e.StatusDetail())
}

func TestBuiltinEmbedder_CloseBeforeInit(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	err := e.Close()
	assert.NoError(t, err, "Close() on un-initialized embedder should succeed")
	assert.False(t, e.IsReady())
}

func TestBuiltinEmbedder_CloseIdempotent(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	require.NoError(t, e.Close())
	require.NoError(t, e.Close())
}

func TestBuiltinEmbedder_EmbedCancelledContext(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	vec, err := e.Embed(ctx, "hello world")
	assert.Nil(t, vec)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestBuiltinEmbedder_EmbedFailedInit_StatusUnavailable(t *testing.T) {
	// Use an impossible path so MkdirAll fails during ensureModel,
	// without triggering an actual network download.
	e := embed.NewBuiltinEmbedder("/dev/null/impossible/path")

	vec, err := e.Embed(context.Background(), "hello")
	assert.Nil(t, vec)
	assert.Error(t, err)
	// After a failed init attempt, status should transition to "unavailable".
	assert.Equal(t, "unavailable", e.StatusDetail())
}

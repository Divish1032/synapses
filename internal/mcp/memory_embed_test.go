package mcp

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testEmbedder is a mock that satisfies embed.Embedder.
type testEmbedder struct {
	vec       []float32
	err       error
	model     string
	callCount atomic.Int32
}

func (e *testEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	e.callCount.Add(1)
	return e.vec, e.err
}

func (e *testEmbedder) Model() string { return e.model }

// compile-time check
var _ embed.Embedder = (*testEmbedder)(nil)

func TestEmbedMemory_NilEmbedder(t *testing.T) {
	s := &Server{}
	// Should not panic with nil embedder.
	s.embedMemory(nil, nil, "mem1", "some content")
}

func TestEmbedMemory_EmptyContent(t *testing.T) {
	e := &testEmbedder{vec: []float32{0.1}, model: "test"}
	s := &Server{}
	s.embedMemory(e, nil, "mem1", "")
	assert.Equal(t, int32(0), e.callCount.Load())
}

func TestEmbedMemory_StoresEmbedding(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	// Insert a memory first.
	mid, err := st.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "auth service uses JWT",
		AgentID: "test",
		Source:  store.SourceManual,
	})
	require.NoError(t, err)

	e := &testEmbedder{
		vec:   make([]float32, 384),
		model: "all-MiniLM-L6-v2",
	}
	for i := range e.vec {
		e.vec[i] = float32(i) * 0.001
	}

	s := &Server{}
	s.embedMemory(e, st, mid, "auth service uses JWT")

	assert.Equal(t, int32(1), e.callCount.Load())
	assert.Equal(t, 1, st.MemoryEmbeddingCount())
}

func TestEmbedMemory_EmbedError_NoStoreWrite(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	e := &testEmbedder{
		err:   assert.AnError,
		model: "test",
	}

	s := &Server{}
	s.embedMemory(e, st, "mem1", "content")

	assert.Equal(t, int32(1), e.callCount.Load())
	assert.Equal(t, 0, st.MemoryEmbeddingCount())
}

func TestEmbedAllMemories_EmbedsUnembeddedMemories(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	// Insert 3 memories.
	for i := 0; i < 3; i++ {
		_, err := st.InsertMemory(store.Memory{
			Tier:    store.TierProject,
			Content: "memory content " + string(rune('A'+i)),
			AgentID: "test",
			Source:  store.SourceManual,
		})
		require.NoError(t, err)
	}

	vec := make([]float32, 384)
	for i := range vec {
		vec[i] = 0.01
	}
	e := &testEmbedder{vec: vec, model: "test-model"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	EmbedAllMemories(ctx, e, st, nil)

	assert.Equal(t, int32(3), e.callCount.Load())
	assert.Equal(t, 3, st.MemoryEmbeddingCount())
}

func TestEmbedAllMemories_NilEmbedder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	// Should not panic.
	EmbedAllMemories(context.Background(), nil, st, nil)
}

func TestEmbedAllMemories_NilStore(t *testing.T) {
	e := &testEmbedder{vec: []float32{0.1}, model: "test"}
	// Should not panic.
	EmbedAllMemories(context.Background(), e, nil, nil)
}

func TestEmbedAllMemories_ContextCancellation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	// Insert memories.
	for i := 0; i < 5; i++ {
		_, err := st.InsertMemory(store.Memory{
			Tier:    store.TierProject,
			Content: "memory content for cancel test " + string(rune('A'+i)),
			AgentID: "test",
			Source:  store.SourceManual,
		})
		require.NoError(t, err)
	}

	// Cancel immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	vec := make([]float32, 384)
	e := &testEmbedder{vec: vec, model: "test"}

	EmbedAllMemories(ctx, e, st, nil)

	// Should have embedded 0 or very few memories due to cancellation.
	assert.LessOrEqual(t, e.callCount.Load(), int32(5))
}

func TestEmbedAllMemories_SkipsAlreadyEmbedded(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer st.Close()

	vec := make([]float32, 384)
	for i := range vec {
		vec[i] = 0.01
	}

	// Insert a memory and pre-embed it.
	mid, err := st.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "already embedded content",
		AgentID: "test",
		Source:  store.SourceManual,
	})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMemoryEmbedding(mid, "test-model", vec))

	// Insert another memory without embedding.
	_, err = st.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "not yet embedded content xyz",
		AgentID: "test",
		Source:  store.SourceManual,
	})
	require.NoError(t, err)

	e := &testEmbedder{vec: vec, model: "test-model"}

	EmbedAllMemories(context.Background(), e, st, nil)

	// Only the un-embedded memory should be processed.
	assert.Equal(t, int32(1), e.callCount.Load())
	assert.Equal(t, 2, st.MemoryEmbeddingCount())
}

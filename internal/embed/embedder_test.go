package embed_test

import (
	"context"
	"testing"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockEmbedder is a test double that satisfies the Embedder interface.
type mockEmbedder struct {
	vec   []float32
	err   error
	model string
	calls int
}

func (m *mockEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	m.calls++
	return m.vec, m.err
}

func (m *mockEmbedder) Model() string {
	return m.model
}

func TestEmbedderInterface_MockSatisfies(t *testing.T) {
	var e embed.Embedder = &mockEmbedder{
		vec:   []float32{0.1, 0.2, 0.3},
		model: "test-model",
	}
	vec, err := e.Embed(context.Background(), "hello")
	require.NoError(t, err)
	assert.Len(t, vec, 3)
	assert.Equal(t, "test-model", e.Model())
}

func TestOllamaEmbedder_NilOnEmptyEndpoint(t *testing.T) {
	e := embed.NewOllamaEmbedder("", "")
	assert.Nil(t, e)
}

func TestOllamaEmbedder_CreatesWithEndpoint(t *testing.T) {
	e := embed.NewOllamaEmbedder("http://localhost:11434/api/embeddings", "")
	require.NotNil(t, e)
	assert.Equal(t, "nomic-embed-text", e.Model())
	assert.NotNil(t, e.Client())
}

func TestOllamaEmbedder_CustomModel(t *testing.T) {
	e := embed.NewOllamaEmbedder("http://localhost:11434/api/embeddings", "all-minilm")
	require.NotNil(t, e)
	assert.Equal(t, "all-minilm", e.Model())
}

func TestBuiltinEmbedder_Model(t *testing.T) {
	e := embed.NewBuiltinEmbedder("/tmp/test-models")
	assert.Equal(t, "all-MiniLM-L6-v2", e.Model())
}

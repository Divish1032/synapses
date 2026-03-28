// Package embed provides embedding generation for converting text into
// float32 vectors for similarity search. The Embedder interface abstracts
// over multiple backends (builtin ONNX, Ollama, OpenAI).
package embed

import "context"

// Embedder generates vector embeddings from text. Implementations must be
// safe for concurrent use. A nil Embedder is NOT safe — callers must check.
type Embedder interface {
	// Embed returns a vector embedding for text.
	// Returns (nil, nil) only when the embedder is intentionally disabled.
	Embed(ctx context.Context, text string) ([]float32, error)

	// WarmUp pre-initializes the embedder (e.g. downloads the model file).
	// Call at daemon startup in a background goroutine so the first Embed()
	// call doesn't block on model download. Implementations that need no
	// warmup should return nil immediately.
	WarmUp(ctx context.Context) error

	// Model returns the model name used for embedding generation.
	// Used as the model key in UpsertMemoryEmbedding for cache invalidation.
	Model() string

	// Close releases any resources held by the embedder (e.g. ONNX sessions).
	// Implementations where Close is a no-op should return nil.
	Close() error
}

package embed

import (
	"context"
	"fmt"
	"time"
)

// OllamaEmbedder wraps an embed.Client to satisfy the Embedder interface.
// Use this when embeddings mode is "ollama" — delegates to a local Ollama
// instance via the existing HTTP client.
type OllamaEmbedder struct {
	client *Client
}

// NewOllamaEmbedder creates an Embedder that delegates to a local Ollama
// instance. endpoint is the Ollama API URL (e.g. "http://localhost:11434/api/embeddings").
// model defaults to "nomic-embed-text" when empty.
// Returns nil if endpoint is empty.
func NewOllamaEmbedder(endpoint, model string, opts ...Option) *OllamaEmbedder {
	c := NewClient(endpoint, model, opts...)
	if c == nil {
		return nil
	}
	return &OllamaEmbedder{client: c}
}

// Embed generates an embedding via the Ollama HTTP API.
func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return o.client.Embed(ctx, text)
}

// Model returns the configured embedding model name.
func (o *OllamaEmbedder) Model() string {
	return o.client.Model()
}

// WarmUp validates that the Ollama server has the embedding model available by
// performing a single test embed. Returns an error if the model is not pulled
// or the server is unreachable — callers can fall back to builtin ONNX.
func (o *OllamaEmbedder) WarmUp(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := o.client.Embed(ctx, "warmup")
	if err != nil {
		return fmt.Errorf("ollama warmup: %w", err)
	}
	return nil
}

// Close is a no-op for the Ollama embedder (HTTP client has no resources to release).
func (o *OllamaEmbedder) Close() error { return nil }

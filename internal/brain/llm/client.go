// Package llm provides the LLM client abstraction for synapses-intelligence.
// All LLM calls use structured JSON output to ensure deterministic, fast parsing.
package llm

import (
	"context"
	"io"
)

// LLMClient is the interface for all LLM backends.
// Implementations: OllamaClient (production), MockClient (tests).
type LLMClient interface {
	// Generate sends a prompt to the LLM and returns the raw response text.
	// The caller is responsible for parsing the JSON response.
	Generate(ctx context.Context, prompt string) (string, error)

	// Available returns true if the backend is reachable and the model is loaded.
	Available(ctx context.Context) bool

	// ModelName returns the configured model identifier.
	ModelName() string

	// ModelPulled returns true if the model is already present locally
	// (no download needed).
	ModelPulled(ctx context.Context) bool

	// PullModel downloads the model, streaming progress to w.
	PullModel(ctx context.Context, w io.Writer) error
}

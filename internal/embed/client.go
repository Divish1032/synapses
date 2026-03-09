// Package embed provides a fail-silent client for generating vector embeddings
// from an Ollama-compatible or OpenAI-compatible HTTP endpoint.
// A nil *Client is safe — all methods return (nil, nil) so callers can use
// the zero value without checking for configuration.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client calls an embedding endpoint to convert text into float32 vectors.
// Supports three formats, auto-detected from the endpoint URL:
//   - Brain  (/v1/embed)        — synapses-intelligence native (Ollama-free)
//   - OpenAI (/v1/embeddings)   — OpenAI-compatible
//   - Ollama (/api/embeddings)  — Ollama native
//
// A nil Client is safe to use — Embed returns (nil, nil).
type Client struct {
	endpoint string
	model    string
	http     *http.Client
}

// NewClient creates a Client for the given endpoint. Returns nil if endpoint
// is empty (embedding disabled). model defaults to "nomic-embed-text" when
// empty, which produces 768-dimensional vectors and is available in Ollama
// without any extra setup.
func NewClient(endpoint, model string) *Client {
	if endpoint == "" {
		return nil
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	return &Client{
		endpoint: endpoint,
		model:    model,
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

// NewBrainClient creates a Client that calls synapses-intelligence's native
// POST /v1/embed endpoint. This requires no Ollama — embeddings are generated
// by a llama-server subprocess managed by the brain sidecar.
//
// brainURL is the base URL of the brain server, e.g. "http://localhost:11435".
// Returns nil if brainURL is empty.
func NewBrainClient(brainURL string) *Client {
	if brainURL == "" {
		return nil
	}
	return &Client{
		endpoint: strings.TrimRight(brainURL, "/") + "/v1/embed",
		model:    "nomic-embed-text-v1.5.Q4_K_M",
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Embed returns a vector embedding for text.
// The format is auto-detected from the endpoint URL:
//   - "/v1/embed"       → Brain format  (synapses-intelligence native)
//   - "/v1/embeddings"  → OpenAI format
//   - otherwise         → Ollama format
//
// Returns (nil, nil) if the client is nil.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	if c == nil {
		return nil, nil
	}

	isBrain := strings.HasSuffix(c.endpoint, "/v1/embed")
	isOpenAI := !isBrain && strings.Contains(c.endpoint, "/v1/embeddings")

	var bodyMap map[string]interface{}
	switch {
	case isBrain:
		// Brain format: {"input": "text"}
		bodyMap = map[string]interface{}{"input": text}
	case isOpenAI:
		// OpenAI format: {"model": "...", "input": "text"}
		bodyMap = map[string]interface{}{"model": c.model, "input": text}
	default:
		// Ollama format: {"model": "...", "prompt": "text"}
		bodyMap = map[string]interface{}{"model": c.model, "prompt": text}
	}

	body, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed endpoint returned %d", resp.StatusCode)
	}

	if isOpenAI {
		var out struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("decode embed response: %w", err)
		}
		if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
			return nil, fmt.Errorf("empty embedding from endpoint")
		}
		return out.Data[0].Embedding, nil
	}

	// Brain format and Ollama format both use {"embedding": [float, ...]}
	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding from endpoint")
	}
	return out.Embedding, nil
}

// EmbedBatch returns vector embeddings for a batch of texts in one HTTP round-trip.
// Supports Brain batch format ({"input": [...]}) and OpenAI batch format.
// Ollama does not support batch — falls back to serial Embed() calls.
// Returns (nil, nil) if the client is nil or texts is empty.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if c == nil || len(texts) == 0 {
		return nil, nil
	}

	isBrain := strings.HasSuffix(c.endpoint, "/v1/embed")
	isOpenAI := !isBrain && strings.Contains(c.endpoint, "/v1/embeddings")

	if !isBrain && !isOpenAI {
		// Ollama doesn't support batch — fall back to serial.
		return c.embedSerial(ctx, texts)
	}

	var bodyMap map[string]interface{}
	if isBrain {
		bodyMap = map[string]interface{}{"input": texts}
	} else {
		bodyMap = map[string]interface{}{"model": c.model, "input": texts}
	}

	body, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed batch request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed batch endpoint returned %d", resp.StatusCode)
	}

	if isOpenAI {
		var out struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("decode batch embed response: %w", err)
		}
		if len(out.Data) != len(texts) {
			return nil, fmt.Errorf("batch response length mismatch: got %d, want %d", len(out.Data), len(texts))
		}
		vecs := make([][]float32, len(out.Data))
		for i, d := range out.Data {
			vecs[i] = d.Embedding
		}
		return vecs, nil
	}

	// Brain format: {"embeddings": [[...], ...]}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode batch embed response: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("batch response length mismatch: got %d, want %d", len(out.Embeddings), len(texts))
	}
	return out.Embeddings, nil
}

// embedSerial falls back to individual Embed() calls for endpoints that don't
// support batching (e.g. Ollama). Stops and returns an error on the first failure.
func (c *Client) embedSerial(ctx context.Context, texts []string) ([][]float32, error) {
	vecs := make([][]float32, len(texts))
	for i, text := range texts {
		v, err := c.Embed(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("serial embed[%d]: %w", i, err)
		}
		vecs[i] = v
	}
	return vecs, nil
}

// Model returns the configured embedding model name.
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.model
}

// Endpoint returns the configured embedding endpoint URL.
func (c *Client) Endpoint() string {
	if c == nil {
		return ""
	}
	return c.endpoint
}

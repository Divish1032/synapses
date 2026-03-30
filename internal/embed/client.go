// Package embed provides a fail-silent client for generating vector embeddings
// from an Ollama-compatible or OpenAI-compatible HTTP endpoint.
// A nil *Client is safe — all methods return (nil, nil) so callers can use
// the zero value without checking for configuration.
//
// Supported endpoint formats (auto-detected from URL):
//   - OpenAI (/v1/embeddings) — batch-capable, request {"model":...,"input":...}
//   - Ollama (/api/embeddings) — serial-only, request {"model":...,"prompt":...}
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// HTTPDoer is the interface for making HTTP requests.
// *http.Client satisfies this interface. Expose it so callers can inject
// custom transports (retry, tracing, rate-limiting) or test doubles without
// needing a real network. This is the same pattern used by AWS SDK v2,
// google-cloud-go, and stripe-go.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client calls an embedding endpoint to convert text into float32 vectors.
// Supports two formats, auto-detected from the endpoint URL:
//   - OpenAI (/v1/embeddings)   — OpenAI-compatible, supports batch
//   - Ollama (/api/embeddings)  — Ollama native, serial-only
//
// A nil Client is safe to use — Embed returns (nil, nil).
type Client struct {
	endpoint   string
	model      string
	httpClient HTTPDoer
}

// Option is a functional option for Client construction.
type Option func(*Client)

// WithHTTPDoer replaces the default *http.Client with a custom HTTPDoer.
// Use this to inject retry wrappers, custom transports, or test doubles.
//
//	// Production: add tracing
//	embed.NewClient(url, model, embed.WithHTTPDoer(tracedClient))
//
//	// Tests: inject a mock without needing a real HTTP server
//	embed.NewClient(url, model, embed.WithHTTPDoer(myMock))
func WithHTTPDoer(d HTTPDoer) Option {
	return func(c *Client) { c.httpClient = d }
}

// NewClient creates a Client for the given endpoint. Returns nil if endpoint
// is empty (embedding disabled). model defaults to "nomic-embed-text" when
// empty, which produces 768-dimensional vectors and is available in Ollama
// without any extra setup.
func NewClient(endpoint, model string, opts ...Option) *Client {
	if endpoint == "" {
		return nil
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	c := &Client{
		endpoint:   endpoint,
		model:      model,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Embed returns a vector embedding for text.
// The format is auto-detected from the endpoint URL:
//   - "/v1/embeddings"  → OpenAI format
//   - otherwise         → Ollama format
//
// Returns (nil, nil) if the client is nil.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	if c == nil {
		return nil, nil
	}

	isOpenAI := strings.Contains(c.endpoint, "/v1/embeddings")

	var bodyMap map[string]interface{}
	if isOpenAI {
		// OpenAI format: {"model": "...", "input": "text"}
		bodyMap = map[string]interface{}{"model": c.model, "input": text}
	} else {
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

	resp, err := c.httpClient.Do(req)
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
		normed := normalizeL2(out.Data[0].Embedding)
		if normed == nil {
			return nil, fmt.Errorf("embedding contains NaN/Inf values")
		}
		return normed, nil
	}

	// Ollama format: {"embedding": [float, ...]}
	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding from endpoint")
	}
	normed := normalizeL2(out.Embedding)
	if normed == nil {
		return nil, fmt.Errorf("embedding contains NaN/Inf values")
	}
	return normed, nil
}

// EmbedBatch returns vector embeddings for a batch of texts in one HTTP round-trip.
// Supports OpenAI batch format ({"model":...,"input":[...]}).
// Ollama does not support batch — falls back to serial Embed() calls.
// Returns (nil, nil) if the client is nil or texts is empty.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if c == nil || len(texts) == 0 {
		return nil, nil
	}

	isOpenAI := strings.Contains(c.endpoint, "/v1/embeddings")

	if !isOpenAI {
		// Ollama doesn't support batch — fall back to serial.
		return c.embedSerial(ctx, texts)
	}

	bodyMap := map[string]interface{}{"model": c.model, "input": texts}

	body, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed batch request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed batch endpoint returned %d", resp.StatusCode)
	}

	// OpenAI batch format: {"data": [{"embedding": [...]}, ...]}
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
		normed := normalizeL2(d.Embedding)
		if normed == nil {
			return nil, fmt.Errorf("batch embedding[%d] contains NaN/Inf values", i)
		}
		vecs[i] = normed
	}
	return vecs, nil
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

// normalizeL2 returns a unit-length copy of v. Returns v unchanged if already
// normalized (within tolerance) or if the vector has zero magnitude.
func normalizeL2(v []float32) []float32 {
	if len(v) == 0 {
		return v
	}
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil
		}
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil
	}
	if norm == 0 {
		return nil
	}
	if norm > 0.999 && norm < 1.001 {
		out := make([]float32, len(v))
		copy(out, v)
		return out
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}

// WarmUp is a no-op for the HTTP client embedder (model is managed by the remote server).
func (c *Client) WarmUp(_ context.Context) error { return nil }

// Model returns the configured embedding model name.
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.model
}

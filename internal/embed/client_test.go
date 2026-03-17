package embed_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SynapsesOS/synapses/internal/embed"
)

// mockDoer implements embed.HTTPDoer for unit tests — no real network needed.
type mockDoer struct {
	resp *http.Response
	err  error
}

func (m *mockDoer) Do(_ *http.Request) (*http.Response, error) {
	return m.resp, m.err
}

func newMockResponse(statusCode int, body interface{}) *http.Response {
	b, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(bytes.NewReader(b)),
		Header:     make(http.Header),
	}
}

var ctx = context.Background()

// ── NewClient / NewBrainClient ────────────────────────────────────────────────

func TestNewClient_EmptyEndpoint(t *testing.T) {
	c := embed.NewClient("", "")
	if c != nil {
		t.Error("expected nil client for empty endpoint")
	}
}

func TestNewClient_DefaultModel(t *testing.T) {
	c := embed.NewClient("http://localhost:11434/api/embeddings", "")
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_CustomModel(t *testing.T) {
	c := embed.NewClient("http://localhost:11434/api/embeddings", "mxbai-embed-large")
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewBrainClient_EmptyURL(t *testing.T) {
	c := embed.NewBrainClient("")
	if c != nil {
		t.Error("expected nil for empty brain URL")
	}
}

func TestNewBrainClient_ValidURL(t *testing.T) {
	c := embed.NewBrainClient("http://localhost:11435")
	if c == nil {
		t.Fatal("expected non-nil brain client")
	}
}

// ── Embed — nil client ────────────────────────────────────────────────────────

func TestEmbed_NilClient(t *testing.T) {
	var c *embed.Client
	vec, err := c.Embed(ctx, "hello world")
	if vec != nil || err != nil {
		t.Errorf("expected (nil, nil) for nil client, got (%v, %v)", vec, err)
	}
}

// ── Embed — Brain format ──────────────────────────────────────────────────────

func newBrainEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string][]float32{ //nolint:errcheck
			"embedding": {0.1, 0.2, 0.3},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbed_BrainFormat(t *testing.T) {
	srv := newBrainEmbedServer(t)
	c := embed.NewBrainClient(srv.URL)
	vec, err := c.Embed(ctx, "test input")
	if err != nil {
		t.Fatalf("Embed (brain): %v", err)
	}
	if len(vec) == 0 {
		t.Error("expected non-empty embedding")
	}
}

// ── Embed — OpenAI format ─────────────────────────────────────────────────────

func newOpenAIEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"data": []map[string]interface{}{
				{"embedding": []float32{0.4, 0.5, 0.6}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbed_OpenAIFormat(t *testing.T) {
	srv := newOpenAIEmbedServer(t)
	c := embed.NewClient(srv.URL+"/v1/embeddings", "text-embedding-ada-002")
	vec, err := c.Embed(ctx, "test input")
	if err != nil {
		t.Fatalf("Embed (OpenAI): %v", err)
	}
	if len(vec) == 0 {
		t.Error("expected non-empty embedding")
	}
}

func TestEmbed_OpenAIFormat_EmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"data": []interface{}{}}) //nolint:errcheck
	}))
	defer srv.Close()
	c := embed.NewClient(srv.URL+"/v1/embeddings", "model")
	_, err := c.Embed(ctx, "test")
	if err == nil {
		t.Error("expected error for empty OpenAI data")
	}
}

// ── Embed — Ollama format ─────────────────────────────────────────────────────

func newOllamaEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string][]float32{ //nolint:errcheck
			"embedding": {0.7, 0.8, 0.9},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbed_OllamaFormat(t *testing.T) {
	srv := newOllamaEmbedServer(t)
	c := embed.NewClient(srv.URL+"/api/embeddings", "nomic-embed-text")
	vec, err := c.Embed(ctx, "test input")
	if err != nil {
		t.Fatalf("Embed (Ollama): %v", err)
	}
	if len(vec) == 0 {
		t.Error("expected non-empty embedding")
	}
}

// ── Embed — error paths ───────────────────────────────────────────────────────

func TestEmbed_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := embed.NewClient(srv.URL+"/api/embeddings", "model")
	_, err := c.Embed(ctx, "test")
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestEmbed_Unreachable(t *testing.T) {
	c := embed.NewClient("http://127.0.0.1:19999/api/embeddings", "model")
	_, err := c.Embed(ctx, "test")
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}

// ── WithHTTPDoer — interface injection (AWS SDK / stripe-go pattern) ──────────

// TestWithHTTPDoer_NewClient verifies that WithHTTPDoer replaces the default
// HTTP transport. The injected mock returns a canned response without touching
// the network at all — pure unit test, zero I/O.
func TestWithHTTPDoer_NewClient_UsesInjectedTransport(t *testing.T) {
	mock := &mockDoer{
		resp: newMockResponse(http.StatusOK, map[string][]float32{
			"embedding": {0.1, 0.2, 0.3},
		}),
	}
	c := embed.NewClient("http://nowhere.invalid/api/embeddings", "model",
		embed.WithHTTPDoer(mock))
	vec, err := c.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("expected no error with injected doer, got %v", err)
	}
	if len(vec) != 3 {
		t.Errorf("expected 3 dims, got %d", len(vec))
	}
}

// TestWithHTTPDoer_NewBrainClient verifies injection on the Brain client path.
func TestWithHTTPDoer_NewBrainClient_UsesInjectedTransport(t *testing.T) {
	mock := &mockDoer{
		resp: newMockResponse(http.StatusOK, map[string][]float32{
			"embedding": {0.5, 0.6},
		}),
	}
	c := embed.NewBrainClient("http://nowhere.invalid", embed.WithHTTPDoer(mock))
	vec, err := c.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(vec) != 2 {
		t.Errorf("expected 2 dims, got %d", len(vec))
	}
}

// TestWithHTTPDoer_PropagatesTransportError verifies that transport-level errors
// (e.g. connection refused) are surfaced when using an injected doer.
func TestWithHTTPDoer_PropagatesTransportError(t *testing.T) {
	mock := &mockDoer{err: fmt.Errorf("dial tcp: connection refused")}
	c := embed.NewClient("http://nowhere.invalid/api/embeddings", "model",
		embed.WithHTTPDoer(mock))
	_, err := c.Embed(context.Background(), "hello")
	if err == nil {
		t.Error("expected error from injected transport, got nil")
	}
}

// ── Embed — decode error paths (malformed JSON from server) ───────────────────

// TestEmbed_OllamaBrainDecodeError covers the decode-error branch for the
// brain/Ollama shared response path ({"embedding": [...]}).
func TestEmbed_OllamaBrainDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := embed.NewClient(srv.URL+"/api/embeddings", "model")
	_, err := c.Embed(ctx, "test")
	if err == nil {
		t.Error("expected decode error for malformed JSON")
	}
}

// TestEmbed_OpenAIDecodeError covers the decode-error branch for the OpenAI
// response path ({"data": [{"embedding": [...]}]}).
func TestEmbed_OpenAIDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := embed.NewClient(srv.URL+"/v1/embeddings", "model")
	_, err := c.Embed(ctx, "test")
	if err == nil {
		t.Error("expected decode error for malformed OpenAI JSON")
	}
}

// ── EmbedBatch — decode error paths ───────────────────────────────────────────

// TestEmbedBatch_BrainDecodeError covers the Brain batch decode-error branch.
func TestEmbedBatch_BrainDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := embed.NewBrainClient(srv.URL)
	_, err := c.EmbedBatch(ctx, []string{"a", "b"})
	if err == nil {
		t.Error("expected decode error for malformed Brain batch JSON")
	}
}

// TestEmbedBatch_OpenAIDecodeError covers the OpenAI batch decode-error branch.
func TestEmbedBatch_OpenAIDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := embed.NewClient(srv.URL+"/v1/embeddings", "model")
	_, err := c.EmbedBatch(ctx, []string{"a", "b"})
	if err == nil {
		t.Error("expected decode error for malformed OpenAI batch JSON")
	}
}

// ── Embed — empty embedding error paths ───────────────────────────────────────

// TestEmbed_OllamaEmptyEmbedding covers the "empty embedding" error for
// brain/Ollama format ({"embedding": []}).
func TestEmbed_OllamaEmptyEmbedding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string][]float32{"embedding": {}}) //nolint:errcheck
	}))
	defer srv.Close()
	c := embed.NewClient(srv.URL+"/api/embeddings", "model")
	_, err := c.Embed(ctx, "test")
	if err == nil {
		t.Error("expected error for empty Ollama embedding")
	}
}

// TestEmbed_OllamaBrainEmptyEmbeddingViaBrainClient covers the same empty-
// embedding branch reached through NewBrainClient (/v1/embed path).
func TestEmbed_BrainEmptyEmbedding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string][]float32{"embedding": {}}) //nolint:errcheck
	}))
	defer srv.Close()
	c := embed.NewBrainClient(srv.URL)
	_, err := c.Embed(ctx, "test")
	if err == nil {
		t.Error("expected error for empty Brain embedding")
	}
}

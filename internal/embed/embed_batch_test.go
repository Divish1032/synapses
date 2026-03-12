package embed_test

// Additional tests targeting EmbedBatch, embedSerial (via Ollama batch path),
// Model, and Endpoint accessors — bringing embed coverage from 40% → ~90%.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SynapsesOS/synapses/internal/embed"
)

// ── EmbedBatch — nil client / empty input ────────────────────────────────────

func TestEmbedBatch_NilClient(t *testing.T) {
	var c *embed.Client
	vecs, err := c.EmbedBatch(ctx, []string{"a", "b"})
	if vecs != nil || err != nil {
		t.Errorf("expected (nil, nil) for nil client, got (%v, %v)", vecs, err)
	}
}

func TestEmbedBatch_EmptyTexts(t *testing.T) {
	c := embed.NewClient("http://localhost:9999/api/embeddings", "model")
	vecs, err := c.EmbedBatch(ctx, nil)
	if vecs != nil || err != nil {
		t.Errorf("expected (nil, nil) for empty texts, got (%v, %v)", vecs, err)
	}
}

// ── EmbedBatch — Brain batch format (/v1/embed) ───────────────────────────────

func newBrainBatchServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"embeddings": [][]float32{{0.1, 0.2}, {0.3, 0.4}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbedBatch_BrainFormat(t *testing.T) {
	srv := newBrainBatchServer(t)
	c := embed.NewBrainClient(srv.URL)
	vecs, err := c.EmbedBatch(ctx, []string{"hello", "world"})
	if err != nil {
		t.Fatalf("EmbedBatch (brain): %v", err)
	}
	if len(vecs) != 2 {
		t.Errorf("expected 2 embeddings, got %d", len(vecs))
	}
}

func TestEmbedBatch_BrainFormat_LengthMismatch(t *testing.T) {
	// Server returns 1 embedding but we asked for 2 — should error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"embeddings": [][]float32{{0.1, 0.2}},
		})
	}))
	defer srv.Close()
	c := embed.NewBrainClient(srv.URL)
	_, err := c.EmbedBatch(ctx, []string{"hello", "world"})
	if err == nil {
		t.Error("expected error for length mismatch")
	}
}

// ── EmbedBatch — OpenAI batch format (/v1/embeddings) ─────────────────────────

func newOpenAIBatchServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"data": []map[string]interface{}{
				{"embedding": []float32{0.1, 0.2}},
				{"embedding": []float32{0.3, 0.4}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbedBatch_OpenAIFormat(t *testing.T) {
	srv := newOpenAIBatchServer(t)
	c := embed.NewClient(srv.URL+"/v1/embeddings", "text-embedding-ada-002")
	vecs, err := c.EmbedBatch(ctx, []string{"foo", "bar"})
	if err != nil {
		t.Fatalf("EmbedBatch (OpenAI): %v", err)
	}
	if len(vecs) != 2 {
		t.Errorf("expected 2 embeddings, got %d", len(vecs))
	}
}

func TestEmbedBatch_OpenAIFormat_LengthMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"data": []map[string]interface{}{
				{"embedding": []float32{0.1}},
			},
		})
	}))
	defer srv.Close()
	c := embed.NewClient(srv.URL+"/v1/embeddings", "model")
	_, err := c.EmbedBatch(ctx, []string{"foo", "bar"})
	if err == nil {
		t.Error("expected error for OpenAI length mismatch")
	}
}

// ── EmbedBatch — Ollama serial fallback (/api/embeddings) ─────────────────────

func newOllamaBatchServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string][]float32{ //nolint:errcheck
			"embedding": {0.5, 0.6, 0.7},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbedBatch_OllamaFallbackSerial(t *testing.T) {
	// Ollama doesn't support batch; EmbedBatch falls back to embedSerial.
	srv := newOllamaBatchServer(t)
	c := embed.NewClient(srv.URL+"/api/embeddings", "nomic-embed-text")
	vecs, err := c.EmbedBatch(ctx, []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("EmbedBatch (Ollama serial fallback): %v", err)
	}
	if len(vecs) != 2 {
		t.Errorf("expected 2 embeddings, got %d", len(vecs))
	}
}

func TestEmbedBatch_OllamaSerial_ErrorPropagates(t *testing.T) {
	// Server returns 500 → embedSerial should propagate the error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := embed.NewClient(srv.URL+"/api/embeddings", "model")
	_, err := c.EmbedBatch(ctx, []string{"a", "b"})
	if err == nil {
		t.Error("expected error for 500 in serial fallback")
	}
}

// ── EmbedBatch — HTTP error paths ─────────────────────────────────────────────

func TestEmbedBatch_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	c := embed.NewBrainClient(srv.URL)
	_, err := c.EmbedBatch(ctx, []string{"hello"})
	if err == nil {
		t.Error("expected error for 400 response")
	}
}

func TestEmbedBatch_Unreachable(t *testing.T) {
	c := embed.NewBrainClient("http://127.0.0.1:19999")
	_, err := c.EmbedBatch(ctx, []string{"hello"})
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}

// ── Model / Endpoint accessors ────────────────────────────────────────────────

func TestModel_NilClient(t *testing.T) {
	var c *embed.Client
	if m := c.Model(); m != "" {
		t.Errorf("expected empty model for nil client, got %q", m)
	}
}

func TestModel_NonNil(t *testing.T) {
	c := embed.NewClient("http://localhost:11434/api/embeddings", "my-model")
	if m := c.Model(); m != "my-model" {
		t.Errorf("expected %q, got %q", "my-model", m)
	}
}

func TestEndpoint_NilClient(t *testing.T) {
	var c *embed.Client
	if e := c.Endpoint(); e != "" {
		t.Errorf("expected empty endpoint for nil client, got %q", e)
	}
}

func TestEndpoint_NonNil(t *testing.T) {
	url := "http://localhost:11434/api/embeddings"
	c := embed.NewClient(url, "model")
	if e := c.Endpoint(); e != url {
		t.Errorf("expected %q, got %q", url, e)
	}
}

func TestEndpoint_BrainClient(t *testing.T) {
	c := embed.NewBrainClient("http://localhost:11435")
	if e := c.Endpoint(); e == "" {
		t.Error("expected non-empty endpoint for brain client")
	}
}

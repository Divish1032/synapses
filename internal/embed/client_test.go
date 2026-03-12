package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SynapsesOS/synapses/internal/embed"
)

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

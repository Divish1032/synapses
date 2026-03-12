package scout_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SynapsesOS/synapses/internal/scout"
)

var ctx = context.Background()

func newScoutServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/search", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(scout.SearchResponse{ //nolint:errcheck
			Hits: []scout.SearchHit{{Title: "result", URL: "http://example.com"}},
		})
	})
	mux.HandleFunc("/v1/fetch", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(scout.FetchResponse{ContentMD: "# page", Title: "Test"}) //nolint:errcheck
	})
	mux.HandleFunc("/v1/deep-search", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(scout.DeepSearchResponse{Query: "go concurrency"}) //nolint:errcheck
	})
	mux.HandleFunc("/v1/lookup-docs", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(scout.LookupDocsResponse{Content: "## docs", Title: "Docs"}) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestNewClient_DefaultTimeout(t *testing.T) {
	c := scout.NewClient("http://localhost:11436", 0)
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_CustomTimeout(t *testing.T) {
	c := scout.NewClient("http://localhost:11436", 10)
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestHealth_Success(t *testing.T) {
	srv := newScoutServer(t)
	c := scout.NewClient(srv.URL, 5)
	if !c.Health(ctx) {
		t.Error("expected Health to return true")
	}
}

func TestHealth_Unreachable(t *testing.T) {
	c := scout.NewClient("http://127.0.0.1:19999", 1)
	if c.Health(ctx) {
		t.Error("expected Health to return false for unreachable server")
	}
}

func TestSearch_Success(t *testing.T) {
	srv := newScoutServer(t)
	c := scout.NewClient(srv.URL, 5)
	resp := c.Search(ctx, scout.SearchRequest{Query: "sqlite go driver"})
	if resp == nil {
		t.Fatal("expected non-nil SearchResponse")
	}
	if len(resp.Hits) == 0 {
		t.Error("expected at least one search hit")
	}
}

func TestSearch_Unreachable(t *testing.T) {
	c := scout.NewClient("http://127.0.0.1:19999", 1)
	resp := c.Search(ctx, scout.SearchRequest{Query: "test"})
	if resp != nil {
		t.Error("expected nil for unreachable scout")
	}
}

func TestFetch_Success(t *testing.T) {
	srv := newScoutServer(t)
	c := scout.NewClient(srv.URL, 5)
	resp := c.Fetch(ctx, scout.FetchRequest{Input: "http://example.com"})
	if resp == nil {
		t.Fatal("expected non-nil FetchResponse")
	}
	if resp.ContentMD == "" {
		t.Error("expected non-empty ContentMD")
	}
}

func TestFetch_Unreachable(t *testing.T) {
	c := scout.NewClient("http://127.0.0.1:19999", 1)
	resp := c.Fetch(ctx, scout.FetchRequest{Input: "http://example.com"})
	if resp != nil {
		t.Error("expected nil for unreachable scout")
	}
}

func TestDeepSearch_Success(t *testing.T) {
	srv := newScoutServer(t)
	c := scout.NewClient(srv.URL, 5)
	resp := c.DeepSearch(ctx, scout.DeepSearchRequest{Query: "go concurrency"})
	if resp == nil {
		t.Fatal("expected non-nil DeepSearchResponse")
	}
}

func TestDeepSearch_Unreachable(t *testing.T) {
	c := scout.NewClient("http://127.0.0.1:19999", 1)
	resp := c.DeepSearch(ctx, scout.DeepSearchRequest{Query: "test"})
	if resp != nil {
		t.Error("expected nil for unreachable scout")
	}
}

func TestLookupDocs_Success(t *testing.T) {
	srv := newScoutServer(t)
	c := scout.NewClient(srv.URL, 5)
	resp := c.LookupDocs(ctx, scout.LookupDocsRequest{Query: "sql.Open"})
	if resp == nil {
		t.Fatal("expected non-nil LookupDocsResponse")
	}
	if resp.Content == "" {
		t.Error("expected non-empty markdown")
	}
}

func TestLookupDocs_Unreachable(t *testing.T) {
	c := scout.NewClient("http://127.0.0.1:19999", 1)
	resp := c.LookupDocs(ctx, scout.LookupDocsRequest{Query: "test"})
	if resp != nil {
		t.Error("expected nil for unreachable scout")
	}
}

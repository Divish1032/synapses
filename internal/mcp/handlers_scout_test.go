package mcp

// Tests for scout_tools.go handlers.
// Covers nil scout client (all handlers return tool errors gracefully),
// missing required parameters, and success paths via a real httptest server.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SynapsesOS/synapses/internal/scout"
)

// newScoutTestServer returns a test HTTP server serving all scout endpoints.
func newScoutTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/search", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(scout.SearchResponse{ //nolint:errcheck
			Query: "test",
			Hits:  []scout.SearchHit{{Title: "Result", URL: "http://example.com", Snippet: "snip"}},
			Count: 1,
		})
	})
	mux.HandleFunc("/v1/fetch", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(scout.FetchResponse{ //nolint:errcheck
			URL:       "http://example.com",
			Title:     "Example",
			ContentMD: "# Example",
			WordCount: 3,
		})
	})
	mux.HandleFunc("/v1/deep-search", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(scout.DeepSearchResponse{ //nolint:errcheck
			Query: "deep",
			Hits:  []scout.SearchHit{{Title: "Deep Result", URL: "http://example.com/deep"}},
			Count: 1,
		})
	})
	mux.HandleFunc("/v1/lookup-docs", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(scout.LookupDocsResponse{ //nolint:errcheck
			Query:   "go http client",
			Content: "HTTP client documentation...",
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// wireScout creates a Server with the scout client pointed at scoutSrv.
func wireScout(t *testing.T, scoutSrv *httptest.Server) *Server {
	t.Helper()
	s := newTestServer(t)
	s.SetScoutClient(scout.NewClient(scoutSrv.URL, 5))
	return s
}

// ── getScoutClient ────────────────────────────────────────────────────────────

func TestGetScoutClient_Nil(t *testing.T) {
	s := newTestServer(t)
	if sc := s.getScoutClient(); sc != nil {
		t.Error("expected nil scout client when not wired")
	}
}

func TestGetScoutClient_Wired(t *testing.T) {
	srv := newScoutTestServer(t)
	s := wireScout(t, srv)
	if sc := s.getScoutClient(); sc == nil {
		t.Error("expected non-nil scout client after SetScoutClient")
	}
}

// ── handleWebSearch ───────────────────────────────────────────────────────────

func TestHandleWebSearch_NilScout(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleWebSearch(ctx, callTool(map[string]any{"query": "go testing"}))
	mustErrorResult(t, res, err)
}

func TestHandleWebSearch_NoQuery(t *testing.T) {
	srv := newScoutTestServer(t)
	s := wireScout(t, srv)
	res, err := s.handleWebSearch(ctx, callTool(map[string]any{}))
	mustErrorResult(t, res, err)
}

func TestHandleWebSearch_Success(t *testing.T) {
	srv := newScoutTestServer(t)
	s := wireScout(t, srv)
	res, err := s.handleWebSearch(ctx, callTool(map[string]any{"query": "go testing"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "hits")
	hasKey(t, m, "count")
}

func TestHandleWebSearch_WithOptions(t *testing.T) {
	srv := newScoutTestServer(t)
	s := wireScout(t, srv)
	args := map[string]any{
		"query":       "go testing",
		"max_results": float64(3),
		"region":      "us",
		"timelimit":   "d",
	}
	res, err := s.handleWebSearch(ctx, callTool(args))
	m := mustResult(t, res, err)
	hasKey(t, m, "query")
}

// ── handleWebFetch ────────────────────────────────────────────────────────────

func TestHandleWebFetch_NilScout(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleWebFetch(ctx, callTool(map[string]any{"input": "http://example.com"}))
	mustErrorResult(t, res, err)
}

func TestHandleWebFetch_NoInput(t *testing.T) {
	srv := newScoutTestServer(t)
	s := wireScout(t, srv)
	res, err := s.handleWebFetch(ctx, callTool(map[string]any{}))
	mustErrorResult(t, res, err)
}

func TestHandleWebFetch_Success(t *testing.T) {
	srv := newScoutTestServer(t)
	s := wireScout(t, srv)
	res, err := s.handleWebFetch(ctx, callTool(map[string]any{"input": "http://example.com"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "url")
	hasKey(t, m, "content_md")
}

func TestHandleWebFetch_ForceRefresh(t *testing.T) {
	srv := newScoutTestServer(t)
	s := wireScout(t, srv)
	args := map[string]any{"input": "http://example.com", "force_refresh": true}
	res, err := s.handleWebFetch(ctx, callTool(args))
	m := mustResult(t, res, err)
	hasKey(t, m, "title")
}

// ── handleWebAnnotate ─────────────────────────────────────────────────────────

func TestHandleWebAnnotate_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleWebAnnotate(ctx, callTool(map[string]any{
		"node_id": "node-x",
		"note":    "test",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleWebAnnotate_NoNodeID(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleWebAnnotate(ctx, callTool(map[string]any{"note": "test note"}))
	mustErrorResult(t, res, err)
}

func TestHandleWebAnnotate_NoNoteOrHits(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleWebAnnotate(ctx, callTool(map[string]any{"node_id": "x"}))
	mustErrorResult(t, res, err)
}

func TestHandleWebAnnotate_WithNote(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	args := map[string]any{
		"node_id":  string(loginID),
		"note":     "Found relevant docs",
		"agent_id": "test-agent",
	}
	res, err := s.handleWebAnnotate(ctx, callTool(args))
	m := mustResult(t, res, err)
	hasKey(t, m, "id")
	hasKey(t, m, "status")
}

func TestHandleWebAnnotate_WithHitsJSON(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	hits := `[{"title":"Go Docs","url":"https://pkg.go.dev","snippet":"The Go standard library"}]`
	args := map[string]any{
		"node_id": string(loginID),
		"hits":    hits,
	}
	res, err := s.handleWebAnnotate(ctx, callTool(args))
	m := mustResult(t, res, err)
	hasKey(t, m, "id")
}

func TestHandleWebAnnotate_InvalidHitsJSON(t *testing.T) {
	// Bad JSON for hits → note stays empty → "note or hits is required" error.
	s, loginID, _ := newPopulatedServer(t)
	args := map[string]any{
		"node_id": string(loginID),
		"hits":    "not-json",
	}
	res, err := s.handleWebAnnotate(ctx, callTool(args))
	mustErrorResult(t, res, err)
}

// ── handleLookupDocs ──────────────────────────────────────────────────────────

func TestHandleLookupDocs_NilScout(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleLookupDocs(ctx, callTool(map[string]any{"query": "go http"}))
	mustErrorResult(t, res, err)
}

func TestHandleLookupDocs_NoQuery(t *testing.T) {
	srv := newScoutTestServer(t)
	s := wireScout(t, srv)
	res, err := s.handleLookupDocs(ctx, callTool(map[string]any{}))
	mustErrorResult(t, res, err)
}

func TestHandleLookupDocs_Success(t *testing.T) {
	srv := newScoutTestServer(t)
	s := wireScout(t, srv)
	args := map[string]any{"query": "go http client", "max_chars": float64(5000)}
	res, err := s.handleLookupDocs(ctx, callTool(args))
	m := mustResult(t, res, err)
	hasKey(t, m, "content")
}

func TestHandleLookupDocs_ErrorInResponse(t *testing.T) {
	// Scout returns a response with an error field set.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/lookup-docs", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(scout.LookupDocsResponse{Error: "no results found"}) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := newTestServer(t)
	s.SetScoutClient(scout.NewClient(srv.URL, 5))
	res, err := s.handleLookupDocs(ctx, callTool(map[string]any{"query": "something"}))
	mustErrorResult(t, res, err)
}

// ── handleWebDeepSearch ───────────────────────────────────────────────────────

func TestHandleWebDeepSearch_NilScout(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleWebDeepSearch(ctx, callTool(map[string]any{"query": "deep search"}))
	mustErrorResult(t, res, err)
}

func TestHandleWebDeepSearch_NoQuery(t *testing.T) {
	srv := newScoutTestServer(t)
	s := wireScout(t, srv)
	res, err := s.handleWebDeepSearch(ctx, callTool(map[string]any{}))
	mustErrorResult(t, res, err)
}

func TestHandleWebDeepSearch_Success(t *testing.T) {
	srv := newScoutTestServer(t)
	s := wireScout(t, srv)
	args := map[string]any{
		"query":       "dependency injection go",
		"max_results": float64(5),
	}
	res, err := s.handleWebDeepSearch(ctx, callTool(args))
	m := mustResult(t, res, err)
	hasKey(t, m, "hits")
	hasKey(t, m, "count")
}

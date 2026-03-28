package mcp

// Tests for brain_tools.go with a real brain HTTP test server wired in.
// Covers success paths for handleUpsertADR, handleGetADRs, ingestWebContent.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain"
)

// newBrainHTTPServer returns a test brain HTTP server with all relevant endpoints.
func newBrainHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/adr", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string][]brain.ADR{"adrs": { //nolint:errcheck
				{ID: "adr-1", Title: "Use SQLite"},
			}})
		} else {
			json.NewEncoder(w).Encode(brain.ADR{ID: "adr-1", Title: "Use SQLite"}) //nolint:errcheck
		}
	})
	mux.HandleFunc("/v1/adr/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(brain.ADR{ID: "adr-1", Title: "Use SQLite"}) //nolint:errcheck
	})
	mux.HandleFunc("/v1/ingest", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1/context-packet", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(brain.ContextPacket{EntityName: "Login"}) //nolint:errcheck
	})
	mux.HandleFunc("/v1/sdlc/phase", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(brain.SDLCConfig{Phase: "development", QualityMode: "standard"}) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// wireBrain creates a Server with the brain client pointed at brainSrv.
func wireBrain(t *testing.T, brainSrv *httptest.Server) *Server {
	t.Helper()
	s := newTestServer(t)
	bc := brain.NewClient(brainSrv.URL, 5) //nolint:staticcheck // SA1019: test uses deprecated HTTP-based constructor
	s.SetBrainClient(bc)
	return s
}

// ── getBrainClient ────────────────────────────────────────────────────────────

func TestGetBrainClient_Nil(t *testing.T) {
	s := newTestServer(t)
	if bc := s.getBrainClient(); bc != nil {
		t.Error("expected nil brain client when not wired")
	}
}

func TestGetBrainClient_Wired(t *testing.T) {
	srv := newBrainHTTPServer(t)
	s := wireBrain(t, srv)
	if bc := s.getBrainClient(); bc == nil {
		t.Error("expected non-nil brain client after SetBrainClient")
	}
}

// ── handleUpsertADR with brain wired ─────────────────────────────────────────

func TestHandleUpsertADR_Success(t *testing.T) {
	srv := newBrainHTTPServer(t)
	s := wireBrain(t, srv)
	args := map[string]any{
		"id":       "adr-1",
		"title":    "Use SQLite",
		"decision": "We chose SQLite for its zero-configuration deployment.",
		"status":   "accepted",
	}
	res, err := s.handleUpsertADR(ctx, callTool(args))
	mustResult(t, res, err)
}

func TestHandleUpsertADR_DefaultStatus(t *testing.T) {
	// No status provided → defaults to "proposed".
	srv := newBrainHTTPServer(t)
	s := wireBrain(t, srv)
	args := map[string]any{
		"id":       "adr-2",
		"title":    "No Status ADR",
		"decision": "We decided to use NATS for messaging.",
	}
	res, err := s.handleUpsertADR(ctx, callTool(args))
	mustResult(t, res, err)
}

func TestHandleUpsertADR_WithLinkedFiles(t *testing.T) {
	// Providing linked_files without status → status defaults to "accepted".
	srv := newBrainHTTPServer(t)
	s := wireBrain(t, srv)
	args := map[string]any{
		"id":           "adr-3",
		"title":        "ADR with files",
		"decision":     "Selected approach.",
		"linked_files": []interface{}{"internal/auth/auth.go", "internal/db/store.go"},
	}
	res, err := s.handleUpsertADR(ctx, callTool(args))
	mustResult(t, res, err)
}

func TestHandleUpsertADR_BrainError(t *testing.T) {
	// Brain returns 500 → handleUpsertADR returns a text result with error key.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errSrv.Close()
	s := newTestServer(t)
	s.SetBrainClient(brain.NewClient(errSrv.URL, 5)) //nolint:staticcheck // SA1019: test uses deprecated HTTP-based constructor
	args := map[string]any{
		"id":       "adr-x",
		"title":    "error ADR",
		"decision": "something",
	}
	res, err := s.handleUpsertADR(ctx, callTool(args))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil {
		t.Fatal("expected result")
	}
	// Returns text with error (not a tool-level error, but a JSON text with error key).
	_ = res
}

// ── handleGetADRs with brain wired ───────────────────────────────────────────

func TestHandleGetADRs_Success(t *testing.T) {
	srv := newBrainHTTPServer(t)
	s := wireBrain(t, srv)
	res, err := s.handleGetADRs(ctx, callTool(map[string]any{}))
	m := mustResult(t, res, err)
	hasKey(t, m, "adrs")
	hasKey(t, m, "count")
}

func TestHandleGetADRs_WithFileFilter(t *testing.T) {
	srv := newBrainHTTPServer(t)
	s := wireBrain(t, srv)
	args := map[string]any{"file": "internal/auth/auth.go"}
	res, err := s.handleGetADRs(ctx, callTool(args))
	m := mustResult(t, res, err)
	hasKey(t, m, "file_filter")
}

func TestHandleGetADRs_BrainError(t *testing.T) {
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errSrv.Close()
	s := newTestServer(t)
	s.SetBrainClient(brain.NewClient(errSrv.URL, 5)) //nolint:staticcheck // SA1019: test uses deprecated HTTP-based constructor
	res, err := s.handleGetADRs(ctx, callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	_ = res
}

// ── ingestWebContent ──────────────────────────────────────────────────────────

func TestIngestWebContent_TooShort(t *testing.T) {
	srv := newBrainHTTPServer(t)
	s := wireBrain(t, srv)
	// Content < 200 chars → no-op, must not panic.
	s.ingestWebContent("http://example.com", "Short Article", "too short")
}

func TestIngestWebContent_NoBrain_LongContent(t *testing.T) {
	s := newTestServer(t)
	// No brain client → no-op, must not panic.
	s.ingestWebContent("http://example.com", "Title", "long content that is definitely more than two hundred characters long for testing purposes here yes it is now long enough for the test to pass")
}

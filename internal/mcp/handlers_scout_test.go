package mcp

// Tests for scout_tools.go handlers.
// Covers handleWebAnnotate and handleLookupDocs.

import (
	"testing"
)

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

func TestHandleLookupDocs_NilWebCache(t *testing.T) {
	s := newTestServer(t)
	// webCache not set → should return tool error
	res, err := s.handleLookupDocs(ctx, callTool(map[string]any{"package": "github.com/foo/bar"}))
	mustErrorResult(t, res, err)
}

func TestHandleLookupDocs_NoParams(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleLookupDocs(ctx, callTool(map[string]any{}))
	mustErrorResult(t, res, err)
}

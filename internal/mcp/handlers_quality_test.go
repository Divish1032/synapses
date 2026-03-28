package mcp

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── handleUpsertGap ───────────────────────────────────────────────────────────

func TestHandleUpsertGap_HappyPath(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     "parser.go:DetectProvenance",
		"gap_id":      "dist-relative-path",
		"description": "dist/ relative path not matched when no leading component",
		"severity":    "medium",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "id")
	hasKey(t, m, "status_msg")
	if m["gap_id"] != "dist-relative-path" {
		t.Errorf("gap_id mismatch: got %v", m["gap_id"])
	}
	if m["status"] != "open" {
		t.Errorf("expected default status=open, got %v", m["status"])
	}
}

func TestHandleUpsertGap_Idempotent(t *testing.T) {
	s := newTestServer(t)
	args := map[string]any{
		"node_id":     "parser.go:DetectProvenance",
		"gap_id":      "dist-relative-path",
		"description": "original",
		"severity":    "low",
	}
	res1, err1 := s.handleUpsertGap(ctx, callTool(args))
	m1 := mustResult(t, res1, err1)

	args["description"] = "updated"
	args["severity"] = "high"
	res2, err2 := s.handleUpsertGap(ctx, callTool(args))
	m2 := mustResult(t, res2, err2)

	if m1["id"] != m2["id"] {
		t.Errorf("expected same ID on upsert: %v != %v", m1["id"], m2["id"])
	}
	if m2["description"] != "updated" {
		t.Errorf("description not updated: %v", m2["description"])
	}
}

func TestHandleUpsertGap_MarkFixed(t *testing.T) {
	s := newTestServer(t)
	// create
	_, _ = s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     "tools.go:handleSessionInit",
		"gap_id":      "session-reset",
		"description": "counter carries over on agent reconnect",
		"severity":    "medium",
	}))
	// mark fixed
	res, err := s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     "tools.go:handleSessionInit",
		"gap_id":      "session-reset",
		"description": "counter carries over on agent reconnect",
		"severity":    "medium",
		"status":      "fixed",
		"fix_notes":   "clear ctxCalls on session_init",
	}))
	m := mustResult(t, res, err)
	if m["status"] != "fixed" {
		t.Errorf("expected status=fixed, got %v", m["status"])
	}
}

func TestHandleUpsertGap_MissingNodeID(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertGap(ctx, callTool(map[string]any{
		"gap_id":      "some-gap",
		"description": "missing node_id",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleUpsertGap_MissingGapID(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     "parser.go:DetectProvenance",
		"description": "missing gap_id",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleUpsertGap_MissingDescription(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "parser.go:DetectProvenance",
		"gap_id":  "some-gap",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleUpsertGap_InvalidSeverity(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     "parser.go:DetectProvenance",
		"gap_id":      "some-gap",
		"description": "desc",
		"severity":    "extreme",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleUpsertGap_InvalidStatus(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     "parser.go:DetectProvenance",
		"gap_id":      "some-gap",
		"description": "desc",
		"status":      "resolved",
	}))
	mustErrorResult(t, res, err)
}

// ── handleGetGaps ─────────────────────────────────────────────────────────────

func TestHandleGetGaps_EmptyByDefault(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetGaps(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "gaps")
	hasKey(t, m, "count")
	if m["count"].(float64) != 0 {
		t.Errorf("expected 0 gaps in fresh store, got %v", m["count"])
	}
}

func TestHandleGetGaps_OpenFilter(t *testing.T) {
	s := newTestServer(t)
	// insert open + fixed
	_, _ = s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "n1", "gap_id": "g1", "description": "d", "severity": "medium",
	}))
	_, _ = s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "n2", "gap_id": "g2", "description": "d", "severity": "low", "status": "fixed",
	}))

	res, err := s.handleGetGaps(ctx, callTool(map[string]any{"status": "open"}))
	m := mustResult(t, res, err)
	if m["count"].(float64) != 1 {
		t.Errorf("expected 1 open gap, got %v", m["count"])
	}
}

func TestHandleGetGaps_AllFilter(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "n1", "gap_id": "g1", "description": "d", "severity": "medium",
	}))
	_, _ = s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "n1", "gap_id": "g1", "description": "d", "severity": "medium", "status": "fixed",
	}))

	res, err := s.handleGetGaps(ctx, callTool(map[string]any{"status": "all"}))
	m := mustResult(t, res, err)
	if m["count"].(float64) != 1 {
		t.Errorf("expected 1 gap (upserted same gap_id), got %v", m["count"])
	}
}

func TestHandleGetGaps_HasHint(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetGaps(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "hint")
}

// TestHandleGetGaps_FileMetacharSafety verifies that LIKE metacharacters in the
// file filter do not match unrelated gaps (Security F11 — GetGaps path).
func TestHandleGetGaps_FileMetacharSafety(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// Upsert a gap attached to a real node (uses the server's graph).
	_, err := srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     "AuthLogin",
		"gap_id":      "test-gap",
		"description": "test gap for LIKE escape test",
		"severity":    "low",
	}))
	if err != nil {
		t.Fatalf("upsert gap: %v", err)
	}

	// file="%" must not match gaps whose node_id does not contain a literal "%".
	res, err := srv.handleGetGaps(ctx, callTool(map[string]any{
		"file":   "%",
		"status": "all",
	}))
	if err != nil {
		t.Fatalf("handleGetGaps file=%%: %v", err)
	}
	m := mustResult(t, res, err)
	gaps, _ := m["gaps"].([]interface{})
	if len(gaps) != 0 {
		t.Errorf("file=%% matched %d gap(s); want 0 — LIKE metachar not escaped in GetGaps", len(gaps))
	}
}

// TestUpsertGap_BareNameResolvesToGraphID verifies that passing a bare function
// name as node_id gets resolved to a canonical "{repoID}::{file}::{name}" ID
// so the gap surfaces in get_context() queries rather than being silently lost.
func TestUpsertGap_BareNameResolvesToGraphID(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	// Pass bare name — no "::" separators.
	res, err := srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     "AuthLogin",
		"gap_id":      "missing-rate-limit",
		"description": "no rate limiting on login endpoint",
		"severity":    "high",
	}))
	m := mustResult(t, res, err)

	// The stored node_id must be the canonical graph ID, not the bare name.
	storedNodeID, _ := m["node_id"].(string)
	if storedNodeID == "AuthLogin" {
		t.Errorf("node_id was not resolved: still bare name %q (expected canonical ID like %q)", storedNodeID, string(loginID))
	}
	if storedNodeID != string(loginID) {
		t.Errorf("resolved node_id %q != expected canonical ID %q", storedNodeID, string(loginID))
	}

	// The status_msg must NOT contain a "not found" warning.
	statusMsg, _ := m["status_msg"].(string)
	if containsStr(statusMsg, "not found") {
		t.Errorf("unexpected warning in status_msg: %q", statusMsg)
	}
}

// TestUpsertGap_UnknownBareNameWarns verifies that a bare name with no graph
// match produces a warning in status_msg instead of silently storing.
func TestUpsertGap_UnknownBareNameWarns(t *testing.T) {
	s := newTestServer(t) // empty graph

	res, err := s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     "NonExistentFunction",
		"gap_id":      "some-gap",
		"description": "d",
		"severity":    "low",
	}))
	m := mustResult(t, res, err)

	statusMsg, _ := m["status_msg"].(string)
	if !containsStr(statusMsg, "not found") {
		t.Errorf("expected 'not found' warning in status_msg for unknown bare name, got: %q", statusMsg)
	}
}

// TestGetViolations_StoreNil_HasZeroValueKeys verifies that handleGetViolations
// always writes open_quality_gaps and quality_gap_count even when the store is
// nil (index-only mode). Callers must not panic on type-asserting these keys.
func TestGetViolations_StoreNil_HasZeroValueKeys(t *testing.T) {
	g := newGraphForTest(t)
	cfg := newConfigForTest(t)
	srv := New(g, cfg, nil) // explicitly nil store
	t.Cleanup(func() { srv.Close() })

	res, err := srv.handleGetViolations(ctx, callTool(nil))
	m := mustResult(t, res, err)

	// Both keys must exist, even with nil store.
	if _, ok := m["open_quality_gaps"]; !ok {
		t.Error("open_quality_gaps key missing when store is nil")
	}
	if _, ok := m["quality_gap_count"]; !ok {
		t.Error("quality_gap_count key missing when store is nil")
	}
	// Values must be the zero defaults.
	if count, _ := m["quality_gap_count"].(float64); count != 0 {
		t.Errorf("expected quality_gap_count=0, got %v", count)
	}
}

// containsStr reports whether substr appears in s. Named to avoid colliding with
// any standard-library import while staying readable in test assertions.
func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return len(substr) == 0
}

func newGraphForTest(t *testing.T) *graph.Graph {
	t.Helper()
	return graph.New("test-repo")
}

func newConfigForTest(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

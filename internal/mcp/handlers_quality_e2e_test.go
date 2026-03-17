package mcp

// End-to-end tests for the R32 Quality Intelligence Layer.
//
// Each test exercises the full data path:
//   upsert_gap → persisted in SQLite → surfaces in get_context / get_violations / session_init
//
// Coverage matrix:
//   - get_context (JSON format):  quality_gaps field populated on entity with open gap
//   - get_context (compact text): ⚠ warning lines rendered before annotations
//   - get_violations:             open_quality_gaps + quality_gap_count always present
//   - session_init working_state: open_quality_gaps count reflects reality
//   - full lifecycle:             open → fixed removes from all surfaces
//   - cross-entity isolation:     gap on A does not appear in get_context for B
//   - get_gaps file filter:       file= returns gaps for all nodes in that file

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// rawText extracts the plain text body from a tool result.
// Works for both text-content results (format=compact) and JSON results.
func rawText(t *testing.T, result *mcp.CallToolResult, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
	if result.IsError {
		t.Fatalf("tool returned error: %v", result.Content)
	}
	if len(result.Content) == 0 {
		t.Fatal("empty content")
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	return tc.Text
}

// ── get_context — JSON path ───────────────────────────────────────────────────

// TestE2E_GetContext_ShowsOpenGap verifies that when a quality gap is recorded
// on a node, get_context(entity=...) surfaces it in the quality_gaps JSON field.
func TestE2E_GetContext_ShowsOpenGap(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	// Record a gap on AuthLogin using its real graph node ID.
	_, err := srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     string(loginID),
		"gap_id":      "missing-rate-limit",
		"description": "No rate limiting applied before token issuance",
		"severity":    "high",
		"agent_id":    "agent-review",
	}))
	if err != nil {
		t.Fatalf("upsert_gap: %v", err)
	}

	// get_context (default JSON format).
	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	raw := rawText(t, res, err)

	var dc map[string]any
	if err := json.Unmarshal([]byte(raw), &dc); err != nil {
		t.Fatalf("unmarshal get_context: %v\nraw: %s", err, raw)
	}

	gaps, ok := dc["quality_gaps"]
	if !ok {
		t.Fatal("quality_gaps key missing from get_context response")
	}
	gapList, ok := gaps.([]any)
	if !ok || len(gapList) == 0 {
		t.Fatalf("expected non-empty quality_gaps, got: %v", gaps)
	}
	// Verify gap fields are present.
	g0, _ := gapList[0].(map[string]any)
	if g0["gap_id"] != "missing-rate-limit" {
		t.Errorf("gap_id mismatch: %v", g0["gap_id"])
	}
	if g0["severity"] != "high" {
		t.Errorf("severity mismatch: %v", g0["severity"])
	}
}

// TestE2E_GetContext_NoGapOnOtherEntity verifies cross-entity isolation:
// a gap on AuthLogin must NOT appear when get_context is called for AuthLogout.
func TestE2E_GetContext_NoGapOnOtherEntity(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     string(loginID),
		"gap_id":      "login-only-gap",
		"description": "only on login",
		"severity":    "low",
	}))

	// get_context for AuthLogout — must NOT contain the gap.
	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogout",
		"format": "json",
	}))
	raw := rawText(t, res, err)

	var dc map[string]any
	if err := json.Unmarshal([]byte(raw), &dc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if gaps, ok := dc["quality_gaps"]; ok {
		if gapList, ok := gaps.([]any); ok && len(gapList) > 0 {
			t.Errorf("expected no quality_gaps on AuthLogout, got %d", len(gapList))
		}
	}
}

// ── get_context — compact text path ──────────────────────────────────────────

// TestE2E_GetContext_Compact_RendersGapWarning verifies that the compact text
// output includes the ⚠ quality gap header before annotations.
func TestE2E_GetContext_Compact_RendersGapWarning(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     string(loginID),
		"gap_id":      "no-audit-log",
		"description": "login events not written to audit log",
		"severity":    "medium",
	}))

	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "compact",
	}))
	text := rawText(t, res, err)

	if !strings.Contains(text, "open quality gap") {
		t.Errorf("compact output missing quality gap warning; got:\n%s", text)
	}
	if !strings.Contains(text, "no-audit-log") {
		t.Errorf("compact output missing gap_id; got:\n%s", text)
	}
	if !strings.Contains(text, "[medium]") {
		t.Errorf("compact output missing severity tag; got:\n%s", text)
	}
}

// TestE2E_GetContext_Compact_NoGap_NoWarning verifies that when no gaps exist
// the compact output contains no quality gap warning lines.
func TestE2E_GetContext_Compact_NoGap_NoWarning(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "compact",
	}))
	text := rawText(t, res, err)

	if strings.Contains(text, "open quality gap") {
		t.Errorf("unexpected quality gap warning in clean compact output:\n%s", text)
	}
}

// ── get_violations ────────────────────────────────────────────────────────────

// TestE2E_GetViolations_AlwaysHasGapKeys verifies open_quality_gaps and
// quality_gap_count are present even when there are no gaps (zero-value contract).
func TestE2E_GetViolations_AlwaysHasGapKeys(t *testing.T) {
	srv := newTestServer(t)

	res, err := srv.handleGetViolations(ctx, callTool(nil))
	m := mustResult(t, res, err)

	hasKey(t, m, "open_quality_gaps")
	hasKey(t, m, "quality_gap_count")
	if m["quality_gap_count"].(float64) != 0 {
		t.Errorf("expected 0 gaps in fresh server, got %v", m["quality_gap_count"])
	}
	gaps := m["open_quality_gaps"].([]any)
	if len(gaps) != 0 {
		t.Errorf("expected empty gap list, got %v", gaps)
	}
}

// TestE2E_GetViolations_ListsOpenGaps verifies recorded gaps appear in the list.
func TestE2E_GetViolations_ListsOpenGaps(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "n1", "gap_id": "g1", "description": "first gap", "severity": "high",
	}))
	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "n2", "gap_id": "g2", "description": "second gap", "severity": "medium",
	}))

	res, err := srv.handleGetViolations(ctx, callTool(nil))
	m := mustResult(t, res, err)

	if m["quality_gap_count"].(float64) != 2 {
		t.Errorf("expected 2 open gaps, got %v", m["quality_gap_count"])
	}
	gaps := m["open_quality_gaps"].([]any)
	if len(gaps) != 2 {
		t.Errorf("expected 2 gaps in list, got %d", len(gaps))
	}
}

// TestE2E_GetViolations_FixedGapsNotListed verifies fixed gaps are excluded.
func TestE2E_GetViolations_FixedGapsNotListed(t *testing.T) {
	srv := newTestServer(t)

	// Create then immediately fix a gap.
	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "n1", "gap_id": "g1", "description": "gap", "severity": "medium",
	}))
	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "n1", "gap_id": "g1", "description": "gap", "severity": "medium",
		"status": "fixed", "fix_notes": "implemented",
	}))

	res, err := srv.handleGetViolations(ctx, callTool(nil))
	m := mustResult(t, res, err)

	if m["quality_gap_count"].(float64) != 0 {
		t.Errorf("expected 0 open gaps after fix, got %v", m["quality_gap_count"])
	}
}

// ── session_init working_state ────────────────────────────────────────────────

// TestE2E_SessionInit_IncludesGapCount verifies that session_init returns
// open_quality_gaps in working_state so agents know the tech debt state on startup.
func TestE2E_SessionInit_IncludesGapCount(t *testing.T) {
	srv := newTestServer(t)

	// Record two gaps.
	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "n1", "gap_id": "gap-a", "description": "desc", "severity": "low",
	}))
	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "n2", "gap_id": "gap-b", "description": "desc", "severity": "medium",
	}))

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	m := mustResult(t, res, err)

	ws, ok := m["working_state"].(map[string]any)
	if !ok {
		t.Fatalf("working_state not a map: %T", m["working_state"])
	}
	count, ok := ws["open_quality_gaps"]
	if !ok {
		t.Fatal("open_quality_gaps missing from working_state")
	}
	if count.(float64) != 2 {
		t.Errorf("expected open_quality_gaps=2, got %v", count)
	}
}

// TestE2E_SessionInit_ZeroGapCount verifies the key is present even when
// there are no gaps (zero-value contract for agents checking the field).
func TestE2E_SessionInit_ZeroGapCount(t *testing.T) {
	srv := newTestServer(t)

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	m := mustResult(t, res, err)

	ws, ok := m["working_state"].(map[string]any)
	if !ok {
		t.Fatalf("working_state not a map: %T", m["working_state"])
	}
	count, ok := ws["open_quality_gaps"]
	if !ok {
		t.Fatal("open_quality_gaps missing from working_state even when zero")
	}
	if count.(float64) != 0 {
		t.Errorf("expected 0, got %v", count)
	}
}

// ── Full lifecycle ────────────────────────────────────────────────────────────

// TestE2E_FullLifecycle_OpenToFixed exercises the complete quality gap lifecycle:
// open → surfaces everywhere → fixed → removed from all surfaces.
func TestE2E_FullLifecycle_OpenToFixed(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)
	nodeID := string(loginID)

	// Step 1: record gap.
	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     nodeID,
		"gap_id":      "lifecycle-gap",
		"description": "test lifecycle gap",
		"severity":    "medium",
	}))

	// Step 2: verify it surfaces in get_context.
	res1, err1 := srv.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin"}))
	raw1 := rawText(t, res1, err1)
	var dc1 map[string]any
	if err := json.Unmarshal([]byte(raw1), &dc1); err != nil {
		t.Fatalf("unmarshal step 2: %v", err)
	}
	gaps1, _ := dc1["quality_gaps"].([]any)
	if len(gaps1) != 1 {
		t.Errorf("step 2: expected 1 gap in get_context, got %d", len(gaps1))
	}

	// Step 3: verify it surfaces in get_violations.
	res2, err2 := srv.handleGetViolations(ctx, callTool(nil))
	m2 := mustResult(t, res2, err2)
	if m2["quality_gap_count"].(float64) != 1 {
		t.Errorf("step 3: expected 1 gap in get_violations, got %v", m2["quality_gap_count"])
	}

	// Step 4: mark as fixed.
	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     nodeID,
		"gap_id":      "lifecycle-gap",
		"description": "test lifecycle gap",
		"severity":    "medium",
		"status":      "fixed",
		"fix_notes":   "fixed in PR #42",
	}))

	// Step 5: gap must be gone from get_context.
	res3, err3 := srv.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin"}))
	raw3 := rawText(t, res3, err3)
	var dc3 map[string]any
	if err := json.Unmarshal([]byte(raw3), &dc3); err != nil {
		t.Fatalf("unmarshal step 5: %v", err)
	}
	if gaps3, ok := dc3["quality_gaps"].([]any); ok && len(gaps3) > 0 {
		t.Errorf("step 5: expected 0 gaps after fix, got %d", len(gaps3))
	}

	// Step 6: gap must be gone from get_violations open list.
	res4, err4 := srv.handleGetViolations(ctx, callTool(nil))
	m4 := mustResult(t, res4, err4)
	if m4["quality_gap_count"].(float64) != 0 {
		t.Errorf("step 6: expected 0 open gaps after fix, got %v", m4["quality_gap_count"])
	}

	// Step 7: gap is still retrievable via get_gaps(status="fixed").
	res5, err5 := srv.handleGetGaps(ctx, callTool(map[string]any{
		"node_id": nodeID,
		"status":  "fixed",
	}))
	m5 := mustResult(t, res5, err5)
	if m5["count"].(float64) != 1 {
		t.Errorf("step 7: expected 1 fixed gap, got %v", m5["count"])
	}
}

// ── get_gaps — file filter ────────────────────────────────────────────────────

// TestE2E_GetGaps_FileFilter verifies that get_gaps(file=...) returns gaps for
// all nodes whose file path matches. Uses the populated server graph which has
// two functions in pkg/auth/auth.go.
func TestE2E_GetGaps_FileFilter(t *testing.T) {
	srv, loginID, logoutID := newPopulatedServer(t)

	// Gap on AuthLogin.
	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": string(loginID), "gap_id": "login-gap",
		"description": "login gap", "severity": "low",
	}))
	// Gap on AuthLogout.
	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": string(logoutID), "gap_id": "logout-gap",
		"description": "logout gap", "severity": "low",
	}))
	// Gap on an unrelated node (not in auth.go).
	_, _ = srv.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id": "other/file.go:OtherFunc", "gap_id": "other-gap",
		"description": "other gap", "severity": "low",
	}))

	// Filter by file — should return the 2 auth.go gaps, not the other-file gap.
	res, err := srv.handleGetGaps(ctx, callTool(map[string]any{
		"file":   "pkg/auth/auth.go",
		"status": "open",
	}))
	m := mustResult(t, res, err)
	if m["count"].(float64) != 2 {
		t.Errorf("expected 2 gaps for auth.go, got %v (hint: file filter uses node.File match)", m["count"])
	}
}

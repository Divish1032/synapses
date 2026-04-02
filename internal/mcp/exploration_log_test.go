package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/store"
)

// makeTextResult creates a *mcp.CallToolResult with a single text content block.
func makeTextResult(v any) *mcp.CallToolResult {
	data, _ := json.Marshal(v)
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.NewTextContent(string(data))},
	}
}

// TestExtractExplorationCapture_UnknownTool verifies that tools outside the
// exploration set return nil — no capture produced.
func TestExtractExplorationCapture_UnknownTool(t *testing.T) {
	result := makeTextResult(map[string]any{"ok": true})
	for _, tool := range []string{"session_init", "memory", "tasks", "end_session"} {
		if c := extractExplorationCapture(tool, map[string]any{}, result); c != nil {
			t.Errorf("expected nil for %q, got %+v", tool, c)
		}
	}
}

// TestExtractExplorationCapture_NilResult verifies nil result → nil capture.
func TestExtractExplorationCapture_NilResult(t *testing.T) {
	cap := extractExplorationCapture("get_context", map[string]any{"entity": "AuthService"}, nil)
	if cap != nil {
		t.Errorf("expected nil for nil result, got %+v", cap)
	}
}

// TestExtractExplorationCapture_ErrorResult verifies error results are skipped.
func TestExtractExplorationCapture_ErrorResult(t *testing.T) {
	errResult := mcp.NewToolResultError("entity not found")
	cap := extractExplorationCapture("get_context", map[string]any{"entity": "Missing"}, errResult)
	if cap != nil {
		t.Errorf("expected nil for error result, got %+v", cap)
	}
}

// TestExtractExplorationCapture_GetContext_Full verifies extraction from a
// get_context response with caller/callee counts and security constraints.
func TestExtractExplorationCapture_GetContext_Full(t *testing.T) {
	resp := map[string]any{
		"root": map[string]any{"name": "AuthService", "file": "pkg/auth/auth.go", "line": 10},
		"callees": []any{
			map[string]any{"node": map[string]any{"name": "TokenValidator"}},
			map[string]any{"node": map[string]any{"name": "SessionStore"}},
		},
		"callers": []any{
			map[string]any{"node": map[string]any{"name": "LoginHandler"}},
			map[string]any{"node": map[string]any{"name": "LogoutHandler"}},
			map[string]any{"node": map[string]any{"name": "OAuthHandler"}},
		},
		"enrichment": map[string]any{
			"security_constraints": []any{"All handlers must use AuthMiddleware"},
		},
	}

	result := makeTextResult(resp)
	cap := extractExplorationCapture("get_context", map[string]any{
		"entity": "AuthService",
		"intent": "modify",
	}, result)

	if cap == nil {
		t.Fatal("expected non-nil capture")
	}
	if cap.EntityQueried != "AuthService" {
		t.Errorf("EntityQueried: got %q, want %q", cap.EntityQueried, "AuthService")
	}
	if cap.QueryContext != "modify" {
		t.Errorf("QueryContext: got %q, want %q", cap.QueryContext, "modify")
	}
	if cap.FindingSummary == "" {
		t.Error("FindingSummary should not be empty")
	}
	if !elogContains(cap.FindingSummary, "caller") {
		t.Errorf("FindingSummary should mention callers: %q", cap.FindingSummary)
	}
	if !elogContains(cap.FindingSummary, "callee") {
		t.Errorf("FindingSummary should mention callees: %q", cap.FindingSummary)
	}
	if !elogContains(cap.FindingSummary, "security constraint") {
		t.Errorf("FindingSummary should mention security constraints: %q", cap.FindingSummary)
	}
}

// TestExtractExplorationCapture_GetContext_CacheHit verifies that an
// {unchanged: true} response produces nil (no new findings).
func TestExtractExplorationCapture_GetContext_CacheHit(t *testing.T) {
	result := makeTextResult(map[string]any{"unchanged": true, "entity": "AuthService"})
	cap := extractExplorationCapture("get_context", map[string]any{"entity": "AuthService"}, result)
	if cap != nil {
		t.Errorf("expected nil for cache-hit response, got %+v", cap)
	}
}

// TestExtractExplorationCapture_GetContext_NoRootName verifies fallback to
// query entity name when root is absent.
func TestExtractExplorationCapture_GetContext_NoRootName(t *testing.T) {
	result := makeTextResult(map[string]any{
		"callees": []any{},
		"callers": []any{},
	})
	cap := extractExplorationCapture("get_context", map[string]any{"entity": "UserService"}, result)
	if cap == nil {
		t.Fatal("expected non-nil capture")
	}
	if cap.EntityQueried != "UserService" {
		t.Errorf("EntityQueried fallback failed: got %q", cap.EntityQueried)
	}
}

// TestExtractExplorationCapture_Search verifies extraction from a search response.
func TestExtractExplorationCapture_Search(t *testing.T) {
	resp := map[string]any{
		"query":    "auth login",
		"count":    float64(3),
		"_summary": "3 result(s) for \"auth login\"",
		"results": []any{
			map[string]any{"name": "handleLogin", "file": "pkg/api/login.go"},
			map[string]any{"name": "validateCredentials", "file": "pkg/auth/auth.go"},
			map[string]any{"name": "AuthService", "file": "pkg/auth/auth.go"},
		},
	}

	result := makeTextResult(resp)
	cap := extractExplorationCapture("search", map[string]any{"query": "auth login"}, result)

	if cap == nil {
		t.Fatal("expected non-nil capture")
	}
	if cap.EntityQueried != "auth login" {
		t.Errorf("EntityQueried: got %q, want %q", cap.EntityQueried, "auth login")
	}
	if !elogContains(cap.FindingSummary, "result") {
		t.Errorf("FindingSummary should include result count: %q", cap.FindingSummary)
	}
	if !elogContains(cap.FindingSummary, "handleLogin") {
		t.Errorf("FindingSummary should include top entity names: %q", cap.FindingSummary)
	}
}

// TestExtractExplorationCapture_Search_NoResults verifies 0-result handling.
func TestExtractExplorationCapture_Search_NoResults(t *testing.T) {
	result := makeTextResult(map[string]any{
		"query":    "nonexistent",
		"count":    float64(0),
		"_summary": "0 result(s) for \"nonexistent\"",
		"results":  []any{},
	})
	cap := extractExplorationCapture("search", map[string]any{"query": "nonexistent"}, result)
	if cap == nil {
		t.Fatal("expected non-nil capture even for 0-result search")
	}
	if cap.EntityQueried != "nonexistent" {
		t.Errorf("EntityQueried: got %q", cap.EntityQueried)
	}
}

// TestExtractExplorationCapture_GetImpact verifies extraction of blast_radius_summary.
func TestExtractExplorationCapture_GetImpact(t *testing.T) {
	resp := map[string]any{
		"blast_radius_summary": "Changing AuthService affects 12 direct callers across 4 packages.",
		"total_affected":       float64(12),
	}

	result := makeTextResult(resp)
	cap := extractExplorationCapture("get_impact", map[string]any{"symbol": "AuthService"}, result)

	if cap == nil {
		t.Fatal("expected non-nil capture")
	}
	if cap.EntityQueried != "AuthService" {
		t.Errorf("EntityQueried: got %q", cap.EntityQueried)
	}
	if cap.FindingSummary != "Changing AuthService affects 12 direct callers across 4 packages." {
		t.Errorf("FindingSummary mismatch: got %q", cap.FindingSummary)
	}
}

// TestExtractExplorationCapture_GetImpact_FallbackCount verifies fallback to
// total_affected when blast_radius_summary is absent.
func TestExtractExplorationCapture_GetImpact_FallbackCount(t *testing.T) {
	result := makeTextResult(map[string]any{"total_affected": float64(8)})
	cap := extractExplorationCapture("get_impact", map[string]any{"symbol": "UserRepo"}, result)
	if cap == nil {
		t.Fatal("expected non-nil capture")
	}
	if !elogContains(cap.FindingSummary, "8 entity") {
		t.Errorf("FindingSummary should mention total_affected: %q", cap.FindingSummary)
	}
}

// TestExtractExplorationCapture_Validate_Post verifies extraction from post-write response.
func TestExtractExplorationCapture_Validate_Post(t *testing.T) {
	resp := map[string]any{
		"violations": []any{
			map[string]any{"rule_id": "no-direct-db", "severity": "CRITICAL"},
			map[string]any{"rule_id": "no-circular-dep", "severity": "HIGH"},
			map[string]any{"rule_id": "coupling-increase", "severity": "MEDIUM"},
		},
	}

	result := makeTextResult(resp)
	cap := extractExplorationCapture("validate", map[string]any{
		"phase":         "post",
		"files_written": "pkg/api/handler.go",
	}, result)

	if cap == nil {
		t.Fatal("expected non-nil capture")
	}
	if cap.QueryContext != "post" {
		t.Errorf("QueryContext: got %q, want %q", cap.QueryContext, "post")
	}
	if !elogContains(cap.FindingSummary, "3 violation") {
		t.Errorf("FindingSummary should mention violation count: %q", cap.FindingSummary)
	}
	if !elogContains(cap.FindingSummary, "CRITICAL") {
		t.Errorf("FindingSummary should mention CRITICAL severity: %q", cap.FindingSummary)
	}
}

// TestExtractExplorationCapture_Validate_NoViolations verifies clean response.
func TestExtractExplorationCapture_Validate_NoViolations(t *testing.T) {
	result := makeTextResult(map[string]any{"violations": []any{}})
	cap := extractExplorationCapture("validate", map[string]any{
		"phase":         "post",
		"files_written": "pkg/auth/auth.go",
	}, result)
	if cap == nil {
		t.Fatal("expected non-nil capture")
	}
	if cap.FindingSummary != "no violations found" {
		t.Errorf("FindingSummary: got %q, want %q", cap.FindingSummary, "no violations found")
	}
}

// TestExtractExplorationCapture_Validate_PrePhaseSkipped verifies that
// validate(phase=pre) is not captured.
func TestExtractExplorationCapture_Validate_PrePhaseSkipped(t *testing.T) {
	result := makeTextResult(map[string]any{"ok": true})
	cap := extractExplorationCapture("validate", map[string]any{"phase": "pre"}, result)
	if cap != nil {
		t.Errorf("expected nil for phase=pre, got %+v", cap)
	}
}

// TestCapStrHelper verifies the cap/truncation helper.
func TestCapStrHelper(t *testing.T) {
	tests := []struct {
		input  string
		max    int
		expect string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hell…"},
		{"", 10, ""},
		{"a", 1, "a"},
	}
	for _, tc := range tests {
		got := capStr(tc.input, tc.max)
		if got != tc.expect {
			t.Errorf("capStr(%q, %d) = %q, want %q", tc.input, tc.max, got, tc.expect)
		}
	}
}

// ── Integration: compaction recovery includes explored_entities ───────────────

// TestExplorationLog_CompactionRecovery_IncludesExploredEntities verifies that
// buildCompactionRecovery populates explored_entities from the exploration log.
func TestExplorationLog_CompactionRecovery_IncludesExploredEntities(t *testing.T) {
	srv := newTestServer(t)

	// Bootstrap a session so getSynapseSessionID resolves.
	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "recovery-agent",
		"scope":    "standard",
	}))
	sessionID := srv.getSynapseSessionID(SessionIDFromContext(ctx))
	if sessionID == "" {
		t.Skip("no session ID resolved")
	}

	// Directly insert exploration log entries into the server's store
	// (simulating what ledgerWrapped captures asynchronously).
	entries := []store.ExplorationEntry{
		{
			SessionID:      sessionID,
			ProjectID:      srv.projectID,
			ToolName:       "get_context",
			EntityQueried:  "AuthService",
			QueryContext:   "modify",
			FindingSummary: "AuthService: 5 caller(s), 3 callee(s), 1 security constraint(s)",
		},
		{
			SessionID:      sessionID,
			ProjectID:      srv.projectID,
			ToolName:       "search",
			EntityQueried:  "auth login",
			FindingSummary: "3 result(s): handleLogin, validateCreds, AuthService",
		},
		{
			SessionID:      sessionID,
			ProjectID:      srv.projectID,
			ToolName:       "get_impact",
			EntityQueried:  "UserRepo",
			FindingSummary: "UserRepo affects 8 entities across 3 packages",
		},
	}
	for _, e := range entries {
		if err := srv.store.AppendExplorationEntry(e); err != nil {
			t.Fatalf("AppendExplorationEntry: %v", err)
		}
	}

	// Also populate work ledger so buildCompactionRecovery has entity/file data.
	_ = srv.store.AppendLedger(store.LedgerEntry{
		SessionID: sessionID,
		ProjectID: srv.projectID,
		ToolName:  "get_context",
		EntityIDs: []string{"AuthService", "UserRepo"},
		FilePaths: []string{"pkg/auth/auth.go"},
	})

	// Call buildCompactionRecovery.
	recovery := srv.buildCompactionRecovery("recovery-agent", sessionID)
	if recovery == nil {
		t.Fatal("expected non-nil recovery packet")
	}

	exploredRaw, ok := recovery["explored_entities"]
	if !ok {
		t.Fatal("expected explored_entities in recovery packet")
	}
	explored, ok := exploredRaw.([]compactExploration)
	if !ok {
		t.Fatalf("explored_entities has unexpected type %T", exploredRaw)
	}
	if len(explored) == 0 {
		t.Error("explored_entities should not be empty")
	}

	// Verify at least one entry has a finding.
	foundFinding := false
	for _, e := range explored {
		if e.Finding != "" {
			foundFinding = true
			break
		}
	}
	if !foundFinding {
		t.Errorf("expected at least one explored_entity with a finding, got %+v", explored)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// elogContains is a case-insensitive contains check for exploration log test assertions.
// Named to avoid collision with the 'contains' function declared in bug_sweep_test.go.
func elogContains(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

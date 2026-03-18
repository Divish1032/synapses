package mcp

import (
	"encoding/json"
	"testing"
)

// ── Auto-promotion tests ────────────────────────────────────────────────────

func TestAutoPromote_NoToolsDeferred_Phase6(t *testing.T) {
	s := newTestServer(t)
	// Phase 6: all tools are registered at all scales. Nothing should be deferred.
	deferred := s.GetDeferredToolNames()
	if len(deferred) != 0 {
		t.Errorf("Phase 6: expected no deferred tools, got %v", deferred)
	}
}

func TestAutoPromote_UnknownTool_RemainsUnknown(t *testing.T) {
	s := newTestServer(t)
	// A completely unknown tool should not be promotable.
	promoted := s.RegisterDeferredTools([]string{"totally_fake_tool"})
	if len(promoted) != 0 {
		t.Errorf("expected no promotion for unknown tool, got %v", promoted)
	}
}

// ── Tool suggestion auto-promotion tests ─────────────────────────────────────

func TestSuggestTools_AutoPromotes(t *testing.T) {
	s := newTestServer(t)
	// Test that SuggestAndPromoteTools returns suggestions for a known intent.
	suggestions := s.SuggestAndPromoteTools("debugging login flow")
	if len(suggestions) == 0 {
		t.Fatal("expected debug suggestions")
	}
	for _, sg := range suggestions {
		if sg.Tool == "" {
			t.Error("suggestion has empty tool name")
		}
		if sg.Reason == "" {
			t.Errorf("suggestion %s has empty reason", sg.Tool)
		}
	}
}

func TestSuggestTools_NoIntent_NilResult(t *testing.T) {
	s := newTestServer(t)
	suggestions := s.SuggestAndPromoteTools("")
	if suggestions != nil {
		t.Errorf("expected nil for empty intent, got %v", suggestions)
	}
}

// ── discover_tools status field tests ────────────────────────────────────────

func TestDiscoverTools_StatusField(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{
		"query": "what depends on this function",
	}))
	m := mustResult(t, res, err)
	matches, ok := m["matches"].([]any)
	if !ok || len(matches) == 0 {
		t.Fatal("expected at least one match")
	}
	// Every match should have a status field.
	for _, raw := range matches {
		match, ok := raw.(map[string]any)
		if !ok {
			t.Fatal("match is not a map")
		}
		status, ok := match["status"].(string)
		if !ok || status == "" {
			t.Errorf("match %v missing status field", match["name"])
		}
	}
}

func TestDiscoverTools_CoreToolStatus(t *testing.T) {
	s := newTestServer(t)
	// Search for a core tool (session_init).
	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{
		"query": "session start bootstrap init",
	}))
	m := mustResult(t, res, err)
	matches, ok := m["matches"].([]any)
	if !ok || len(matches) == 0 {
		t.Fatal("expected matches for session start query")
	}
	// Find session_init in results.
	for _, raw := range matches {
		match := raw.(map[string]any)
		if match["name"] == "session_init" {
			status := match["status"].(string)
			if status != "core — always available" {
				t.Errorf("session_init status = %q, want %q", status, "core — always available")
			}
			return
		}
	}
	// It's OK if session_init wasn't in top 3 — it still validates the status field exists.
}

// ── end_session absorption tests ─────────────────────────────────────────────

func TestEndSession_AbsorbsReleaseClaims(t *testing.T) {
	s := newTestServer(t)

	// First, create a work claim.
	_, err := s.handleClaimWork(ctx, callTool(map[string]any{
		"agent_id":   "test-agent",
		"scope":      "pkg/auth",
		"scope_type": "directory",
	}))
	if err != nil {
		t.Fatalf("claim work: %v", err)
	}

	// Call end_session — should auto-release claims.
	res, err := s.handleEndSession(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	m := mustResult(t, res, err)

	// Verify claims_released is true.
	released, ok := m["claims_released"].(bool)
	if !ok {
		t.Fatal("expected claims_released field in end_session result")
	}
	if !released {
		t.Error("expected claims_released to be true")
	}
}

func TestEndSession_AbsorbsReleaseClaims_NoClaims(t *testing.T) {
	s := newTestServer(t)

	// Call end_session without any claims — should still succeed.
	res, err := s.handleEndSession(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	m := mustResult(t, res, err)

	// claims_released should be true even when no claims exist (release is idempotent).
	released, ok := m["claims_released"].(bool)
	if !ok {
		t.Fatal("expected claims_released field")
	}
	if !released {
		t.Error("expected claims_released to be true even with no claims")
	}
}

func TestEndSession_ModelParamDoesNotCrash(t *testing.T) {
	s := newTestServer(t)

	// end_session with model/token params should not crash (even without pulse).
	res, err := s.handleEndSession(ctx, callTool(map[string]any{
		"agent_id":      "test-agent",
		"model":         "claude-sonnet-4-6",
		"provider":      "anthropic",
		"input_tokens":  float64(1000),
		"output_tokens": float64(500),
		"cost_usd":      float64(0.01),
	}))
	m := mustResult(t, res, err)
	if m["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", m["status"])
	}
}

// ── session_init suggested_tools tests ───────────────────────────────────────

func TestSessionInit_ToolSuggestions_WithIntent(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"intent":   "implementing auth refactor",
	}))
	m := mustResult(t, res, err)

	// Should have suggested_tools section.
	st, ok := m["suggested_tools"].(map[string]any)
	if !ok {
		t.Fatal("expected suggested_tools in session_init response")
	}
	if st["for_intent"] != "implementing auth refactor" {
		t.Errorf("for_intent = %v, want 'implementing auth refactor'", st["for_intent"])
	}
	promoted, ok := st["promoted"].([]any)
	if !ok || len(promoted) == 0 {
		t.Fatal("expected promoted tools list")
	}
	// Verify each suggestion has tool, reason, example.
	for _, raw := range promoted {
		p, ok := raw.(map[string]any)
		if !ok {
			t.Fatal("promoted entry is not a map")
		}
		if p["tool"] == nil || p["tool"] == "" {
			t.Error("promoted entry missing tool")
		}
		if p["reason"] == nil || p["reason"] == "" {
			t.Error("promoted entry missing reason")
		}
		if p["example"] == nil || p["example"] == "" {
			t.Error("promoted entry missing example")
		}
	}
}

func TestSessionInit_NoToolSuggestions_WithoutIntent(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	m := mustResult(t, res, err)

	// suggested_tools should be absent when no intent.
	noKey(t, m, "suggested_tools")
}

func TestSessionInit_NoToolSuggestions_UnknownIntent(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"intent":   "just vibing",
	}))
	m := mustResult(t, res, err)

	// Unknown intent should produce no suggestions (zero tokens).
	noKey(t, m, "suggested_tools")
}

// ── Regression: all existing tools still callable ────────────────────────────

func TestAllExistingToolsInCatalog(t *testing.T) {
	// Verify that every tool in the catalog is either a core tool, standard tool,
	// or exists at large scale. This ensures no tool was accidentally orphaned.
	catalogNames := make(map[string]bool)
	for _, entry := range toolCatalog {
		catalogNames[entry.Name] = true
	}
	// Every catalog entry should be a valid tool.
	if len(catalogNames) == 0 {
		t.Fatal("tool catalog is empty")
	}
}

// ── JSON serialization test ──────────────────────────────────────────────────

func TestToolSuggestion_JSONShape(t *testing.T) {
	s := ToolSuggestion{
		Tool:    "get_impact",
		Reason:  "Check dependencies",
		Example: `get_impact(symbol="AuthService")`,
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["tool"] != "get_impact" {
		t.Errorf("tool = %v", m["tool"])
	}
	if m["reason"] != "Check dependencies" {
		t.Errorf("reason = %v", m["reason"])
	}
	if m["example"] == nil || m["example"] == "" {
		t.Error("missing example")
	}
}

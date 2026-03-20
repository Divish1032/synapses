package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── Tool suggestion tests ────────────────────────────────────────────────────

func TestSuggestToolsForIntent_ReturnsSuggestions(t *testing.T) {
	s := newTestServer(t)
	suggestions := s.SuggestToolsForIntent("debugging login flow")
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

func TestSuggestToolsForIntent_NilForEmpty(t *testing.T) {
	s := newTestServer(t)
	suggestions := s.SuggestToolsForIntent("")
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
	for _, raw := range matches {
		match := raw.(map[string]any)
		status, ok := match["status"].(string)
		if !ok || status == "" {
			t.Errorf("match %v missing status field", match["name"])
		}
		// Status must be one of the valid values — never "promoted".
		valid := status == "core — always available" ||
			status == "standard — always available" ||
			status == "available — ready to call" ||
			status == "hidden — not in tools/list, still callable"
		if !valid {
			t.Errorf("invalid status %q for %v", status, match["name"])
		}
	}
}

func TestDiscoverTools_CoreToolStatus(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{
		"query": "session start bootstrap init",
	}))
	m := mustResult(t, res, err)
	matches, ok := m["matches"].([]any)
	if !ok || len(matches) == 0 {
		t.Fatal("expected matches")
	}
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
}

func TestDiscoverTools_NoPromotedStatus(t *testing.T) {
	// "promoted" status must never appear — all tools are always registered.
	s := newTestServer(t)
	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{
		"query": "impact blast radius callers",
		"debug": true,
	}))
	m := mustResult(t, res, err)
	matches, ok := m["matches"].([]any)
	if !ok {
		t.Fatal("expected matches")
	}
	for _, raw := range matches {
		match := raw.(map[string]any)
		if strings.Contains(match["status"].(string), "promoted") {
			t.Errorf("found 'promoted' status for %v — dead code path", match["name"])
		}
	}
}

// ── Hidden tools tests (Sprint 8 #1) ─────────────────────────────────────────

func TestHiddenTools_SetContainsExpectedTools(t *testing.T) {
	expected := []string{
		"get_working_state", "get_project_identity", "report_usage",
		"check_plan_safety", "get_edge_types", "plan_context",
	}
	for _, name := range expected {
		if !hiddenTools[name] {
			t.Errorf("expected %q in hiddenTools set", name)
		}
	}
}

func TestHiddenTools_CoreToolsNotHidden(t *testing.T) {
	// Core tools must never be hidden.
	coreNames := []string{
		"session_init", "prepare_context", "search", "find_entity",
		"validate_plan", "verify_implementation", "remember", "recall",
		"create_plan", "update_task", "end_session", "discover_tools",
	}
	for _, name := range coreNames {
		if hiddenTools[name] {
			t.Errorf("core tool %q must not be in hiddenTools", name)
		}
	}
}

func TestDiscoverTools_HiddenToolStatus(t *testing.T) {
	s := newTestServer(t)
	// check_plan_safety is in the tool catalog and hidden — search for it.
	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{
		"query": "safety check failed before similar risk interjection",
	}))
	m := mustResult(t, res, err)
	matches, ok := m["matches"].([]any)
	if !ok || len(matches) == 0 {
		t.Fatal("expected matches for safety query")
	}
	found := false
	for _, raw := range matches {
		match := raw.(map[string]any)
		if match["name"] == "check_plan_safety" {
			found = true
			status := match["status"].(string)
			if status != "hidden — not in tools/list, still callable" {
				t.Errorf("check_plan_safety status = %q, want hidden", status)
			}
		}
	}
	if !found {
		t.Error("check_plan_safety not found in discover_tools results — should be discoverable even when hidden")
	}
}

func TestDiscoverTools_EmptyQuery_HiddenToolsMarked(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{}))
	m := mustResult(t, res, err)
	categories, ok := m["categories"].(map[string]any)
	if !ok {
		t.Fatal("expected categories")
	}
	// Walk all categories and check that hidden tools have status field.
	for _, tools := range categories {
		for _, raw := range tools.([]any) {
			entry := raw.(map[string]any)
			name := entry["name"].(string)
			if hiddenTools[name] {
				st, _ := entry["status"].(string)
				if st == "" {
					t.Errorf("hidden tool %q missing status in category overview", name)
				}
			}
		}
	}
}

func TestHiddenTools_StillCallable(t *testing.T) {
	// Hidden tools must still be callable — they're registered, just unlisted.
	s := newTestServer(t)
	// Call get_project_identity (hidden) — should return a valid response, not an error.
	res, err := s.handleGetProjectIdentity(ctx, callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("hidden tool get_project_identity returned error: %v", err)
	}
	if res == nil || res.IsError {
		t.Error("hidden tool get_project_identity should still work when called directly")
	}
}

// ── end_session absorption tests ─────────────────────────────────────────────

func TestEndSession_ModelParamDoesNotCrash(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleEndSession(ctx, callTool(map[string]any{
		"agent_id": "test-agent", "model": "claude-sonnet-4-6",
		"provider": "anthropic", "input_tokens": float64(1000),
		"output_tokens": float64(500), "cost_usd": float64(0.01),
	}))
	m := mustResult(t, res, err)
	if m["status"] != "ok" {
		t.Errorf("status = %v, want ok", m["status"])
	}
}

// ── session_init suggested_tools tests ───────────────────────────────────────

func TestSessionInit_ToolSuggestions_WithIntent(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent", "intent": "implementing auth refactor",
	}))
	m := mustResult(t, res, err)
	st, ok := m["suggested_tools"].(map[string]any)
	if !ok {
		t.Fatal("expected suggested_tools in session_init response")
	}
	if st["for_intent"] != "implementing auth refactor" {
		t.Errorf("for_intent = %v", st["for_intent"])
	}
	tools, ok := st["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatal("expected tools list")
	}
	for _, raw := range tools {
		p := raw.(map[string]any)
		if p["tool"] == nil || p["tool"] == "" {
			t.Error("entry missing tool")
		}
		if p["reason"] == nil || p["reason"] == "" {
			t.Error("entry missing reason")
		}
	}
}

func TestSessionInit_NoToolSuggestions_WithoutIntent(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "test-agent"}))
	m := mustResult(t, res, err)
	noKey(t, m, "suggested_tools")
}

func TestSessionInit_NoToolSuggestions_UnknownIntent(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent", "intent": "just vibing",
	}))
	m := mustResult(t, res, err)
	noKey(t, m, "suggested_tools")
}

// ── Regression tests ─────────────────────────────────────────────────────────

func TestAllExistingToolsInCatalog(t *testing.T) {
	if len(toolCatalog) == 0 {
		t.Fatal("tool catalog is empty")
	}
}

func TestToolSuggestion_JSONShape(t *testing.T) {
	s := ToolSuggestion{Tool: "get_impact", Reason: "Check deps", Example: `get_impact(symbol="X")`}
	data, _ := json.Marshal(s)
	var m map[string]any
	json.Unmarshal(data, &m)
	if m["tool"] != "get_impact" {
		t.Errorf("tool = %v", m["tool"])
	}
}

// ── Path safety test (Gap 6) ─────────────────────────────────────────────────

func TestEntityImpact_RelativePathMatchesAbsoluteGraph(t *testing.T) {
	g := graph.New("test-repo")
	id := g.MakeNodeID("/Users/dev/repo/internal/auth.go", "AuthService")
	g.AddNode(&graph.Node{
		ID: id, Name: "AuthService", Type: graph.NodeStruct,
		File: "/Users/dev/repo/internal/auth.go", Line: 10,
	})
	// Simulate watcher-style relative path lookup.
	results := g.FindByFile("internal/auth.go")
	if len(results) != 1 {
		t.Fatalf("expected 1 match for relative path, got %d", len(results))
	}
	if results[0].Name != "AuthService" {
		t.Errorf("expected AuthService, got %s", results[0].Name)
	}
}

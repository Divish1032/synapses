package mcp

import (
	"context"
	"encoding/json"
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

// ── Hidden tools tests (Sprint 8 #1) ─────────────────────────────────────────

func TestHiddenTools_SetContainsExpectedTools(t *testing.T) {
	// Sprint 25: all tools merged, no hidden tools remain.
	if len(hiddenTools) != 0 {
		t.Errorf("expected hiddenTools to be empty after Phase 5 consolidation, got %d entries", len(hiddenTools))
	}
}

func TestHiddenTools_CoreToolsNotHidden(t *testing.T) {
	// Core tools must never be hidden.
	// Sprint 23.9: 8-tool surface — rules and annotate merged into validate and memory.
	coreNames := []string{
		"session_init", "get_context", "search", "validate",
		"memory", "tasks", "end_session", "get_impact",
	}
	for _, name := range coreNames {
		if hiddenTools[name] {
			t.Errorf("core tool %q must not be in hiddenTools", name)
		}
	}
}

func TestDiscoverTools_HiddenToolStatus(t *testing.T) {
	// Sprint 25: discover_tools removed and hiddenTools empty after Phase 5 consolidation.
	// This test now verifies the validate tool (which absorbed check_plan_safety) is registered.
	s := newTestServer(t)
	result, err := s.DispatchTool(context.Background(), "validate", map[string]interface{}{
		"phase":            "safety",
		"plan_description": "test plan",
	})
	if err != nil {
		t.Fatalf("validate(phase=safety) dispatch error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
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

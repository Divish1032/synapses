package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// ── TEST-004: handleBenchmark — nil-graph, nil-store, and basic coverage ──────

func TestHandleBenchmark_NilGraph(t *testing.T) {
	s := newTestServer(t)
	s.graph = nil // force nil
	res, err := s.handleBenchmark(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Error("expected tool error for nil graph")
	}
}

func TestHandleBenchmark_NilStore(t *testing.T) {
	// Use a populated server (non-empty graph) so the empty-graph guard doesn't
	// fire before we reach the nil-store guard.
	s, _, _ := newPopulatedServer(t)
	s.store = nil // force nil
	res, err := s.handleBenchmark(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Error("expected tool error for nil store")
	}
}

func TestHandleBenchmark_EmptyGraph(t *testing.T) {
	s := newTestServer(t) // graph has 0 nodes
	res, err := s.handleBenchmark(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Error("expected tool error for empty graph")
	}
}

func TestHandleBenchmark_InvalidScenario(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleBenchmark(context.Background(), callTool(map[string]any{
		"scenario": "nonexistent_scenario_xyz",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Error("expected tool error for invalid scenario")
	}
}

func TestHandleBenchmark_AllScenarios(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleBenchmark(context.Background(), callTool(map[string]any{
		"scenario": "all",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	// Populated graph — benchmark should run (some scenarios may skip if graph is too small).
	if res.IsError {
		t.Errorf("unexpected tool error: %v", res.Content)
	}
}

// ── TEST-005: handleGetMyAnalytics — basic coverage ──────────────────────────

func TestHandleGetMyAnalytics_NoPulse(t *testing.T) {
	s := newTestServer(t)
	// No pulse client → returns "not available" gracefully.
	res, err := s.handleGetMyAnalytics(context.Background(), callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	// Should succeed (not be a tool error) even without pulse.
	if res.IsError {
		t.Errorf("unexpected tool error: %v", res.Content)
	}
}

func TestHandleGetMyAnalytics_EmptyAgentID(t *testing.T) {
	s := newTestServer(t)
	// Empty agent_id falls back to getLastAgent(), not an error.
	res, err := s.handleGetMyAnalytics(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestHandleGetMyAnalytics_HappyPath(t *testing.T) {
	s := newTestServer(t)
	// First create some activity.
	_, _ = s.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id":     "analytics-agent",
		"decision":     "test decision for analytics",
		"episode_type": "decision",
		"outcome":      "success",
	}))

	res, err := s.handleGetMyAnalytics(context.Background(), callTool(map[string]any{
		"agent_id": "analytics-agent",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	if res.IsError {
		t.Errorf("unexpected tool error: %v", res.Content)
	}
}

// ── TEST-007: CallTool dispatch — registered tools must be callable ───────────

func TestCallTool_DispatchCoversRegisteredTools(t *testing.T) {
	s := newTestServer(t)

	// Get all registered tool names from the server's toolHandlers map.
	s.toolHandlersMu.RLock()
	toolNames := make([]string, 0, len(s.toolHandlers))
	for name := range s.toolHandlers {
		toolNames = append(toolNames, name)
	}
	s.toolHandlersMu.RUnlock()

	if len(toolNames) == 0 {
		t.Fatal("no tools registered — expected at least 10")
	}

	// Verify each registered tool can be dispatched (returns a result, even if
	// it's a validation error). The key assertion: no tool silently returns nil.
	ctx := context.Background()
	for _, name := range toolNames {
		t.Run(name, func(t *testing.T) {
			req := mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name:      name,
					Arguments: map[string]any{},
				},
			}
			s.toolHandlersMu.RLock()
			handler, ok := s.toolHandlers[name]
			s.toolHandlersMu.RUnlock()

			if !ok {
				t.Fatalf("tool %q registered but handler not in dispatch map", name)
			}

			res, err := handler(ctx, req)
			if err != nil {
				// Handler errors are fine (e.g., store nil), just verify no panic.
				return
			}
			if res == nil {
				t.Errorf("tool %q returned nil result — dispatch may be broken", name)
			}
		})
	}
}

// ── BUG-022: toolError helper ────────────────────────────────────────────────

func TestToolError_UniqueConstraint(t *testing.T) {
	res, _ := toolError("create plan", fmt.Errorf("UNIQUE constraint failed: plans.id"))
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	if !res.IsError {
		t.Error("expected IsError=true")
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !contains(text, "Hint:") {
		t.Error("expected recovery hint in error message")
	}
}

func TestToolError_GenericError(t *testing.T) {
	res, _ := toolError("send message", fmt.Errorf("some random error"))
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	text := res.Content[0].(mcp.TextContent).Text
	if contains(text, "Hint:") {
		t.Error("generic errors should not have hints")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

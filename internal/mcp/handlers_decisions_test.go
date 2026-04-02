package mcp

import (
	"strings"
	"testing"
)

// TestHandleDecide_Create verifies creating a decision returns a decision_id
// and confirmation message.
func TestHandleDecide_Create(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":       "decide",
		"agent_id":     "tester",
		"choice":       "Use repository pattern for database access",
		"alternatives": "direct DB calls from handlers; active record",
		"reasoning":    "Testability: allows mock injection. 12/14 packages already follow this pattern.",
		"context":      "Adding user management service",
	}))
	m := mustResult(t, result, err)

	hasKey(t, m, "decision_id")
	if id, _ := m["decision_id"].(string); id == "" {
		t.Error("expected non-empty decision_id")
	}
	if choice, _ := m["choice"].(string); choice != "Use repository pattern for database access" {
		t.Errorf("expected choice echoed back, got %q", choice)
	}
	if msg, _ := m["message"].(string); !strings.Contains(msg, "Decision recorded") {
		t.Errorf("unexpected message: %q", msg)
	}
}

// TestHandleDecide_MinimalFields verifies that only agent_id and choice are
// required — all other fields are optional.
func TestHandleDecide_MinimalFields(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "decide",
		"agent_id": "tester",
		"choice":   "Use table-driven tests",
	}))
	m := mustResult(t, result, err)

	if id, _ := m["decision_id"].(string); id == "" {
		t.Error("expected non-empty decision_id for minimal decision")
	}
}

// TestHandleDecide_MissingAgentID verifies that omitting agent_id returns an error.
func TestHandleDecide_MissingAgentID(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action": "decide",
		"choice": "some choice",
	}))
	if result == nil || !result.IsError {
		t.Error("expected tool error for missing agent_id")
	}
}

// TestHandleDecide_MissingChoice verifies that omitting choice returns an error.
func TestHandleDecide_MissingChoice(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":    "decide",
		"agent_id":  "tester",
		"reasoning": "some reasoning",
	}))
	if result == nil || !result.IsError {
		t.Error("expected tool error for missing choice")
	}
}

// TestHandleDecide_NilStore verifies that a nil store returns a tool error,
// not a panic.
func TestHandleDecide_NilStore(t *testing.T) {
	srv := newTestServer(t)
	srv.store = nil

	result, err := srv.handleDecide(ctx, callTool(map[string]any{
		"agent_id": "tester",
		"choice":   "test",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Error("expected tool error result when store is nil")
	}
}

// TestHandleListDecisions_Basic verifies that decisions created via handleDecide
// are returned by handleListDecisions.
func TestHandleListDecisions_Basic(t *testing.T) {
	srv := newTestServer(t)

	// Create two decisions.
	_, _ = srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":    "decide",
		"agent_id":  "tester",
		"choice":    "Use JWT for authentication",
		"reasoning": "RS256 asymmetric keys — verifiable without shared secret",
	}))
	_, _ = srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":    "decide",
		"agent_id":  "tester",
		"choice":    "Use PostgreSQL for persistence",
		"reasoning": "ACID compliance required",
	}))

	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "list_decisions",
		"agent_id": "tester",
	}))
	m := mustResult(t, result, err)

	decisions, ok := m["decisions"].([]interface{})
	if !ok {
		t.Fatalf("expected decisions array, got %T", m["decisions"])
	}
	if len(decisions) != 2 {
		t.Errorf("expected 2 decisions, got %d", len(decisions))
	}
	if count, _ := m["count"].(float64); int(count) != 2 {
		t.Errorf("expected count=2, got %v", m["count"])
	}
}

// TestHandleListDecisions_Search verifies that the query param filters by keyword.
func TestHandleListDecisions_Search(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":    "decide",
		"agent_id":  "tester",
		"choice":    "Use JWT for authentication",
		"reasoning": "RS256 asymmetric keys",
	}))
	_, _ = srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":    "decide",
		"agent_id":  "tester",
		"choice":    "Use PostgreSQL for persistence",
		"reasoning": "ACID compliance",
	}))

	// Search for "JWT" — should return only the first decision.
	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "list_decisions",
		"agent_id": "tester",
		"query":    "JWT",
	}))
	m := mustResult(t, result, err)

	decisions, _ := m["decisions"].([]interface{})
	if len(decisions) != 1 {
		t.Errorf("expected 1 JWT match, got %d", len(decisions))
	}
	firstDec, _ := decisions[0].(map[string]interface{})
	if choice, _ := firstDec["choice"].(string); choice != "Use JWT for authentication" {
		t.Errorf("expected JWT decision, got %q", choice)
	}
}

// TestHandleListDecisions_NilStore verifies that a nil store returns a tool error.
func TestHandleListDecisions_NilStore(t *testing.T) {
	srv := newTestServer(t)
	srv.store = nil

	result, err := srv.handleListDecisions(ctx, callTool(map[string]any{
		"agent_id": "tester",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Error("expected tool error result when store is nil")
	}
}

// TestHandleListDecisions_MissingAgentID verifies that omitting agent_id returns an error.
func TestHandleListDecisions_MissingAgentID(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action": "list_decisions",
	}))
	if result == nil || !result.IsError {
		t.Error("expected tool error for missing agent_id")
	}
}

// TestHandleDecide_SummariseDecision verifies the summariseDecision helper
// truncates at 60 chars and appends an ellipsis for long strings.
func TestHandleDecide_SummariseDecision(t *testing.T) {
	short := "Use JWT"
	if summariseDecision(short) != short {
		t.Errorf("short string should not be truncated: %q", summariseDecision(short))
	}

	long := "Use JWT with RS256 asymmetric keys for authentication and authorisation because the public key can be distributed without exposing the secret"
	got := summariseDecision(long)
	if len([]rune(got)) > 65 { // 60 chars + "…" (3 bytes but 1 rune)
		t.Errorf("long string should be truncated, got len=%d: %q", len(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got: %q", got)
	}
}

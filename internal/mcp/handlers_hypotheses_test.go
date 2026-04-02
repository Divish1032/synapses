package mcp

import (
	"strings"
	"testing"
)

// TestHandleHypothesize_Create verifies creating a new hypothesis returns
// a hypothesis_id and state=active.
func TestHandleHypothesize_Create(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "hypothesize",
		"agent_id": "tester",
		"content":  "I think the bug is in AuthService.validateToken because it skips expiry checks",
	}))
	m := mustResult(t, result, err)

	hasKey(t, m, "hypothesis_id")
	if id, _ := m["hypothesis_id"].(string); id == "" {
		t.Error("expected non-empty hypothesis_id")
	}
	if state, _ := m["state"].(string); state != "active" {
		t.Errorf("expected state=active, got %q", state)
	}
	if msg, _ := m["message"].(string); !strings.Contains(msg, "Hypothesis recorded") {
		t.Errorf("unexpected message: %q", msg)
	}
}

// TestHandleHypothesize_Create_MissingContent verifies that omitting content
// on a create call returns an error.
func TestHandleHypothesize_Create_MissingContent(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "hypothesize",
		"agent_id": "tester",
		// no content
	}))
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.IsError {
		t.Error("expected error result for missing content")
	}
}

// TestHandleHypothesize_Create_MissingAgentID verifies that omitting agent_id returns an error.
func TestHandleHypothesize_Create_MissingAgentID(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":  "hypothesize",
		"content": "some theory",
		// no agent_id
	}))
	if result == nil || !result.IsError {
		t.Error("expected error result for missing agent_id")
	}
}

// TestHandleHypothesize_Update_Confirm verifies that updating a hypothesis to
// confirmed yields the right message.
func TestHandleHypothesize_Update_Confirm(t *testing.T) {
	srv := newTestServer(t)

	// Create first.
	createResult, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "hypothesize",
		"agent_id": "tester",
		"content":  "Retry logic skips the backoff on 503 errors",
	}))
	m := mustResult(t, createResult, err)
	id := m["hypothesis_id"].(string)

	// Update to confirmed.
	updateResult, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":        "hypothesize",
		"agent_id":      "tester",
		"hypothesis_id": id,
		"state":         "confirmed",
		"evidence":      "reproduced with curl -X POST, returns 503 without delay",
	}))
	um := mustResult(t, updateResult, err)
	if um["state"] != "confirmed" {
		t.Errorf("expected state=confirmed, got %v", um["state"])
	}
	if msg, _ := um["message"].(string); !strings.Contains(msg, "confirmed") {
		t.Errorf("expected confirmation message, got: %q", msg)
	}
}

// TestHandleHypothesize_Update_Reject verifies the invalidation prompt is
// present when a hypothesis is rejected.
func TestHandleHypothesize_Update_Reject(t *testing.T) {
	srv := newTestServer(t)

	// Create first.
	createResult, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "hypothesize",
		"agent_id": "tester",
		"content":  "Memory leak is caused by the graph cache",
	}))
	cm := mustResult(t, createResult, nil)
	id := cm["hypothesis_id"].(string)

	// Reject with evidence.
	rejectResult, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":        "hypothesize",
		"agent_id":      "tester",
		"hypothesis_id": id,
		"state":         "rejected",
		"evidence":      "heap profile shows leak in HTTP connection pool, not graph",
	}))
	rm := mustResult(t, rejectResult, err)
	if rm["state"] != "rejected" {
		t.Errorf("expected state=rejected, got %v", rm["state"])
	}
	// Invalidation prompt must be present.
	hasKey(t, rm, "invalidation_prompt")
	// Message must reference the original content.
	msg, _ := rm["message"].(string)
	if !strings.Contains(msg, "Memory leak is caused by the graph cache") {
		t.Errorf("invalidation message should reference original content: %q", msg)
	}
}

// TestHandleHypothesize_Update_NotFound verifies that updating a non-existent
// hypothesis_id returns an error.
func TestHandleHypothesize_Update_NotFound(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":        "hypothesize",
		"agent_id":      "tester",
		"hypothesis_id": "no-such-id",
		"state":         "confirmed",
	}))
	if result == nil || !result.IsError {
		t.Error("expected error for nonexistent hypothesis_id")
	}
}

// TestHandleHypothesize_Update_NoStateNoEvidence verifies that updating with
// neither state nor evidence returns an error.
func TestHandleHypothesize_Update_NoStateNoEvidence(t *testing.T) {
	srv := newTestServer(t)

	// Create first.
	cr, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "hypothesize",
		"agent_id": "tester",
		"content":  "some theory",
	}))
	cm := mustResult(t, cr, nil)
	id := cm["hypothesis_id"].(string)

	// Update with neither state nor evidence.
	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":        "hypothesize",
		"agent_id":      "tester",
		"hypothesis_id": id,
		// no state, no evidence
	}))
	if result == nil || !result.IsError {
		t.Error("expected error when both state and evidence are absent")
	}
}

// TestHandleHypothesize_Update_EvidenceOnly verifies that an evidence-only update
// (no state change) preserves the existing state and appends the new evidence.
func TestHandleHypothesize_Update_EvidenceOnly(t *testing.T) {
	srv := newTestServer(t)

	// Create first.
	cr, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "hypothesize",
		"agent_id": "tester",
		"content":  "The timeout is caused by a missing index",
	}))
	cm := mustResult(t, cr, nil)
	id := cm["hypothesis_id"].(string)

	// Update with evidence only — no state provided.
	ur, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":        "hypothesize",
		"agent_id":      "tester",
		"hypothesis_id": id,
		"evidence":      "EXPLAIN ANALYZE shows seq scan on users table",
		// no state
	}))
	um := mustResult(t, ur, err)

	// State must remain active.
	if um["state"] != "active" {
		t.Errorf("expected state=active after evidence-only update, got %v", um["state"])
	}
	if _, ok := um["invalidation_prompt"]; ok {
		t.Error("evidence-only update should not include invalidation_prompt")
	}
}

// TestHandleListHypotheses_Basic verifies that list_hypotheses returns created hypotheses.
func TestHandleListHypotheses_Basic(t *testing.T) {
	srv := newTestServer(t)

	// Create two hypotheses.
	for _, content := range []string{
		"theory one about auth",
		"theory two about database",
	} {
		_, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
			"action":   "hypothesize",
			"agent_id": "agent-1",
			"content":  content,
		}))
		if err != nil {
			t.Fatalf("create hypothesis: %v", err)
		}
	}

	// List all hypotheses.
	listResult, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":       "list_hypotheses",
		"agent_id":     "agent-1",
		"state_filter": "active",
	}))
	lm := mustResult(t, listResult, err)
	count, _ := lm["count"].(float64)
	if count != 2 {
		t.Errorf("expected 2 hypotheses, got %v", count)
	}
}

// TestHandleListHypotheses_StateFilter verifies that state_filter limits results correctly.
func TestHandleListHypotheses_StateFilter(t *testing.T) {
	srv := newTestServer(t)

	// Create three hypotheses and update two of them.
	createAndGet := func(content string) string {
		r, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
			"action":   "hypothesize",
			"agent_id": "a",
			"content":  content,
		}))
		m := mustResult(t, r, nil)
		return m["hypothesis_id"].(string)
	}

	id1 := createAndGet("active theory")
	id2 := createAndGet("confirmed theory")
	id3 := createAndGet("rejected theory")
	_ = id1

	srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action": "hypothesize", "agent_id": "a",
		"hypothesis_id": id2, "state": "confirmed",
	}))
	srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action": "hypothesize", "agent_id": "a",
		"hypothesis_id": id3, "state": "rejected",
	}))

	// Query confirmed.
	lr, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":       "list_hypotheses",
		"agent_id":     "a",
		"state_filter": "confirmed",
	}))
	lm := mustResult(t, lr, err)
	if count, _ := lm["count"].(float64); count != 1 {
		t.Errorf("expected 1 confirmed, got %v", count)
	}

	// Query all.
	lr2, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":       "list_hypotheses",
		"agent_id":     "a",
		"state_filter": "all",
	}))
	lm2 := mustResult(t, lr2, nil)
	if count, _ := lm2["count"].(float64); count != 3 {
		t.Errorf("expected 3 total, got %v", count)
	}
}

// TestHandleListHypotheses_MissingAgentID verifies that omitting agent_id returns an error.
func TestHandleListHypotheses_MissingAgentID(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action": "list_hypotheses",
	}))
	if result == nil || !result.IsError {
		t.Error("expected error for missing agent_id")
	}
}

// TestHandleHypothesize_NilStore verifies that a nil store returns a tool error,
// not a panic.
func TestHandleHypothesize_NilStore(t *testing.T) {
	srv := newTestServer(t)
	srv.store = nil

	result, err := srv.handleHypothesize(ctx, callTool(map[string]any{
		"action":   "hypothesize",
		"agent_id": "a",
		"content":  "theory",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Error("expected tool error result when store is nil")
	}
}

// TestHandleListHypotheses_NilStore verifies that a nil store returns a tool error.
func TestHandleListHypotheses_NilStore(t *testing.T) {
	srv := newTestServer(t)
	srv.store = nil

	result, err := srv.handleListHypotheses(ctx, callTool(map[string]any{
		"action":   "list_hypotheses",
		"agent_id": "a",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Error("expected tool error result when store is nil")
	}
}

// TestHandleHypothesize_UnknownAction verifies the dispatch error path.
func TestHandleHypothesize_UnknownAction(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action": "not_a_real_action",
	}))
	if result == nil || !result.IsError {
		t.Error("expected error for unknown action")
	}
}

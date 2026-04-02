package mcp

// Sprint 25.5: session handoff protocol integration tests.
// Verifies that end_session stores a structured handoff payload and that
// session_init retrieves it in _briefing.session_handoff for the same agent.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestHandoff_EndSessionStoresHandoff verifies that calling end_session with a
// summary causes a handoff memory to be stored, retrievable via GetLatestHandoff.
func TestHandoff_EndSessionStoresHandoff(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	res, err := srv.handleEndSession(context.Background(), callTool(map[string]any{
		"agent_id": "agent-handoff",
		"summary":  "Implemented OAuth login flow in pkg/auth.go",
	}))
	if err != nil {
		t.Fatalf("handleEndSession: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", extractErrorText(t, res))
	}

	// Handoff memory should now exist in the store.
	mem, err := srv.store.GetLatestHandoff("agent-handoff", srv.projectID)
	if err != nil {
		t.Fatalf("GetLatestHandoff: %v", err)
	}
	if mem == nil {
		t.Fatal("expected a handoff memory to be stored after end_session with summary, got nil")
	}

	// Payload should deserialise correctly and contain the agent_summary.
	var payload handoffPayload
	if err := json.Unmarshal([]byte(mem.Content), &payload); err != nil {
		t.Fatalf("unmarshal handoff payload: %v", err)
	}
	if payload.AgentSummary != "Implemented OAuth login flow in pkg/auth.go" {
		t.Errorf("expected agent_summary to match, got %q", payload.AgentSummary)
	}
	if payload.SessionAt == 0 {
		t.Error("expected session_at to be set (uniqueness nonce)")
	}
}

// TestHandoff_EndSessionNoSummaryNoWork_NoHandoff verifies that when end_session
// is called with no summary, no decisions, no hypotheses, and no session work,
// no handoff memory is stored (nothing meaningful to persist).
func TestHandoff_EndSessionNoSummaryNoWork_NoHandoff(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	res, err := srv.handleEndSession(context.Background(), callTool(map[string]any{
		"agent_id": "agent-empty",
		// No summary, no task_id.
	}))
	if err != nil {
		t.Fatalf("handleEndSession: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", extractErrorText(t, res))
	}

	mem, err := srv.store.GetLatestHandoff("agent-empty", srv.projectID)
	if err != nil {
		t.Fatalf("GetLatestHandoff: %v", err)
	}
	// No summary + no work → no handoff memory.
	if mem != nil {
		t.Errorf("expected no handoff memory for empty session, got content: %s", mem.Content)
	}
}

// TestHandoff_SessionInitReceivesHandoff verifies the roundtrip:
// end_session(with summary) → session_init returns _briefing.session_handoff.
func TestHandoff_SessionInitReceivesHandoff(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// Simulate a completed session with a meaningful summary.
	if _, err := srv.handleEndSession(context.Background(), callTool(map[string]any{
		"agent_id": "agent-roundtrip",
		"summary":  "Added rate limiting to all API endpoints",
	})); err != nil {
		t.Fatalf("handleEndSession: %v", err)
	}

	// Next session_init should see the handoff in _briefing.
	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "agent-roundtrip",
		"scope":    "full",
	}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleSessionInit error: %s", extractErrorText(t, res))
	}

	text := extractText(t, res)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal session_init response: %v", err)
	}

	briefing, ok := resp["_briefing"].(map[string]interface{})
	if !ok {
		t.Fatal("_briefing missing from session_init response")
	}

	handoff, ok := briefing["session_handoff"].(map[string]interface{})
	if !ok || handoff == nil {
		t.Fatal("session_handoff missing from _briefing (expected after end_session with summary)")
	}

	agentSummary, _ := handoff["agent_summary"].(string)
	if agentSummary != "Added rate limiting to all API endpoints" {
		t.Errorf("session_handoff.agent_summary: got %q, want %q",
			agentSummary, "Added rate limiting to all API endpoints")
	}

	note, _ := handoff["note"].(string)
	if note == "" {
		t.Error("session_handoff.note should not be empty")
	}
}

// TestHandoff_SessionInitAbsentForDifferentAgent verifies that agent A's handoff
// is NOT returned when agent B calls session_init — handoffs are agent-scoped.
func TestHandoff_SessionInitAbsentForDifferentAgent(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// Agent A ends its session with a summary.
	if _, err := srv.handleEndSession(context.Background(), callTool(map[string]any{
		"agent_id": "agent-alpha",
		"summary":  "Alpha's private findings",
	})); err != nil {
		t.Fatalf("handleEndSession agent-alpha: %v", err)
	}

	// Agent B starts a new session — should NOT see agent-alpha's handoff.
	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "agent-beta",
		"scope":    "full",
	}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleSessionInit error: %s", extractErrorText(t, res))
	}

	text := extractText(t, res)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	briefing, _ := resp["_briefing"].(map[string]interface{})
	if _, has := briefing["session_handoff"]; has {
		t.Error("agent-beta must not see agent-alpha's handoff")
	}
}

// TestHandoff_DecisionsIncludedInHandoff verifies that when the agent has stored
// decisions, they appear in the handoff payload.
func TestHandoff_DecisionsIncludedInHandoff(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// Pre-seed a decision for this agent+project.
	_, err := srv.store.InsertDecision(store.Decision{
		AgentID:   "agent-decide",
		ProjectID: srv.projectID,
		Choice:    "Use JWT tokens for API authentication",
		Reasoning: "Stateless, easy to distribute",
		Context:   "API auth redesign",
	})
	if err != nil {
		t.Fatalf("InsertDecision: %v", err)
	}

	// End session to build handoff.
	if _, err := srv.handleEndSession(context.Background(), callTool(map[string]any{
		"agent_id": "agent-decide",
		"summary":  "Reviewed auth strategy",
	})); err != nil {
		t.Fatalf("handleEndSession: %v", err)
	}

	mem, err := srv.store.GetLatestHandoff("agent-decide", srv.projectID)
	if err != nil || mem == nil {
		t.Fatalf("GetLatestHandoff: err=%v, mem=%v", err, mem)
	}

	var payload handoffPayload
	if err := json.Unmarshal([]byte(mem.Content), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(payload.KeyDecisions) == 0 {
		t.Fatal("expected KeyDecisions to be populated from stored decision, got empty")
	}
	found := false
	for _, d := range payload.KeyDecisions {
		if d != "" && len(d) > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected at least one non-empty decision in payload, got: %v", payload.KeyDecisions)
	}
}

// TestHandoff_ActiveHypothesesIncluded verifies that ACTIVE hypotheses appear
// in the handoff payload, and REJECTED ones do not.
func TestHandoff_ActiveHypothesesIncluded(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// Active hypothesis — should appear.
	if _, err := srv.store.InsertHypothesis(store.Hypothesis{
		AgentID:   "agent-hyp",
		ProjectID: srv.projectID,
		Content:   "The slowdown is caused by N+1 queries in UserRepo.ListAll",
		State:     store.HypothesisStateActive,
	}); err != nil {
		t.Fatalf("InsertHypothesis active: %v", err)
	}

	// Rejected hypothesis — must NOT appear.
	if _, err := srv.store.InsertHypothesis(store.Hypothesis{
		AgentID:   "agent-hyp",
		ProjectID: srv.projectID,
		Content:   "Old wrong theory",
		State:     store.HypothesisStateRejected,
	}); err != nil {
		t.Fatalf("InsertHypothesis rejected: %v", err)
	}

	if _, err := srv.handleEndSession(context.Background(), callTool(map[string]any{
		"agent_id": "agent-hyp",
		"summary":  "Investigating performance issues",
	})); err != nil {
		t.Fatalf("handleEndSession: %v", err)
	}

	mem, err := srv.store.GetLatestHandoff("agent-hyp", srv.projectID)
	if err != nil || mem == nil {
		t.Fatalf("GetLatestHandoff: err=%v, mem=%v", err, mem)
	}

	var payload handoffPayload
	if err := json.Unmarshal([]byte(mem.Content), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(payload.OpenHypotheses) == 0 {
		t.Fatal("expected OpenHypotheses to contain the active hypothesis, got empty")
	}
	for _, h := range payload.OpenHypotheses {
		if h == "Old wrong theory" {
			t.Error("rejected hypothesis must not appear in open_hypotheses")
		}
	}
}

// TestHandoff_QuickModeSkipsHandoff verifies that session_init in quick mode
// does NOT inject session_handoff, keeping lightweight sessions lean.
func TestHandoff_QuickModeSkipsHandoff(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// End session with summary so a handoff IS stored.
	if _, err := srv.handleEndSession(context.Background(), callTool(map[string]any{
		"agent_id": "agent-quick",
		"summary":  "Quick session summary",
	})); err != nil {
		t.Fatalf("handleEndSession: %v", err)
	}

	// session_init in quick mode — must NOT include session_handoff.
	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "agent-quick",
		"scope":    "quick",
	}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}

	text := extractText(t, res)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	briefing, _ := resp["_briefing"].(map[string]interface{})
	if _, has := briefing["session_handoff"]; has {
		t.Error("session_handoff must be absent in quick mode to keep lightweight sessions lean")
	}
}

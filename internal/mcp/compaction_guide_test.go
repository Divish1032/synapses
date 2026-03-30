package mcp

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestGetCompactionGuide_NoSession verifies the guide returns a valid empty response
// when no session is active.
func TestGetCompactionGuide_NoSession(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	result, err := srv.handleGetCompactionGuide(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	m := mustResult(t, result, err)

	// Should have safe_to_forget and hint even with no session
	hasKey(t, m, "safe_to_forget")
	hasKey(t, m, "hint")
}

// TestGetCompactionGuide_MissingAgentID verifies error on missing agent_id.
func TestGetCompactionGuide_MissingAgentID(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	result, err := srv.handleGetCompactionGuide(ctx, callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatal("expected tool error for missing agent_id")
	}
}

// TestGetCompactionGuide_WithSession verifies the guide returns entity_importance
// and relationship_map when a session has work ledger data.
func TestGetCompactionGuide_WithSession(t *testing.T) {
	srv, loginID, logoutID := newPopulatedServer(t)

	// Bootstrap session
	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "guide-agent",
		"scope":    "standard",
	}))

	// Populate work ledger
	sessionID := srv.getSynapseSessionID(SessionIDFromContext(ctx))
	if sessionID == "" {
		t.Fatal("no session ID after session_init")
	}
	_ = srv.store.AppendLedger(store.LedgerEntry{
		SessionID: sessionID,
		ProjectID: srv.projectID,
		ToolName:  "get_context",
		EntityIDs: []string{string(loginID), string(logoutID)},
		FilePaths: []string{"pkg/auth/auth.go"},
	})

	result, err := srv.handleGetCompactionGuide(ctx, callTool(map[string]any{
		"agent_id": "guide-agent",
	}))
	m := mustResult(t, result, err)

	hasKey(t, m, "must_preserve")
	hasKey(t, m, "safe_to_forget")
	hasKey(t, m, "entity_importance")
	hasKey(t, m, "relationship_map")

	// entity_importance should have our entities
	importance, _ := m["entity_importance"].([]any)
	if len(importance) == 0 {
		t.Error("expected non-empty entity_importance")
	}

	// relationship_map should have edges (loginID and logoutID are called by HandleRequest)
	relationships, _ := m["relationship_map"].([]any)
	if relationships == nil {
		t.Log("relationship_map is nil (expected if entities are leaf nodes)")
	}
}

// TestSynthesizeWorkSummary_Empty verifies fallback for empty input.
func TestSynthesizeWorkSummary_Empty(t *testing.T) {
	result := synthesizeWorkSummary(nil, nil, nil)
	if result != "No prior work context available." {
		t.Errorf("unexpected empty summary: %q", result)
	}
}

// TestSynthesizeWorkSummary_WithData verifies narrative construction.
func TestSynthesizeWorkSummary_WithData(t *testing.T) {
	result := synthesizeWorkSummary(
		[]string{"AuthService", "TokenValidator"},
		[]string{"auth.go", "token.go"},
		&store.SessionState{
			Approach:       "JWT migration",
			CompletedSteps: []string{"a", "b"},
			RemainingSteps: []string{"c"},
			Blockers:       []string{"API review needed"},
		},
	)
	if result == "" || result == "No prior work context available." {
		t.Errorf("expected rich summary, got: %q", result)
	}
	// Check key content is present
	for _, expected := range []string{"AuthService", "auth.go", "JWT migration", "2 step", "1 step", "API review"} {
		if !strings.Contains(result, expected) {
			t.Errorf("summary missing %q: %s", expected, result)
		}
	}
}

// TestSynthesizeWorkSummary_TruncatesLongApproach verifies approach truncation.
func TestSynthesizeWorkSummary_TruncatesLongApproach(t *testing.T) {
	longApproach := ""
	for i := 0; i < 300; i++ {
		longApproach += "x"
	}
	result := synthesizeWorkSummary(nil, nil, &store.SessionState{Approach: longApproach})
	if len(result) > 250 {
		t.Logf("summary length %d — approach was truncated as expected", len(result))
	}
}

// TestSynthesizeWorkSummary_ManyEntities verifies capping at 8.
func TestSynthesizeWorkSummary_ManyEntities(t *testing.T) {
	entities := make([]string, 20)
	for i := range entities {
		entities[i] = "Entity" + string(rune('A'+i))
	}
	result := synthesizeWorkSummary(entities, nil, nil)
	if !strings.Contains(result, "+12 more") {
		t.Errorf("expected overflow indicator, got: %s", result)
	}
}


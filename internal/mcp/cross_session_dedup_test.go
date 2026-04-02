package mcp

// Sprint 25.4: cross-session exploration dedup integration tests.
// White-box tests (package mcp) so we can call registerSynapseSession and
// AppendExplorationEntry directly to set up state without going through
// full session_init.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestGetContext_PriorExploration_InjectedWhenEntityExploredBefore verifies that
// the prior_exploration field is present in get_context when the entity was
// explored in a prior session, and absent when it was not.
func TestGetContext_PriorExploration_InjectedWhenEntityExploredBefore(t *testing.T) {
	srv, ids := newServerWithDirectionalGraph(t)
	_ = ids

	// Register a current session so getSynapseSessionID returns a non-empty ID.
	const mcpSessID = "test-mcp-sess"
	const synapsesSessID = "test-synapses-sess"
	srv.registerSynapseSession(mcpSessID, synapsesSessID, "agent-1", "test-model")

	// Seed prior session exploration for "Target" (the entity we'll query).
	priorEntries := []store.ExplorationEntry{
		{
			SessionID:      "prior-sess-1",
			ProjectID:      srv.projectID,
			ToolName:       "get_context",
			EntityQueried:  "Target",
			FindingSummary: "Target: 1 caller (CallerA), 1 callee (CalleeB)",
		},
		{
			SessionID:      "prior-sess-2",
			ProjectID:      srv.projectID,
			ToolName:       "get_context",
			EntityQueried:  "Target",
			FindingSummary: "Target: no security violations found",
		},
	}
	for _, e := range priorEntries {
		if err := srv.store.AppendExplorationEntry(e); err != nil {
			t.Fatalf("AppendExplorationEntry: %v", err)
		}
	}

	// Call get_context for "Target" in the current session.
	ctx := WithSessionID(context.Background(), mcpSessID)
	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": "Target",
		"format": "json",
	}))
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", extractErrorText(t, res))
	}

	raw := extractJSON(t, res)
	if raw == nil {
		t.Fatal("nil JSON response")
	}

	// prior_exploration must be present since Target was explored in prior sessions.
	pe, ok := raw["prior_exploration"].(map[string]interface{})
	if !ok || pe == nil {
		t.Fatal("expected prior_exploration in get_context response, got none")
	}
	hitCount, _ := pe["hit_count"].(float64)
	if hitCount < 2 {
		t.Errorf("expected hit_count >= 2, got %.0f", hitCount)
	}
	note, _ := pe["note"].(string)
	if note == "" {
		t.Error("prior_exploration.note should not be empty")
	}
	topFinding, _ := pe["top_finding"].(string)
	if topFinding == "" {
		t.Error("prior_exploration.top_finding should not be empty (seeded entries have findings)")
	}
}

// TestGetContext_PriorExploration_AbsentWhenNoHistory verifies that prior_exploration
// is NOT injected when the entity has never been explored before.
func TestGetContext_PriorExploration_AbsentWhenNoHistory(t *testing.T) {
	srv, ids := newServerWithDirectionalGraph(t)
	_ = ids

	const mcpSessID = "clean-mcp-sess"
	const synapsesSessID = "clean-synapses-sess"
	srv.registerSynapseSession(mcpSessID, synapsesSessID, "agent-clean", "test-model")
	// No exploration log entries seeded — fresh start.

	ctx := WithSessionID(context.Background(), mcpSessID)
	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": "Target",
		"format": "json",
	}))
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", extractErrorText(t, res))
	}

	raw := extractJSON(t, res)
	if raw == nil {
		t.Fatal("nil JSON response")
	}

	if _, ok := raw["prior_exploration"]; ok {
		t.Error("prior_exploration should be absent when entity has no prior-session history")
	}
}

// TestGetContext_PriorExploration_ExcludesCurrentSession verifies that exploration
// entries from the current session do NOT trigger prior_exploration injection.
// This prevents false positives where the agent gets a "you explored this before"
// note for work it just did in the same session.
func TestGetContext_PriorExploration_ExcludesCurrentSession(t *testing.T) {
	srv, ids := newServerWithDirectionalGraph(t)
	_ = ids

	const mcpSessID = "same-mcp-sess"
	const synapsesSessID = "same-synapses-sess"
	srv.registerSynapseSession(mcpSessID, synapsesSessID, "agent-same", "test-model")

	// Exploration entry in the CURRENT session — must not trigger injection.
	if err := srv.store.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:      synapsesSessID,
		ProjectID:      srv.projectID,
		ToolName:       "get_context",
		EntityQueried:  "Target",
		FindingSummary: "current session finding",
	}); err != nil {
		t.Fatalf("AppendExplorationEntry: %v", err)
	}

	ctx := WithSessionID(context.Background(), mcpSessID)
	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": "Target",
		"format": "json",
	}))
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", extractErrorText(t, res))
	}

	raw := extractJSON(t, res)
	if raw == nil {
		t.Fatal("nil JSON response")
	}

	// Current-session-only exploration MUST NOT produce prior_exploration.
	if _, ok := raw["prior_exploration"]; ok {
		t.Error("prior_exploration must not be injected for current-session-only exploration history")
	}
}

// TestSessionInit_PreviouslyExplored_InBriefing verifies that session_init
// includes a previously_explored section in _briefing when entities were
// explored at least minHits(=2) times in prior sessions.
func TestSessionInit_PreviouslyExplored_InBriefing(t *testing.T) {
	srv, _, authID := newPopulatedServer(t)
	_ = authID

	// Seed prior session explorations: AuthLogin explored 3 times in prior sessions.
	for i := 0; i < 3; i++ {
		if err := srv.store.AppendExplorationEntry(store.ExplorationEntry{
			SessionID:      "prior-sess",
			ProjectID:      srv.projectID,
			ToolName:       "get_context",
			EntityQueried:  "AuthLogin",
			FindingSummary: "AuthLogin: 2 callers, handles OAuth flow",
		}); err != nil {
			t.Fatalf("AppendExplorationEntry: %v", err)
		}
	}

	// Call session_init and capture the session ID it creates.
	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "agent-briefing-test",
		"scope":    "full",
	}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", extractErrorText(t, res))
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

	pe, ok := briefing["previously_explored"].(map[string]interface{})
	if !ok || pe == nil {
		t.Fatal("previously_explored missing from _briefing (expected for entity explored 3 times in prior session)")
	}
	entities, ok := pe["entities"].([]interface{})
	if !ok || len(entities) == 0 {
		t.Fatal("previously_explored.entities should be non-empty")
	}
	note, _ := pe["note"].(string)
	if note == "" {
		t.Error("previously_explored.note should not be empty")
	}

	// Verify AuthLogin appears in the list.
	found := false
	for _, item := range entities {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if m["entity"] == "AuthLogin" {
			found = true
			hitCount, _ := m["hit_count"].(float64)
			if hitCount < 3 {
				t.Errorf("AuthLogin hit_count: got %.0f, want >= 3", hitCount)
			}
			// All 3 hits come from the same "prior-sess" → session_count must be 1.
			sessionCount, _ := m["session_count"].(float64)
			if sessionCount != 1 {
				t.Errorf("AuthLogin session_count: got %.0f, want 1", sessionCount)
			}
		}
	}
	if !found {
		t.Errorf("AuthLogin not found in previously_explored.entities: %v", entities)
	}
}

// TestSessionInit_PreviouslyExplored_AbsentForNewProject verifies that
// previously_explored is NOT added when there is no prior exploration history.
func TestSessionInit_PreviouslyExplored_AbsentForNewProject(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)
	// No exploration entries seeded.

	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "agent-new",
		"scope":    "full",
	}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", extractErrorText(t, res))
	}

	text := extractText(t, res)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	briefing, _ := resp["_briefing"].(map[string]interface{})
	if _, hasPE := briefing["previously_explored"]; hasPE {
		t.Error("previously_explored should be absent when no prior exploration history exists")
	}
}

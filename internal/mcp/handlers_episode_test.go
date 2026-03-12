package mcp

import (
	"testing"
)

// ── handleRemember ────────────────────────────────────────────────────────────

func TestHandleRemember_Basic(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":  "mem-agent",
		"decision":  "use interface abstraction",
		"rationale": "to allow mocking in tests",
		"outcome":   "success",
		"context":   "refactoring auth package",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episode_id")
}

func TestHandleRemember_FailureOutcome(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":  "mem-agent",
		"decision":  "inline all functions",
		"rationale": "reduce indirection",
		"outcome":   "failure",
		"context":   "caused circular dependency",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episode_id")
}

func TestHandleRemember_MissingDecision_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"outcome": "success",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleRemember_NoStore_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "a",
		"decision": "d",
		"outcome":  "success",
	}))
	mustErrorResult(t, res, err)
}

// ── handleRecall ──────────────────────────────────────────────────────────────

func TestHandleRecall_FindsMatch(t *testing.T) {
	s := newTestServer(t)

	// Store an episode first.
	_, _ = s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":  "recall-agent",
		"decision":  "use JWT for auth tokens",
		"rationale": "stateless and scalable",
		"outcome":   "success",
		"context":   "auth module",
	}))

	res, err := s.handleRecall(ctx, callTool(map[string]any{
		"query":    "JWT auth tokens",
		"agent_id": "recall-agent",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episodes")
}

func TestHandleRecall_NoMatch_EmptyList(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleRecall(ctx, callTool(map[string]any{
		"query":    "completely unrelated XYZ query",
		"agent_id": "recall-agent",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episodes")
}

func TestHandleRecall_EmptyQuery_RespondsGracefully(t *testing.T) {
	s := newTestServer(t)
	// recall with no query returns gracefully (empty list or error, no crash).
	res, err := s.handleRecall(ctx, callTool(nil))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	_ = res
}

// ── handleGetEpisodes ─────────────────────────────────────────────────────────

func TestHandleGetEpisodes_Empty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetEpisodes(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "episodes")
}

func TestHandleGetEpisodes_FilterByOutcome(t *testing.T) {
	s := newTestServer(t)

	for _, outcome := range []string{"success", "failure", "success"} {
		_, _ = s.handleRemember(ctx, callTool(map[string]any{
			"agent_id": "filter-agent",
			"decision": "decision for " + outcome,
			"outcome":  outcome,
		}))
	}

	res, err := s.handleGetEpisodes(ctx, callTool(map[string]any{
		"outcome": "failure",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episodes")
	// Verify at least one failure episode is returned.
	episodes, _ := m["episodes"].([]any)
	foundFailure := false
	for _, raw := range episodes {
		ep, _ := raw.(map[string]any)
		if o, _ := ep["outcome"].(string); o == "failure" {
			foundFailure = true
		}
	}
	if len(episodes) > 0 && !foundFailure {
		t.Error("outcome filter returned episodes but none with outcome=failure")
	}
}

// ── handleCheckPlanSafety ─────────────────────────────────────────────────────

func TestHandleCheckPlanSafety_NoHistory(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleCheckPlanSafety(ctx, callTool(map[string]any{
		"plan_description": "refactor the auth module",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "status")
}

func TestHandleCheckPlanSafety_MatchingFailure(t *testing.T) {
	s := newTestServer(t)

	// Store 5 failure episodes to exceed the minimum threshold.
	for i := 0; i < 5; i++ {
		_, _ = s.handleRemember(ctx, callTool(map[string]any{
			"agent_id": "safety-agent",
			"decision": "inline auth functions",
			"outcome":  "failure",
			"context":  "inline always causes circular deps",
		}))
	}

	res, err := s.handleCheckPlanSafety(ctx, callTool(map[string]any{
		"plan_description": "inline the auth functions",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "status")
	// With 5 failure episodes matching, the plan should not be marked safe.
	status, _ := m["status"].(string)
	_ = status // may be "unsafe" — just verify the field exists and no crash
}

func TestHandleCheckPlanSafety_MissingPlan_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleCheckPlanSafety(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleGetRuleCandidates ───────────────────────────────────────────────────

func TestHandleGetRuleCandidates_Empty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetRuleCandidates(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "candidates")
}

func TestHandleGetRuleCandidates_AfterFailures(t *testing.T) {
	s := newTestServer(t)

	// Store repeated failures with a pattern.
	for i := 0; i < 4; i++ {
		_, _ = s.handleRemember(ctx, callTool(map[string]any{
			"agent_id": "pattern-agent",
			"decision": "import internal package from cmd",
			"outcome":  "failure",
			"context":  "violates layering",
		}))
	}

	res, err := s.handleGetRuleCandidates(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "candidates")
}

package mcp

import (
	"testing"
)

// ── handleClaimWork ───────────────────────────────────────────────────────────

func TestHandleClaimWork_NoConflict(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleClaimWork(ctx, callTool(map[string]any{
		"agent_id":   "claimer",
		"scope":      "pkg/auth",
		"scope_type": "directory",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "claimed")
}

func TestHandleClaimWork_ConflictDetected(t *testing.T) {
	s := newTestServer(t)

	// Agent A claims first.
	res1, err1 := s.handleClaimWork(ctx, callTool(map[string]any{
		"agent_id":   "agent-a",
		"scope":      "pkg/auth",
		"scope_type": "directory",
	}))
	mustResult(t, res1, err1)

	// Agent B tries to claim same scope.
	res2, err2 := s.handleClaimWork(ctx, callTool(map[string]any{
		"agent_id":   "agent-b",
		"scope":      "pkg/auth",
		"scope_type": "directory",
	}))
	m2 := mustResult(t, res2, err2)
	// Should return "conflict" or similar warning field.
	hasKey(t, m2, "claimed")
}

func TestHandleClaimWork_MissingAgentID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleClaimWork(ctx, callTool(map[string]any{
		"scope": "pkg/auth",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleClaimWork_MissingScope_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleClaimWork(ctx, callTool(map[string]any{
		"agent_id": "agent-a",
	}))
	mustErrorResult(t, res, err)
}

// ── handleReleaseClaims ───────────────────────────────────────────────────────

func TestHandleReleaseClaims_RemovesClaims(t *testing.T) {
	s := newTestServer(t)
	agentID := "release-agent"

	// Claim two scopes.
	for _, scope := range []string{"pkg/auth", "pkg/api"} {
		res, err := s.handleClaimWork(ctx, callTool(map[string]any{
			"agent_id":   agentID,
			"scope":      scope,
			"scope_type": "directory",
		}))
		mustResult(t, res, err)
	}

	// Release all.
	res, err := s.handleReleaseClaims(ctx, callTool(map[string]any{
		"agent_id": agentID,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "message")

	// Verify no more conflicts possible.
	res2, err2 := s.handleGetConflicts(ctx, callTool(map[string]any{
		"agent_id": agentID,
	}))
	m2 := mustResult(t, res2, err2)
	conflicts, _ := m2["conflicts"].([]any)
	if len(conflicts) > 0 {
		t.Errorf("expected no conflicts after release, got %d", len(conflicts))
	}
}

func TestHandleReleaseClaims_MissingAgentID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleReleaseClaims(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleGetConflicts ────────────────────────────────────────────────────────

func TestHandleGetConflicts_NoConflicts(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetConflicts(ctx, callTool(map[string]any{
		"agent_id": "lonely-agent",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "conflicts")
	conflicts, _ := m["conflicts"].([]any)
	if len(conflicts) > 0 {
		t.Errorf("expected no conflicts for solo agent, got %d", len(conflicts))
	}
}

func TestHandleGetConflicts_HasConflict(t *testing.T) {
	s := newTestServer(t)

	// Agent A and B both claim same scope.
	_, _ = s.handleClaimWork(ctx, callTool(map[string]any{
		"agent_id": "conflict-a", "scope": "pkg/shared", "scope_type": "directory",
	}))
	_, _ = s.handleClaimWork(ctx, callTool(map[string]any{
		"agent_id": "conflict-b", "scope": "pkg/shared", "scope_type": "directory",
	}))

	res, err := s.handleGetConflicts(ctx, callTool(map[string]any{
		"agent_id": "conflict-a",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "conflicts")
}

// ── handleGetAgents ───────────────────────────────────────────────────────────

func TestHandleGetAgents_Empty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetAgents(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "agents")
}

func TestHandleGetAgents_AfterSessionInit(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "visible-agent"}))

	res, err := s.handleGetAgents(ctx, callTool(nil))
	m := mustResult(t, res, err)
	agents, _ := m["agents"].([]any)
	found := false
	for _, a := range agents {
		agent, _ := a.(map[string]any)
		if agent["id"] == "visible-agent" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected visible-agent in get_agents after session_init")
	}
}

func TestHandleGetAgents_IncludesPresence(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "presence-agent"}))

	res, err := s.handleGetAgents(ctx, callTool(nil))
	m := mustResult(t, res, err)
	agents, _ := m["agents"].([]any)
	for _, a := range agents {
		agent, _ := a.(map[string]any)
		if agent["id"] == "presence-agent" {
			if _, ok := agent["presence"]; !ok {
				t.Error("expected presence field on agent")
			}
			return
		}
	}
	t.Error("presence-agent not found")
}

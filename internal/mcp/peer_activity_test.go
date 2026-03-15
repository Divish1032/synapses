package mcp

import (
	"context"
	"testing"
)

// ── get_peer_activity (Tier 3, on-demand) ─────────────────────────────────────

func TestGetPeerActivity_MissingAgentID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetPeerActivity(context.Background(), callTool(map[string]any{}))
	mustErrorResult(t, res, err)
}

func TestGetPeerActivity_UnknownAgent_ReturnsErrorField(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetPeerActivity(context.Background(), callTool(map[string]any{
		"agent_id": "ghost-agent",
	}))
	m := mustResult(t, res, err)
	if _, ok := m["error"]; !ok {
		t.Error("expected 'error' field for unknown agent_id, got none")
	}
}

func TestGetPeerActivity_KnownAgent_ReturnsDigest(t *testing.T) {
	s := newTestServer(t)

	// Register the peer via session_init so it appears in the agents table.
	_, _ = s.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "peer-agent",
		"intent":   "implementing auth module",
	}))

	res, err := s.handleGetPeerActivity(context.Background(), callTool(map[string]any{
		"agent_id": "peer-agent",
	}))
	m := mustResult(t, res, err)

	// Must return structured digest fields.
	if _, ok := m["agent_id"]; !ok {
		t.Error("expected agent_id in response")
	}
	if _, ok := m["presence"]; !ok {
		t.Error("expected presence in response")
	}
	if _, ok := m["recent_actions"]; !ok {
		t.Error("expected recent_actions in response")
	}
	if _, ok := m["latest_seq"]; !ok {
		t.Error("expected latest_seq in response")
	}
}

func TestGetPeerActivity_IntentSurfaced(t *testing.T) {
	s := newTestServer(t)

	_, _ = s.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "focused-agent",
		"intent":   "fixing auth race condition",
	}))

	res, err := s.handleGetPeerActivity(context.Background(), callTool(map[string]any{
		"agent_id": "focused-agent",
	}))
	m := mustResult(t, res, err)

	if intent, _ := m["intent"].(string); intent != "fixing auth race condition" {
		t.Errorf("expected intent='fixing auth race condition', got %q", intent)
	}
}

func TestGetPeerActivity_ScopesIncluded(t *testing.T) {
	s := newTestServer(t)

	// Register peer and claim a scope.
	_, _ = s.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "scope-agent",
	}))
	_, _ = s.handleClaimWork(context.Background(), callTool(map[string]any{
		"agent_id":   "scope-agent",
		"scope":      "pkg/auth",
		"scope_type": "directory",
	}))

	res, err := s.handleGetPeerActivity(context.Background(), callTool(map[string]any{
		"agent_id": "scope-agent",
	}))
	m := mustResult(t, res, err)

	scopes, _ := m["scopes"].([]any)
	if len(scopes) == 0 {
		t.Error("expected scopes to include pkg/auth claim")
	}
}

func TestGetPeerActivity_HintPresent(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleSessionInit(context.Background(), callTool(map[string]any{"agent_id": "hint-agent"}))

	res, err := s.handleGetPeerActivity(context.Background(), callTool(map[string]any{
		"agent_id": "hint-agent",
	}))
	m := mustResult(t, res, err)

	if _, ok := m["hint"]; !ok {
		t.Error("expected hint field in get_peer_activity response")
	}
}

func TestGetPeerActivity_SinceSeqFiltering(t *testing.T) {
	// Pass since_seq > 0 — should return events only after that cursor.
	s := newTestServer(t)
	_, _ = s.handleSessionInit(context.Background(), callTool(map[string]any{"agent_id": "seq-agent"}))

	// Get the current latest_seq.
	res1, err1 := s.handleGetPeerActivity(context.Background(), callTool(map[string]any{
		"agent_id": "seq-agent",
	}))
	m1 := mustResult(t, res1, err1)
	latestSeq, _ := m1["latest_seq"].(float64)

	// Emit a new event for this agent.
	_ = s.store.AppendEvent("agent_examining", "seq-agent", `{"entity":"AuthLogin"}`)

	res2, err2 := s.handleGetPeerActivity(context.Background(), callTool(map[string]any{
		"agent_id":  "seq-agent",
		"since_seq": latestSeq,
	}))
	m2 := mustResult(t, res2, err2)

	actions2, _ := m2["recent_actions"].([]any)
	if len(actions2) == 0 {
		t.Error("expected ≥1 action since previous seq after new event was appended")
	}
}

// ── agent_awareness in session_init ───────────────────────────────────────────

func TestSessionInit_NoConflictsNoAlerts_NoAgentAwareness(t *testing.T) {
	// Solo session: no peers, no conflicts, no dep alerts → agent_awareness omitted.
	s := newTestServer(t)
	res, err := s.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "solo-agent",
	}))
	m := mustResult(t, res, err)
	if _, ok := m["agent_awareness"]; ok {
		t.Error("expected agent_awareness to be omitted in a solo session with no signals")
	}
}

func TestSessionInit_ActivePeer_SurfacesActiveCount(t *testing.T) {
	s := newTestServer(t)

	// Register a peer.
	_, _ = s.handleSessionInit(context.Background(), callTool(map[string]any{"agent_id": "peer-a"}))

	// Main agent's session_init should see active_count.
	res, err := s.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "main-agent",
	}))
	m := mustResult(t, res, err)

	awareness, ok := m["agent_awareness"].(map[string]any)
	if !ok {
		t.Fatal("expected agent_awareness when a peer is active")
	}
	if _, ok := awareness["active_count"]; !ok {
		t.Error("expected active_count in agent_awareness")
	}
}

func TestSessionInit_ConflictingClaim_SurfacesConflicts(t *testing.T) {
	s := newTestServer(t)

	// Peer-A claims a scope.
	_, _ = s.handleClaimWork(context.Background(), callTool(map[string]any{
		"agent_id":   "peer-a",
		"scope":      "pkg/auth",
		"scope_type": "directory",
	}))

	// main-agent also claims the same scope.
	_, _ = s.handleClaimWork(context.Background(), callTool(map[string]any{
		"agent_id":   "main-agent",
		"scope":      "pkg/auth",
		"scope_type": "directory",
	}))

	// session_init for main-agent should surface conflicts.
	res, err := s.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "main-agent",
	}))
	m := mustResult(t, res, err)

	awareness, ok := m["agent_awareness"].(map[string]any)
	if !ok {
		t.Fatal("expected agent_awareness with conflict")
	}
	conflicts, _ := awareness["conflicts"].([]any)
	if len(conflicts) == 0 {
		t.Error("expected at least one conflict in agent_awareness.conflicts")
	}
}

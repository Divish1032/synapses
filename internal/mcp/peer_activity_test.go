package mcp

// Sprint 25.7: cross-agent exploration sharing integration tests.
// White-box tests (package mcp) so we can call registerSynapseSession and
// store methods directly to set up state without going through full session_init.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// insertHandoffMemory is a helper that inserts a handoff memory for the given
// agent and project directly into srv.store, bypassing end_session flow.
func insertHandoffMemory(t *testing.T, srv *Server, agentID, projectID, content string) {
	t.Helper()
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:          store.TierSessionLog,
		Content:       content,
		AgentID:       agentID,
		Source:        store.SourceAuto,
		Tags:          `["handoff","session_end","auto"]`,
		SourceProject: projectID,
	})
	if err != nil {
		t.Fatalf("insertHandoffMemory agent=%s: %v", agentID, err)
	}
}

// TestSessionInit_PeerActivity_PresentWhenPeerHasHandoff verifies that
// _briefing.peer_activity appears when another agent has a recent handoff
// on the same project.
func TestSessionInit_PeerActivity_PresentWhenPeerHasHandoff(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// Agent "implementer" left a handoff on this project.
	insertHandoffMemory(t, srv, "implementer", srv.projectID,
		`{"agent_summary":"Implemented OAuth flow, all unit tests passing","accomplished":["add OAuth"],"remaining":["write e2e tests"]}`)

	// Agent "reviewer" starts a new session.
	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "reviewer",
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

	pa, ok := briefing["peer_activity"].(map[string]interface{})
	if !ok || pa == nil {
		t.Fatal("peer_activity missing from _briefing — expected because implementer has a recent handoff")
	}

	// note must be non-empty.
	note, _ := pa["note"].(string)
	if note == "" {
		t.Error("peer_activity.note must not be empty")
	}

	// peers must contain the implementer entry.
	peers, ok := pa["peers"].([]interface{})
	if !ok || len(peers) == 0 {
		t.Fatal("peer_activity.peers must be non-empty")
	}

	peer, ok := peers[0].(map[string]interface{})
	if !ok {
		t.Fatal("peers[0] must be a map")
	}
	if peer["agent_id"] != "implementer" {
		t.Errorf("expected peer agent_id='implementer', got %v", peer["agent_id"])
	}
	if _, hasSum := peer["summary"]; !hasSum {
		t.Error("expected peer entry to have a summary from the handoff")
	}
}

// TestSessionInit_PeerActivity_AbsentWhenNoPeerHandoffs verifies that
// _briefing.peer_activity is NOT added when no peer agents have handoffs.
func TestSessionInit_PeerActivity_AbsentWhenNoPeerHandoffs(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)
	// No handoffs seeded.

	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "solo-reviewer",
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
	if _, hasPeer := briefing["peer_activity"]; hasPeer {
		t.Error("peer_activity must be absent when there are no peer handoffs")
	}
}

// TestSessionInit_PeerActivity_DoesNotIncludeOwnHandoff verifies that the
// calling agent's own handoff does NOT appear in peer_activity (only OTHER
// agents' work is surfaced there; own handoff goes in session_handoff).
func TestSessionInit_PeerActivity_DoesNotIncludeOwnHandoff(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// The calling agent ("solo") has a handoff from a prior session.
	insertHandoffMemory(t, srv, "solo", srv.projectID,
		`{"agent_summary":"Solo's own prior work","accomplished":["refactor db layer"]}`)

	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "solo",
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
	if _, hasPeer := briefing["peer_activity"]; hasPeer {
		t.Error("peer_activity must NOT include the calling agent's own handoff")
	}
	// session_handoff should still surface the agent's own prior work.
	if _, hasHandoff := briefing["session_handoff"]; !hasHandoff {
		t.Error("session_handoff should be present for the agent's own prior session")
	}
}

// TestSessionInit_PeerActivity_AbsentInQuickMode verifies that peer_activity is
// skipped when scope=quick (same policy as session_handoff).
func TestSessionInit_PeerActivity_AbsentInQuickMode(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// Peer handoff exists but should not surface in quick mode.
	insertHandoffMemory(t, srv, "implementer", srv.projectID,
		`{"agent_summary":"did some work"}`)

	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "reviewer",
		"scope":    "quick",
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
	if _, hasPeer := briefing["peer_activity"]; hasPeer {
		t.Error("peer_activity must be absent in quick mode")
	}
}

// TestSessionInit_PeerActivity_MultiplePeers verifies that when two different
// peer agents have recent handoffs, both appear in peer_activity.peers.
func TestSessionInit_PeerActivity_MultiplePeers(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	insertHandoffMemory(t, srv, "agent-alpha", srv.projectID,
		`{"agent_summary":"Alpha worked on the parser","accomplished":["parser refactor"]}`)
	insertHandoffMemory(t, srv, "agent-beta", srv.projectID,
		`{"agent_summary":"Beta worked on the tests","remaining":["integration tests"]}`)

	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "agent-gamma",
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

	briefing, ok := resp["_briefing"].(map[string]interface{})
	if !ok {
		t.Fatal("_briefing missing")
	}
	pa, ok := briefing["peer_activity"].(map[string]interface{})
	if !ok || pa == nil {
		t.Fatal("peer_activity missing — expected 2 peers")
	}
	peers, _ := pa["peers"].([]interface{})
	if len(peers) != 2 {
		t.Errorf("expected 2 peer entries (alpha + beta), got %d", len(peers))
	}
	// note should mention "2 peer agents"
	note, _ := pa["note"].(string)
	if note == "" {
		t.Error("note must not be empty")
	}
}

// TestSessionInit_PeerActivity_ExploredEntitiesFromHandoff verifies that
// explored_entities from the peer's handoff payload appear in the peer entry.
func TestSessionInit_PeerActivity_ExploredEntitiesFromHandoff(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	insertHandoffMemory(t, srv, "explorer", srv.projectID,
		`{"agent_summary":"Explored auth subsystem","explored_entities":["AuthHandler","TokenValidator","SessionStore"]}`)

	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "newcomer",
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

	briefing, ok := resp["_briefing"].(map[string]interface{})
	if !ok {
		t.Fatal("_briefing missing")
	}
	pa, ok := briefing["peer_activity"].(map[string]interface{})
	if !ok || pa == nil {
		t.Fatal("peer_activity missing")
	}
	peers, _ := pa["peers"].([]interface{})
	if len(peers) == 0 {
		t.Fatal("peers must be non-empty")
	}
	peer, _ := peers[0].(map[string]interface{})
	exploredEntities, ok := peer["explored_entities"].([]interface{})
	if !ok || len(exploredEntities) == 0 {
		t.Error("peer entry must include explored_entities from the handoff payload")
	}
}

// TestSessionInit_PeerActivity_WithHypothesesOnly verifies that peer_activity
// appears even when a peer has active hypotheses but no handoff memory.
func TestSessionInit_PeerActivity_WithHypothesesOnly(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// Peer has an active hypothesis but no handoff.
	_, err := srv.store.InsertHypothesis(store.Hypothesis{
		AgentID:   "analyst",
		ProjectID: srv.projectID,
		Content:   "I think the race is in the session store's cleanup goroutine",
	})
	if err != nil {
		t.Fatalf("InsertHypothesis: %v", err)
	}

	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "developer",
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

	briefing, ok := resp["_briefing"].(map[string]interface{})
	if !ok {
		t.Fatal("_briefing missing")
	}

	pa, ok := briefing["peer_activity"].(map[string]interface{})
	if !ok || pa == nil {
		t.Fatal("peer_activity must be present when peer has active hypotheses")
	}

	peers, _ := pa["peers"].([]interface{})
	if len(peers) == 0 {
		t.Fatal("peers must be non-empty")
	}
	peer, _ := peers[0].(map[string]interface{})
	openHyps, _ := peer["open_hypotheses"].([]interface{})
	if len(openHyps) == 0 {
		t.Error("peer entry must include open_hypotheses when peer has active hypotheses")
	}
}

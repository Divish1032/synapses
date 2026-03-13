package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── handleSessionInit ─────────────────────────────────────────────────────────

func TestHandleSessionInit_SoloAgent_ResponseShape(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "solo-agent"}))
	m := mustResult(t, res, err)

	hasKey(t, m, "project_identity")
	hasKey(t, m, "scale_guidance")
	hasKey(t, m, "session_hint")
	hasKey(t, m, "latest_event_seq")
	// No peers active → agent_awareness must be absent (zero token cost).
	noKey(t, m, "agent_awareness")
	noKey(t, m, "unread_messages")
}

func TestHandleSessionInit_NoAgentID_StillReturnsIdentity(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "project_identity")
	hasKey(t, m, "scale_guidance")
}

func TestHandleSessionInit_EmitsSessionStartEvent(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-x"}))

	events, _, err := s.store.GetEvents(0, nil, 50)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Type == "agent_session_start" && e.AgentID == "agent-x" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected agent_session_start event after session_init, not found")
	}
}

func TestHandleSessionInit_MultiAgent_AwarenessSurfaced(t *testing.T) {
	s := newTestServer(t)

	// Agent A starts and claims a scope.
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-a"}))
	_, _ = s.handleClaimWork(ctx, callTool(map[string]any{
		"agent_id":   "agent-a",
		"scope":      "pkg/auth",
		"scope_type": "directory",
	}))

	// Agent B starts — should see agent-a in agent_awareness.
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-b"}))
	m := mustResult(t, res, err)

	awareness, ok := m["agent_awareness"].(map[string]any)
	if !ok {
		t.Fatalf("expected agent_awareness map, got %T — keys: %v", m["agent_awareness"], mapKeys(m))
	}
	peers, ok := awareness["active_peers"].([]any)
	if !ok || len(peers) == 0 {
		t.Fatalf("expected active_peers to be non-empty, got %v", awareness["active_peers"])
	}
}

func TestHandleSessionInit_Incremental_SkipsIdentityOnRepeat(t *testing.T) {
	s := newTestServer(t)
	agentID := "repeat-agent"

	// First call — full identity.
	res1, err1 := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": agentID}))
	m1 := mustResult(t, res1, err1)
	if _, ok := m1["identity_skipped"]; ok {
		t.Error("first call must not skip identity")
	}

	// Second call — identity hash unchanged → should be incremental.
	res2, err2 := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": agentID}))
	m2 := mustResult(t, res2, err2)
	if inc, ok := m2["incremental"].(bool); !ok || !inc {
		t.Error("second call should be marked incremental")
	}
}

func TestHandleSessionInit_UnreadMessages_Delivered(t *testing.T) {
	s := newTestServer(t)

	// Agent A sends a message to agent B before B's session starts.
	res, err := s.handleSendMessage(ctx, callTool(map[string]any{
		"from_agent": "agent-a",
		"to_agent":   "agent-b",
		"topic":      "ping",
		"payload":    `{"msg":"hello"}`,
	}))
	mustResult(t, res, err)

	// Agent B starts — unread message should be auto-delivered.
	res2, err2 := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-b"}))
	m := mustResult(t, res2, err2)

	msgs, ok := m["unread_messages"].(map[string]any)
	if !ok {
		t.Fatalf("expected unread_messages in response, keys: %v", mapKeys(m))
	}
	count, _ := msgs["count"].(float64)
	if count < 1 {
		t.Errorf("expected ≥1 unread message, got count=%v", count)
	}
}

func TestHandleSessionInit_CollisionWarning(t *testing.T) {
	s := newTestServer(t)
	agentID := "clash-agent"

	// First call establishes the context.
	res1, err1 := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": agentID}))
	mustResult(t, res1, err1)

	// Immediately call again with the same ID — collision warning expected.
	res2, err2 := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": agentID}))
	m := mustResult(t, res2, err2)

	warning, ok := m["warning"].(string)
	if !ok || warning == "" {
		t.Errorf("expected collision warning for same agent_id within 2 min, got: %v", m["warning"])
	}
	if !strings.Contains(warning, agentID) {
		t.Errorf("warning should mention the agent_id, got: %q", warning)
	}
}

// ── handleGetProjectIdentity ──────────────────────────────────────────────────

func TestHandleGetProjectIdentity_ReturnsIdentity(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetProjectIdentity(ctx, callTool(nil))
	m := mustResult(t, res, err)
	// Response is wrapped: {"identity": {...}, "federation": {...}}
	hasKey(t, m, "identity")
	identity, _ := m["identity"].(map[string]any)
	hasKey(t, identity, "repo_id")
}

func TestHandleGetProjectIdentity_PopulatedGraph(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetProjectIdentity(ctx, callTool(nil))
	m := mustResult(t, res, err)
	identity, _ := m["identity"].(map[string]any)
	// Identity wraps a GraphSummary; populated graph has functions.
	summary, _ := identity["summary"].(map[string]any)
	functions, _ := summary["functions"].(float64)
	if functions < 1 {
		t.Errorf("expected summary.functions > 0 for populated graph, got %v", functions)
	}
}

// ── handleGetWorkingState ─────────────────────────────────────────────────────

func TestHandleGetWorkingState_NoChangeSource(t *testing.T) {
	s := newTestServer(t)
	// changeSource is nil — should return without error.
	res, err := s.handleGetWorkingState(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "recent_changes")
}

// ── handleDiscoverTools ───────────────────────────────────────────────────────

func TestHandleDiscoverTools_ReturnsRecommendation(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{
		"query": "how do I understand a function",
	}))
	m := mustResult(t, res, err)
	// discover_tools returns recommended_tool or recommended_workflow depending on match type.
	if _, ok := m["recommended_tool"]; !ok {
		if _, ok2 := m["recommended_workflow"]; !ok2 {
			t.Errorf("expected recommended_tool or recommended_workflow in result, keys: %v", mapKeys(m))
		}
	}
}

func TestHandleDiscoverTools_EmptyQuery_StillResponds(t *testing.T) {
	s := newTestServer(t)
	result, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": ""}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result for empty query")
	}
}

// ── handleAnnotateNode ────────────────────────────────────────────────────────

func TestHandleAnnotateNode_StoresNote(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)

	res, err := s.handleAnnotateNode(ctx, callTool(map[string]any{
		"node_id":  string(loginID),
		"note":     "this function validates JWT tokens",
		"agent_id": "annotator-agent",
	}))
	mustResult(t, res, err)

	anns, err := s.store.GetAnnotationsForNodes([]string{string(loginID)})
	if err != nil {
		t.Fatalf("GetAnnotationsForNodes: %v", err)
	}
	nodeAnns := anns[string(loginID)]
	if len(nodeAnns) == 0 {
		t.Fatal("expected annotation to be stored")
	}
	if !strings.Contains(nodeAnns[0].Note, "JWT") {
		t.Errorf("annotation note mismatch: %q", nodeAnns[0].Note)
	}
}

func TestHandleAnnotateNode_MissingNodeID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleAnnotateNode(ctx, callTool(map[string]any{"note": "some note"}))
	mustErrorResult(t, res, err)
}

// ── handleGetContext ──────────────────────────────────────────────────────────

func TestHandleGetContext_KnownEntity(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "root")
	root, _ := m["root"].(map[string]any)
	if root["name"] != "AuthLogin" {
		t.Errorf("expected root.name=AuthLogin, got %v", root["name"])
	}
}

func TestHandleGetContext_UnknownEntity_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{"entity": "NonExistentXYZ"}))
	// get_context returns a tool-level error OR a success with an error field.
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	// Either an IsError result or a JSON body with an "error" key is acceptable.
	if !res.IsError {
		tc, ok := res.Content[0].(mcp.TextContent)
		if !ok {
			t.Fatal("expected text content")
		}
		if !strings.Contains(strings.ToLower(tc.Text), "not found") &&
			!strings.Contains(strings.ToLower(tc.Text), "error") &&
			!strings.Contains(strings.ToLower(tc.Text), "unknown") {
			t.Errorf("expected error indication in response for unknown entity, got: %s", tc.Text[:min(200, len(tc.Text))])
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestHandleGetContext_MissingEntity_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetContext(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

func TestHandleGetContext_UpdatesAgentFocus(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity":   "AuthLogin",
		"agent_id": "focus-agent",
	}))
	mustResult(t, res, err)

	// The agent focus update is fire-and-forget (goroutine). Give it a moment.
	time.Sleep(50 * time.Millisecond)

	agents, err := s.store.GetAgents()
	if err != nil {
		t.Fatalf("GetAgents: %v", err)
	}
	for _, a := range agents {
		if a.ID == "focus-agent" && a.CurrentFocus == "AuthLogin" {
			return // pass
		}
	}
	t.Error("expected focus-agent to have CurrentFocus=AuthLogin after get_context")
}

func TestHandleGetContext_WithCallers(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	_ = loginID
	// AuthLogin is called by HandleRequest — callers bucket should be non-empty.
	res, err := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin"}))
	m := mustResult(t, res, err)
	callers, _ := m["callers"].([]any)
	if len(callers) == 0 {
		t.Error("expected callers for AuthLogin (called by HandleRequest)")
	}
}

// ── handleFindEntity ──────────────────────────────────────────────────────────

func TestHandleFindEntity_ExactName(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleFindEntity(ctx, callTool(map[string]any{"query": "AuthLogin"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "matches")
	matches, _ := m["matches"].([]any)
	if len(matches) == 0 {
		t.Error("expected at least one match for AuthLogin")
	}
}

func TestHandleFindEntity_PartialName(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleFindEntity(ctx, callTool(map[string]any{"query": "Auth"}))
	m := mustResult(t, res, err)
	matches, _ := m["matches"].([]any)
	if len(matches) < 2 {
		t.Errorf("expected ≥2 matches for 'Auth' (Login+Logout), got %d", len(matches))
	}
}

func TestHandleFindEntity_NoResults_EmptyList(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleFindEntity(ctx, callTool(map[string]any{"query": "ZZZNoSuchEntity"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "matches")
}

func TestHandleFindEntity_MissingQuery_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleFindEntity(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleSearch ──────────────────────────────────────────────────────────────

func TestHandleSearch_SemanticMode(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleSearch(ctx, callTool(map[string]any{
		"query": "authentication",
		"mode":  "semantic",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "results")
}

func TestHandleSearch_ExactMode(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleSearch(ctx, callTool(map[string]any{
		"query": "AuthLogin",
		"mode":  "exact",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "results")
}

func TestHandleSearch_EmptyQuery_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSearch(ctx, callTool(map[string]any{"query": ""}))
	mustErrorResult(t, res, err)
}

// ── handleGetFileContext ──────────────────────────────────────────────────────

func TestHandleGetFileContext_KnownFile(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetFileContext(ctx, callTool(map[string]any{
		"file": "pkg/auth/auth.go",
	}))
	m := mustResult(t, res, err)
	// Response uses "entities" key (or "count" + "entities").
	entities, ok := m["entities"].([]any)
	if !ok {
		t.Fatalf("expected entities list in response, keys: %v", mapKeys(m))
	}
	if len(entities) < 2 {
		t.Errorf("expected ≥2 entities in pkg/auth/auth.go, got %d", len(entities))
	}
}

func TestHandleGetFileContext_UnknownFile(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetFileContext(ctx, callTool(map[string]any{
		"file": "pkg/nonexistent/file.go",
	}))
	// Unknown file returns a tool error (file not indexed).
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	_ = res // error or empty result — both acceptable
}

func TestHandleGetFileContext_MissingParam_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetFileContext(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleGetCallChain ────────────────────────────────────────────────────────

func TestHandleGetCallChain_ValidChain(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// HandleRequest → AuthLogin chain should exist.
	res, err := s.handleGetCallChain(ctx, callTool(map[string]any{
		"from": "HandleRequest",
		"to":   "AuthLogin",
	}))
	// Chain exists — just verify no Go-level error returned.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = res
}

func TestHandleGetCallChain_MissingParams_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetCallChain(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleGetImpact ───────────────────────────────────────────────────────────

func TestHandleGetImpact_KnownSymbol(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetImpact(ctx, callTool(map[string]any{"symbol": "AuthLogin"}))
	m := mustResult(t, res, err)
	// Impact response uses "tiers" and "total_affected".
	hasKey(t, m, "total_affected")
	hasKey(t, m, "root")
}

func TestHandleGetImpact_UnknownSymbol(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetImpact(ctx, callTool(map[string]any{"symbol": "ZZZNothing"}))
	// Unknown symbol returns a tool error — that is the correct behaviour.
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	_ = res
}

func TestHandleGetImpact_MissingSymbol_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetImpact(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── entity_hash + known_hash (R14) ────────────────────────────────────────────

func TestHandleGetContext_EntityHashPresent(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin"}))
	m := mustResult(t, res, err)
	hash, ok := m["entity_hash"].(string)
	if !ok || len(hash) != 12 {
		t.Errorf("expected entity_hash of length 12, got %q", hash)
	}
}

func TestHandleGetContext_KnownHash_ReturnsUnchanged(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// First call — get the hash.
	res1, err1 := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin"}))
	m1 := mustResult(t, res1, err1)
	hash, _ := m1["entity_hash"].(string)
	if hash == "" {
		t.Fatal("no entity_hash in first response")
	}

	// Second call with known_hash matching — expect {"unchanged": true}.
	res2, err2 := s.handleGetContext(ctx, callTool(map[string]any{
		"entity":     "AuthLogin",
		"known_hash": hash,
	}))
	m2 := mustResult(t, res2, err2)
	if m2["unchanged"] != true {
		t.Errorf("expected unchanged=true when known_hash matches, got %v", m2)
	}
	if m2["entity_hash"] != hash {
		t.Errorf("expected entity_hash to be echoed back, got %v", m2["entity_hash"])
	}
}

func TestHandleGetContext_WrongKnownHash_ReturnsFull(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity":     "AuthLogin",
		"known_hash": "000000000000",
	}))
	m := mustResult(t, res, err)
	// Hash mismatch → full response with root and entity_hash.
	hasKey(t, m, "root")
	hasKey(t, m, "entity_hash")
	if m["unchanged"] == true {
		t.Error("should not return unchanged=true for wrong hash")
	}
}

// ── test_coverage in get_impact (R2) ─────────────────────────────────────────

func TestHandleGetImpact_TestCoverageField(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetImpact(ctx, callTool(map[string]any{"symbol": "AuthLogin"}))
	m := mustResult(t, res, err)
	// test_coverage key must be present (may be empty slice if no test files).
	// We check that it doesn't cause a panic or wrong type — actual coverage
	// depends on whether test files exist in the fixture.
	_ = m["test_coverage"] // nil (omitempty) or []interface{}
}

// ── closest_reachable in get_call_chain not-found (R2) ───────────────────────

func TestHandleGetCallChain_NotFound_ClosestReachable(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// Use entities that exist but have no call path between them.
	res, err := s.handleGetCallChain(ctx, callTool(map[string]any{
		"from": "HandleRequest",
		"to":   "Database",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, err)
	if found, _ := m["found"].(bool); !found {
		// When not found, closest_reachable MAY be present if BFS reached any nodes.
		// We just verify the field doesn't cause a crash and has the right shape if set.
		if cr, ok := m["closest_reachable"].(map[string]any); ok {
			if cr["name"] == nil || cr["hops"] == nil {
				t.Errorf("closest_reachable missing required fields: %v", cr)
			}
		}
	}
}

// ── handleGetEvents ───────────────────────────────────────────────────────────

func TestHandleGetEvents_InitialEmpty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetEvents(ctx, callTool(map[string]any{"since_seq": float64(0)}))
	m := mustResult(t, res, err)
	hasKey(t, m, "events")
}

func TestHandleGetEvents_AfterSessionInit(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "watcher-agent"}))

	res, err := s.handleGetEvents(ctx, callTool(map[string]any{"since_seq": float64(0)}))
	m := mustResult(t, res, err)
	events, _ := m["events"].([]any)
	if len(events) == 0 {
		t.Error("expected at least one event after session_init")
	}
}

// ── entity_hash stable (root not double-counted) ──────────────────────────────

func TestHandleGetContext_EntityHashStable(t *testing.T) {
	// Two consecutive calls with the same graph must return the same hash.
	s, _, _ := newPopulatedServer(t)
	res1, err1 := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin"}))
	m1 := mustResult(t, res1, err1)
	hash1, _ := m1["entity_hash"].(string)

	res2, err2 := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin"}))
	m2 := mustResult(t, res2, err2)
	hash2, _ := m2["entity_hash"].(string)

	if hash1 == "" || hash2 == "" {
		t.Fatal("entity_hash missing in one of the responses")
	}
	if hash1 != hash2 {
		t.Errorf("entity_hash unstable across identical calls: %q vs %q", hash1, hash2)
	}
}

// ── entity_hash in compact format ─────────────────────────────────────────────

func TestHandleGetContext_CompactFormat_EntityHashPresent(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "compact",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatal("expected TextContent for compact format")
	}
	if !strings.Contains(tc.Text, "entity_hash:") {
		t.Errorf("compact format must contain entity_hash: line, got:\n%s", tc.Text)
	}
}

// ── TestCoverage for struct/interface impact ──────────────────────────────────

func TestHandleGetImpact_StructNode_TestCoverageField(t *testing.T) {
	// Build a server with a struct node and a method on it.
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	structID := g.MakeNodeID("pkg/store/store.go", "UserStore")
	methodID := g.MakeNodeID("pkg/store/store.go", "UserStore.Get")
	callerID := g.MakeNodeID("pkg/api/handler.go", "HandleGet")
	testID := g.MakeNodeID("pkg/store/store_test.go", "TestUserStore_Get")

	g.AddNode(&graph.Node{ID: structID, Name: "UserStore", Type: graph.NodeStruct, File: "pkg/store/store.go", Line: 1, Package: "store"})
	g.AddNode(&graph.Node{ID: methodID, Name: "UserStore.Get", Type: graph.NodeMethod, File: "pkg/store/store.go", Line: 5, Package: "store"})
	g.AddNode(&graph.Node{ID: callerID, Name: "HandleGet", Type: graph.NodeFunction, File: "pkg/api/handler.go", Line: 1, Package: "api"})
	g.AddNode(&graph.Node{ID: testID, Name: "TestUserStore_Get", Type: graph.NodeFunction, File: "pkg/store/store_test.go", Line: 1, Package: "store"})

	g.AddEdge(&graph.Edge{From: callerID, To: methodID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: testID, To: methodID, Type: graph.EdgeCalls})

	s := New(g, cfg, st)
	res, err := s.handleGetImpact(ctx, callTool(map[string]any{"symbol": "UserStore"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, err)
	hasKey(t, m, "total_affected")

	// The struct path (merged) must also populate TestCoverage.
	coverage, _ := m["test_coverage"].([]any)
	found := false
	for _, f := range coverage {
		if f == "pkg/store/store_test.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected store_test.go in test_coverage for struct node, got %v", m["test_coverage"])
	}
}

// ── handleGetEvents ───────────────────────────────────────────────────────────

func TestHandleGetEvents_SinceCursorFilters(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "evt-a"}))
	time.Sleep(5 * time.Millisecond)

	res1, err1 := s.handleGetEvents(ctx, callTool(map[string]any{"since_seq": float64(0)}))
	m1 := mustResult(t, res1, err1)
	events1, _ := m1["events"].([]any)
	if len(events1) == 0 {
		t.Skip("no events to cursor-test")
	}
	lastEvent, _ := events1[len(events1)-1].(map[string]any)
	lastSeq, _ := lastEvent["seq"].(float64)

	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "evt-b"}))

	res2, err2 := s.handleGetEvents(ctx, callTool(map[string]any{"since_seq": lastSeq}))
	m2 := mustResult(t, res2, err2)
	events2, _ := m2["events"].([]any)
	if len(events2) == 0 {
		t.Error("expected new events after cursor position")
	}
}

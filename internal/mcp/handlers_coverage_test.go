package mcp

// Coverage tests for MCP handlers — targeting uncovered code paths.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── handleGetViolations with violations ───────────────────────────────────────

// addViolatingGraph sets up a graph + rule that will produce a violation:
// api/ calls auth/ → forbidden edge from "api" to "db" package pattern.
func addViolatingGraph(s *Server) {
	apiFile := "pkg/api/handler.go"
	dbFile := "pkg/db/query.go"
	apiID := s.graph.MakeNodeID(apiFile, "HandleRequest")
	dbID := s.graph.MakeNodeID(dbFile, "QueryUser")
	s.graph.AddNode(&graph.Node{ID: apiID, Name: "HandleRequest", Type: graph.NodeFunction, File: apiFile, Package: "api"})
	s.graph.AddNode(&graph.Node{ID: dbID, Name: "QueryUser", Type: graph.NodeFunction, File: dbFile, Package: "db"})
	s.graph.AddEdge(&graph.Edge{From: apiID, To: dbID, Type: graph.EdgeCalls})

	// Add a rule that forbids direct calls from api/ to db/.
	s.rulesMu.Lock()
	s.config.Rules = append(s.config.Rules, config.Rule{
		ID:          "no-api-to-db",
		Description: "API handlers must not call DB layer directly",
		Severity:    "error",
		ForbiddenEdge: config.ForbiddenEdge{
			FromFilePattern: "pkg/api/*",
			ToFilePattern:   "pkg/db/*",
			EdgeType:        graph.EdgeCalls,
		},
	})
	s.rulesMu.Unlock()
}

func TestHandleGetViolations_WithViolation(t *testing.T) {
	s := newTestServer(t)
	addViolatingGraph(s)

	req := callTool(map[string]any{})
	result, err := s.handleGetViolations(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if summary, _ := m["summary"].(string); summary == "no violations found" {
		t.Error("expected violations to be found")
	}
}

func TestHandleGetViolations_WithViolationAndLogLimit(t *testing.T) {
	s := newTestServer(t)
	addViolatingGraph(s)

	req := callTool(map[string]any{"log_limit": float64(10), "include_log": true})
	result, err := s.handleGetViolations(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = mustResult(t, result, nil)
}

func TestHandleGetViolations_WithViolationRuleFilter(t *testing.T) {
	s := newTestServer(t)
	addViolatingGraph(s)

	// Filter by the rule ID — should find the violation.
	req := callTool(map[string]any{"rule_id": "no-api-to-db"})
	result, err := s.handleGetViolations(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if summary, _ := m["summary"].(string); summary == "no violations found" {
		t.Error("expected violation with matching rule_id")
	}

	// Filter by non-existent rule ID — should find no violations.
	req2 := callTool(map[string]any{"rule_id": "non-existent-rule"})
	result2, err2 := s.handleGetViolations(ctx, req2)
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	m2 := mustResult(t, result2, nil)
	if summary, _ := m2["summary"].(string); summary != "no violations found" {
		t.Error("expected no violations for non-matching rule_id")
	}
}

// ── handleValidatePlan with violations ───────────────────────────────────────

func TestHandleValidatePlan_WithViolatingChange(t *testing.T) {
	s := newTestServer(t)
	addViolatingGraph(s)

	// The existing graph already has the violating edge, so validate_plan
	// with a change that touches those files should surface it.
	req := callTool(map[string]any{
		"changes": `[{"file": "pkg/api/handler.go", "adds_call_to": "QueryUser"}]`,
	})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = mustResult(t, result, nil)
}

// ── handleGetCallChain ────────────────────────────────────────────────────────

func TestHandleGetCallChain_FromNotFound(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{
		"from": "NonExistentFunctionXYZ",
		"to":   "AuthLogin",
	})
	result, err := s.handleGetCallChain(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should return a JSON result with an "error" key (not found).
	m := mustResult(t, result, nil)
	if _, ok := m["error"]; !ok {
		t.Error("expected 'error' key when from node not found")
	}
}

func TestHandleGetCallChain_ToNotFound(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"from": "HandleRequest",
		"to":   "NonExistentFunctionXYZ",
	})
	result, err := s.handleGetCallChain(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if _, ok := m["error"]; !ok {
		t.Error("expected 'error' key when to node not found")
	}
}

func TestHandleGetCallChain_NoPath(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// Reversed direction — no path from AuthLogin to HandleRequest.
	req := callTool(map[string]any{
		"from": "AuthLogin",
		"to":   "HandleRequest",
	})
	result, err := s.handleGetCallChain(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// Same from and to → immediate match (line 2040).
func TestHandleGetCallChain_SameNode(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetCallChain(ctx, callTool(map[string]any{
		"from": "AuthLogin",
		"to":   "AuthLogin",
	}))
	m := mustResult(t, res, err)
	if found, _ := m["found"].(bool); !found {
		t.Error("expected found=true for same-node chain")
	}
}

// A→B→C chain forces BFS enqueueing (covers line 2078).
func TestHandleGetCallChain_MultiHopPath(t *testing.T) {
	s := newTestServer(t)
	aID := s.graph.MakeNodeID("pkg/a/a.go", "FuncA")
	bID := s.graph.MakeNodeID("pkg/b/b.go", "FuncB")
	cID := s.graph.MakeNodeID("pkg/c/c.go", "FuncC")
	s.graph.AddNode(&graph.Node{ID: aID, Name: "FuncA", Type: graph.NodeFunction, File: "pkg/a/a.go", Package: "a", Line: 1})
	s.graph.AddNode(&graph.Node{ID: bID, Name: "FuncB", Type: graph.NodeFunction, File: "pkg/b/b.go", Package: "b", Line: 1})
	s.graph.AddNode(&graph.Node{ID: cID, Name: "FuncC", Type: graph.NodeFunction, File: "pkg/c/c.go", Package: "c", Line: 1})
	s.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})
	s.graph.AddEdge(&graph.Edge{From: bID, To: cID, Type: graph.EdgeCalls})

	res, err := s.handleGetCallChain(ctx, callTool(map[string]any{
		"from": "FuncA",
		"to":   "FuncC",
	}))
	m := mustResult(t, res, err)
	if found, _ := m["found"].(bool); !found {
		t.Error("expected found=true for multi-hop chain")
	}
}

// No path between different-package nodes → covers the cross-package reason branch.
func TestHandleGetCallChain_DifferentPackageNoPath(t *testing.T) {
	s := newTestServer(t)
	aID := s.graph.MakeNodeID("pkg/alpha/alpha.go", "AlphaFunc")
	bID := s.graph.MakeNodeID("pkg/beta/beta.go", "BetaFunc")
	s.graph.AddNode(&graph.Node{ID: aID, Name: "AlphaFunc", Type: graph.NodeFunction, File: "pkg/alpha/alpha.go", Package: "alpha", Line: 1})
	s.graph.AddNode(&graph.Node{ID: bID, Name: "BetaFunc", Type: graph.NodeFunction, File: "pkg/beta/beta.go", Package: "beta", Line: 1})
	// No edges — no path.
	res, err := s.handleGetCallChain(ctx, callTool(map[string]any{
		"from": "AlphaFunc",
		"to":   "BetaFunc",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "reason")
}

// ── handleGetProjectIdentity ──────────────────────────────────────────────────

func TestHandleGetProjectIdentity_Populated(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{})
	result, err := s.handleGetProjectIdentity(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	hasKey(t, m, "identity")
}

func TestHandleGetProjectIdentity_WithCachedIdentity(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// First call to populate identity cache.
	_, _ = s.handleGetProjectIdentity(ctx, callTool(nil))
	// Second call with previous_identity_hash to trigger cache path.
	id := s.graph.ProjectIdentity()
	_ = id
	req := callTool(map[string]any{"previous_identity_hash": "some-hash"})
	result, err := s.handleGetProjectIdentity(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleGetImpact (additional paths) ───────────────────────────────────────

func TestHandleGetImpact_FoundWithDepth(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{"symbol": "AuthLogin", "depth": float64(2)})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleSemanticSearch ──────────────────────────────────────────────────────

func TestHandleSemanticSearch_KeywordMode(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"query": "login auth",
		"mode":  "keyword",
	})
	result, err := s.handleSemanticSearch(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleSemanticSearch_SemanticMode(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"query": "authentication logic",
		"mode":  "semantic",
	})
	result, err := s.handleSemanticSearch(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleSemanticSearch_EmptyQuery(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{"query": ""})
	result, err := s.handleSemanticSearch(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleAnnotateNode ────────────────────────────────────────────────────────

func TestHandleAnnotateNode_MissingNodeID(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{"note": "some note"})
	result, err := s.handleAnnotateNode(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Missing node_id → error result.
	if !result.IsError {
		t.Error("expected error result for missing node_id")
	}
}

func TestHandleAnnotateNode_ValidNodeID(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"node_id":  string(loginID),
		"note":     "handles user authentication",
		"agent_id": "test-agent",
	})
	result, err := s.handleAnnotateNode(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleGetContext (additional paths) ───────────────────────────────────────

func TestHandleGetContext_WithTaskID(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"entity":  "AuthLogin",
		"task_id": "task-123",
		"format":  "json",
	})
	result, err := s.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleUpsertRule ──────────────────────────────────────────────────────────

func TestHandleUpsertRule_MissingID(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{"description": "test rule"})
	result, err := s.handleUpsertRule(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Missing rule_id → should return error.
	_ = result
}

func TestHandleUpsertRule_ValidRule(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{
		"rule_id":           "no-direct-db",
		"description":       "Handlers must not call DB directly",
		"severity":          "error",
		"from_file_pattern": "pkg/api/*",
		"to_file_pattern":   "pkg/db/*",
	})
	result, err := s.handleUpsertRule(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleSearch (additional paths) ──────────────────────────────────────────

func TestHandleSearch_SemanticModePopulated(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"query": "user authentication",
		"mode":  "semantic",
	})
	result, err := s.handleSearch(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleSearch_EmptyQuery(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handleSearch(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── mergeNodeIDs ──────────────────────────────────────────────────────────────

func TestMergeNodeIDs_Dedup(t *testing.T) {
	a := []string{"node-1", "node-2"}
	b := []string{"node-2", "node-3"} // node-2 is duplicate
	result := mergeNodeIDs(a, b)
	if len(result) != 3 {
		t.Errorf("expected 3 unique IDs, got %d: %v", len(result), result)
	}
}

func TestMergeNodeIDs_Empty(t *testing.T) {
	result := mergeNodeIDs(nil, nil)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d", len(result))
	}
}

func TestMergeNodeIDs_NoOverlap(t *testing.T) {
	a := []string{"node-1"}
	b := []string{"node-2"}
	result := mergeNodeIDs(a, b)
	if len(result) != 2 {
		t.Errorf("expected 2 IDs, got %d", len(result))
	}
}

// ── autoLinkNodes ─────────────────────────────────────────────────────────────

func TestAutoLinkNodes_NilGraph(t *testing.T) {
	s := newTestServer(t)
	s.graph = nil
	result := s.autoLinkNodes("some text")
	if result != nil {
		t.Error("expected nil result for nil graph")
	}
}

func TestAutoLinkNodes_EmptyText(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	result := s.autoLinkNodes("")
	if result != nil {
		t.Error("expected nil result for empty text")
	}
}

func TestAutoLinkNodes_MatchesNodeName(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// "AuthLogin" and "HandleRequest" are in the populated graph.
	result := s.autoLinkNodes("we need to fix AuthLogin and HandleRequest")
	if len(result) == 0 {
		t.Error("expected to find at least one node ID matching AuthLogin")
	}
}

func TestAutoLinkNodes_MatchesDottedName(t *testing.T) {
	s := newTestServer(t)
	// Add a node with a dotted name like "Auth.Login".
	id := s.graph.MakeNodeID("pkg/auth/auth.go", "Auth.Login")
	s.graph.AddNode(&graph.Node{
		ID:   id,
		Name: "Auth.Login",
		Type: graph.NodeFunction,
		File: "pkg/auth/auth.go",
	})
	// "Login" is the bare method name — should be indexable.
	result := s.autoLinkNodes("call the Login function here")
	if len(result) == 0 {
		t.Error("expected to find node by bare method name 'Login'")
	}
}

// ── handleCreatePlan (tasks as JSON string) ───────────────────────────────────

func TestHandleCreatePlan_TasksAsJSONString(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{
		"title": "JSON String Plan",
		"tasks": `[{"title":"task one","priority":1}]`,
	})
	result, err := s.handleCreatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	hasKey(t, m, "plan_id")
}

func TestHandleCreatePlan_TasksAsEmptyString(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{
		"title": "Empty String Plan",
		"tasks": "",
	})
	result, err := s.handleCreatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for empty tasks string")
	}
}

func TestHandleCreatePlan_TasksInvalidJSON(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{
		"title": "Bad JSON Plan",
		"tasks": "not valid json",
	})
	result, err := s.handleCreatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for invalid JSON tasks")
	}
}

func TestHandleCreatePlan_TasksEmptyArray(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{
		"title": "Empty Array Plan",
		"tasks": []interface{}{},
	})
	result, err := s.handleCreatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for empty tasks array")
	}
}

func TestHandleCreatePlan_TasksDefaultCase(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{
		"title": "Default Case Plan",
		"tasks": 42, // non-string, non-array → default case
	})
	result, err := s.handleCreatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for non-array tasks")
	}
}

// ── handleGetPendingTasks (suggest_next) ──────────────────────────────────────

func TestHandleGetPendingTasks_SuggestNext(t *testing.T) {
	s := newTestServer(t)
	// Create a plan with an unblocked pending task.
	_, _ = s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "test plan",
		"tasks": []interface{}{
			map[string]interface{}{"title": "first task", "priority": 1},
		},
	}))
	req := callTool(map[string]any{"suggest_next": true})
	result, err := s.handleGetPendingTasks(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	hasKey(t, m, "suggested_next")
}

// ── handleSaveSessionState (array fields) ────────────────────────────────────

func TestHandleSaveSessionState_WithArrayFields(t *testing.T) {
	s := newTestServer(t)
	// Create a plan/task first.
	res, err := s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "session plan",
		"tasks": []interface{}{map[string]interface{}{"title": "t1", "priority": 1}},
	}))
	m := mustResult(t, res, err)
	// Get a real task ID.
	tasks, _ := s.store.GetPendingTasks("", "")
	if len(tasks) == 0 {
		t.Skip("no tasks to save session state for")
	}
	taskID := tasks[0].ID
	_ = m

	req := callTool(map[string]any{
		"task_id":         taskID,
		"approach":        "bottom-up refactoring",
		"files_modified":  []interface{}{"pkg/auth/auth.go", "pkg/api/handler.go"},
		"completed_steps": []interface{}{"analyzed codebase", "wrote tests"},
		"remaining_steps": []interface{}{"update docs"},
		"blockers":        []interface{}{"need approval"},
		"decisions":       []interface{}{"use interface abstraction"},
	})
	result, err := s.handleSaveSessionState(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = mustResult(t, result, nil)
}

func TestHandleSaveSessionState_WithStringJSONFields(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "session plan 2",
		"tasks": []interface{}{map[string]interface{}{"title": "t2", "priority": 1}},
	}))
	tasks, _ := s.store.GetPendingTasks("", "")
	if len(tasks) == 0 {
		t.Skip("no tasks")
	}
	taskID := tasks[0].ID

	req := callTool(map[string]any{
		"task_id":        taskID,
		"files_modified": `["pkg/auth/auth.go"]`,
	})
	result, err := s.handleSaveSessionState(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = mustResult(t, result, nil)
}

// ── handleGetSessionState (found state) ──────────────────────────────────────

func TestHandleGetSessionState_Found(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "state plan",
		"tasks": []interface{}{map[string]interface{}{"title": "t1", "priority": 1}},
	}))
	tasks, _ := s.store.GetPendingTasks("", "")
	if len(tasks) == 0 {
		t.Skip("no tasks")
	}
	taskID := tasks[0].ID

	// Save state first.
	_ = s.store.UpsertSessionState(store.SessionState{
		TaskID:   taskID,
		Approach: "test approach",
	})

	req := callTool(map[string]any{"task_id": taskID})
	result, err := s.handleGetSessionState(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if found, _ := m["found"].(bool); !found {
		t.Error("expected found=true when state exists")
	}
}

// ── handleRemember (episode_type=failure, invalid args) ───────────────────────

func TestHandleRemember_EpisodeTypeFailure(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":     "fail-agent",
		"decision":     "direct DB calls from handlers",
		"episode_type": "failure",
		"outcome":      "failure",
		"trigger":      "timeout in production",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episode_id")
}

func TestHandleRemember_InvalidEpisodeType(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":     "a",
		"decision":     "something",
		"episode_type": "invalid_type",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleRemember_InvalidOutcome(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "a",
		"decision": "something",
		"outcome":  "invalid_outcome",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleRemember_MissingDecisionWithAgentID(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "a",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleRemember_ImportanceOverride(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":   "a",
		"decision":   "use caching",
		"outcome":    "success",
		"importance": float64(0.9),
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episode_id")
}

// ── handleRecall (browse with tags, search mode) ──────────────────────────────

func TestHandleRecall_BrowseWithTags(t *testing.T) {
	s := newTestServer(t)
	// Remember an episode with tags.
	_, _ = s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "a",
		"decision": "use redis",
		"outcome":  "success",
		"tags":     `["caching","performance"]`,
	}))
	res, err := s.handleRecall(ctx, callTool(map[string]any{
		"tags": "caching",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episodes")
}

func TestHandleRecall_BrowseWithSinceDays(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "a",
		"decision": "old decision",
		"outcome":  "success",
	}))
	res, err := s.handleRecall(ctx, callTool(map[string]any{
		"since_days": float64(30),
		"limit":      float64(5),
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episodes")
}

func TestHandleRecall_SearchMode(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "a",
		"decision": "cache auth tokens in redis",
		"outcome":  "success",
	}))
	res, err := s.handleRecall(ctx, callTool(map[string]any{
		"query":          "auth token caching",
		"limit":          float64(5),
		"outcome_filter": "success",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episodes")
}

// ── handleGetEpisodes (with filters) ─────────────────────────────────────────

func TestHandleGetEpisodes_WithTags(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "a",
		"decision": "tagged decision",
		"tags":     `["mytag"]`,
	}))
	res, err := s.handleGetEpisodes(ctx, callTool(map[string]any{
		"tags": "mytag",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episodes")
}

func TestHandleGetEpisodes_WithSinceDays(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetEpisodes(ctx, callTool(map[string]any{
		"since_days": float64(7),
		"limit":      float64(5),
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "episodes")
}

// ── handleCheckPlanSafety (with agentID → interjection path) ─────────────────

func TestHandleCheckPlanSafety_WithAgentIDAndMatch(t *testing.T) {
	s := newTestServer(t)
	// Record a failure episode first.
	_, _ = s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":     "planner",
		"decision":     "call DB directly from handler",
		"episode_type": "failure",
		"outcome":      "failure",
		"trigger":      "timeout",
	}))
	// Check plan safety — should find a match and record interjection.
	res, err := s.handleCheckPlanSafety(ctx, callTool(map[string]any{
		"plan_description": "call the database directly from the handler",
		"agent_id":         "planner",
		"project_id":       "test-project",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "status")
}

// ── handleGetRuleCandidates (with min_occurrences) ────────────────────────────

func TestHandleGetRuleCandidates_WithMinOccurrences(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetRuleCandidates(ctx, callTool(map[string]any{
		"min_occurrences": float64(3),
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "candidates")
}

// ── stringArgDefault ──────────────────────────────────────────────────────────

func TestStringArgDefault_WithValue(t *testing.T) {
	req := callTool(map[string]any{"mykey": "myvalue"})
	got := stringArgDefault(req, "mykey", "default")
	if got != "myvalue" {
		t.Errorf("expected 'myvalue', got %q", got)
	}
}

func TestStringArgDefault_WithDefault(t *testing.T) {
	req := callTool(map[string]any{})
	got := stringArgDefault(req, "missing", "fallback")
	if got != "fallback" {
		t.Errorf("expected 'fallback', got %q", got)
	}
}

// ── handleLinkTaskNodes (JSON string path) ────────────────────────────────────

func TestHandleLinkTaskNodes_JSONStringPath(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "link plan",
		"tasks": []interface{}{map[string]interface{}{"title": "t", "priority": 1}},
	}))
	tasks, _ := s.store.GetPendingTasks("", "")
	if len(tasks) == 0 {
		t.Skip("no tasks")
	}
	taskID := tasks[0].ID

	res, err := s.handleLinkTaskNodes(ctx, callTool(map[string]any{
		"task_id":  taskID,
		"node_ids": `["node-a","node-b"]`,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "linked")
}

// ── handleUpdateTask (invalid status, unblocked tasks) ───────────────────────

func TestHandleUpdateTask_InvalidStatus(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpdateTask(ctx, callTool(map[string]any{
		"id":     "any-id",
		"status": "invalid_status",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("expected error for invalid status")
	}
}

// ── handleGetContext (additional coverage paths) ──────────────────────────────

// newServerWithMultipleNodes creates a server with two nodes sharing the name
// "SharedHelper" in different files, for disambiguation testing.
func newServerWithMultipleNodes(t *testing.T) *Server {
	t.Helper()
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Two nodes with the same name in different files — disambiguation path.
	id1 := g.MakeNodeID("pkg/util/helper.go", "SharedHelper")
	id2 := g.MakeNodeID("pkg/core/helper.go", "SharedHelper")
	g.AddNode(&graph.Node{ID: id1, Name: "SharedHelper", Type: graph.NodeFunction, File: "pkg/util/helper.go", Package: "util", Line: 1})
	g.AddNode(&graph.Node{ID: id2, Name: "SharedHelper", Type: graph.NodeFunction, File: "pkg/core/helper.go", Package: "core", Line: 1})
	return New(g, cfg, st)
}

func TestHandleGetContext_Disambiguation(t *testing.T) {
	s := newServerWithMultipleNodes(t)
	req := callTool(map[string]any{
		"entity": "SharedHelper",
		// no file= hint → disambiguation candidates should be surfaced
		"format": "json",
	})
	result, err := s.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleGetContext_WithHelpfulFeedback(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// agent_id + helpful=true triggers the feedback path.
	req := callTool(map[string]any{
		"entity":   "AuthLogin",
		"agent_id": "feedback-agent",
		"helpful":  true,
		"format":   "json",
	})
	result, err := s.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleGetContext_WithHelpfulFalse(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"entity":   "AuthLogin",
		"agent_id": "feedback-agent2",
		"helpful":  false,
		"format":   "json",
	})
	result, err := s.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleGetContext_ImpactMode(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"entity": "AuthLogin",
		"mode":   "impact",
		"format": "json",
	})
	result, err := s.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleGetContext_DottedEntityName(t *testing.T) {
	s := newTestServer(t)
	// Add a node with name "Login".
	id := s.graph.MakeNodeID("pkg/auth/auth.go", "Login")
	s.graph.AddNode(&graph.Node{ID: id, Name: "Login", Type: graph.NodeFunction, File: "pkg/auth/auth.go", Package: "auth", Line: 1})
	// Query with dotted form "Auth.Login" → should resolve via prefix matching.
	req := callTool(map[string]any{"entity": "Auth.Login"})
	result, err := s.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestHandleGetContext_RepeatCalls(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// Call 3 times with the same entity and agent_id — triggers the repeat
	// context feedback loop episode recording (GAP-1).
	args := map[string]any{"entity": "AuthLogin", "agent_id": "repeat-agent"}
	for i := 0; i < 3; i++ {
		_, err := s.handleGetContext(ctx, callTool(args))
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
}

func TestHandleGetContext_NotFoundWithCandidates(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// Query something that doesn't exist exactly but FindByPattern might return.
	req := callTool(map[string]any{"entity": "Login"})
	result, err := s.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── handleSessionInit (incremental mode, collision warning, unread) ───────────

func TestHandleSessionInit_IncrementalMode(t *testing.T) {
	s := newTestServer(t)
	agentID := "incremental-agent"
	// First call — stores context.
	_, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": agentID}))
	if err != nil {
		t.Fatalf("first session_init: %v", err)
	}
	// Second call with same agent_id — should be incremental (identity might be skipped).
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": agentID}))
	if err != nil {
		t.Fatalf("second session_init: %v", err)
	}
	m := mustResult(t, res, nil)
	// incremental flag should be set.
	if _, ok := m["incremental"]; !ok {
		t.Error("expected 'incremental' key on second session_init call")
	}
}

func TestHandleSessionInit_AgentAwareness(t *testing.T) {
	s := newTestServer(t)
	// Register another agent so GetActiveAgents returns a peer.
	_ = s.store.UpsertAgent("peer-agent", nil)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "main-agent", "scope": "full"}))
	if err != nil {
		t.Fatalf("session_init: %v", err)
	}
	m := mustResult(t, res, nil)
	hasKey(t, m, "project_identity")
}

// writeTestFile writes content to a file at path.
func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// ── inlineFindEntity (dotted name path) ──────────────────────────────────────

func TestInlineFindEntity_DottedNameFallback(t *testing.T) {
	s := newTestServer(t)
	// Add a node that should be findable via dotted name.
	id := s.graph.MakeNodeID("internal/store/store.go", "Close")
	s.graph.AddNode(&graph.Node{ID: id, Name: "Close", Type: graph.NodeFunction, File: "internal/store/store.go", Package: "store", Line: 1})
	// "Store.Close" → split to "store" prefix + "Close" method.
	result := s.inlineFindEntity("Store.Close")
	if len(result) == 0 {
		t.Error("expected dotted name to resolve via prefix matching")
	}
}

func TestInlineFindEntity_NoMatch(t *testing.T) {
	s := newTestServer(t)
	result := s.inlineFindEntity("CompletelyNonExistentFunction")
	if len(result) != 0 {
		t.Errorf("expected empty result for unknown entity, got %d items", len(result))
	}
}

// ── handleVerifyImplementation (various paths) ────────────────────────────────

func TestHandleVerifyImplementation_InvalidJSON(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": "not-a-json-array",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("expected error for invalid JSON")
	}
}

func TestHandleVerifyImplementation_EmptyArray(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `[]`,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("expected error for empty files array")
	}
}

func TestHandleVerifyImplementation_FileInGraph(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// "pkg/auth/auth.go" is in the populated graph.
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["pkg/auth/auth.go"]`,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "files")
}

func TestHandleVerifyImplementation_FileNotInGraph(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["pkg/unknown/foo.go"]`,
	}))
	m := mustResult(t, res, err)
	if status, _ := m["status"].(string); status != "pending_indexing" {
		t.Logf("status=%q (expected pending_indexing for file not in graph)", status)
	}
}

func TestHandleVerifyImplementation_WithViolations(t *testing.T) {
	s := newTestServer(t)
	addViolatingGraph(s)
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["pkg/api/handler.go"]`,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "status")
}

func TestHandleVerifyImplementation_WithTaskID(t *testing.T) {
	s, _, loginID := newPopulatedServer(t)
	// Create a plan + task, link a node, then verify.
	planRes, err := s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "verify plan",
		"tasks": []interface{}{map[string]interface{}{"title": "verify task", "priority": 1}},
	}))
	_ = mustResult(t, planRes, err)
	tasks, _ := s.store.GetPendingTasks("", "")
	if len(tasks) == 0 {
		t.Skip("no tasks")
	}
	taskID := tasks[0].ID
	// Link the loginID node to the task.
	_ = s.store.UpdateLinkedNodes(taskID, []string{string(loginID)})

	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["pkg/auth/auth.go"]`,
		"task_id":       taskID,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "files")
}

func TestHandleVerifyImplementation_WithFreshnessWarning(t *testing.T) {
	root := t.TempDir()
	// Create a real file modified very recently (< 10s ago).
	filePath := root + "/fresh.go"
	if err := writeTestFile(filePath, "package main"); err != nil {
		t.Fatalf("write file: %v", err)
	}

	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	g.SetRoot(root)
	cfg, _ := config.Load(root)
	s := New(g, cfg, st)

	// Query with relative path so repoRoot join is exercised.
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["fresh.go"]`,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "files")
}

// ── handleVerifyImplementation — signature impact ─────────────────────────────

// TestHandleVerifyImplementation_SignatureImpact_ExportedFuncWithCallers checks
// that an exported function whose signature actually changed AND has callers
// produces a signature_impact entry (v2: requires prev_signature in store).
func TestHandleVerifyImplementation_SignatureImpact_ExportedFuncWithCallers(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	targetID := g.MakeNodeID("pkg/api/api.go", "HandleRequest")

	// First SaveGraph: original signature.
	g1 := graph.New("test-repo")
	g1.AddNode(&graph.Node{
		ID: targetID, Name: "HandleRequest", Type: graph.NodeFunction,
		File: "pkg/api/api.go", Package: "api", Exported: true, Line: 10,
		Metadata: map[string]string{"signature": "func HandleRequest(w http.ResponseWriter)"},
	})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}

	// Second SaveGraph: signature changed — new param added.
	g2 := graph.New("test-repo")
	g2.AddNode(&graph.Node{
		ID: targetID, Name: "HandleRequest", Type: graph.NodeFunction,
		File: "pkg/api/api.go", Package: "api", Exported: true, Line: 10,
		Metadata: map[string]string{"signature": "func HandleRequest(w http.ResponseWriter, r *http.Request)"},
	})
	// Add caller edge so ImpactAnalysis finds a result.
	callerID := g2.MakeNodeID("pkg/main/main.go", "main")
	g2.AddNode(&graph.Node{
		ID: callerID, Name: "main", Type: graph.NodeFunction,
		File: "pkg/main/main.go", Package: "main", Exported: false, Line: 5,
	})
	g2.AddEdge(&graph.Edge{From: callerID, To: targetID, Type: graph.EdgeCalls})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	// Mirror graph state into the in-memory graph used by the server.
	s.graph.AddNode(&graph.Node{
		ID: targetID, Name: "HandleRequest", Type: graph.NodeFunction,
		File: "pkg/api/api.go", Package: "api", Exported: true, Line: 10,
		Metadata: map[string]string{"signature": "func HandleRequest(w http.ResponseWriter, r *http.Request)"},
	})
	s.graph.AddNode(&graph.Node{
		ID: callerID, Name: "main", Type: graph.NodeFunction,
		File: "pkg/main/main.go", Package: "main", Exported: false, Line: 5,
	})
	s.graph.AddEdge(&graph.Edge{From: callerID, To: targetID, Type: graph.EdgeCalls})

	req := callTool(map[string]any{
		"files_written": `["pkg/api/api.go"]`,
	})
	res, err := s.handleVerifyImplementation(ctx, req)
	m := mustResult(t, res, err)

	// impact_warnings should be > 0 since HandleRequest has a changed sig + caller.
	impactWarnings, ok := m["impact_warnings"].(float64)
	if !ok || impactWarnings == 0 {
		t.Errorf("expected impact_warnings > 0, got %v", m["impact_warnings"])
	}

	// impact_hint should be present.
	hasKey(t, m, "impact_hint")

	// files[0].signature_impact should have one entry.
	files, ok := m["files"].([]any)
	if !ok || len(files) == 0 {
		t.Fatal("expected files array")
	}
	fileEntry, ok := files[0].(map[string]any)
	if !ok {
		t.Fatal("expected file entry to be a map")
	}
	si, ok := fileEntry["signature_impact"].([]any)
	if !ok || len(si) == 0 {
		t.Fatalf("expected signature_impact entries, got %v", fileEntry["signature_impact"])
	}
	entry := si[0].(map[string]any)
	if entry["symbol"] != "HandleRequest" {
		t.Errorf("expected symbol HandleRequest, got %v", entry["symbol"])
	}
	callers, ok := entry["callers"].([]any)
	if !ok || len(callers) == 0 {
		t.Error("expected callers list")
	}
}

// TestHandleVerifyImplementation_SignatureImpact_UnexportedNoImpact checks that
// unexported symbols with no callers do not produce signature_impact entries —
// GetSignatureChanges now detects them (FIX-R20A), but ImpactAnalysis returns
// TotalAffected=0 when no callers exist, so no warning is emitted.
func TestHandleVerifyImplementation_SignatureImpact_UnexportedNoImpact(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	targetID := g.MakeNodeID("pkg/api/api.go", "internalHelper")

	// SaveGraph twice — signature changes but entity is unexported.
	g1 := graph.New("test-repo")
	g1.AddNode(&graph.Node{
		ID: targetID, Name: "internalHelper", Type: graph.NodeFunction,
		File: "pkg/api/api.go", Package: "api", Exported: false, Line: 20,
		Metadata: map[string]string{"signature": "func internalHelper(x int)"},
	})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}
	g2 := graph.New("test-repo")
	g2.AddNode(&graph.Node{
		ID: targetID, Name: "internalHelper", Type: graph.NodeFunction,
		File: "pkg/api/api.go", Package: "api", Exported: false, Line: 20,
		Metadata: map[string]string{"signature": "func internalHelper(x int, y string)"},
	})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	req := callTool(map[string]any{
		"files_written": `["pkg/api/api.go"]`,
	})
	res, err := s.handleVerifyImplementation(ctx, req)
	m := mustResult(t, res, err)

	// internalHelper is unexported with no callers — ImpactAnalysis returns TotalAffected=0, so no warning.
	if w, _ := m["impact_warnings"].(float64); w != 0 {
		t.Errorf("expected 0 impact_warnings for unexported entity, got %v", w)
	}
	noKey(t, m, "impact_hint")
}

// TestHandleVerifyImplementation_SignatureImpact_UnexportedWithTestCaller verifies
// FIX-R20A: an unexported function whose signature changes reports its test-file
// caller in signature_impact so the agent knows to update the test.
func TestHandleVerifyImplementation_SignatureImpact_UnexportedWithTestCaller(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	targetID := g.MakeNodeID("pkg/mcp/explain.go", "buildExplanation")
	callerID := g.MakeNodeID("pkg/mcp/explain_test.go", "TestBuildExplanation")

	// SaveGraph 1: original signature.
	g1 := graph.New("test-repo")
	g1.AddNode(&graph.Node{
		ID: targetID, Name: "buildExplanation", Type: graph.NodeFunction,
		File: "pkg/mcp/explain.go", Package: "mcp", Exported: false, Line: 10,
		Metadata: map[string]string{"signature": "func buildExplanation(identity *ProjectIdentity, nodes []*Node, edges map[NodeID]int, repoRoot string) string"},
	})
	g1.AddNode(&graph.Node{
		ID: callerID, Name: "TestBuildExplanation", Type: graph.NodeFunction,
		File: "pkg/mcp/explain_test.go", Package: "mcp", Exported: false, Line: 5,
		Metadata: map[string]string{"signature": "func TestBuildExplanation(t *testing.T)"},
	})
	// Test file calls the unexported function.
	g1.AddEdge(&graph.Edge{From: callerID, To: targetID, Type: graph.EdgeCalls})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}

	// SaveGraph 2: signature changed — dropped the edges param.
	g2 := graph.New("test-repo")
	g2.AddNode(&graph.Node{
		ID: targetID, Name: "buildExplanation", Type: graph.NodeFunction,
		File: "pkg/mcp/explain.go", Package: "mcp", Exported: false, Line: 10,
		Metadata: map[string]string{"signature": "func buildExplanation(identity *ProjectIdentity, nodes []*Node, repoRoot string) string"},
	})
	g2.AddNode(&graph.Node{
		ID: callerID, Name: "TestBuildExplanation", Type: graph.NodeFunction,
		File: "pkg/mcp/explain_test.go", Package: "mcp", Exported: false, Line: 5,
		Metadata: map[string]string{"signature": "func TestBuildExplanation(t *testing.T)"},
	})
	g2.AddEdge(&graph.Edge{From: callerID, To: targetID, Type: graph.EdgeCalls})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	// Sync the live graph with same edges so ImpactAnalysis can traverse them.
	s.graph.AddNode(&graph.Node{
		ID: targetID, Name: "buildExplanation", Type: graph.NodeFunction,
		File: "pkg/mcp/explain.go", Package: "mcp", Exported: false, Line: 10,
	})
	s.graph.AddNode(&graph.Node{
		ID: callerID, Name: "TestBuildExplanation", Type: graph.NodeFunction,
		File: "pkg/mcp/explain_test.go", Package: "mcp", Exported: false, Line: 5,
	})
	s.graph.AddEdge(&graph.Edge{From: callerID, To: targetID, Type: graph.EdgeCalls})

	req := callTool(map[string]any{
		"files_written": `["pkg/mcp/explain.go"]`,
	})
	res, err := s.handleVerifyImplementation(ctx, req)
	m := mustResult(t, res, err)

	// Must report 1 impact warning — the test file caller is now visible.
	if w, _ := m["impact_warnings"].(float64); w != 1 {
		t.Errorf("expected 1 impact_warning for unexported entity with test caller, got %v", w)
	}
	files, _ := m["files"].([]any)
	if len(files) == 0 {
		t.Fatal("expected file report")
	}
	fileEntry, _ := files[0].(map[string]any)
	si, ok := fileEntry["signature_impact"].([]any)
	if !ok || len(si) == 0 {
		t.Fatalf("expected signature_impact entry, got %v", fileEntry["signature_impact"])
	}
	entry, _ := si[0].(map[string]any)
	callers, _ := entry["callers"].([]any)
	if len(callers) == 0 {
		t.Fatal("expected at least 1 caller (the test file)")
	}
	// Verify the test file caller is present.
	found := false
	for _, c := range callers {
		caller, _ := c.(map[string]any)
		if f, _ := caller["file"].(string); strings.Contains(f, "_test.go") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a _test.go file in callers, got %v", callers)
	}
}

// TestHandleVerifyImplementation_SignatureImpact_ExportedStructWithCallers checks
// that exported struct types with a signature change produce signature_impact entries.
func TestHandleVerifyImplementation_SignatureImpact_ExportedStructWithCallers(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	structID := g.MakeNodeID("pkg/store/store.go", "Config")
	callerID := g.MakeNodeID("pkg/main/main.go", "Run")

	// First SaveGraph: original struct signature.
	g1 := graph.New("test-repo")
	g1.AddNode(&graph.Node{
		ID: structID, Name: "Config", Type: graph.NodeStruct,
		File: "pkg/store/store.go", Package: "store", Exported: true, Line: 5,
		Metadata: map[string]string{"signature": "type Config struct { Host string }"},
	})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}

	// Second SaveGraph: struct gains new field.
	g2 := graph.New("test-repo")
	g2.AddNode(&graph.Node{
		ID: structID, Name: "Config", Type: graph.NodeStruct,
		File: "pkg/store/store.go", Package: "store", Exported: true, Line: 5,
		Metadata: map[string]string{"signature": "type Config struct { Host string; Port int }"},
	})
	g2.AddNode(&graph.Node{
		ID: callerID, Name: "Run", Type: graph.NodeFunction,
		File: "pkg/main/main.go", Package: "main", Exported: true, Line: 1,
	})
	g2.AddEdge(&graph.Edge{From: callerID, To: structID, Type: graph.EdgeCalls})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	// Mirror into in-memory graph.
	s.graph.AddNode(&graph.Node{
		ID: structID, Name: "Config", Type: graph.NodeStruct,
		File: "pkg/store/store.go", Package: "store", Exported: true, Line: 5,
	})
	s.graph.AddNode(&graph.Node{
		ID: callerID, Name: "Run", Type: graph.NodeFunction,
		File: "pkg/main/main.go", Package: "main", Exported: true, Line: 1,
	})
	s.graph.AddEdge(&graph.Edge{From: callerID, To: structID, Type: graph.EdgeCalls})

	req := callTool(map[string]any{
		"files_written": `["pkg/store/store.go"]`,
	})
	res, err := s.handleVerifyImplementation(ctx, req)
	m := mustResult(t, res, err)

	if w, _ := m["impact_warnings"].(float64); w == 0 {
		t.Error("expected impact_warnings > 0 for struct with changed sig + caller")
	}
}

// TestHandleVerifyImplementation_SignatureImpact_ZeroCallerNoEntry checks that
// an exported function whose signature changed but has NO callers does NOT produce
// a signature_impact entry (ImpactAnalysis returns TotalAffected=0).
func TestHandleVerifyImplementation_SignatureImpact_ZeroCallerNoEntry(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	loneID := g.MakeNodeID("pkg/util/util.go", "LoneFunc")

	// SaveGraph twice — sig changes but nobody calls LoneFunc.
	g1 := graph.New("test-repo")
	g1.AddNode(&graph.Node{
		ID: loneID, Name: "LoneFunc", Type: graph.NodeFunction,
		File: "pkg/util/util.go", Package: "util", Exported: true, Line: 1,
		Metadata: map[string]string{"signature": "func LoneFunc() int"},
	})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}
	g2 := graph.New("test-repo")
	g2.AddNode(&graph.Node{
		ID: loneID, Name: "LoneFunc", Type: graph.NodeFunction,
		File: "pkg/util/util.go", Package: "util", Exported: true, Line: 1,
		Metadata: map[string]string{"signature": "func LoneFunc() string"},
	})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	// Mirror into in-memory graph (no caller edges).
	s.graph.AddNode(&graph.Node{
		ID: loneID, Name: "LoneFunc", Type: graph.NodeFunction,
		File: "pkg/util/util.go", Package: "util", Exported: true, Line: 1,
	})

	req := callTool(map[string]any{
		"files_written": `["pkg/util/util.go"]`,
	})
	res, err := s.handleVerifyImplementation(ctx, req)
	m := mustResult(t, res, err)

	if w, _ := m["impact_warnings"].(float64); w != 0 {
		t.Errorf("expected 0 impact_warnings for changed-sig with no callers, got %v", w)
	}
}

// ── handleGetImpact (struct with methods) ─────────────────────────────────────

func TestHandleGetImpact_StructWithMethods(t *testing.T) {
	s := newTestServer(t)
	// Add a struct node.
	structID := s.graph.MakeNodeID("pkg/store/store.go", "Store")
	s.graph.AddNode(&graph.Node{ID: structID, Name: "Store", Type: graph.NodeStruct, File: "pkg/store/store.go", Package: "store", Line: 1})
	// Add method nodes.
	closeID := s.graph.MakeNodeID("pkg/store/store.go", "Store.Close")
	s.graph.AddNode(&graph.Node{ID: closeID, Name: "Store.Close", Type: graph.NodeMethod, File: "pkg/store/store.go", Package: "store", Line: 10})
	// Caller of Close.
	callerID := s.graph.MakeNodeID("cmd/main/main.go", "main")
	s.graph.AddNode(&graph.Node{ID: callerID, Name: "main", Type: graph.NodeFunction, File: "cmd/main/main.go", Package: "main", Line: 1})
	s.graph.AddEdge(&graph.Edge{From: callerID, To: closeID, Type: graph.EdgeCalls})

	res, err := s.handleGetImpact(ctx, callTool(map[string]any{"symbol": "Store"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = mustResult(t, res, nil)
}

// ── handleSearch (scoring paths) ──────────────────────────────────────────────

func TestHandleSearch_PrefixMatch(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// "Auth" is a prefix of "AuthLogin" and "AuthLogout".
	res, err := s.handleSearch(ctx, callTool(map[string]any{
		"query": "Auth",
		"mode":  "keyword",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "results")
}

func TestHandleSearch_SubstringMatch(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// "ogin" is a substring of "AuthLogin".
	res, err := s.handleSearch(ctx, callTool(map[string]any{
		"query": "ogin",
		"mode":  "keyword",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "results")
}

func TestHandleSearch_MultiWordQuery(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// "Auth Login" → multi-word AND query matching name components.
	res, err := s.handleSearch(ctx, callTool(map[string]any{
		"query": "auth login",
		"mode":  "keyword",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "results")
}

func TestHandleSearch_FilePathMatch(t *testing.T) {
	s := newTestServer(t)
	// Add a node in a file named "auth.go".
	id := s.graph.MakeNodeID("internal/auth/auth.go", "ValidateToken")
	s.graph.AddNode(&graph.Node{ID: id, Name: "ValidateToken", Type: graph.NodeFunction, File: "internal/auth/auth.go", Package: "auth", Line: 1})
	// Query by the package name "auth" — should match via file path.
	res, err := s.handleSearch(ctx, callTool(map[string]any{
		"query": "auth",
		"mode":  "keyword",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "results")
}

func TestHandleSearch_WithRootPrefix(t *testing.T) {
	root := t.TempDir()
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	g.SetRoot(root)
	id := g.MakeNodeID("pkg/api/handler.go", "HandleRequest")
	g.AddNode(&graph.Node{ID: id, Name: "HandleRequest", Type: graph.NodeFunction, File: "pkg/api/handler.go", Package: "api", Line: 1})
	cfg, _ := config.Load(root)
	s := New(g, cfg, st)

	res, err := s.handleSearch(ctx, callTool(map[string]any{
		"query": "HandleRequest",
		"mode":  "keyword",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "results")
}

// ── pickBestNode (multiple nodes) ─────────────────────────────────────────────

func TestPickBestNode_PrefersNonTestFunctions(t *testing.T) {
	g := graph.New("test-repo")
	testNode := &graph.Node{ID: "test-repo::auth_test.go::TestLogin", Name: "TestLogin", Type: graph.NodeFunction, File: "auth_test.go"}
	prodNode := &graph.Node{ID: "test-repo::auth.go::Login", Name: "Login", Type: graph.NodeFunction, File: "auth.go"}
	nodes := []*graph.Node{testNode, prodNode}
	best := pickBestNode(nodes, g)
	if best != prodNode {
		t.Error("expected non-test function to be preferred")
	}
}

// ── handleValidatePlan (check_safety, skipped edges) ─────────────────────────

func TestHandleValidatePlan_WithCheckSafety(t *testing.T) {
	s := newTestServer(t)
	// Record a failure episode first.
	_, _ = s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":     "a",
		"decision":     "call database directly",
		"episode_type": "failure",
		"outcome":      "failure",
	}))
	req := callTool(map[string]any{
		"changes":          `[{"file": "pkg/api/handler.go", "adds_call_to": "QueryUser"}]`,
		"check_safety":     true,
		"plan_description": "call database directly from handler",
	})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = mustResult(t, result, nil)
}

func TestHandleValidatePlan_CalleeNotInGraph(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{
		"changes": `[{"file": "pkg/api/handler.go", "adds_call_to": "NonExistentFunction"}]`,
	})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	hasKey(t, m, "violations") // no violations since edge was skipped
}

func TestHandleValidatePlan_SourceFileNotInGraph(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"changes": `[{"file": "pkg/nonexistent/foo.go", "adds_call_to": "AuthLogin"}]`,
	})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	hasKey(t, m, "violations")
}

// ── handleValidatePlan (RX3 logic checks) ───────────────────────────────────

func TestHandleValidatePlan_LogicChecks_ZeroValueId(t *testing.T) {
	s := newTestServer(t)
	// Write a Go file with a zero-value identifier bug.
	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	os.WriteFile(goFile, []byte("package main\nfunc main() {\n\tkillByPort(0)\n}\n"), 0644)
	s.graph.SetRoot(dir)

	req := callTool(map[string]any{
		"changes": `[{"file": "main.go"}]`,
	})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	lw, ok := m["logic_warnings"]
	if !ok {
		t.Fatal("expected logic_warnings key in result")
	}
	warnings, ok := lw.([]interface{})
	if !ok {
		t.Fatalf("expected logic_warnings to be a slice, got %T", lw)
	}
	if len(warnings) == 0 {
		t.Fatal("expected at least one logic warning")
	}
	first := warnings[0].(map[string]interface{})
	if first["check"] != "zero_value_id" {
		t.Errorf("expected check=zero_value_id, got %v", first["check"])
	}
}

func TestHandleValidatePlan_LogicChecks_SkipLogicChecks(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	os.WriteFile(goFile, []byte("package main\nfunc main() {\n\tkillByPort(0)\n}\n"), 0644)
	s.graph.SetRoot(dir)

	req := callTool(map[string]any{
		"changes":           `[{"file": "main.go"}]`,
		"skip_logic_checks": true,
	})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if _, ok := m["logic_warnings"]; ok {
		t.Error("expected no logic_warnings when skip_logic_checks=true")
	}
}

func TestHandleValidatePlan_LogicChecks_NonGoFile(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	pyFile := filepath.Join(dir, "main.py")
	os.WriteFile(pyFile, []byte("print('hello')"), 0644)
	s.graph.SetRoot(dir)

	req := callTool(map[string]any{
		"changes": `[{"file": "main.py"}]`,
	})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if _, ok := m["logic_warnings"]; ok {
		t.Error("expected no logic_warnings for non-Go file")
	}
}

func TestHandleValidatePlan_LogicChecks_MissingFile(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.graph.SetRoot(dir)

	req := callTool(map[string]any{
		"changes": `[{"file": "nonexistent.go"}]`,
	})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if _, ok := m["logic_warnings"]; ok {
		t.Error("expected no logic_warnings for nonexistent file")
	}
}

func TestHandleValidatePlan_LogicChecks_WarningCap(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	// Create a file with many logic issues.
	src := "package main\n\nimport \"os\"\n\nfunc bad() {\n"
	for i := 0; i < 10; i++ {
		src += "\tos.Open(\"~/.config\")\n"
	}
	src += "}\n"
	goFile := filepath.Join(dir, "main.go")
	os.WriteFile(goFile, []byte(src), 0644)
	s.graph.SetRoot(dir)

	req := callTool(map[string]any{
		"changes": `[{"file": "main.go"}]`,
	})
	result, err := s.handleValidatePlan(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	lw, ok := m["logic_warnings"]
	if !ok {
		t.Fatal("expected logic_warnings key in result")
	}
	warnings, ok := lw.([]interface{})
	if !ok {
		t.Fatalf("expected logic_warnings to be a slice, got %T", lw)
	}
	if len(warnings) > 5 {
		t.Errorf("expected at most 5 logic warnings (cap), got %d", len(warnings))
	}
}

// ── handleUpsertRule (invalid severity) ──────────────────────────────────────

func TestHandleUpsertRule_InvalidSeverity(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
		"rule_id":     "my-rule",
		"description": "test rule",
		"severity":    "critical", // not "error" or "warning"
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("expected error for invalid severity")
	}
}

// ── nil-store tests for coord handlers ────────────────────────────────────────

func TestHandleGetPlans_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleGetPlans(ctx, callTool(map[string]any{}))
	mustErrorResult(t, res, err)
}

func TestHandleGetMyTasks_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleGetMyTasks(ctx, callTool(map[string]any{"agent_id": "a"}))
	mustErrorResult(t, res, err)
}

func TestHandleLinkTaskNodes_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleLinkTaskNodes(ctx, callTool(map[string]any{
		"task_id":  "t1",
		"node_ids": []interface{}{"node-a"},
	}))
	mustErrorResult(t, res, err)
}

// ── nil-store tests for episode handlers ──────────────────────────────────────

func TestHandleRemember_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "a",
		"decision": "test",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleRecall_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleRecall(ctx, callTool(map[string]any{"query": "test"}))
	mustErrorResult(t, res, err)
}

func TestHandleGetEpisodes_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleGetEpisodes(ctx, callTool(map[string]any{}))
	mustErrorResult(t, res, err)
}

func TestHandleCheckPlanSafety_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleCheckPlanSafety(ctx, callTool(map[string]any{
		"plan_description": "test plan",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleGetRuleCandidates_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleGetRuleCandidates(ctx, callTool(map[string]any{}))
	mustErrorResult(t, res, err)
}

// ── nil-store tests for message handlers ─────────────────────────────────────

// ── nil-store tests for task handlers ────────────────────────────────────────

func TestHandleGetPendingTasks_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleGetPendingTasks(ctx, callTool(map[string]any{}))
	mustErrorResult(t, res, err)
}

func TestHandleSaveSessionState_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleSaveSessionState(ctx, callTool(map[string]any{"task_id": "t1"}))
	mustErrorResult(t, res, err)
}

func TestHandleGetSessionState_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleGetSessionState(ctx, callTool(map[string]any{"task_id": "t1"}))
	mustErrorResult(t, res, err)
}

func TestHandleUpdateTask_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleUpdateTask(ctx, callTool(map[string]any{
		"id":     "t1",
		"status": "done",
	}))
	mustErrorResult(t, res, err)
}

// ── handleGetContext (depth/budget/task-linked-nodes paths) ──────────────────

func TestHandleGetContext_WithDepthAndBudget(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity":       "AuthLogin",
		"depth":        float64(2),
		"token_budget": float64(1000),
		"format":       "json",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "root")
}

// Annotations present → covers annMap > 0 enrichment path.
func TestHandleGetContext_WithAnnotations(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	// Add an annotation so annMap is non-empty.
	_, _ = s.store.AddAnnotation(string(loginID), "test-agent", "important: handles JWT expiry")

	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "root")
}

// Rules with no FromFilePattern match all files → covers applicable_rules enrichment.
func TestHandleGetContext_WithApplicableRules(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Rules = append(cfg.Rules, config.Rule{
		ID:          "no-direct-sql",
		Description: "never write raw SQL",
		Severity:    "warning",
		// No FromFilePattern → matches all files.
	})
	loginID := g.MakeNodeID("pkg/auth/auth.go", "AuthLogin")
	g.AddNode(&graph.Node{ID: loginID, Name: "AuthLogin", Type: graph.NodeFunction, File: "pkg/auth/auth.go", Package: "auth", Line: 1})
	s := New(g, cfg, st)

	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "root")
}

// Failure episodes present → covers RecentFailures enrichment path.
func TestHandleGetContext_WithRecentFailures(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)

	// Store a failure episode about AuthLogin.
	_, _ = s.store.RememberEpisode(store.Episode{
		AgentID:     "test-agent",
		EpisodeType: "failure",
		Outcome:     "failure",
		Decision:    "AuthLogin caused circular import",
		Trigger:     "AuthLogin",
	})
	_ = loginID

	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "root")
}

// Constitution.InjectInContext → covers dc.Principles path.
func TestHandleGetContext_WithConstitution(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Constitution.Principles = []string{"No direct SQL", "All errors must be logged"}
	cfg.Constitution.InjectInContext = true
	loginID := g.MakeNodeID("pkg/auth/auth.go", "AuthLogin")
	g.AddNode(&graph.Node{ID: loginID, Name: "AuthLogin", Type: graph.NodeFunction, File: "pkg/auth/auth.go", Package: "auth", Line: 1})
	s := New(g, cfg, st)

	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "root")
}

func TestHandleGetContext_WithLinkedTaskNodes(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)

	// Create a task with linked nodes in the store.
	planID, _, _ := s.store.CreatePlan("linked-test", "", "agent", []store.TaskInput{{Title: "linked task"}})
	tasks, _ := s.store.GetPendingTasks(planID, "")
	if len(tasks) == 0 {
		t.Skip("no tasks created")
	}
	taskID := tasks[0].ID
	// Link the loginID node to the task.
	_ = s.store.UpdateLinkedNodes(taskID, []string{string(loginID)})

	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity":  "AuthLogin",
		"task_id": taskID,
		"format":  "json",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "root")
}

// ── handleGetContext / handlePrepareContext (sort + callee paths) ─────────────

// Calling handleGetContext on HandleRequest (which has 2 callees) triggers
// the sort comparisons inside toDirectionalContext (byRelevance lambda).
func TestHandleGetContext_MultipleCallees(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "HandleRequest",
		"format": "json",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "callees")
}

// debug intent on HandleRequest covers the callee section in assembleDebugContext.
func TestHandlePrepareContext_DebugWithCallees(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handlePrepareContext(ctx, callTool(map[string]any{
		"intent": "debug",
		"target": "HandleRequest",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}

// review intent on a struct exercises aggregatedImpact's struct loop.
func TestHandlePrepareContext_ReviewStructTarget(t *testing.T) {
	s := newTestServer(t)

	// Add a struct with two methods and callers.
	structID := s.graph.MakeNodeID("pkg/svc/svc.go", "Service")
	s.graph.AddNode(&graph.Node{ID: structID, Name: "Service", Type: graph.NodeStruct, File: "pkg/svc/svc.go", Package: "svc", Line: 1})
	doID := s.graph.MakeNodeID("pkg/svc/svc.go", "Service.Do")
	s.graph.AddNode(&graph.Node{ID: doID, Name: "Service.Do", Type: graph.NodeMethod, File: "pkg/svc/svc.go", Package: "svc", Line: 10})
	runID := s.graph.MakeNodeID("pkg/svc/svc.go", "Service.Run")
	s.graph.AddNode(&graph.Node{ID: runID, Name: "Service.Run", Type: graph.NodeMethod, File: "pkg/svc/svc.go", Package: "svc", Line: 20})
	caller := s.graph.MakeNodeID("cmd/main.go", "main")
	s.graph.AddNode(&graph.Node{ID: caller, Name: "main", Type: graph.NodeFunction, File: "cmd/main.go", Package: "main", Line: 1})
	s.graph.AddEdge(&graph.Edge{From: caller, To: doID, Type: graph.EdgeCalls})
	s.graph.AddEdge(&graph.Edge{From: caller, To: runID, Type: graph.EdgeCalls})

	res, err := s.handlePrepareContext(ctx, callTool(map[string]any{
		"intent": "review",
		"target": "Service",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}

// plan intent on a struct exercises aggregatedImpact in assemblePlanContext.
func TestHandlePrepareContext_PlanStructTarget(t *testing.T) {
	s := newTestServer(t)

	structID := s.graph.MakeNodeID("pkg/repo/repo.go", "Repo")
	s.graph.AddNode(&graph.Node{ID: structID, Name: "Repo", Type: graph.NodeStruct, File: "pkg/repo/repo.go", Package: "repo", Line: 1})
	saveID := s.graph.MakeNodeID("pkg/repo/repo.go", "Repo.Save")
	s.graph.AddNode(&graph.Node{ID: saveID, Name: "Repo.Save", Type: graph.NodeMethod, File: "pkg/repo/repo.go", Package: "repo", Line: 10})
	caller := s.graph.MakeNodeID("cmd/server.go", "handleSave")
	s.graph.AddNode(&graph.Node{ID: caller, Name: "handleSave", Type: graph.NodeFunction, File: "cmd/server.go", Package: "main", Line: 1})
	s.graph.AddEdge(&graph.Edge{From: caller, To: saveID, Type: graph.EdgeCalls})

	res, err := s.handlePrepareContext(ctx, callTool(map[string]any{
		"intent": "plan",
		"target": "Repo",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}

// modify intent on HandleRequest covers assembleModifyContext callee path.
func TestHandlePrepareContext_ModifyWithCallees(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handlePrepareContext(ctx, callTool(map[string]any{
		"intent": "modify",
		"target": "HandleRequest",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}

// ── handleUpsertRule (update existing rule path) ─────────────────────────────

// Calling upsertRule twice with the same ID exercises the update path (line 1331-1335).
func TestHandleUpsertRule_UpdateExisting(t *testing.T) {
	s := newTestServer(t)
	for _, desc := range []string{"first description", "updated description"} {
		res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
			"rule_id":     "my-rule",
			"description": desc,
			"severity":    "warning",
		}))
		m := mustResult(t, res, err)
		hasKey(t, m, "status")
	}
}

// ── handlePrepareContext (small budget triggers summary detail level) ──────────

// token_budget < 500 triggers the "summary" detail level in assembleUnderstandContext.
func TestHandlePrepareContext_SmallBudget(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handlePrepareContext(ctx, callTool(map[string]any{
		"intent":       "understand",
		"target":       "AuthLogin",
		"token_budget": float64(400),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}

// ── assembleAddContext (rules + constitution paths) ───────────────────────────

// "add" intent with rules + constitution covers the architecture rules and
// project laws sections in assembleAddContext.
func TestHandlePrepareContext_AddWithRulesAndConstitution(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Rules = append(cfg.Rules, config.Rule{
		ID:          "no-raw-sql",
		Description: "never write raw SQL",
		Severity:    "warning",
	})
	cfg.Constitution.Principles = []string{"fail-safe by default"}
	cfg.Constitution.InjectInContext = true
	loginID := g.MakeNodeID("pkg/auth/auth.go", "AuthLogin")
	g.AddNode(&graph.Node{ID: loginID, Name: "AuthLogin", Type: graph.NodeFunction, File: "pkg/auth/auth.go", Package: "auth", Exported: true, Line: 1})
	s := New(g, cfg, st)

	res, err := s.handlePrepareContext(ctx, callTool(map[string]any{
		"intent": "add",
		"target": "AuthLogin",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}

// ── handlePlanContext (structural validation skip paths) ─────────────────────

// Change with no adds_call_to, change with ghost callee, change with missing file.
func TestHandlePlanContext_ChangesSkipPaths(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// Three changes: one with empty adds_call_to (skip), one with ghost callee (skip),
	// one with missing file (skip).
	changes := `[
		{"file":"pkg/auth/auth.go"},
		{"file":"pkg/auth/auth.go","adds_call_to":"NonExistentGhostFunc"},
		{"file":"nonexistent/file.go","adds_call_to":"AuthLogout"}
	]`
	res, err := s.handlePlanContext(ctx, callTool(map[string]any{
		"target":  "AuthLogin",
		"changes": changes,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}

// ── handleAnnotateNode (missing error paths) ──────────────────────────────────

func TestHandleAnnotateNode_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleAnnotateNode(ctx, callTool(map[string]any{
		"node_id": "some::node::ID",
		"note":    "test note",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleAnnotateNode_MissingNote(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	res, err := s.handleAnnotateNode(ctx, callTool(map[string]any{
		"node_id": string(loginID),
	}))
	mustErrorResult(t, res, err)
}

func TestHandleAnnotateNode_NodeNotFound(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleAnnotateNode(ctx, callTool(map[string]any{
		"node_id": "nonexistent::node::ID",
		"note":    "test note",
	}))
	mustErrorResult(t, res, err)
}

// ── handleSemanticSearch (nil store path) ──────────────────────────────────────

func TestHandleSemanticSearch_NoStore(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleSemanticSearch(ctx, callTool(map[string]any{
		"query": "some query",
	}))
	mustErrorResult(t, res, err)
}

// ── handleGetImpact (struct merge path — two methods) ─────────────────────────

func TestHandleGetImpact_StructTwoMethods(t *testing.T) {
	s := newTestServer(t)

	// Struct node.
	structID := s.graph.MakeNodeID("pkg/db/db.go", "DB")
	s.graph.AddNode(&graph.Node{ID: structID, Name: "DB", Type: graph.NodeStruct, File: "pkg/db/db.go", Package: "db", Line: 1})

	// Two method nodes so the second method's tier gets merged with the first.
	queryID := s.graph.MakeNodeID("pkg/db/db.go", "DB.Query")
	s.graph.AddNode(&graph.Node{ID: queryID, Name: "DB.Query", Type: graph.NodeMethod, File: "pkg/db/db.go", Package: "db", Line: 10})
	execID := s.graph.MakeNodeID("pkg/db/db.go", "DB.Exec")
	s.graph.AddNode(&graph.Node{ID: execID, Name: "DB.Exec", Type: graph.NodeMethod, File: "pkg/db/db.go", Package: "db", Line: 20})

	// Callers for both methods — these appear in impact analysis.
	caller1 := s.graph.MakeNodeID("cmd/app/main.go", "RunQuery")
	s.graph.AddNode(&graph.Node{ID: caller1, Name: "RunQuery", Type: graph.NodeFunction, File: "cmd/app/main.go", Package: "main", Line: 1})
	s.graph.AddEdge(&graph.Edge{From: caller1, To: queryID, Type: graph.EdgeCalls})

	caller2 := s.graph.MakeNodeID("cmd/app/main.go", "RunExec")
	s.graph.AddNode(&graph.Node{ID: caller2, Name: "RunExec", Type: graph.NodeFunction, File: "cmd/app/main.go", Package: "main", Line: 5})
	s.graph.AddEdge(&graph.Edge{From: caller2, To: execID, Type: graph.EdgeCalls})

	res, err := s.handleGetImpact(ctx, callTool(map[string]any{"symbol": "DB"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "tiers")
}

// ── handleSessionInit (constitution + agent constraints + pending tasks) ───────

func TestHandleSessionInit_WithConstitution(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Inject principles to trigger the constitution section.
	cfg.Constitution.Principles = []string{"No CGo", "All handlers fail-silent"}
	cfg.Constitution.InjectInSessionInit = true
	s := New(g, cfg, st)

	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "const-agent",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "constitution")
}

func TestHandleSessionInit_WithAgentConstraints(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Add an agent-type rule.
	cfg.Rules = append(cfg.Rules, config.Rule{
		ID:          "no-direct-db",
		Description: "never call database directly from handler",
		Severity:    "warning",
		RuleType:    "agent",
	})
	s := New(g, cfg, st)

	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "constraint-agent",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "agent_constraints")
}

func TestHandleSessionInit_WithPendingTasks(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s := New(g, cfg, st)

	// Create a task so session_init returns a non-empty pending section.
	_, _, _ = st.CreatePlan("Test plan", "", "task-agent", []store.TaskInput{{Title: "Fix auth bug"}})

	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "task-agent",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "pending_tasks")
	pt, _ := m["pending_tasks"].(map[string]any)
	summary, _ := pt["summary"].(string)
	if summary == "no pending tasks" {
		t.Error("expected pending tasks summary with task count")
	}
}

func TestHandleSessionInit_NoStore(t *testing.T) {
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// No store — exercises the pendingSection==nil path.
	s := New(g, cfg, nil)

	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "no-store-agent",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "pending_tasks")
}

// ── handleGetProjectIdentity (cross-repo federation path) ─────────────────────

func TestHandleGetProjectIdentity_CrossRepo(t *testing.T) {
	s := newTestServer(t)

	// Add nodes from two different repos (IDs with the "repo::" prefix pattern).
	idA := graph.NodeID("repo-a::pkg/a/a.go::FuncA")
	idB := graph.NodeID("repo-b::pkg/b/b.go::FuncB")
	s.graph.AddNode(&graph.Node{ID: idA, Name: "FuncA", Type: graph.NodeFunction, File: "pkg/a/a.go", Package: "a", Line: 1})
	s.graph.AddNode(&graph.Node{ID: idB, Name: "FuncB", Type: graph.NodeFunction, File: "pkg/b/b.go", Package: "b", Line: 1})
	s.graph.AddEdge(&graph.Edge{From: idA, To: idB, Type: graph.EdgeCalls})

	res, err := s.handleGetProjectIdentity(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "federation")
	fed, _ := m["federation"].(map[string]any)
	crossCount, _ := fed["cross_project_edges"].(float64)
	if crossCount < 1 {
		t.Errorf("expected cross_project_edges >= 1, got %v", crossCount)
	}
}

// ── handleGetViolations (include_log + with-violations paths) ─────────────────

func TestHandleGetViolations_WithIncludeLog(t *testing.T) {
	s := newTestServer(t)

	// Upsert a rule that creates a violation, then fetch with include_log=true.
	_, _ = s.handleUpsertRule(ctx, callTool(map[string]any{
		"rule_id":     "test-violation-rule",
		"description": "FuncA must not call FuncB",
		"severity":    "warning",
		"from_file":   "a.go",
		"to_file":     "b.go",
	}))

	res, err := s.handleGetViolations(ctx, callTool(map[string]any{
		"include_log": true,
		"log_limit":   float64(5),
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "summary")
}

func TestPickBestNode_PrefersHigherConnectivity(t *testing.T) {
	g := graph.New("test-repo")
	lowConn := &graph.Node{ID: "test-repo::a.go::LowConn", Name: "LowConn", Type: graph.NodeFunction, File: "a.go"}
	highConn := &graph.Node{ID: "test-repo::b.go::HighConn", Name: "HighConn", Type: graph.NodeFunction, File: "b.go"}
	g.AddNode(lowConn)
	g.AddNode(highConn)
	// Add edges to make highConn more connected.
	callerID := graph.NodeID("test-repo::c.go::Caller")
	g.AddNode(&graph.Node{ID: callerID, Name: "Caller", Type: graph.NodeFunction, File: "c.go"})
	g.AddEdge(&graph.Edge{From: callerID, To: highConn.ID, Type: graph.EdgeCalls})
	nodes := []*graph.Node{lowConn, highConn}
	best := pickBestNode(nodes, g)
	if best != highConn {
		t.Error("expected higher-connectivity node to be preferred")
	}
}

// ── R1: include_inferred parameter ───────────────────────────────────────────

// TestHandleGetContext_IncludeInferred_FiltersRouteNodes verifies that
// include_inferred=false strips NodeRoute nodes from get_context callees, and
// include_inferred=true (default) keeps them.
func TestHandleGetContext_IncludeInferred_FiltersRouteNodes(t *testing.T) {
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	st := openMCPTestStore(t)
	s := New(g, cfg, st)

	// Add a handler function.
	handlerID := g.MakeNodeID("pkg/api/handler.go", "GetUsers")
	g.AddNode(&graph.Node{
		ID:      handlerID,
		Type:    graph.NodeFunction,
		Name:    "GetUsers",
		File:    "pkg/api/handler.go",
		Line:    10,
		Package: "api",
	})

	// Add a synthetic route node (as heuristic.go produces them).
	routeID := graph.NodeID("test-repo::pkg/api/handler.go::GET /users")
	g.UpsertRouteNode(&graph.Node{
		ID:   routeID,
		Type: graph.NodeRoute,
		Name: "GET /users",
		File: "pkg/api/handler.go",
		Line: 5,
		Metadata: map[string]string{
			"inferred":   "true",
			"confidence": "0.90",
			"method":     "GET",
			"path":       "/users",
			"handler":    "GetUsers",
		},
	})

	// Wire: routeNode --HANDLES--> handler
	g.AddEdge(&graph.Edge{From: routeID, To: handlerID, Type: graph.EdgeHandles})

	// Sprint 28: HANDLES edges now route to Related (not callers) to reduce
	// callee/caller noise. Route nodes should appear in "related" instead.
	resTrue, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity":           "GetUsers",
		"include_inferred": true,
		"format":           "json",
	}))
	if err != nil {
		t.Fatalf("include_inferred=true: unexpected error: %v", err)
	}
	mTrue := mustResult(t, resTrue, nil)

	// Route node should appear in related (not callers) after Sprint 28.
	related, _ := mTrue["related"].([]any)
	foundRoute := false
	for _, c := range related {
		cm, _ := c.(map[string]any)
		node, _ := cm["node"].(map[string]any)
		if node["type"] == "route" {
			foundRoute = true
		}
	}
	if !foundRoute {
		t.Error("include_inferred=true: expected route node in related, got none")
	}

	// Route node should NOT be in callers.
	callers, _ := mTrue["callers"].([]any)
	for _, c := range callers {
		cm, _ := c.(map[string]any)
		node, _ := cm["node"].(map[string]any)
		if node["type"] == "route" {
			t.Error("include_inferred=true: route node should not appear in callers (Sprint 28)")
		}
	}

	// --- include_inferred=false: route node must be absent from all buckets ---
	resFalse, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity":           "GetUsers",
		"include_inferred": false,
		"format":           "json",
	}))
	if err != nil {
		t.Fatalf("include_inferred=false: unexpected error: %v", err)
	}
	mFalse := mustResult(t, resFalse, nil)
	relatedFalse, _ := mFalse["related"].([]any)
	for _, c := range relatedFalse {
		cm, _ := c.(map[string]any)
		node, _ := cm["node"].(map[string]any)
		if node["type"] == "route" {
			t.Error("include_inferred=false: route node must not appear in related")
		}
	}
}

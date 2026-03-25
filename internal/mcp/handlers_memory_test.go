package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestSessionInit_SurfacesProjectMemories verifies that session_init includes
// project-tier memories in the relevant_memories section.
func TestSessionInit_SurfacesProjectMemories(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// Insert a project memory.
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "The auth module was refactored to use JWT tokens last week.",
		AgentID: "agent-1",
		Source:  store.SourceManual,
		Tags:    `["project"]`,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	// Call session_init.
	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-1",
		"scope":    "full",
	}))
	m := mustResult(t, result, err)

	// Verify relevant_memories is present and contains our memory.
	hasKey(t, m, "relevant_memories")
	memSection, ok := m["relevant_memories"].(map[string]any)
	if !ok {
		t.Fatalf("relevant_memories is not a map: %T", m["relevant_memories"])
	}
	mems, ok := memSection["memories"].([]any)
	if !ok || len(mems) == 0 {
		t.Fatal("expected at least 1 memory in relevant_memories")
	}
	first := mems[0].(map[string]any)
	if first["tier"] != "project" {
		t.Errorf("expected tier=project, got %v", first["tier"])
	}
	if first["content"] == "" {
		t.Error("expected non-empty content")
	}
}

// TestSessionInit_NoMemories_OmitsSection verifies that when no memories exist,
// the relevant_memories key is omitted (zero token cost).
func TestSessionInit_NoMemories_OmitsSection(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-2",
	}))
	m := mustResult(t, result, err)

	noKey(t, m, "relevant_memories")
}

// TestSessionInit_SurfacesSessionHistory verifies that session logs from prior
// sessions appear in relevant_memories.
func TestSessionInit_SurfacesSessionHistory(t *testing.T) {
	srv := newTestServer(t)

	// Insert a session-log memory from a prior session.
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierSessionLog,
		Content: "Session by agent-3 worked on auth refactoring, touched 5 files.",
		AgentID: "agent-3",
		Source:  store.SourceAuto,
		Tags:    `["session_end","auto"]`,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-3",
		"scope":    "full",
	}))
	m := mustResult(t, result, err)

	hasKey(t, m, "relevant_memories")
	memSection := m["relevant_memories"].(map[string]any)
	mems := memSection["memories"].([]any)

	found := false
	for _, mem := range mems {
		entry := mem.(map[string]any)
		if entry["label"] == "session_history" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a session_history label in relevant_memories")
	}
}

// TestGetContext_SurfacesEntityMemories verifies that get_context includes
// entity memories in the entity_memories field.
func TestGetContext_SurfacesEntityMemories(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	// Insert an entity-tier memory for AuthLogin.
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:     store.TierEntity,
		Content:  "AuthLogin was refactored to fix a deadlock in concurrent access.",
		EntityID: string(loginID),
		AgentID:  "agent-4",
		Source:   store.SourceAuto,
		Tags:     `["session_end","entity_change"]`,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	result, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	m := mustResult(t, result, err)

	// The entity_memories field should be present.
	hasKey(t, m, "entity_memories")
	mems, ok := m["entity_memories"].([]any)
	if !ok || len(mems) == 0 {
		t.Fatal("expected at least 1 entity memory")
	}
	first := mems[0].(map[string]any)
	content, _ := first["content"].(string)
	if content == "" {
		t.Error("expected non-empty content in entity memory")
	}
}

// TestGetContext_NoEntityMemories_OmitsField verifies that when no entity
// memories exist, the entity_memories field is omitted.
func TestGetContext_NoEntityMemories(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	result, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	m := mustResult(t, result, err)

	noKey(t, m, "entity_memories")
}

// TestEndSession_CreatesMemories verifies that end_session creates session-log
// and project-tier memories.
func TestEndSession_CreatesMemories(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleEndSession(ctx, callTool(map[string]any{
		"agent_id": "agent-5",
		"summary":  "Refactored the auth module to use JWT tokens for better security.",
	}))
	m := mustResult(t, result, err)

	saved, ok := m["memories_saved"].(float64)
	if !ok || saved < 2 {
		t.Errorf("expected at least 2 memories saved, got %v", m["memories_saved"])
	}

	// Verify memories exist in the store.
	projMems, _ := srv.store.QueryMemories(store.TierProject, "", "", 10)
	if len(projMems) == 0 {
		t.Error("expected project-tier memories after end_session")
	}

	sessMems, _ := srv.store.QueryRecentSessionMemories("agent-5", 10)
	if len(sessMems) == 0 {
		t.Error("expected session-log memories after end_session")
	}
}

// TestRemember_DualWritesToMemories verifies that remember() writes to both
// the episodes table and the unified memories table.
func TestRemember_DualWritesToMemories(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id":  "agent-6",
		"decision":  "Switched from bcrypt to argon2id for password hashing due to timing attacks.",
		"rationale": "bcrypt has known timing side-channels in some implementations.",
	}))
	m := mustResult(t, result, err)

	// Episode should exist.
	if m["episode_id"] == nil {
		t.Error("expected episode_id in result")
	}

	// Project-tier memory should also exist.
	projMems, _ := srv.store.QueryMemories(store.TierProject, "", "", 10)
	found := false
	for _, mem := range projMems {
		if mem.AgentID == "agent-6" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected project-tier memory from remember() dual-write")
	}
}

// TestAnnotateNode_DualWritesToMemories verifies that annotate_node writes
// to both the annotations table and the entity-tier memories table.
func TestAnnotateNode_DualWritesToMemories(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	result, err := srv.handleAnnotateNode(ctx, callTool(map[string]any{
		"node_id":  string(loginID),
		"note":     "This function has a known race condition under high concurrency.",
		"agent_id": "agent-7",
	}))
	m := mustResult(t, result, err)

	if m["annotation_id"] == nil {
		t.Error("expected annotation_id in result")
	}

	// Entity-tier memory should also exist.
	entityMems, _ := srv.store.QueryMemories(store.TierEntity, string(loginID), "", 10)
	if len(entityMems) == 0 {
		t.Error("expected entity-tier memory from annotate_node() dual-write")
	}
}

// TestMemoryTouchOnAccess verifies that surfacing memories in session_init
// renews their TTL via TouchMemory.
func TestMemoryTouchOnAccess(t *testing.T) {
	srv := newTestServer(t)

	id, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "Important architectural decision about database schema.",
		AgentID: "agent-8",
		Source:  store.SourceManual,
		Tags:    `["project"]`,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	// Read the initial state.
	mems, _ := srv.store.QueryMemories(store.TierProject, "", "", 10)
	var initial store.Memory
	for _, m := range mems {
		if m.ID == id {
			initial = m
			break
		}
	}
	if initial.ID == "" {
		t.Fatal("could not find initial memory")
	}
	if initial.AccessCount != 0 {
		t.Fatalf("expected initial access_count=0, got %d", initial.AccessCount)
	}

	// Call session_init with scope=full so memories are surfaced and touched.
	// Default scope is "standard" which skips memory surfacing (quickMode).
	srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-8",
		"scope":    "full",
	}))
	// TouchMemory runs in a background goroutine — give it time to complete.
	time.Sleep(100 * time.Millisecond)

	// Read again and verify touch evidence: access_count incremented, last_accessed_at set.
	mems, _ = srv.store.QueryMemories(store.TierProject, "", "", 10)
	for _, m := range mems {
		if m.ID == id {
			if m.AccessCount < 1 {
				t.Errorf("expected access_count >= 1 after touch, got %d", m.AccessCount)
			}
			if m.LastAccessedAt == "" {
				t.Error("expected last_accessed_at to be set after touch")
			}
			if m.ExpiresAt < initial.ExpiresAt {
				t.Errorf("expected expires_at not to decrease, got %s < %s", m.ExpiresAt, initial.ExpiresAt)
			}
			return
		}
	}
	t.Error("memory not found after touch")
}

// TestE2E_SessionMemoryRoundtrip tests the full flow: session → remember →
// end_session → new session_init surfaces memories.
func TestE2E_SessionMemoryRoundtrip(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	// Step 1: First session — agent remembers something.
	srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-e2e"}))
	srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id":       "agent-e2e",
		"decision":       "AuthLogin signature was changed to accept projectID parameter.",
		"affected_nodes": mustJSON(t, []string{string(loginID)}),
		"episode_type":   "failure",
	}))

	// Step 2: End session with summary.
	srv.handleEndSession(ctx, callTool(map[string]any{
		"agent_id": "agent-e2e",
		"summary":  "Fixed AuthLogin to accept projectID. Watch for callers that need updating.",
	}))

	// Step 3: New session — memories should surface.
	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-e2e",
		"scope":    "full",
	}))
	m := mustResult(t, result, err)

	hasKey(t, m, "relevant_memories")
	memSection := m["relevant_memories"].(map[string]any)
	count := memSection["count"].(float64)
	if count < 1 {
		t.Errorf("expected at least 1 relevant memory, got %v", count)
	}
}

// TestSessionInit_SurfacesTaskLinkedEntityMemories verifies the full path:
// create a plan with linked nodes, insert entity memories for those nodes,
// mark the task in_progress, then verify session_init surfaces them as
// "task_entity" memories. This tests the type assertion path that was
// previously only tested implicitly.
func TestSessionInit_SurfacesTaskLinkedEntityMemories(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	// Create a plan with one task linked to loginID.
	planID, _, err := srv.store.CreatePlan("Fix auth", "Fix auth login", "agent-task", []store.TaskInput{{
		Title:       "Fix auth login concurrency",
		Description: "Fix the race condition in AuthLogin",
		Priority:    "p0",
		LinkedNodes: []string{string(loginID)},
	}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	// Get the task ID and mark it in_progress.
	tasks, err := srv.store.GetPendingTasks(planID, "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("GetPendingTasks: %v (len=%d)", err, len(tasks))
	}
	taskID := tasks[0].ID
	if _, _, err := srv.store.UpdateTask(taskID, "in_progress", "", "agent-task"); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	// Insert an entity-tier memory for loginID.
	_, err = srv.store.InsertMemory(store.Memory{
		Tier:     store.TierEntity,
		Content:  "AuthLogin was refactored to fix a deadlock under concurrent access patterns.",
		EntityID: string(loginID),
		AgentID:  "agent-task",
		Source:   store.SourceAuto,
		Tags:     `["entity_change"]`,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	// session_init should surface the entity memory via the task_entity path.
	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-task",
		"scope":    "full",
	}))
	m := mustResult(t, result, err)

	hasKey(t, m, "relevant_memories")
	memSection := m["relevant_memories"].(map[string]any)
	mems := memSection["memories"].([]any)

	found := false
	for _, mem := range mems {
		entry := mem.(map[string]any)
		if entry["label"] == "task_entity" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected task_entity memory in relevant_memories, labels present: %v",
			func() []string {
				labels := make([]string, 0, len(mems))
				for _, mem := range mems {
					labels = append(labels, mem.(map[string]any)["label"].(string))
				}
				return labels
			}())
	}
}

// TestStartBackground_Idempotent verifies that calling StartBackground multiple
// times does not spawn multiple goroutines (only one expiry loop runs).
func TestStartBackground_Idempotent(t *testing.T) {
	srv := newTestServer(t)
	// Call StartBackground three times — should be safe.
	srv.StartBackground()
	srv.StartBackground()
	srv.StartBackground()
	srv.Close()
	// If multiple goroutines were spawned, Close() would only stop one,
	// but the others would panic on a closed channel. Reaching here means
	// at most one goroutine was launched.
}

// TestRecall_SearchSurfacesMemories verifies that recall(query=...) returns
// memories from the unified memories table in addition to episodes.
// This is the key regression test for the recall gap: memories written by
// end_session, annotate_node, and remember() must be queryable via recall().
func TestRecall_SearchSurfacesMemories(t *testing.T) {
	srv := newTestServer(t)

	// Write a memory directly to the memories table (simulates end_session output).
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "Argon2id replaced bcrypt for password hashing after timing attack audit.",
		AgentID: "agent-recall",
		Source:  store.SourceAuto,
		Tags:    `["security","session_end"]`,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	// recall() with a matching query should surface the memory.
	result, err := srv.handleRecall(ctx, callTool(map[string]any{
		"query": "argon2id",
	}))
	m := mustResult(t, result, err)

	// memories field must be present and contain the match.
	mems, ok := m["memories"].([]any)
	if !ok || len(mems) == 0 {
		t.Fatalf("expected memories field in recall result, got: %v", m["memories"])
	}
	first := mems[0].(map[string]any)
	content, _ := first["content"].(string)
	if content == "" {
		t.Error("expected non-empty content in recalled memory")
	}
}

// TestRecall_SearchNoMemoryMatch verifies that recall() with a query that
// matches no memories omits the memories field (zero token cost).
func TestRecall_SearchNoMemoryMatch(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleRecall(ctx, callTool(map[string]any{
		"query": "xyznonexistentterm999",
	}))
	m := mustResult(t, result, err)

	// memories field should be absent when there are no matches.
	if m["memories"] != nil {
		t.Errorf("expected no memories field for unmatched query, got: %v", m["memories"])
	}
}

// mustJSON marshals v to a JSON string, fataling on error.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

// ── AM-5: tier_hint enforcement tests ────────────────────────────────────────

// TestRemember_TierHint_WhenNoAnchorNodes verifies that remember() without
// anchor_nodes returns a tier_hint guiding agents toward Tier 2 storage.
func TestRemember_TierHint_WhenNoAnchorNodes(t *testing.T) {
	srv := newTestServer(t)
	result, err := srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id":     "agent-1",
		"decision":     "Use JWT for auth",
		"episode_type": "decision",
		"outcome":      "success",
	}))
	m := mustResult(t, result, err)

	hint, ok := m["tier_hint"].(string)
	if !ok || hint == "" {
		t.Error("expected tier_hint in response when anchor_nodes is absent")
	}
	if m["anchored_to"] != nil {
		t.Error("anchored_to should be absent when no anchor_nodes provided")
	}
}

// TestRemember_NoTierHint_WhenAnchorNodesProvided verifies that remember()
// with anchor_nodes returns anchored_to and omits tier_hint.
func TestRemember_NoTierHint_WhenAnchorNodesProvided(t *testing.T) {
	srv := newTestServer(t)
	result, err := srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id":     "agent-1",
		"decision":     "AuthService uses JWT",
		"episode_type": "pattern",
		"outcome":      "success",
		"anchor_nodes": `["repo::internal/auth/service.go::AuthService"]`,
	}))
	m := mustResult(t, result, err)

	if m["tier_hint"] != nil {
		t.Error("tier_hint should be absent when anchor_nodes is provided")
	}
	anchored, ok := m["anchored_to"].(float64)
	if !ok || anchored != 1 {
		t.Errorf("expected anchored_to=1, got %v", m["anchored_to"])
	}
}

// ── AM-1: E2E Integration Tests ──────────────────────────────────────────────

// TestE2E_RememberWithAnchors_FullPath exercises the complete lifecycle:
// 1. Call handleRemember with anchor_nodes
// 2. Verify memory exists in store
// 3. Verify anchors exist in memory_anchors table
// 4. Verify anchors are cleaned up when memory expires
func TestE2E_RememberWithAnchors_FullPath(t *testing.T) {
	srv := newTestServer(t)

	// Step 1: Call handleRemember with anchor_nodes.
	anchorNodeIDs := []string{
		"repo::internal/auth/service.go::AuthService",
		"repo::internal/auth/middleware.go::AuthMiddleware",
	}
	res, err := srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id":     "e2e-agent",
		"decision":     "AuthService delegates to AuthMiddleware for all HTTP handler auth",
		"rationale":    "centralizes auth logic, avoids duplication across handlers",
		"outcome":      "success",
		"anchor_nodes": mustJSON(t, anchorNodeIDs),
	}))
	m := mustResult(t, res, err)

	// Verify response contains anchored_to count.
	if v, ok := m["anchored_to"].(float64); !ok || v != 2 {
		t.Fatalf("expected anchored_to=2, got %v", m["anchored_to"])
	}

	// Step 2: Verify project-tier memory exists in store.
	projMems, err := srv.store.QueryMemories(store.TierProject, "", "e2e-agent", 10)
	if err != nil {
		t.Fatalf("QueryMemories: %v", err)
	}
	if len(projMems) == 0 {
		t.Fatal("expected project-tier memory from remember() with anchors")
	}

	// Step 3: Verify anchors exist by querying each anchor node.
	memID := projMems[0].ID
	for _, nodeID := range anchorNodeIDs {
		mems, err := srv.store.GetMemoriesByAnchorNode(nodeID, 10)
		if err != nil {
			t.Fatalf("GetMemoriesByAnchorNode(%s): %v", nodeID, err)
		}
		found := false
		for _, m := range mems {
			if m.ID == memID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected memory %s to be anchored to %s", memID, nodeID)
		}
	}
}

// TestE2E_RememberWithAnchors_InvalidNodeID verifies format validation.
func TestE2E_RememberWithAnchors_InvalidNodeID(t *testing.T) {
	srv := newTestServer(t)
	res, err := srv.handleRemember(ctx, callTool(map[string]any{
		"agent_id":     "e2e-agent",
		"decision":     "some decision about something important",
		"outcome":      "success",
		"anchor_nodes": `["hello_no_separator"]`,
	}))
	mustErrorResult(t, res, err)
}

// ── AM-3: invalidated_memories in session_init ──────────────────────────

// TestSessionInit_SurfacesInvalidatedMemories verifies that session_init
// returns an invalidated_memories section for stale+unsurfaced memories
// and that subsequent calls don't re-surface them.
func TestSessionInit_SurfacesInvalidatedMemories(t *testing.T) {
	srv := newTestServer(t)

	// Insert an entity memory and mark it stale (simulates AM-2 cascade).
	id, err := srv.store.InsertMemory(store.Memory{
		Tier:     store.TierEntity,
		Content:  "synapses-intelligence is an active sidecar",
		EntityID: "repo::intel/main.go::main",
		AgentID:  "agent-1",
		Source:   store.SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.MarkEntityMemoriesStaleForNodes([]string{"repo::intel/main.go::main"}, "anchor node removed"); err != nil {
		t.Fatal(err)
	}

	// First session_init should surface the invalidated memory.
	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-1",
	}))
	m := mustResult(t, result, err)

	hasKey(t, m, "invalidated_memories")
	invalSection, ok := m["invalidated_memories"].(map[string]any)
	if !ok {
		t.Fatalf("invalidated_memories is not a map: %T", m["invalidated_memories"])
	}
	mems, ok := invalSection["memories"].([]any)
	if !ok || len(mems) == 0 {
		t.Fatal("expected at least 1 invalidated memory")
	}
	first := mems[0].(map[string]any)
	if first["id"] != id {
		t.Errorf("expected id=%q, got %v", id, first["id"])
	}
	if first["stale_reason"] != "anchor node removed" {
		t.Errorf("unexpected stale_reason: %v", first["stale_reason"])
	}

	// memory_integrity warning should be present.
	hasKey(t, m, "memory_integrity")

	// Allow background goroutine to mark surfaced.
	time.Sleep(50 * time.Millisecond)

	// Second session_init should NOT re-surface the same memory.
	result2, err2 := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-1",
	}))
	m2 := mustResult(t, result2, err2)
	if _, found := m2["invalidated_memories"]; found {
		t.Error("invalidated_memories should be absent on second call after surfacing")
	}
}

// TestSessionInit_InvalidatedMemories_PerAgentIsolation verifies that
// agent-A surfacing does NOT suppress the invalidation signal for agent-B.
func TestSessionInit_InvalidatedMemories_PerAgentIsolation(t *testing.T) {
	srv := newTestServer(t)

	_, err := srv.store.InsertMemory(store.Memory{
		Tier:     store.TierEntity,
		Content:  "auth middleware uses JWT tokens for session validation",
		EntityID: "repo::auth/middleware.go::AuthMiddleware",
		Source:   store.SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.MarkEntityMemoriesStaleForNodes([]string{"repo::auth/middleware.go::AuthMiddleware"}, "node removed"); err != nil {
		t.Fatal(err)
	}

	// Agent-A calls session_init — sees invalidated memory.
	resultA, errA := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-A"}))
	mA := mustResult(t, resultA, errA)
	hasKey(t, mA, "invalidated_memories")

	// Allow background goroutine to mark surfaced for agent-A.
	time.Sleep(50 * time.Millisecond)

	// Agent-B calls session_init — should STILL see the invalidated memory.
	resultB, errB := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-B"}))
	mB := mustResult(t, resultB, errB)
	if _, found := mB["invalidated_memories"]; !found {
		t.Error("agent-B should still see invalidated_memories after agent-A surfaced them")
	}

	// Agent-A calls again — should NOT see them.
	resultA2, errA2 := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-A"}))
	mA2 := mustResult(t, resultA2, errA2)
	if _, found := mA2["invalidated_memories"]; found {
		t.Error("agent-A should NOT see invalidated_memories on second call")
	}
}

// ── AM-4: include_stale param on recall ──────────────────────────────────────

// TestRecall_IncludeStale_False_ExcludesStaled verifies that recall() without
// include_stale (default false) never returns stale memories — even when query matches.
func TestRecall_IncludeStale_False_ExcludesStaled(t *testing.T) {
	srv := newTestServer(t)

	const entityID = "repo::sidecar.go::SidecarPort"
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:     store.TierEntity,
		Content:  "Deprecated sidecar port was 9090 before port remapping in v2.",
		EntityID: entityID,
		AgentID:  "agent-1",
		Source:   store.SourceManual,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	// Stale the memory via entity ID.
	if err := srv.store.MarkEntityMemoriesStaleForNodes([]string{entityID}, "node removed"); err != nil {
		t.Fatalf("MarkEntityMemoriesStaleForNodes: %v", err)
	}

	// recall without include_stale — should return nothing.
	result, err := srv.handleRecall(ctx, callTool(map[string]any{
		"query": "sidecar port",
	}))
	m := mustResult(t, result, err)
	if m["memories"] != nil {
		t.Errorf("recall(include_stale=false) should not return stale memories, got: %v", m["memories"])
	}
}

// TestRecall_IncludeStale_True_ReturnsStaledMemory verifies that
// recall(include_stale=true) returns stale memories for explicit audit.
func TestRecall_IncludeStale_True_ReturnsStaledMemory(t *testing.T) {
	srv := newTestServer(t)

	const entityID = "repo::sidecar.go::SidecarPortV2"
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:     store.TierEntity,
		Content:  "Deprecated sidecar port was 9090 before port remapping in v2.",
		EntityID: entityID,
		AgentID:  "agent-1",
		Source:   store.SourceManual,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	// Stale the memory.
	if err := srv.store.MarkEntityMemoriesStaleForNodes([]string{entityID}, "node removed"); err != nil {
		t.Fatalf("MarkEntityMemoriesStaleForNodes: %v", err)
	}

	// recall with include_stale=true — should return the stale memory.
	result, err := srv.handleRecall(ctx, callTool(map[string]any{
		"query":         "sidecar port",
		"include_stale": true,
	}))
	m := mustResult(t, result, err)
	mems, ok := m["memories"].([]any)
	if !ok || len(mems) == 0 {
		t.Fatalf("recall(include_stale=true) expected stale memory in results, got: %v", m["memories"])
	}
}

// TestRecall_Browse_IncludeStale_True_ReturnsStaledMemory verifies that
// recall in browse mode (empty query) with include_stale=true also returns stale.
func TestRecall_Browse_IncludeStale_True_ReturnsStaledMemory(t *testing.T) {
	srv := newTestServer(t)

	const entityID = "repo::config.go::OldConfig"
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:     store.TierEntity,
		Content:  "Old component config that was archived in refactor.",
		EntityID: entityID,
		AgentID:  "agent-browse",
		Source:   store.SourceManual,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	// Stale the memory.
	if err := srv.store.MarkEntityMemoriesStaleForNodes([]string{entityID}, "node removed"); err != nil {
		t.Fatalf("MarkEntityMemoriesStaleForNodes: %v", err)
	}

	// Browse without include_stale — stale memory absent.
	result, err := srv.handleRecall(ctx, callTool(map[string]any{}))
	m := mustResult(t, result, err)
	if mems, ok := m["memories"].([]any); ok && len(mems) > 0 {
		t.Errorf("browse(include_stale=false) should not return stale memories, got %d", len(mems))
	}

	// Browse with include_stale=true — stale memory present.
	result2, err2 := srv.handleRecall(ctx, callTool(map[string]any{
		"include_stale": true,
	}))
	m2 := mustResult(t, result2, err2)
	mems2, ok2 := m2["memories"].([]any)
	if !ok2 || len(mems2) == 0 {
		t.Fatalf("browse(include_stale=true) expected stale memory in results, got: %v", m2["memories"])
	}
}

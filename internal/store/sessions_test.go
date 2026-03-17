package store

import (
	"sync"
	"testing"
	"time"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// createTestSession is a test-only convenience that calls GetOrResumeSession
// with a unique mcp_session_id so each call produces a fresh independent session row.
// This keeps tests isolated without exposing a footgun in the production API.
func createTestSession(t *testing.T, st *Store, agentID, projectID, intent string) string {
	t.Helper()
	id, _, err := st.GetOrResumeSession(agentID, projectID, newID(), intent, 0)
	if err != nil {
		t.Fatalf("createTestSession(%s, %s): %v", agentID, projectID, err)
	}
	return id
}

func mustCreatePlan(t *testing.T, st *Store, title string, tasks []TaskInput) string {
	t.Helper()
	planID, _, err := st.CreatePlan(title, "", "", tasks)
	if err != nil {
		t.Fatalf("CreatePlan(%q): %v", title, err)
	}
	return planID
}

func mustGetFirstTask(t *testing.T, st *Store, planID string) string {
	t.Helper()
	tasks, err := st.GetPendingTasks(planID, "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("GetPendingTasks for plan %s: err=%v tasks=%d", planID, err, len(tasks))
	}
	return tasks[0].ID
}

// ── GetOrResumeSession (session creation) ────────────────────────────────────

func TestGetOrResumeSession_CreatesNewSession(t *testing.T) {
	st := openTestStore(t)
	id, resumed, err := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "fix bug", 300)
	if err != nil {
		t.Fatalf("GetOrResumeSession: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty session ID")
	}
	if resumed {
		t.Error("expected resumed=false for first call")
	}
}

func TestGetOrResumeSession_EmptyIntent(t *testing.T) {
	st := openTestStore(t)
	id, _, err := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "", 300)
	if err != nil {
		t.Fatalf("GetOrResumeSession with empty intent: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty session ID")
	}
}

func TestGetOrResumeSession_ResumesWithinWindow(t *testing.T) {
	st := openTestStore(t)
	id1, _, _ := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 300)

	id2, resumed, err := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 300)
	if err != nil {
		t.Fatalf("second GetOrResumeSession: %v", err)
	}
	if !resumed {
		t.Error("expected resumed=true within reconnect window")
	}
	if id2 != id1 {
		t.Errorf("expected same session ID on resume: got %q want %q", id2, id1)
	}
}

func TestGetOrResumeSession_NewSessionAfterWindow(t *testing.T) {
	st := openTestStore(t)
	id1, _, _ := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 0)

	// Manually expire the session by setting last_seen_at outside the default window.
	past := time.Now().UTC().Unix() - 400
	st.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id1) //nolint:errcheck

	id2, resumed, err := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 0)
	if err != nil {
		t.Fatalf("GetOrResumeSession after window: %v", err)
	}
	if resumed {
		t.Error("expected resumed=false after window expired")
	}
	if id2 == id1 {
		t.Error("expected new session ID after window expired")
	}
}

func TestGetOrResumeSession_DifferentConnectionsNeverCollide(t *testing.T) {
	st := openTestStore(t)
	// Two simultaneous connections with same agentID — must be independent sessions.
	idA, resumedA, _ := st.GetOrResumeSession("claude-code", "proj-1", "mcp-conn-A", "work", 300)
	idB, resumedB, _ := st.GetOrResumeSession("claude-code", "proj-1", "mcp-conn-B", "work", 300)

	if idA == idB {
		t.Errorf("different MCP connections must get different session IDs, both got %q", idA)
	}
	if resumedA || resumedB {
		t.Error("neither connection should resume the other's session")
	}
}

func TestGetOrResumeSession_SupersedesOwnPriorSession(t *testing.T) {
	st := openTestStore(t)
	id1, _, _ := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 0)
	past := time.Now().UTC().Unix() - 400
	st.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id1) //nolint:errcheck

	id2, _, _ := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 0)

	var endReason string
	var endedAt *int64
	err := st.db.QueryRow(`SELECT ended_at, end_reason FROM sessions WHERE id = ?`, id1).Scan(&endedAt, &endReason)
	if err != nil {
		t.Fatalf("query old session: %v", err)
	}
	if endedAt == nil {
		t.Errorf("old session %s must be superseded (ended_at should be set)", id1)
	}
	if endReason != "superseded" {
		t.Errorf("old session end_reason: got %q want %q", endReason, "superseded")
	}
	_ = id2
}

func TestGetOrResumeSession_DoesNotSupersedeConcurrentConnection(t *testing.T) {
	st := openTestStore(t)
	idA, _, _ := st.GetOrResumeSession("claude-code", "proj-1", "mcp-conn-A", "work", 300)

	// Expire Window A's session so it's outside the reconnect window.
	past := time.Now().UTC().Unix() - 400
	st.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, idA) //nolint:errcheck

	// Window B starts fresh — must NOT supersede Window A's session.
	st.GetOrResumeSession("claude-code", "proj-1", "mcp-conn-B", "work", 300) //nolint:errcheck

	var endedAt *int64
	st.db.QueryRow(`SELECT ended_at FROM sessions WHERE id = ?`, idA).Scan(&endedAt) //nolint:errcheck
	if endedAt != nil {
		t.Errorf("Window B must not supersede Window A's session (different mcp_session_id)")
	}
}

func TestGetOrResumeSession_ConcurrentCallsSameMCPSession(t *testing.T) {
	st := openTestStore(t)
	// Fire 10 concurrent GetOrResumeSession calls for the same connection.
	// Exactly one fresh session must be created; all others must resume it.
	const n = 10
	ids := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			id, _, _ := st.GetOrResumeSession("agent-c", "proj-c", "mcp-conn-c", "work", 300)
			ids[i] = id
		}()
	}
	wg.Wait()

	first := ids[0]
	for i, id := range ids {
		if id == "" {
			t.Errorf("goroutine %d returned empty session ID", i)
		}
		if id != first {
			t.Errorf("goroutine %d got different session ID %q (want %q) — BEGIN IMMEDIATE serialization failed", i, id, first)
		}
	}

	var count int
	st.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE mcp_session_id = 'mcp-conn-c' AND ended_at IS NULL`).Scan(&count) //nolint:errcheck
	if count != 1 {
		t.Errorf("expected exactly 1 live session for mcp-conn-c, got %d", count)
	}
}

// ── TouchSession ──────────────────────────────────────────────────────────────

func TestTouchSession_UpdatesLastSeen(t *testing.T) {
	st := openTestStore(t)
	id := createTestSession(t, st, "agent-1", "proj-1", "")

	// Backdate so it appears stale.
	past := time.Now().UTC().Unix() - 120
	st.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id) //nolint:errcheck

	st.TouchSession(id)

	// After Touch, last_seen_at should be ~now — session must NOT appear stale.
	stale, err := st.GetStaleSessions("proj-1", "other-id", time.Minute)
	if err != nil {
		t.Fatalf("GetStaleSessions: %v", err)
	}
	for _, s := range stale {
		if s.SessionID == id {
			t.Errorf("session %s appears stale after Touch — last_seen not updated", id)
		}
	}
}

func TestTouchSession_EmptyIDIsNoop(t *testing.T) {
	st := openTestStore(t)
	st.TouchSession("") // must not panic
}

// ── EndSession ────────────────────────────────────────────────────────────────

func TestEndSession_MarksEnded(t *testing.T) {
	st := openTestStore(t)
	id := createTestSession(t, st, "agent-1", "proj-1", "")

	if err := st.EndSession(id, "clean", "success", "all done"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	// Ended sessions must NOT appear in stale detection.
	stale, _ := st.GetStaleSessions("proj-1", "other", 0)
	for _, s := range stale {
		if s.SessionID == id {
			t.Errorf("ended session %s still appears as stale", id)
		}
	}
}

func TestEndSession_UnknownIDIsNoop(t *testing.T) {
	st := openTestStore(t)
	if err := st.EndSession("nonexistent-id", "clean", "unknown", ""); err != nil {
		t.Fatalf("EndSession on unknown ID: %v", err)
	}
}

// ── GetStaleSessions ──────────────────────────────────────────────────────────

func TestGetStaleSessions_DetectsStale(t *testing.T) {
	st := openTestStore(t)

	id := createTestSession(t, st, "agent-stale", "proj-s", "working")
	past := time.Now().UTC().Unix() - 120
	st.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id) //nolint:errcheck

	stale, err := st.GetStaleSessions("proj-s", "different-current", time.Minute)
	if err != nil {
		t.Fatalf("GetStaleSessions: %v", err)
	}
	found := false
	for _, s := range stale {
		if s.SessionID == id {
			found = true
			if s.AgentID != "agent-stale" {
				t.Errorf("wrong agent_id: got %q", s.AgentID)
			}
		}
	}
	if !found {
		t.Errorf("expected session %s in stale results", id)
	}
}

func TestGetStaleSessions_ExcludesCurrentSession(t *testing.T) {
	st := openTestStore(t)
	id := createTestSession(t, st, "agent-1", "proj-s", "")
	past := time.Now().UTC().Unix() - 120
	st.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id) //nolint:errcheck

	stale, _ := st.GetStaleSessions("proj-s", id, time.Minute)
	for _, s := range stale {
		if s.SessionID == id {
			t.Errorf("current session %s must not appear in stale results", id)
		}
	}
}

func TestGetStaleSessions_ExcludesEndedSessions(t *testing.T) {
	st := openTestStore(t)
	id := createTestSession(t, st, "agent-1", "proj-s", "")
	_ = st.EndSession(id, "clean", "success", "")

	stale, _ := st.GetStaleSessions("proj-s", "other", 0)
	for _, s := range stale {
		if s.SessionID == id {
			t.Errorf("cleanly ended session %s must not appear as stale", id)
		}
	}
}

func TestGetStaleSessions_ExcludesOtherProjects(t *testing.T) {
	st := openTestStore(t)
	createTestSession(t, st, "agent-1", "proj-A", "") //nolint:errcheck

	stale, _ := st.GetStaleSessions("proj-B", "other", 0)
	for _, s := range stale {
		if s.AgentID == "agent-1" {
			t.Errorf("session from proj-A leaked into proj-B stale results")
		}
	}
}

func TestGetStaleSessions_CappedAtFive(t *testing.T) {
	st := openTestStore(t)
	past := time.Now().UTC().Unix() - 120
	for i := 0; i < 8; i++ {
		id := createTestSession(t, st, "agent-x", "proj-cap", "")
		st.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id) //nolint:errcheck
	}
	stale, _ := st.GetStaleSessions("proj-cap", "other", time.Minute)
	if len(stale) > 5 {
		t.Errorf("stale results must be capped at 5, got %d", len(stale))
	}
}

func TestGetStaleSessions_TimestampsAreRFC3339(t *testing.T) {
	st := openTestStore(t)
	id := createTestSession(t, st, "agent-1", "proj-ts", "")
	past := time.Now().UTC().Unix() - 120
	st.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id) //nolint:errcheck

	stale, _ := st.GetStaleSessions("proj-ts", "other", time.Minute)
	for _, s := range stale {
		if s.SessionID != id {
			continue
		}
		if s.StartedAt == "" || s.LastSeenAt == "" {
			t.Errorf("timestamps must be non-empty RFC3339 strings")
		}
		if _, err := time.Parse(time.RFC3339, s.StartedAt); err != nil {
			t.Errorf("StartedAt %q is not valid RFC3339: %v", s.StartedAt, err)
		}
		if _, err := time.Parse(time.RFC3339, s.LastSeenAt); err != nil {
			t.Errorf("LastSeenAt %q is not valid RFC3339: %v", s.LastSeenAt, err)
		}
	}
}

// ── LinkSessionTask / GetOrphanedTasks ────────────────────────────────────────

func TestGetOrphanedTasks_DetectsCreatedNotCompleted(t *testing.T) {
	st := openTestStore(t)
	sessID := createTestSession(t, st, "agent-1", "proj-1", "")
	planID := mustCreatePlan(t, st, "orphan-plan", []TaskInput{{Title: "task-A", Priority: "p0"}})
	taskID := mustGetFirstTask(t, st, planID)

	st.LinkSessionTask(sessID, taskID, SessionTaskCreated)

	orphans, err := st.GetOrphanedTasks(sessID)
	if err != nil {
		t.Fatalf("GetOrphanedTasks: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan, got %d", len(orphans))
	}
	if orphans[0].TaskID != taskID {
		t.Errorf("orphan task ID: got %q want %q", orphans[0].TaskID, taskID)
	}
	if orphans[0].Action != "created" {
		t.Errorf("orphan action: got %q want %q", orphans[0].Action, "created")
	}
}

func TestGetOrphanedTasks_ClaimedAfterCreated_ReturnsLatestAction(t *testing.T) {
	st := openTestStore(t)
	sessID := createTestSession(t, st, "agent-1", "proj-1", "")
	planID := mustCreatePlan(t, st, "claimed-plan", []TaskInput{{Title: "task-B", Priority: "p0"}})
	taskID := mustGetFirstTask(t, st, planID)

	// Both actions inserted — latest is 'claimed'.
	st.LinkSessionTask(sessID, taskID, SessionTaskCreated)
	st.LinkSessionTask(sessID, taskID, SessionTaskClaimed)

	orphans, err := st.GetOrphanedTasks(sessID)
	if err != nil {
		t.Fatalf("GetOrphanedTasks: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("expected exactly 1 orphan (not duplicate rows), got %d", len(orphans))
	}
	// Latest action by `at` timestamp is 'claimed' — more informative than 'created'.
	if orphans[0].Action != "claimed" {
		t.Errorf("expected latest action 'claimed', got %q", orphans[0].Action)
	}
}

func TestGetOrphanedTasks_ExcludesCompleted(t *testing.T) {
	st := openTestStore(t)
	sessID := createTestSession(t, st, "agent-1", "proj-1", "")
	planID := mustCreatePlan(t, st, "done-plan", []TaskInput{{Title: "task-done", Priority: "p0"}})
	taskID := mustGetFirstTask(t, st, planID)

	st.LinkSessionTask(sessID, taskID, SessionTaskCreated)
	st.LinkSessionTask(sessID, taskID, SessionTaskCompleted)

	orphans, _ := st.GetOrphanedTasks(sessID)
	for _, o := range orphans {
		if o.TaskID == taskID {
			t.Errorf("completed task %s must not appear as orphan", taskID)
		}
	}
}

func TestGetOrphanedTasks_ExcludesCompletedByOtherSession(t *testing.T) {
	st := openTestStore(t)
	sessA := createTestSession(t, st, "agent-A", "proj-1", "")
	sessB := createTestSession(t, st, "agent-B", "proj-1", "")
	planID := mustCreatePlan(t, st, "cross-plan", []TaskInput{{Title: "shared-task", Priority: "p0"}})
	taskID := mustGetFirstTask(t, st, planID)

	st.LinkSessionTask(sessA, taskID, SessionTaskCreated)
	st.LinkSessionTask(sessB, taskID, SessionTaskCompleted)

	orphans, _ := st.GetOrphanedTasks(sessA)
	for _, o := range orphans {
		if o.TaskID == taskID {
			t.Errorf("task completed by another session must not appear as orphan for sessA")
		}
	}
}

func TestGetOrphanedTasks_EmptySessionHasNoOrphans(t *testing.T) {
	st := openTestStore(t)
	sessID := createTestSession(t, st, "agent-1", "proj-1", "")

	orphans, err := st.GetOrphanedTasks(sessID)
	if err != nil {
		t.Fatalf("GetOrphanedTasks on empty session: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("expected 0 orphans, got %d", len(orphans))
	}
}

func TestLinkSessionTask_EmptyIDsAreNoop(t *testing.T) {
	st := openTestStore(t)
	st.LinkSessionTask("", "task-1", SessionTaskCreated)
	st.LinkSessionTask("sess-1", "", SessionTaskCreated)
	st.LinkSessionTask("", "", SessionTaskCreated)
}

// ── GetToolCallSummary ────────────────────────────────────────────────────────

func TestGetToolCallSummary_EmptySession(t *testing.T) {
	st := openTestStore(t)
	sessID := createTestSession(t, st, "agent-1", "proj-1", "")

	summary, err := st.GetToolCallSummary(sessID)
	if err != nil {
		t.Fatalf("GetToolCallSummary on empty session: %v", err)
	}
	if summary.TotalCalls != 0 {
		t.Errorf("expected 0 calls, got %d", summary.TotalCalls)
	}
	if summary.DurationMs != 0 {
		t.Errorf("expected 0 duration, got %d", summary.DurationMs)
	}
}

func TestGetToolCallSummary_AccumulatesDuration(t *testing.T) {
	st := openTestStore(t)
	sessID := createTestSession(t, st, "agent-1", "proj-1", "")

	for i := 0; i < 3; i++ {
		st.RecordToolCall("get_context", "agent-1", sessID, "Entity", 100, true)
	}
	st.RecordToolCall("search", "agent-1", sessID, "", 200, false)

	summary, err := st.GetToolCallSummary(sessID)
	if err != nil {
		t.Fatalf("GetToolCallSummary: %v", err)
	}
	if summary.TotalCalls != 4 {
		t.Errorf("TotalCalls: got %d want 4", summary.TotalCalls)
	}
	// Duration is cumulative sum: 3×100 + 200 = 500 ms — not wall-clock span.
	if summary.DurationMs != 500 {
		t.Errorf("DurationMs: got %d want 500 (cumulative sum)", summary.DurationMs)
	}
	// ErrorRate: 1 failure / 4 calls = 0.25.
	if summary.ErrorRate < 0.24 || summary.ErrorRate > 0.26 {
		t.Errorf("ErrorRate: got %.4f want ~0.25", summary.ErrorRate)
	}
}

func TestGetToolCallSummary_TopToolsOrdered(t *testing.T) {
	st := openTestStore(t)
	sessID := createTestSession(t, st, "agent-1", "proj-1", "")

	for i := 0; i < 5; i++ {
		st.RecordToolCall("get_context", "agent-1", sessID, "", 10, true)
	}
	for i := 0; i < 2; i++ {
		st.RecordToolCall("search", "agent-1", sessID, "", 10, true)
	}
	st.RecordToolCall("session_init", "agent-1", sessID, "", 10, true)

	summary, _ := st.GetToolCallSummary(sessID)
	if len(summary.TopTools) == 0 {
		t.Fatal("expected top tools, got none")
	}
	if summary.TopTools[0].ToolName != "get_context" {
		t.Errorf("top tool: got %q want %q", summary.TopTools[0].ToolName, "get_context")
	}
	if summary.TopTools[0].Count != 5 {
		t.Errorf("top tool count: got %d want 5", summary.TopTools[0].Count)
	}
}

func TestGetToolCallSummary_DoesNotLeakAcrossSessions(t *testing.T) {
	st := openTestStore(t)
	sessA := createTestSession(t, st, "agent-A", "proj-1", "")
	sessB := createTestSession(t, st, "agent-B", "proj-1", "")

	for i := 0; i < 3; i++ {
		st.RecordToolCall("get_context", "agent-A", sessA, "", 50, true)
	}
	st.RecordToolCall("search", "agent-B", sessB, "", 50, true)

	sumA, _ := st.GetToolCallSummary(sessA)
	sumB, _ := st.GetToolCallSummary(sessB)

	if sumA.TotalCalls != 3 {
		t.Errorf("sessA: got %d calls want 3", sumA.TotalCalls)
	}
	if sumB.TotalCalls != 1 {
		t.Errorf("sessB: got %d calls want 1", sumB.TotalCalls)
	}
}

// ── PruneToolCallsOlderThan ───────────────────────────────────────────────────

func TestPruneToolCallsOlderThan_RemovesOldRows(t *testing.T) {
	st := openTestStore(t)
	sessID := createTestSession(t, st, "agent-1", "proj-1", "")

	st.db.Exec( //nolint:errcheck
		`INSERT INTO tool_calls(tool_name, agent_id, session_id, entity, duration_ms, success, created_at)
		 VALUES ('old_tool', 'agent-1', ?, '', 10, 1, '2020-01-01T00:00:00Z')`, sessID)
	st.RecordToolCall("recent_tool", "agent-1", sessID, "", 10, true)

	// Reset debounce so prune runs.
	st.lastPruneMu.Lock()
	st.lastPruneAt = time.Time{}
	st.lastPruneMu.Unlock()

	n, err := st.PruneToolCallsOlderThan(7 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneToolCallsOlderThan: %v", err)
	}
	if n == 0 {
		t.Error("expected at least 1 row pruned")
	}

	var count int
	st.db.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE tool_name = 'recent_tool'`).Scan(&count) //nolint:errcheck
	if count == 0 {
		t.Error("recent tool_call was incorrectly pruned")
	}
}

func TestPruneToolCallsOlderThan_Debounce(t *testing.T) {
	st := openTestStore(t)

	st.lastPruneMu.Lock()
	st.lastPruneAt = time.Time{}
	st.lastPruneMu.Unlock()

	n1, _ := st.PruneToolCallsOlderThan(time.Hour)
	n2, err := st.PruneToolCallsOlderThan(time.Hour)
	if err != nil {
		t.Fatalf("second PruneToolCallsOlderThan: %v", err)
	}
	if n2 != 0 {
		t.Errorf("debounce failed: second call deleted %d rows (want 0); first deleted %d", n2, n1)
	}
}

// ── Full lifecycle ────────────────────────────────────────────────────────────

func TestSessionLifecycle_CreateTouchEnd(t *testing.T) {
	st := openTestStore(t)

	sessID := createTestSession(t, st, "agent-1", "proj-1", "refactor auth")

	for i := 0; i < 5; i++ {
		st.TouchSession(sessID)
	}

	// Active session must not appear stale within a large threshold.
	stale, _ := st.GetStaleSessions("proj-1", "other", 24*time.Hour)
	for _, s := range stale {
		if s.SessionID == sessID {
			t.Error("active session must not be stale within threshold")
		}
	}

	if err := st.EndSession(sessID, "clean", "success", "refactored Auth.Login"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	stale, _ = st.GetStaleSessions("proj-1", "other", 0)
	for _, s := range stale {
		if s.SessionID == sessID {
			t.Error("ended session must not appear in stale results")
		}
	}
}

func TestSessionLifecycle_OrphanDetection(t *testing.T) {
	st := openTestStore(t)

	sessID := createTestSession(t, st, "agent-1", "proj-1", "")
	planID := mustCreatePlan(t, st, "lifecycle-plan", []TaskInput{
		{Title: "task-alpha", Priority: "p0"},
		{Title: "task-beta", Priority: "p1"},
	})
	tasks, _ := st.GetPendingTasks(planID, "")

	// Alpha: created and claimed but not completed → orphan, latest action = "claimed".
	st.LinkSessionTask(sessID, tasks[0].ID, SessionTaskCreated)
	st.LinkSessionTask(sessID, tasks[0].ID, SessionTaskClaimed)

	// Beta: created and completed → not orphan.
	st.LinkSessionTask(sessID, tasks[1].ID, SessionTaskCreated)
	st.LinkSessionTask(sessID, tasks[1].ID, SessionTaskCompleted)

	orphans, err := st.GetOrphanedTasks(sessID)
	if err != nil {
		t.Fatalf("GetOrphanedTasks: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan, got %d: %+v", len(orphans), orphans)
	}
	if orphans[0].TaskID != tasks[0].ID {
		t.Errorf("wrong orphan: got %q want %q", orphans[0].TaskID, tasks[0].ID)
	}
	// Latest action for alpha is "claimed" (inserted after "created").
	if orphans[0].Action != "claimed" {
		t.Errorf("expected latest action 'claimed', got %q", orphans[0].Action)
	}
	if orphans[0].LikelyStatus != "unclear" {
		t.Errorf("default LikelyStatus: got %q want %q", orphans[0].LikelyStatus, "unclear")
	}
}

func TestSessionLifecycle_ParallelSessions(t *testing.T) {
	st := openTestStore(t)

	sessA := createTestSession(t, st, "agent-A", "proj-parallel", "feature-X")
	sessB := createTestSession(t, st, "agent-B", "proj-parallel", "feature-Y")
	sessC := createTestSession(t, st, "agent-C", "proj-parallel", "bugfix-Z")

	for i := 0; i < 3; i++ {
		st.TouchSession(sessA)
		st.TouchSession(sessB)
		st.TouchSession(sessC)
	}

	st.EndSession(sessA, "clean", "success", "")  //nolint:errcheck
	st.EndSession(sessB, "clean", "partial", "") //nolint:errcheck

	past := time.Now().UTC().Unix() - 120
	for _, sid := range []string{sessA, sessB, sessC} {
		st.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, sid) //nolint:errcheck
	}

	stale, _ := st.GetStaleSessions("proj-parallel", "other", time.Minute)
	foundC := false
	for _, s := range stale {
		if s.SessionID == sessA || s.SessionID == sessB {
			t.Errorf("ended session %s must not appear stale", s.SessionID)
		}
		if s.SessionID == sessC {
			foundC = true
		}
	}
	if !foundC {
		t.Error("open session C must appear in stale results")
	}
}

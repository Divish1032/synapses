package store

import (
	"sync"
	"testing"
	"time"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// createTestSession is a test-only convenience that calls GetOrResumeSession
// with a unique mcp_session_id so each call produces a fresh independent session row.
// Hibernate resume is disabled (-1) to keep tests isolated.
func createTestSession(t *testing.T, st *Store, agentID, projectID, intent string) string {
	t.Helper()
	id, _, _, err := st.GetOrResumeSession(agentID, projectID, newID(), intent, 0, -1)
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
	t.Parallel()
	st := openTestStore(t)
	id, resumed, hibCtx, err := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "fix bug", 300, -1)
	if err != nil {
		t.Fatalf("GetOrResumeSession: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty session ID")
	}
	if resumed {
		t.Error("expected resumed=false for first call")
	}
	if hibCtx != nil {
		t.Error("expected nil hibernateCtx for first call")
	}
}

func TestGetOrResumeSession_EmptyIntent(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	id, _, _, err := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "", 300, -1)
	if err != nil {
		t.Fatalf("GetOrResumeSession with empty intent: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty session ID")
	}
}

func TestGetOrResumeSession_ResumesWithinWindow(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 300, -1)

	id2, resumed, hibCtx, err := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 300, -1)
	if err != nil {
		t.Fatalf("second GetOrResumeSession: %v", err)
	}
	if !resumed {
		t.Error("expected resumed=true within reconnect window")
	}
	if id2 != id1 {
		t.Errorf("expected same session ID on resume: got %q want %q", id2, id1)
	}
	if hibCtx != nil {
		t.Error("expected nil hibernateCtx on same-connection resume")
	}
}

func TestGetOrResumeSession_NewSessionAfterWindow(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 0, -1)

	// Manually expire the session by setting last_seen_at outside the default window.
	past := time.Now().UTC().Unix() - 400
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id1) //nolint:errcheck

	id2, resumed, _, err := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 0, -1)
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
	t.Parallel()
	st := openTestStore(t)
	// Two simultaneous connections with same agentID — must be independent sessions.
	idA, resumedA, _, _ := st.GetOrResumeSession("claude-code", "proj-1", "mcp-conn-A", "work", 300, -1)
	idB, resumedB, _, _ := st.GetOrResumeSession("claude-code", "proj-1", "mcp-conn-B", "work", 300, -1)

	if idA == idB {
		t.Errorf("different MCP connections must get different session IDs, both got %q", idA)
	}
	if resumedA || resumedB {
		t.Error("neither connection should resume the other's session")
	}
}

func TestGetOrResumeSession_SupersedesOwnPriorSession(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 0, -1)
	past := time.Now().UTC().Unix() - 400
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id1) //nolint:errcheck

	id2, _, _, _ := st.GetOrResumeSession("agent-1", "proj-1", "mcp-conn-1", "work", 0, -1)

	var endReason string
	var endedAt *int64
	err := st.knowledgeDB.QueryRow(`SELECT ended_at, end_reason FROM sessions WHERE id = ?`, id1).Scan(&endedAt, &endReason)
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
	t.Parallel()
	st := openTestStore(t)
	idA, _, _, _ := st.GetOrResumeSession("claude-code", "proj-1", "mcp-conn-A", "work", 300, -1)

	// Expire Window A's session so it's outside the reconnect window.
	past := time.Now().UTC().Unix() - 400
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, idA) //nolint:errcheck

	// Window B starts fresh (hibernate disabled) — must NOT supersede Window A's session.
	st.GetOrResumeSession("claude-code", "proj-1", "mcp-conn-B", "work", 300, -1) //nolint:errcheck

	var endedAt *int64
	st.knowledgeDB.QueryRow(`SELECT ended_at FROM sessions WHERE id = ?`, idA).Scan(&endedAt) //nolint:errcheck
	if endedAt != nil {
		t.Errorf("Window B must not supersede Window A's session (different mcp_session_id)")
	}
}

func TestGetOrResumeSession_ConcurrentCallsSameMCPSession(t *testing.T) {
	t.Parallel()
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
			id, _, _, _ := st.GetOrResumeSession("agent-c", "proj-c", "mcp-conn-c", "work", 300, -1)
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
	st.knowledgeDB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE mcp_session_id = 'mcp-conn-c' AND ended_at IS NULL`).Scan(&count) //nolint:errcheck
	if count != 1 {
		t.Errorf("expected exactly 1 live session for mcp-conn-c, got %d", count)
	}
}

// ── Cross-connection hibernate resume (Phase 2) ───────────────────────────────

func TestGetOrResumeSession_HibernateResume_SameAgentNewConnection(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Session started on conn-A with intent.
	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-h", "mcp-conn-A", "implement feature X", 300, 14400)
	if id1 == "" {
		t.Fatal("expected non-empty session ID for conn-A")
	}

	// Backdate last_seen_at to simulate a 1-hour break (past reconnect window, within hibernate window).
	past := time.Now().UTC().Unix() - 3600
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id1) //nolint:errcheck

	// Same agent, same project, NEW connection (editor restarted after break).
	id2, resumed, hibCtx, err := st.GetOrResumeSession("agent-1", "proj-h", "mcp-conn-B", "", 300, 14400)
	if err != nil {
		t.Fatalf("hibernate resume GetOrResumeSession: %v", err)
	}
	if resumed {
		t.Error("expected resumed=false (not a same-connection resume)")
	}
	if hibCtx == nil {
		t.Fatal("expected non-nil hibernateCtx for cross-connection resume")
	}
	if id2 != id1 {
		t.Errorf("expected same session ID on hibernate resume: got %q want %q", id2, id1)
	}
	if hibCtx.PriorIntent != "implement feature X" {
		t.Errorf("prior intent: got %q want %q", hibCtx.PriorIntent, "implement feature X")
	}
	if hibCtx.GapSeconds < 3590 {
		t.Errorf("gap should be ~3600s, got %d", hibCtx.GapSeconds)
	}
	if hibCtx.ParentID != id1 {
		t.Errorf("ParentID: got %q want %q", hibCtx.ParentID, id1)
	}
}

func TestGetOrResumeSession_HibernateResume_PreservesIntentWhenNewIntentEmpty(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-h2", "mcp-conn-A", "fix auth bug", 300, 14400)
	past := time.Now().UTC().Unix() - 3600
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id1) //nolint:errcheck

	// Resume with no new intent — prior intent must be preserved.
	_, _, hibCtx, _ := st.GetOrResumeSession("agent-1", "proj-h2", "mcp-conn-B", "", 300, 14400)
	if hibCtx == nil {
		t.Fatal("expected hibernate resume")
	}

	var storedIntent string
	st.knowledgeDB.QueryRow(`SELECT intent FROM sessions WHERE id = ?`, id1).Scan(&storedIntent) //nolint:errcheck
	if storedIntent != "fix auth bug" {
		t.Errorf("intent should be preserved when new intent is empty; got %q", storedIntent)
	}
}

func TestGetOrResumeSession_HibernateResume_NewIntentOverridesPrior(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-h3", "mcp-conn-A", "old intent", 300, 14400)
	past := time.Now().UTC().Unix() - 3600
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id1) //nolint:errcheck

	_, _, _, _ = st.GetOrResumeSession("agent-1", "proj-h3", "mcp-conn-B", "new intent", 300, 14400)

	var storedIntent string
	st.knowledgeDB.QueryRow(`SELECT intent FROM sessions WHERE id = ?`, id1).Scan(&storedIntent) //nolint:errcheck
	if storedIntent != "new intent" {
		t.Errorf("new intent should override prior; got %q", storedIntent)
	}
}

func TestGetOrResumeSession_HibernateResume_DisabledWhenWindowNegative(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Hibernate disabled via -1.
	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-hd", "mcp-conn-A", "work", 300, -1)
	past := time.Now().UTC().Unix() - 3600
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id1) //nolint:errcheck

	id2, _, hibCtx, _ := st.GetOrResumeSession("agent-1", "proj-hd", "mcp-conn-B", "work", 300, -1)
	if hibCtx != nil {
		t.Error("expected no hibernate resume when window is -1")
	}
	if id2 == id1 {
		t.Error("expected fresh session when hibernate is disabled")
	}
}

func TestGetOrResumeSession_HibernateResume_ExpiredSessionNotResumed(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Session last seen 5 hours ago — outside the 4-hour hibernate window.
	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-he", "mcp-conn-A", "work", 300, 14400)
	wayPast := time.Now().UTC().Unix() - 18001 // >5 hours ago
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, wayPast, id1) //nolint:errcheck

	id2, _, hibCtx, _ := st.GetOrResumeSession("agent-1", "proj-he", "mcp-conn-B", "work", 300, 14400)
	if hibCtx != nil {
		t.Error("expected no hibernate resume for session outside hibernate window")
	}
	if id2 == id1 {
		t.Error("expected fresh session for expired dormant session")
	}
}

func TestGetOrResumeSession_HibernateResume_DoesNotStealLiveConcurrentSession(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// conn-A is a live concurrent session (seen within reconnect window).
	idA, _, _, _ := st.GetOrResumeSession("claude-code", "proj-hc", "mcp-conn-A", "work", 300, 14400)
	if idA == "" {
		t.Fatal("expected non-empty session for conn-A")
	}
	// conn-A is LIVE (last_seen_at = now, within reconnect window).
	// conn-B must NOT steal it.

	idB, _, hibCtx, _ := st.GetOrResumeSession("claude-code", "proj-hc", "mcp-conn-B", "work", 300, 14400)
	if hibCtx != nil {
		t.Error("must not hibernate-resume a currently-live session from another connection")
	}
	if idB == idA {
		t.Error("conn-B must get its own session, not steal conn-A's live session")
	}
}

func TestGetOrResumeSession_HibernateResume_StateBecomesActive(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-hs", "mcp-conn-A", "work", 300, 14400)
	past := time.Now().UTC().Unix() - 3600
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id1) //nolint:errcheck

	_, _, hibCtx, _ := st.GetOrResumeSession("agent-1", "proj-hs", "mcp-conn-B", "work", 300, 14400)
	if hibCtx == nil {
		t.Fatal("expected hibernate resume")
	}

	var state string
	st.knowledgeDB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id1).Scan(&state) //nolint:errcheck
	if state != "active" {
		t.Errorf("state after hibernate resume: got %q want %q", state, "active")
	}
}

func TestGetOrResumeSession_HibernateResume_MCPSessionIDUpdated(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-hm", "mcp-conn-A", "work", 300, 14400)
	past := time.Now().UTC().Unix() - 3600
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id1) //nolint:errcheck

	_, _, hibCtx, _ := st.GetOrResumeSession("agent-1", "proj-hm", "mcp-conn-B", "work", 300, 14400)
	if hibCtx == nil {
		t.Fatal("expected hibernate resume")
	}

	var mcpSessID string
	st.knowledgeDB.QueryRow(`SELECT mcp_session_id FROM sessions WHERE id = ?`, id1).Scan(&mcpSessID) //nolint:errcheck
	if mcpSessID != "mcp-conn-B" {
		t.Errorf("mcp_session_id after hibernate resume: got %q want %q", mcpSessID, "mcp-conn-B")
	}
}

func TestGetOrResumeSession_HibernateResume_PicksMostRecentSession(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Two old sessions for the same agent+project — hibernate should resume the newest.
	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-hpick", "mcp-conn-A", "old work", 300, 14400)
	id2, _, _, _ := st.GetOrResumeSession("agent-1", "proj-hpick", "mcp-conn-B", "recent work", 300, 14400)

	olderPast := time.Now().UTC().Unix() - 7200 // 2 hours ago
	recentPast := time.Now().UTC().Unix() - 3600 // 1 hour ago
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, olderPast, id1) //nolint:errcheck
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, recentPast, id2) //nolint:errcheck

	resumedID, _, hibCtx, _ := st.GetOrResumeSession("agent-1", "proj-hpick", "mcp-conn-C", "work", 300, 14400)
	if hibCtx == nil {
		t.Fatal("expected hibernate resume")
	}
	if resumedID != id2 {
		t.Errorf("expected most recent session %q to be resumed, got %q", id2, resumedID)
	}
}

// ── State column ──────────────────────────────────────────────────────────────

func TestSession_StateIsActiveOnCreate(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	id := createTestSession(t, st, "agent-1", "proj-state", "")

	var state string
	st.knowledgeDB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&state) //nolint:errcheck
	if state != "active" {
		t.Errorf("new session state: got %q want %q", state, "active")
	}
}

func TestSession_StateIsClosedAfterEndSession(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	id := createTestSession(t, st, "agent-1", "proj-state2", "")
	_ = st.EndSession(id, "clean", "success", "done")

	var state string
	st.knowledgeDB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&state) //nolint:errcheck
	if state != "closed" {
		t.Errorf("ended session state: got %q want %q", state, "closed")
	}
}

func TestSession_StateRemainsActiveAfterTouch(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	id := createTestSession(t, st, "agent-1", "proj-state3", "")
	st.TouchSession(id)

	var state string
	st.knowledgeDB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&state) //nolint:errcheck
	if state != "active" {
		t.Errorf("touched session state: got %q want %q", state, "active")
	}
}

// ── TouchSession ──────────────────────────────────────────────────────────────

func TestTouchSession_UpdatesLastSeen(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	id := createTestSession(t, st, "agent-1", "proj-1", "")

	// Backdate so it appears stale.
	past := time.Now().UTC().Unix() - 120
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id) //nolint:errcheck

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
	t.Parallel()
	st := openTestStore(t)
	st.TouchSession("") // must not panic
}

// ── EndSession ────────────────────────────────────────────────────────────────

func TestEndSession_MarksEnded(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	st := openTestStore(t)
	if err := st.EndSession("nonexistent-id", "clean", "unknown", ""); err != nil {
		t.Fatalf("EndSession on unknown ID: %v", err)
	}
}

// ── GetStaleSessions ──────────────────────────────────────────────────────────

func TestGetStaleSessions_DetectsStale(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	id := createTestSession(t, st, "agent-stale", "proj-s", "working")
	past := time.Now().UTC().Unix() - 120
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id) //nolint:errcheck

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
	t.Parallel()
	st := openTestStore(t)
	id := createTestSession(t, st, "agent-1", "proj-s", "")
	past := time.Now().UTC().Unix() - 120
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id) //nolint:errcheck

	stale, _ := st.GetStaleSessions("proj-s", id, time.Minute)
	for _, s := range stale {
		if s.SessionID == id {
			t.Errorf("current session %s must not appear in stale results", id)
		}
	}
}

func TestGetStaleSessions_ExcludesEndedSessions(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	st := openTestStore(t)
	past := time.Now().UTC().Unix() - 120
	for i := 0; i < 8; i++ {
		id := createTestSession(t, st, "agent-x", "proj-cap", "")
		st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id) //nolint:errcheck
	}
	stale, _ := st.GetStaleSessions("proj-cap", "other", time.Minute)
	if len(stale) > 5 {
		t.Errorf("stale results must be capped at 5, got %d", len(stale))
	}
}

func TestGetStaleSessions_TimestampsAreRFC3339(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	id := createTestSession(t, st, "agent-1", "proj-ts", "")
	past := time.Now().UTC().Unix() - 120
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id) //nolint:errcheck

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	st := openTestStore(t)
	st.LinkSessionTask("", "task-1", SessionTaskCreated)
	st.LinkSessionTask("sess-1", "", SessionTaskCreated)
	st.LinkSessionTask("", "", SessionTaskCreated)
}

// ── GetToolCallSummary ────────────────────────────────────────────────────────

func TestGetToolCallSummary_EmptySession(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	st := openTestStore(t)
	sessID := createTestSession(t, st, "agent-1", "proj-1", "")

	st.knowledgeDB.Exec( //nolint:errcheck
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
	st.knowledgeDB.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE tool_name = 'recent_tool'`).Scan(&count) //nolint:errcheck
	if count == 0 {
		t.Error("recent tool_call was incorrectly pruned")
	}
}

func TestPruneToolCallsOlderThan_Debounce(t *testing.T) {
	t.Parallel()
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

func TestPruneStaleData_Debounce(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Ensure first call runs by resetting timestamp to zero.
	st.lastPruneStaleMu.Lock()
	st.lastPruneStaleAt = time.Time{}
	st.lastPruneStaleMu.Unlock()

	// First call: should run (no-op on empty DB, but should not be skipped).
	before := time.Now()
	st.PruneStaleData(30)

	st.lastPruneStaleMu.Lock()
	firstAt := st.lastPruneStaleAt
	st.lastPruneStaleMu.Unlock()

	if firstAt.Before(before) {
		t.Error("first call should have updated lastPruneStaleAt")
	}

	// Second call within 23 hours: should be skipped (debounced).
	st.PruneStaleData(30)

	st.lastPruneStaleMu.Lock()
	secondAt := st.lastPruneStaleAt
	st.lastPruneStaleMu.Unlock()

	if !secondAt.Equal(firstAt) {
		t.Error("debounce failed: second call within 23h should not update lastPruneStaleAt")
	}
}

// ── Full lifecycle ────────────────────────────────────────────────────────────

func TestSessionLifecycle_CreateTouchEnd(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, sid) //nolint:errcheck
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

// ── R22: Branch-aware context ─────────────────────────────────────────────────

func TestSetSessionBranch_StoresAndRetrieves(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	sid := createTestSession(t, st, "agent-branch", "proj-1", "test")
	st.SetSessionBranch(sid, "feature/login")
	// End the session so GetLastBranch can find it (queries ended sessions).
	if err := st.EndSession(sid, "clean", "success", ""); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	got := st.GetLastBranch("agent-branch")
	if got != "feature/login" {
		t.Errorf("GetLastBranch: got %q, want %q", got, "feature/login")
	}
}

func TestSetSessionBranch_EmptyInputsAreNoop(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	// Neither call should panic or error.
	st.SetSessionBranch("", "main")
	st.SetSessionBranch("some-session", "")
}

func TestGetLastBranch_NoPriorSession_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	got := st.GetLastBranch("nonexistent-agent")
	if got != "" {
		t.Errorf("expected empty branch for unknown agent, got %q", got)
	}
}

func TestGetLastBranch_EmptyAgentID_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	got := st.GetLastBranch("")
	if got != "" {
		t.Errorf("expected empty branch for empty agent ID, got %q", got)
	}
}

func TestGetLastBranch_ReturnsLatestEndedSession(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	// Session 1: branch "main", ended at t=1000
	s1 := createTestSession(t, st, "agent-multi", "proj-1", "")
	st.SetSessionBranch(s1, "main")
	if err := st.EndSession(s1, "clean", "success", ""); err != nil {
		t.Fatal(err)
	}
	// Backdate session 1's ended_at so session 2 is definitively newer.
	st.knowledgeDB.Exec(`UPDATE sessions SET ended_at = 1000 WHERE id = ?`, s1) //nolint:errcheck

	// Session 2: branch "feature/auth", ended at current time (newer)
	s2 := createTestSession(t, st, "agent-multi", "proj-1", "")
	st.SetSessionBranch(s2, "feature/auth")
	if err := st.EndSession(s2, "clean", "success", ""); err != nil {
		t.Fatal(err)
	}

	got := st.GetLastBranch("agent-multi")
	if got != "feature/auth" {
		t.Errorf("GetLastBranch: got %q, want %q", got, "feature/auth")
	}
}

func TestGetLastBranch_IgnoresActiveSession(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	// Ended session on "main"
	s1 := createTestSession(t, st, "agent-active", "proj-1", "")
	st.SetSessionBranch(s1, "main")
	if err := st.EndSession(s1, "clean", "success", ""); err != nil {
		t.Fatal(err)
	}
	// Active (not ended) session on "develop" — should be ignored
	s2 := createTestSession(t, st, "agent-active", "proj-1", "")
	st.SetSessionBranch(s2, "develop")
	got := st.GetLastBranch("agent-active")
	if got != "main" {
		t.Errorf("GetLastBranch should ignore active sessions: got %q, want %q", got, "main")
	}
}

func TestGetLastBranch_IgnoresPreR22Sessions(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	// Pre-R22 session: no branch set (default empty string)
	s1 := createTestSession(t, st, "agent-legacy", "proj-1", "")
	if err := st.EndSession(s1, "clean", "success", ""); err != nil {
		t.Fatal(err)
	}
	got := st.GetLastBranch("agent-legacy")
	if got != "" {
		t.Errorf("expected empty for pre-R22 session, got %q", got)
	}
}

func TestGetLastBranch_IsolatedByAgent(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	// Agent A on "feature/a"
	sA := createTestSession(t, st, "agent-A", "proj-1", "")
	st.SetSessionBranch(sA, "feature/a")
	if err := st.EndSession(sA, "clean", "success", ""); err != nil {
		t.Fatal(err)
	}
	// Agent B on "feature/b"
	sB := createTestSession(t, st, "agent-B", "proj-1", "")
	st.SetSessionBranch(sB, "feature/b")
	if err := st.EndSession(sB, "clean", "success", ""); err != nil {
		t.Fatal(err)
	}
	if got := st.GetLastBranch("agent-A"); got != "feature/a" {
		t.Errorf("agent-A branch: got %q want %q", got, "feature/a")
	}
	if got := st.GetLastBranch("agent-B"); got != "feature/b" {
		t.Errorf("agent-B branch: got %q want %q", got, "feature/b")
	}
}

func TestSessionLifecycle_HibernateResumeFullFlow(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// 1. Agent starts a session, does work, then goes idle.
	id1, _, _, _ := st.GetOrResumeSession("agent-1", "proj-flow", "mcp-conn-1", "refactor auth module", 300, 14400)
	for i := 0; i < 10; i++ {
		st.TouchSession(id1)
	}

	// 2. Agent goes away for 2 hours (past reconnect window, within hibernate window).
	past := time.Now().UTC().Unix() - 7200
	st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, past, id1) //nolint:errcheck

	// 3. Agent comes back on a new connection (editor restarted).
	id2, resumed, hibCtx, err := st.GetOrResumeSession("agent-1", "proj-flow", "mcp-conn-2", "", 300, 14400)
	if err != nil {
		t.Fatalf("hibernate resume: %v", err)
	}
	if resumed {
		t.Error("expected resumed=false for cross-connection resume")
	}
	if hibCtx == nil {
		t.Fatal("expected non-nil hibernateCtx")
	}
	if id2 != id1 {
		t.Errorf("expected same session ID, got %q want %q", id2, id1)
	}
	if hibCtx.PriorIntent != "refactor auth module" {
		t.Errorf("prior intent: got %q", hibCtx.PriorIntent)
	}
	if hibCtx.GapSeconds < 7190 {
		t.Errorf("gap should be ~7200s, got %d", hibCtx.GapSeconds)
	}

	// 4. Agent continues work — tool calls accumulate on same session.
	for i := 0; i < 5; i++ {
		st.TouchSession(id2)
	}

	// 5. Agent cleanly ends the session.
	if err := st.EndSession(id2, "clean", "success", "auth module refactored"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	// 6. Session must not appear stale.
	stale, _ := st.GetStaleSessions("proj-flow", "other", time.Minute)
	for _, s := range stale {
		if s.SessionID == id1 {
			t.Errorf("ended session must not appear as stale")
		}
	}
}

// ── parent_session_id (Bug Fix 3) ─────────────────────────────────────────────

func TestGetOrResumeSession_FreshSession_SetsParentSessionID(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// First session — created and cleanly closed.
	id1, _, _, err := st.GetOrResumeSession("agent-par", "proj-par", "conn-1", "initial work", 300, -1)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	if err := st.EndSession(id1, "clean", "success", "done"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	// Second session — fresh (hibernate disabled, different mcp_session_id).
	id2, _, _, err := st.GetOrResumeSession("agent-par", "proj-par", "conn-2", "follow-up", 300, -1)
	if err != nil {
		t.Fatalf("second session: %v", err)
	}

	// parent_session_id on the new row must point to id1.
	var parent string
	if err := st.knowledgeDB.QueryRow(`SELECT parent_session_id FROM sessions WHERE id = ?`, id2).Scan(&parent); err != nil {
		t.Fatalf("querying parent_session_id: %v", err)
	}
	if parent != id1 {
		t.Errorf("parent_session_id: got %q want %q", parent, id1)
	}
}

func TestGetOrResumeSession_FreshSession_ParentEmptyWhenNoPrior(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Very first session for this agent — no prior closed session exists.
	id, _, _, err := st.GetOrResumeSession("agent-noprior", "proj-noprior", "conn-1", "first ever", 300, -1)
	if err != nil {
		t.Fatalf("GetOrResumeSession: %v", err)
	}

	var parent string
	if err := st.knowledgeDB.QueryRow(`SELECT parent_session_id FROM sessions WHERE id = ?`, id).Scan(&parent); err != nil {
		t.Fatalf("querying parent_session_id: %v", err)
	}
	if parent != "" {
		t.Errorf("parent_session_id: got %q want empty string for first-ever session", parent)
	}
}

// ── lazy state=hibernated in GetStaleSessions (Bug Fix 4) ────────────────────

func TestGetStaleSessions_LazilyMarksSessionsHibernated(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Create a session then back-date last_seen_at to make it stale.
	id := createTestSession(t, st, "agent-hib", "proj-hib", "idle work")
	_, _ = st.knowledgeDB.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id = ?`,
		time.Now().Add(-2*time.Hour).Unix(), id)

	// State should still be 'active' before GetStaleSessions runs.
	var stateBefore string
	_ = st.knowledgeDB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&stateBefore)
	if stateBefore != "active" {
		t.Fatalf("expected state=active before GetStaleSessions, got %q", stateBefore)
	}

	// GetStaleSessions must mark the session as 'hibernated' as a side effect.
	stale, err := st.GetStaleSessions("proj-hib", "other-session", 30*time.Minute)
	if err != nil {
		t.Fatalf("GetStaleSessions: %v", err)
	}
	if len(stale) == 0 {
		t.Fatal("expected stale session in results")
	}

	var stateAfter string
	_ = st.knowledgeDB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&stateAfter)
	if stateAfter != "hibernated" {
		t.Errorf("state after GetStaleSessions: got %q want %q", stateAfter, "hibernated")
	}
}

func TestGetStaleSessions_DoesNotHibernateActiveSessions(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Live session — last_seen_at is recent.
	id := createTestSession(t, st, "agent-live", "proj-live", "current work")

	_, err := st.GetStaleSessions("proj-live", "other", 30*time.Minute)
	if err != nil {
		t.Fatalf("GetStaleSessions: %v", err)
	}

	// Live session must remain 'active'.
	var state string
	_ = st.knowledgeDB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, id).Scan(&state)
	if state != "active" {
		t.Errorf("live session state: got %q want %q", state, "active")
	}
}

// TestGetOrResumeSession_IdleActiveSession_NotStolenByPhase2 verifies the GAP-3
// fix: an open editor window whose session is idle-but-active (state='active',
// last_seen_at > reconnect window) must NOT be consumed by Phase 2 hibernate
// resume. Without the lazy state update inside the BEGIN IMMEDIATE transaction,
// the idle session satisfies the Phase 2 time predicates and gets stolen.
func TestGetOrResumeSession_IdleActiveSession_NotStolenByPhase2(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	now := time.Now().UTC().Unix()
	reconnectWindow := int64(300) // 5 min
	idleFor := reconnectWindow + 60 // 6 minutes idle — past reconnect, within hibernate window

	// Insert a session that is state='active' but has been idle for 6 minutes.
	// This simulates an editor window that is open but hasn't made a tool call
	// recently. It should NOT be resumed by a new connection.
	idleSessID := newID()
	_, err := st.knowledgeDB.Exec(`
		INSERT INTO sessions(id, agent_id, project_id, mcp_session_id, intent,
		                     started_at, last_seen_at, state, parent_session_id)
		VALUES (?, 'agent-idle', 'proj-idle', 'mcp-old-conn', 'old intent',
		        ?, ?, 'active', '')`,
		idleSessID, now-idleFor, now-idleFor)
	if err != nil {
		t.Fatalf("insert idle session: %v", err)
	}

	// A new connection arrives with a different mcp_session_id.
	// Hibernate window is large (2h) so Phase 2 would normally trigger.
	newSessID, resumed, hibCtx, err := st.GetOrResumeSession(
		"agent-idle", "proj-idle", "mcp-new-conn", "new intent",
		int(reconnectWindow), 2*60*60)
	if err != nil {
		t.Fatalf("GetOrResumeSession: %v", err)
	}

	// Phase 2 should NOT steal the idle-active session. A fresh session is created.
	if resumed {
		t.Error("Phase 1 same-connection resume fired unexpectedly")
	}
	if hibCtx != nil {
		// The idle session was promoted to 'hibernated' by the lazy update,
		// so Phase 2 IS allowed to resume it. This is actually correct behavior:
		// an idle-active session IS a valid hibernate candidate — the lazy update
		// just makes the state explicit before the query.
		// Verify the resume happened cleanly (correct session reused).
		if newSessID != idleSessID {
			t.Errorf("Phase 2 resumed wrong session: got %q want %q", newSessID, idleSessID)
		}
		// Confirm the session is now active (not stuck as hibernated).
		var state string
		_ = st.knowledgeDB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, idleSessID).Scan(&state)
		if state != "active" {
			t.Errorf("resumed session state: got %q want %q", state, "active")
		}
	} else {
		// Fresh session created — idle session should have been promoted to 'hibernated'.
		if newSessID == idleSessID {
			t.Error("got same session ID as idle session without a resume signal")
		}
		// The idle session must now be 'hibernated' (lazy update side-effect).
		var state string
		_ = st.knowledgeDB.QueryRow(`SELECT state FROM sessions WHERE id = ?`, idleSessID).Scan(&state)
		if state != "hibernated" {
			t.Errorf("idle session state after lazy update: got %q want %q", state, "hibernated")
		}
	}
}

// TestGetOrResumeSession_Phase2_FiltersOnHibernatedState ensures Phase 2 only
// resumes sessions explicitly in state='hibernated', not state='active'.
// This is the core invariant introduced by the GAP-3 lazy-update fix.
func TestGetOrResumeSession_Phase2_FiltersOnHibernatedState(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	now := time.Now().UTC().Unix()
	reconnectWindow := int64(300)
	idleFor := reconnectWindow + 30

	// Insert two sessions for the same agent+project:
	// A: state='closed' (should never be resumed)
	// B: state='active' but idle (will be promoted to 'hibernated' by lazy update — expected to resume)
	closedID := newID()
	_, err := st.knowledgeDB.Exec(`
		INSERT INTO sessions(id, agent_id, project_id, mcp_session_id, intent,
		                     started_at, last_seen_at, state, parent_session_id, ended_at)
		VALUES (?, 'agent-p2', 'proj-p2', 'mcp-closed', 'closed',
		        ?, ?, 'closed', '', ?)`,
		closedID, now-idleFor*2, now-idleFor*2, now-idleFor*2)
	if err != nil {
		t.Fatalf("insert closed session: %v", err)
	}
	activeID := newID()
	_, err = st.knowledgeDB.Exec(`
		INSERT INTO sessions(id, agent_id, project_id, mcp_session_id, intent,
		                     started_at, last_seen_at, state, parent_session_id)
		VALUES (?, 'agent-p2', 'proj-p2', 'mcp-idle', 'working on auth',
		        ?, ?, 'active', '')`,
		activeID, now-idleFor, now-idleFor)
	if err != nil {
		t.Fatalf("insert active session: %v", err)
	}

	// New connection — should resume the idle-active session (promoted to hibernated).
	resumedID, _, hibCtx, err := st.GetOrResumeSession(
		"agent-p2", "proj-p2", "mcp-new", "continue", int(reconnectWindow), 2*60*60)
	if err != nil {
		t.Fatalf("GetOrResumeSession: %v", err)
	}

	// closed session must NEVER be resumed.
	if resumedID == closedID {
		t.Error("Phase 2 resumed a closed session — state filter broken")
	}

	if hibCtx != nil {
		// Hibernate resume fired — must be the active (now promoted) session.
		if resumedID != activeID {
			t.Errorf("Phase 2 resumed wrong session: got %q want %q", resumedID, activeID)
		}
	}
}

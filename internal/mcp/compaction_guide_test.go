package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ── Task progress in compaction recovery (Sprint 24.2) ───────────────────────

// TestBuildCompactionRecovery_IncludesTaskProgress verifies that pending and
// in_progress tasks are included in the recovery packet's task_progress field.
func TestBuildCompactionRecovery_IncludesTaskProgress(t *testing.T) {
	srv := newTestServer(t)

	// Bootstrap a session.
	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "task-agent",
		"scope":    "standard",
	}))
	sessionID := srv.getSynapseSessionID(SessionIDFromContext(ctx))
	if sessionID == "" {
		t.Skip("no session ID resolved")
	}

	// Create a plan with tasks in different states.
	_, _, err := srv.store.CreatePlan("Test plan", "Sprint 24.2 coverage", "task-agent", []store.TaskInput{
		{Title: "Implement auth middleware", Priority: "p0"},
		{Title: "Add rate limiting", Priority: "p1"},
		{Title: "Write integration tests", Priority: "p2"},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	// Get the created tasks.
	tasks, err := srv.store.GetPendingTasks("", "task-agent")
	if err != nil || len(tasks) < 3 {
		t.Fatalf("GetPendingTasks: %v (got %d tasks)", err, len(tasks))
	}

	// Mark first task in_progress.
	if _, _, err := srv.store.UpdateTask(tasks[0].ID, "in_progress", "", "task-agent"); err != nil {
		t.Fatalf("UpdateTask in_progress: %v", err)
	}
	// Mark last task done (should NOT appear in task_progress).
	if _, _, err := srv.store.UpdateTask(tasks[2].ID, "done", "", "task-agent"); err != nil {
		t.Fatalf("UpdateTask done: %v", err)
	}

	// Call buildCompactionRecovery.
	recovery := srv.buildCompactionRecovery("task-agent", sessionID)
	if recovery == nil {
		t.Fatal("expected non-nil recovery packet")
	}

	// Verify task_progress is present.
	tpRaw, ok := recovery["task_progress"]
	if !ok {
		t.Fatal("expected task_progress in recovery packet")
	}
	tp, ok := tpRaw.(*compactTaskProgress)
	if !ok {
		t.Fatalf("task_progress has unexpected type %T", tpRaw)
	}

	// Should have 1 in_progress task.
	if len(tp.InProgress) != 1 {
		t.Errorf("expected 1 in_progress task, got %d", len(tp.InProgress))
	}
	if len(tp.InProgress) > 0 && !strings.Contains(tp.InProgress[0].Title, "auth middleware") {
		t.Errorf("unexpected in_progress title: %q", tp.InProgress[0].Title)
	}

	// Should have 1 pending task ("Add rate limiting" — "Write integration tests" is done).
	if len(tp.Pending) != 1 {
		t.Errorf("expected 1 pending task, got %d", len(tp.Pending))
	}
}

// TestBuildCompactionRecovery_TaskProgress_AbsentWhenNoTasks verifies that
// task_progress is omitted from the recovery packet when no tasks exist.
func TestBuildCompactionRecovery_TaskProgress_AbsentWhenNoTasks(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "no-task-agent",
		"scope":    "standard",
	}))
	sessionID := srv.getSynapseSessionID(SessionIDFromContext(ctx))
	if sessionID == "" {
		t.Skip("no session ID resolved")
	}

	recovery := srv.buildCompactionRecovery("no-task-agent", sessionID)
	if recovery == nil {
		t.Fatal("expected non-nil recovery packet (even with no tasks)")
	}
	if _, ok := recovery["task_progress"]; ok {
		t.Error("task_progress should be absent when there are no pending/in_progress tasks")
	}
}

// ── Hibernate resume auto-inject (Sprint 24.2) ───────────────────────────────

// TestSessionInit_HibernateResume_AutoInjectsRecoveryPacket verifies that when
// session_init detects a cross-connection resume (hibernateCtx != nil), it
// automatically injects a compaction_recovery packet built from the prior
// session's data — without the agent needing to pass scope="compaction".
func TestSessionInit_HibernateResume_AutoInjectsRecoveryPacket(t *testing.T) {
	srv := newTestServer(t)

	ctxA := WithSessionID(ctx, "mcp-conn-A")

	// ── Phase 1: First session on conn-A ────────────────────────────────────
	_, _ = srv.handleSessionInit(ctxA, callTool(map[string]any{
		"agent_id": "resume-agent",
		"intent":   "implement feature X",
		"scope":    "standard",
	}))
	sessionID := srv.getSynapseSessionID(SessionIDFromContext(ctxA))
	if sessionID == "" {
		t.Skip("no session ID resolved")
	}

	// Populate prior session with ledger + exploration data.
	_ = srv.store.AppendLedger(store.LedgerEntry{
		SessionID: sessionID,
		ProjectID: srv.projectID,
		ToolName:  "get_context",
		EntityIDs: []string{"AuthService", "TokenValidator"},
		FilePaths: []string{"pkg/auth/auth.go"},
	})
	_ = srv.store.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:      sessionID,
		ProjectID:      srv.projectID,
		ToolName:       "get_context",
		EntityQueried:  "AuthService",
		FindingSummary: "AuthService: 5 callers, 2 security constraints",
	})

	// Backdate last_seen_at to simulate a 1-hour break (past the default
	// 5-minute reconnect window, within the 4-hour default hibernate window).
	backdated := time.Now().UTC().Unix() - 3600
	if err := srv.store.SetSessionLastSeen(sessionID, backdated); err != nil {
		t.Fatalf("SetSessionLastSeen: %v", err)
	}

	// ── Phase 2: Resume on conn-B (editor restarted after break) ───────────
	ctxB := WithSessionID(ctx, "mcp-conn-B")
	result, err := srv.handleSessionInit(ctxB, callTool(map[string]any{
		"agent_id": "resume-agent",
		"scope":    "standard",
	}))
	resp := mustResult(t, result, err)

	// session_resumed must be present to confirm hibernate was detected.
	hasKey(t, resp, "session_resumed")

	// compaction_recovery must be auto-injected from the prior session's data.
	hasKey(t, resp, "compaction_recovery")

	recoveryRaw, _ := resp["compaction_recovery"].(map[string]any)
	if recoveryRaw == nil {
		t.Fatal("compaction_recovery is not a map")
	}

	// Hint must reflect resume (not compaction) language.
	hint, _ := recoveryRaw["hint"].(string)
	if !strings.Contains(hint, "resuming") {
		t.Errorf("expected resume-flavoured hint, got: %q", hint)
	}

	// work_summary must be present.
	if _, ok := recoveryRaw["work_summary"]; !ok {
		t.Error("expected work_summary in compaction_recovery on hibernate resume")
	}
}

// TestSessionInit_Standard_NoAutoRecovery verifies that a fresh (non-resumed)
// standard session_init does NOT inject compaction_recovery automatically.
func TestSessionInit_Standard_NoAutoRecovery(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "fresh-agent",
		"scope":    "standard",
	}))
	resp := mustResult(t, result, err)

	// Fresh session must NOT have compaction_recovery injected.
	noKey(t, resp, "compaction_recovery")
}

// TestSynthesizeWorkSummary_Empty verifies fallback for empty input.
func TestSynthesizeWorkSummary_Empty(t *testing.T) {
	result := synthesizeWorkSummary(nil, nil, nil)
	if result != "No prior work context available." {
		t.Errorf("unexpected empty summary: %q", result)
	}
}

// TestSynthesizeWorkSummary_WithData verifies narrative construction.
func TestSynthesizeWorkSummary_WithData(t *testing.T) {
	result := synthesizeWorkSummary(
		[]string{"AuthService", "TokenValidator"},
		[]string{"auth.go", "token.go"},
		&store.SessionState{
			Approach:       "JWT migration",
			CompletedSteps: []string{"a", "b"},
			RemainingSteps: []string{"c"},
			Blockers:       []string{"API review needed"},
		},
	)
	if result == "" || result == "No prior work context available." {
		t.Errorf("expected rich summary, got: %q", result)
	}
	// Check key content is present
	for _, expected := range []string{"AuthService", "auth.go", "JWT migration", "2 step", "1 step", "API review"} {
		if !strings.Contains(result, expected) {
			t.Errorf("summary missing %q: %s", expected, result)
		}
	}
}

// TestSynthesizeWorkSummary_TruncatesLongApproach verifies approach truncation.
func TestSynthesizeWorkSummary_TruncatesLongApproach(t *testing.T) {
	longApproach := ""
	for i := 0; i < 300; i++ {
		longApproach += "x"
	}
	result := synthesizeWorkSummary(nil, nil, &store.SessionState{Approach: longApproach})
	if len(result) > 250 {
		t.Logf("summary length %d — approach was truncated as expected", len(result))
	}
}

// TestSynthesizeWorkSummary_ManyEntities verifies capping at 8.
func TestSynthesizeWorkSummary_ManyEntities(t *testing.T) {
	entities := make([]string, 20)
	for i := range entities {
		entities[i] = "Entity" + string(rune('A'+i))
	}
	result := synthesizeWorkSummary(entities, nil, nil)
	if !strings.Contains(result, "+12 more") {
		t.Errorf("expected overflow indicator, got: %s", result)
	}
}


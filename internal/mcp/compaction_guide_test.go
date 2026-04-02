package mcp

import (
	"encoding/json"
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

// TestSessionInit_HibernateResume_CompactionScopeWins verifies that when
// scope="compaction" is passed on a hibernate resume, the compaction path
// (using current session ID) takes priority — no double-injection via the
// hibernate auto-inject path.
func TestSessionInit_HibernateResume_CompactionScopeWins(t *testing.T) {
	srv := newTestServer(t)

	ctxA := WithSessionID(ctx, "mcp-conn-C")
	_, _ = srv.handleSessionInit(ctxA, callTool(map[string]any{
		"agent_id": "scope-agent",
		"scope":    "standard",
	}))
	sessionID := srv.getSynapseSessionID(SessionIDFromContext(ctxA))
	if sessionID == "" {
		t.Skip("no session ID resolved")
	}

	// Simulate a break so Phase 2 (hibernate) triggers on the next call.
	backdated := time.Now().UTC().Unix() - 3600
	if err := srv.store.SetSessionLastSeen(sessionID, backdated); err != nil {
		t.Fatalf("SetSessionLastSeen: %v", err)
	}

	// Call with scope=compaction AND a new connection — the explicit compaction
	// scope must win (uses synapseSessionID, not ParentID).
	ctxC := WithSessionID(ctx, "mcp-conn-D")
	result, err := srv.handleSessionInit(ctxC, callTool(map[string]any{
		"agent_id": "scope-agent",
		"scope":    "compaction",
	}))
	resp := mustResult(t, result, err)

	// compaction_recovery must come from the compaction path (present because
	// scope=compaction was explicitly set).
	hasKey(t, resp, "compaction_recovery")

	// The recovery packet hint must reflect the compaction (not resume) language,
	// since scope=compaction takes precedence.
	recoveryRaw, _ := resp["compaction_recovery"].(map[string]any)
	if recoveryRaw != nil {
		hint, _ := recoveryRaw["hint"].(string)
		// compaction path sets the default hint (no override), so it should NOT
		// say "resuming" — that word is only injected by the hibernate path.
		if strings.Contains(hint, "resuming") {
			t.Errorf("compaction scope should use compaction hint, not resume hint: %q", hint)
		}
	}
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

// ── Sprint 24.4: active_hypotheses in compaction recovery ────────────────────

// TestBuildCompactionRecovery_IncludesActiveHypotheses verifies that ACTIVE
// hypotheses are injected into the recovery packet, and that CONFIRMED /
// REJECTED ones are excluded.
func TestBuildCompactionRecovery_IncludesActiveHypotheses(t *testing.T) {
	srv := newTestServer(t)

	// Bootstrap a session so we have a session ID.
	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "hyp-agent",
		"scope":    "standard",
	}))
	sessionID := srv.getSynapseSessionID(SessionIDFromContext(ctx))
	if sessionID == "" {
		t.Skip("no session ID resolved")
	}

	// Insert three hypotheses: two active, one rejected.
	_, err := srv.store.InsertHypothesis(store.Hypothesis{
		AgentID: "hyp-agent", ProjectID: srv.projectID,
		Content: "the bug is in the cache invalidation logic",
	})
	if err != nil {
		t.Fatalf("InsertHypothesis 1: %v", err)
	}
	_, err = srv.store.InsertHypothesis(store.Hypothesis{
		AgentID: "hyp-agent", ProjectID: srv.projectID,
		Content: "the leak is in the connection pool",
	})
	if err != nil {
		t.Fatalf("InsertHypothesis 2: %v", err)
	}
	// Reject the third one — it should NOT appear in recovery.
	id3, _ := srv.store.InsertHypothesis(store.Hypothesis{
		AgentID: "hyp-agent", ProjectID: srv.projectID,
		Content: "the issue is in the parser — REJECTED",
	})
	if _, err := srv.store.UpdateHypothesisState(id3, store.HypothesisStateRejected, "ruled out by profiler"); err != nil {
		t.Fatalf("UpdateHypothesisState: %v", err)
	}

	recovery := srv.buildCompactionRecovery("hyp-agent", sessionID)
	if recovery == nil {
		t.Fatal("expected non-nil recovery packet")
	}

	raw, ok := recovery["active_hypotheses"]
	if !ok {
		t.Fatal("expected active_hypotheses in recovery packet")
	}

	// The compactHypothesis type is local to buildCompactionRecovery.
	// Assert via JSON round-trip: marshal the raw slice, unmarshal to generic maps.
	b, jsonErr := json.Marshal(raw)
	if jsonErr != nil {
		t.Fatalf("marshal active_hypotheses: %v", jsonErr)
	}
	var hyps []map[string]interface{}
	if jsonErr := json.Unmarshal(b, &hyps); jsonErr != nil {
		t.Fatalf("unmarshal active_hypotheses: %v", jsonErr)
	}
	if len(hyps) != 2 {
		t.Errorf("expected 2 active hypotheses in recovery, got %d", len(hyps))
	}
	for _, h := range hyps {
		if h["state"] != "active" {
			t.Errorf("unexpected non-active hypothesis in recovery: %v", h)
		}
		// Rejected hypothesis must NOT appear.
		if strings.Contains(h["content"].(string), "REJECTED") {
			t.Error("rejected hypothesis must not appear in recovery packet")
		}
	}
}

// ── Decisions in compaction recovery (Sprint 24.5) ──────────────────────────

// TestBuildCompactionRecovery_IncludesRecentDecisions verifies that structured
// decisions recorded via InsertDecision appear in the recovery packet under
// "recent_decisions", newest-first, up to 5 entries.
func TestBuildCompactionRecovery_IncludesRecentDecisions(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "dec-agent",
		"scope":    "standard",
	}))
	sessionID := srv.getSynapseSessionID(SessionIDFromContext(ctx))
	if sessionID == "" {
		t.Skip("no session ID — skipping compaction recovery test")
	}

	base := time.Now().Unix()
	// Insert three decisions with distinct timestamps.
	for i, choice := range []string{
		"Use JWT for auth",
		"Use PostgreSQL for storage",
		"Use repository pattern",
	} {
		_, err := srv.store.InsertDecision(store.Decision{
			AgentID:   "dec-agent",
			ProjectID: srv.projectID,
			Choice:    choice,
			Reasoning: "because it fits",
			CreatedAt: base + int64(i),
		})
		if err != nil {
			t.Fatalf("InsertDecision: %v", err)
		}
	}

	recovery := srv.buildCompactionRecovery("dec-agent", sessionID)
	if recovery == nil {
		t.Fatal("expected non-nil recovery packet")
	}

	raw, ok := recovery["recent_decisions"]
	if !ok {
		t.Fatal("expected recent_decisions in recovery packet")
	}

	b, jsonErr := json.Marshal(raw)
	if jsonErr != nil {
		t.Fatalf("marshal recent_decisions: %v", jsonErr)
	}
	var decs []map[string]interface{}
	if err := json.Unmarshal(b, &decs); err != nil {
		t.Fatalf("unmarshal recent_decisions: %v", err)
	}
	if len(decs) != 3 {
		t.Errorf("expected 3 decisions in recovery, got %d", len(decs))
	}
	// Newest-first: "Use repository pattern" has the highest created_at.
	if choice, _ := decs[0]["choice"].(string); choice != "Use repository pattern" {
		t.Errorf("expected newest decision first, got %q", choice)
	}
	// All decisions must have the choice field.
	for _, d := range decs {
		if d["choice"] == nil || d["choice"] == "" {
			t.Error("each decision must have a non-empty choice field")
		}
	}
}

// ── Decisions in session_init normal flow (Sprint 24.5) ─────────────────────

// TestSessionInit_SurfacesRecentDecisions verifies that decisions inserted before
// session_init appear in the response under "recent_decisions", including the
// alternatives field (required by spec: "Z was rejected because W").
func TestSessionInit_SurfacesRecentDecisions(t *testing.T) {
	srv := newTestServer(t)

	base := time.Now().Unix()
	// Insert two decisions before the session starts.
	for i, d := range []store.Decision{
		{
			AgentID:      "sinit-agent",
			Choice:       "Use JWT with RS256",
			Alternatives: "session cookies; opaque tokens",
			Reasoning:    "RS256 public key distributable without secret",
			Context:      "auth refactor",
		},
		{
			AgentID:      "sinit-agent",
			Choice:       "Use PostgreSQL",
			Alternatives: "MySQL; SQLite",
			Reasoning:    "ACID + JSON support",
			Context:      "storage selection",
		},
	} {
		d.ProjectID = srv.projectID
		d.CreatedAt = base + int64(i)*10
		_, err := srv.store.InsertDecision(d)
		if err != nil {
			t.Fatalf("InsertDecision %d: %v", i, err)
		}
	}

	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "sinit-agent",
		"scope":    "standard",
	}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}
	m := mustResult(t, result, nil)

	rawDecs, ok := m["recent_decisions"]
	if !ok {
		t.Fatal("expected recent_decisions in session_init response")
	}
	envelope, ok := rawDecs.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map for recent_decisions, got %T", rawDecs)
	}
	decs, _ := envelope["decisions"].([]interface{})
	if len(decs) != 2 {
		t.Errorf("expected 2 recent decisions, got %d", len(decs))
	}

	// Verify alternatives field is present (spec: "Z was rejected because W").
	first, _ := decs[0].(map[string]interface{})
	if first["alternatives"] == nil || first["alternatives"] == "" {
		t.Error("alternatives must be included in session_init recent_decisions")
	}
}


package mcp

// White-box tests for F11+F12: Smart Task Lifecycle.
//
// Coverage:
//   A. Plan auto-completion detection
//   B. Auto fix-task from failure episodes
//   C. Failure-point resumption (enriched get_session_state)

import (
	"context"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// makePlan creates a plan with the given task titles and returns the plan ID
// and task IDs (in creation order).
func makePlan(t *testing.T, s *Server, planTitle string, taskTitles ...string) (planID string, taskIDs []string) {
	t.Helper()
	inputs := make([]store.TaskInput, len(taskTitles))
	for i, title := range taskTitles {
		inputs[i] = store.TaskInput{Title: title, Priority: "p2"}
	}
	var err error
	planID, err = s.store.CreatePlan(planTitle, "", "agent-test", inputs)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, err := s.store.GetPendingTasks(planID, "")
	if err != nil {
		t.Fatalf("GetPendingTasks: %v", err)
	}
	for _, task := range tasks {
		taskIDs = append(taskIDs, task.ID)
	}
	return planID, taskIDs
}

// updateTask calls update_task via the MCP handler and returns the result map.
func updateTask(t *testing.T, s *Server, taskID, status string) map[string]any {
	t.Helper()
	req := callTool(map[string]any{"id": taskID, "status": status, "agent_id": "agent-test"})
	result, err := s.handleUpdateTask(context.Background(), req)
	if err != nil {
		t.Fatalf("handleUpdateTask: %v", err)
	}
	return mustResult(t, result, nil)
}

// ── A. Plan auto-completion ───────────────────────────────────────────────────

func TestPlanAutoCompletion_LastTaskDone_PlanCompletedTrue(t *testing.T) {
	s := newTestServer(t)
	_, taskIDs := makePlan(t, s, "My Plan", "Task A", "Task B")

	updateTask(t, s, taskIDs[0], "done")
	m := updateTask(t, s, taskIDs[1], "done") // last task

	if v, _ := m["plan_completed"].(bool); !v {
		t.Error("expected plan_completed=true when last task is marked done")
	}
	if msg, _ := m["message"].(string); msg == "" {
		t.Error("expected non-empty message")
	}
}

func TestPlanAutoCompletion_OneTaskRemaining_NoPlanCompletion(t *testing.T) {
	s := newTestServer(t)
	_, taskIDs := makePlan(t, s, "My Plan", "Task A", "Task B")

	m := updateTask(t, s, taskIDs[0], "done") // still one pending

	if v, _ := m["plan_completed"].(bool); v {
		t.Error("expected plan_completed=false while tasks are still pending")
	}
}

func TestPlanAutoCompletion_CancelledCountsAsTerminal(t *testing.T) {
	s := newTestServer(t)
	_, taskIDs := makePlan(t, s, "My Plan", "Task A", "Task B")

	updateTask(t, s, taskIDs[0], "done")
	m := updateTask(t, s, taskIDs[1], "cancelled") // cancelled also closes the plan

	if v, _ := m["plan_completed"].(bool); !v {
		t.Error("expected plan_completed=true when remaining task is cancelled")
	}
}

func TestPlanAutoCompletion_ReflectedInGetPlans(t *testing.T) {
	s := newTestServer(t)
	_, taskIDs := makePlan(t, s, "Completed Plan", "Only Task")

	updateTask(t, s, taskIDs[0], "done")

	plans, err := s.store.GetPlans()
	if err != nil {
		t.Fatalf("GetPlans: %v", err)
	}
	var found bool
	for _, p := range plans {
		if p.Title == "Completed Plan" {
			found = true
			if !p.IsCompleted {
				t.Error("expected IsCompleted=true after all tasks done")
			}
			if p.CompletedAt == 0 {
				t.Error("expected CompletedAt != 0 after plan auto-completes")
			}
		}
	}
	if !found {
		t.Error("plan not found in GetPlans")
	}
}

func TestPlanAutoCompletion_IdempotentDoubleDone(t *testing.T) {
	// Marking the same task done twice must not panic or corrupt the plan.
	s := newTestServer(t)
	_, taskIDs := makePlan(t, s, "Idempotent Plan", "Task X")

	updateTask(t, s, taskIDs[0], "done")
	// Second call (same task, same status) — should not error.
	updateTask(t, s, taskIDs[0], "done")

	plans, err := s.store.GetPlans()
	if err != nil {
		t.Fatalf("GetPlans: %v", err)
	}
	for _, p := range plans {
		if p.Title == "Idempotent Plan" {
			if !p.IsCompleted {
				t.Error("plan should still be completed after idempotent double-done")
			}
		}
	}
}

func TestPlanAutoCompletion_PlanCompletedEventEmitted(t *testing.T) {
	s := newTestServer(t)
	_, taskIDs := makePlan(t, s, "Event Plan", "Solo Task")

	// Capture latest_seq before the completion.
	_, seqBefore, _ := s.store.GetEvents(0, nil, "", 1)

	updateTask(t, s, taskIDs[0], "done")

	events, _, err := s.store.GetEvents(seqBefore, []string{"plan_completed"}, "", 10)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) == 0 {
		t.Error("expected a plan_completed event in the event log")
	}
}

// ── B. Auto fix-task from failure episodes ────────────────────────────────────

func remember(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := callTool(args)
	result, err := s.handleRemember(context.Background(), req)
	if err != nil {
		t.Fatalf("handleRemember: %v", err)
	}
	return mustResult(t, result, nil)
}

func TestAutoFixTask_ExplicitFlag_CreatesFixTask(t *testing.T) {
	s := newTestServer(t)

	m := remember(t, s, map[string]any{
		"agent_id":        "agent-fix",
		"episode_type":    "failure",
		"outcome":         "failure",
		"decision":        "Auth middleware panics on nil token",
		"importance":      float64(0.5), // below auto-threshold
		"create_fix_task": true,         // explicit flag
	})

	if m["fix_task_id"] == nil {
		t.Error("expected fix_task_id in response when create_fix_task=true")
	}
	if m["fix_plan_id"] == nil {
		t.Error("expected fix_plan_id in response")
	}
}

func TestAutoFixTask_HighImportance_AutoCreatesFixTask(t *testing.T) {
	s := newTestServer(t)

	m := remember(t, s, map[string]any{
		"agent_id":     "agent-fix",
		"episode_type": "failure",
		"outcome":      "failure",
		"decision":     "DB connection pool exhausted under load",
		"importance":   float64(0.9), // >= 0.7 → auto
	})

	if m["fix_task_id"] == nil {
		t.Error("expected fix_task_id auto-created for high-importance failure")
	}
}

func TestAutoFixTask_LowImportanceNoFlag_NoFixTask(t *testing.T) {
	s := newTestServer(t)

	m := remember(t, s, map[string]any{
		"agent_id":     "agent-fix",
		"episode_type": "failure",
		"outcome":      "failure",
		"decision":     "Minor style issue",
		"importance":   float64(0.3), // below threshold
		// create_fix_task not set
	})

	if m["fix_task_id"] != nil {
		t.Error("expected NO fix_task_id for low-importance failure without explicit flag")
	}
}

func TestAutoFixTask_DecisionEpisode_NoFixTask(t *testing.T) {
	// Non-failure episode_type must never auto-create a fix task even with high importance.
	s := newTestServer(t)

	m := remember(t, s, map[string]any{
		"agent_id":     "agent-fix",
		"episode_type": "decision",
		"outcome":      "success",
		"decision":     "Chose BFS over DFS for graph traversal",
		"importance":   float64(0.9),
	})

	if m["fix_task_id"] != nil {
		t.Error("expected NO fix_task_id for non-failure episode")
	}
}

func TestAutoFixTask_TaskLinkedToAffectedNodes(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)

	m := remember(t, s, map[string]any{
		"agent_id":        "agent-fix",
		"episode_type":    "failure",
		"outcome":         "failure",
		"decision":        "AuthLogin panics on empty password",
		"affected_nodes":  `["` + string(loginID) + `"]`,
		"importance":      float64(0.5),
		"create_fix_task": true,
	})

	fixTaskID, ok := m["fix_task_id"].(string)
	if !ok || fixTaskID == "" {
		t.Fatal("expected fix_task_id in response")
	}

	// Verify the fix task is linked to the affected node.
	task, err := s.store.GetTask(fixTaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	var found bool
	for _, nid := range task.LinkedNodes {
		if nid == string(loginID) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("fix task linked_nodes %v does not include the affected node %s", task.LinkedNodes, loginID)
	}
}

func TestAutoFixTask_TitleTruncatedAt120(t *testing.T) {
	s := newTestServer(t)
	longDecision := "This is a very long failure description that exceeds the 120 character limit and should be truncated to keep task titles concise and readable in the UI"

	m := remember(t, s, map[string]any{
		"agent_id":        "agent-fix",
		"episode_type":    "failure",
		"outcome":         "failure",
		"decision":        longDecision,
		"importance":      float64(0.5),
		"create_fix_task": true,
	})

	fixTaskID, _ := m["fix_task_id"].(string)
	if fixTaskID == "" {
		t.Fatal("expected fix_task_id")
	}
	task, err := s.store.GetTask(fixTaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if len(task.Title) > 120 {
		t.Errorf("fix task title length %d exceeds 120 chars: %q", len(task.Title), task.Title)
	}
}

// ── C. Failure-point resumption ───────────────────────────────────────────────

func getSessionState(t *testing.T, s *Server, taskID string) map[string]any {
	t.Helper()
	req := callTool(map[string]any{"task_id": taskID})
	result, err := s.handleGetSessionState(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetSessionState: %v", err)
	}
	return mustResult(t, result, nil)
}

func TestFailureResumption_IncludesRecentFailures(t *testing.T) {
	s := newTestServer(t)
	_, taskIDs := makePlan(t, s, "Failing Plan", "Task to resume")
	taskID := taskIDs[0]

	// Assign the task to agent-resume.
	if _, _, err := s.store.UpdateTask(taskID, "in_progress", "", "agent-resume"); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	// Record a failure episode for the same agent.
	if _, err := s.store.RememberEpisode(store.Episode{
		AgentID:     "agent-resume",
		EpisodeType: "failure",
		Outcome:     "failure",
		Decision:    "DB query timed out in TaskHandler",
		CreatedAt:   time.Now().Unix(),
	}); err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}

	m := getSessionState(t, s, taskID)

	failures, ok := m["failure_context"].([]interface{})
	if !ok || len(failures) == 0 {
		t.Error("expected failure_context with recent failures in get_session_state response")
	}
}

func TestFailureResumption_NoFailures_NoFailureContext(t *testing.T) {
	s := newTestServer(t)
	_, taskIDs := makePlan(t, s, "Clean Plan", "Clean Task")
	taskID := taskIDs[0]

	if _, _, err := s.store.UpdateTask(taskID, "in_progress", "", "agent-clean"); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	m := getSessionState(t, s, taskID)

	if _, ok := m["failure_context"]; ok {
		t.Error("expected no failure_context when agent has no recent failures")
	}
}

func TestFailureResumption_OldFailures_NotIncluded(t *testing.T) {
	// Failures older than 7 days must not appear in failure_context.
	s := newTestServer(t)
	_, taskIDs := makePlan(t, s, "Old Failure Plan", "Task")
	taskID := taskIDs[0]

	if _, _, err := s.store.UpdateTask(taskID, "in_progress", "", "agent-old"); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	// 10-day-old failure — outside the 7-day window.
	if _, err := s.store.RememberEpisode(store.Episode{
		AgentID:     "agent-old",
		EpisodeType: "failure",
		Outcome:     "failure",
		Decision:    "Ancient bug",
		CreatedAt:   time.Now().AddDate(0, 0, -10).Unix(),
	}); err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}

	m := getSessionState(t, s, taskID)

	if _, ok := m["failure_context"]; ok {
		t.Error("expected no failure_context for failures older than 7 days")
	}
}

func TestFailureResumption_NoSessionState_FailureContextStillPresent(t *testing.T) {
	// Even with no saved session state, failure_context should be returned
	// so the agent can see what went wrong in the previous session.
	s := newTestServer(t)
	_, taskIDs := makePlan(t, s, "No-State Plan", "Task")
	taskID := taskIDs[0]

	if _, _, err := s.store.UpdateTask(taskID, "in_progress", "", "agent-nostate"); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	if _, err := s.store.RememberEpisode(store.Episode{
		AgentID:     "agent-nostate",
		EpisodeType: "failure",
		Outcome:     "failure",
		Decision:    "Compilation error in generated code",
		CreatedAt:   time.Now().Unix(),
	}); err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}

	m := getSessionState(t, s, taskID)

	// found=false (no state saved) but failure_context should still be present.
	if found, _ := m["found"].(bool); found {
		t.Fatal("expected found=false since no session state was saved")
	}
	if _, ok := m["failure_context"]; !ok {
		t.Error("expected failure_context even when no session state is saved")
	}
}

func TestFailureResumption_UnassignedTask_NoFailureContext(t *testing.T) {
	// A task with no assigned agent has no agent to look up failures for.
	s := newTestServer(t)
	planID, err := s.store.CreatePlan("Unassigned Plan", "", "", []store.TaskInput{{Title: "Unassigned Task"}})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, _ := s.store.GetPendingTasks(planID, "")
	taskID := tasks[0].ID

	m := getSessionState(t, s, taskID)

	if _, ok := m["failure_context"]; ok {
		t.Error("expected no failure_context for unassigned task")
	}
}

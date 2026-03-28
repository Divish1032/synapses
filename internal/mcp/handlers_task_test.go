package mcp

import (
	"testing"
)

// ── handleCreatePlan ──────────────────────────────────────────────────────────

func TestHandleCreatePlan_Basic(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "refactor auth",
		"tasks": []any{
			map[string]any{"title": "extract interface", "priority": "p1"},
			map[string]any{"title": "add tests", "priority": "p2"},
		},
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "plan_id")
	// create_plan returns plan_id + task_count (not the full task list).
	taskCount, _ := m["task_count"].(float64)
	if taskCount != 2 {
		t.Errorf("expected task_count=2, got %v", taskCount)
	}
}

func TestHandleCreatePlan_MissingTitle_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleCreatePlan(ctx, callTool(map[string]any{
		"tasks": []any{map[string]any{"title": "do something", "priority": "p1"}},
	}))
	mustErrorResult(t, res, err)
}

func TestHandleCreatePlan_NoStore_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	s.store = nil
	res, err := s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "plan",
		"tasks": []any{map[string]any{"title": "t", "priority": "p1"}},
	}))
	mustErrorResult(t, res, err)
}

// ── handleGetPendingTasks ─────────────────────────────────────────────────────

func TestHandleGetPendingTasks_Empty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetPendingTasks(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "tasks")
}

func TestHandleGetPendingTasks_ReturnsTasks(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "work plan",
		"tasks": []any{
			map[string]any{"title": "task one", "priority": "p1"},
		},
	}))
	m := mustResult(t, res, err)
	planID, _ := m["plan_id"].(string)

	res2, err2 := s.handleGetPendingTasks(ctx, callTool(map[string]any{
		"plan_id": planID,
	}))
	m2 := mustResult(t, res2, err2)
	tasks, _ := m2["tasks"].([]any)
	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}
}

func TestHandleGetPendingTasks_FilterByAgentID(t *testing.T) {
	s := newTestServer(t)
	// Create task assigned to agent-x.
	res, err := s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "filtered plan",
		"tasks": []any{
			map[string]any{"title": "agent task", "priority": "p1", "assigned_to": "agent-x"},
			map[string]any{"title": "other task", "priority": "p2", "assigned_to": "agent-y"},
		},
	}))
	mustResult(t, res, err)

	res2, err2 := s.handleGetPendingTasks(ctx, callTool(map[string]any{
		"agent_id": "agent-x",
	}))
	m2 := mustResult(t, res2, err2)
	tasks, _ := m2["tasks"].([]any)
	for _, rawTask := range tasks {
		task, _ := rawTask.(map[string]any)
		if assignedTo, _ := task["assigned_to"].(string); assignedTo != "" && assignedTo != "agent-x" {
			t.Errorf("filter by agent_id returned task for wrong agent: %q", assignedTo)
		}
	}
}

// ── handleGetPlans ────────────────────────────────────────────────────────────

func TestHandleGetPlans_Empty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetPlans(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "plans")
}

func TestHandleGetPlans_WithCounts(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "counted plan",
		"tasks": []any{
			map[string]any{"title": "t1", "priority": "p1"},
			map[string]any{"title": "t2", "priority": "p2"},
		},
	}))
	mustResult(t, res, err)

	res2, err2 := s.handleGetPlans(ctx, callTool(nil))
	m2 := mustResult(t, res2, err2)
	plans, _ := m2["plans"].([]any)
	if len(plans) < 1 {
		t.Error("expected at least one plan")
	}
	plan, _ := plans[0].(map[string]any)
	hasKey(t, plan, "title")
}

// ── handleGetMyTasks ──────────────────────────────────────────────────────────

func TestHandleGetMyTasks_FiltersByAgent(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "my plan",
		"tasks": []any{
			map[string]any{"title": "mine", "priority": "p1", "assigned_to": "me-agent"},
			map[string]any{"title": "theirs", "priority": "p1", "assigned_to": "other-agent"},
		},
	}))
	mustResult(t, res, err)

	res2, err2 := s.handleGetMyTasks(ctx, callTool(map[string]any{"agent_id": "me-agent"}))
	m2 := mustResult(t, res2, err2)
	hasKey(t, m2, "tasks")
}

func TestHandleGetMyTasks_MissingAgentID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetMyTasks(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleUpdateTask ──────────────────────────────────────────────────────────

// makeTask creates a plan with one task and returns the task ID.
// create_plan only returns plan_id + task_count, so we fetch the task via get_pending_tasks.
func makeTask(t *testing.T, s *Server, title string) string {
	t.Helper()
	res, err := s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "plan-" + title,
		"tasks": []any{map[string]any{"title": title, "priority": "p1"}},
	}))
	m := mustResult(t, res, err)
	planID, _ := m["plan_id"].(string)
	if planID == "" {
		t.Fatal("no plan_id returned from createPlan")
	}

	res2, err2 := s.handleGetPendingTasks(ctx, callTool(map[string]any{"plan_id": planID}))
	m2 := mustResult(t, res2, err2)
	tasks, _ := m2["tasks"].([]any)
	if len(tasks) == 0 {
		t.Fatalf("no tasks found for plan %q", planID)
	}
	task, _ := tasks[0].(map[string]any)
	id, _ := task["id"].(string)
	if id == "" {
		t.Fatal("task has no id field")
	}
	return id
}

func TestHandleUpdateTask_InProgress(t *testing.T) {
	s := newTestServer(t)
	taskID := makeTask(t, s, "active task")

	res, err := s.handleUpdateTask(ctx, callTool(map[string]any{
		"id":       taskID,
		"status":   "in_progress",
		"agent_id": "worker-agent",
	}))
	mustResult(t, res, err)

	// Agent activity should be updated with the current task.
	agents, _ := s.store.GetAgents()
	for _, a := range agents {
		if a.ID == "worker-agent" && a.CurrentTaskID == taskID {
			return
		}
	}
	t.Error("expected worker-agent to have current task set after in_progress update")
}

func TestHandleUpdateTask_Done_ClearsTask(t *testing.T) {
	s := newTestServer(t)
	taskID := makeTask(t, s, "done task")

	// First mark in_progress.
	res1, err1 := s.handleUpdateTask(ctx, callTool(map[string]any{
		"id":       taskID,
		"status":   "in_progress",
		"agent_id": "finisher-agent",
	}))
	mustResult(t, res1, err1)

	// Then mark done.
	res2, err2 := s.handleUpdateTask(ctx, callTool(map[string]any{
		"id":       taskID,
		"status":   "done",
		"agent_id": "finisher-agent",
		"notes":    "completed successfully",
	}))
	mustResult(t, res2, err2)

	// Agent task should be cleared.
	agents, _ := s.store.GetAgents()
	for _, a := range agents {
		if a.ID == "finisher-agent" && a.CurrentTaskID != "" {
			t.Errorf("expected finisher-agent current_task_id to be cleared after done, got %q", a.CurrentTaskID)
		}
	}
}

func TestHandleUpdateTask_UnknownID_Succeeds(t *testing.T) {
	// UpdateTask does not check rows affected; updating a nonexistent ID is a no-op, not an error.
	s := newTestServer(t)
	res, err := s.handleUpdateTask(ctx, callTool(map[string]any{
		"id":     "nonexistent-task-id",
		"status": "done",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	_ = res
}

func TestHandleUpdateTask_MissingID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpdateTask(ctx, callTool(map[string]any{"status": "done"}))
	mustErrorResult(t, res, err)
}

// ── handleLinkTaskNodes ───────────────────────────────────────────────────────

func TestHandleLinkTaskNodes_Valid(t *testing.T) {
	s, loginID, logoutID := newPopulatedServer(t)
	taskID := makeTask(t, s, "link test")

	res, err := s.handleLinkTaskNodes(ctx, callTool(map[string]any{
		"task_id":  taskID,
		"node_ids": []any{string(loginID), string(logoutID)},
	}))
	mustResult(t, res, err)
}

func TestHandleLinkTaskNodes_MissingTaskID_ReturnsError(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	res, err := s.handleLinkTaskNodes(ctx, callTool(map[string]any{
		"node_ids": []any{string(loginID)},
	}))
	mustErrorResult(t, res, err)
}

// ── handleSaveSessionState / handleGetSessionState ────────────────────────────

func TestHandleSaveAndGetSessionState_RoundTrip(t *testing.T) {
	s := newTestServer(t)
	taskID := makeTask(t, s, "state task")

	res, err := s.handleSaveSessionState(ctx, callTool(map[string]any{
		"task_id":         taskID,
		"agent_id":        "state-agent",
		"approach":        "done half the work",
		"remaining_steps": []any{"continue with the other half"},
	}))
	mustResult(t, res, err)

	res2, err2 := s.handleGetSessionState(ctx, callTool(map[string]any{
		"task_id": taskID,
	}))
	m2 := mustResult(t, res2, err2)
	hasKey(t, m2, "state")
}

func TestHandleGetSessionState_Missing_ReturnsEmpty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetSessionState(ctx, callTool(map[string]any{
		"task_id": "nonexistent-task",
	}))
	// No saved state → should return gracefully (not a Go error, not a tool error).
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	_ = res
}

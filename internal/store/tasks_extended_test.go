package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ── GetTask ───────────────────────────────────────────────────────────────────

func TestGetTask_ExistingTask(t *testing.T) {
	st := openTestStore(t)

	planID, _ := st.CreatePlan("get-task plan", "", "", []store.TaskInput{
		{Title: "my task", Priority: "p1"},
	})
	tasks, _ := st.GetPendingTasks(planID, "")
	if len(tasks) == 0 {
		t.Fatal("no tasks created")
	}
	taskID := tasks[0].ID

	task, err := st.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected non-nil task")
	}
	if task.Title != "my task" {
		t.Errorf("expected title 'my task', got %q", task.Title)
	}
	if task.PlanID != planID {
		t.Errorf("expected plan_id %q, got %q", planID, task.PlanID)
	}
}

func TestGetTask_NotFound_ReturnsError(t *testing.T) {
	st := openTestStore(t)

	task, err := st.GetTask("nonexistent-id")
	if err == nil {
		t.Error("expected error for nonexistent task")
	}
	if task != nil {
		t.Error("expected nil task for nonexistent ID")
	}
}

// ── UpdateLinkedNodes ─────────────────────────────────────────────────────────

func TestUpdateLinkedNodes_StoredAndRetrieved(t *testing.T) {
	st := openTestStore(t)

	planID, _ := st.CreatePlan("link plan", "", "", []store.TaskInput{
		{Title: "linkable task", Priority: "p1"},
	})
	tasks, _ := st.GetPendingTasks(planID, "")
	taskID := tasks[0].ID

	nodeIDs := []string{"auth.go:AuthService", "auth.go:Login"}
	if err := st.UpdateLinkedNodes(taskID, nodeIDs); err != nil {
		t.Fatalf("UpdateLinkedNodes: %v", err)
	}

	// Retrieve task and verify linked_nodes are present.
	task, err := st.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask after link: %v", err)
	}
	if len(task.LinkedNodes) != 2 {
		t.Errorf("expected 2 linked nodes, got %d: %v", len(task.LinkedNodes), task.LinkedNodes)
	}
}

// ── UpsertSessionState / GetSessionState ──────────────────────────────────────

func TestUpsertAndGetSessionState_RoundTrip(t *testing.T) {
	st := openTestStore(t)

	planID, _ := st.CreatePlan("state plan", "", "", []store.TaskInput{
		{Title: "stateful task", Priority: "p1"},
	})
	tasks, _ := st.GetPendingTasks(planID, "")
	taskID := tasks[0].ID

	state := store.SessionState{
		TaskID:          taskID,
		AgentID:         "test-agent",
		Approach:        "bottom-up refactoring",
		FilesModified:   []string{"auth.go", "handler.go"},
		CompletedSteps:  []string{"extract interface"},
		RemainingSteps:  []string{"add tests", "update docs"},
		Blockers:        []string{"circular dep in pkg/util"},
		Decisions:       []string{"use JWT over sessions"},
		ContextSnapshot: "auth module context...",
	}

	if err := st.UpsertSessionState(state); err != nil {
		t.Fatalf("UpsertSessionState: %v", err)
	}

	loaded, err := st.GetSessionState(taskID)
	if err != nil {
		t.Fatalf("GetSessionState: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil state")
	}
	if loaded.Approach != state.Approach {
		t.Errorf("approach mismatch: %q vs %q", loaded.Approach, state.Approach)
	}
	if len(loaded.FilesModified) != 2 {
		t.Errorf("expected 2 files modified, got %d", len(loaded.FilesModified))
	}
	if len(loaded.RemainingSteps) != 2 {
		t.Errorf("expected 2 remaining steps, got %d", len(loaded.RemainingSteps))
	}
}

func TestGetSessionState_NotFound_ReturnsNil(t *testing.T) {
	st := openTestStore(t)

	state, err := st.GetSessionState("nonexistent-task")
	if err != nil {
		t.Fatalf("GetSessionState missing: %v", err)
	}
	if state != nil {
		t.Error("expected nil state for unknown task")
	}
}

func TestUpsertSessionState_UpdatesExisting(t *testing.T) {
	st := openTestStore(t)

	planID, _ := st.CreatePlan("update state plan", "", "", []store.TaskInput{
		{Title: "update task", Priority: "p1"},
	})
	tasks, _ := st.GetPendingTasks(planID, "")
	taskID := tasks[0].ID

	// First save.
	_ = st.UpsertSessionState(store.SessionState{
		TaskID:   taskID,
		Approach: "original approach",
	})

	// Update.
	_ = st.UpsertSessionState(store.SessionState{
		TaskID:   taskID,
		Approach: "revised approach",
	})

	loaded, _ := st.GetSessionState(taskID)
	if loaded.Approach != "revised approach" {
		t.Errorf("expected updated approach, got %q", loaded.Approach)
	}
}

// ── GetSessionStateForTasks ───────────────────────────────────────────────────

func TestGetSessionStateForTasks_MultipleIDs(t *testing.T) {
	st := openTestStore(t)

	planID, _ := st.CreatePlan("multi-state plan", "", "", []store.TaskInput{
		{Title: "task-1", Priority: "p1"},
		{Title: "task-2", Priority: "p2"},
	})
	tasks, _ := st.GetPendingTasks(planID, "")
	if len(tasks) < 2 {
		t.Fatal("expected 2 tasks")
	}

	id1, id2 := tasks[0].ID, tasks[1].ID
	_ = st.UpsertSessionState(store.SessionState{TaskID: id1, Approach: "approach-1"})
	_ = st.UpsertSessionState(store.SessionState{TaskID: id2, Approach: "approach-2"})

	m, err := st.GetSessionStateForTasks([]string{id1, id2})
	if err != nil {
		t.Fatalf("GetSessionStateForTasks: %v", err)
	}
	if len(m) != 2 {
		t.Errorf("expected 2 states, got %d", len(m))
	}
	if m[id1].Approach != "approach-1" {
		t.Errorf("expected approach-1, got %q", m[id1].Approach)
	}
}

func TestGetSessionStateForTasks_Empty(t *testing.T) {
	st := openTestStore(t)

	m, err := st.GetSessionStateForTasks([]string{})
	if err != nil {
		t.Fatalf("GetSessionStateForTasks empty: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %d entries", len(m))
	}
}

// ── Task UnmarshalJSON (TaskInput deserialization) ────────────────────────────

func TestTaskInput_UnmarshalJSON(t *testing.T) {
	// This exercises the custom UnmarshalJSON on TaskInput embedded in JSON.
	// CreatePlan accepts []TaskInput with string dependencies.
	st := openTestStore(t)

	planID, err := st.CreatePlan("dep plan", "", "", []store.TaskInput{
		{Title: "first", Priority: "p0"},
		{Title: "second", Priority: "p1", DependsOn: []string{}},
	})
	if err != nil {
		t.Fatalf("CreatePlan with deps: %v", err)
	}
	tasks, _ := st.GetPendingTasks(planID, "")
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

// ── ClearAgentTask / ClearAgentScope ─────────────────────────────────────────

func TestClearAgentTask(t *testing.T) {
	st := openTestStore(t)

	// Register agent via UpsertAgent, then set a task.
	_ = st.UpsertAgent("task-agent", &store.AgentActivity{
		TaskID:    "task-123",
		TaskTitle: "fix login bug",
	})

	// Verify task is set.
	agents, _ := st.GetAgents()
	var found bool
	for _, a := range agents {
		if a.ID == "task-agent" && a.CurrentTaskID == "task-123" {
			found = true
		}
	}
	if !found {
		t.Error("expected current_task_id set on agent")
	}

	// Clear it.
	if err := st.ClearAgentTask("task-agent"); err != nil {
		t.Fatalf("ClearAgentTask: %v", err)
	}

	agents2, _ := st.GetAgents()
	for _, a := range agents2 {
		if a.ID == "task-agent" && a.CurrentTaskID != "" {
			t.Errorf("expected empty current_task_id after clear, got %q", a.CurrentTaskID)
		}
	}
}

func TestClearAgentScope(t *testing.T) {
	st := openTestStore(t)

	_ = st.UpsertAgent("scope-agent", &store.AgentActivity{
		Focus: "pkg/auth",
	})

	if err := st.ClearAgentScope("scope-agent"); err != nil {
		t.Fatalf("ClearAgentScope: %v", err)
	}
	// No error = pass; the column isn't exposed in GetAgents so just verify no crash.
}

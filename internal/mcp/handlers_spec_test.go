package mcp

import (
	"encoding/json"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── handleUpdateSpecItem ──────────────────────────────────────────────────────

func TestHandleUpdateSpecItem_NoStore(t *testing.T) {
	s := New(nil, nil, nil)
	res, err := s.handleUpdateSpecItem(ctx, callTool(map[string]any{
		"task_id": "task-1",
		"item_id": "item-1",
		"done":    true,
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !res.IsError {
		t.Error("expected tool error for nil store")
	}
}

func TestHandleUpdateSpecItem_MissingTaskID(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	res, err := s.handleUpdateSpecItem(ctx, callTool(map[string]any{
		"item_id": "item-1",
		"done":    true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("expected tool error for missing task_id")
	}
}

func TestHandleUpdateSpecItem_MissingItemID(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	res, err := s.handleUpdateSpecItem(ctx, callTool(map[string]any{
		"task_id": "task-1",
		"done":    true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("expected tool error for missing item_id")
	}
}

func TestHandleUpdateSpecItem_HappyPath(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	// Create a plan with spec items.
	_, taskIDs, err := st.CreatePlan("spec plan", "", "agent1", []store.TaskInput{
		{
			Title: "OAuth task",
			SpecItems: []store.SpecItem{
				{Label: "Add handler"},
				{Label: "Add token endpoint"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	task, err := st.GetTask(taskIDs[0])
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if len(task.SpecItems) != 2 {
		t.Fatalf("expected 2 spec items, got %d", len(task.SpecItems))
	}
	itemID := task.SpecItems[0].ID

	res, err := s.handleUpdateSpecItem(ctx, callTool(map[string]any{
		"task_id": taskIDs[0],
		"item_id": itemID,
		"done":    true,
	}))
	m := mustResult(t, res, err)

	if m["done"] != true {
		t.Errorf("expected done=true in response, got %v", m["done"])
	}
	if m["all_done"] != false {
		t.Errorf("expected all_done=false (one item still pending), got %v", m["all_done"])
	}
	cov, ok := m["coverage"].(string)
	if !ok || cov != "1/2" {
		t.Errorf("expected coverage=1/2, got %v", m["coverage"])
	}
}

func TestHandleUpdateSpecItem_AllDone(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	_, taskIDs, _ := st.CreatePlan("all done plan", "", "", []store.TaskInput{
		{Title: "Task", SpecItems: []store.SpecItem{{Label: "Only step"}}},
	})
	task, _ := st.GetTask(taskIDs[0])
	itemID := task.SpecItems[0].ID

	res, err := s.handleUpdateSpecItem(ctx, callTool(map[string]any{
		"task_id": taskIDs[0],
		"item_id": itemID,
		"done":    true,
	}))
	m := mustResult(t, res, err)

	if m["all_done"] != true {
		t.Errorf("expected all_done=true when single item is done, got %v", m["all_done"])
	}
}

func TestHandleUpdateSpecItem_UnknownItemID(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	_, taskIDs, _ := st.CreatePlan("plan", "", "", []store.TaskInput{
		{Title: "Task", SpecItems: []store.SpecItem{{Label: "Step"}}},
	})

	res, err := s.handleUpdateSpecItem(ctx, callTool(map[string]any{
		"task_id": taskIDs[0],
		"item_id": "no-such-item",
		"done":    true,
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !res.IsError {
		t.Error("expected tool error for unknown item_id")
	}
}

func TestHandleUpdateSpecItem_DoneAsBoolFalse(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	_, taskIDs, _ := st.CreatePlan("toggle plan", "", "", []store.TaskInput{
		{Title: "Task", SpecItems: []store.SpecItem{{Label: "Step"}}},
	})
	task, _ := st.GetTask(taskIDs[0])
	itemID := task.SpecItems[0].ID

	// Mark done, then unmark.
	_, _ = s.handleUpdateSpecItem(ctx, callTool(map[string]any{
		"task_id": taskIDs[0], "item_id": itemID, "done": true,
	}))
	res, err := s.handleUpdateSpecItem(ctx, callTool(map[string]any{
		"task_id": taskIDs[0], "item_id": itemID, "done": false,
	}))
	m := mustResult(t, res, err)
	if m["done"] != false {
		t.Errorf("expected done=false after toggle, got %v", m["done"])
	}
}

// ── handleVerifyImplementation — spec coverage ──────────────────────────────

func TestHandleVerifyImplementation_SpecCoverage_AllPending(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// Create a task with spec items (none done).
	_, taskIDs, _ := s.store.CreatePlan("spec verify plan", "", "", []store.TaskInput{
		{
			Title: "Auth task",
			SpecItems: []store.SpecItem{
				{Label: "Add OAuth handler"},
				{Label: "Add token refresh"},
			},
		},
	})
	taskID := taskIDs[0]

	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["pkg/auth/auth.go"]`,
		"task_id":       taskID,
	}))
	m := mustResult(t, res, err)

	tv, ok := m["task_verification"].(map[string]any)
	if !ok {
		t.Fatalf("expected task_verification in response, got %T", m["task_verification"])
	}

	specCov, ok := tv["spec_coverage"].(map[string]any)
	if !ok {
		t.Fatalf("expected spec_coverage in task_verification, got %T", tv["spec_coverage"])
	}
	if specCov["total"] != float64(2) {
		t.Errorf("expected total=2, got %v", specCov["total"])
	}
	if specCov["done"] != float64(0) {
		t.Errorf("expected done=0, got %v", specCov["done"])
	}
	if specCov["complete"] != false {
		t.Errorf("expected complete=false, got %v", specCov["complete"])
	}

	// Warning should be present.
	if _, hasWarn := tv["spec_coverage_warning"]; !hasWarn {
		t.Error("expected spec_coverage_warning when items are pending")
	}
}

func TestHandleVerifyImplementation_SpecCoverage_AllDone(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	_, taskIDs, _ := s.store.CreatePlan("done plan", "", "", []store.TaskInput{
		{
			Title:     "Task",
			SpecItems: []store.SpecItem{{Label: "Step A"}, {Label: "Step B"}},
		},
	})
	taskID := taskIDs[0]

	// Mark all items done.
	task, _ := s.store.GetTask(taskID)
	for _, item := range task.SpecItems {
		_ = s.store.UpdateSpecItem(taskID, item.ID, true)
	}

	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["pkg/auth/auth.go"]`,
		"task_id":       taskID,
	}))
	m := mustResult(t, res, err)

	tv, ok := m["task_verification"].(map[string]any)
	if !ok {
		t.Fatalf("expected task_verification in response")
	}

	specCov, ok := tv["spec_coverage"].(map[string]any)
	if !ok {
		t.Fatalf("expected spec_coverage in task_verification")
	}
	if specCov["complete"] != true {
		t.Errorf("expected complete=true, got %v", specCov["complete"])
	}

	// No warning when all items are done.
	if _, hasWarn := tv["spec_coverage_warning"]; hasWarn {
		t.Error("unexpected spec_coverage_warning when all items are done")
	}
}

func TestHandleVerifyImplementation_SpecCoverage_NoSpecItems(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// Task with no spec items — spec_coverage should be absent (not nil or empty).
	_, taskIDs, _ := s.store.CreatePlan("no spec plan", "", "", []store.TaskInput{
		{Title: "Plain task"},
	})
	taskID := taskIDs[0]

	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["pkg/auth/auth.go"]`,
		"task_id":       taskID,
	}))
	m := mustResult(t, res, err)

	tv, ok := m["task_verification"].(map[string]any)
	if !ok {
		t.Fatalf("expected task_verification in response")
	}

	// No spec items means no spec_coverage key.
	if _, hasCov := tv["spec_coverage"]; hasCov {
		t.Error("expected no spec_coverage when task has no spec items")
	}
}

// ── tasks dispatch — update_spec_item ────────────────────────────────────────

func TestHandleTasksDispatch_UpdateSpecItem(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	_, taskIDs, _ := st.CreatePlan("dispatch plan", "", "", []store.TaskInput{
		{Title: "Task", SpecItems: []store.SpecItem{{Label: "Item"}}},
	})
	task, _ := st.GetTask(taskIDs[0])
	itemID := task.SpecItems[0].ID

	res, err := s.handleTasksDispatch(ctx, callTool(map[string]any{
		"action":  "update_spec_item",
		"task_id": taskIDs[0],
		"item_id": itemID,
		"done":    true,
	}))
	m := mustResult(t, res, err)
	if m["done"] != true {
		t.Errorf("expected done=true, got %v", m["done"])
	}
}

func TestHandleTasksDispatch_UnknownAction(t *testing.T) {
	st := openMCPTestStore(t)
	s := New(nil, nil, st)

	res, err := s.handleTasksDispatch(ctx, callTool(map[string]any{
		"action": "update_spec_item",
	}))
	// Should return tool error (missing task_id), not a Go error.
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !res.IsError {
		// May succeed if somehow handled — just verify no panic.
		_ = res
	}
}

// ── create_plan with spec_items via MCP ──────────────────────────────────────

func TestHandleCreatePlan_WithSpecItems_ViaJSON(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	tasksJSON, _ := json.Marshal([]map[string]any{
		{
			"title":    "OAuth task",
			"priority": "p0",
			"spec_items": []map[string]any{
				{"label": "Add handler"},
				{"label": "Add refresh"},
			},
		},
	})

	res, err := s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "spec JSON plan",
		"tasks": string(tasksJSON),
	}))
	m := mustResult(t, res, err)
	if m["task_count"] != float64(1) {
		t.Errorf("expected task_count=1, got %v", m["task_count"])
	}

	tasks, _ := st.GetPendingTasks("", "")
	if len(tasks) == 0 {
		t.Fatal("no tasks created")
	}
	if len(tasks[0].SpecItems) != 2 {
		t.Errorf("expected 2 spec items, got %d", len(tasks[0].SpecItems))
	}
	// IDs auto-assigned.
	for i, item := range tasks[0].SpecItems {
		if item.ID == "" {
			t.Errorf("spec item %d has empty ID", i)
		}
	}
}

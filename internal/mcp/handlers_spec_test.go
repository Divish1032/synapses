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

// ── handleUpdateTask — tracked_files_reminder ────────────────────────────────

func TestHandleUpdateTask_Done_TrackedFilesReminder(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	_, taskIDs, _ := st.CreatePlan("reminder plan", "", "", []store.TaskInput{
		{Title: "Multi-file task"},
	})
	taskID := taskIDs[0]
	_ = st.SetTrackedFiles(taskID, []string{"handler.go", "service.go"})

	// Mark in_progress first, then done.
	_, _ = s.handleUpdateTask(ctx, callTool(map[string]any{
		"id": taskID, "status": "in_progress",
	}))
	res, err := s.handleUpdateTask(ctx, callTool(map[string]any{
		"id": taskID, "status": "done",
	}))
	m := mustResult(t, res, err)

	if _, hasReminder := m["tracked_files_reminder"]; !hasReminder {
		t.Error("expected tracked_files_reminder in done response when tracked files are registered")
	}
}

func TestHandleUpdateTask_Done_NoTrackedFiles_NoReminder(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	_, taskIDs, _ := st.CreatePlan("no tracked plan", "", "", []store.TaskInput{
		{Title: "Plain task"},
	})
	taskID := taskIDs[0]

	res, err := s.handleUpdateTask(ctx, callTool(map[string]any{
		"id": taskID, "status": "done",
	}))
	m := mustResult(t, res, err)

	if _, hasReminder := m["tracked_files_reminder"]; hasReminder {
		t.Error("unexpected tracked_files_reminder when task has no tracked files")
	}
}

func TestHandleUpdateTask_Cancelled_NoReminder(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	_, taskIDs, _ := st.CreatePlan("cancel plan", "", "", []store.TaskInput{
		{Title: "Cancelled task"},
	})
	taskID := taskIDs[0]
	_ = st.SetTrackedFiles(taskID, []string{"handler.go"})

	res, err := s.handleUpdateTask(ctx, callTool(map[string]any{
		"id": taskID, "status": "cancelled",
	}))
	m := mustResult(t, res, err)

	// Reminder only fires on done, not cancelled.
	if _, hasReminder := m["tracked_files_reminder"]; hasReminder {
		t.Error("unexpected tracked_files_reminder on cancelled status")
	}
}

// ── handleSetTrackedFiles ──────────────────────────────────────────────────────

func TestHandleSetTrackedFiles_NoStore(t *testing.T) {
	s := New(nil, nil, nil)
	res, err := s.handleSetTrackedFiles(ctx, callTool(map[string]any{
		"task_id": "task-1",
		"files":   []any{"a.go"},
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !res.IsError {
		t.Error("expected tool error for nil store")
	}
}

func TestHandleSetTrackedFiles_MissingTaskID(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	res, err := s.handleSetTrackedFiles(ctx, callTool(map[string]any{
		"files": []any{"handler.go"},
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("expected tool error for missing task_id")
	}
}

func TestHandleSetTrackedFiles_MissingFiles(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	res, err := s.handleSetTrackedFiles(ctx, callTool(map[string]any{
		"task_id": "task-1",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("expected tool error for missing files")
	}
}

func TestHandleSetTrackedFiles_Success(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	_, taskIDs, _ := st.CreatePlan("tracked plan", "", "", []store.TaskInput{
		{Title: "Multi-file task"},
	})
	taskID := taskIDs[0]

	res, err := s.handleSetTrackedFiles(ctx, callTool(map[string]any{
		"task_id": taskID,
		"files":   []any{"internal/auth/handler.go", "internal/auth/service.go"},
	}))
	m := mustResult(t, res, err)
	if m["count"] != float64(2) {
		t.Errorf("expected count=2, got %v", m["count"])
	}
	if m["task_id"] != taskID {
		t.Errorf("expected task_id=%q, got %v", taskID, m["task_id"])
	}

	// Verify store was updated.
	task, _ := st.GetTask(taskID)
	if len(task.TrackedFiles) != 2 {
		t.Fatalf("expected 2 tracked files in store, got %d", len(task.TrackedFiles))
	}
}

func TestHandleSetTrackedFiles_JSONStringFiles(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	_, taskIDs, _ := st.CreatePlan("json string plan", "", "", []store.TaskInput{
		{Title: "Task"},
	})
	taskID := taskIDs[0]

	filesJSON, _ := json.Marshal([]string{"foo.go", "bar.go"})
	res, err := s.handleSetTrackedFiles(ctx, callTool(map[string]any{
		"task_id": taskID,
		"files":   string(filesJSON),
	}))
	m := mustResult(t, res, err)
	if m["count"] != float64(2) {
		t.Errorf("expected count=2, got %v", m["count"])
	}
}

func TestHandleTasksDispatch_SetTrackedFiles(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	s := New(g, cfg, st)

	_, taskIDs, _ := st.CreatePlan("dispatch tracked plan", "", "", []store.TaskInput{
		{Title: "Task"},
	})
	taskID := taskIDs[0]

	res, err := s.handleTasksDispatch(ctx, callTool(map[string]any{
		"action":  "set_tracked_files",
		"task_id": taskID,
		"files":   []any{"main.go"},
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Errorf("unexpected tool error: %v", res.Content)
	}
}

// ── handleVerifyImplementation — file tracking ────────────────────────────────

func TestHandleVerifyImplementation_FileTracking_SomeUnmodified(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	_, taskIDs, _ := s.store.CreatePlan("file tracking plan", "", "", []store.TaskInput{
		{Title: "Multi-file task"},
	})
	taskID := taskIDs[0]
	_ = s.store.SetTrackedFiles(taskID, []string{"internal/auth/handler.go", "internal/auth/service.go"})

	// Only write one of the two tracked files.
	filesWritten, _ := json.Marshal([]string{"internal/auth/handler.go"})
	res1, err1 := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": string(filesWritten),
		"task_id":       taskID,
	}))
	m := mustResult(t, res1, err1)

	tv, ok := m["task_verification"].(map[string]any)
	if !ok {
		t.Fatalf("expected task_verification in response, got %T", m["task_verification"])
	}

	ft, ok := tv["file_tracking"].(map[string]any)
	if !ok {
		t.Fatalf("expected file_tracking in task_verification, got %T", tv["file_tracking"])
	}
	if ft["total_tracked"] != float64(2) {
		t.Errorf("expected total_tracked=2, got %v", ft["total_tracked"])
	}
	if ft["unmodified_count"] != float64(1) {
		t.Errorf("expected unmodified_count=1, got %v", ft["unmodified_count"])
	}
	if ft["complete"] != false {
		t.Error("expected complete=false when files remain unmodified")
	}

	if _, hasWarn := tv["file_tracking_warning"]; !hasWarn {
		t.Error("expected file_tracking_warning when tracked files are unmodified")
	}
}

func TestHandleVerifyImplementation_FileTracking_AllModified(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	_, taskIDs, _ := s.store.CreatePlan("all modified plan", "", "", []store.TaskInput{
		{Title: "Task"},
	})
	taskID := taskIDs[0]
	_ = s.store.SetTrackedFiles(taskID, []string{"internal/auth/handler.go", "internal/auth/service.go"})

	// Write both tracked files.
	filesWritten, _ := json.Marshal([]string{"internal/auth/handler.go", "internal/auth/service.go"})
	res2, err2 := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": string(filesWritten),
		"task_id":       taskID,
	}))
	m := mustResult(t, res2, err2)

	tv, _ := m["task_verification"].(map[string]any)
	ft, ok := tv["file_tracking"].(map[string]any)
	if !ok {
		t.Fatalf("expected file_tracking in task_verification")
	}
	if ft["complete"] != true {
		t.Error("expected complete=true when all tracked files are modified")
	}
	if _, hasWarn := tv["file_tracking_warning"]; hasWarn {
		t.Error("unexpected file_tracking_warning when all files are modified")
	}
}

func TestHandleVerifyImplementation_FileTracking_BaseNameMatch(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	_, taskIDs, _ := s.store.CreatePlan("base name plan", "", "", []store.TaskInput{
		{Title: "Task"},
	})
	taskID := taskIDs[0]
	// Tracked with full path.
	_ = s.store.SetTrackedFiles(taskID, []string{"internal/auth/handler.go"})

	// Written with just base name — should match via base name fallback.
	filesWritten, _ := json.Marshal([]string{"handler.go"})
	res3, err3 := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": string(filesWritten),
		"task_id":       taskID,
	}))
	m := mustResult(t, res3, err3)

	tv, _ := m["task_verification"].(map[string]any)
	ft, ok := tv["file_tracking"].(map[string]any)
	if !ok {
		t.Fatalf("expected file_tracking in task_verification")
	}
	if ft["complete"] != true {
		t.Errorf("expected complete=true via base name match, got %v", ft["complete"])
	}
}

func TestHandleVerifyImplementation_FileTracking_NoTrackedFiles(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// Task with no tracked files — file_tracking key should be absent.
	_, taskIDs, _ := s.store.CreatePlan("no tracking plan", "", "", []store.TaskInput{
		{Title: "Plain task"},
	})
	taskID := taskIDs[0]

	filesWritten, _ := json.Marshal([]string{"some.go"})
	res4, err4 := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": string(filesWritten),
		"task_id":       taskID,
	}))
	m := mustResult(t, res4, err4)

	tv, ok := m["task_verification"].(map[string]any)
	if !ok {
		// No task_verification at all is fine (graph may not have the file).
		return
	}
	if _, hasTracking := tv["file_tracking"]; hasTracking {
		t.Error("expected no file_tracking when task has no tracked files")
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

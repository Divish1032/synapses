package store

import (
	"testing"
)

func TestSetTrackedFiles_Basic(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, taskIDs, err := st.CreatePlan("file plan", "desc", "agent1", []TaskInput{
		{Title: "Multi-file task"},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	taskID := taskIDs[0]

	files := []string{"internal/auth/handler.go", "internal/auth/service.go"}
	if err := st.SetTrackedFiles(taskID, files); err != nil {
		t.Fatalf("SetTrackedFiles: %v", err)
	}

	task, err := st.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if len(task.TrackedFiles) != 2 {
		t.Fatalf("expected 2 tracked files, got %d", len(task.TrackedFiles))
	}
	if task.TrackedFiles[0] != "internal/auth/handler.go" {
		t.Errorf("expected first file %q, got %q", "internal/auth/handler.go", task.TrackedFiles[0])
	}
	if task.TrackedFiles[1] != "internal/auth/service.go" {
		t.Errorf("expected second file %q, got %q", "internal/auth/service.go", task.TrackedFiles[1])
	}
}

func TestSetTrackedFiles_Replaces(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, taskIDs, _ := st.CreatePlan("replace plan", "", "", []TaskInput{
		{Title: "Task"},
	})
	taskID := taskIDs[0]

	_ = st.SetTrackedFiles(taskID, []string{"a.go", "b.go"})
	_ = st.SetTrackedFiles(taskID, []string{"c.go"})

	task, _ := st.GetTask(taskID)
	if len(task.TrackedFiles) != 1 {
		t.Fatalf("expected 1 tracked file after replace, got %d", len(task.TrackedFiles))
	}
	if task.TrackedFiles[0] != "c.go" {
		t.Errorf("expected %q, got %q", "c.go", task.TrackedFiles[0])
	}
}

func TestSetTrackedFiles_EmptyTaskID(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	err := st.SetTrackedFiles("", []string{"a.go"})
	if err == nil {
		t.Error("expected error for empty task_id")
	}
}

func TestSetTrackedFiles_UnknownTask(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	err := st.SetTrackedFiles("nonexistent-task-id", []string{"a.go"})
	if err == nil {
		t.Error("expected error for unknown task ID, got nil")
	}
}

func TestSetTrackedFiles_NilSlice(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, taskIDs, _ := st.CreatePlan("nil plan", "", "", []TaskInput{
		{Title: "Task"},
	})
	taskID := taskIDs[0]

	// nil treated as empty; existing tracked files cleared — but we guard with empty
	// so the task must exist; nil input returns an error would be wrong here.
	// Actually SetTrackedFiles converts nil to [] and updates — but empty would violate
	// the "at least one file" guard at the MCP layer, not the store layer.
	// So store allows nil→empty (clearing tracked files is valid).
	if err := st.SetTrackedFiles(taskID, nil); err != nil {
		t.Fatalf("SetTrackedFiles with nil: %v", err)
	}

	task, _ := st.GetTask(taskID)
	if task.TrackedFiles != nil && len(task.TrackedFiles) != 0 {
		t.Errorf("expected empty tracked files after nil set, got %v", task.TrackedFiles)
	}
}

func TestCreatePlan_WithTrackedFiles(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, taskIDs, err := st.CreatePlan("tracked plan", "desc", "", []TaskInput{
		{
			Title:        "Task with files",
			TrackedFiles: []string{"handler.go", "service.go", "repo.go"},
		},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	task, err := st.GetTask(taskIDs[0])
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if len(task.TrackedFiles) != 3 {
		t.Fatalf("expected 3 tracked files, got %d", len(task.TrackedFiles))
	}
	if task.TrackedFiles[2] != "repo.go" {
		t.Errorf("expected third file %q, got %q", "repo.go", task.TrackedFiles[2])
	}
}

func TestGetPendingTasks_IncludesTrackedFiles(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, _, err := st.CreatePlan("pending tracked plan", "", "", []TaskInput{
		{
			Title:        "Tracked task",
			TrackedFiles: []string{"foo.go", "bar.go"},
		},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	tasks, err := st.GetPendingTasks("", "")
	if err != nil {
		t.Fatalf("GetPendingTasks: %v", err)
	}

	var found *Task
	for i := range tasks {
		if tasks[i].Title == "Tracked task" {
			found = &tasks[i]
			break
		}
	}
	if found == nil {
		t.Fatal("tracked task not found in pending tasks")
	}
	if len(found.TrackedFiles) != 2 {
		t.Errorf("expected 2 tracked files in pending task, got %d", len(found.TrackedFiles))
	}
}

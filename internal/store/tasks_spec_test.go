package store

import (
	"sync"
	"testing"
)

func TestCreatePlan_WithSpecItems(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, taskIDs, err := st.CreatePlan("spec plan", "desc", "agent1", []TaskInput{
		{
			Title:    "Implement OAuth",
			Priority: "p0",
			SpecItems: []SpecItem{
				{Label: "Add OAuth handler"},
				{Label: "Add token refresh endpoint"},
				{Label: "Update auth middleware"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if len(taskIDs) != 1 {
		t.Fatalf("expected 1 task ID, got %d", len(taskIDs))
	}

	task, err := st.GetTask(taskIDs[0])
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if len(task.SpecItems) != 3 {
		t.Fatalf("expected 3 spec items, got %d", len(task.SpecItems))
	}
	// All items should have auto-assigned IDs.
	for i, item := range task.SpecItems {
		if item.ID == "" {
			t.Errorf("spec item %d has empty ID", i)
		}
		if item.Done {
			t.Errorf("spec item %d should start undone", i)
		}
	}
	if task.SpecItems[0].Label != "Add OAuth handler" {
		t.Errorf("expected first label %q, got %q", "Add OAuth handler", task.SpecItems[0].Label)
	}
}

func TestCreatePlan_SpecItemIDsPreserved(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, taskIDs, err := st.CreatePlan("id plan", "", "", []TaskInput{
		{
			Title: "Task with explicit IDs",
			SpecItems: []SpecItem{
				{ID: "explicit-id-1", Label: "Step one"},
				{ID: "explicit-id-2", Label: "Step two"},
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
	if task.SpecItems[0].ID != "explicit-id-1" {
		t.Errorf("expected ID %q, got %q", "explicit-id-1", task.SpecItems[0].ID)
	}
	if task.SpecItems[1].ID != "explicit-id-2" {
		t.Errorf("expected ID %q, got %q", "explicit-id-2", task.SpecItems[1].ID)
	}
}

func TestUpdateSpecItem_MarkDone(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, taskIDs, err := st.CreatePlan("update plan", "", "", []TaskInput{
		{
			Title: "Multi-step task",
			SpecItems: []SpecItem{
				{Label: "Step A"},
				{Label: "Step B"},
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

	// Mark first item done.
	itemID := task.SpecItems[0].ID
	if err := st.UpdateSpecItem(taskIDs[0], itemID, true); err != nil {
		t.Fatalf("UpdateSpecItem: %v", err)
	}

	// Re-fetch and verify.
	task2, err := st.GetTask(taskIDs[0])
	if err != nil {
		t.Fatalf("GetTask after update: %v", err)
	}
	if !task2.SpecItems[0].Done {
		t.Error("expected spec item 0 to be done")
	}
	if task2.SpecItems[1].Done {
		t.Error("expected spec item 1 to still be pending")
	}
}

func TestUpdateSpecItem_ToggleBack(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, taskIDs, _ := st.CreatePlan("toggle plan", "", "", []TaskInput{
		{Title: "Task", SpecItems: []SpecItem{{Label: "Step"}}},
	})
	task, _ := st.GetTask(taskIDs[0])
	itemID := task.SpecItems[0].ID

	// Mark done then unmark.
	_ = st.UpdateSpecItem(taskIDs[0], itemID, true)
	_ = st.UpdateSpecItem(taskIDs[0], itemID, false)

	task2, _ := st.GetTask(taskIDs[0])
	if task2.SpecItems[0].Done {
		t.Error("expected spec item to be pending after toggle back")
	}
}

func TestUpdateSpecItem_UnknownItemID(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, taskIDs, _ := st.CreatePlan("err plan", "", "", []TaskInput{
		{Title: "Task", SpecItems: []SpecItem{{Label: "Step"}}},
	})

	err := st.UpdateSpecItem(taskIDs[0], "nonexistent-id", true)
	if err == nil {
		t.Error("expected error for unknown item ID, got nil")
	}
}

func TestUpdateSpecItem_EmptyTaskID(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	err := st.UpdateSpecItem("", "some-item", true)
	if err == nil {
		t.Error("expected error for empty task_id")
	}
}

func TestUpdateSpecItem_EmptyItemID(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	err := st.UpdateSpecItem("some-task", "", true)
	if err == nil {
		t.Error("expected error for empty item_id")
	}
}

func TestGetPendingTasks_IncludesSpecItems(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, _, err := st.CreatePlan("pending spec plan", "", "", []TaskInput{
		{
			Title: "Task with items",
			SpecItems: []SpecItem{
				{Label: "Item 1"},
				{Label: "Item 2"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	tasks, err := st.GetPendingTasks("", "")
	if err != nil {
		t.Fatalf("GetPendingTasks: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("expected at least one task")
	}

	var found *Task
	for i := range tasks {
		if tasks[i].Title == "Task with items" {
			found = &tasks[i]
			break
		}
	}
	if found == nil {
		t.Fatal("task not found in pending tasks")
	}
	if len(found.SpecItems) != 2 {
		t.Errorf("expected 2 spec items in pending task, got %d", len(found.SpecItems))
	}
}

func TestAssignSpecItemIDs_EmptySlice(t *testing.T) {
	result := assignSpecItemIDs(nil)
	if result != nil {
		t.Errorf("expected nil for nil input, got %v", result)
	}
	result = assignSpecItemIDs([]SpecItem{})
	if len(result) != 0 {
		t.Errorf("expected empty slice, got %v", result)
	}
}

func TestAssignSpecItemIDs_MixedIDs(t *testing.T) {
	items := []SpecItem{
		{ID: "has-id", Label: "A"},
		{ID: "", Label: "B"},
		{ID: "also-has-id", Label: "C"},
	}
	result := assignSpecItemIDs(items)
	if result[0].ID != "has-id" {
		t.Errorf("expected preserved ID, got %q", result[0].ID)
	}
	if result[1].ID == "" {
		t.Error("expected auto-generated ID for empty item")
	}
	if result[2].ID != "also-has-id" {
		t.Errorf("expected preserved ID, got %q", result[2].ID)
	}
	// Original slice should not be modified.
	if items[1].ID != "" {
		t.Error("expected original slice to be unmodified")
	}
}

// TestUpdateSpecItem_ConcurrentUpdates verifies that simultaneous UpdateSpecItem
// calls on different items in the same task do not clobber each other.
// Without the transaction wrapping the read-modify-write, two goroutines could
// both read the same spec_items JSON, each mutate a different item, and one
// UPDATE would overwrite the other's change — leaving one item incorrectly
// unchanged. The -race flag catches data races; the assertion catches the
// logical correctness.
func TestUpdateSpecItem_ConcurrentUpdates(t *testing.T) {
	st := openTestStore(t)

	_, taskIDs, err := st.CreatePlan("concurrent plan", "", "agent", []TaskInput{
		{
			Title: "Concurrent Task",
			SpecItems: []SpecItem{
				{ID: "item-a", Label: "Step A"},
				{ID: "item-b", Label: "Step B"},
				{ID: "item-c", Label: "Step C"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	taskID := taskIDs[0]

	// Mark all three items done concurrently.
	var wg sync.WaitGroup
	for _, id := range []string{"item-a", "item-b", "item-c"} {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.UpdateSpecItem(taskID, id, true); err != nil {
				t.Errorf("UpdateSpecItem(%q): %v", id, err)
			}
		}()
	}
	wg.Wait()

	// All three must be done — no update must have been lost.
	task, err := st.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	for _, item := range task.SpecItems {
		if !item.Done {
			t.Errorf("spec item %q (%s) should be done but is not", item.ID, item.Label)
		}
	}
}

package store_test

import (
	"testing"

	"github.com/synapses/synapses/internal/store"
)

func TestCreatePlan_AndGetPendingTasks(t *testing.T) {
	st := openTestStore(t)

	planID, err := st.CreatePlan("test plan", "desc", "", []store.TaskInput{
		{Title: "Task A", Priority: "p0"},
		{Title: "Task B", Priority: "p1"},
		{Title: "Task C", Priority: "p2"},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if planID == "" {
		t.Fatal("expected non-empty plan ID")
	}

	tasks, err := st.GetPendingTasks("", "")
	if err != nil {
		t.Fatalf("GetPendingTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}
	// Must be ordered by priority: p0 first.
	if tasks[0].Priority != "p0" {
		t.Errorf("expected first task priority p0, got %s", tasks[0].Priority)
	}
	if tasks[0].Status != "pending" {
		t.Errorf("expected status pending, got %s", tasks[0].Status)
	}
}

func TestGetPendingTasks_FilterByPlanID(t *testing.T) {
	st := openTestStore(t)

	planA, _ := st.CreatePlan("plan A", "", "", []store.TaskInput{{Title: "A1", Priority: "p1"}})
	_, _ = st.CreatePlan("plan B", "", "", []store.TaskInput{{Title: "B1", Priority: "p1"}})

	tasksA, err := st.GetPendingTasks(planA, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasksA) != 1 {
		t.Fatalf("expected 1 task for plan A, got %d", len(tasksA))
	}
	if tasksA[0].PlanID != planA {
		t.Errorf("task has wrong plan_id: %s", tasksA[0].PlanID)
	}
}

func TestUpdateTask_StatusAndNotes(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.CreatePlan("plan", "", "", []store.TaskInput{
		{Title: "My task", Priority: "p0"},
	})

	tasks, _ := st.GetPendingTasks("", "")
	if len(tasks) == 0 {
		t.Fatal("no tasks created")
	}
	taskID := tasks[0].ID

	if _, err := st.UpdateTask(taskID, "in_progress", "started work", ""); err != nil {
		t.Fatalf("UpdateTask in_progress: %v", err)
	}
	if _, err := st.UpdateTask(taskID, "done", "all done", ""); err != nil {
		t.Fatalf("UpdateTask done: %v", err)
	}

	// Task is now done — should not appear in pending results.
	remaining, _ := st.GetPendingTasks("", "")
	if len(remaining) != 0 {
		t.Errorf("expected 0 pending tasks after done, got %d", len(remaining))
	}
}

func TestGetPlans_Summary(t *testing.T) {
	st := openTestStore(t)

	planID, _ := st.CreatePlan("summary plan", "", "", []store.TaskInput{
		{Title: "T1", Priority: "p0"},
		{Title: "T2", Priority: "p1"},
	})

	tasks, _ := st.GetPendingTasks(planID, "")
	_, _ = st.UpdateTask(tasks[0].ID, "done", "", "")

	plans, err := st.GetPlans()
	if err != nil {
		t.Fatalf("GetPlans: %v", err)
	}
	if len(plans) == 0 {
		t.Fatal("expected at least one plan")
	}
	p := plans[0]
	if p.TotalTasks != 2 {
		t.Errorf("expected 2 total tasks, got %d", p.TotalTasks)
	}
	if p.DoneTasks != 1 {
		t.Errorf("expected 1 done task, got %d", p.DoneTasks)
	}
	if p.PendingTasks != 1 {
		t.Errorf("expected 1 pending task, got %d", p.PendingTasks)
	}
}

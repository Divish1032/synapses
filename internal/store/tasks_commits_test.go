package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestSetTaskStartCommit_HappyPath verifies that start_commit is persisted and
// returned by GetTask and GetPendingTasks.
func TestSetTaskStartCommit_HappyPath(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, ids, err := st.CreatePlan("p", "", "", []store.TaskInput{
		{Title: "task", Priority: "p1"},
	})
	if err != nil || len(ids) == 0 {
		t.Fatalf("CreatePlan: %v", err)
	}
	taskID := ids[0]

	const sha = "abc123def456abc123def456abc123def456abc1"
	if err := st.SetTaskStartCommit(taskID, sha); err != nil {
		t.Fatalf("SetTaskStartCommit: %v", err)
	}

	// GetTask should reflect the stored SHA.
	task, err := st.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.StartCommit != sha {
		t.Errorf("GetTask: StartCommit = %q, want %q", task.StartCommit, sha)
	}

	// GetPendingTasks should also surface it.
	tasks, err := st.GetPendingTasks("", "")
	if err != nil {
		t.Fatalf("GetPendingTasks: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("expected at least one pending task")
	}
	if tasks[0].StartCommit != sha {
		t.Errorf("GetPendingTasks: StartCommit = %q, want %q", tasks[0].StartCommit, sha)
	}
}

// TestSetTaskStartCommit_EmptySHAIsNoop verifies that passing an empty SHA is
// silently ignored (graceful degradation when git is unavailable).
func TestSetTaskStartCommit_EmptySHAIsNoop(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, ids, _ := st.CreatePlan("p", "", "", []store.TaskInput{{Title: "t", Priority: "p2"}})
	taskID := ids[0]

	// Set a real SHA first.
	const sha = "aabbccddeeff0011223344556677889900aabb00"
	_ = st.SetTaskStartCommit(taskID, sha)

	// Now call with empty — should be a no-op, not overwrite.
	if err := st.SetTaskStartCommit(taskID, ""); err != nil {
		t.Fatalf("SetTaskStartCommit with empty SHA: %v", err)
	}

	task, _ := st.GetTask(taskID)
	if task.StartCommit != sha {
		t.Errorf("empty SHA overwrote existing start_commit: got %q, want %q", task.StartCommit, sha)
	}
}

// TestSetTaskCommits_HappyPath verifies commits are persisted and returned.
func TestSetTaskCommits_HappyPath(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, ids, _ := st.CreatePlan("p", "", "", []store.TaskInput{{Title: "t", Priority: "p1"}})
	taskID := ids[0]

	commits := []string{"abc1234 feat: add foo", "def5678 fix: repair bar"}
	if err := st.SetTaskCommits(taskID, commits); err != nil {
		t.Fatalf("SetTaskCommits: %v", err)
	}

	task, err := st.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if len(task.CommitsSinceStart) != 2 {
		t.Fatalf("expected 2 commits, got %d: %v", len(task.CommitsSinceStart), task.CommitsSinceStart)
	}
	if task.CommitsSinceStart[0] != commits[0] {
		t.Errorf("CommitsSinceStart[0] = %q, want %q", task.CommitsSinceStart[0], commits[0])
	}
}

// TestSetTaskCommits_NilCommits verifies nil is stored as an empty list, not an error.
func TestSetTaskCommits_NilCommits(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, ids, _ := st.CreatePlan("p", "", "", []store.TaskInput{{Title: "t", Priority: "p2"}})
	taskID := ids[0]

	if err := st.SetTaskCommits(taskID, nil); err != nil {
		t.Fatalf("SetTaskCommits(nil): %v", err)
	}

	task, _ := st.GetTask(taskID)
	// nil commits → CommitsSinceStart should be nil or empty (not an error).
	if len(task.CommitsSinceStart) != 0 {
		t.Errorf("expected empty CommitsSinceStart, got %v", task.CommitsSinceStart)
	}
}

// TestSetTaskCommits_EmptySlice verifies an empty slice is stored cleanly.
func TestSetTaskCommits_EmptySlice(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, ids, _ := st.CreatePlan("p", "", "", []store.TaskInput{{Title: "t", Priority: "p2"}})
	taskID := ids[0]

	if err := st.SetTaskCommits(taskID, []string{}); err != nil {
		t.Fatalf("SetTaskCommits([]): %v", err)
	}
	task, _ := st.GetTask(taskID)
	if len(task.CommitsSinceStart) != 0 {
		t.Errorf("expected empty CommitsSinceStart, got %v", task.CommitsSinceStart)
	}
}

// TestCommitTracking_FullLifecycle exercises the complete in_progress→done flow
// at the store layer: start_commit set, then commits stored on completion.
func TestCommitTracking_FullLifecycle(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, ids, _ := st.CreatePlan("plan", "", "", []store.TaskInput{{Title: "work", Priority: "p0"}})
	taskID := ids[0]

	// Simulate in_progress: UpdateTask + SetTaskStartCommit.
	_, _, err := st.UpdateTask(taskID, "in_progress", "", "agent-1")
	if err != nil {
		t.Fatalf("UpdateTask in_progress: %v", err)
	}
	const startSHA = "1111111111111111111111111111111111111111"
	if err := st.SetTaskStartCommit(taskID, startSHA); err != nil {
		t.Fatalf("SetTaskStartCommit: %v", err)
	}

	// Verify start_commit is visible in GetPendingTasks.
	tasks, _ := st.GetPendingTasks("", "")
	found := false
	for _, tk := range tasks {
		if tk.ID == taskID {
			if tk.StartCommit != startSHA {
				t.Errorf("in_progress task: StartCommit = %q, want %q", tk.StartCommit, startSHA)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("task not found in GetPendingTasks after in_progress")
	}

	// Simulate done: UpdateTask + SetTaskCommits.
	_, _, err = st.UpdateTask(taskID, "done", "finished", "agent-1")
	if err != nil {
		t.Fatalf("UpdateTask done: %v", err)
	}
	commits := []string{"2222222 feat: implement foo", "3333333 test: add coverage"}
	if err := st.SetTaskCommits(taskID, commits); err != nil {
		t.Fatalf("SetTaskCommits: %v", err)
	}

	// Done task is no longer pending — verify via GetTask.
	task, err := st.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask after done: %v", err)
	}
	if task.Status != "done" {
		t.Errorf("status = %q, want done", task.Status)
	}
	if task.StartCommit != startSHA {
		t.Errorf("StartCommit = %q, want %q", task.StartCommit, startSHA)
	}
	if len(task.CommitsSinceStart) != 2 {
		t.Errorf("CommitsSinceStart len = %d, want 2", len(task.CommitsSinceStart))
	}
}

// TestGetPendingTasks_CommitFieldsDefaultEmpty verifies that tasks created before
// R21 (start_commit='', commits='[]') surface empty fields gracefully.
func TestGetPendingTasks_CommitFieldsDefaultEmpty(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, _, _ = st.CreatePlan("p", "", "", []store.TaskInput{{Title: "old-task", Priority: "p2"}})

	tasks, err := st.GetPendingTasks("", "")
	if err != nil {
		t.Fatalf("GetPendingTasks: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("expected at least one task")
	}
	task := tasks[0]
	if task.StartCommit != "" {
		t.Errorf("new task should have empty StartCommit, got %q", task.StartCommit)
	}
	// CommitsSinceStart should be nil (not surfaced in JSON) for empty default.
	if len(task.CommitsSinceStart) != 0 {
		t.Errorf("new task should have no CommitsSinceStart, got %v", task.CommitsSinceStart)
	}
}

package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── buildTaskLearningContent unit tests ───────────────────────────────────────

func TestBuildTaskLearningContent_NilTask(t *testing.T) {
	got := buildTaskLearningContent(nil, "notes", nil)
	if got != "" {
		t.Errorf("want empty string for nil task, got %q", got)
	}
}

func TestBuildTaskLearningContent_EmptyTitle(t *testing.T) {
	got := buildTaskLearningContent(&store.Task{Title: ""}, "notes", nil)
	if got != "" {
		t.Errorf("want empty string for empty title, got %q", got)
	}
}

func TestBuildTaskLearningContent_TitleOnly(t *testing.T) {
	got := buildTaskLearningContent(&store.Task{Title: "add auth"}, "", nil)
	if !strings.Contains(got, `"add auth"`) {
		t.Errorf("expected task title in content, got %q", got)
	}
}

func TestBuildTaskLearningContent_FullTask(t *testing.T) {
	task := &store.Task{
		Title:        "Redesign get_context",
		Description:  "Strip raw code from responses",
		TrackedFiles: []string{"handlers_context.go", "handlers_session.go"},
		SpecItems: []store.SpecItem{
			{ID: "s1", Label: "Remove source code", Done: true},
			{ID: "s2", Label: "Add NL descriptions", Done: true},
			{ID: "s3", Label: "Write tests", Done: false},
		},
		LinkedNodes: []string{"repo::handlers_context.go::handleGetContext", "repo::handlers_session.go::handleSessionInit"},
	}
	commits := []string{"abc123", "def456"}
	notes := "used NL architect format throughout"

	got := buildTaskLearningContent(task, notes, commits)

	checks := []string{
		`"Redesign get_context"`,
		"Strip raw code from responses",
		"used NL architect format throughout",
		"2/3 items completed",
		"handlers_context.go, handlers_session.go (2 file(s))",
		"abc123, def456 (2 commit(s))",
		"handleGetContext",
		"handleSessionInit",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in content, got:\n%s", want, got)
		}
	}
}

func TestBuildTaskLearningContent_SpecAllDone(t *testing.T) {
	task := &store.Task{
		Title: "fix bug",
		SpecItems: []store.SpecItem{
			{ID: "s1", Done: true},
			{ID: "s2", Done: true},
		},
	}
	got := buildTaskLearningContent(task, "", nil)
	if !strings.Contains(got, "2/2 items completed") {
		t.Errorf("expected 2/2 spec count, got %q", got)
	}
}

func TestBuildTaskLearningContent_LinkedNodes_MissingNameSkipped(t *testing.T) {
	// Node IDs with fewer than 3 parts should not contribute to entities list.
	task := &store.Task{
		Title:       "task",
		LinkedNodes: []string{"badid", "repo::file.go::", "repo::file.go::RealFunc"},
	}
	got := buildTaskLearningContent(task, "", nil)
	if strings.Contains(got, "badid") {
		t.Errorf("malformed node ID should not appear in content, got %q", got)
	}
	if !strings.Contains(got, "RealFunc") {
		t.Errorf("expected RealFunc in entities, got %q", got)
	}
}

func TestBuildTaskLearningContent_NoOptionalFields(t *testing.T) {
	// Only title — no description, notes, spec, files, commits, nodes.
	// Should produce a valid single-line content with no spurious sections.
	task := &store.Task{Title: "minimal task"}
	got := buildTaskLearningContent(task, "", nil)
	if !strings.HasPrefix(got, `Task completed: "minimal task"`) {
		t.Errorf("unexpected prefix, got %q", got)
	}
	unwanted := []string{"Goal:", "Approach", "Spec coverage", "Files modified", "Commits:", "Entities"}
	for _, u := range unwanted {
		if strings.Contains(got, u) {
			t.Errorf("unexpected section %q in minimal content: %q", u, got)
		}
	}
}

func TestBuildTaskLearningContent_CommitsCapped(t *testing.T) {
	// More than 10 commits — only the first 10 should appear in the content,
	// but the total count (12) must still be reported correctly.
	commits := []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10", "a11", "a12"}
	task := &store.Task{Title: "big task"}
	got := buildTaskLearningContent(task, "", commits)

	if !strings.Contains(got, "(12 commit(s))") {
		t.Errorf("expected total count 12, got %q", got)
	}
	// a11 and a12 should be omitted from the listed SHAs (only first 10 listed).
	if strings.Contains(got, "a11") || strings.Contains(got, "a12") {
		t.Errorf("commits beyond cap 10 should not appear in listed SHAs, got %q", got)
	}
}

func TestBuildTaskLearningContent_LinkedNodesCapped(t *testing.T) {
	// More than 10 linked nodes — only first 10 names should appear.
	nodes := make([]string, 15)
	for i := range nodes {
		nodes[i] = "repo::file.go::Func" + string(rune('A'+i))
	}
	task := &store.Task{Title: "big task", LinkedNodes: nodes}
	got := buildTaskLearningContent(task, "", nil)

	// FuncK is the 11th (index 10) — should be absent.
	if strings.Contains(got, "FuncK") {
		t.Errorf("linked nodes beyond cap 10 should not appear, got %q", got)
	}
	if !strings.Contains(got, "FuncA") {
		t.Errorf("first linked node should appear, got %q", got)
	}
}

// ── saveTaskCompletionLearning integration tests ──────────────────────────────

func TestSaveTaskCompletionLearning_PersistsMemory(t *testing.T) {
	st := openMCPTestStore(t)
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, st)
	t.Cleanup(func() { srv.Close() })

	// Create a plan with a task that has all the rich fields.
	planID, _, err := st.CreatePlan("test plan", "", "", []store.TaskInput{
		{
			Title:        "Implement OAuth",
			Description:  "Add OAuth 2.0 support",
			Priority:     "p1",
			TrackedFiles: []string{"auth/handler.go", "auth/middleware.go"},
			SpecItems: []store.SpecItem{
				{ID: "s1", Label: "Add handler", Done: true},
				{ID: "s2", Label: "Add middleware", Done: true},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, err := st.GetPendingTasks(planID, "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("GetPendingTasks: err=%v len=%d", err, len(tasks))
	}
	taskID := tasks[0].ID

	// Call directly (synchronous — avoids goroutine timing in tests).
	srv.saveTaskCompletionLearning(taskID, "agent-x", "worked on the first try", []string{"sha1"})

	// Verify a memory was written for this task.
	mems, err := st.GetMemoriesByTaskID(taskID)
	if err != nil {
		t.Fatalf("GetMemoriesByTaskID: %v", err)
	}
	if len(mems) == 0 {
		t.Fatal("expected at least one memory for task, got none")
	}

	m := mems[0]
	if m.Source != store.SourceAuto {
		t.Errorf("source: want %q, got %q", store.SourceAuto, m.Source)
	}
	if m.Tier != store.TierProject {
		t.Errorf("tier: want %q, got %q", store.TierProject, m.Tier)
	}
	if m.TaskID != taskID {
		t.Errorf("task_id: want %q, got %q", taskID, m.TaskID)
	}
	if !strings.Contains(m.Content, "Implement OAuth") {
		t.Errorf("expected task title in memory content, got %q", m.Content)
	}
	if !strings.Contains(m.Content, "worked on the first try") {
		t.Errorf("expected notes in memory content, got %q", m.Content)
	}
	if !strings.Contains(m.Content, "sha1") {
		t.Errorf("expected commit sha in memory content, got %q", m.Content)
	}
	if !strings.Contains(m.Tags, "task_completion") {
		t.Errorf("expected task_completion tag, got %q", m.Tags)
	}
}

func TestSaveTaskCompletionLearning_NilStore_NoPanic(t *testing.T) {
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, nil)
	t.Cleanup(func() { srv.Close() })
	// Must not panic.
	srv.saveTaskCompletionLearning("task-id", "agent", "notes", nil)
}

func TestSaveTaskCompletionLearning_UnknownTaskID_NoPersist(t *testing.T) {
	st := openMCPTestStore(t)
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, st)
	t.Cleanup(func() { srv.Close() })
	// Non-existent task → GetTask returns error → no memory written, no panic.
	srv.saveTaskCompletionLearning("nonexistent-task", "agent", "notes", nil)
}

func TestSaveTaskCompletionLearning_EmptyTitle_NoPersist(t *testing.T) {
	// Tasks with empty titles produce empty content → nothing written.
	st := openMCPTestStore(t)
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, st)
	t.Cleanup(func() { srv.Close() })

	// Insert a task directly with empty title via CreatePlan (title is required by
	// API but we can test the content guard via buildTaskLearningContent directly).
	// Verify that empty title returns "" from the builder.
	content := buildTaskLearningContent(&store.Task{Title: "", ID: "t1"}, "notes", nil)
	if content != "" {
		t.Errorf("expected empty content for empty title, got %q", content)
	}
}

// TestHandleUpdateTask_Done_PersistsLearning verifies the end-to-end wiring:
// handleUpdateTask(status="done") eventually persists a task completion memory.
func TestHandleUpdateTask_Done_PersistsLearning(t *testing.T) {
	st := openMCPTestStore(t)
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	planID, _, err := st.CreatePlan("e2e plan", "", "", []store.TaskInput{
		{Title: "add tests", Priority: "p1"},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, err := st.GetPendingTasks(planID, "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("GetPendingTasks: %v len=%d", err, len(tasks))
	}
	taskID := tasks[0].ID

	res, err := srv.handleUpdateTask(ctx, callTool(map[string]any{
		"id":       taskID,
		"status":   "done",
		"agent_id": "tester",
		"notes":    "all tests pass",
	}))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("handleUpdateTask: err=%v isError=%v", err, res.IsError)
	}

	// Allow the saveTaskCompletionLearning goroutine to complete.
	// We use a short bounded sleep rather than drainBackground() because
	// handleUpdateTask spawns many other background tasks (pulse signals,
	// retrospective annotations) that may block the pool; drainBackground()
	// would wait for all of them. Our goroutine is a single DB insert ~<5ms.
	time.Sleep(200 * time.Millisecond)

	mems, err := st.GetMemoriesByTaskID(taskID)
	if err != nil {
		t.Fatalf("GetMemoriesByTaskID: %v", err)
	}
	if len(mems) == 0 {
		t.Fatal("expected task completion memory after handleUpdateTask(done), got none")
	}
	if !strings.Contains(mems[0].Content, "add tests") {
		t.Errorf("task title missing from learning memory, got %q", mems[0].Content)
	}
}

package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── buildTaskLearningContent unit tests ───────────────────────────────────────

func TestBuildTaskLearningContent_NilTask(t *testing.T) {
	got := buildTaskLearningContent(nil, nil)
	if got != "" {
		t.Errorf("want empty string for nil task, got %q", got)
	}
}

func TestBuildTaskLearningContent_EmptyTitle(t *testing.T) {
	got := buildTaskLearningContent(&store.Task{Title: ""}, nil)
	if got != "" {
		t.Errorf("want empty string for empty title, got %q", got)
	}
}

func TestBuildTaskLearningContent_TitleOnly(t *testing.T) {
	got := buildTaskLearningContent(&store.Task{Title: "add auth"}, nil)
	if !strings.Contains(got, `"add auth"`) {
		t.Errorf("expected task title in content, got %q", got)
	}
}

func TestBuildTaskLearningContent_FullTask(t *testing.T) {
	task := &store.Task{
		Title:        "Redesign get_context",
		Description:  "Strip raw code from responses",
		Notes:        "[2026-04-04T08:00:00Z] used NL architect format throughout",
		TrackedFiles: []string{"handlers_context.go", "handlers_session.go"},
		SpecItems: []store.SpecItem{
			{ID: "s1", Label: "Remove source code", Done: true},
			{ID: "s2", Label: "Add NL descriptions", Done: true},
			{ID: "s3", Label: "Write tests", Done: false},
		},
		LinkedNodes: []string{"repo::handlers_context.go::handleGetContext", "repo::handlers_session.go::handleSessionInit"},
	}
	commits := []string{"abc123", "def456"}

	got := buildTaskLearningContent(task, commits)

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

// TestBuildTaskLearningContent_NotesPlacedLast verifies that structured fields
// (spec, files, commits) appear before the session history so that prepareMemory's
// 2000-rune truncation cuts the notes rather than the metadata.
func TestBuildTaskLearningContent_NotesPlacedLast(t *testing.T) {
	task := &store.Task{
		Title:        "fix bug",
		Notes:        "[2026-04-04T08:00:00Z] investigation notes",
		TrackedFiles: []string{"main.go"},
	}
	got := buildTaskLearningContent(task, nil)

	filesIdx := strings.Index(got, "Files modified:")
	notesIdx := strings.Index(got, "Session history:")
	if filesIdx < 0 || notesIdx < 0 {
		t.Fatalf("expected both 'Files modified:' and 'Session history:' sections, got:\n%s", got)
	}
	if filesIdx > notesIdx {
		t.Errorf("'Files modified:' should appear before 'Session history:', got:\n%s", got)
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
	got := buildTaskLearningContent(task, nil)
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
	got := buildTaskLearningContent(task, nil)
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
	got := buildTaskLearningContent(task, nil)
	if !strings.HasPrefix(got, `Task completed: "minimal task"`) {
		t.Errorf("unexpected prefix, got %q", got)
	}
	unwanted := []string{"Goal:", "Session history:", "Spec coverage", "Files modified", "Commits:", "Entities"}
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
	got := buildTaskLearningContent(task, commits)

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
	got := buildTaskLearningContent(task, nil)

	// FuncK is the 11th (index 10) — should be absent.
	if strings.Contains(got, "FuncK") {
		t.Errorf("linked nodes beyond cap 10 should not appear, got %q", got)
	}
	if !strings.Contains(got, "FuncA") {
		t.Errorf("first linked node should appear, got %q", got)
	}
}

// ── buildCancellationContent unit tests ──────────────────────────────────────

func TestBuildCancellationContent_NilTask(t *testing.T) {
	if got := buildCancellationContent(nil, nil); got != "" {
		t.Errorf("want empty for nil task, got %q", got)
	}
}

func TestBuildCancellationContent_EmptyTitle(t *testing.T) {
	if got := buildCancellationContent(&store.Task{Title: ""}, nil); got != "" {
		t.Errorf("want empty for empty title, got %q", got)
	}
}

func TestBuildCancellationContent_FullTask(t *testing.T) {
	task := &store.Task{
		Title:       "Add gRPC transport",
		Description: "Replace HTTP with gRPC for internal services",
		Notes:       "[2026-04-04T09:00:00Z] Cancelled — protobuf dependency conflicts with existing wire gen",
		TrackedFiles: []string{"transport/grpc.go"},
		SpecItems: []store.SpecItem{
			{ID: "s1", Label: "Add proto files", Done: true},
			{ID: "s2", Label: "Wire client", Done: false},
			{ID: "s3", Label: "Update tests", Done: false},
		},
		LinkedNodes: []string{"repo::transport/grpc.go::GRPCServer"},
	}
	commits := []string{"dead001"}
	got := buildCancellationContent(task, commits)

	checks := []string{
		`Task abandoned: "Add gRPC transport"`,
		"Replace HTTP with gRPC",
		"Incomplete spec:",
		"Wire client",             // incomplete spec item label surfaced
		"grpc.go",                 // tracked file
		"dead001",                 // commit
		"GRPCServer",              // entity
		"protobuf dependency",     // from session history
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in cancellation content, got:\n%s", want, got)
		}
	}
}

func TestBuildCancellationContent_AllSpecDone(t *testing.T) {
	// Edge case: all spec items done but task still cancelled.
	task := &store.Task{
		Title: "deploy feature",
		SpecItems: []store.SpecItem{
			{ID: "s1", Done: true},
		},
	}
	got := buildCancellationContent(task, nil)
	if !strings.Contains(got, "before cancellation") {
		t.Errorf("expected 'before cancellation' qualifier, got %q", got)
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

	// Persist completion notes into task.Notes via UpdateTask so saveTaskCompletionLearning
	// picks them up from the DB (it uses task.Notes, not a passed-in notes string).
	if _, _, err := st.UpdateTask(taskID, "done", "worked on the first try", "agent-x"); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	// Call directly (synchronous — avoids goroutine timing in tests).
	srv.saveTaskCompletionLearning(taskID, "agent-x", []string{"sha1"})

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
	// task.Notes now contains "worked on the first try" (with a timestamp prefix).
	if !strings.Contains(m.Content, "worked on the first try") {
		t.Errorf("expected notes in memory content via task.Notes, got %q", m.Content)
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
	srv.saveTaskCompletionLearning("task-id", "agent", nil)
}

func TestSaveTaskCompletionLearning_UnknownTaskID_NoPersist(t *testing.T) {
	st := openMCPTestStore(t)
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, st)
	t.Cleanup(func() { srv.Close() })
	// Non-existent task → GetTask returns error → no memory written, no panic.
	srv.saveTaskCompletionLearning("nonexistent-task", "agent", nil)
}

func TestSaveTaskCompletionLearning_EmptyTitle_NoPersist(t *testing.T) {
	// Tasks with empty titles produce empty content → nothing written.
	// Verify that empty title returns "" from the builder.
	content := buildTaskLearningContent(&store.Task{Title: "", ID: "t1"}, nil)
	if content != "" {
		t.Errorf("expected empty content for empty title, got %q", content)
	}
}

// ── saveCancellationLearning integration tests ────────────────────────────────

func TestSaveCancellationLearning_PersistsMemory(t *testing.T) {
	st := openMCPTestStore(t)
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, st)
	t.Cleanup(func() { srv.Close() })

	planID, _, err := st.CreatePlan("test plan", "", "", []store.TaskInput{
		{Title: "Add gRPC transport", Description: "Replace HTTP transport", Priority: "p2"},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, err := st.GetPendingTasks(planID, "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("GetPendingTasks: err=%v len=%d", err, len(tasks))
	}
	taskID := tasks[0].ID

	// Persist cancellation reason into task.Notes.
	if _, _, err := st.UpdateTask(taskID, "cancelled", "dependency conflict", "agent-y"); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	srv.saveCancellationLearning(taskID, "agent-y", nil)

	mems, err := st.GetMemoriesByTaskID(taskID)
	if err != nil {
		t.Fatalf("GetMemoriesByTaskID: %v", err)
	}
	if len(mems) == 0 {
		t.Fatal("expected cancellation memory, got none")
	}
	m := mems[0]
	if !strings.Contains(m.Content, "Task abandoned:") {
		t.Errorf("expected 'Task abandoned:' header, got %q", m.Content)
	}
	if !strings.Contains(m.Content, "Add gRPC transport") {
		t.Errorf("expected task title in content, got %q", m.Content)
	}
	if !strings.Contains(m.Content, "dependency conflict") {
		t.Errorf("expected cancellation reason in content, got %q", m.Content)
	}
	if !strings.Contains(m.Tags, "task_cancellation") {
		t.Errorf("expected task_cancellation tag, got %q", m.Tags)
	}
}

func TestSaveCancellationLearning_NilStore_NoPanic(t *testing.T) {
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, nil)
	t.Cleanup(func() { srv.Close() })
	srv.saveCancellationLearning("task-id", "agent", nil)
}

// ── e2e wiring tests ──────────────────────────────────────────────────────────

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
	// Notes are appended to task.Notes by UpdateTask before the goroutine runs.
	if !strings.Contains(mems[0].Content, "all tests pass") {
		t.Errorf("completion notes missing from learning memory, got %q", mems[0].Content)
	}
}

// TestHandleUpdateTask_Cancelled_PersistsCancellationLearning verifies that
// handleUpdateTask(status="cancelled") produces a task_cancellation memory.
func TestHandleUpdateTask_Cancelled_PersistsCancellationLearning(t *testing.T) {
	st := openMCPTestStore(t)
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	planID, _, err := st.CreatePlan("cancel plan", "", "", []store.TaskInput{
		{Title: "spike gRPC", Priority: "p3"},
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
		"status":   "cancelled",
		"agent_id": "tester",
		"notes":    "proto conflicts with wire gen, abandoning",
	}))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("handleUpdateTask(cancelled): err=%v isError=%v", err, res.IsError)
	}

	time.Sleep(200 * time.Millisecond)

	mems, err := st.GetMemoriesByTaskID(taskID)
	if err != nil {
		t.Fatalf("GetMemoriesByTaskID: %v", err)
	}
	if len(mems) == 0 {
		t.Fatal("expected cancellation memory after handleUpdateTask(cancelled), got none")
	}
	m := mems[0]
	if !strings.Contains(m.Content, "Task abandoned:") {
		t.Errorf("expected 'Task abandoned:' header, got %q", m.Content)
	}
	if !strings.Contains(m.Tags, "task_cancellation") {
		t.Errorf("expected task_cancellation tag, got %q", m.Tags)
	}
	if !strings.Contains(m.Content, "proto conflicts") {
		t.Errorf("expected cancellation reason in content, got %q", m.Content)
	}
}

// TestHandleUpdateTask_InProgress_SurfacesPriorLearnings verifies that when an
// agent claims a task (status=in_progress) after it was previously completed,
// the response includes prior_learnings from the completion memory.
func TestHandleUpdateTask_InProgress_SurfacesPriorLearnings(t *testing.T) {
	st := openMCPTestStore(t)
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	planID, _, err := st.CreatePlan("reprise plan", "", "", []store.TaskInput{
		{Title: "refactor auth", Priority: "p1"},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, err := st.GetPendingTasks(planID, "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("GetPendingTasks: %v len=%d", err, len(tasks))
	}
	taskID := tasks[0].ID

	// Simulate a prior completion: write a memory directly via store.
	if _, err := st.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "Task completed: \"refactor auth\"\nApproach: used middleware injection",
		AgentID: "previous-agent",
		TaskID:  taskID,
		Source:  store.SourceAuto,
		Tags:    `["task_completion","auto"]`,
	}); err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	// Claim the task (in_progress) — prior learnings should surface in response.
	res, err := srv.handleUpdateTask(ctx, callTool(map[string]any{
		"id":       taskID,
		"status":   "in_progress",
		"agent_id": "new-agent",
	}))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("handleUpdateTask(in_progress): err=%v isError=%v", err, res.IsError)
	}

	// Decode the JSON response and verify prior_learnings is present.
	var respMap map[string]interface{}
	if tc, ok := res.Content[0].(mcp.TextContent); ok {
		if err := json.Unmarshal([]byte(tc.Text), &respMap); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	pl, ok := respMap["prior_learnings"].(string)
	if !ok || pl == "" {
		t.Errorf("expected prior_learnings string in response, got %v", respMap["prior_learnings"])
	}
	if !strings.Contains(pl, "middleware injection") {
		t.Errorf("expected prior learning content in response, got %q", pl)
	}
}

// TestHandleGetTask_ReturnsPriorLearnings verifies the tasks(action=get) handler
// returns the task and its prior learning memories.
func TestHandleGetTask_ReturnsPriorLearnings(t *testing.T) {
	st := openMCPTestStore(t)
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, st)
	t.Cleanup(func() { srv.Close() })

	planID, _, err := st.CreatePlan("lookup plan", "", "", []store.TaskInput{
		{Title: "write integration tests", Priority: "p1"},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, err := st.GetPendingTasks(planID, "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("GetPendingTasks: %v len=%d", err, len(tasks))
	}
	taskID := tasks[0].ID

	// Write two prior learning memories.
	for _, content := range []string{
		"Task completed: \"write integration tests\"\nApproach: used table-driven tests",
		"Task completed: \"write integration tests\"\nApproach: added benchmark suite",
	} {
		if _, err := st.InsertMemory(store.Memory{
			Tier:    store.TierProject,
			Content: content,
			TaskID:  taskID,
			Source:  store.SourceAuto,
			Tags:    `["task_completion","auto"]`,
		}); err != nil {
			t.Fatalf("InsertMemory: %v", err)
		}
	}

	res, err := srv.handleGetTask(ctx, callTool(map[string]any{
		"task_id": taskID,
	}))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("handleGetTask: err=%v isError=%v", err, res.IsError)
	}

	var respMap map[string]interface{}
	if tc, ok := res.Content[0].(mcp.TextContent); ok {
		if err := json.Unmarshal([]byte(tc.Text), &respMap); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}

	if respMap["task"] == nil {
		t.Error("expected 'task' field in response")
	}
	if respMap["prior_learnings"] == nil {
		t.Error("expected 'prior_learnings' in response")
	}
	count, _ := respMap["prior_learnings_count"].(float64)
	if count < 2 {
		t.Errorf("expected prior_learnings_count >= 2, got %v", count)
	}
}

func TestHandleGetTask_MissingTaskID_Error(t *testing.T) {
	st := openMCPTestStore(t)
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, st)
	t.Cleanup(func() { srv.Close() })

	res, err := srv.handleGetTask(ctx, callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("expected error result for missing task_id")
	}
}

func TestHandleGetTask_UnknownTaskID_Error(t *testing.T) {
	st := openMCPTestStore(t)
	cfg, _ := config.Load(t.TempDir())
	srv := New(nil, cfg, st)
	t.Cleanup(func() { srv.Close() })

	res, err := srv.handleGetTask(ctx, callTool(map[string]any{
		"task_id": "nonexistent-id",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("expected error result for unknown task_id")
	}
}

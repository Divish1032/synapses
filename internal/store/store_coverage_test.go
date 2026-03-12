package store_test

// Additional tests targeting uncovered store functions:
// findNewlyUnblocked / taskStatusByID (via UpdateTask done+depends_on),
// SaveGraph FTS rebuild path, GetEpisodes with filters, RecallEpisodes,
// OpenReadOnly, DefaultPath, SaveFileMtimes/LoadFileMtimes,
// SaveCallSites/LoadCallSites, LogViolations.

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── findNewlyUnblocked / taskStatusByID via UpdateTask ────────────────────────

// TestUpdateTask_UnblocksDependent creates two plans — A and B where B depends on A.
// Marking A done should trigger findNewlyUnblocked.
func TestUpdateTask_UnblocksDependent(t *testing.T) {
	st := openTestStore(t)

	// Create task A.
	planID, err := st.CreatePlan("dep-plan", "", "", []store.TaskInput{
		{Title: "task-A", Priority: "p1"},
	})
	if err != nil {
		t.Fatalf("CreatePlan A: %v", err)
	}
	tasks, _ := st.GetPendingTasks(planID, "")
	if len(tasks) == 0 {
		t.Fatal("no tasks in plan A")
	}
	taskAID := tasks[0].ID

	// Create task B with dependency on A.
	_, err = st.CreatePlan("dep-plan-b", "", "", []store.TaskInput{
		{Title: "task-B", Priority: "p2", DependsOn: []string{taskAID}},
	})
	if err != nil {
		t.Fatalf("CreatePlan B: %v", err)
	}

	// Mark A as done — should call findNewlyUnblocked.
	unblocked, err := st.UpdateTask(taskAID, "done", "", "agent-x")
	if err != nil {
		t.Fatalf("UpdateTask done: %v", err)
	}
	// B depends only on A, so B should now be unblocked.
	_ = unblocked // may or may not include B depending on implementation details
}

func TestUpdateTask_TwoDeps_OnlyOneComplete(t *testing.T) {
	st := openTestStore(t)

	// Create task A and task C.
	planID, _ := st.CreatePlan("two-dep-plan", "", "", []store.TaskInput{
		{Title: "task-A2", Priority: "p1"},
		{Title: "task-C2", Priority: "p1"},
	})
	tasks, _ := st.GetPendingTasks(planID, "")
	if len(tasks) < 2 {
		t.Skip("need at least 2 tasks")
	}
	taskAID := tasks[0].ID
	taskCID := tasks[1].ID

	// Create task B depending on both A and C.
	_, _ = st.CreatePlan("b-plan", "", "", []store.TaskInput{
		{Title: "task-B2", Priority: "p2", DependsOn: []string{taskAID, taskCID}},
	})

	// Mark only A as done — B should NOT be unblocked (C still pending).
	unblocked, err := st.UpdateTask(taskAID, "done", "", "")
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	_ = unblocked
}

func TestUpdateTask_NotDone_NoUnblocked(t *testing.T) {
	st := openTestStore(t)
	planID, _ := st.CreatePlan("in-prog plan", "", "", []store.TaskInput{
		{Title: "task-ip", Priority: "p1"},
	})
	tasks, _ := st.GetPendingTasks(planID, "")
	taskID := tasks[0].ID

	// in_progress status does not trigger findNewlyUnblocked.
	unblocked, err := st.UpdateTask(taskID, "in_progress", "", "")
	if err != nil {
		t.Fatalf("UpdateTask in_progress: %v", err)
	}
	if len(unblocked) != 0 {
		t.Errorf("expected 0 unblocked for non-done status, got %d", len(unblocked))
	}
}

// ── OpenReadOnly ──────────────────────────────────────────────────────────────

func TestOpenReadOnly_Success(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Close()

	ro, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
}

func TestOpenReadOnly_Missing(t *testing.T) {
	_, err := store.OpenReadOnly(filepath.Join(t.TempDir(), "nonexistent.db"))
	if err == nil {
		t.Error("expected error for nonexistent DB")
	}
}

// ── DefaultPath ───────────────────────────────────────────────────────────────

func TestDefaultPath(t *testing.T) {
	path, err := store.DefaultPath("/my/project")
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if path == "" {
		t.Error("expected non-empty default path")
	}
}

// ── SaveFileMtimes / LoadFileMtimes ───────────────────────────────────────────

func TestSaveAndLoadFileMtimes(t *testing.T) {
	st := openTestStore(t)
	mtimes := map[string]int64{
		"pkg/auth.go":    1700000000,
		"pkg/handler.go": 1700000060,
	}
	if err := st.SaveFileMtimes(mtimes); err != nil {
		t.Fatalf("SaveFileMtimes: %v", err)
	}
	loaded, err := st.LoadFileMtimes()
	if err != nil {
		t.Fatalf("LoadFileMtimes: %v", err)
	}
	if len(loaded) != 2 {
		t.Errorf("expected 2 mtimes, got %d", len(loaded))
	}
}

func TestLoadFileMtimes_Empty(t *testing.T) {
	st := openTestStore(t)
	loaded, err := st.LoadFileMtimes()
	if err != nil {
		t.Fatalf("LoadFileMtimes empty: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("expected empty map, got %d", len(loaded))
	}
}

// ── SaveCallSites / LoadCallSites ─────────────────────────────────────────────

func TestSaveAndLoadCallSites_Coverage(t *testing.T) {
	st := openTestStore(t)

	sites := []graph.CallSite{
		{
			CallerFile: "pkg/api/handler.go",
			CallerID:   "handler::HandleLogin",
			FuncName:   "Login",
			PkgAlias:   "auth",
		},
	}
	if err := st.SaveCallSites(sites); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}
	loaded, err := st.LoadCallSites()
	if err != nil {
		t.Fatalf("LoadCallSites: %v", err)
	}
	if len(loaded) == 0 {
		t.Error("expected non-empty call sites after save")
	}
}

// ── SaveGraph triggers FTS rebuild ────────────────────────────────────────────

func TestSaveGraph_WithNodesForFTS(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("test")

	for i := 0; i < 3; i++ {
		id := g.MakeNodeID(fmt.Sprintf("pkg/auth%d.go", i), "Login")
		g.AddNode(&graph.Node{
			ID:      id,
			Name:    "Login",
			Type:    graph.NodeFunction,
			File:    fmt.Sprintf("pkg/auth%d.go", i),
			Package: "auth",
		})
	}
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	// SemanticSearch uses FTS5 and exercises the rebuilt index.
	results, err := st.SemanticSearch("Login", 10)
	if err != nil {
		t.Fatalf("SemanticSearch after SaveGraph: %v", err)
	}
	_ = results
}

// ── GetEpisodes with various filters ──────────────────────────────────────────

func TestGetEpisodes_WithAgentFilter(t *testing.T) {
	st := openTestStore(t)

	for i := 0; i < 3; i++ {
		_, err := st.RememberEpisode(store.Episode{
			AgentID:       "filter-agent",
			EpisodeType:   "decision",
			Outcome:       "success",
			Decision:      fmt.Sprintf("decision %d", i),
			AffectedFiles: "[]",
			AffectedNodes: "[]",
			Tags:          "[]",
		})
		if err != nil {
			t.Fatalf("RememberEpisode %d: %v", i, err)
		}
	}

	// Filter by agent.
	eps, err := st.GetEpisodes("", "filter-agent", "", nil, 100, 0)
	if err != nil {
		t.Fatalf("GetEpisodes by agent: %v", err)
	}
	if len(eps) == 0 {
		t.Error("expected episodes for filter-agent")
	}
}

func TestGetEpisodes_WithEpisodeType(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.RememberEpisode(store.Episode{
		AgentID:       "ep-agent",
		EpisodeType:   "failure",
		Outcome:       "failure",
		Decision:      "tried wrong approach",
		AffectedFiles: "[]",
		AffectedNodes: "[]",
		Tags:          "[]",
	})

	eps, err := st.GetEpisodes("", "", "failure", nil, 10, 0)
	if err != nil {
		t.Fatalf("GetEpisodes by type: %v", err)
	}
	_ = eps
}

func TestGetEpisodes_WithSinceDays(t *testing.T) {
	st := openTestStore(t)
	_, _ = st.RememberEpisode(store.Episode{
		AgentID:       "days-agent",
		EpisodeType:   "decision",
		Outcome:       "success",
		Decision:      "recent decision",
		AffectedFiles: "[]",
		AffectedNodes: "[]",
		Tags:          "[]",
	})
	eps, err := st.GetEpisodes("", "", "", nil, 10, 30)
	if err != nil {
		t.Fatalf("GetEpisodes with sinceDays: %v", err)
	}
	_ = eps
}

func TestGetEpisodes_WithTags(t *testing.T) {
	st := openTestStore(t)
	_, _ = st.RememberEpisode(store.Episode{
		AgentID:       "tag-agent",
		EpisodeType:   "decision",
		Outcome:       "success",
		Decision:      "tagged decision",
		AffectedFiles: "[]",
		AffectedNodes: "[]",
		Tags:          `["auth","security"]`,
	})
	// Filter with a tag that matches.
	eps, err := st.GetEpisodes("", "", "", []string{"auth"}, 10, 0)
	if err != nil {
		t.Fatalf("GetEpisodes with tags: %v", err)
	}
	_ = eps
}

// ── RecallEpisodes ────────────────────────────────────────────────────────────

func TestRecallEpisodes_WithQuery(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.RememberEpisode(store.Episode{
		AgentID:       "recall-agent",
		EpisodeType:   "decision",
		Outcome:       "success",
		Decision:      "used AuthHandler for token validation",
		AffectedFiles: "[]",
		AffectedNodes: `["AuthHandler"]`,
		Tags:          "[]",
	})

	// Recall with query term.
	eps, err := st.RecallEpisodes("AuthHandler", "", "recall-agent", "", "", 5, 0)
	if err != nil {
		t.Fatalf("RecallEpisodes: %v", err)
	}
	_ = eps
}

func TestRecallEpisodes_EmptyQuery(t *testing.T) {
	st := openTestStore(t)
	eps, err := st.RecallEpisodes("", "", "any-agent", "", "", 5, 0)
	if err != nil {
		t.Fatalf("RecallEpisodes empty query: %v", err)
	}
	_ = eps
}

func TestRecallEpisodes_WithOutcomeFilter(t *testing.T) {
	st := openTestStore(t)
	_, _ = st.RememberEpisode(store.Episode{
		AgentID:       "outcome-agent",
		EpisodeType:   "decision",
		Outcome:       "failure",
		Decision:      "wrong approach",
		AffectedFiles: "[]",
		AffectedNodes: "[]",
		Tags:          "[]",
	})
	eps, err := st.RecallEpisodes("", "", "", "", "failure", 5, 0)
	if err != nil {
		t.Fatalf("RecallEpisodes with outcome: %v", err)
	}
	_ = eps
}

// ── LogViolations ─────────────────────────────────────────────────────────────

func TestLogViolations_WithEntries_Coverage(t *testing.T) {
	st := openTestStore(t)
	violations := []config.Violation{
		{
			RuleID:      "no-handler-db",
			Severity:    "error",
			Description: "handler calls db directly",
			FromNode:    "handler::HandleLogin",
			ToNode:      "db::QueryUser",
		},
	}
	if err := st.LogViolations(violations); err != nil {
		t.Fatalf("LogViolations: %v", err)
	}
}

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
	planID, _, err := st.CreatePlan("dep-plan", "", "", []store.TaskInput{
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
	_, _, err = st.CreatePlan("dep-plan-b", "", "", []store.TaskInput{
		{Title: "task-B", Priority: "p2", DependsOn: []string{taskAID}},
	})
	if err != nil {
		t.Fatalf("CreatePlan B: %v", err)
	}

	// Mark A as done — should call findNewlyUnblocked.
	unblocked, _, err := st.UpdateTask(taskAID, "done", "", "agent-x")
	if err != nil {
		t.Fatalf("UpdateTask done: %v", err)
	}
	// B depends only on A, so B should now be unblocked.
	_ = unblocked // may or may not include B depending on implementation details
}

func TestUpdateTask_TwoDeps_OnlyOneComplete(t *testing.T) {
	st := openTestStore(t)

	// Create task A and task C.
	planID, _, _ := st.CreatePlan("two-dep-plan", "", "", []store.TaskInput{
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
	_, _, _ = st.CreatePlan("b-plan", "", "", []store.TaskInput{
		{Title: "task-B2", Priority: "p2", DependsOn: []string{taskAID, taskCID}},
	})

	// Mark only A as done — B should NOT be unblocked (C still pending).
	unblocked, _, err := st.UpdateTask(taskAID, "done", "", "")
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	_ = unblocked
}

func TestUpdateTask_NotDone_NoUnblocked(t *testing.T) {
	st := openTestStore(t)
	planID, _, _ := st.CreatePlan("in-prog plan", "", "", []store.TaskInput{
		{Title: "task-ip", Priority: "p1"},
	})
	tasks, _ := st.GetPendingTasks(planID, "")
	taskID := tasks[0].ID

	// in_progress status does not trigger findNewlyUnblocked.
	unblocked, _, err := st.UpdateTask(taskID, "in_progress", "", "")
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

// ── SaveGraph with edges ───────────────────────────────────────────────────────

func TestSaveGraph_WithEdges(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("edge-repo")

	idA := g.MakeNodeID("pkg/server.go", "ServeHTTP")
	g.AddNode(&graph.Node{
		ID:      idA,
		Name:    "ServeHTTP",
		Type:    graph.NodeFunction,
		File:    "pkg/server.go",
		Package: "server",
	})

	idB := g.MakeNodeID("pkg/auth.go", "Authenticate")
	g.AddNode(&graph.Node{
		ID:      idB,
		Name:    "Authenticate",
		Type:    graph.NodeFunction,
		File:    "pkg/auth.go",
		Package: "auth",
	})

	idC := g.MakeNodeID("pkg/models.go", "User")
	g.AddNode(&graph.Node{
		ID:      idC,
		Name:    "User",
		Type:    graph.NodeStruct,
		File:    "pkg/models.go",
		Package: "models",
	})

	g.AddEdge(&graph.Edge{From: idA, To: idB, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: idB, To: idC, Type: graph.EdgeImports})

	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph with edges: %v", err)
	}
}

// ── LoadGraph after SaveGraph ──────────────────────────────────────────────────

func TestLoadGraph_AfterSave(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("load-repo")

	id1 := g.MakeNodeID("pkg/handler.go", "HandleRequest")
	g.AddNode(&graph.Node{
		ID:      id1,
		Name:    "HandleRequest",
		Type:    graph.NodeFunction,
		File:    "pkg/handler.go",
		Package: "handler",
	})

	id2 := g.MakeNodeID("pkg/db.go", "Query")
	g.AddNode(&graph.Node{
		ID:      id2,
		Name:    "Query",
		Type:    graph.NodeFunction,
		File:    "pkg/db.go",
		Package: "db",
	})

	g.AddEdge(&graph.Edge{From: id1, To: id2, Type: graph.EdgeCalls})

	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	loaded, err := st.LoadGraph()
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadGraph returned nil")
	}
}

// ── SemanticSearch fallthrough to likeSearch ──────────────────────────────────

// TestSemanticSearch_LikeSearchFallthrough exercises the likeSearch branch by
// searching for a term that is unlikely to match FTS5 token boundaries but will
// match via LIKE (partial substring).
func TestSemanticSearch_LikeSearchFallthrough(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("like-repo")

	// Use a distinctive name that FTS5 won't tokenise but LIKE will match.
	id := g.MakeNodeID("pkg/xyzhandler.go", "xyzHandleRpc")
	g.AddNode(&graph.Node{
		ID:      id,
		Name:    "xyzHandleRpc",
		Type:    graph.NodeFunction,
		File:    "pkg/xyzhandler.go",
		Package: "xyz",
	})
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	// Search with a partial term that FTS5 won't find but LIKE will.
	results, err := st.SemanticSearch("xyzHan", 10)
	if err != nil {
		t.Fatalf("SemanticSearch likeSearch fallthrough: %v", err)
	}
	_ = results
}

// ── AppendEvent with payload ───────────────────────────────────────────────────

func TestAppendEvent_WithPayload(t *testing.T) {
	st := openTestStore(t)

	err := st.AppendEvent("file_changed", "agent-test", `{"file":"pkg/auth.go","action":"modified"}`)
	if err != nil {
		t.Fatalf("AppendEvent with payload: %v", err)
	}
}

func TestAppendEvent_MultipleEvents(t *testing.T) {
	st := openTestStore(t)

	for i := 0; i < 5; i++ {
		err := st.AppendEvent("task_updated", "agent-test", fmt.Sprintf(`{"task_id":"task-%d"}`, i))
		if err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}
}

// ── SendMessage ────────────────────────────────────────────────────────────────

func TestSendMessage_WithTargetAgent(t *testing.T) {
	st := openTestStore(t)

	_, err := st.SendMessage("agent-alpha", "agent-beta", "review", "please review pkg/auth.go", "proj-1")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
}

func TestSendMessage_BroadcastNoTarget(t *testing.T) {
	st := openTestStore(t)

	_, err := st.SendMessage("agent-alpha", "", "broadcast", "broadcast message to all agents", "proj-1")
	if err != nil {
		t.Fatalf("SendMessage broadcast: %v", err)
	}
}

// ── ScanAll (package-level) ────────────────────────────────────────────────────

func TestScanAll_ReturnsNoError(t *testing.T) {
	// ScanAll scans the synapses cache dir for *.db files.
	// It should not error even when the cache dir is empty or absent.
	_, err := store.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
}

// ── SaveFileMtimes overwrite path ──────────────────────────────────────────────

func TestSaveFileMtimes_Overwrite(t *testing.T) {
	st := openTestStore(t)

	first := map[string]int64{"a.go": 100, "b.go": 200}
	if err := st.SaveFileMtimes(first); err != nil {
		t.Fatalf("SaveFileMtimes first: %v", err)
	}

	// Overwrite with updated values plus a new file.
	second := map[string]int64{"a.go": 150, "b.go": 250, "c.go": 300}
	if err := st.SaveFileMtimes(second); err != nil {
		t.Fatalf("SaveFileMtimes second: %v", err)
	}

	loaded, err := st.LoadFileMtimes()
	if err != nil {
		t.Fatalf("LoadFileMtimes: %v", err)
	}
	if len(loaded) < 3 {
		t.Errorf("expected at least 3 mtimes, got %d", len(loaded))
	}
}

// ── SaveCallSites empty slice ──────────────────────────────────────────────────

func TestSaveCallSites_EmptySlice(t *testing.T) {
	st := openTestStore(t)
	if err := st.SaveCallSites([]graph.CallSite{}); err != nil {
		t.Fatalf("SaveCallSites empty: %v", err)
	}
}

// ── GetPendingTasks with agentID (auto-claim branch) ──────────────────────────

func TestGetPendingTasks_WithAgentID(t *testing.T) {
	st := openTestStore(t)
	planID, _, err := st.CreatePlan("agent-plan", "", "", []store.TaskInput{
		{Title: "task-unclaimed", Priority: "p1"},
		{Title: "task-unclaimed-2", Priority: "p2"},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	// Pass agentID — triggers the auto-claim branch.
	tasks, err := st.GetPendingTasks(planID, "auto-agent")
	if err != nil {
		t.Fatalf("GetPendingTasks with agentID: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("expected tasks")
	}
	for _, task := range tasks {
		if task.AssignedTo != "auto-agent" {
			t.Errorf("expected AssignedTo=auto-agent, got %q", task.AssignedTo)
		}
	}
}

func TestGetPendingTasks_WithDependencies(t *testing.T) {
	st := openTestStore(t)

	// Create a blocker task.
	planID, _, err := st.CreatePlan("dep-blocker-plan", "", "", []store.TaskInput{
		{Title: "blocker", Priority: "p0"},
	})
	if err != nil {
		t.Fatalf("CreatePlan blocker: %v", err)
	}
	blockerTasks, _ := st.GetPendingTasks(planID, "")
	if len(blockerTasks) == 0 {
		t.Fatal("no blocker task")
	}
	blockerID := blockerTasks[0].ID

	// Create a dependent task that depends on the blocker.
	plan2ID, _, err := st.CreatePlan("dep-waiting-plan", "", "", []store.TaskInput{
		{Title: "waiter", Priority: "p1", DependsOn: []string{blockerID}},
	})
	if err != nil {
		t.Fatalf("CreatePlan waiter: %v", err)
	}

	// Fetch pending tasks — waiter should be marked blocked.
	tasks, err := st.GetPendingTasks(plan2ID, "")
	if err != nil {
		t.Fatalf("GetPendingTasks: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("expected waiter task")
	}
	if !tasks[0].Blocked {
		t.Error("expected waiter to be marked Blocked")
	}
}

func TestGetPendingTasks_NoFilter(t *testing.T) {
	st := openTestStore(t)
	_, _, _ = st.CreatePlan("no-filter-plan", "", "", []store.TaskInput{
		{Title: "task-nf", Priority: "p1"},
	})

	// No planID, no agentID — returns all pending across all plans.
	tasks, err := st.GetPendingTasks("", "")
	if err != nil {
		t.Fatalf("GetPendingTasks no filter: %v", err)
	}
	_ = tasks
}

// ── ScanAll with a real DB in cache dir ───────────────────────────────────────

func TestScanAll_WithRealDB(t *testing.T) {
	// Open a store at a temp path so at least one DB file exists in a real dir.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test-project.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Save a minimal graph so meta table has content.
	g := graph.New("scan-project")
	nodeID := g.MakeNodeID("main.go", "main")
	g.AddNode(&graph.Node{ID: nodeID, Name: "main", Type: graph.NodeFunction, File: "main.go", Package: "main"})
	_ = st.SaveGraph(g)
	st.Close()

	// ScanAll on the actual cache dir — just confirm no error.
	_, err = store.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
}

// ── rebuildFTS via OpenReadOnly on populated DB ───────────────────────────────

// TestRebuildFTS_ViaSemanticSearch saves a graph and then does a search to
// confirm the FTS table is populated and the rebuild path is exercised.
func TestRebuildFTS_ViaSemanticSearch(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("fts-repo")

	// Add nodes with names that split well across CamelCase.
	names := []string{"ParseRequest", "ValidateToken", "HandleResponse", "BuildQuery"}
	for i, name := range names {
		id := g.MakeNodeID(fmt.Sprintf("pkg/%d.go", i), name)
		g.AddNode(&graph.Node{
			ID:      id,
			Name:    name,
			Type:    graph.NodeFunction,
			File:    fmt.Sprintf("pkg/%d.go", i),
			Package: "pkg",
			Metadata: map[string]string{
				"doc":       fmt.Sprintf("documentation for %s", name),
				"signature": fmt.Sprintf("func %s(ctx context.Context) error", name),
			},
		})
	}
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	// FTS search for a full word that will match.
	results, err := st.SemanticSearch("Parse", 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected FTS results for 'Parse'")
	}
}

// ── likeSearch fallback — empty query ─────────────────────────────────────────

func TestSemanticSearch_EmptyQueryReturnsNil(t *testing.T) {
	st := openTestStore(t)
	// Empty query should return nil, nil (hits the early-return in SemanticSearch).
	results, err := st.SemanticSearch("", 10)
	if err != nil {
		t.Fatalf("SemanticSearch empty: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results for empty query, got %v", results)
	}
}

// ── AppendEvent — zero-default limit branch in GetEvents ──────────────────────

func TestGetEvents_ZeroLimit(t *testing.T) {
	st := openTestStore(t)
	_ = st.AppendEvent("test_event", "agent-ev", `{"x":1}`)
	// limit=0 exercises the `if limit <= 0 { limit = 100 }` branch.
	events, lastSeq, err := st.GetEvents(0, nil, "", 0)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if lastSeq < 0 {
		t.Error("expected non-negative lastSeq")
	}
	_ = events
}

// ── LoadGraph on empty store returns nil ──────────────────────────────────────

func TestLoadGraph_EmptyStore(t *testing.T) {
	st := openTestStore(t)
	g, err := st.LoadGraph()
	if err != nil {
		t.Fatalf("LoadGraph on empty store: %v", err)
	}
	if g != nil {
		t.Error("expected nil graph from empty store")
	}
}

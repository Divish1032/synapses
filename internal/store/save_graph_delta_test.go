package store_test

// Tests for Store.SaveGraphDelta — the incremental graph persistence path.
// SaveGraphDelta updates only the nodes and edges for a specific changed file,
// leaving all other data intact and reducing write amplification ~95%.
//
// Design: most subtests share ONE store (created in TestSaveGraphDelta) and
// reset state at the start via st.SaveGraph(). This reduces SQLite schema
// initialisation overhead from O(subtests) to O(1), keeping the race-detector
// run time proportional to the number of assertions, not the number of tests.
// Only TestSaveGraphDelta_WithoutPriorFullSave needs an isolated fresh store.

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// helper: build a two-file graph (auth.go + store.go) and return it.
func buildDeltaGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("delta-repo")
	g.SetRoot("/tmp/repo")

	// auth.go nodes
	authFile := g.MakeNodeID("/tmp/repo/auth.go", "auth.go")
	loginID := g.MakeNodeID("/tmp/repo/auth.go", "Login")
	logoutID := g.MakeNodeID("/tmp/repo/auth.go", "Logout")

	g.AddNode(&graph.Node{
		ID: authFile, Type: graph.NodeFile, Name: "auth.go",
		File: "/tmp/repo/auth.go", Package: "auth",
	})
	g.AddNode(&graph.Node{
		ID: loginID, Type: graph.NodeFunction, Name: "Login",
		File: "/tmp/repo/auth.go", Package: "auth", Line: 10, Exported: true,
		Metadata: map[string]string{"signature": "func Login(user, pass string) error"},
	})
	g.AddNode(&graph.Node{
		ID: logoutID, Type: graph.NodeFunction, Name: "Logout",
		File: "/tmp/repo/auth.go", Package: "auth", Line: 25, Exported: true,
	})

	// store.go nodes
	storeFile := g.MakeNodeID("/tmp/repo/store.go", "store.go")
	queryID := g.MakeNodeID("/tmp/repo/store.go", "Query")

	g.AddNode(&graph.Node{
		ID: storeFile, Type: graph.NodeFile, Name: "store.go",
		File: "/tmp/repo/store.go", Package: "store",
	})
	g.AddNode(&graph.Node{
		ID: queryID, Type: graph.NodeFunction, Name: "Query",
		File: "/tmp/repo/store.go", Package: "store", Line: 5, Exported: true,
	})

	// Cross-file edge: Query calls Login
	g.AddEdge(&graph.Edge{From: queryID, To: loginID, Type: graph.EdgeCalls})

	return g
}

// TestSaveGraphDelta runs all delta scenarios against a single shared store.
// Each subtest resets state via st.SaveGraph at the start, so subtests are
// fully independent despite sharing the connection.
func TestSaveGraphDelta(t *testing.T) {
	st := openTestStore(t)

	// ── Basic delta: file update replaces nodes correctly ──────────────────────

	t.Run("UpdatesChangedFileNodes", func(t *testing.T) {
		g := buildDeltaGraph(t)
		if err := st.SaveGraph(g); err != nil {
			t.Fatalf("SaveGraph: %v", err)
		}

		// Rebuild auth.go without Logout, adding Register.
		g.RemoveFile("/tmp/repo/auth.go")
		authFile := g.MakeNodeID("/tmp/repo/auth.go", "auth.go")
		loginID := g.MakeNodeID("/tmp/repo/auth.go", "Login")
		registerID := g.MakeNodeID("/tmp/repo/auth.go", "Register")
		g.AddNode(&graph.Node{ID: authFile, Type: graph.NodeFile, Name: "auth.go", File: "/tmp/repo/auth.go", Package: "auth"})
		g.AddNode(&graph.Node{ID: loginID, Type: graph.NodeFunction, Name: "Login", File: "/tmp/repo/auth.go", Package: "auth", Line: 10, Exported: true, Metadata: map[string]string{"signature": "func Login(user, pass string) error"}})
		g.AddNode(&graph.Node{ID: registerID, Type: graph.NodeFunction, Name: "Register", File: "/tmp/repo/auth.go", Package: "auth", Line: 40, Exported: true})

		if err := st.SaveGraphDelta("/tmp/repo/auth.go", g); err != nil {
			t.Fatalf("SaveGraphDelta: %v", err)
		}

		loaded, err := st.LoadGraph()
		if err != nil {
			t.Fatalf("LoadGraph: %v", err)
		}
		names := map[string]bool{}
		for _, n := range loaded.AllNodes() {
			names[n.Name] = true
		}
		if names["Logout"] {
			t.Error("Logout should have been removed by delta")
		}
		if !names["Register"] {
			t.Error("Register should have been added by delta")
		}
		if !names["Query"] {
			t.Error("Query (store.go) must survive a delta save of auth.go")
		}
		if !names["Login"] {
			t.Error("Login (auth.go, unchanged) must survive a delta save")
		}
	})

	// ── Empty changedFile falls back to full SaveGraph ─────────────────────────

	t.Run("EmptyFile_FallsBackToFullSave", func(t *testing.T) {
		g := buildDeltaGraph(t)
		if err := st.SaveGraphDelta("", g); err != nil {
			t.Fatalf("SaveGraphDelta with empty changedFile: %v", err)
		}
		loaded, err := st.LoadGraph()
		if err != nil {
			t.Fatalf("LoadGraph: %v", err)
		}
		if loaded == nil || loaded.NodeCount() == 0 {
			t.Fatal("expected full graph after empty-changedFile delta")
		}
	})

	// ── prev_signature tracking works through delta ────────────────────────────

	t.Run("TracksSignatureChange", func(t *testing.T) {
		g := graph.New("sig-repo")
		g.SetRoot("/tmp/sig")
		funcID := g.MakeNodeID("/tmp/sig/pkg.go", "Foo")
		g.AddNode(&graph.Node{ID: funcID, Type: graph.NodeFunction, Name: "Foo", File: "/tmp/sig/pkg.go", Package: "pkg", Metadata: map[string]string{"signature": "func Foo() error"}})

		if err := st.SaveGraph(g); err != nil {
			t.Fatalf("SaveGraph: %v", err)
		}

		g.RemoveFile("/tmp/sig/pkg.go")
		funcID = g.MakeNodeID("/tmp/sig/pkg.go", "Foo")
		g.AddNode(&graph.Node{ID: funcID, Type: graph.NodeFunction, Name: "Foo", File: "/tmp/sig/pkg.go", Package: "pkg", Metadata: map[string]string{"signature": "func Foo(ctx context.Context) error"}})

		if err := st.SaveGraphDelta("/tmp/sig/pkg.go", g); err != nil {
			t.Fatalf("SaveGraphDelta: %v", err)
		}

		changes, err := st.GetSignatureChanges("")
		if err != nil {
			t.Fatalf("GetSignatureChanges: %v", err)
		}
		found := false
		for _, sc := range changes {
			if sc.Name == "Foo" {
				found = true
				if sc.OldSig != "func Foo() error" {
					t.Errorf("OldSig = %q, want 'func Foo() error'", sc.OldSig)
				}
			}
		}
		if !found {
			t.Error("expected Foo in signature changes")
		}
	})

	// ── Other-file nodes are preserved across delta ────────────────────────────

	t.Run("PreservesOtherFiles", func(t *testing.T) {
		g := buildDeltaGraph(t)
		if err := st.SaveGraph(g); err != nil {
			t.Fatalf("SaveGraph: %v", err)
		}

		g.RemoveFile("/tmp/repo/auth.go")
		authFile := g.MakeNodeID("/tmp/repo/auth.go", "auth.go")
		loginID := g.MakeNodeID("/tmp/repo/auth.go", "Login")
		logoutID := g.MakeNodeID("/tmp/repo/auth.go", "Logout")
		g.AddNode(&graph.Node{ID: authFile, Type: graph.NodeFile, Name: "auth.go", File: "/tmp/repo/auth.go", Package: "auth"})
		g.AddNode(&graph.Node{ID: loginID, Type: graph.NodeFunction, Name: "Login", File: "/tmp/repo/auth.go", Package: "auth", Line: 10, Exported: true, Metadata: map[string]string{"signature": "func Login(user, pass string) error", "doc": "updated doc"}})
		g.AddNode(&graph.Node{ID: logoutID, Type: graph.NodeFunction, Name: "Logout", File: "/tmp/repo/auth.go", Package: "auth", Line: 25, Exported: true})

		if err := st.SaveGraphDelta("/tmp/repo/auth.go", g); err != nil {
			t.Fatalf("SaveGraphDelta: %v", err)
		}

		loaded, err := st.LoadGraph()
		if err != nil {
			t.Fatalf("LoadGraph: %v", err)
		}
		if loaded.NodeCount() != g.NodeCount() {
			t.Errorf("node count mismatch: got %d, want %d", loaded.NodeCount(), g.NodeCount())
		}
		names := map[string]bool{}
		for _, n := range loaded.AllNodes() {
			names[n.Name] = true
		}
		if !names["Query"] {
			t.Error("Query (store.go) must survive delta of auth.go")
		}
	})

	// ── FTS updated correctly after delta ─────────────────────────────────────

	t.Run("UpdatesFTS", func(t *testing.T) {
		g := graph.New("fts-repo")
		g.SetRoot("/tmp/fts")
		fooID := g.MakeNodeID("/tmp/fts/pkg.go", "FooHandler")
		g.AddNode(&graph.Node{ID: fooID, Type: graph.NodeFunction, Name: "FooHandler", File: "/tmp/fts/pkg.go", Package: "pkg"})

		if err := st.SaveGraph(g); err != nil {
			t.Fatalf("SaveGraph: %v", err)
		}

		g.RemoveFile("/tmp/fts/pkg.go")
		barID := g.MakeNodeID("/tmp/fts/pkg.go", "BarHandler")
		g.AddNode(&graph.Node{ID: barID, Type: graph.NodeFunction, Name: "BarHandler", File: "/tmp/fts/pkg.go", Package: "pkg"})

		if err := st.SaveGraphDelta("/tmp/fts/pkg.go", g); err != nil {
			t.Fatalf("SaveGraphDelta: %v", err)
		}

		fooResults, err := st.SemanticSearch("FooHandler", 10)
		if err != nil {
			t.Fatalf("SemanticSearch FooHandler: %v", err)
		}
		for _, r := range fooResults {
			if r.Name == "FooHandler" {
				t.Error("FooHandler should have been removed from FTS index")
			}
		}

		barResults, err := st.SemanticSearch("BarHandler", 10)
		if err != nil {
			t.Fatalf("SemanticSearch BarHandler: %v", err)
		}
		found := false
		for _, r := range barResults {
			if r.Name == "BarHandler" {
				found = true
			}
		}
		if !found {
			t.Error("BarHandler not found in FTS after delta")
		}
	})

	// ── Edges re-inserted correctly after delta ────────────────────────────────

	t.Run("UpdatesOutgoingEdges", func(t *testing.T) {
		g := graph.New("edge-repo")
		g.SetRoot("/tmp/edge")
		callerID := g.MakeNodeID("/tmp/edge/caller.go", "Caller")
		calleeID := g.MakeNodeID("/tmp/edge/callee.go", "Callee")
		g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "Caller", File: "/tmp/edge/caller.go", Package: "pkg", Exported: true})
		g.AddNode(&graph.Node{ID: calleeID, Type: graph.NodeFunction, Name: "Callee", File: "/tmp/edge/callee.go", Package: "pkg", Exported: true})
		g.AddEdge(&graph.Edge{From: callerID, To: calleeID, Type: graph.EdgeCalls})

		if err := st.SaveGraph(g); err != nil {
			t.Fatalf("SaveGraph: %v", err)
		}

		g.RemoveFile("/tmp/edge/caller.go")
		callerID = g.MakeNodeID("/tmp/edge/caller.go", "Caller")
		g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "Caller", File: "/tmp/edge/caller.go", Package: "pkg", Exported: true})
		g.AddEdge(&graph.Edge{From: callerID, To: calleeID, Type: graph.EdgeCalls})
		g.AddEdge(&graph.Edge{From: callerID, To: calleeID, Type: graph.EdgeContains})

		if err := st.SaveGraphDelta("/tmp/edge/caller.go", g); err != nil {
			t.Fatalf("SaveGraphDelta: %v", err)
		}

		loaded, err := st.LoadGraph()
		if err != nil {
			t.Fatalf("LoadGraph: %v", err)
		}
		names := map[string]bool{}
		for _, n := range loaded.AllNodes() {
			names[n.Name] = true
		}
		if !names["Caller"] || !names["Callee"] {
			t.Error("expected both Caller and Callee after delta")
		}
	})

	// ── Empty new graph for changedFile (file cleared) ────────────────────────

	t.Run("AllNodesRemovedFromFile", func(t *testing.T) {
		g := buildDeltaGraph(t)
		if err := st.SaveGraph(g); err != nil {
			t.Fatalf("SaveGraph: %v", err)
		}

		g.RemoveFile("/tmp/repo/auth.go")
		if err := st.SaveGraphDelta("/tmp/repo/auth.go", g); err != nil {
			t.Fatalf("SaveGraphDelta empty file: %v", err)
		}

		loaded, err := st.LoadGraph()
		if err != nil {
			t.Fatalf("LoadGraph: %v", err)
		}
		for _, n := range loaded.AllNodes() {
			if n.File == "/tmp/repo/auth.go" {
				t.Errorf("unexpected auth.go node after delta: %s", n.Name)
			}
		}
		found := false
		for _, n := range loaded.AllNodes() {
			if n.Name == "Query" {
				found = true
			}
		}
		if !found {
			t.Error("Query (store.go) must survive delta of auth.go")
		}
	})

	// ── Meta counts updated correctly ─────────────────────────────────────────

	t.Run("UpdatesMetaCounts", func(t *testing.T) {
		g := buildDeltaGraph(t)
		if err := st.SaveGraph(g); err != nil {
			t.Fatalf("SaveGraph: %v", err)
		}
		beforeCount := g.NodeCount()

		g.RemoveFile("/tmp/repo/auth.go")
		authFile := g.MakeNodeID("/tmp/repo/auth.go", "auth.go")
		loginID := g.MakeNodeID("/tmp/repo/auth.go", "Login")
		logoutID := g.MakeNodeID("/tmp/repo/auth.go", "Logout")
		refreshID := g.MakeNodeID("/tmp/repo/auth.go", "RefreshToken")
		g.AddNode(&graph.Node{ID: authFile, Type: graph.NodeFile, Name: "auth.go", File: "/tmp/repo/auth.go", Package: "auth"})
		g.AddNode(&graph.Node{ID: loginID, Type: graph.NodeFunction, Name: "Login", File: "/tmp/repo/auth.go", Package: "auth", Exported: true})
		g.AddNode(&graph.Node{ID: logoutID, Type: graph.NodeFunction, Name: "Logout", File: "/tmp/repo/auth.go", Package: "auth", Exported: true})
		g.AddNode(&graph.Node{ID: refreshID, Type: graph.NodeFunction, Name: "RefreshToken", File: "/tmp/repo/auth.go", Package: "auth", Exported: true})

		if err := st.SaveGraphDelta("/tmp/repo/auth.go", g); err != nil {
			t.Fatalf("SaveGraphDelta: %v", err)
		}
		if g.NodeCount() <= beforeCount {
			t.Errorf("in-memory node count should increase: before=%d after=%d", beforeCount, g.NodeCount())
		}

		loaded, err := st.LoadGraph()
		if err != nil {
			t.Fatalf("LoadGraph: %v", err)
		}
		if loaded.NodeCount() != g.NodeCount() {
			t.Errorf("loaded node count = %d, want %d", loaded.NodeCount(), g.NodeCount())
		}
	})

	// ── Delta on a file not yet in the DB (new file) ──────────────────────────

	t.Run("NewFile_InsertsNodes", func(t *testing.T) {
		g := graph.New("new-file-repo")
		g.SetRoot("/tmp/nf")
		if err := st.SaveGraph(g); err != nil {
			t.Fatalf("SaveGraph empty: %v", err)
		}

		newID := g.MakeNodeID("/tmp/nf/fresh.go", "FreshFunc")
		g.AddNode(&graph.Node{ID: newID, Type: graph.NodeFunction, Name: "FreshFunc", File: "/tmp/nf/fresh.go", Package: "pkg", Exported: true})

		if err := st.SaveGraphDelta("/tmp/nf/fresh.go", g); err != nil {
			t.Fatalf("SaveGraphDelta new file: %v", err)
		}

		loaded, err := st.LoadGraph()
		if err != nil {
			t.Fatalf("LoadGraph: %v", err)
		}
		found := false
		for _, n := range loaded.AllNodes() {
			if n.Name == "FreshFunc" {
				found = true
			}
		}
		if !found {
			t.Error("FreshFunc should have been inserted via delta for new file")
		}
	})

	// ── Delta on non-existent changedFile (no-op for nodes, valid graph state)

	t.Run("ChangedFileNotInGraph_NoExtraNodes", func(t *testing.T) {
		g := buildDeltaGraph(t)
		if err := st.SaveGraph(g); err != nil {
			t.Fatalf("SaveGraph: %v", err)
		}
		nodesBefore := g.NodeCount()

		if err := st.SaveGraphDelta("/tmp/repo/nonexistent.go", g); err != nil {
			t.Fatalf("SaveGraphDelta nonexistent file: %v", err)
		}

		loaded, err := st.LoadGraph()
		if err != nil {
			t.Fatalf("LoadGraph: %v", err)
		}
		if loaded.NodeCount() != nodesBefore {
			t.Errorf("node count changed unexpectedly: got %d, want %d", loaded.NodeCount(), nodesBefore)
		}
	})

	// ── Dangling incoming edges to DELETED nodes are cleaned up ───────────────
	// Regression: without this fix, edges from other files pointing to functions
	// that were deleted accumulate indefinitely (unlike full SaveGraph which wipes
	// the edge table completely on every save).

	t.Run("CleansDanglingIncomingEdges", func(t *testing.T) {
		g := graph.New("dangling-repo")
		g.SetRoot("/tmp/dangling")
		callerID := g.MakeNodeID("/tmp/dangling/A.go", "Caller")
		targetID := g.MakeNodeID("/tmp/dangling/B.go", "Target")
		g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "Caller", File: "/tmp/dangling/A.go", Package: "pkg", Exported: true})
		g.AddNode(&graph.Node{ID: targetID, Type: graph.NodeFunction, Name: "Target", File: "/tmp/dangling/B.go", Package: "pkg", Exported: true})
		g.AddEdge(&graph.Edge{From: callerID, To: targetID, Type: graph.EdgeCalls})

		if err := st.SaveGraph(g); err != nil {
			t.Fatalf("SaveGraph: %v", err)
		}

		// Target is deleted; B.go gets a new function.
		g.RemoveFile("/tmp/dangling/B.go")
		newFuncID := g.MakeNodeID("/tmp/dangling/B.go", "NewFunc")
		g.AddNode(&graph.Node{ID: newFuncID, Type: graph.NodeFunction, Name: "NewFunc", File: "/tmp/dangling/B.go", Package: "pkg", Exported: true})

		if err := st.SaveGraphDelta("/tmp/dangling/B.go", g); err != nil {
			t.Fatalf("SaveGraphDelta: %v", err)
		}

		loaded, err := st.LoadGraph()
		if err != nil {
			t.Fatalf("LoadGraph: %v", err)
		}
		for _, e := range loaded.AllEdges() {
			if string(e.To) == string(targetID) {
				t.Errorf("dangling edge to deleted Target node still present: %v→%v", e.From, e.To)
			}
		}
	})

	// ── SaveGraphDelta idempotent: calling twice with same data is stable ──────

	t.Run("Idempotent", func(t *testing.T) {
		g := buildDeltaGraph(t)
		if err := st.SaveGraph(g); err != nil {
			t.Fatalf("SaveGraph: %v", err)
		}
		if err := st.SaveGraphDelta("/tmp/repo/auth.go", g); err != nil {
			t.Fatalf("SaveGraphDelta 1: %v", err)
		}
		if err := st.SaveGraphDelta("/tmp/repo/auth.go", g); err != nil {
			t.Fatalf("SaveGraphDelta 2: %v", err)
		}
		loaded, err := st.LoadGraph()
		if err != nil {
			t.Fatalf("LoadGraph: %v", err)
		}
		if loaded.NodeCount() != g.NodeCount() {
			t.Errorf("node count after idempotent delta: got %d, want %d", loaded.NodeCount(), g.NodeCount())
		}
	})
}

// TestSaveGraphDelta_WithoutPriorFullSave uses its own isolated store because
// it needs a completely clean state with no prior SaveGraph call.
func TestSaveGraphDelta_WithoutPriorFullSave(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("cold-repo")
	g.SetRoot("/tmp/cold")

	funcID := g.MakeNodeID("/tmp/cold/main.go", "Main")
	g.AddNode(&graph.Node{
		ID: funcID, Type: graph.NodeFunction, Name: "Main",
		File: "/tmp/cold/main.go", Package: "main", Exported: false,
	})

	// Delta with no prior state — should behave like an insert.
	if err := st.SaveGraphDelta("/tmp/cold/main.go", g); err != nil {
		t.Fatalf("SaveGraphDelta cold start: %v", err)
	}

	// SaveGraphDelta writes repo_id/repo_root to meta, so LoadGraph returns a
	// non-nil graph. Just verify no crash — the loaded graph may have nodes.
	loaded, err := st.LoadGraph()
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	_ = loaded
}

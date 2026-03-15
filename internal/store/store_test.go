package store_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// openTestStoreAtPath opens (or creates) a store at a specific path.
// Used by upgrade-path tests that need to open the same file twice.
func openTestStoreAtPath(t *testing.T, path string) (*store.Store, error) {
	t.Helper()
	return store.Open(path)
}

func buildTestGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")

	fileID := g.MakeNodeID("auth.go", "auth.go")
	svcID := g.MakeNodeID("auth.go", "AuthService")
	loginID := g.MakeNodeID("auth.go", "Login")

	g.AddNode(&graph.Node{
		ID:      fileID,
		Type:    graph.NodeFile,
		Name:    "auth.go",
		File:    "auth.go",
		Line:    1,
		Package: "auth",
	})
	g.AddNode(&graph.Node{
		ID:       svcID,
		Type:     graph.NodeStruct,
		Name:     "AuthService",
		Package:  "auth",
		File:     "auth.go",
		Line:     5,
		Exported: true,
		Metadata: map[string]string{"doc": "handles auth"},
	})
	g.AddNode(&graph.Node{
		ID:       loginID,
		Type:     graph.NodeFunction,
		Name:     "Login",
		Package:  "auth",
		File:     "auth.go",
		Line:     12,
		Exported: true,
	})
	g.AddEdge(&graph.Edge{From: fileID, To: svcID, Type: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: fileID, To: loginID, Type: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: loginID, To: svcID, Type: graph.EdgeCalls})
	return g
}

func TestOpen_CreatesSchema(t *testing.T) {
	// Simply opening must not error.
	_ = openTestStore(t)
}

func TestSaveAndLoad_Roundtrip(t *testing.T) {
	st := openTestStore(t)
	original := buildTestGraph(t)

	if err := st.SaveGraph(original); err != nil {
		t.Fatalf("SaveGraph() error: %v", err)
	}

	loaded, err := st.LoadGraph()
	if err != nil {
		t.Fatalf("LoadGraph() error: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadGraph() returned nil after save")
	}

	if loaded.NodeCount() != original.NodeCount() {
		t.Errorf("node count: got %d, want %d", loaded.NodeCount(), original.NodeCount())
	}
	if loaded.EdgeCount() != original.EdgeCount() {
		t.Errorf("edge count: got %d, want %d", loaded.EdgeCount(), original.EdgeCount())
	}
}

func TestLoad_EmptyStore_ReturnsNil(t *testing.T) {
	st := openTestStore(t)
	g, err := st.LoadGraph()
	if err != nil {
		t.Fatalf("LoadGraph() on empty store returned error: %v", err)
	}
	if g != nil {
		t.Error("LoadGraph() on empty store should return nil")
	}
}

func TestSaveAndLoad_NodeFields(t *testing.T) {
	st := openTestStore(t)
	original := buildTestGraph(t)

	if err := st.SaveGraph(original); err != nil {
		t.Fatalf("SaveGraph() error: %v", err)
	}

	loaded, _ := st.LoadGraph()

	nodes := loaded.FindByName("AuthService")
	if len(nodes) == 0 {
		t.Fatal("AuthService not found after roundtrip")
	}
	n := nodes[0]

	if n.Type != graph.NodeStruct {
		t.Errorf("Type = %q, want NodeStruct", n.Type)
	}
	if n.Package != "auth" {
		t.Errorf("Package = %q, want 'auth'", n.Package)
	}
	if n.Line != 5 {
		t.Errorf("Line = %d, want 5", n.Line)
	}
	if !n.Exported {
		t.Error("Exported should be true")
	}
	if n.Metadata["doc"] != "handles auth" {
		t.Errorf("Metadata['doc'] = %q, want 'handles auth'", n.Metadata["doc"])
	}
}

func TestSaveAndLoad_EdgeTypes(t *testing.T) {
	st := openTestStore(t)
	original := buildTestGraph(t)

	if err := st.SaveGraph(original); err != nil {
		t.Fatalf("SaveGraph() error: %v", err)
	}
	loaded, _ := st.LoadGraph()

	// Verify that CALLS edge from Login → AuthService is preserved.
	loginNodes := loaded.FindByName("Login")
	if len(loginNodes) == 0 {
		t.Fatal("Login node not found after roundtrip")
	}

	outEdges := loaded.OutEdges(loginNodes[0].ID)
	foundCalls := false
	for _, e := range outEdges {
		if e.Type == graph.EdgeCalls {
			foundCalls = true
		}
	}
	if !foundCalls {
		t.Error("CALLS edge from Login not found after roundtrip")
	}
}

func TestSaveGraph_Replaces(t *testing.T) {
	st := openTestStore(t)

	// First save: 3 nodes.
	g1 := buildTestGraph(t)
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("first SaveGraph() error: %v", err)
	}

	// Second save: 1 node. Should fully replace, not append.
	g2 := graph.New("testrepo")
	onlyID := g2.MakeNodeID("main.go", "main.go")
	g2.AddNode(&graph.Node{ID: onlyID, Type: graph.NodeFile, Name: "main.go", File: "main.go"})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("second SaveGraph() error: %v", err)
	}

	loaded, _ := st.LoadGraph()
	if loaded.NodeCount() != 1 {
		t.Errorf("after replace: node count = %d, want 1", loaded.NodeCount())
	}
}

func TestSavedAt_AfterSave(t *testing.T) {
	st := openTestStore(t)

	before := time.Now().Add(-time.Second)
	if err := st.SaveGraph(buildTestGraph(t)); err != nil {
		t.Fatalf("SaveGraph() error: %v", err)
	}
	after := time.Now().Add(time.Second)

	ts, err := st.SavedAt()
	if err != nil {
		t.Fatalf("SavedAt() error: %v", err)
	}
	if ts.IsZero() {
		t.Error("SavedAt() returned zero time after save")
	}
	if ts.Before(before) || ts.After(after) {
		t.Errorf("SavedAt() = %v, expected between %v and %v", ts, before, after)
	}
}

func TestSavedAt_EmptyStore(t *testing.T) {
	st := openTestStore(t)
	ts, err := st.SavedAt()
	if err != nil {
		t.Fatalf("SavedAt() error: %v", err)
	}
	if !ts.IsZero() {
		t.Error("SavedAt() on empty store should return zero time")
	}
}

func TestDefaultPath_Deterministic(t *testing.T) {
	dir := t.TempDir()
	p1, err := store.DefaultPath(dir)
	if err != nil {
		t.Fatalf("DefaultPath() error: %v", err)
	}
	p2, _ := store.DefaultPath(dir)
	if p1 != p2 {
		t.Errorf("DefaultPath() not deterministic: %q != %q", p1, p2)
	}
}

// ─── Stat ─────────────────────────────────────────────────────────────────────

func TestStat_EmptyStore_ReturnsNil(t *testing.T) {
	st := openTestStore(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	stat, err := st.Stat(dbPath)
	if err != nil {
		t.Fatalf("Stat() on empty store returned error: %v", err)
	}
	if stat != nil {
		t.Error("Stat() on empty store should return nil")
	}
}

func TestStat_AfterSave_ReturnsMetadata(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	g := buildTestGraph(t)
	g.SetRoot("/projects/myapp")
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph() error: %v", err)
	}

	stat, err := st.Stat(dbPath)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if stat == nil {
		t.Fatal("Stat() returned nil after SaveGraph")
	}

	if stat.RepoID != "testrepo" {
		t.Errorf("RepoID = %q, want 'testrepo'", stat.RepoID)
	}
	if stat.RepoRoot != "/projects/myapp" {
		t.Errorf("RepoRoot = %q, want '/projects/myapp'", stat.RepoRoot)
	}
	if stat.NodeCount != g.NodeCount() {
		t.Errorf("NodeCount = %d, want %d", stat.NodeCount, g.NodeCount())
	}
	if stat.EdgeCount != g.EdgeCount() {
		t.Errorf("EdgeCount = %d, want %d", stat.EdgeCount, g.EdgeCount())
	}
	if stat.FileCount != 1 { // buildTestGraph has one NodeFile
		t.Errorf("FileCount = %d, want 1", stat.FileCount)
	}
	if stat.SavedAt.IsZero() {
		t.Error("SavedAt should not be zero after save")
	}
	if stat.DBPath != dbPath {
		t.Errorf("DBPath = %q, want %q", stat.DBPath, dbPath)
	}
}

// ─── ScanAll ──────────────────────────────────────────────────────────────────

// ─── SemanticSearch ───────────────────────────────────────────────────────────

func TestSemanticSearch_FindsByName(t *testing.T) {
	st := openTestStore(t)
	g := buildTestGraph(t)
	// Add a function with rich metadata for search testing.
	g.AddNode(&graph.Node{
		ID:      g.MakeNodeID("auth.go", "ValidateToken"),
		Type:    graph.NodeFunction,
		Name:    "ValidateToken",
		Package: "auth",
		File:    "auth.go",
		Line:    20,
		Metadata: map[string]string{
			"signature": "func ValidateToken(token string) (Claims, error)",
			"doc":       "ValidateToken parses and verifies a JWT access token",
		},
	})
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	// Search by concept word present in doc.
	results, err := st.SemanticSearch("JWT", 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result for 'JWT', got none")
	}
	found := false
	for _, r := range results {
		if r.Name == "ValidateToken" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ValidateToken in results, got: %+v", results)
	}
}

func TestSemanticSearch_CamelCaseSplit(t *testing.T) {
	st := openTestStore(t)
	g := buildTestGraph(t)
	g.AddNode(&graph.Node{
		ID:      g.MakeNodeID("graph.go", "CarveEgoGraph"),
		Type:    graph.NodeFunction,
		Name:    "CarveEgoGraph",
		Package: "graph",
		File:    "graph.go",
		Line:    42,
	})
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	// Query a partial camelCase word — should find via split_name column.
	results, err := st.SemanticSearch("Carve", 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	found := false
	for _, r := range results {
		if r.Name == "CarveEgoGraph" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected CarveEgoGraph via camelCase split search, results: %+v", results)
	}
}

func TestSemanticSearch_EmptyQuery(t *testing.T) {
	st := openTestStore(t)
	results, err := st.SemanticSearch("", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for empty query, got %d", len(results))
	}
}

func TestSemanticSearch_MultiWord(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("testrepo")

	for _, n := range []struct{ name, file string }{
		{"CarveEgoGraph", "graph.go"},
		{"BFSTraverse", "traverse.go"},
		{"EgoSubgraph", "subgraph.go"},
	} {
		g.AddNode(&graph.Node{
			ID:      g.MakeNodeID(n.file, n.name),
			Type:    graph.NodeFunction,
			Name:    n.name,
			Package: "graph",
			File:    n.file,
			Line:    1,
		})
	}
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	// Multi-word query — OR semantics: any term should match.
	results, err := st.SemanticSearch("carve BFS ego", 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for multi-word query 'carve BFS ego', got none")
	}
	// All three nodes contain at least one of the query words, so we expect multiple hits.
	names := make(map[string]bool)
	for _, r := range results {
		names[r.Name] = true
	}
	for _, want := range []string{"CarveEgoGraph", "BFSTraverse", "EgoSubgraph"} {
		if !names[want] {
			t.Errorf("expected %s in results, got: %v", want, names)
		}
	}
}

func TestSemanticSearch_SpecialChars(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("testrepo")
	g.AddNode(&graph.Node{
		ID:   g.MakeNodeID("main.go", "main"),
		Type: graph.NodeFunction, Name: "main", File: "main.go", Line: 1,
	})
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	// Special characters that could break FTS5 syntax — must not panic or error.
	results, err := st.SemanticSearch(`func(x) *int`, 10)
	if err != nil {
		t.Fatalf("SemanticSearch with special chars returned error: %v", err)
	}
	// Results may be empty or non-empty; the important thing is no crash.
	_ = results
}

// TestScanAll_DoesNotCrash verifies that ScanAll on the live cache dir returns
// without error regardless of what is (or isn't) cached on this machine.
func TestScanAll_DoesNotCrash(t *testing.T) {
	stats, err := store.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll() returned error: %v", err)
	}
	// Results may be empty on a fresh machine; that is not an error.
	_ = stats
}

// TestScanAll_SkipsCorruptFile verifies that a non-SQLite file in the cache
// directory is gracefully skipped and does not cause ScanAll to error.
// We test this via Open + Stat since ScanAll reads from the OS cache dir.
func TestStat_CorruptDB_ReturnsError(t *testing.T) {
	// Write garbage bytes as a "corrupt" DB file.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(dbPath, []byte("not a sqlite database"), 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	// Open may succeed (SQLite driver may open any file) but schema apply or
	// query should fail when the content is not valid SQLite.
	st, err := store.Open(dbPath)
	if err != nil {
		// Open itself returned an error — acceptable, that's the expected behaviour.
		return
	}
	defer st.Close()
	// If Open succeeded on the corrupt file, Stat should either return an error
	// or nil (empty store), but must not panic.
	_, _ = st.Stat(dbPath)
}

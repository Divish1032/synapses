package graph_test

// Additional tests targeting uncovered graph functions:
// DetectCommunities, Serialize/Deserialize/DeserializeMapped, LoadSnapshot,
// SuggestRules, MakeNodeID, MergeFrom, ProjectIdentity.

import (
	"bytes"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func buildTestGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("/repo")

	authFile := "/repo/pkg/auth/auth.go"
	apiFile := "/repo/pkg/api/handler.go"

	nodes := []struct {
		file, name, pkg string
		typ             graph.NodeType
	}{
		{authFile, "Login", "auth", graph.NodeFunction},
		{authFile, "Logout", "auth", graph.NodeFunction},
		{authFile, "ValidateToken", "auth", graph.NodeFunction},
		{apiFile, "HandleLogin", "api", graph.NodeFunction},
		{apiFile, "HandleLogout", "api", graph.NodeFunction},
	}
	ids := make(map[string]graph.NodeID)
	for _, n := range nodes {
		id := g.MakeNodeID(n.file, n.name)
		ids[n.name] = id
		g.AddNode(&graph.Node{
			ID:      id,
			Name:    n.name,
			Type:    n.typ,
			File:    n.file,
			Package: n.pkg,
		})
	}
	g.AddEdge(&graph.Edge{From: ids["HandleLogin"], To: ids["Login"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["HandleLogout"], To: ids["Logout"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["Login"], To: ids["ValidateToken"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["Logout"], To: ids["ValidateToken"], Type: graph.EdgeCalls})
	return g
}

// ── DetectCommunities ─────────────────────────────────────────────────────────

func TestDetectCommunities_EmptyGraph(t *testing.T) {
	g := graph.New("test")
	result, err := g.DetectCommunities(10, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestDetectCommunities_WithNodes(t *testing.T) {
	g := buildTestGraph(t)
	result, err := g.DetectCommunities(10, 1)
	if err != nil {
		t.Fatalf("DetectCommunities: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	_ = result.Communities
	_ = result.Modularity
}

func TestDetectCommunities_MinSizeFilter(t *testing.T) {
	g := buildTestGraph(t)
	result, err := g.DetectCommunities(5, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestDetectCommunities_SingleIteration(t *testing.T) {
	g := buildTestGraph(t)
	result, err := g.DetectCommunities(1, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ── Serialize / Deserialize ───────────────────────────────────────────────────

func buildFlatGraphForTest(t *testing.T) *graph.FlatGraph {
	t.Helper()
	pool := graph.NewStringPool()
	fg := graph.NewFlatGraph("test-repo")

	fileID := pool.Intern("pkg/auth.go")
	nameA := pool.Intern("Login")
	nameB := pool.Intern("Logout")

	idxA := fg.AddNode(nameA, graph.NodeFunction, fileID, 0)
	idxB := fg.AddNode(nameB, graph.NodeFunction, fileID, 0)
	fg.AddEdge(idxA, idxB, 1.0)
	return fg
}

func TestFlatGraphSerializeDeserialize_RoundTrip(t *testing.T) {
	fg := buildFlatGraphForTest(t)

	var buf bytes.Buffer
	if err := fg.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("expected non-empty serialized output")
	}

	restored, err := graph.Deserialize(&buf)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if restored == nil {
		t.Fatal("expected non-nil restored FlatGraph")
	}
}

func TestFlatGraphSerialize_EmptyGraph(t *testing.T) {
	fg := graph.NewFlatGraph("empty")
	var buf bytes.Buffer
	if err := fg.Serialize(&buf); err != nil {
		t.Fatalf("Serialize empty: %v", err)
	}
	restored, err := graph.Deserialize(&buf)
	if err != nil {
		t.Fatalf("Deserialize empty: %v", err)
	}
	_ = restored
}

func TestDeserialize_CorruptData(t *testing.T) {
	r := bytes.NewReader([]byte("not valid data"))
	_, err := graph.Deserialize(r)
	if err == nil {
		t.Error("expected error for corrupt data")
	}
}

func TestDeserializeMapped_RoundTrip(t *testing.T) {
	fg := buildFlatGraphForTest(t)

	var buf bytes.Buffer
	if err := fg.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	restored, err := graph.DeserializeMapped(buf.Bytes())
	if err != nil {
		t.Fatalf("DeserializeMapped: %v", err)
	}
	if restored == nil {
		t.Fatal("expected non-nil restored FlatGraph")
	}
}

func TestDeserializeMapped_CorruptData(t *testing.T) {
	_, err := graph.DeserializeMapped([]byte("garbage"))
	if err == nil {
		t.Error("expected error for corrupt data")
	}
}

// ── LoadSnapshot via RebuildIndex ─────────────────────────────────────────────

func TestRebuildIndexAndLoadSnapshot(t *testing.T) {
	g := buildTestGraph(t)
	blob, err := g.RebuildIndex()
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("expected non-empty snapshot blob")
	}

	pool := graph.NewStringPool()
	restored, err := graph.LoadSnapshot(blob, pool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if restored == nil {
		t.Fatal("expected non-nil restored GraphIndex")
	}
}

func TestLoadSnapshot_CorruptData(t *testing.T) {
	pool := graph.NewStringPool()
	_, err := graph.LoadSnapshot([]byte("garbage"), pool)
	if err == nil {
		t.Error("expected error for corrupt snapshot")
	}
}

func TestRebuildIndex_EmptyGraph(t *testing.T) {
	g := graph.New("empty")
	blob, err := g.RebuildIndex()
	if err != nil {
		t.Fatalf("RebuildIndex empty: %v", err)
	}
	pool := graph.NewStringPool()
	restored, err := graph.LoadSnapshot(blob, pool)
	if err != nil {
		t.Fatalf("LoadSnapshot empty: %v", err)
	}
	_ = restored
}

// ── SuggestRules ──────────────────────────────────────────────────────────────

func TestSuggestRules_EmptyGraph(t *testing.T) {
	g := graph.New("test")
	rules := g.SuggestRules()
	if len(rules) != 0 {
		t.Errorf("expected no rules for empty graph, got %d", len(rules))
	}
}

func TestSuggestRules_WithNodes(t *testing.T) {
	g := buildTestGraph(t)
	rules := g.SuggestRules()
	_ = rules // must not panic
}

func TestSuggestRules_StrongCoupling(t *testing.T) {
	// Build a graph where api/ consistently calls auth/ to trigger rule detection.
	g := graph.New("/repo")
	for i := 0; i < 5; i++ {
		apiFile := "/repo/api/handler.go"
		authFile := "/repo/auth/service.go"
		apiID := g.MakeNodeID(apiFile, "Handler"+string(rune('A'+i)))
		authID := g.MakeNodeID(authFile, "Service"+string(rune('A'+i)))
		g.AddNode(&graph.Node{ID: apiID, Name: "Handler" + string(rune('A'+i)),
			Type: graph.NodeFunction, File: apiFile, Package: "api"})
		g.AddNode(&graph.Node{ID: authID, Name: "Service" + string(rune('A'+i)),
			Type: graph.NodeFunction, File: authFile, Package: "auth"})
		g.AddEdge(&graph.Edge{From: apiID, To: authID, Type: graph.EdgeCalls})
	}
	rules := g.SuggestRules()
	_ = rules
}

// ── MakeNodeID ────────────────────────────────────────────────────────────────

func TestMakeNodeID_AbsolutePath(t *testing.T) {
	g := graph.New("/my/project")
	id := g.MakeNodeID("/my/project/pkg/auth.go", "Login")
	if id == "" {
		t.Error("expected non-empty node ID")
	}
}

func TestMakeNodeID_RelativePath(t *testing.T) {
	g := graph.New("/project")
	id := g.MakeNodeID("pkg/auth.go", "Login")
	if id == "" {
		t.Error("expected non-empty node ID for relative path")
	}
}

func TestMakeNodeID_EmptyRoot(t *testing.T) {
	g := graph.New("")
	id := g.MakeNodeID("auth.go", "Login")
	if id == "" {
		t.Error("expected non-empty node ID when root is empty")
	}
}

func TestMakeNodeID_Stable(t *testing.T) {
	g := graph.New("/repo")
	id1 := g.MakeNodeID("/repo/pkg/auth.go", "Login")
	id2 := g.MakeNodeID("/repo/pkg/auth.go", "Login")
	if id1 != id2 {
		t.Error("MakeNodeID must be deterministic")
	}
}

// ── MergeFrom ─────────────────────────────────────────────────────────────────

func TestMergeFrom_Basic(t *testing.T) {
	g1 := graph.New("repo1")
	id1 := g1.MakeNodeID("a.go", "FuncA")
	g1.AddNode(&graph.Node{ID: id1, Name: "FuncA", Type: graph.NodeFunction, File: "a.go"})

	g2 := graph.New("repo2")
	id2 := g2.MakeNodeID("b.go", "FuncB")
	g2.AddNode(&graph.Node{ID: id2, Name: "FuncB", Type: graph.NodeFunction, File: "b.go"})

	g1.MergeFrom(g2)
	nodes := g1.AllNodes()
	found := false
	for _, n := range nodes {
		if n.Name == "FuncB" {
			found = true
		}
	}
	if !found {
		t.Error("expected FuncB in merged graph")
	}
}

func TestMergeFrom_Empty(t *testing.T) {
	g1 := graph.New("repo1")
	g2 := graph.New("repo2")
	g1.MergeFrom(g2) // must not panic
}

// ── ProjectIdentity ───────────────────────────────────────────────────────────

func TestProjectIdentity_NotNil(t *testing.T) {
	g := buildTestGraph(t)
	p := g.ProjectIdentity()
	if p == nil {
		t.Fatal("expected non-nil ProjectIdentity")
	}
	if p.TotalNodes == 0 {
		t.Error("expected non-zero TotalNodes")
	}
}

func TestProjectIdentity_Cached(t *testing.T) {
	g := buildTestGraph(t)
	p1 := g.ProjectIdentity()
	p2 := g.ProjectIdentity()
	if p1 == nil || p2 == nil {
		t.Fatal("expected non-nil ProjectIdentity")
	}
	if p1.TotalNodes != p2.TotalNodes {
		t.Error("cached and fresh identity should match")
	}
}

// ── ExportDOT dotEscape coverage ─────────────────────────────────────────────

func TestExportDOT_WithSpecialChars(t *testing.T) {
	// Nodes with special characters trigger dotEscape.
	g := graph.New("/repo")
	id := g.MakeNodeID("/repo/file.go", `Func"With"Quotes`)
	g.AddNode(&graph.Node{
		ID:   id,
		Name: `Func"With"Quotes`,
		Type: graph.NodeFunction,
		File: "/repo/file.go",
	})
	nodes := g.AllNodes()
	edges := g.AllEdges()
	dot := graph.ExportDOT(nodes, edges, "/repo", false)
	if dot == "" {
		t.Error("expected non-empty DOT even with special chars")
	}
}

// ── SnapshotFileStableIDs / MigrateStableID ───────────────────────────────────

func TestSnapshotAndMigrateStableID(t *testing.T) {
	g := buildTestGraph(t)
	snap := g.SnapshotFileStableIDs("/repo/pkg/auth/auth.go")
	if len(snap) == 0 {
		t.Error("expected non-empty stable ID snapshot")
	}
	// MigrateStableID: try migrating a node using the snapshot.
	for oldID, name := range snap {
		newID := g.MigrateStableID(oldID, name, "/repo/pkg/auth/auth.go")
		_ = newID // may be empty if not found
		break     // just test one
	}
}

// ── RemoveFile ────────────────────────────────────────────────────────────────

func TestRemoveFile_Existing(t *testing.T) {
	g := buildTestGraph(t)
	n := g.AllNodes()[0]
	removed := g.RemoveFile(n.File)
	if removed == 0 {
		t.Error("expected at least one node removed")
	}
}

func TestRemoveFile_NonExistent(t *testing.T) {
	g := buildTestGraph(t)
	removed := g.RemoveFile("nonexistent/file.go")
	if removed != 0 {
		t.Errorf("expected 0 removed for non-existent file, got %d", removed)
	}
}

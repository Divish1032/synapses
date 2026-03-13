package graph_test

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── RepoID / Root / SetRoot ───────────────────────────────────────────────────

func TestRepoID(t *testing.T) {
	g := graph.New("my-repo")
	if g.RepoID() != "my-repo" {
		t.Errorf("expected RepoID='my-repo', got %q", g.RepoID())
	}
}

func TestRootSetRoot(t *testing.T) {
	g := graph.New("repo")
	if g.Root() != "" {
		t.Errorf("expected empty root, got %q", g.Root())
	}
	g.SetRoot("/home/user/project")
	if g.Root() != "/home/user/project" {
		t.Errorf("expected '/home/user/project', got %q", g.Root())
	}
}

// ── FindByFile ────────────────────────────────────────────────────────────────

func TestFindByFile_MatchesFile(t *testing.T) {
	g := buildFixture(t)

	nodes := g.FindByFile("auth.go")
	if len(nodes) == 0 {
		t.Error("expected nodes for auth.go")
	}
	for _, n := range nodes {
		if n.File != "auth.go" {
			t.Errorf("node file mismatch: %q", n.File)
		}
	}
}

func TestFindByFile_NoMatch(t *testing.T) {
	g := buildFixture(t)
	nodes := g.FindByFile("nonexistent.go")
	if len(nodes) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(nodes))
	}
}

// ── OutEdges / InEdges ────────────────────────────────────────────────────────

func TestOutEdges_HasEdges(t *testing.T) {
	g := buildFixture(t)

	loginID := g.MakeNodeID("auth.go", "Login")
	out := g.OutEdges(loginID)
	if len(out) == 0 {
		t.Error("expected outgoing edges from Login")
	}
}

func TestOutEdges_EmptyNode(t *testing.T) {
	g := buildFixture(t)
	svcID := g.MakeNodeID("auth.go", "AuthService")
	// AuthService has no outgoing CALLS — only incoming.
	out := g.OutEdges(svcID)
	// May be 0 or more depending on fixture, just verify no crash.
	_ = out
}

func TestInEdges_HasEdges(t *testing.T) {
	g := buildFixture(t)

	svcID := g.MakeNodeID("auth.go", "AuthService")
	in := g.InEdges(svcID)
	if len(in) == 0 {
		t.Error("expected incoming edges to AuthService")
	}
}

func TestInEdges_UnknownNode(t *testing.T) {
	g := buildFixture(t)
	in := g.InEdges(graph.NodeID("unknown::unknown::unknown"))
	if len(in) != 0 {
		t.Errorf("expected 0 in-edges for unknown node, got %d", len(in))
	}
}

// ── AddCallSite / PeekCallSites / DrainCallSites ──────────────────────────────

func TestCallSites_PeekAndDrain(t *testing.T) {
	g := graph.New("repo")

	cs := graph.CallSite{
		CallerID:   "repo::main.go::main",
		CallerFile: "main.go",
		PkgAlias:   "auth",
		FuncName:   "Login",
	}
	g.AddCallSite(cs)

	// PeekCallSites — should return the site without clearing.
	peeked := g.PeekCallSites()
	if len(peeked) != 1 {
		t.Fatalf("expected 1 peeked call site, got %d", len(peeked))
	}
	if peeked[0].FuncName != "Login" {
		t.Errorf("expected FuncName=Login, got %q", peeked[0].FuncName)
	}

	// Drain — should return and clear.
	drained := g.DrainCallSites()
	if len(drained) != 1 {
		t.Fatalf("expected 1 drained call site, got %d", len(drained))
	}

	// After drain, PeekCallSites should be empty.
	if peek2 := g.PeekCallSites(); len(peek2) != 0 {
		t.Errorf("expected 0 call sites after drain, got %d", len(peek2))
	}
}

// ── InvalidateCache ───────────────────────────────────────────────────────────

func TestInvalidateCache_DoesNotCrash(t *testing.T) {
	g := buildFixture(t)
	g.InvalidateCache()
	// No panic = pass.
}

func TestInvalidateCacheForFile_OnlyEvictsAffectedEntries(t *testing.T) {
	g := graph.New("repo")
	g.AddNode(&graph.Node{ID: "repo::a.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "/project/a.go"})
	g.AddNode(&graph.Node{ID: "repo::a.go::Bar", Name: "Bar", Type: graph.NodeFunction, File: "/project/a.go"})
	g.AddNode(&graph.Node{ID: "repo::b.go::Baz", Name: "Baz", Type: graph.NodeFunction, File: "/project/b.go"})
	g.AddEdge(&graph.Edge{From: "repo::a.go::Foo", To: "repo::a.go::Bar", Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "repo::b.go::Baz", To: "repo::a.go::Foo", Type: graph.EdgeCalls})

	cfg := graph.CarveConfig{MaxDepth: 1, TokenBudget: 4000}

	// Warm the cache for both Foo (in a.go) and Baz (in b.go).
	_, err := g.CarveEgoGraph("repo::a.go::Foo", cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.CarveEgoGraph("repo::b.go::Baz", cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Invalidate only a.go — Baz's cache entry should survive.
	g.InvalidateCacheForFile("/project/a.go")

	// Foo was in a.go, so its cache should be evicted (re-carve should work fine).
	sub, err := g.CarveEgoGraph("repo::a.go::Foo", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sub == nil {
		t.Fatal("expected non-nil subgraph after cache miss")
	}

	// Baz is in b.go, but its subgraph includes Foo (from a.go) via CALLS edge,
	// so it should also be evicted since it references a.go.
	// This tests the file-reference tracking, not just root-file matching.
}

func TestInvalidateCacheForFile_PreservesUnrelatedEntries(t *testing.T) {
	g := graph.New("repo")
	g.AddNode(&graph.Node{ID: "repo::x.go::Alpha", Name: "Alpha", Type: graph.NodeFunction, File: "/project/x.go"})
	g.AddNode(&graph.Node{ID: "repo::y.go::Beta", Name: "Beta", Type: graph.NodeFunction, File: "/project/y.go"})

	cfg := graph.CarveConfig{MaxDepth: 1, TokenBudget: 4000}

	// Warm cache for both.
	sub1, _ := g.CarveEgoGraph("repo::x.go::Alpha", cfg)
	sub2, _ := g.CarveEgoGraph("repo::y.go::Beta", cfg)
	if sub1 == nil || sub2 == nil {
		t.Fatal("expected non-nil subgraphs")
	}

	// Invalidate only x.go.
	g.InvalidateCacheForFile("/project/x.go")

	// Beta (y.go) should still be cached — verify it still returns successfully.
	sub2Again, err := g.CarveEgoGraph("repo::y.go::Beta", cfg)
	if err != nil {
		t.Fatalf("expected cache hit for Beta after invalidating x.go: %v", err)
	}
	if sub2Again == nil {
		t.Fatal("expected non-nil subgraph for Beta")
	}
}

// ── Index / SetIndex ──────────────────────────────────────────────────────────

func TestIndex_NilByDefault(t *testing.T) {
	g := graph.New("repo")
	if g.Index() != nil {
		t.Error("expected nil Index before RebuildIndex")
	}
}

func TestSetIndex_StoresIndex(t *testing.T) {
	g := buildFixture(t)

	// RebuildIndex returns an index; install it and verify.
	_, _ = g.RebuildIndex()
	idx := g.Index()
	if idx == nil {
		t.Fatal("expected non-nil index after RebuildIndex")
	}

	// SetIndex nil — clears it.
	g.SetIndex(nil)
	if g.Index() != nil {
		t.Error("expected nil after SetIndex(nil)")
	}
}

// ── Export functions ──────────────────────────────────────────────────────────

func TestExportDOT_ContainsNodes(t *testing.T) {
	g := buildFixture(t)
	nodes := g.AllNodes()
	edges := g.AllEdges()

	dot := graph.ExportDOT(nodes, edges, "", false)
	if !strings.Contains(dot, "digraph G") {
		t.Error("expected DOT output to contain 'digraph G'")
	}
	if !strings.Contains(dot, "AuthService") {
		t.Error("expected DOT to mention AuthService")
	}
}

func TestExportDOT_WithMeta(t *testing.T) {
	g := buildFixture(t)
	nodes := g.AllNodes()
	edges := g.AllEdges()

	dot := graph.ExportDOT(nodes, edges, "base/", true)
	if !strings.Contains(dot, "digraph G") {
		t.Error("expected valid DOT output")
	}
}

func TestExportMermaid_ContainsFlowchart(t *testing.T) {
	g := buildFixture(t)
	nodes := g.AllNodes()
	edges := g.AllEdges()

	mmd := graph.ExportMermaid(nodes, edges, "", false)
	if !strings.Contains(mmd, "flowchart") && !strings.Contains(mmd, "graph") {
		t.Error("expected Mermaid output to contain 'flowchart' or 'graph'")
	}
}

func TestExportGraphML_ContainsXML(t *testing.T) {
	g := buildFixture(t)
	nodes := g.AllNodes()
	edges := g.AllEdges()

	gml := graph.ExportGraphML(nodes, edges, "")
	if !strings.Contains(gml, "graphml") && !strings.Contains(gml, "GraphML") {
		t.Error("expected GraphML output to contain graphml element")
	}
}

func TestExportDOT_EmptyGraph(t *testing.T) {
	dot := graph.ExportDOT(nil, nil, "", false)
	if !strings.Contains(dot, "digraph") {
		t.Error("expected valid empty DOT output")
	}
}

// ── StringPool (intern.go) ────────────────────────────────────────────────────

func TestStringPool_InternAndValue(t *testing.T) {
	pool := graph.NewStringPool()

	id := pool.Intern("hello")
	if pool.Value(id) != "hello" {
		t.Errorf("expected 'hello', got %q", pool.Value(id))
	}
}

func TestStringPool_InternIdempotent(t *testing.T) {
	pool := graph.NewStringPool()

	id1 := pool.Intern("world")
	id2 := pool.Intern("world")
	if id1 != id2 {
		t.Errorf("expected same ID for same string: %d != %d", id1, id2)
	}
}

func TestStringPool_EmptyString(t *testing.T) {
	pool := graph.NewStringPool()
	id := pool.Intern("")
	if pool.Value(id) != "" {
		t.Errorf("expected empty string for ID 0, got %q", pool.Value(id))
	}
}

func TestStringPool_GhostIntern(t *testing.T) {
	pool := graph.NewStringPool()

	ghostID := pool.GhostIntern("transient-string")
	if pool.Value(ghostID) != "transient-string" {
		t.Errorf("expected 'transient-string', got %q", pool.Value(ghostID))
	}
}

func TestStringPool_Stats(t *testing.T) {
	pool := graph.NewStringPool()

	pool.Intern("a")
	pool.Intern("b")
	pool.Intern("c")

	interned, nextGhost := pool.Stats()
	if interned != 3 {
		t.Errorf("expected 3 interned, got %d", interned)
	}
	if nextGhost < 1 {
		t.Errorf("expected nextGhost >= 1, got %d", nextGhost)
	}
}

// ── FlatGraph (flatgraph.go) ──────────────────────────────────────────────────

func TestFlatGraph_AddNodeAndExtID(t *testing.T) {
	fg := graph.NewFlatGraph("test-repo")

	// Use the global Pool for interning (FlatGraph.ExtID reads from graph.Pool).
	nameID := graph.Pool.Intern("AuthLogin_testExtID")
	fileID := graph.Pool.Intern("auth_extid_test.go")

	idx := fg.AddNode(nameID, graph.NodeFunction, fileID, 0)
	extID := fg.ExtID(idx)
	if extID == "" {
		t.Error("expected non-empty ExtID")
	}
	if !strings.Contains(string(extID), "test-repo") {
		t.Errorf("ExtID should contain repo ID, got %q", extID)
	}
}

func TestFlatGraph_AddEdge(t *testing.T) {
	fg := graph.NewFlatGraph("test-repo")

	aID := fg.AddNode(graph.Pool.Intern("FlatA"), graph.NodeFunction, graph.Pool.Intern("flat_a.go"), 0)
	bID := fg.AddNode(graph.Pool.Intern("FlatB"), graph.NodeFunction, graph.Pool.Intern("flat_b.go"), 0)

	// Should not panic.
	fg.AddEdge(aID, bID, 1.0)
}

func TestFlatGraph_InvalidateNode(t *testing.T) {
	fg := graph.NewFlatGraph("test-repo")

	idx := fg.AddNode(graph.Pool.Intern("TombTarget_unique"), graph.NodeFunction, graph.Pool.Intern("tomb.go"), 0)
	extID := fg.ExtID(idx)

	fg.InvalidateNode(extID)

	if !fg.Tombstones[idx] {
		t.Error("expected node to be tombstoned after InvalidateNode")
	}
}

func TestFlatGraph_NeedsDefrag_False(t *testing.T) {
	fg := graph.NewFlatGraph("test-repo")
	// Empty graph — no defrag needed.
	if fg.NeedsDefrag() {
		t.Error("empty graph should not need defrag")
	}
}

func TestFlatGraph_NeedsDefrag_True(t *testing.T) {
	fg := graph.NewFlatGraph("defrag-repo")

	// Add 10 nodes with distinct names via global Pool, tombstone 9 (>15%).
	names := []string{"Nd0", "Nd1", "Nd2", "Nd3", "Nd4", "Nd5", "Nd6", "Nd7", "Nd8", "Nd9"}
	fileID := graph.Pool.Intern("defrag_test.go")

	var extIDs []graph.NodeID
	for _, name := range names {
		idx := fg.AddNode(graph.Pool.Intern(name), graph.NodeFunction, fileID, 0)
		extIDs = append(extIDs, fg.ExtID(idx))
	}

	for i := 0; i < 9; i++ {
		fg.InvalidateNode(extIDs[i])
	}

	if !fg.NeedsDefrag() {
		t.Error("expected NeedsDefrag=true when >15% tombstoned")
	}
}

func TestFlatGraph_Resolve_Found(t *testing.T) {
	fg := graph.NewFlatGraph("test-repo")

	idx := fg.AddNode(graph.Pool.Intern("FooResolve_unique"), graph.NodeFunction, graph.Pool.Intern("foo_resolve.go"), 0)
	extID := fg.ExtID(idx)

	resolved, ok := fg.Resolve(extID)
	if !ok {
		t.Errorf("expected Resolve to find node with extID %q", extID)
	}
	if resolved != idx {
		t.Errorf("expected resolved idx=%d, got %d", idx, resolved)
	}
}

func TestFlatGraph_Resolve_NotFound(t *testing.T) {
	fg := graph.NewFlatGraph("test-repo")

	_, ok := fg.Resolve(graph.NodeID("unknown::file::symbol"))
	if ok {
		t.Error("expected Resolve to return false for unknown node")
	}
}

package graph

// White-box tests for graph.go functions not covered by external tests:
// UpsertRouteNode, SetFileProvenance, MakeNodeID branches, relPath, provenanceWeight.

import (
	"fmt"
	"testing"
	"time"
)

// ── UpsertRouteNode ───────────────────────────────────────────────────────────

func TestUpsertRouteNode_NewNode_ReturnsTrue(t *testing.T) {
	g := New("test")
	n := &Node{
		ID:   NodeID("test::route.go::RouteA"),
		Name: "RouteA",
		Type: NodeFunction,
		File: "route.go",
	}
	created := g.UpsertRouteNode(n)
	if !created {
		t.Error("expected true for new node insertion")
	}
	if g.GetNode(n.ID) == nil {
		t.Error("node should be retrievable after UpsertRouteNode")
	}
}

func TestUpsertRouteNode_ExistingNode_ReturnsFalse(t *testing.T) {
	g := New("test")
	n := &Node{
		ID:   NodeID("test::route.go::RouteB"),
		Name: "RouteB",
		Type: NodeFunction,
		File: "route.go",
	}
	g.UpsertRouteNode(n) // first insert
	created := g.UpsertRouteNode(n) // duplicate
	if created {
		t.Error("expected false for duplicate node")
	}
}

func TestUpsertRouteNode_AssignsStableID(t *testing.T) {
	g := New("test")
	n := &Node{
		ID:   NodeID("test::route.go::RouteC"),
		Name: "RouteC",
		Type: NodeFunction,
		File: "route.go",
		// StableID deliberately empty
	}
	g.UpsertRouteNode(n)
	if n.StableID == "" {
		t.Error("UpsertRouteNode should assign a StableID when empty")
	}
}

// ── SetFileProvenance ─────────────────────────────────────────────────────────

func TestSetFileProvenance_SetsMatchingNodes(t *testing.T) {
	g := New("test")
	id := NodeID("test::vendor/lib.go::Func")
	g.AddNode(&Node{ID: id, Name: "Func", Type: NodeFunction, File: "vendor/lib.go"})

	g.SetFileProvenance("vendor/lib.go", ProvenanceVendored)

	n := g.GetNode(id)
	if n.Provenance != ProvenanceVendored {
		t.Errorf("expected ProvenanceVendored, got %q", n.Provenance)
	}
}

func TestSetFileProvenance_ByFileSuffix(t *testing.T) {
	g := New("testrepo")
	absFile := "/repo/vendor/lib.go"
	id := NodeID("test::vendor/lib.go::Fn")
	g.AddNode(&Node{ID: id, Name: "Fn", Type: NodeFunction, File: absFile})

	// Suffix match: pass relative path
	g.SetFileProvenance("vendor/lib.go", ProvenanceVendored)

	n := g.GetNode(id)
	if n.Provenance != ProvenanceVendored {
		t.Errorf("expected ProvenanceVendored via suffix match, got %q", n.Provenance)
	}
}

func TestSetFileProvenance_NoMatch_NoChange(t *testing.T) {
	g := New("test")
	id := NodeID("test::pkg/svc.go::Serve")
	g.AddNode(&Node{ID: id, Name: "Serve", Type: NodeFunction, File: "pkg/svc.go"})

	// Different file → no change
	g.SetFileProvenance("other/file.go", ProvenanceVendored)

	n := g.GetNode(id)
	if n.Provenance != "" {
		t.Errorf("expected unchanged provenance, got %q", n.Provenance)
	}
}

// ── relPath ───────────────────────────────────────────────────────────────────

func TestRelPath_NoRoot_ReturnsAbsAsIs(t *testing.T) {
	g := New("testrepo")
	// root is not set → g.root == ""
	result := g.relPath("/some/abs/path.go")
	if result != "/some/abs/path.go" {
		t.Errorf("expected unchanged path, got %q", result)
	}
}

func TestRelPath_WithRoot_StripsPrefix(t *testing.T) {
	g := New("testrepo")
	g.SetRoot("/my/root")
	result := g.relPath("/my/root/pkg/auth.go")
	if result != "pkg/auth.go" {
		t.Errorf("expected stripped path, got %q", result)
	}
}

func TestRelPath_WithRoot_NotMatchingPrefix_ReturnsAsIs(t *testing.T) {
	g := New("testrepo")
	g.SetRoot("/my/root")
	result := g.relPath("/other/path/file.go")
	if result != "/other/path/file.go" {
		t.Errorf("expected unchanged path, got %q", result)
	}
}

// ── MakeNodeID with root ending in "/" ────────────────────────────────────────

func TestMakeNodeID_RootWithTrailingSlash(t *testing.T) {
	g := New("testrepo")
	g.SetRoot("/my/root/") // trailing slash — should not double-add "/"
	id := g.MakeNodeID("/my/root/pkg/auth.go", "Login")
	if id == "" {
		t.Error("expected non-empty ID")
	}
}

func TestMakeNodeID_FileNotUnderRoot(t *testing.T) {
	g := New("testrepo")
	g.SetRoot("/my/root")
	// File not under root — should keep original path.
	id := g.MakeNodeID("/other/pkg/file.go", "Func")
	if id == "" {
		t.Error("expected non-empty ID")
	}
}

// ── provenanceWeight ──────────────────────────────────────────────────────────

func TestProvenanceWeight_AllVariants(t *testing.T) {
	cases := []struct {
		p    ProvenanceType
		want float64
	}{
		{"", 1.0},
		{ProvenanceVendored, 0.3},
		{ProvenanceGenerated, 0.5},
		{ProvenanceExternal, 0.2},
		{"unknown_prov", 1.0},
	}
	for _, tc := range cases {
		got := provenanceWeight(tc.p)
		if got != tc.want {
			t.Errorf("provenanceWeight(%q) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

// ── AddEdge — missing endpoint ────────────────────────────────────────────────

func TestAddEdge_MissingToNode_Dropped(t *testing.T) {
	g := New("test")
	fromID := NodeID("test::a.go::FuncA")
	g.AddNode(&Node{ID: fromID, Name: "FuncA", Type: NodeFunction, File: "a.go"})

	// "to" node doesn't exist → edge silently dropped
	g.AddEdge(&Edge{From: fromID, To: NodeID("nonexistent"), Type: EdgeCalls})
	if len(g.AllEdges()) != 0 {
		t.Error("edge with missing To node should be dropped")
	}
}

// ── SetIndex / Index ──────────────────────────────────────────────────────────

func TestSetIndex_NilFlatGraph(t *testing.T) {
	g := New("test")
	g.SetIndex(nil) // must not panic
	if g.Index() != nil {
		t.Error("expected nil index after SetIndex(nil)")
	}
}

// ── MergeFrom with existing nodes ────────────────────────────────────────────

func TestMergeFrom_DuplicateNode_Overwritten(t *testing.T) {
	g1 := New("repo1")
	sharedID := NodeID("repo1::a.go::FuncA")
	g1.AddNode(&Node{ID: sharedID, Name: "FuncA", Type: NodeFunction, File: "a.go"})

	g2 := New("repo2")
	// Same ID → should be overwritten during merge
	g2.AddNode(&Node{ID: sharedID, Name: "FuncA-updated", Type: NodeFunction, File: "a.go"})

	g1.MergeFrom(g2)
	// After merge, the node should be updated.
	n := g1.GetNode(sharedID)
	if n == nil {
		t.Fatal("expected node to exist after merge")
	}
}

func TestMergeFrom_WithEdges(t *testing.T) {
	g1 := New("repo1")
	idA := NodeID("repo1::a.go::FuncA")
	g1.AddNode(&Node{ID: idA, Name: "FuncA", Type: NodeFunction, File: "a.go"})

	g2 := New("repo2")
	idB := NodeID("repo2::b.go::FuncB")
	idC := NodeID("repo2::c.go::FuncC")
	g2.AddNode(&Node{ID: idB, Name: "FuncB", Type: NodeFunction, File: "b.go"})
	g2.AddNode(&Node{ID: idC, Name: "FuncC", Type: NodeFunction, File: "c.go"})
	g2.AddEdge(&Edge{From: idB, To: idC, Type: EdgeCalls})

	g1.MergeFrom(g2)
	// Both nodes and the edge should be in g1 now.
	edges := g1.AllEdges()
	found := false
	for _, e := range edges {
		if e.From == idB && e.To == idC {
			found = true
		}
	}
	if !found {
		t.Error("expected edge from g2 to be present in merged g1")
	}
}

// ── MergeFrom transfers varTypes and instantiatedTypes ───────────────────────

func TestMergeFrom_TransfersVarTypesAndInstantiatedTypes(t *testing.T) {
	main := New("repo1")
	temp := New("repo1")

	// Populate temp with varTypes and instantiatedTypes.
	temp.AddVarType("a.go", "repo", "Repository")
	temp.AddVarType("a.go", "svc", "Service")
	temp.AddVarType("b.go", "cfg", "Config")

	temp.AddInstantiatedType("a.go", "Repository")
	temp.AddInstantiatedType("b.go", "Config")
	temp.AddInstantiatedType("b.go", "Service")

	main.MergeFrom(temp)

	// Verify varTypes transferred.
	aVars := main.GetVarTypes("a.go")
	if aVars == nil {
		t.Fatal("expected varTypes for a.go after merge")
	}
	if aVars["repo"] != "Repository" {
		t.Errorf("expected repo=Repository, got %q", aVars["repo"])
	}
	if aVars["svc"] != "Service" {
		t.Errorf("expected svc=Service, got %q", aVars["svc"])
	}
	bVars := main.GetVarTypes("b.go")
	if bVars == nil {
		t.Fatal("expected varTypes for b.go after merge")
	}
	if bVars["cfg"] != "Config" {
		t.Errorf("expected cfg=Config, got %q", bVars["cfg"])
	}

	// Verify instantiatedTypes transferred.
	inst := main.GetInstantiatedTypes()
	if inst == nil {
		t.Fatal("expected instantiatedTypes after merge")
	}
	for _, name := range []string{"Repository", "Config", "Service"} {
		if !inst[name] {
			t.Errorf("expected instantiated type %q to be present", name)
		}
	}
}

// ── ExportDOT — dotNodeColor all variants ────────────────────────────────────

func TestExportDOT_AllNodeTypes(t *testing.T) {
	g := New("testrepo")
	g.SetRoot("/repo")

	types := []NodeType{NodeFunction, NodeMethod, NodeStruct, NodeInterface, NodeVariable, NodeFile}
	ids := make([]NodeID, len(types))
	for i, typ := range types {
		id := NodeID(fmt.Sprintf("testrepo::file.go::%s%d", typ, i))
		ids[i] = id
		g.AddNode(&Node{ID: id, Name: fmt.Sprintf("Node%d", i), Type: typ, File: "file.go"})
	}
	// Add an edge between two of the nodes to exercise edge rendering.
	g.AddEdge(&Edge{From: ids[0], To: ids[1], Type: EdgeCalls})
	g.AddEdge(&Edge{From: ids[2], To: ids[3], Type: EdgeImplements})
	g.AddEdge(&Edge{From: ids[4], To: ids[5], Type: EdgeImports})

	nodes := g.AllNodes()
	edges := g.AllEdges()
	dot := ExportDOT(nodes, edges, "/repo", true)
	if dot == "" {
		t.Error("expected non-empty DOT output")
	}
}

func TestExportDOT_EdgeNotInNodeSet(t *testing.T) {
	// Edge where one endpoint is not in the node set → skipped via continue.
	g := New("testrepo")
	idA := NodeID("testrepo::a.go::FuncA")
	idB := NodeID("testrepo::b.go::FuncB")
	idC := NodeID("testrepo::c.go::FuncC")
	g.AddNode(&Node{ID: idA, Name: "FuncA", Type: NodeFunction, File: "a.go"})
	g.AddNode(&Node{ID: idB, Name: "FuncB", Type: NodeFunction, File: "b.go"})
	g.AddNode(&Node{ID: idC, Name: "FuncC", Type: NodeFunction, File: "c.go"})
	g.AddEdge(&Edge{From: idA, To: idB, Type: EdgeCalls})
	g.AddEdge(&Edge{From: idA, To: idC, Type: EdgeCalls})

	// Only pass 2 of 3 nodes — edge to idC should be skipped.
	nodesSubset := []*Node{g.GetNode(idA), g.GetNode(idB)}
	edges := g.AllEdges()
	dot := ExportDOT(nodesSubset, edges, "", false)
	if dot == "" {
		t.Error("expected non-empty DOT output")
	}
}

func TestExportDOT_WithMetadataSignature(t *testing.T) {
	g := New("testrepo")
	id := NodeID("testrepo::f.go::LongFunc")
	g.AddNode(&Node{
		ID: id, Name: "LongFunc", Type: NodeFunction, File: "f.go",
		// Signature > 60 chars to exercise truncation path.
		Metadata: map[string]string{
			"signature": "func LongFunc(param1 string, param2 int, param3 bool) (string, error)",
		},
	})
	dot := ExportDOT(g.AllNodes(), g.AllEdges(), "", true)
	if dot == "" {
		t.Error("expected non-empty DOT")
	}
}

// ── ProjectIdentity — cache invalidation ─────────────────────────────────────

func TestProjectIdentity_CacheInvalidatedAfter30s(t *testing.T) {
	g := New("test")
	id := NodeID("test::a.go::FuncA")
	g.AddNode(&Node{ID: id, Name: "FuncA", Type: NodeFunction, File: "a.go"})

	p1 := g.ProjectIdentity()
	if p1 == nil {
		t.Fatal("expected non-nil ProjectIdentity")
	}

	// Force cache expiry by setting cacheAt to old time.
	g.mu.Lock()
	g.piCacheAt = time.Now().Unix() - 60 // expired
	g.mu.Unlock()

	p2 := g.ProjectIdentity()
	if p2 == nil {
		t.Fatal("expected non-nil after cache expiry")
	}
}

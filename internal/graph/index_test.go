package graph_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── GraphIndex methods (index.go) ─────────────────────────────────────────────

func buildIndexedGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := buildFixture(t)
	_, err := g.RebuildIndex()
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	return g
}

func TestGraphIndex_Ready(t *testing.T) {
	g := buildIndexedGraph(t)
	idx := g.Index()
	if idx == nil {
		t.Fatal("expected non-nil index")
	}
	if !idx.Ready() {
		t.Error("expected index to be Ready after RebuildIndex")
	}
}

func TestGraphIndex_Seq_KnownNode(t *testing.T) {
	g := buildIndexedGraph(t)
	idx := g.Index()

	loginID := g.MakeNodeID("auth.go", "Login")
	seq := idx.Seq(loginID)
	if seq == 0 {
		t.Errorf("expected non-zero seq for known node %q", loginID)
	}
}

func TestGraphIndex_Seq_UnknownNode(t *testing.T) {
	g := buildIndexedGraph(t)
	idx := g.Index()

	seq := idx.Seq(graph.NodeID("unknown::unknown::unknown"))
	if seq != 0 {
		t.Errorf("expected seq=0 for unknown node, got %d", seq)
	}
}

func TestGraphIndex_NodeName(t *testing.T) {
	g := buildIndexedGraph(t)
	idx := g.Index()

	loginID := g.MakeNodeID("auth.go", "Login")
	seq := idx.Seq(loginID)
	name := idx.NodeName(seq)
	if name != "Login" {
		t.Errorf("expected NodeName=Login, got %q", name)
	}
}

func TestGraphIndex_NodeFile(t *testing.T) {
	g := buildIndexedGraph(t)
	idx := g.Index()

	loginID := g.MakeNodeID("auth.go", "Login")
	seq := idx.Seq(loginID)
	file := idx.NodeFile(seq)
	if file == "" {
		t.Error("expected non-empty NodeFile")
	}
}

func TestGraphIndex_IsTombstoned_False(t *testing.T) {
	g := buildIndexedGraph(t)
	idx := g.Index()

	loginID := g.MakeNodeID("auth.go", "Login")
	seq := idx.Seq(loginID)
	if idx.IsTombstoned(seq) {
		t.Error("expected Login not tombstoned in fresh index")
	}
}

func TestGraphIndex_MarkTombstone(t *testing.T) {
	g := buildIndexedGraph(t)
	idx := g.Index()

	loginID := g.MakeNodeID("auth.go", "Login")
	seq := idx.Seq(loginID)

	idx.MarkTombstone(seq)
	if !idx.IsTombstoned(seq) {
		t.Error("expected node tombstoned after MarkTombstone")
	}
}

func TestGraphIndex_TombstoneRatio(t *testing.T) {
	g := buildIndexedGraph(t)
	idx := g.Index()

	// Before any tombstones.
	ratio := idx.TombstoneRatio()
	if ratio < 0 || ratio > 1 {
		t.Errorf("expected ratio in [0,1], got %f", ratio)
	}
}

func TestGraphIndex_OutNeighbours(t *testing.T) {
	g := buildIndexedGraph(t)
	idx := g.Index()

	loginID := g.MakeNodeID("auth.go", "Login")
	seq := idx.Seq(loginID)
	targets, types := idx.OutNeighbours(seq)
	// Login calls AuthService — should have at least one out-neighbour.
	if len(targets) == 0 {
		t.Error("expected outgoing neighbours for Login")
	}
	if len(targets) != len(types) {
		t.Error("targets and types slices must have same length")
	}
}

func TestGraphIndex_InNeighbours(t *testing.T) {
	g := buildIndexedGraph(t)
	idx := g.Index()

	svcID := g.MakeNodeID("auth.go", "AuthService")
	seq := idx.Seq(svcID)
	sources, _ := idx.InNeighbours(seq)
	if len(sources) == 0 {
		t.Error("expected incoming neighbours for AuthService")
	}
}

func TestGraphIndex_OutNeighbours_InvalidSeq(t *testing.T) {
	g := buildIndexedGraph(t)
	idx := g.Index()

	targets, types := idx.OutNeighbours(99999)
	if targets != nil || types != nil {
		t.Error("expected nil for out-of-bounds seq")
	}
}

// ── EdgeCountsByType / MergeFrom / RemoveCallSitesForFile ─────────────────────

func TestEdgeCountsByType(t *testing.T) {
	g := buildFixture(t)

	counts := g.EdgeCountsByType()
	if len(counts) == 0 {
		t.Error("expected at least one edge type")
	}
	// Fixture has CALLS edges.
	if counts[graph.EdgeCalls] == 0 {
		t.Error("expected CALLS edges in fixture")
	}
}

func TestMergeFrom(t *testing.T) {
	g1 := graph.New("repo1")
	id1 := g1.MakeNodeID("a.go", "Func1")
	g1.AddNode(&graph.Node{ID: id1, Name: "Func1", File: "a.go", Type: graph.NodeFunction})

	g2 := graph.New("repo2")
	id2 := g2.MakeNodeID("b.go", "Func2")
	g2.AddNode(&graph.Node{ID: id2, Name: "Func2", File: "b.go", Type: graph.NodeFunction})

	// Merge g2 into g1.
	g1.MergeFrom(g2)

	nodes := g1.AllNodes()
	found := false
	for _, n := range nodes {
		if n.Name == "Func2" {
			found = true
		}
	}
	if !found {
		t.Error("expected Func2 from g2 to appear in g1 after MergeFrom")
	}
}

func TestRemoveCallSitesForFile(t *testing.T) {
	g := graph.New("repo")

	g.AddCallSite(graph.CallSite{CallerID: "x", CallerFile: "auth.go", FuncName: "Login"})
	g.AddCallSite(graph.CallSite{CallerID: "y", CallerFile: "handler.go", FuncName: "Handle"})

	g.RemoveCallSitesForFile("auth.go")

	remaining := g.PeekCallSites()
	if len(remaining) != 1 {
		t.Errorf("expected 1 remaining call site, got %d", len(remaining))
	}
	if remaining[0].CallerFile != "handler.go" {
		t.Errorf("expected handler.go call site, got %q", remaining[0].CallerFile)
	}
}

// ── ImpactAnalysis ────────────────────────────────────────────────────────────

func TestImpactAnalysis_KnownNode(t *testing.T) {
	g := buildFixture(t)

	svcID := g.MakeNodeID("auth.go", "AuthService")
	result, err := g.ImpactAnalysis(svcID, 3)
	if err != nil {
		t.Fatalf("ImpactAnalysis: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil ImpactResult")
	}
}

func TestImpactAnalysis_UnknownNode_ReturnsError(t *testing.T) {
	g := graph.New("repo")
	_, err := g.ImpactAnalysis(graph.NodeID("unknown::file::func"), 3)
	if err == nil {
		t.Error("expected error for unknown node")
	}
}

func TestErrNodeNotFound_Error(t *testing.T) {
	e := graph.ErrNodeNotFound("my-node-id")
	msg := e.Error()
	if msg == "" {
		t.Error("expected non-empty error message")
	}
}


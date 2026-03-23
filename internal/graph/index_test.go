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

// ── EigenvectorCentrality ──────────────────────────────────────────────────────

// TestEigenvectorCentrality_HubHighest verifies that on a star graph the hub
// node (connected to all spokes) receives the highest centrality score.
//
//	hub → A, hub → B, hub → C, hub → D
func TestEigenvectorCentrality_HubHighest(t *testing.T) {
	g := graph.New("star")
	hubID := g.MakeNodeID("hub.go", "Hub")
	g.AddNode(&graph.Node{ID: hubID, Type: graph.NodeFunction, Name: "Hub", File: "hub.go"})

	var spokeIDs []graph.NodeID
	for _, name := range []string{"A", "B", "C", "D"} {
		sid := g.MakeNodeID("spokes.go", name)
		g.AddNode(&graph.Node{ID: sid, Type: graph.NodeFunction, Name: name, File: "spokes.go"})
		g.AddEdge(&graph.Edge{From: hubID, To: sid, Type: graph.EdgeCalls})
		spokeIDs = append(spokeIDs, sid)
	}

	_, err := g.RebuildIndex()
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	idx := g.Index()

	hubSeq := idx.Seq(hubID)
	hubCent := idx.EigenvectorCentrality[hubSeq]

	// Hub must have centrality 1.0 (max-normalised).
	if hubCent != 1.0 {
		t.Errorf("hub centrality = %f, want 1.0", hubCent)
	}

	// Every spoke must have centrality strictly less than the hub.
	for _, sid := range spokeIDs {
		seq := idx.Seq(sid)
		c := idx.EigenvectorCentrality[seq]
		if c >= hubCent {
			t.Errorf("spoke centrality %f >= hub centrality %f", c, hubCent)
		}
	}
}

// TestEigenvectorCentrality_ChainMiddleHigher verifies that middle nodes in a
// chain (A→B→C→D) have higher centrality than the endpoints.
func TestEigenvectorCentrality_ChainMiddleHigher(t *testing.T) {
	g := graph.New("chain")
	names := []string{"A", "B", "C", "D"}
	ids := make([]graph.NodeID, len(names))
	for i, name := range names {
		id := g.MakeNodeID("chain.go", name)
		ids[i] = id
		g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: name, File: "chain.go"})
	}
	for i := 0; i < len(ids)-1; i++ {
		g.AddEdge(&graph.Edge{From: ids[i], To: ids[i+1], Type: graph.EdgeCalls})
	}

	_, err := g.RebuildIndex()
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	idx := g.Index()

	seqA := idx.Seq(ids[0])
	seqB := idx.Seq(ids[1])
	seqC := idx.Seq(ids[2])
	seqD := idx.Seq(ids[3])

	centA := idx.EigenvectorCentrality[seqA]
	centB := idx.EigenvectorCentrality[seqB]
	centC := idx.EigenvectorCentrality[seqC]
	centD := idx.EigenvectorCentrality[seqD]

	// Both endpoints (A, D) should have lower centrality than interior (B, C).
	if centA >= centB {
		t.Errorf("endpoint A centrality %f >= interior B centrality %f", centA, centB)
	}
	if centD >= centC {
		t.Errorf("endpoint D centrality %f >= interior C centrality %f", centD, centC)
	}
}

// TestEigenvectorCentrality_EmptyGraph verifies no panic and a valid (non-nil)
// centrality slice for a graph with no edges.
func TestEigenvectorCentrality_EmptyGraph(t *testing.T) {
	g := graph.New("empty")
	_, err := g.RebuildIndex()
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	idx := g.Index()
	if idx.EigenvectorCentrality == nil {
		t.Error("expected non-nil EigenvectorCentrality for empty graph")
	}
}

// TestEigenvectorCentrality_InRange verifies all centrality values are in [0, 1].
func TestEigenvectorCentrality_InRange(t *testing.T) {
	g := buildIndexedGraph(t) // uses buildFixture (5-node graph with edges)
	idx := g.Index()

	for i, c := range idx.EigenvectorCentrality {
		if c < 0 || c > 1.0+1e-9 {
			t.Errorf("centrality[%d] = %f out of [0,1]", i, c)
		}
	}
}

// TestEigenvectorCentrality_BoostsInCarve verifies that a node with high
// centrality ranks higher than a same-hop node with low centrality after
// CarveEgoGraph applies the centrality boost.
//
// Graph: root → central (also called by A,B,C) ; root → leaf (no other edges)
// Both are 1 hop from root — leaf should be outranked by central after boost.
func TestEigenvectorCentrality_BoostsInCarve(t *testing.T) {
	g := graph.New("boost")

	rootID := g.MakeNodeID("x.go", "Root")
	centralID := g.MakeNodeID("x.go", "Central")
	leafID := g.MakeNodeID("x.go", "Leaf")

	g.AddNode(&graph.Node{ID: rootID, Type: graph.NodeFunction, Name: "Root", File: "x.go"})
	g.AddNode(&graph.Node{ID: centralID, Type: graph.NodeFunction, Name: "Central", File: "x.go"})
	g.AddNode(&graph.Node{ID: leafID, Type: graph.NodeFunction, Name: "Leaf", File: "x.go"})

	// Both reachable from root at 1 hop.
	g.AddEdge(&graph.Edge{From: rootID, To: centralID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: rootID, To: leafID, Type: graph.EdgeCalls})

	// Extra callers that make Central architecturally important.
	for _, name := range []string{"Caller1", "Caller2", "Caller3"} {
		cid := g.MakeNodeID("callers.go", name)
		g.AddNode(&graph.Node{ID: cid, Type: graph.NodeFunction, Name: name, File: "callers.go"})
		g.AddEdge(&graph.Edge{From: cid, To: centralID, Type: graph.EdgeCalls})
	}

	_, err := g.RebuildIndex()
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	cfg := graph.DefaultCarveConfig()
	sub, err := g.CarveEgoGraph(rootID, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	// Find relevance scores for central and leaf.
	var centralRel, leafRel float64
	for _, cn := range sub.Nodes {
		switch cn.Node.ID {
		case centralID:
			centralRel = cn.Relevance
		case leafID:
			leafRel = cn.Relevance
		}
	}

	if centralRel == 0 || leafRel == 0 {
		t.Fatalf("expected both central (%f) and leaf (%f) in sub-graph", centralRel, leafRel)
	}
	if centralRel <= leafRel {
		t.Errorf("expected central (%f) to outrank leaf (%f) after centrality boost", centralRel, leafRel)
	}
}


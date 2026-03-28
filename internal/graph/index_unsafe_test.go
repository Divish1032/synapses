package graph

import (
	"sort"
	"testing"
)

// helper: build a graph with MyStruct (struct) + MyStruct.Foo, MyStruct.Bar (methods)
// and an edge Foo -> Bar, then rebuild the index.
func buildMethodGraph(t *testing.T) *Graph {
	t.Helper()
	g := New("test-repo")

	structID := g.MakeNodeID("my.go", "MyStruct")
	fooID := g.MakeNodeID("my.go", "MyStruct.Foo")
	barID := g.MakeNodeID("my.go", "MyStruct.Bar")

	g.AddNode(&Node{ID: structID, Name: "MyStruct", Type: NodeStruct, File: "my.go", Package: "mypkg", Exported: true})
	g.AddNode(&Node{ID: fooID, Name: "MyStruct.Foo", Type: NodeMethod, File: "my.go", Package: "mypkg", Exported: true})
	g.AddNode(&Node{ID: barID, Name: "MyStruct.Bar", Type: NodeMethod, File: "my.go", Package: "mypkg", Exported: true})

	g.AddEdge(&Edge{From: fooID, To: barID, Type: EdgeCalls})

	if _, err := g.RebuildIndex(); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	return g
}

func TestReceiverMethodSeqs(t *testing.T) {
	g := buildMethodGraph(t)
	idx := g.Index()

	seqs := idx.ReceiverMethodSeqs("MyStruct")
	if len(seqs) != 2 {
		t.Fatalf("expected 2 method seqs, got %d", len(seqs))
	}

	// Verify both seqs map back to the expected method names.
	names := make([]string, len(seqs))
	for i, s := range seqs {
		names[i] = idx.NodeName(s)
	}
	sort.Strings(names)
	if names[0] != "MyStruct.Bar" || names[1] != "MyStruct.Foo" {
		t.Errorf("unexpected method names: %v", names)
	}
}

func TestUnsafeSeq_MatchesSafeSeq(t *testing.T) {
	g := buildMethodGraph(t)
	idx := g.Index()

	fooID := g.MakeNodeID("my.go", "MyStruct.Foo")

	safe := idx.Seq(fooID)
	unsafe := idx.UnsafeSeq(fooID)
	if safe != unsafe {
		t.Errorf("Seq=%d != UnsafeSeq=%d", safe, unsafe)
	}
	if safe == 0 {
		t.Error("expected non-zero seq for known node")
	}
}

func TestUnsafeOutNeighbours_MatchesSafe(t *testing.T) {
	g := buildMethodGraph(t)
	idx := g.Index()

	fooSeq := idx.Seq(g.MakeNodeID("my.go", "MyStruct.Foo"))

	safeTargets, safeTypes := idx.OutNeighbours(fooSeq)
	unsafeTargets, unsafeTypes := idx.UnsafeOutNeighbours(fooSeq)

	if len(safeTargets) != len(unsafeTargets) {
		t.Fatalf("target length mismatch: safe=%d unsafe=%d", len(safeTargets), len(unsafeTargets))
	}
	for i := range safeTargets {
		if safeTargets[i] != unsafeTargets[i] {
			t.Errorf("target[%d]: safe=%d unsafe=%d", i, safeTargets[i], unsafeTargets[i])
		}
		if safeTypes[i] != unsafeTypes[i] {
			t.Errorf("type[%d]: safe=%d unsafe=%d", i, safeTypes[i], unsafeTypes[i])
		}
	}
}

func TestUnsafeIsTombstoned(t *testing.T) {
	g := buildMethodGraph(t)
	idx := g.Index()

	fooSeq := idx.Seq(g.MakeNodeID("my.go", "MyStruct.Foo"))

	if idx.UnsafeIsTombstoned(fooSeq) {
		t.Error("expected live node to not be tombstoned")
	}

	idx.MarkTombstone(fooSeq)

	if !idx.UnsafeIsTombstoned(fooSeq) {
		t.Error("expected node to be tombstoned after MarkTombstone")
	}
}

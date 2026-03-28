package graph_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// buildEdgeTestGraph creates a small graph:
//
//	a.go:  Foo --CALLS--> Bar (in b.go)
//	b.go:  Bar --CALLS--> Baz (in b.go, self-edge within b.go)
//	c.go:  Qux (isolated)
func buildEdgeTestGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("repo")

	fooID := g.MakeNodeID("a.go", "Foo")
	g.AddNode(&graph.Node{ID: fooID, Type: graph.NodeFunction, Name: "Foo", File: "a.go", Package: "pkga"})

	barID := g.MakeNodeID("b.go", "Bar")
	g.AddNode(&graph.Node{ID: barID, Type: graph.NodeFunction, Name: "Bar", File: "b.go", Package: "pkgb"})

	bazID := g.MakeNodeID("b.go", "Baz")
	g.AddNode(&graph.Node{ID: bazID, Type: graph.NodeFunction, Name: "Baz", File: "b.go", Package: "pkgb"})

	quxID := g.MakeNodeID("c.go", "Qux")
	g.AddNode(&graph.Node{ID: quxID, Type: graph.NodeFunction, Name: "Qux", File: "c.go", Package: "pkgc"})

	// a.go → b.go (cross-file edge)
	g.AddEdge(&graph.Edge{From: fooID, To: barID, Type: graph.EdgeCalls})
	// b.go internal edge
	g.AddEdge(&graph.Edge{From: barID, To: bazID, Type: graph.EdgeCalls})

	return g
}

func TestInEdgesForFile_ReturnsIncomingEdgesToFile(t *testing.T) {
	g := buildEdgeTestGraph(t)

	// b.go nodes: Bar and Baz.
	// Incoming to Bar: Foo→Bar (from a.go)
	// Incoming to Baz: Bar→Baz (within b.go)
	// Total incoming to any b.go node = 2.
	inEdges := g.InEdgesForFile("b.go")
	if len(inEdges) != 2 {
		t.Fatalf("want 2 incoming edges to b.go nodes, got %d: %v", len(inEdges), inEdges)
	}
	for _, e := range inEdges {
		if e.Type != graph.EdgeCalls {
			t.Errorf("want EdgeCalls, got %v", e.Type)
		}
	}
}

func TestInEdgesForFile_NoIncomingEdges(t *testing.T) {
	g := buildEdgeTestGraph(t)

	// a.go has no incoming edges (it only sends)
	inEdges := g.InEdgesForFile("a.go")
	if len(inEdges) != 0 {
		t.Errorf("want 0 incoming edges for a.go, got %d", len(inEdges))
	}
}

func TestEdgesForFile_UnionOfBothDirections(t *testing.T) {
	g := buildEdgeTestGraph(t)

	// b.go has:
	//   OUT: Bar→Baz (outgoing, within b.go)
	//   IN:  Foo→Bar (incoming from a.go)
	// Plus the internal Bar→Baz appears in OUT for Bar node
	edges := g.EdgesForFile("b.go")
	if len(edges) < 2 {
		t.Fatalf("want at least 2 edges for b.go, got %d: %v", len(edges), edges)
	}

	seen := make(map[string]bool)
	for _, e := range edges {
		seen[string(e.From)+">"+string(e.To)] = true
	}
	fooID := g.MakeNodeID("a.go", "Foo")
	barID := g.MakeNodeID("b.go", "Bar")
	bazID := g.MakeNodeID("b.go", "Baz")

	if !seen[string(fooID)+">"+string(barID)] {
		t.Error("EdgesForFile(b.go) missing incoming edge Foo→Bar")
	}
	if !seen[string(barID)+">"+string(bazID)] {
		t.Error("EdgesForFile(b.go) missing outgoing edge Bar→Baz")
	}
}

func TestEdgesForFile_SelfEdgeDeduplicatedOnce(t *testing.T) {
	g := graph.New("repo")
	fooID := g.MakeNodeID("a.go", "Foo")
	g.AddNode(&graph.Node{ID: fooID, Type: graph.NodeFunction, Name: "Foo", File: "a.go", Package: "p"})
	// Self-edge within a.go
	g.AddEdge(&graph.Edge{From: fooID, To: fooID, Type: graph.EdgeCalls})

	edges := g.EdgesForFile("a.go")
	if len(edges) != 1 {
		t.Errorf("self-edge should appear exactly once, got %d edges", len(edges))
	}
}

func TestEdgesForFile_IsolatedFileReturnsEmpty(t *testing.T) {
	g := buildEdgeTestGraph(t)

	edges := g.EdgesForFile("c.go") // Qux has no edges
	if len(edges) != 0 {
		t.Errorf("want 0 edges for isolated c.go, got %d", len(edges))
	}
}

func TestEdgesForFile_UnknownFileReturnsEmpty(t *testing.T) {
	g := buildEdgeTestGraph(t)

	edges := g.EdgesForFile("nonexistent.go")
	if edges != nil && len(edges) != 0 {
		t.Errorf("want empty for unknown file, got %d edges", len(edges))
	}
}

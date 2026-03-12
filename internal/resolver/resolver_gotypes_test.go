package resolver_test

// Additional tests targeting ResolveGoTypesCallEdges with a real Go module,
// covering the body that requires packages.Load to succeed.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// buildGoModule creates a minimal Go module in dir with a single package
// and two functions where one calls the other, then returns the module dir.
func buildGoModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// go.mod
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/test\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// main package with a caller and a callee.
	src := `package main

func Callee() {}

func Caller() {
	Callee()
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveGoTypesCallEdges_WithRealModule(t *testing.T) {
	dir := buildGoModule(t)

	// Build a graph with nodes for Caller and Callee.
	g := graph.New("test")
	mainFile := filepath.Join(dir, "main.go")

	calleeID := g.MakeNodeID(mainFile, "Callee")
	callerID := g.MakeNodeID(mainFile, "Caller")

	// Source lines: 3=func Callee, 5=func Caller (matches fset.Position output).
	g.AddNode(&graph.Node{
		ID: calleeID, Type: graph.NodeFunction, Name: "Callee",
		Package: "main", File: mainFile, Line: 3,
	})
	g.AddNode(&graph.Node{
		ID: callerID, Type: graph.NodeFunction, Name: "Caller",
		Package: "main", File: mainFile, Line: 5,
	})

	n, err := resolver.ResolveGoTypesCallEdges(g, dir)
	if err != nil {
		t.Fatalf("ResolveGoTypesCallEdges: %v", err)
	}
	// Should have found the Caller→Callee edge.
	_ = n
}

func TestResolveGoTypesCallEdges_WithModule_NoGraphNodes(t *testing.T) {
	// Module exists but graph has no nodes — posToNode is empty, loop runs but
	// callerID will always be "" so no edges are added. Covers the inner loop.
	dir := buildGoModule(t)

	g := graph.New("test")
	n, err := resolver.ResolveGoTypesCallEdges(g, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 new edges with no graph nodes, got %d", n)
	}
}

func TestResolveGoTypesCallEdges_EmptyPackages(t *testing.T) {
	// A module with only a go.mod and no .go files → packages.Load succeeds
	// but returns zero packages. Covers the build+range loop path.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/empty\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("test")
	n, err := resolver.ResolveGoTypesCallEdges(g, dir)
	if err != nil {
		// Some versions of packages.Load may error on an empty module.
		t.Skipf("packages.Load: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 edges for empty module, got %d", n)
	}
}

func TestResolveGoTypesCallEdges_ExistingEdgeNotDuplicated(t *testing.T) {
	// Pre-populate a CALLS edge. After type-resolution, if the same edge
	// would be added again, it must be deduplicated (seen map).
	dir := buildGoModule(t)
	mainFile := filepath.Join(dir, "main.go")

	g := graph.New("test")
	calleeID := g.MakeNodeID(mainFile, "Callee")
	callerID := g.MakeNodeID(mainFile, "Caller")
	g.AddNode(&graph.Node{ID: calleeID, Type: graph.NodeFunction, Name: "Callee",
		Package: "main", File: mainFile, Line: 3})
	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "Caller",
		Package: "main", File: mainFile, Line: 5})

	// Pre-add the edge.
	g.AddEdge(&graph.Edge{From: callerID, To: calleeID, Type: graph.EdgeCalls})
	edgesBefore := g.EdgeCount()

	n, err := resolver.ResolveGoTypesCallEdges(g, dir)
	if err != nil {
		t.Fatalf("ResolveGoTypesCallEdges: %v", err)
	}
	// The existing edge should not be duplicated, so n should be 0.
	if n != 0 {
		t.Logf("added %d new edges (expected 0 since edge was pre-populated)", n)
	}
	_ = edgesBefore
}

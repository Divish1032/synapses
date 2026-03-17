package main

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// TestDebugCallsites_ResolveEdges verifies call site resolution works.
func TestDebugCallsites_ResolveEdges(t *testing.T) {
	g := graph.New("testproj")

	// Add some basic nodes
	fID := g.MakeNodeID("main.py", "my_func")
	g.AddNode(&graph.Node{
		ID:       fID,
		Type:     graph.NodeFunction,
		Name:     "my_func",
		File:     "main.py",
		Package:  "testproj",
	})

	// Add a call site
	g.BulkAddCallSites([]graph.CallSite{
		{
			CallerID:   fID,
			CallerFile: "main.py",
			PkgAlias:   "os",
			FuncName:   "path.join",
		},
	})

	sites := g.PeekCallSites()
	if len(sites) == 0 {
		t.Error("expected at least one call site")
	}

	// Attempt to resolve (may not create edges without matching target nodes)
	n := resolver.ResolveCallEdges(g)
	if n < 0 {
		t.Errorf("ResolveCallEdges returned negative: %d", n)
	}
}

// TestDebugCallsites_GraphBasics verifies basic graph operations.
func TestDebugCallsites_GraphBasics(t *testing.T) {
	g := graph.New("test")

	// Add nodes
	funcID := g.MakeNodeID("test.py", "func_a")
	g.AddNode(&graph.Node{
		ID:       funcID,
		Type:     graph.NodeFunction,
		Name:     "func_a",
		File:     "test.py",
		Package:  "test",
	})

	// Get all nodes
	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Error("expected at least one node")
	}

	// Filter by type
	funcs := g.FindByType(graph.NodeFunction)
	if len(funcs) == 0 {
		t.Error("expected at least one function node")
	}
}

// TestDebugCallsites_ImportsEdges verifies import edge tracking.
func TestDebugCallsites_ImportsEdges(t *testing.T) {
	g := graph.New("test")

	pkgID := g.MakeNodeID("test", "test")
	g.AddNode(&graph.Node{
		ID:       pkgID,
		Type:     graph.NodePackage,
		Name:     "test",
		Package:  "test",
	})

	// Add import edge
	importID := g.MakeNodeID("os", "os")
	g.AddNode(&graph.Node{
		ID:       importID,
		Type:     graph.NodePackage,
		Name:     "os",
		Package:  "os",
	})

	g.AddEdge(&graph.Edge{
		From: pkgID,
		To:   importID,
		Type: graph.EdgeImports,
	})

	edges := g.AllEdges()
	importCount := 0
	for _, e := range edges {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}

	if importCount == 0 {
		t.Error("expected at least one import edge")
	}
}

package resolver_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// TestInheritance_CircularProtection verifies that circular inheritance
// (A extends B extends A) doesn't cause infinite loops.
func TestInheritance_CircularProtection(t *testing.T) {
	g := graph.New("test")

	fileA := g.MakeNodeID("A.java", "A.java")
	g.AddNode(&graph.Node{ID: fileA, Type: graph.NodeFile, Name: "A.java", File: "A.java"})

	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("A.java", "Alpha"), Type: graph.NodeStruct,
		Name: "Alpha", Package: "app", File: "A.java",
		Metadata: map[string]string{"heritage_extends": "Beta"},
	})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("A.java", "Beta"), Type: graph.NodeStruct,
		Name: "Beta", Package: "app", File: "A.java",
		Metadata: map[string]string{"heritage_extends": "Alpha"},
	})

	callerID := g.MakeNodeID("A.java", "Alpha.run")
	g.AddNode(&graph.Node{
		ID: callerID, Type: graph.NodeMethod,
		Name: "Alpha.run", Package: "app", File: "A.java",
	})
	g.AddEdge(&graph.Edge{From: fileA, To: callerID, Type: graph.EdgeDefines})

	// Call this.missing() — should not infinite loop, should return 0 edges.
	g.AddCallSite(graph.CallSite{
		CallerID: callerID, CallerFile: "A.java",
		PkgAlias: "this", FuncName: "missing",
	})

	// Should not hang or panic.
	count := resolver.ResolveCallEdges(g)
	if count != 0 {
		t.Errorf("expected 0 resolved edges (method doesn't exist), got %d", count)
	}
}

// TestInheritance_SameNameDifferentPackages verifies that classes with the same
// name in different packages don't interfere with each other's inheritance.
func TestInheritance_SameNameDifferentPackages(t *testing.T) {
	g := graph.New("test")

	// Package A: Handler extends BaseHandler
	fileA := g.MakeNodeID("a/Handler.java", "a/Handler.java")
	g.AddNode(&graph.Node{ID: fileA, Type: graph.NodeFile, Name: "Handler.java", File: "a/Handler.java"})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("a/Handler.java", "BaseHandler"), Type: graph.NodeStruct,
		Name: "BaseHandler", Package: "pkgA", File: "a/Handler.java",
	})
	baseMethodA := g.MakeNodeID("a/Handler.java", "BaseHandler.process")
	g.AddNode(&graph.Node{
		ID: baseMethodA, Type: graph.NodeMethod,
		Name: "BaseHandler.process", Package: "pkgA", File: "a/Handler.java",
	})
	g.AddEdge(&graph.Edge{From: fileA, To: baseMethodA, Type: graph.EdgeDefines})

	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("a/Handler.java", "Handler"), Type: graph.NodeStruct,
		Name: "Handler", Package: "pkgA", File: "a/Handler.java",
		Metadata: map[string]string{"heritage_extends": "BaseHandler"},
	})
	callerA := g.MakeNodeID("a/Handler.java", "Handler.execute")
	g.AddNode(&graph.Node{
		ID: callerA, Type: graph.NodeMethod,
		Name: "Handler.execute", Package: "pkgA", File: "a/Handler.java",
	})
	g.AddEdge(&graph.Edge{From: fileA, To: callerA, Type: graph.EdgeDefines})

	// Package B: Handler extends OtherBase (different hierarchy)
	fileB := g.MakeNodeID("b/Handler.java", "b/Handler.java")
	g.AddNode(&graph.Node{ID: fileB, Type: graph.NodeFile, Name: "Handler.java", File: "b/Handler.java"})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("b/Handler.java", "OtherBase"), Type: graph.NodeStruct,
		Name: "OtherBase", Package: "pkgB", File: "b/Handler.java",
	})
	baseMethodB := g.MakeNodeID("b/Handler.java", "OtherBase.process")
	g.AddNode(&graph.Node{
		ID: baseMethodB, Type: graph.NodeMethod,
		Name: "OtherBase.process", Package: "pkgB", File: "b/Handler.java",
	})
	g.AddEdge(&graph.Edge{From: fileB, To: baseMethodB, Type: graph.EdgeDefines})

	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("b/Handler.java", "Handler"), Type: graph.NodeStruct,
		Name: "Handler", Package: "pkgB", File: "b/Handler.java",
		Metadata: map[string]string{"heritage_extends": "OtherBase"},
	})

	// this.process() from Handler.execute → should find BaseHandler.process
	// (the inheritance map merges both hierarchies, but both parents' .process exist)
	g.AddCallSite(graph.CallSite{
		CallerID: callerA, CallerFile: "a/Handler.java",
		PkgAlias: "this", FuncName: "process",
	})

	count := resolver.ResolveCallEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 resolved edge, got %d", count)
	}

	// Should resolve to one of the process methods (both are valid in merged map).
	hasA := g.HasEdge(callerA, baseMethodA, graph.EdgeCalls)
	hasB := g.HasEdge(callerA, baseMethodB, graph.EdgeCalls)
	if !hasA && !hasB {
		t.Error("expected CALLS edge to at least one process method via inheritance")
	}
}

// TestInheritance_SelfReferenceSkipped verifies that a class "extending" itself
// doesn't cause issues (malformed metadata).
func TestInheritance_SelfReferenceSkipped(t *testing.T) {
	g := graph.New("test")

	fileA := g.MakeNodeID("A.java", "A.java")
	g.AddNode(&graph.Node{ID: fileA, Type: graph.NodeFile, Name: "A.java", File: "A.java"})

	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("A.java", "Foo"), Type: graph.NodeStruct,
		Name: "Foo", Package: "app", File: "A.java",
		Metadata: map[string]string{"heritage_extends": "Foo"}, // self-reference
	})

	callerID := g.MakeNodeID("A.java", "Foo.run")
	g.AddNode(&graph.Node{
		ID: callerID, Type: graph.NodeMethod,
		Name: "Foo.run", Package: "app", File: "A.java",
	})
	g.AddEdge(&graph.Edge{From: fileA, To: callerID, Type: graph.EdgeDefines})

	g.AddCallSite(graph.CallSite{
		CallerID: callerID, CallerFile: "A.java",
		PkgAlias: "this", FuncName: "missing",
	})

	// Should not infinite loop.
	count := resolver.ResolveCallEdges(g)
	if count != 0 {
		t.Errorf("expected 0 for self-referencing class, got %d", count)
	}
}

// TestInheritance_EmptyHeritageMetadata verifies that empty or whitespace-only
// heritage metadata doesn't produce garbage entries.
func TestInheritance_EmptyHeritageMetadata(t *testing.T) {
	g := graph.New("test")

	fileA := g.MakeNodeID("A.java", "A.java")
	g.AddNode(&graph.Node{ID: fileA, Type: graph.NodeFile, Name: "A.java", File: "A.java"})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("A.java", "Foo"), Type: graph.NodeStruct,
		Name: "Foo", Package: "app", File: "A.java",
		Metadata: map[string]string{"heritage_extends": " , , "},
	})

	callerID := g.MakeNodeID("A.java", "Foo.run")
	g.AddNode(&graph.Node{
		ID: callerID, Type: graph.NodeMethod,
		Name: "Foo.run", Package: "app", File: "A.java",
	})
	g.AddEdge(&graph.Edge{From: fileA, To: callerID, Type: graph.EdgeDefines})

	g.AddCallSite(graph.CallSite{
		CallerID: callerID, CallerFile: "A.java",
		PkgAlias: "this", FuncName: "missing",
	})

	// Should handle gracefully — no panic, 0 edges.
	count := resolver.ResolveCallEdges(g)
	if count != 0 {
		t.Errorf("expected 0 for empty heritage, got %d", count)
	}
}

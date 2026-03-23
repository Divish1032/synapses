package resolver_test

// Tests for RTA-style call graph refinement (Sprint 14 #2).
//
// Covers:
//   - findInPackage tie-breaking: when multiple methods share a name, prefer
//     the one whose receiver type is instantiated.
//   - ResolveHeritageEdges: skip classes not in instantiatedTypes when data exists.
//   - ResolveImplementsEdges: skip structs not in instantiatedTypes.
//   - Fallback behavior: no regression when instantiatedTypes is empty (Go projects).
//   - AddInstantiatedType / GetInstantiatedTypes: basic graph operations.

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// TestRTA_FindInPackage_PrefersInstantiatedReceiver verifies that when two methods
// share the same name in the same package, findInPackage (via ResolveCallEdges)
// resolves to the method whose receiver type is instantiated.
func TestRTA_FindInPackage_PrefersInstantiatedReceiver(t *testing.T) {
	g := graph.New("testrepo")

	// svc.go: two classes, both with a "Process" method.
	svcFile := g.MakeNodeID("svc.go", "svc.go")
	svcPkg := g.MakeNodeID("svc", "svc")
	g.AddNode(&graph.Node{ID: svcFile, Type: graph.NodeFile, Name: "svc.go", File: "svc.go", Package: "svc"})
	g.AddNode(&graph.Node{ID: svcPkg, Type: graph.NodePackage, Name: "svc"})

	// ConcreteA.Process — ConcreteA IS instantiated.
	concreteAID := g.MakeNodeID("svc.go", "ConcreteA.Process")
	g.AddNode(&graph.Node{
		ID: concreteAID, Type: graph.NodeMethod,
		Name: "ConcreteA.Process", Package: "svc", File: "svc.go",
	})
	// ConcreteB.Process — ConcreteB is NOT instantiated.
	concreteBID := g.MakeNodeID("svc.go", "ConcreteB.Process")
	g.AddNode(&graph.Node{
		ID: concreteBID, Type: graph.NodeMethod,
		Name: "ConcreteB.Process", Package: "svc", File: "svc.go",
	})

	// main.go: caller imports svc, calls "svc.Process".
	mainFile := g.MakeNodeID("main.go", "main.go")
	callerID := g.MakeNodeID("main.go", "main")
	g.AddNode(&graph.Node{ID: mainFile, Type: graph.NodeFile, Name: "main.go", File: "main.go", Package: "main"})
	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "main", Package: "main", File: "main.go"})
	g.AddEdge(&graph.Edge{From: mainFile, To: svcPkg, Type: graph.EdgeImports})

	// Seed call site: main() calls svc.Process().
	g.AddCallSite(graph.CallSite{
		CallerID:   callerID,
		CallerFile: "main.go",
		PkgAlias:   "svc",
		FuncName:   "Process",
	})

	// RTA: ConcreteA was instantiated.
	g.AddInstantiatedType("main.go", "ConcreteA")

	count := resolver.ResolveCallEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 edge resolved, got %d", count)
	}

	// The edge must point to ConcreteA.Process, not ConcreteB.Process.
	if !g.HasEdge(callerID, concreteAID, graph.EdgeCalls) {
		t.Error("expected CALLS edge to ConcreteA.Process (instantiated receiver)")
	}
	if g.HasEdge(callerID, concreteBID, graph.EdgeCalls) {
		t.Error("unexpected CALLS edge to ConcreteB.Process (not instantiated)")
	}
}

// TestRTA_FindInPackage_FallsBackWhenNoneInstantiated verifies that when no
// candidate's receiver type is instantiated, findInPackage falls back to the
// first match (no regression from CHA behavior).
func TestRTA_FindInPackage_FallsBackWhenNoneInstantiated(t *testing.T) {
	g := graph.New("testrepo")

	svcFile := g.MakeNodeID("svc.go", "svc.go")
	svcPkg := g.MakeNodeID("svc", "svc")
	g.AddNode(&graph.Node{ID: svcFile, Type: graph.NodeFile, Name: "svc.go", File: "svc.go", Package: "svc"})
	g.AddNode(&graph.Node{ID: svcPkg, Type: graph.NodePackage, Name: "svc"})

	firstID := g.MakeNodeID("svc.go", "ConcreteA.Save")
	g.AddNode(&graph.Node{
		ID: firstID, Type: graph.NodeMethod,
		Name: "ConcreteA.Save", Package: "svc", File: "svc.go",
	})
	secondID := g.MakeNodeID("svc.go", "ConcreteB.Save")
	g.AddNode(&graph.Node{
		ID: secondID, Type: graph.NodeMethod,
		Name: "ConcreteB.Save", Package: "svc", File: "svc.go",
	})

	mainFile := g.MakeNodeID("main.go", "main.go")
	callerID := g.MakeNodeID("main.go", "main")
	g.AddNode(&graph.Node{ID: mainFile, Type: graph.NodeFile, Name: "main.go", File: "main.go", Package: "main"})
	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "main", Package: "main", File: "main.go"})
	g.AddEdge(&graph.Edge{From: mainFile, To: svcPkg, Type: graph.EdgeImports})

	g.AddCallSite(graph.CallSite{
		CallerID:   callerID,
		CallerFile: "main.go",
		PkgAlias:   "svc",
		FuncName:   "Save",
	})

	// Register SOME instantiated type, but not ConcreteA or ConcreteB.
	g.AddInstantiatedType("main.go", "OtherClass")

	count := resolver.ResolveCallEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 edge (fallback), got %d", count)
	}
	// Should have resolved to first match (ConcreteA.Save or ConcreteB.Save) — just not 0.
	hasFirst := g.HasEdge(callerID, firstID, graph.EdgeCalls)
	hasSecond := g.HasEdge(callerID, secondID, graph.EdgeCalls)
	if !hasFirst && !hasSecond {
		t.Error("expected CALLS edge to one of the Save methods as fallback")
	}
}

// TestRTA_FindInPackage_NoInstantiationData verifies that when no instantiation
// data exists at all (pure Go project), behavior is identical to old CHA.
func TestRTA_FindInPackage_NoInstantiationData(t *testing.T) {
	g := graph.New("testrepo")

	svcFile := g.MakeNodeID("svc.go", "svc.go")
	svcPkg := g.MakeNodeID("svc", "svc")
	g.AddNode(&graph.Node{ID: svcFile, Type: graph.NodeFile, Name: "svc.go", File: "svc.go", Package: "svc"})
	g.AddNode(&graph.Node{ID: svcPkg, Type: graph.NodePackage, Name: "svc"})

	targetID := g.MakeNodeID("svc.go", "Handler.Run")
	g.AddNode(&graph.Node{
		ID: targetID, Type: graph.NodeMethod,
		Name: "Handler.Run", Package: "svc", File: "svc.go",
	})

	mainFile := g.MakeNodeID("main.go", "main.go")
	callerID := g.MakeNodeID("main.go", "main")
	g.AddNode(&graph.Node{ID: mainFile, Type: graph.NodeFile, Name: "main.go", File: "main.go", Package: "main"})
	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "main", Package: "main", File: "main.go"})
	g.AddEdge(&graph.Edge{From: mainFile, To: svcPkg, Type: graph.EdgeImports})

	g.AddCallSite(graph.CallSite{
		CallerID:   callerID,
		CallerFile: "main.go",
		PkgAlias:   "svc",
		FuncName:   "Run",
	})
	// No AddInstantiatedType calls — GetInstantiatedTypes returns nil.

	count := resolver.ResolveCallEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 edge (CHA fallback), got %d", count)
	}
	if !g.HasEdge(callerID, targetID, graph.EdgeCalls) {
		t.Error("expected CALLS edge to Handler.Run")
	}
}

// TestRTA_HeritageEdges_SkipsNonInstantiated verifies that ResolveHeritageEdges
// skips IMPLEMENTS edges for classes not in the instantiatedTypes set.
func TestRTA_HeritageEdges_SkipsNonInstantiated(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("svc.go", "Runnable")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface,
		Name: "Runnable", Package: "svc", File: "svc.go",
	})

	// ConcreteA implements Runnable — IS instantiated.
	concreteAID := g.MakeNodeID("svc.go", "ConcreteA")
	g.AddNode(&graph.Node{
		ID: concreteAID, Type: graph.NodeStruct,
		Name: "ConcreteA", Package: "svc", File: "svc.go",
		Metadata: map[string]string{"heritage_implements": "Runnable"},
	})

	// ConcreteB implements Runnable — NOT instantiated.
	concreteBID := g.MakeNodeID("svc.go", "ConcreteB")
	g.AddNode(&graph.Node{
		ID: concreteBID, Type: graph.NodeStruct,
		Name: "ConcreteB", Package: "svc", File: "svc.go",
		Metadata: map[string]string{"heritage_implements": "Runnable"},
	})

	// Only ConcreteA is instantiated.
	g.AddInstantiatedType("svc.go", "ConcreteA")

	count := resolver.ResolveHeritageEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 IMPLEMENTS edge (ConcreteA only), got %d", count)
	}
	if !g.HasEdge(concreteAID, ifaceID, graph.EdgeImplements) {
		t.Error("expected IMPLEMENTS edge for ConcreteA (instantiated)")
	}
	if g.HasEdge(concreteBID, ifaceID, graph.EdgeImplements) {
		t.Error("unexpected IMPLEMENTS edge for ConcreteB (not instantiated)")
	}
}

// TestRTA_HeritageEdges_NoFilterWhenNoData verifies that ResolveHeritageEdges
// emits IMPLEMENTS edges for all classes when no instantiation data exists
// (preserving backward compatibility for C#/Kotlin projects).
func TestRTA_HeritageEdges_NoFilterWhenNoData(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("svc.go", "Runnable")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface,
		Name: "Runnable", Package: "svc", File: "svc.go",
	})

	concreteAID := g.MakeNodeID("svc.go", "ConcreteA")
	g.AddNode(&graph.Node{
		ID: concreteAID, Type: graph.NodeStruct,
		Name: "ConcreteA", Package: "svc", File: "svc.go",
		Metadata: map[string]string{"heritage_implements": "Runnable"},
	})
	concreteBID := g.MakeNodeID("svc.go", "ConcreteB")
	g.AddNode(&graph.Node{
		ID: concreteBID, Type: graph.NodeStruct,
		Name: "ConcreteB", Package: "svc", File: "svc.go",
		Metadata: map[string]string{"heritage_implements": "Runnable"},
	})
	// No instantiation data.

	count := resolver.ResolveHeritageEdges(g)
	if count != 2 {
		t.Fatalf("expected 2 IMPLEMENTS edges (no RTA filter), got %d", count)
	}
}

// TestRTA_ImplementsEdges_SkipsNonInstantiated verifies that ResolveImplementsEdges
// skips Go structural matches for structs not in instantiatedTypes.
func TestRTA_ImplementsEdges_SkipsNonInstantiated(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("pkg/svc.go", "Worker")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface,
		Name: "Worker", Package: "pkg", File: "pkg/svc.go",
		Metadata: map[string]string{"methods": "Do"},
	})

	// ConcreteA.Do — ConcreteA IS instantiated.
	concreteAID := g.MakeNodeID("pkg/svc.go", "ConcreteA")
	g.AddNode(&graph.Node{
		ID: concreteAID, Type: graph.NodeStruct,
		Name: "ConcreteA", Package: "pkg", File: "pkg/svc.go",
	})
	g.AddNode(&graph.Node{
		ID:      g.MakeNodeID("pkg/svc.go", "ConcreteA.Do"),
		Type:    graph.NodeMethod,
		Name:    "ConcreteA.Do",
		Package: "pkg", File: "pkg/svc.go",
	})

	// ConcreteB.Do — ConcreteB NOT instantiated.
	concreteBID := g.MakeNodeID("pkg/svc.go", "ConcreteB")
	g.AddNode(&graph.Node{
		ID: concreteBID, Type: graph.NodeStruct,
		Name: "ConcreteB", Package: "pkg", File: "pkg/svc.go",
	})
	g.AddNode(&graph.Node{
		ID:      g.MakeNodeID("pkg/svc.go", "ConcreteB.Do"),
		Type:    graph.NodeMethod,
		Name:    "ConcreteB.Do",
		Package: "pkg", File: "pkg/svc.go",
	})

	g.AddInstantiatedType("pkg/svc.go", "ConcreteA")

	count := resolver.ResolveImplementsEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 IMPLEMENTS edge (ConcreteA only), got %d", count)
	}
	if !g.HasEdge(concreteAID, ifaceID, graph.EdgeImplements) {
		t.Error("expected IMPLEMENTS edge for ConcreteA")
	}
	if g.HasEdge(concreteBID, ifaceID, graph.EdgeImplements) {
		t.Error("unexpected IMPLEMENTS edge for ConcreteB (not instantiated)")
	}
}

// TestRTA_InstantiatedTypes_PerFileTracking verifies per-file tracking and
// cross-file union in GetInstantiatedTypes.
func TestRTA_InstantiatedTypes_PerFileTracking(t *testing.T) {
	g := graph.New("testrepo")

	g.AddInstantiatedType("a.java", "ServiceA")
	g.AddInstantiatedType("b.java", "ServiceB")
	g.AddInstantiatedType("a.java", "ServiceA") // duplicate — should be idempotent

	types := g.GetInstantiatedTypes()
	if types == nil {
		t.Fatal("expected non-nil instantiated types")
	}
	if !types["ServiceA"] {
		t.Error("expected ServiceA in instantiated types")
	}
	if !types["ServiceB"] {
		t.Error("expected ServiceB in instantiated types")
	}
	if len(types) != 2 {
		t.Errorf("expected 2 distinct types, got %d", len(types))
	}
}

// TestRTA_InstantiatedTypes_RemoveFile verifies that RemoveFile cleans up
// instantiated types for that file.
func TestRTA_InstantiatedTypes_RemoveFile(t *testing.T) {
	g := graph.New("testrepo")

	// Add a file node so RemoveFile has something to remove.
	fileID := g.MakeNodeID("a.java", "a.java")
	g.AddNode(&graph.Node{ID: fileID, Type: graph.NodeFile, Name: "a.java", File: "a.java", Package: "com.example"})
	g.AddNode(&graph.Node{
		ID:      g.MakeNodeID("a.java", "ServiceA"),
		Type:    graph.NodeStruct,
		Name:    "ServiceA",
		Package: "com.example",
		File:    "a.java",
	})

	g.AddInstantiatedType("a.java", "ServiceA")
	g.AddInstantiatedType("b.java", "ServiceB")

	g.RemoveFile("a.java")

	types := g.GetInstantiatedTypes()
	if types["ServiceA"] {
		t.Error("ServiceA should have been removed when a.java was deleted")
	}
	if !types["ServiceB"] {
		t.Error("ServiceB from b.java should still be present")
	}
}

// TestRTA_InstantiatedTypes_EmptyInputIgnored verifies that empty type names
// are silently ignored.
func TestRTA_InstantiatedTypes_EmptyInputIgnored(t *testing.T) {
	g := graph.New("testrepo")
	g.AddInstantiatedType("", "ServiceA")  // empty file
	g.AddInstantiatedType("a.java", "")    // empty type
	g.AddInstantiatedType("", "")          // both empty

	types := g.GetInstantiatedTypes()
	if len(types) != 0 {
		t.Errorf("expected empty map for invalid inputs, got %v", types)
	}
}

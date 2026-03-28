package resolver_test

// Tests for RTA-style call graph refinement (Sprint 14 #2 and Sprint 14 #8).
//
// Key behaviors verified:
//   1. findInPackage emits edges to ALL instantiated receivers (true RTA multi-target).
//   2. Deterministic results: sorted pkgIndex means same graph always produces same edges.
//   3. CHA fallback: when no instantiated receiver exists, still emit to first match.
//   4. No-data fallback: pure Go projects (no instantiatedTypes) behave like old CHA.
//   5. ResolveImplementsEdges: skips structs not in instantiatedTypes (Go structural).
//   6. ResolveHeritageEdges: NO RTA filter — abstract base class chains stay intact.
//   7. AddInstantiatedType / GetInstantiatedTypes: basic graph operations.
//   8. RemoveFile cleans up instantiation data.

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// helpers ─────────────────────────────────────────────────────────────────────

func makePkg(g *graph.Graph, file, pkg string) graph.NodeID {
	fileID := g.MakeNodeID(file, file)
	pkgID := g.MakeNodeID(pkg, pkg)
	g.AddNode(&graph.Node{ID: fileID, Type: graph.NodeFile, Name: file, File: file, Package: pkg})
	g.AddNode(&graph.Node{ID: pkgID, Type: graph.NodePackage, Name: pkg})
	return pkgID
}

func addMethod(g *graph.Graph, file, pkg, name string) graph.NodeID {
	id := g.MakeNodeID(file, name)
	g.AddNode(&graph.Node{ID: id, Type: graph.NodeMethod, Name: name, Package: pkg, File: file})
	return id
}

func addFunction(g *graph.Graph, file, pkg, name string) graph.NodeID {
	id := g.MakeNodeID(file, name)
	g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: name, Package: pkg, File: file})
	return id
}

// TestRTA_MultiTarget_AllInstantiatedReceiversGetEdge verifies that when
// multiple classes in the same package have the same method and ALL are
// instantiated, CALLS edges are emitted to ALL of them — not just the first.
// This is the core RTA correctness requirement (Bacon & Sweeney OOPSLA 1996).
func TestRTA_MultiTarget_AllInstantiatedReceiversGetEdge(t *testing.T) {
	g := graph.New("testrepo")

	svcPkg := makePkg(g, "svc.go", "svc")

	// Three classes all with a "Process" method — all instantiated.
	aID := addMethod(g, "svc.go", "svc", "ConcreteA.Process")
	bID := addMethod(g, "svc.go", "svc", "ConcreteB.Process")
	cID := addMethod(g, "svc.go", "svc", "ConcreteC.Process")

	mainPkg := makePkg(g, "main.go", "main")
	_ = mainPkg
	callerID := addFunction(g, "main.go", "main", "main")
	g.AddEdge(&graph.Edge{From: g.MakeNodeID("main.go", "main.go"), To: svcPkg, Type: graph.EdgeImports})

	g.AddCallSite(graph.CallSite{
		CallerID: callerID, CallerFile: "main.go",
		PkgAlias: "svc", FuncName: "Process",
	})

	// All three are instantiated.
	g.AddInstantiatedType("main.go", "ConcreteA")
	g.AddInstantiatedType("main.go", "ConcreteB")
	g.AddInstantiatedType("main.go", "ConcreteC")

	count := resolver.ResolveCallEdges(g)
	if count != 3 {
		t.Fatalf("expected 3 CALLS edges (one per instantiated class), got %d", count)
	}
	for _, id := range []graph.NodeID{aID, bID, cID} {
		if !g.HasEdge(callerID, id, graph.EdgeCalls) {
			t.Errorf("missing CALLS edge to %s", id)
		}
	}
}

// TestRTA_MultiTarget_OnlyInstantiatedGetEdge verifies that when some classes
// are instantiated and some are not, only the instantiated ones get edges.
func TestRTA_MultiTarget_OnlyInstantiatedGetEdge(t *testing.T) {
	g := graph.New("testrepo")

	svcPkg := makePkg(g, "svc.go", "svc")

	aID := addMethod(g, "svc.go", "svc", "ConcreteA.Save")
	bID := addMethod(g, "svc.go", "svc", "ConcreteB.Save") // not instantiated
	cID := addMethod(g, "svc.go", "svc", "ConcreteC.Save")

	callerID := addFunction(g, "main.go", "main", "main")
	makePkg(g, "main.go", "main")
	g.AddEdge(&graph.Edge{From: g.MakeNodeID("main.go", "main.go"), To: svcPkg, Type: graph.EdgeImports})

	g.AddCallSite(graph.CallSite{
		CallerID: callerID, CallerFile: "main.go",
		PkgAlias: "svc", FuncName: "Save",
	})

	g.AddInstantiatedType("main.go", "ConcreteA")
	// ConcreteB NOT instantiated.
	g.AddInstantiatedType("main.go", "ConcreteC")

	count := resolver.ResolveCallEdges(g)
	if count != 2 {
		t.Fatalf("expected 2 CALLS edges (ConcreteA + ConcreteC), got %d", count)
	}
	if !g.HasEdge(callerID, aID, graph.EdgeCalls) {
		t.Error("missing CALLS edge to ConcreteA.Save")
	}
	if g.HasEdge(callerID, bID, graph.EdgeCalls) {
		t.Error("unexpected CALLS edge to ConcreteB.Save (not instantiated)")
	}
	if !g.HasEdge(callerID, cID, graph.EdgeCalls) {
		t.Error("missing CALLS edge to ConcreteC.Save")
	}
}

// TestRTA_FindInPackage_FallsBackWhenNoneInstantiated verifies that when no
// candidate's receiver type is instantiated, a single CHA fallback edge is
// still emitted (no lost edges).
func TestRTA_FindInPackage_FallsBackWhenNoneInstantiated(t *testing.T) {
	g := graph.New("testrepo")

	svcPkg := makePkg(g, "svc.go", "svc")
	addMethod(g, "svc.go", "svc", "ConcreteA.Run")
	addMethod(g, "svc.go", "svc", "ConcreteB.Run")

	callerID := addFunction(g, "main.go", "main", "main")
	makePkg(g, "main.go", "main")
	g.AddEdge(&graph.Edge{From: g.MakeNodeID("main.go", "main.go"), To: svcPkg, Type: graph.EdgeImports})

	g.AddCallSite(graph.CallSite{
		CallerID: callerID, CallerFile: "main.go",
		PkgAlias: "svc", FuncName: "Run",
	})

	// Some data exists, but NOT for ConcreteA or ConcreteB.
	g.AddInstantiatedType("main.go", "OtherClass")

	count := resolver.ResolveCallEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 CHA fallback edge, got %d", count)
	}
}

// TestRTA_FindInPackage_NoInstantiationData verifies that when no instantiation
// data exists (pure Go project), exactly one CHA edge is emitted — identical to
// pre-RTA behavior.
func TestRTA_FindInPackage_NoInstantiationData(t *testing.T) {
	g := graph.New("testrepo")

	svcPkg := makePkg(g, "svc.go", "svc")
	targetID := addMethod(g, "svc.go", "svc", "Handler.Run")

	callerID := addFunction(g, "main.go", "main", "main")
	makePkg(g, "main.go", "main")
	g.AddEdge(&graph.Edge{From: g.MakeNodeID("main.go", "main.go"), To: svcPkg, Type: graph.EdgeImports})

	g.AddCallSite(graph.CallSite{
		CallerID: callerID, CallerFile: "main.go",
		PkgAlias: "svc", FuncName: "Run",
	})
	// No AddInstantiatedType calls — GetInstantiatedTypes returns nil.

	count := resolver.ResolveCallEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 CHA edge, got %d", count)
	}
	if !g.HasEdge(callerID, targetID, graph.EdgeCalls) {
		t.Error("expected CALLS edge to Handler.Run")
	}
}

// TestRTA_Determinism verifies that the same graph always produces the same
// CALLS edges regardless of run order (sorted pkgIndex).
func TestRTA_Determinism(t *testing.T) {
	// Build the same graph twice and verify identical edge targets.
	build := func() (graph.NodeID, graph.NodeID) {
		g := graph.New("testrepo")
		svcPkg := makePkg(g, "svc.go", "svc")
		alpha := addMethod(g, "svc.go", "svc", "AlphaService.Execute") // lexicographically first
		addMethod(g, "svc.go", "svc", "ZetaService.Execute")

		callerID := addFunction(g, "main.go", "main", "run")
		makePkg(g, "main.go", "main")
		g.AddEdge(&graph.Edge{From: g.MakeNodeID("main.go", "main.go"), To: svcPkg, Type: graph.EdgeImports})
		g.AddCallSite(graph.CallSite{
			CallerID: callerID, CallerFile: "main.go",
			PkgAlias: "svc", FuncName: "Execute",
		})

		// Only AlphaService is instantiated — should always resolve to alpha.
		g.AddInstantiatedType("main.go", "AlphaService")
		resolver.ResolveCallEdges(g)
		return callerID, alpha
	}

	callerID, alphaID := build()
	// Run 50 times — non-determinism would show up quickly.
	for i := 0; i < 50; i++ {
		g2 := graph.New("testrepo")
		svcPkg2 := makePkg(g2, "svc.go", "svc")
		a2 := addMethod(g2, "svc.go", "svc", "AlphaService.Execute")
		_ = addMethod(g2, "svc.go", "svc", "ZetaService.Execute")
		caller2 := addFunction(g2, "main.go", "main", "run")
		makePkg(g2, "main.go", "main")
		g2.AddEdge(&graph.Edge{From: g2.MakeNodeID("main.go", "main.go"), To: svcPkg2, Type: graph.EdgeImports})
		g2.AddCallSite(graph.CallSite{
			CallerID: caller2, CallerFile: "main.go",
			PkgAlias: "svc", FuncName: "Execute",
		})
		g2.AddInstantiatedType("main.go", "AlphaService")
		resolver.ResolveCallEdges(g2)

		if !g2.HasEdge(caller2, a2, graph.EdgeCalls) {
			t.Fatalf("run %d: expected deterministic edge to AlphaService.Execute", i)
		}
		_ = callerID
		_ = alphaID
	}
}

// TestRTA_HeritageEdges_AbstractBaseClassPreserved verifies that abstract base
// classes (never directly instantiated) still get their IMPLEMENTS edges.
// This is the critical correctness case: AbstractBase → Interface must survive
// even though AbstractBase is not in instantiatedTypes.
func TestRTA_HeritageEdges_AbstractBaseClassPreserved(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("svc.go", "Auditable")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface,
		Name: "Auditable", Package: "svc", File: "svc.go",
	})

	// AbstractBase implements Auditable — never directly instantiated.
	abstractID := g.MakeNodeID("svc.go", "AbstractBase")
	g.AddNode(&graph.Node{
		ID: abstractID, Type: graph.NodeStruct,
		Name: "AbstractBase", Package: "svc", File: "svc.go",
		Metadata: map[string]string{"heritage_implements": "Auditable"},
	})

	// ConcreteImpl extends AbstractBase — IS instantiated.
	concreteID := g.MakeNodeID("svc.go", "ConcreteImpl")
	g.AddNode(&graph.Node{
		ID: concreteID, Type: graph.NodeStruct,
		Name: "ConcreteImpl", Package: "svc", File: "svc.go",
		Metadata: map[string]string{"heritage_extends": "AbstractBase"},
	})

	// Only ConcreteImpl is instantiated.
	g.AddInstantiatedType("svc.go", "ConcreteImpl")

	count := resolver.ResolveHeritageEdges(g)
	// Both AbstractBase→Auditable AND ConcreteImpl→AbstractBase must be emitted.
	if count != 2 {
		t.Fatalf("expected 2 IMPLEMENTS edges (both abstract and concrete), got %d", count)
	}
	if !g.HasEdge(abstractID, ifaceID, graph.EdgeImplements) {
		t.Error("missing AbstractBase→Auditable edge — abstract base class chain broken")
	}
	if !g.HasEdge(concreteID, abstractID, graph.EdgeImplements) {
		t.Error("missing ConcreteImpl→AbstractBase edge")
	}
}

// TestRTA_HeritageEdges_EmitsAllRegardlessOfInstantiation verifies that
// ResolveHeritageEdges emits edges for ALL classes — instantiated or not —
// because nominal type declarations are always structurally correct.
func TestRTA_HeritageEdges_EmitsAllRegardlessOfInstantiation(t *testing.T) {
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

	// Only ConcreteA is instantiated — but BOTH must get IMPLEMENTS edges.
	g.AddInstantiatedType("svc.go", "ConcreteA")

	count := resolver.ResolveHeritageEdges(g)
	if count != 2 {
		t.Fatalf("expected 2 IMPLEMENTS edges (all heritage, no RTA filter), got %d", count)
	}
	if !g.HasEdge(concreteAID, ifaceID, graph.EdgeImplements) {
		t.Error("missing ConcreteA→Runnable")
	}
	if !g.HasEdge(concreteBID, ifaceID, graph.EdgeImplements) {
		t.Error("missing ConcreteB→Runnable — RTA filter must NOT apply to heritage edges")
	}
}

// TestRTA_ImplementsEdges_SkipsNonInstantiated verifies that the Go structural
// heuristic (ResolveImplementsEdges) skips structs not in instantiatedTypes.
// This is where RTA filtering IS appropriate — structural matching can over-match.
func TestRTA_ImplementsEdges_SkipsNonInstantiated(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("pkg/svc.go", "Worker")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface,
		Name: "Worker", Package: "pkg", File: "pkg/svc.go",
		Metadata: map[string]string{"methods": "Do"},
	})

	concreteAID := g.MakeNodeID("pkg/svc.go", "ConcreteA")
	g.AddNode(&graph.Node{ID: concreteAID, Type: graph.NodeStruct, Name: "ConcreteA", Package: "pkg", File: "pkg/svc.go"})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("pkg/svc.go", "ConcreteA.Do"), Type: graph.NodeMethod,
		Name: "ConcreteA.Do", Package: "pkg", File: "pkg/svc.go",
	})

	concreteBID := g.MakeNodeID("pkg/svc.go", "ConcreteB")
	g.AddNode(&graph.Node{ID: concreteBID, Type: graph.NodeStruct, Name: "ConcreteB", Package: "pkg", File: "pkg/svc.go"})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("pkg/svc.go", "ConcreteB.Do"), Type: graph.NodeMethod,
		Name: "ConcreteB.Do", Package: "pkg", File: "pkg/svc.go",
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

// TestRTA_ImplementsEdges_NoFilterWhenNoData verifies that ResolveImplementsEdges
// emits all Go structural matches when no instantiation data exists.
func TestRTA_ImplementsEdges_NoFilterWhenNoData(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("pkg/svc.go", "Worker")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface,
		Name: "Worker", Package: "pkg", File: "pkg/svc.go",
		Metadata: map[string]string{"methods": "Do"},
	})

	for _, name := range []string{"ConcreteA", "ConcreteB"} {
		sID := g.MakeNodeID("pkg/svc.go", name)
		g.AddNode(&graph.Node{ID: sID, Type: graph.NodeStruct, Name: name, Package: "pkg", File: "pkg/svc.go"})
		g.AddNode(&graph.Node{
			ID: g.MakeNodeID("pkg/svc.go", name+".Do"), Type: graph.NodeMethod,
			Name: name + ".Do", Package: "pkg", File: "pkg/svc.go",
		})
	}
	// No instantiation data → no filter.

	count := resolver.ResolveImplementsEdges(g)
	if count != 2 {
		t.Fatalf("expected 2 IMPLEMENTS edges (no RTA filter), got %d", count)
	}
}

// TestRTA_InstantiatedTypes_PerFileTracking verifies per-file tracking and
// cross-file union in GetInstantiatedTypes.
func TestRTA_InstantiatedTypes_PerFileTracking(t *testing.T) {
	g := graph.New("testrepo")

	g.AddInstantiatedType("a.java", "ServiceA")
	g.AddInstantiatedType("b.java", "ServiceB")
	g.AddInstantiatedType("a.java", "ServiceA") // duplicate — idempotent

	types := g.GetInstantiatedTypes()
	if types == nil {
		t.Fatal("expected non-nil instantiated types")
	}
	if !types["ServiceA"] || !types["ServiceB"] {
		t.Errorf("missing expected types: %v", types)
	}
	if len(types) != 2 {
		t.Errorf("expected 2 distinct types, got %d", len(types))
	}
}

// TestRTA_InstantiatedTypes_RemoveFile verifies that RemoveFile cleans up
// instantiated types for that file.
func TestRTA_InstantiatedTypes_RemoveFile(t *testing.T) {
	g := graph.New("testrepo")

	fileID := g.MakeNodeID("a.java", "a.java")
	g.AddNode(&graph.Node{ID: fileID, Type: graph.NodeFile, Name: "a.java", File: "a.java", Package: "com.example"})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("a.java", "ServiceA"), Type: graph.NodeStruct,
		Name: "ServiceA", Package: "com.example", File: "a.java",
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
// and empty file paths are silently ignored.
func TestRTA_InstantiatedTypes_EmptyInputIgnored(t *testing.T) {
	g := graph.New("testrepo")
	g.AddInstantiatedType("", "ServiceA")
	g.AddInstantiatedType("a.java", "")
	g.AddInstantiatedType("", "")

	types := g.GetInstantiatedTypes()
	if len(types) != 0 {
		t.Errorf("expected empty map for invalid inputs, got %v", types)
	}
}

// TestRTA_SpringServiceGetsImplementsEdge is the primary regression test for
// Sprint 14 #8 (DI annotation tracking). A @Repository-annotated class that is
// NEVER instantiated via new should still appear in instantiatedTypes after
// parsing, and ResolveHeritageEdges must emit the IMPLEMENTS edge (Java uses
// nominal heritage resolution, not the structural heuristic).
func TestRTA_SpringServiceGetsImplementsEdge(t *testing.T) {
	const src = `
public interface UserRepository {
    void save(Object user);
}

@Repository
public class JpaUserRepository implements UserRepository {
    public void save(Object user) {}
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "repo.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	// JpaUserRepository must be in instantiatedTypes via @Repository annotation.
	types := g.GetInstantiatedTypes()
	if !types["JpaUserRepository"] {
		t.Fatalf("JpaUserRepository not in instantiatedTypes — @Repository annotation not tracked")
	}

	// Java uses nominal heritage resolution.
	resolver.ResolveHeritageEdges(g)

	// Find the JpaUserRepository and UserRepository nodes.
	var implID, ifaceID graph.NodeID
	for _, n := range g.AllNodes() {
		switch n.Name {
		case "JpaUserRepository":
			implID = n.ID
		case "UserRepository":
			ifaceID = n.ID
		}
	}
	if implID == "" {
		t.Fatal("JpaUserRepository node not found in graph")
	}
	if ifaceID == "" {
		t.Fatal("UserRepository node not found in graph")
	}
	if !g.HasEdge(implID, ifaceID, graph.EdgeImplements) {
		t.Errorf("expected IMPLEMENTS edge from JpaUserRepository to UserRepository")
	}
}

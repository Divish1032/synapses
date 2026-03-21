package resolver_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

func TestResolveHeritageEdges_BasicImplements(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("svc.ts", "IService")
	classID := g.MakeNodeID("svc.ts", "MyService")

	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "IService",
		Package: "svc", File: "svc.ts",
	})
	g.AddNode(&graph.Node{
		ID: classID, Type: graph.NodeStruct, Name: "MyService",
		Package: "svc", File: "svc.ts",
		Metadata: map[string]string{"heritage_implements": "IService"},
	})

	count := resolver.ResolveHeritageEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 heritage IMPLEMENTS edge, got %d", count)
	}

	found := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeImplements && e.From == classID && e.To == ifaceID {
			found = true
		}
	}
	if !found {
		t.Error("expected IMPLEMENTS edge MyService → IService")
	}
}

func TestResolveHeritageEdges_MultipleInterfaces(t *testing.T) {
	g := graph.New("test")

	id1 := g.MakeNodeID("a.ts", "Readable")
	id2 := g.MakeNodeID("a.ts", "Writable")
	classID := g.MakeNodeID("a.ts", "Stream")

	g.AddNode(&graph.Node{ID: id1, Type: graph.NodeInterface, Name: "Readable", Package: "a", File: "a.ts"})
	g.AddNode(&graph.Node{ID: id2, Type: graph.NodeInterface, Name: "Writable", Package: "a", File: "a.ts"})
	g.AddNode(&graph.Node{
		ID: classID, Type: graph.NodeStruct, Name: "Stream", Package: "a", File: "a.ts",
		Metadata: map[string]string{"heritage_implements": "Readable,Writable"},
	})

	count := resolver.ResolveHeritageEdges(g)
	if count != 2 {
		t.Fatalf("expected 2 heritage IMPLEMENTS edges, got %d", count)
	}
}

func TestResolveHeritageEdges_Extends(t *testing.T) {
	g := graph.New("test")

	baseID := g.MakeNodeID("base.ts", "Base")
	childID := g.MakeNodeID("child.ts", "Child")

	g.AddNode(&graph.Node{ID: baseID, Type: graph.NodeStruct, Name: "Base", Package: "app", File: "base.ts"})
	g.AddNode(&graph.Node{
		ID: childID, Type: graph.NodeStruct, Name: "Child", Package: "app", File: "child.ts",
		Metadata: map[string]string{"heritage_extends": "Base"},
	})

	count := resolver.ResolveHeritageEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 heritage IMPLEMENTS edge for extends, got %d", count)
	}

	found := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeImplements && e.From == childID && e.To == baseID {
			found = true
		}
	}
	if !found {
		t.Error("expected IMPLEMENTS edge Child → Base")
	}
}

func TestResolveHeritageEdges_ExtendsAndImplements(t *testing.T) {
	g := graph.New("test")

	baseID := g.MakeNodeID("a.java", "AbstractService")
	ifaceID := g.MakeNodeID("a.java", "Serializable")
	classID := g.MakeNodeID("a.java", "UserService")

	g.AddNode(&graph.Node{ID: baseID, Type: graph.NodeStruct, Name: "AbstractService", Package: "com.app", File: "a.java"})
	g.AddNode(&graph.Node{ID: ifaceID, Type: graph.NodeInterface, Name: "Serializable", Package: "com.app", File: "a.java"})
	g.AddNode(&graph.Node{
		ID: classID, Type: graph.NodeStruct, Name: "UserService", Package: "com.app", File: "a.java",
		Metadata: map[string]string{
			"heritage_extends":    "AbstractService",
			"heritage_implements": "Serializable",
		},
	})

	count := resolver.ResolveHeritageEdges(g)
	if count != 2 {
		t.Fatalf("expected 2 heritage edges (extends + implements), got %d", count)
	}
}

func TestResolveHeritageEdges_NoSelfImplement(t *testing.T) {
	g := graph.New("test")

	classID := g.MakeNodeID("a.ts", "Foo")
	g.AddNode(&graph.Node{
		ID: classID, Type: graph.NodeStruct, Name: "Foo", Package: "p", File: "a.ts",
		Metadata: map[string]string{"heritage_implements": "Foo"},
	})

	count := resolver.ResolveHeritageEdges(g)
	if count != 0 {
		t.Errorf("expected 0 edges (no self-implement), got %d", count)
	}
}

func TestResolveHeritageEdges_CrossPackageMatch(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("api/handler.java", "Handler")
	classID := g.MakeNodeID("impl/auth.java", "AuthHandler")

	g.AddNode(&graph.Node{ID: ifaceID, Type: graph.NodeInterface, Name: "Handler", Package: "api", File: "api/handler.java"})
	g.AddNode(&graph.Node{
		ID: classID, Type: graph.NodeStruct, Name: "AuthHandler", Package: "impl", File: "impl/auth.java",
		Metadata: map[string]string{"heritage_implements": "Handler"},
	})

	count := resolver.ResolveHeritageEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 heritage edge (cross-package), got %d", count)
	}
}

func TestResolveHeritageEdges_PrefersPackageMatch(t *testing.T) {
	g := graph.New("test")

	// Two interfaces named "Service" in different packages.
	samePkgID := g.MakeNodeID("app/svc.java", "Service")
	otherPkgID := g.MakeNodeID("lib/svc.java", "Service")
	classID := g.MakeNodeID("app/impl.java", "MyService")

	g.AddNode(&graph.Node{ID: samePkgID, Type: graph.NodeInterface, Name: "Service", Package: "app", File: "app/svc.java"})
	g.AddNode(&graph.Node{ID: otherPkgID, Type: graph.NodeInterface, Name: "Service", Package: "lib", File: "lib/svc.java"})
	g.AddNode(&graph.Node{
		ID: classID, Type: graph.NodeStruct, Name: "MyService", Package: "app", File: "app/impl.java",
		Metadata: map[string]string{"heritage_implements": "Service"},
	})

	count := resolver.ResolveHeritageEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 heritage edge, got %d", count)
	}

	// Should prefer same-package match.
	found := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeImplements && e.From == classID && e.To == samePkgID {
			found = true
		}
	}
	if !found {
		t.Error("expected IMPLEMENTS edge to same-package Service, not cross-package")
	}
}

func TestResolveHeritageEdges_NoDuplicates(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("a.ts", "IFoo")
	classID := g.MakeNodeID("a.ts", "Foo")

	g.AddNode(&graph.Node{ID: ifaceID, Type: graph.NodeInterface, Name: "IFoo", Package: "p", File: "a.ts"})
	g.AddNode(&graph.Node{
		ID: classID, Type: graph.NodeStruct, Name: "Foo", Package: "p", File: "a.ts",
		Metadata: map[string]string{"heritage_implements": "IFoo"},
	})

	resolver.ResolveHeritageEdges(g)
	count := resolver.ResolveHeritageEdges(g)
	if count != 0 {
		t.Errorf("expected 0 on second call (dedup), got %d", count)
	}
}

func TestResolveHeritageEdges_UnknownTargetSkipped(t *testing.T) {
	g := graph.New("test")

	classID := g.MakeNodeID("a.ts", "Foo")
	g.AddNode(&graph.Node{
		ID: classID, Type: graph.NodeStruct, Name: "Foo", Package: "p", File: "a.ts",
		Metadata: map[string]string{"heritage_implements": "NonExistent"},
	})

	count := resolver.ResolveHeritageEdges(g)
	if count != 0 {
		t.Errorf("expected 0 for unknown target, got %d", count)
	}
}

func TestStructuralImplements_SkipsHeritageTaggedNodes(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("svc.ts", "Doer")
	classID := g.MakeNodeID("svc.ts", "MyClass")
	doID := g.MakeNodeID("svc.ts", "MyClass.Do")

	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "Doer", Package: "svc", File: "svc.ts",
		Metadata: map[string]string{"methods": "Do"},
	})
	// This struct has heritage_implements → should be skipped by structural resolver.
	g.AddNode(&graph.Node{
		ID: classID, Type: graph.NodeStruct, Name: "MyClass", Package: "svc", File: "svc.ts",
		Metadata: map[string]string{"heritage_implements": "SomethingElse"},
	})
	g.AddNode(&graph.Node{ID: doID, Type: graph.NodeMethod, Name: "MyClass.Do", Package: "svc", File: "svc.ts"})

	// Structural resolver should NOT match MyClass→Doer because MyClass has heritage metadata.
	count := resolver.ResolveImplementsEdges(g)
	if count != 0 {
		t.Errorf("expected 0 structural IMPLEMENTS edges for heritage-tagged node, got %d", count)
	}
}

func TestStructuralImplements_StillWorksForGoNodes(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("svc.go", "Writer")
	structID := g.MakeNodeID("impl.go", "FileWriter")
	writeID := g.MakeNodeID("impl.go", "FileWriter.Write")

	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "Writer", Package: "io", File: "svc.go",
		Metadata: map[string]string{"methods": "Write"},
	})
	// No heritage metadata → structural heuristic should still work.
	g.AddNode(&graph.Node{ID: structID, Type: graph.NodeStruct, Name: "FileWriter", Package: "io", File: "impl.go"})
	g.AddNode(&graph.Node{ID: writeID, Type: graph.NodeMethod, Name: "FileWriter.Write", Package: "io", File: "impl.go"})

	count := resolver.ResolveImplementsEdges(g)
	if count != 1 {
		t.Errorf("expected 1 structural IMPLEMENTS edge for Go-style node, got %d", count)
	}
}

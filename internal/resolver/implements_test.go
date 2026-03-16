package resolver_test

// Additional tests for ResolveImplementsEdges targeting uncovered branches:
// - Interface with empty methods string
// - Interface with nil metadata
// - Method name without dot (skipped)
// - Struct node missing for matching methods
// - Multiple structs implementing same interface
// - Methods list with whitespace
// - Empty graph (no nodes at all)
// - Only function nodes (no interfaces)

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

func TestImplements_InterfaceEmptyMethodsStr(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("pkg/svc.go", "Empty")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "Empty",
		Package: "pkg", File: "pkg/svc.go",
		Metadata: map[string]string{"methods": ""},
	})

	count := resolver.ResolveImplementsEdges(g)
	if count != 0 {
		t.Errorf("expected 0 for interface with empty methods, got %d", count)
	}
}

func TestImplements_InterfaceNilMetadata(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("pkg/svc.go", "NoMeta")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "NoMeta",
		Package: "pkg", File: "pkg/svc.go",
		// No Metadata at all — methods key will be "".
	})

	count := resolver.ResolveImplementsEdges(g)
	if count != 0 {
		t.Errorf("expected 0 for interface with nil metadata, got %d", count)
	}
}

func TestImplements_MethodWithoutDot_Skipped(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("pkg/svc.go", "Svc")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "Svc",
		Package: "pkg", File: "pkg/svc.go",
		Metadata: map[string]string{"methods": "Do"},
	})

	// Method node without dot in name — should be skipped when building structMethods.
	mID := g.MakeNodeID("pkg/svc.go", "NoDotMethod")
	g.AddNode(&graph.Node{
		ID: mID, Type: graph.NodeMethod, Name: "NoDotMethod",
		Package: "pkg", File: "pkg/svc.go",
	})

	count := resolver.ResolveImplementsEdges(g)
	if count != 0 {
		t.Errorf("expected 0 for method without dot, got %d", count)
	}
}

func TestImplements_StructNodeMissing_NoEdge(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("pkg/svc.go", "Svc")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "Svc",
		Package: "pkg", File: "pkg/svc.go",
		Metadata: map[string]string{"methods": "Do"},
	})

	// Method with correct "Ghost.Do" name but no struct node for "Ghost".
	mID := g.MakeNodeID("pkg/svc.go", "Ghost.Do")
	g.AddNode(&graph.Node{
		ID: mID, Type: graph.NodeMethod, Name: "Ghost.Do",
		Package: "pkg", File: "pkg/svc.go",
	})

	count := resolver.ResolveImplementsEdges(g)
	if count != 0 {
		t.Errorf("expected 0 (struct node missing), got %d", count)
	}
}

func TestImplements_MultipleStructs_SameInterface(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("pkg/svc.go", "Reader")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "Reader",
		Package: "pkg", File: "pkg/svc.go",
		Metadata: map[string]string{"methods": "Read"},
	})

	// Two structs that both implement Reader.
	for _, name := range []string{"FileReader", "NetReader"} {
		sid := g.MakeNodeID("pkg/svc.go", name)
		g.AddNode(&graph.Node{
			ID: sid, Type: graph.NodeStruct, Name: name,
			Package: "pkg", File: "pkg/svc.go",
		})
		mid := g.MakeNodeID("pkg/svc.go", name+".Read")
		g.AddNode(&graph.Node{
			ID: mid, Type: graph.NodeMethod, Name: name + ".Read",
			Package: "pkg", File: "pkg/svc.go",
		})
	}

	count := resolver.ResolveImplementsEdges(g)
	if count != 2 {
		t.Errorf("expected 2 IMPLEMENTS edges, got %d", count)
	}
}

func TestImplements_MethodsWithWhitespace(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("pkg/svc.go", "Svc")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "Svc",
		Package: "pkg", File: "pkg/svc.go",
		Metadata: map[string]string{"methods": " Start , Stop "},
	})

	structID := g.MakeNodeID("pkg/svc.go", "Impl")
	g.AddNode(&graph.Node{
		ID: structID, Type: graph.NodeStruct, Name: "Impl",
		Package: "pkg", File: "pkg/svc.go",
	})

	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("pkg/svc.go", "Impl.Start"), Type: graph.NodeMethod,
		Name: "Impl.Start", Package: "pkg", File: "pkg/svc.go",
	})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("pkg/svc.go", "Impl.Stop"), Type: graph.NodeMethod,
		Name: "Impl.Stop", Package: "pkg", File: "pkg/svc.go",
	})

	count := resolver.ResolveImplementsEdges(g)
	if count != 1 {
		t.Errorf("expected 1 IMPLEMENTS edge with trimmed methods, got %d", count)
	}
}

func TestImplements_EmptyGraph(t *testing.T) {
	g := graph.New("test")
	count := resolver.ResolveImplementsEdges(g)
	if count != 0 {
		t.Errorf("expected 0 for empty graph, got %d", count)
	}
}

func TestImplements_OnlyFunctions_NoInterfaces(t *testing.T) {
	g := graph.New("test")

	id := g.MakeNodeID("pkg/svc.go", "Serve")
	g.AddNode(&graph.Node{
		ID: id, Type: graph.NodeFunction, Name: "Serve",
		Package: "pkg", File: "pkg/svc.go",
	})

	count := resolver.ResolveImplementsEdges(g)
	if count != 0 {
		t.Errorf("expected 0 with no interfaces, got %d", count)
	}
}

func TestImplements_InterfaceMethodsOnlyWhitespace(t *testing.T) {
	g := graph.New("test")

	// methods = "  , , " — after trimming, all empty strings.
	ifaceID := g.MakeNodeID("pkg/svc.go", "Blanks")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "Blanks",
		Package: "pkg", File: "pkg/svc.go",
		Metadata: map[string]string{"methods": "  , , "},
	})

	count := resolver.ResolveImplementsEdges(g)
	if count != 0 {
		t.Errorf("expected 0 for whitespace-only methods, got %d", count)
	}
}

func TestImplements_PartialMatch_ThreeMethods_OnlyTwo(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("pkg/svc.go", "FullSvc")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "FullSvc",
		Package: "pkg", File: "pkg/svc.go",
		Metadata: map[string]string{"methods": "Start,Stop,Restart"},
	})

	structID := g.MakeNodeID("pkg/svc.go", "PartialImpl")
	g.AddNode(&graph.Node{
		ID: structID, Type: graph.NodeStruct, Name: "PartialImpl",
		Package: "pkg", File: "pkg/svc.go",
	})

	// Only Start and Stop — missing Restart.
	for _, m := range []string{"Start", "Stop"} {
		mid := g.MakeNodeID("pkg/svc.go", "PartialImpl."+m)
		g.AddNode(&graph.Node{
			ID: mid, Type: graph.NodeMethod, Name: "PartialImpl." + m,
			Package: "pkg", File: "pkg/svc.go",
		})
	}

	count := resolver.ResolveImplementsEdges(g)
	if count != 0 {
		t.Errorf("expected 0 for partial implementation (2/3 methods), got %d", count)
	}
}

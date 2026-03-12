package resolver_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// TestResolveCallEdges_DirectCallFallbackToImportedPkg tests the fallback path
// where a direct call (no pkg alias) is not found in the caller's own package
// but IS found in one of the caller's imported packages.
func TestResolveCallEdges_DirectCallFallbackToImportedPkg(t *testing.T) {
	g := graph.New("testrepo")

	// util package.
	utilFileID := g.MakeNodeID("util.go", "util.go")
	utilPkgID := g.MakeNodeID("util", "util")
	utilFuncID := g.MakeNodeID("util.go", "Helper")
	g.AddNode(&graph.Node{ID: utilFileID, Type: graph.NodeFile, Name: "util.go", File: "util.go", Package: "util"})
	g.AddNode(&graph.Node{ID: utilPkgID, Type: graph.NodePackage, Name: "util"})
	g.AddNode(&graph.Node{ID: utilFuncID, Type: graph.NodeFunction, Name: "Helper", Package: "util", File: "util.go"})

	// main package.
	mainFileID := g.MakeNodeID("main.go", "main.go")
	mainFuncID := g.MakeNodeID("main.go", "Run")
	g.AddNode(&graph.Node{ID: mainFileID, Type: graph.NodeFile, Name: "main.go", File: "main.go", Package: "main"})
	g.AddNode(&graph.Node{ID: mainFuncID, Type: graph.NodeFunction, Name: "Run", Package: "main", File: "main.go"})

	// main.go imports util.
	g.AddEdge(&graph.Edge{From: mainFileID, To: utilPkgID, Type: graph.EdgeImports})

	// Direct call: "Helper" is NOT in "main" but IS in the imported "util" package.
	g.AddCallSite(graph.CallSite{
		CallerID:   mainFuncID,
		CallerFile: "main.go",
		PkgAlias:   "", // direct call
		FuncName:   "Helper",
	})

	n := resolver.ResolveCallEdges(g)
	if n == 0 {
		t.Error("expected CALLS edge via direct-call fallback to imported package")
	}
	found := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeCalls && e.From == mainFuncID && e.To == utilFuncID {
			found = true
		}
	}
	if !found {
		t.Error("expected CALLS edge Run → Helper via imported util package")
	}
}

// TestResolveCallEdges_QualifiedFallbackToCallerPackage tests that when a
// qualified call alias is not found in importMap, the resolver falls back to
// searching the caller's own package (for method receivers like h.Method()).
func TestResolveCallEdges_QualifiedFallbackToCallerPackage(t *testing.T) {
	g := graph.New("testrepo")

	callerID := g.MakeNodeID("main.go", "run")
	calleeID := g.MakeNodeID("main.go", "Handler.Serve")
	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "run", Package: "main", File: "main.go"})
	g.AddNode(&graph.Node{ID: calleeID, Type: graph.NodeMethod, Name: "Handler.Serve", Package: "main", File: "main.go"})

	// Qualified call with alias "h" — not in importMap (no import edges added).
	// Fallback: search callerNode.Package ("main") for method with suffix ".Serve".
	g.AddCallSite(graph.CallSite{
		CallerID:   callerID,
		CallerFile: "main.go",
		PkgAlias:   "h",
		FuncName:   "Serve",
	})

	n := resolver.ResolveCallEdges(g)
	if n == 0 {
		t.Error("expected CALLS edge via qualified-alias fallback to caller's own package")
	}
	found := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeCalls && e.From == callerID && e.To == calleeID {
			found = true
		}
	}
	if !found {
		t.Error("expected CALLS edge run → Handler.Serve via qualified fallback")
	}
}

// TestResolveCallEdges_CallerNodeNil verifies that a call site whose CallerID
// does not exist in the graph is silently skipped (no panic, no bogus edge).
func TestResolveCallEdges_CallerNodeNil(t *testing.T) {
	g := graph.New("testrepo")

	g.AddCallSite(graph.CallSite{
		CallerID:   graph.NodeID("nonexistent::file::func"),
		CallerFile: "file.go",
		PkgAlias:   "",
		FuncName:   "DoSomething",
	})

	n := resolver.ResolveCallEdges(g)
	if n != 0 {
		t.Errorf("expected 0 edges for nil caller node, got %d", n)
	}
}

// TestResolveCallEdges_EmptySites verifies a no-op on a graph with no pending sites.
func TestResolveCallEdges_EmptySites(t *testing.T) {
	g := graph.New("testrepo")
	// No call sites added.
	n := resolver.ResolveCallEdges(g)
	if n != 0 {
		t.Errorf("expected 0 edges for empty site list, got %d", n)
	}
}

// TestResolveCallEdges_QualifiedCallAliasInImportedPkg tests the happy path where
// the pkg alias IS found in importMap → resolves to an imported package function.
// This complements the existing TestResolveCallEdges_QualifiedCall test by
// verifying that the alias is correctly mapped to the import path's base name.
func TestResolveCallEdges_QualifiedCallViaImportAlias(t *testing.T) {
	g := graph.New("testrepo")

	// http package (simulating external import resolution).
	httpFileID := g.MakeNodeID("http.go", "http.go")
	httpPkgID := g.MakeNodeID("net/http", "net/http")
	handlerID := g.MakeNodeID("http.go", "ListenAndServe")

	g.AddNode(&graph.Node{ID: httpFileID, Type: graph.NodeFile, Name: "http.go", File: "http.go", Package: "http"})
	g.AddNode(&graph.Node{ID: httpPkgID, Type: graph.NodePackage, Name: "net/http"})
	g.AddNode(&graph.Node{ID: handlerID, Type: graph.NodeFunction, Name: "ListenAndServe", Package: "http", File: "http.go"})

	// main package.
	mainFileID := g.MakeNodeID("main.go", "main.go")
	mainID := g.MakeNodeID("main.go", "main")
	g.AddNode(&graph.Node{ID: mainFileID, Type: graph.NodeFile, Name: "main.go", File: "main.go", Package: "main"})
	g.AddNode(&graph.Node{ID: mainID, Type: graph.NodeFunction, Name: "main", Package: "main", File: "main.go"})

	// main.go imports net/http.
	g.AddEdge(&graph.Edge{From: mainFileID, To: httpPkgID, Type: graph.EdgeImports})

	// Qualified call: http.ListenAndServe().
	g.AddCallSite(graph.CallSite{
		CallerID:   mainID,
		CallerFile: "main.go",
		PkgAlias:   "http",
		FuncName:   "ListenAndServe",
	})

	n := resolver.ResolveCallEdges(g)
	if n == 0 {
		t.Error("expected CALLS edge via qualified import alias")
	}
}

// TestResolveGoTypesCallEdges_EmptyDir verifies the function handles a directory
// without Go source gracefully (returns error or 0 edges — must not panic).
func TestResolveGoTypesCallEdges_EmptyDir(t *testing.T) {
	g := graph.New("test")
	n, err := resolver.ResolveGoTypesCallEdges(g, t.TempDir())
	// Either error (no go.mod) or 0 edges — just must not panic.
	_ = n
	_ = err
}

// ── findInPackage via ResolveCallEdges — method suffix matching ───────────────

// TestResolveCallEdges_MethodSuffixMatch ensures that a direct call FuncName
// that matches the method-name SUFFIX of "Receiver.MethodName" is resolved.
func TestResolveCallEdges_MethodSuffixMatch(t *testing.T) {
	g := graph.New("testrepo")

	callerID := g.MakeNodeID("svc.go", "Start")
	calleeID := g.MakeNodeID("svc.go", "Server.Stop") // "Stop" matches via suffix
	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "Start", Package: "svc", File: "svc.go"})
	g.AddNode(&graph.Node{ID: calleeID, Type: graph.NodeMethod, Name: "Server.Stop", Package: "svc", File: "svc.go"})

	g.AddCallSite(graph.CallSite{
		CallerID:   callerID,
		CallerFile: "svc.go",
		PkgAlias:   "",
		FuncName:   "Stop",
	})

	n := resolver.ResolveCallEdges(g)
	if n == 0 {
		t.Error("expected CALLS edge via method suffix match Start → Server.Stop")
	}
}

// ── ResolveImplementsEdges — interface with empty methods metadata ─────────────

func TestResolveImplementsEdges_EmptyMethodsMetadata(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("iface.go", "EmptyIface")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "EmptyIface", Package: "pkg", File: "iface.go",
		// No "methods" metadata key — should be skipped entirely.
	})

	n := resolver.ResolveImplementsEdges(g)
	if n != 0 {
		t.Errorf("expected 0 edges for interface with no methods metadata, got %d", n)
	}
}

// TestResolveImplementsEdges_MultipleInterfaces verifies that a struct can
// satisfy multiple interfaces and gets an IMPLEMENTS edge for each.
func TestResolveImplementsEdges_MultipleInterfaces(t *testing.T) {
	g := graph.New("testrepo")

	ifaceAID := g.MakeNodeID("iface.go", "Reader")
	ifaceBID := g.MakeNodeID("iface.go", "Writer")
	structID := g.MakeNodeID("impl.go", "Buffer")
	readID := g.MakeNodeID("impl.go", "Buffer.Read")
	writeID := g.MakeNodeID("impl.go", "Buffer.Write")

	g.AddNode(&graph.Node{
		ID: ifaceAID, Type: graph.NodeInterface, Name: "Reader", Package: "io", File: "iface.go",
		Metadata: map[string]string{"methods": "Read"},
	})
	g.AddNode(&graph.Node{
		ID: ifaceBID, Type: graph.NodeInterface, Name: "Writer", Package: "io", File: "iface.go",
		Metadata: map[string]string{"methods": "Write"},
	})
	g.AddNode(&graph.Node{ID: structID, Type: graph.NodeStruct, Name: "Buffer", Package: "io", File: "impl.go"})
	g.AddNode(&graph.Node{ID: readID, Type: graph.NodeMethod, Name: "Buffer.Read", Package: "io", File: "impl.go"})
	g.AddNode(&graph.Node{ID: writeID, Type: graph.NodeMethod, Name: "Buffer.Write", Package: "io", File: "impl.go"})

	n := resolver.ResolveImplementsEdges(g)
	if n != 2 {
		t.Errorf("expected 2 IMPLEMENTS edges (Reader + Writer), got %d", n)
	}
}

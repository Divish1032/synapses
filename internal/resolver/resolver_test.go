package resolver_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// buildResolverGraph creates a graph with two files in different packages.
//
//	auth.go (package auth):   AuthService (struct), AuthService.Login (method)
//	main.go (package main):   run (func) — has a call site to "Login" in pkg "auth"
func buildResolverGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")

	// auth.go nodes
	authFile := g.MakeNodeID("auth.go", "auth.go")
	authPkg := g.MakeNodeID("auth", "auth")
	svcID := g.MakeNodeID("auth.go", "AuthService")
	loginID := g.MakeNodeID("auth.go", "AuthService.Login")

	g.AddNode(&graph.Node{ID: authFile, Type: graph.NodeFile, Name: "auth.go", File: "auth.go", Package: "auth"})
	g.AddNode(&graph.Node{ID: authPkg, Type: graph.NodePackage, Name: "auth"})
	g.AddNode(&graph.Node{ID: svcID, Type: graph.NodeStruct, Name: "AuthService", Package: "auth", File: "auth.go"})
	g.AddNode(&graph.Node{ID: loginID, Type: graph.NodeMethod, Name: "AuthService.Login", Package: "auth", File: "auth.go"})

	// main.go nodes
	mainFile := g.MakeNodeID("main.go", "main.go")
	runID := g.MakeNodeID("main.go", "run")

	g.AddNode(&graph.Node{ID: mainFile, Type: graph.NodeFile, Name: "main.go", File: "main.go", Package: "main"})
	g.AddNode(&graph.Node{ID: runID, Type: graph.NodeFunction, Name: "run", Package: "main", File: "main.go"})

	// main.go imports auth package
	g.AddEdge(&graph.Edge{From: mainFile, To: authPkg, Type: graph.EdgeImports})

	// Seed a call site: run() calls auth.Login()
	g.AddCallSite(graph.CallSite{
		CallerID:   runID,
		CallerFile: "main.go",
		PkgAlias:   "auth",
		FuncName:   "Login",
	})

	return g
}

func TestResolveCallEdges_QualifiedCall(t *testing.T) {
	g := buildResolverGraph(t)

	n := resolver.ResolveCallEdges(g)
	if n == 0 {
		t.Fatal("expected at least 1 CALLS edge resolved, got 0")
	}

	// Verify run → AuthService.Login edge exists.
	runID := g.MakeNodeID("main.go", "run")
	loginID := g.MakeNodeID("auth.go", "AuthService.Login")

	found := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeCalls && e.From == runID && e.To == loginID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected CALLS edge run → AuthService.Login, not found")
	}
}

func TestResolveCallEdges_SamePackageDirectCall(t *testing.T) {
	g := graph.New("testrepo")

	callerID := g.MakeNodeID("svc.go", "Start")
	calleeID := g.MakeNodeID("svc.go", "Stop")

	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "Start", Package: "svc", File: "svc.go"})
	g.AddNode(&graph.Node{ID: calleeID, Type: graph.NodeFunction, Name: "Stop", Package: "svc", File: "svc.go"})

	// Direct same-package call (no PkgAlias).
	g.AddCallSite(graph.CallSite{
		CallerID:   callerID,
		CallerFile: "svc.go",
		PkgAlias:   "",
		FuncName:   "Stop",
	})

	n := resolver.ResolveCallEdges(g)
	if n != 1 {
		t.Errorf("expected 1 resolved CALLS edge, got %d", n)
	}
	found := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeCalls && e.From == callerID && e.To == calleeID {
			found = true
		}
	}
	if !found {
		t.Error("expected CALLS edge Start → Stop")
	}
}

func TestResolveCallEdges_Deduplication(t *testing.T) {
	g := graph.New("testrepo")

	callerID := g.MakeNodeID("a.go", "Foo")
	calleeID := g.MakeNodeID("a.go", "Bar")
	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "Foo", Package: "p", File: "a.go"})
	g.AddNode(&graph.Node{ID: calleeID, Type: graph.NodeFunction, Name: "Bar", Package: "p", File: "a.go"})

	// Add the same call site twice (simulates multiple call expressions in one function).
	for range 3 {
		g.AddCallSite(graph.CallSite{CallerID: callerID, CallerFile: "a.go", FuncName: "Bar"})
	}

	n := resolver.ResolveCallEdges(g)
	if n != 1 {
		t.Errorf("expected exactly 1 CALLS edge (deduplicated), got %d", n)
	}
}

func TestResolveCallEdges_UnknownTarget(t *testing.T) {
	g := graph.New("testrepo")

	callerID := g.MakeNodeID("a.go", "Foo")
	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "Foo", Package: "p", File: "a.go"})

	// Call site targeting a non-existent function.
	g.AddCallSite(graph.CallSite{CallerID: callerID, CallerFile: "a.go", FuncName: "DoesNotExist"})

	n := resolver.ResolveCallEdges(g)
	if n != 0 {
		t.Errorf("expected 0 edges for unresolvable target, got %d", n)
	}
}

func TestResolveCallEdges_DrainsPendingSites(t *testing.T) {
	g := graph.New("testrepo")

	callerID := g.MakeNodeID("a.go", "Foo")
	calleeID := g.MakeNodeID("a.go", "Bar")
	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "Foo", Package: "p", File: "a.go"})
	g.AddNode(&graph.Node{ID: calleeID, Type: graph.NodeFunction, Name: "Bar", Package: "p", File: "a.go"})
	g.AddCallSite(graph.CallSite{CallerID: callerID, CallerFile: "a.go", FuncName: "Bar"})

	resolver.ResolveCallEdges(g)

	// Second call should find no pending sites and return 0.
	n := resolver.ResolveCallEdges(g)
	if n != 0 {
		t.Errorf("expected 0 on second resolve (sites drained), got %d", n)
	}
}

// --- IMPLEMENTS tests ---

func TestResolveImplementsEdges_BasicMatch(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("iface.go", "Writer")
	structID := g.MakeNodeID("impl.go", "FileWriter")
	writeID := g.MakeNodeID("impl.go", "FileWriter.Write")

	g.AddNode(&graph.Node{
		ID:       ifaceID,
		Type:     graph.NodeInterface,
		Name:     "Writer",
		Package:  "io",
		File:     "iface.go",
		Metadata: map[string]string{"methods": "Write"},
	})
	g.AddNode(&graph.Node{ID: structID, Type: graph.NodeStruct, Name: "FileWriter", Package: "io", File: "impl.go"})
	g.AddNode(&graph.Node{ID: writeID, Type: graph.NodeMethod, Name: "FileWriter.Write", Package: "io", File: "impl.go"})

	n := resolver.ResolveImplementsEdges(g)
	if n != 1 {
		t.Errorf("expected 1 IMPLEMENTS edge, got %d", n)
	}

	found := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeImplements && e.From == structID && e.To == ifaceID {
			found = true
		}
	}
	if !found {
		t.Error("expected IMPLEMENTS edge FileWriter → Writer")
	}
}

func TestResolveImplementsEdges_PartialMethodsMismatch(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("iface.go", "ReadWriter")
	structID := g.MakeNodeID("impl.go", "Partial")
	readID := g.MakeNodeID("impl.go", "Partial.Read")

	g.AddNode(&graph.Node{
		ID:       ifaceID,
		Type:     graph.NodeInterface,
		Name:     "ReadWriter",
		Package:  "io",
		File:     "iface.go",
		Metadata: map[string]string{"methods": "Read,Write"}, // requires both
	})
	g.AddNode(&graph.Node{ID: structID, Type: graph.NodeStruct, Name: "Partial", Package: "io", File: "impl.go"})
	g.AddNode(&graph.Node{ID: readID, Type: graph.NodeMethod, Name: "Partial.Read", Package: "io", File: "impl.go"})
	// No Write method — should NOT implement ReadWriter.

	n := resolver.ResolveImplementsEdges(g)
	if n != 0 {
		t.Errorf("expected 0 IMPLEMENTS edges (missing Write), got %d", n)
	}
}

func TestResolveImplementsEdges_NoDuplicates(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("iface.go", "Doer")
	structID := g.MakeNodeID("impl.go", "MyDoer")
	doID := g.MakeNodeID("impl.go", "MyDoer.Do")

	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "Doer", Package: "p", File: "iface.go",
		Metadata: map[string]string{"methods": "Do"},
	})
	g.AddNode(&graph.Node{ID: structID, Type: graph.NodeStruct, Name: "MyDoer", Package: "p", File: "impl.go"})
	g.AddNode(&graph.Node{ID: doID, Type: graph.NodeMethod, Name: "MyDoer.Do", Package: "p", File: "impl.go"})

	resolver.ResolveImplementsEdges(g)
	// Call again — should add 0 new edges.
	n := resolver.ResolveImplementsEdges(g)
	if n != 0 {
		t.Errorf("expected 0 new IMPLEMENTS edges on second call (dedup), got %d", n)
	}
}

func TestResolveImplementsEdges_CrossPackageIgnored(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("iface.go", "Signer")
	structID := g.MakeNodeID("impl.go", "RSASigner")
	signID := g.MakeNodeID("impl.go", "RSASigner.Sign")

	// Interface in package "crypto", struct in package "rsa" — different packages.
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface, Name: "Signer", Package: "crypto", File: "iface.go",
		Metadata: map[string]string{"methods": "Sign"},
	})
	g.AddNode(&graph.Node{ID: structID, Type: graph.NodeStruct, Name: "RSASigner", Package: "rsa", File: "impl.go"})
	g.AddNode(&graph.Node{ID: signID, Type: graph.NodeMethod, Name: "RSASigner.Sign", Package: "rsa", File: "impl.go"})

	n := resolver.ResolveImplementsEdges(g)
	if n != 0 {
		t.Errorf("expected 0 IMPLEMENTS edges across packages (heuristic only matches same-package), got %d", n)
	}
}

// TestResolveCallEdges_DeterministicAcrossImports verifies that when two
// imported packages define a function with the same name, the resolver
// always picks the same target regardless of map iteration order.
// This is the regression test for C1 — import map values are now sorted.
func TestResolveCallEdges_DeterministicAcrossImports(t *testing.T) {
	// Run multiple iterations — map iteration order is randomized per Go spec,
	// so repeated runs increase confidence of determinism.
	var firstTarget graph.NodeID
	for iter := 0; iter < 20; iter++ {
		g := graph.New("testrepo")

		// Two packages, each defining a function called "Connect".
		alphaFile := g.MakeNodeID("alpha.go", "alpha.go")
		alphaPkg := g.MakeNodeID("alpha", "alpha")
		alphaConnect := g.MakeNodeID("alpha.go", "Connect")

		betaFile := g.MakeNodeID("beta.go", "beta.go")
		betaPkg := g.MakeNodeID("beta", "beta")
		betaConnect := g.MakeNodeID("beta.go", "Connect")

		callerFile := g.MakeNodeID("main.go", "main.go")
		callerFn := g.MakeNodeID("main.go", "Init")

		g.AddNode(&graph.Node{ID: alphaFile, Type: graph.NodeFile, Name: "alpha.go", File: "alpha.go", Package: "alpha"})
		g.AddNode(&graph.Node{ID: alphaPkg, Type: graph.NodePackage, Name: "github.com/test/alpha"})
		g.AddNode(&graph.Node{ID: alphaConnect, Type: graph.NodeFunction, Name: "Connect", Package: "alpha", File: "alpha.go"})

		g.AddNode(&graph.Node{ID: betaFile, Type: graph.NodeFile, Name: "beta.go", File: "beta.go", Package: "beta"})
		g.AddNode(&graph.Node{ID: betaPkg, Type: graph.NodePackage, Name: "github.com/test/beta"})
		g.AddNode(&graph.Node{ID: betaConnect, Type: graph.NodeFunction, Name: "Connect", Package: "beta", File: "beta.go"})

		g.AddNode(&graph.Node{ID: callerFile, Type: graph.NodeFile, Name: "main.go", File: "main.go", Package: "main"})
		g.AddNode(&graph.Node{ID: callerFn, Type: graph.NodeFunction, Name: "Init", Package: "main", File: "main.go"})

		// main.go imports both packages.
		g.AddEdge(&graph.Edge{From: callerFile, To: alphaPkg, Type: graph.EdgeImports})
		g.AddEdge(&graph.Edge{From: callerFile, To: betaPkg, Type: graph.EdgeImports})

		// Unqualified call to "Connect" from Init — ambiguous between alpha and beta.
		// The fallback path iterates imports; sort ensures deterministic resolution.
		g.AddCallSite(graph.CallSite{
			CallerID:   callerFn,
			CallerFile: "main.go",
			PkgAlias:   "unknown", // not a real import alias, forces fallback path
			FuncName:   "Connect",
		})

		resolver.ResolveCallEdges(g)

		// Find which target was chosen.
		var target graph.NodeID
		for _, e := range g.AllEdges() {
			if e.Type == graph.EdgeCalls && e.From == callerFn {
				target = e.To
				break
			}
		}
		if target == "" {
			t.Fatal("expected a CALLS edge to be resolved, got none")
		}

		if iter == 0 {
			firstTarget = target
		} else if target != firstTarget {
			t.Fatalf("non-deterministic resolution: iteration 0 got %s, iteration %d got %s",
				firstTarget, iter, target)
		}
	}
}

// TestResolveCallEdges_CapitalizedAliasHeuristic tests that lowercase variable
// names (e.g. "router") are matched to their capitalized type (Router.method).
// This is essential for Rust/Go where router.route() should resolve to Router.route.
func TestResolveCallEdges_CapitalizedAliasHeuristic(t *testing.T) {
	g := graph.New("test")

	// Setup: file with a struct + method
	fileID := g.MakeNodeID("router.rs", "router.rs")
	g.AddNode(&graph.Node{ID: fileID, Type: graph.NodeFile, Name: "router.rs", File: "router.rs"})

	structID := g.MakeNodeID("router.rs", "Router")
	g.AddNode(&graph.Node{ID: structID, Type: graph.NodeStruct, Name: "Router", Package: "axum", File: "router.rs"})

	methodID := g.MakeNodeID("router.rs", "Router.route")
	g.AddNode(&graph.Node{ID: methodID, Type: graph.NodeMethod, Name: "Router.route", Package: "axum", File: "router.rs"})

	// Caller file with call site: router.route()
	callerFileID := g.MakeNodeID("main.rs", "main.rs")
	g.AddNode(&graph.Node{ID: callerFileID, Type: graph.NodeFile, Name: "main.rs", File: "main.rs"})

	callerID := g.MakeNodeID("main.rs", "main")
	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "main", Package: "axum", File: "main.rs"})

	g.AddCallSite(graph.CallSite{
		CallerID:   callerID,
		CallerFile: "main.rs",
		FuncName:   "route",
		PkgAlias:   "router", // lowercase — should match Router.route
	})

	count := resolver.ResolveCallEdges(g)
	if count == 0 {
		t.Fatal("expected at least 1 CALLS edge from capitalized alias heuristic, got 0")
	}

	found := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeCalls && e.From == callerID && e.To == methodID {
			found = true
		}
	}
	if !found {
		t.Error("expected CALLS edge main → Router.route via capitalized alias heuristic")
	}
}

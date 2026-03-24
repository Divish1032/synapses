package watcher

// Unit tests for the import-graph methods added in Sprint 14.3.
// Tests cover initImportGraph (filePkg seeding), computeInvalidationSet
// (CALLS-edge based, language-agnostic), and updateImportGraphForFile (filePkg update).

import (
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/store"
)

// newTestWatcher creates a minimal Watcher for unit testing the import-graph
// methods. Store is nil (no persistence); graph has nodes pre-populated.
func newTestWatcher(t *testing.T, g *graph.Graph) *Watcher {
	t.Helper()
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { w.Stop() })
	return w
}

// buildCallsTestGraph creates a graph with CALLS edges:
//
//	a.go (pkg "models"): DoModel --CALLS--> Open (c.go "store")
//	b.go (pkg "handler"): Handle --CALLS--> DoModel (a.go "models")
//	c.go (pkg "store"): Open (no outgoing calls)
func buildCallsTestGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("repo")

	aFuncID := g.MakeNodeID("a.go", "DoModel")
	g.AddNode(&graph.Node{ID: aFuncID, Type: graph.NodeFunction, Name: "DoModel", File: "a.go", Package: "models"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("a.go", "a.go"), Type: graph.NodeFile, Name: "a.go", File: "a.go", Package: "models"})

	bFuncID := g.MakeNodeID("b.go", "Handle")
	g.AddNode(&graph.Node{ID: bFuncID, Type: graph.NodeFunction, Name: "Handle", File: "b.go", Package: "handler"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("b.go", "b.go"), Type: graph.NodeFile, Name: "b.go", File: "b.go", Package: "handler"})

	cFuncID := g.MakeNodeID("c.go", "Open")
	g.AddNode(&graph.Node{ID: cFuncID, Type: graph.NodeFunction, Name: "Open", File: "c.go", Package: "store"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("c.go", "c.go"), Type: graph.NodeFile, Name: "c.go", File: "c.go", Package: "store"})

	// b.go calls a.go
	g.AddEdge(&graph.Edge{From: bFuncID, To: aFuncID, Type: graph.EdgeCalls})
	// a.go calls c.go
	g.AddEdge(&graph.Edge{From: aFuncID, To: cFuncID, Type: graph.EdgeCalls})

	return g
}

func TestInitImportGraph_BuildsFilePkg(t *testing.T) {
	g := buildCallsTestGraph(t)
	w := newTestWatcher(t, g)

	if w.filePkg["a.go"] != "models" {
		t.Errorf("a.go filePkg: want 'models', got %q", w.filePkg["a.go"])
	}
	if w.filePkg["b.go"] != "handler" {
		t.Errorf("b.go filePkg: want 'handler', got %q", w.filePkg["b.go"])
	}
	if w.filePkg["c.go"] != "store" {
		t.Errorf("c.go filePkg: want 'store', got %q", w.filePkg["c.go"])
	}
}

func TestInitImportGraph_SkipsNodePackageNodes(t *testing.T) {
	g := graph.New("repo")
	// NodeFile: a.go with package "models"
	g.AddNode(&graph.Node{ID: g.MakeNodeID("a.go", "a.go"), Type: graph.NodeFile, Name: "a.go", File: "a.go", Package: "models"})
	// NodePackage: has File="a.go" but Package="store" (the imported pkg name).
	// It must NOT overwrite filePkg["a.go"] with "store".
	g.AddNode(&graph.Node{ID: g.MakeNodeID("store", "store"), Type: graph.NodePackage, Name: "store", File: "a.go", Package: "store"})

	w := newTestWatcher(t, g)
	if w.filePkg["a.go"] != "models" {
		t.Errorf("NodePackage should not overwrite filePkg; want 'models', got %q", w.filePkg["a.go"])
	}
}

// TestComputeInvalidationSet_IncludesCallers verifies that files with CALLS
// edges into the changed file are included in the invalidation set.
// The strategy is language-agnostic: it uses resolved CALLS edges, not import names.
func TestComputeInvalidationSet_IncludesCallers(t *testing.T) {
	g := buildCallsTestGraph(t)
	w := newTestWatcher(t, g)

	// When a.go changes:
	// - b.go has Handle --CALLS--> DoModel(a.go) → b.go must be in set
	// - c.go has no edges INTO a.go → c.go must NOT be in set
	invalid := w.computeInvalidationSet("a.go")
	sort.Strings(invalid)

	if !slices.Contains(invalid, "a.go") {
		t.Errorf("a.go itself must be in invalidation set, got %v", invalid)
	}
	if !slices.Contains(invalid, "b.go") {
		t.Errorf("b.go (calls a.go) must be in invalidation set, got %v", invalid)
	}
	if slices.Contains(invalid, "c.go") {
		t.Errorf("c.go (called BY a.go, but doesn't call a.go) must NOT be in set, got %v", invalid)
	}
}

// TestComputeInvalidationSet_IncludesSamePackageFiles verifies that files in
// the same package are always included (they can have unresolved call sites
// that reference new functions not yet in CALLS edges).
func TestComputeInvalidationSet_IncludesSamePackageFiles(t *testing.T) {
	g := graph.New("repo")

	// Two files in the same package "models", no CALLS edges between them yet.
	g.AddNode(&graph.Node{ID: g.MakeNodeID("models/a.go", "models/a.go"), Type: graph.NodeFile, Name: "models/a.go", File: "models/a.go", Package: "models"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("models/a.go", "FuncA"), Type: graph.NodeFunction, Name: "FuncA", File: "models/a.go", Package: "models"})

	g.AddNode(&graph.Node{ID: g.MakeNodeID("models/b.go", "models/b.go"), Type: graph.NodeFile, Name: "models/b.go", File: "models/b.go", Package: "models"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("models/b.go", "FuncB"), Type: graph.NodeFunction, Name: "FuncB", File: "models/b.go", Package: "models"})

	w := newTestWatcher(t, g)

	// When models/a.go changes, models/b.go (same package) must be in the set.
	invalid := w.computeInvalidationSet("models/a.go")
	if !slices.Contains(invalid, "models/b.go") {
		t.Errorf("same-package file models/b.go should be in invalidation set, got %v", invalid)
	}
}

// TestComputeInvalidationSet_UnknownFileReturnsSelf verifies that an unknown
// file (no nodes in the graph) returns just itself.
func TestComputeInvalidationSet_UnknownFileReturnsSelf(t *testing.T) {
	g := graph.New("repo")
	w := newTestWatcher(t, g)

	invalid := w.computeInvalidationSet("unknown.go")
	if len(invalid) != 1 || invalid[0] != "unknown.go" {
		t.Errorf("unknown file should return just itself, got %v", invalid)
	}
}

// TestComputeInvalidationSet_AllLanguagesViaCalls verifies the key property:
// the invalidation set is driven purely by CALLS edges, not by import-path
// name matching. This means it works correctly for all languages (Go, Java,
// TypeScript, Python, etc.) without any language-specific heuristics.
//
// Scenario: Java-style caller "Bar.java" calls "Foo.java"'s function.
// The CALLS edge is the ground truth — no import name matching needed.
func TestComputeInvalidationSet_AllLanguagesViaCalls(t *testing.T) {
	g := graph.New("repo")

	// Foo.java: package "com.example.models"
	fooFuncID := g.MakeNodeID("Foo.java", "doFoo")
	g.AddNode(&graph.Node{ID: fooFuncID, Type: graph.NodeFunction, Name: "doFoo", File: "Foo.java", Package: "com.example.models"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("Foo.java", "Foo.java"), Type: graph.NodeFile, Name: "Foo.java", File: "Foo.java", Package: "com.example.models"})

	// Bar.java: package "com.example.other" — calls Foo.doFoo
	barFuncID := g.MakeNodeID("Bar.java", "doBar")
	g.AddNode(&graph.Node{ID: barFuncID, Type: graph.NodeFunction, Name: "doBar", File: "Bar.java", Package: "com.example.other"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("Bar.java", "Bar.java"), Type: graph.NodeFile, Name: "Bar.java", File: "Bar.java", Package: "com.example.other"})

	// Resolved CALLS edge: Bar.doBar → Foo.doFoo
	g.AddEdge(&graph.Edge{From: barFuncID, To: fooFuncID, Type: graph.EdgeCalls})

	w := newTestWatcher(t, g)

	// When Foo.java changes, Bar.java MUST be in the invalidation set —
	// found via the CALLS edge, NOT via import name matching.
	invalid := w.computeInvalidationSet("Foo.java")
	if !slices.Contains(invalid, "Bar.java") {
		t.Errorf("Bar.java (calls Foo.java) must be in invalidation set for Foo.java, got %v", invalid)
	}
}

// TestComputeInvalidationSet_UnresolvedCrossPackageCallers verifies the third
// invalidation layer: when a file adds a new exported function, other files
// that import its package (but have no CALLS edge yet) are found via a DB
// query on call_sites.pkg_alias. This is the "new public function" gap.
func TestComputeInvalidationSet_UnresolvedCrossPackageCallers(t *testing.T) {
	// Use a temporary on-disk store so DB queries are real.
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Seed: handler.go imports package "models" but has no CALLS edge yet
	// (it references NewUser which doesn't exist yet).
	sites := []graph.CallSite{
		{
			CallerID:   graph.NodeID("repo::handler.go::Handle"),
			CallerFile: "handler.go",
			PkgAlias:   "models",
			FuncName:   "NewUser",
		},
	}
	if err := st.SaveCallSites(sites); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}

	// Graph: models/user.go defines existing functions — no CALLS edge to handler.go.
	g := graph.New("repo")
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("models/user.go", "models/user.go"),
		Type: graph.NodeFile, Name: "models/user.go",
		File: "models/user.go", Package: "models",
	})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("models/user.go", "OldFunc"),
		Type: graph.NodeFunction, Name: "OldFunc",
		File: "models/user.go", Package: "models",
	})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("handler.go", "handler.go"),
		Type: graph.NodeFile, Name: "handler.go",
		File: "handler.go", Package: "main",
	})

	w, werr := New(g, parser.NewWalker(), st)
	if werr != nil {
		t.Fatalf("New: %v", werr)
	}
	t.Cleanup(func() { w.Stop() })

	// When models/user.go is modified (e.g. NewUser is added),
	// handler.go MUST appear in the invalidation set via the DB query —
	// even though no CALLS edge from handler.go → models/user.go exists yet.
	invalid := w.computeInvalidationSet("models/user.go")

	if !slices.Contains(invalid, "handler.go") {
		t.Errorf("handler.go (unresolved caller via pkg_alias='models') must be in invalidation set, got %v", invalid)
	}
	if !slices.Contains(invalid, "models/user.go") {
		t.Errorf("models/user.go itself must be in invalidation set, got %v", invalid)
	}
}

// TestComputeInvalidationSet_TransitiveCallers verifies the 1-hop transitive
// invalidation: if c.go calls b.go and b.go calls a.go, changing a.go must
// invalidate all three files (a.go direct, b.go direct caller, c.go transitive
// caller-of-caller).
func TestComputeInvalidationSet_TransitiveCallers(t *testing.T) {
	g := graph.New("repo")

	// a.go (pkg "alpha")
	aFuncID := g.MakeNodeID("a.go", "FuncA")
	g.AddNode(&graph.Node{ID: aFuncID, Type: graph.NodeFunction, Name: "FuncA", File: "a.go", Package: "alpha"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("a.go", "a.go"), Type: graph.NodeFile, Name: "a.go", File: "a.go", Package: "alpha"})

	// b.go (pkg "beta") — calls a.go
	bFuncID := g.MakeNodeID("b.go", "FuncB")
	g.AddNode(&graph.Node{ID: bFuncID, Type: graph.NodeFunction, Name: "FuncB", File: "b.go", Package: "beta"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("b.go", "b.go"), Type: graph.NodeFile, Name: "b.go", File: "b.go", Package: "beta"})

	// c.go (pkg "gamma") — calls b.go
	cFuncID := g.MakeNodeID("c.go", "FuncC")
	g.AddNode(&graph.Node{ID: cFuncID, Type: graph.NodeFunction, Name: "FuncC", File: "c.go", Package: "gamma"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("c.go", "c.go"), Type: graph.NodeFile, Name: "c.go", File: "c.go", Package: "gamma"})

	// b.go → a.go (b calls a)
	g.AddEdge(&graph.Edge{From: bFuncID, To: aFuncID, Type: graph.EdgeCalls})
	// c.go → b.go (c calls b)
	g.AddEdge(&graph.Edge{From: cFuncID, To: bFuncID, Type: graph.EdgeCalls})

	w := newTestWatcher(t, g)

	invalid := w.computeInvalidationSet("a.go")
	sort.Strings(invalid)

	if !slices.Contains(invalid, "a.go") {
		t.Errorf("a.go (changed file) must be in invalidation set, got %v", invalid)
	}
	if !slices.Contains(invalid, "b.go") {
		t.Errorf("b.go (direct caller of a.go) must be in invalidation set, got %v", invalid)
	}
	if !slices.Contains(invalid, "c.go") {
		t.Errorf("c.go (transitive caller: calls b.go which calls a.go) must be in invalidation set, got %v", invalid)
	}
}

func TestUpdateImportGraphForFile_UpdatesFilePkg(t *testing.T) {
	g := buildCallsTestGraph(t)
	w := newTestWatcher(t, g)

	// Pre-condition: a.go is in package "models"
	if w.filePkg["a.go"] != "models" {
		t.Fatal("pre-condition: a.go should be in package models")
	}

	// Simulate a.go being re-parsed and now in package "newpkg".
	g2 := graph.New("repo")
	g2.AddNode(&graph.Node{ID: g2.MakeNodeID("a.go", "a.go"), Type: graph.NodeFile, Name: "a.go", File: "a.go", Package: "newpkg"})
	g2.AddNode(&graph.Node{ID: g2.MakeNodeID("a.go", "DoModel"), Type: graph.NodeFunction, Name: "DoModel", File: "a.go", Package: "newpkg"})

	w.graph = g2
	w.updateImportGraphForFile("a.go")

	if w.filePkg["a.go"] != "newpkg" {
		t.Errorf("after update, filePkg[a.go] should be 'newpkg', got %q", w.filePkg["a.go"])
	}
}

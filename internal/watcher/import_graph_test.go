package watcher

// Unit tests for the import-graph methods added in Sprint 14.3.
// These test initImportGraph, updateImportGraphForFile, and
// computeInvalidationSet directly since they are on internal watcher state.

import (
	"slices"
	"sort"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
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

// buildImportTestGraph creates a graph with IMPORTS edges:
//
//	a.go (pkg "models") imports "store" and "config"
//	b.go (pkg "handler") imports "models"
//	c.go (pkg "store") no imports
func buildImportTestGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("repo")

	// a.go: file node + function node
	aFileID := g.MakeNodeID("a.go", "a.go")
	g.AddNode(&graph.Node{ID: aFileID, Type: graph.NodeFile, Name: "a.go", File: "a.go", Package: "models"})
	aFuncID := g.MakeNodeID("a.go", "DoModel")
	g.AddNode(&graph.Node{ID: aFuncID, Type: graph.NodeFunction, Name: "DoModel", File: "a.go", Package: "models"})

	// b.go: file node + function node
	bFileID := g.MakeNodeID("b.go", "b.go")
	g.AddNode(&graph.Node{ID: bFileID, Type: graph.NodeFile, Name: "b.go", File: "b.go", Package: "handler"})
	bFuncID := g.MakeNodeID("b.go", "Handle")
	g.AddNode(&graph.Node{ID: bFuncID, Type: graph.NodeFunction, Name: "Handle", File: "b.go", Package: "handler"})

	// c.go: file node + function node
	cFileID := g.MakeNodeID("c.go", "c.go")
	g.AddNode(&graph.Node{ID: cFileID, Type: graph.NodeFile, Name: "c.go", File: "c.go", Package: "store"})
	cFuncID := g.MakeNodeID("c.go", "Open")
	g.AddNode(&graph.Node{ID: cFuncID, Type: graph.NodeFunction, Name: "Open", File: "c.go", Package: "store"})

	// Package nodes for imports
	storePkgID := g.MakeNodeID("store", "store")
	g.AddNode(&graph.Node{ID: storePkgID, Type: graph.NodePackage, Name: "store", File: "a.go", Package: "store"})
	configPkgID := g.MakeNodeID("config", "config")
	g.AddNode(&graph.Node{ID: configPkgID, Type: graph.NodePackage, Name: "config", File: "a.go", Package: "config"})
	modelsPkgID := g.MakeNodeID("models", "models")
	g.AddNode(&graph.Node{ID: modelsPkgID, Type: graph.NodePackage, Name: "models", File: "b.go", Package: "models"})

	// a.go imports store and config
	g.AddEdge(&graph.Edge{From: aFileID, To: storePkgID, Type: graph.EdgeImports})
	g.AddEdge(&graph.Edge{From: aFileID, To: configPkgID, Type: graph.EdgeImports})
	// b.go imports models
	g.AddEdge(&graph.Edge{From: bFileID, To: modelsPkgID, Type: graph.EdgeImports})

	return g
}

func TestInitImportGraph_BuildsFilePkg(t *testing.T) {
	g := buildImportTestGraph(t)
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

func TestInitImportGraph_BuildsPkgImporters(t *testing.T) {
	g := buildImportTestGraph(t)
	w := newTestWatcher(t, g)

	// b.go imports "models", so pkgImporters["models"] should contain b.go
	if !w.pkgImporters["models"]["b.go"] {
		t.Errorf("pkgImporters[models] should contain b.go, got %v", w.pkgImporters["models"])
	}
	// a.go imports "store" and "config"
	if !w.pkgImporters["store"]["a.go"] {
		t.Errorf("pkgImporters[store] should contain a.go, got %v", w.pkgImporters["store"])
	}
	if !w.pkgImporters["config"]["a.go"] {
		t.Errorf("pkgImporters[config] should contain a.go, got %v", w.pkgImporters["config"])
	}
}

func TestComputeInvalidationSet_IncludesChangedFileAndImporters(t *testing.T) {
	g := buildImportTestGraph(t)
	w := newTestWatcher(t, g)

	// When "models" package (a.go) changes:
	// - a.go itself is in the set
	// - b.go (imports "models") is in the set
	// - c.go is NOT in the set (doesn't import models)
	invalid := w.computeInvalidationSet("a.go")
	sort.Strings(invalid)

	wantContains := []string{"a.go", "b.go"}
	for _, f := range wantContains {
		if !slices.Contains(invalid, f) {
			t.Errorf("invalidation set for a.go should contain %q, got %v", f, invalid)
		}
	}
	if slices.Contains(invalid, "c.go") {
		t.Errorf("c.go should NOT be in invalidation set for a.go (it doesn't import models), got %v", invalid)
	}
}

func TestComputeInvalidationSet_IncludesSamePackageFiles(t *testing.T) {
	g := graph.New("repo")

	// Two files in the same package "models"
	a1FileID := g.MakeNodeID("models/a.go", "models/a.go")
	g.AddNode(&graph.Node{ID: a1FileID, Type: graph.NodeFile, Name: "models/a.go", File: "models/a.go", Package: "models"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("models/a.go", "FuncA"), Type: graph.NodeFunction, Name: "FuncA", File: "models/a.go", Package: "models"})

	a2FileID := g.MakeNodeID("models/b.go", "models/b.go")
	g.AddNode(&graph.Node{ID: a2FileID, Type: graph.NodeFile, Name: "models/b.go", File: "models/b.go", Package: "models"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("models/b.go", "FuncB"), Type: graph.NodeFunction, Name: "FuncB", File: "models/b.go", Package: "models"})

	w := newTestWatcher(t, g)

	// When models/a.go changes, models/b.go (same package) must be in the set
	invalid := w.computeInvalidationSet("models/a.go")
	if !slices.Contains(invalid, "models/b.go") {
		t.Errorf("same-package file models/b.go should be in invalidation set, got %v", invalid)
	}
}

func TestComputeInvalidationSet_UnknownFileReturnsSelf(t *testing.T) {
	g := graph.New("repo")
	w := newTestWatcher(t, g)

	invalid := w.computeInvalidationSet("unknown.go")
	if len(invalid) != 1 || invalid[0] != "unknown.go" {
		t.Errorf("unknown file should return just itself, got %v", invalid)
	}
}

// TestComputeInvalidationSet_JavaStyleFilenameBase verifies that the filename-base
// fallback correctly finds importers for OOP languages (Java, C#, Kotlin) where
// the import path includes the class name and path.Base(importPath) = filename base,
// while filePkg stores the fully-qualified package name.
//
// Example: Foo.java in package "com.example.models".
// Importer Bar.java records pkgImporters["Foo"] (from path.Base("com.example.models.Foo")).
// filePkg["Foo.java"] = "com.example.models" — these differ, so a plain pkg lookup misses it.
// The filename-base fallback ("Foo") must cover this case.
func TestComputeInvalidationSet_JavaStyleFilenameBase(t *testing.T) {
	g := graph.New("repo")

	// Foo.java: package "com.example.models"
	fooFileID := g.MakeNodeID("Foo.java", "Foo.java")
	g.AddNode(&graph.Node{ID: fooFileID, Type: graph.NodeFile, Name: "Foo.java", File: "Foo.java", Package: "com.example.models"})
	g.AddNode(&graph.Node{ID: g.MakeNodeID("Foo.java", "Foo"), Type: graph.NodeFunction, Name: "Foo", File: "Foo.java", Package: "com.example.models"})

	// Bar.java imports "com.example.models.Foo" — parser stores pkgNode.Name = "com.example.models.Foo".
	// path.Base of that = "Foo", so pkgImporters["Foo"]["Bar.java"] is set.
	barFileID := g.MakeNodeID("Bar.java", "Bar.java")
	g.AddNode(&graph.Node{ID: barFileID, Type: graph.NodeFile, Name: "Bar.java", File: "Bar.java", Package: "com.example.other"})
	fooImportID := g.MakeNodeID("com.example.models.Foo", "com.example.models.Foo")
	g.AddNode(&graph.Node{ID: fooImportID, Type: graph.NodePackage, Name: "com.example.models.Foo", File: "Bar.java", Package: "com.example.models.Foo"})
	g.AddEdge(&graph.Edge{From: barFileID, To: fooImportID, Type: graph.EdgeImports})

	w := newTestWatcher(t, g)

	// path.Base("com.example.models.Foo") returns the full string (no slash separator),
	// so pkgImporters is keyed by the FULL dotted path, not just "Foo".
	if !w.pkgImporters["com.example.models.Foo"]["Bar.java"] {
		t.Fatalf("pre-condition: pkgImporters[com.example.models.Foo] should contain Bar.java, got %v", w.pkgImporters)
	}
	// filePkg["Foo.java"] = "com.example.models" (the full Java package name)
	if w.filePkg["Foo.java"] != "com.example.models" {
		t.Fatalf("pre-condition: filePkg[Foo.java] should be 'com.example.models', got %q", w.filePkg["Foo.java"])
	}

	// When Foo.java changes, Bar.java MUST be in the invalidation set.
	// Without the filename-base fallback, pkgImporters["com.example.models"] is
	// empty and Bar.java would be missed, silently losing the Bar→Foo CALLS edge.
	invalid := w.computeInvalidationSet("Foo.java")
	found := false
	for _, f := range invalid {
		if f == "Bar.java" {
			found = true
		}
	}
	if !found {
		t.Errorf("Java-style importer Bar.java must be in invalidation set for Foo.java, got %v", invalid)
	}
}

func TestUpdateImportGraphForFile_UpdatesPkgImporters(t *testing.T) {
	g := buildImportTestGraph(t)
	w := newTestWatcher(t, g)

	// Initially b.go imports "models"
	if !w.pkgImporters["models"]["b.go"] {
		t.Fatal("pre-condition: b.go should import models")
	}

	// Simulate b.go being re-parsed with no imports (clear all IMPORTS edges)
	// by removing its file node's IMPORTS edges and calling updateImportGraphForFile.
	// (In a real reparse, RemoveFile + ParseFile would do this; here we simulate
	//  the post-parse state by building a new minimal graph.)
	g2 := graph.New("repo")
	bFileID2 := g2.MakeNodeID("b.go", "b.go")
	g2.AddNode(&graph.Node{ID: bFileID2, Type: graph.NodeFile, Name: "b.go", File: "b.go", Package: "handler"})
	g2.AddNode(&graph.Node{ID: g2.MakeNodeID("b.go", "Handle"), Type: graph.NodeFunction, Name: "Handle", File: "b.go", Package: "handler"})
	// No IMPORTS edges — b.go now imports nothing.

	// Patch watcher's graph reference to the new graph.
	w.graph = g2
	w.updateImportGraphForFile("b.go")

	if w.pkgImporters["models"]["b.go"] {
		t.Error("after update, b.go should no longer be in pkgImporters[models]")
	}
	if w.fileImports["b.go"] != nil && len(w.fileImports["b.go"]) != 0 {
		t.Errorf("after update, b.go fileImports should be empty, got %v", w.fileImports["b.go"])
	}
}

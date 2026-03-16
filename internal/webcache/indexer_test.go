package webcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

func TestHandleGoModChanged(t *testing.T) {
	s := testStore(t)
	c := New(s)

	// Create a project dir with a go.mod
	dir := t.TempDir()
	gomod := `module test

require (
	github.com/foo/bar v1.0.0
)
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-populate cache
	key := PackageCacheKey("github.com/foo/bar", "v1.0.0")
	if err := s.UpsertWebCache(key, "old docs", 0); err != nil {
		t.Fatal(err)
	}

	HandleGoModChanged(dir, c)

	// Cache should be invalidated
	if _, ok := s.GetWebCache(key); ok {
		t.Error("expected cache entry to be invalidated after go.mod change")
	}
}

func TestHandleGoModChanged_NoGoMod(t *testing.T) {
	s := testStore(t)
	c := New(s)

	// Should not panic with a directory without go.mod
	HandleGoModChanged(t.TempDir(), c)
}

func TestIndexProjectImports_EmptyGraph(t *testing.T) {
	s := testStore(t)
	c := New(s)
	g := graph.New("test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Should return immediately with no package nodes
	IndexProjectImports(ctx, t.TempDir(), g, c, 10)
}

func TestIndexProjectImports_CancelledContext(t *testing.T) {
	s := testStore(t)
	c := New(s)
	g := graph.New("test")

	// Add a package node
	g.AddNode(&graph.Node{
		ID:      graph.NodeID("test::pkg:github.com/example/lib"),
		Name:    "github.com/example/lib",
		Type:    graph.NodePackage,
		Package: "github.com/example/lib",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Should return quickly due to cancelled context
	IndexProjectImports(ctx, t.TempDir(), g, c, 10)
}

func TestIndexProjectImports_SkipStdlib(t *testing.T) {
	s := testStore(t)
	c := New(s)
	g := graph.New("test")

	// Add stdlib package node — should be skipped
	g.AddNode(&graph.Node{
		ID:      graph.NodeID("test::pkg:fmt"),
		Name:    "fmt",
		Type:    graph.NodePackage,
		Package: "fmt",
	})

	ctx := context.Background()
	IndexProjectImports(ctx, t.TempDir(), g, c, 10)
	// No crash, no fetch attempted for stdlib
}

func TestIndexProjectImports_AlreadyCached(t *testing.T) {
	s := testStore(t)
	c := New(s)
	g := graph.New("test")

	pkgName := "github.com/cached/pkg"
	g.AddNode(&graph.Node{
		ID:      graph.NodeID("test::pkg:" + pkgName),
		Name:    pkgName,
		Type:    graph.NodePackage,
		Package: pkgName,
	})

	// Pre-populate cache
	key := PackageCacheKey(pkgName, "")
	if err := s.UpsertWebCache(key, "already cached", 0); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	IndexProjectImports(ctx, t.TempDir(), g, c, 10)
	// Should skip fetch since already cached
}

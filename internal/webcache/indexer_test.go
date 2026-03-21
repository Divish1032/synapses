package webcache

import (
	"context"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

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

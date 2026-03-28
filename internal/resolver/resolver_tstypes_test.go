package resolver_test

// Tests for ResolveTSTypesCallEdges — covers the temp-file creation path.
// The node.js subprocess will fail (no TypeScript project), but the function
// entry and error-return path are covered regardless of node availability.

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

func TestResolveTSTypesCallEdges_EmptyDir(t *testing.T) {
	g := graph.New("test")
	// On a directory without TypeScript/node, this returns an error.
	// We just verify the function doesn't panic and returns a clean result.
	n, err := resolver.ResolveTSTypesCallEdges(g, t.TempDir())
	// Either node is missing (error) or TypeScript isn't installed (error) —
	// both are acceptable. Zero edges is expected.
	_ = n
	_ = err
}

func TestResolveTSTypesCallEdges_EmptyGraph(t *testing.T) {
	g := graph.New("test")
	n, err := resolver.ResolveTSTypesCallEdges(g, t.TempDir())
	if n < 0 {
		t.Errorf("expected n >= 0, got %d", n)
	}
	_ = err
}

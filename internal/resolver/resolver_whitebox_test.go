package resolver

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// White-box tests for unexported resolver helpers.

// TestFindByTypedMethod_Found tests that findByTypedMethod correctly finds
// a method in the index by TypeName.MethodName pattern.
func TestFindByTypedMethod_Found(t *testing.T) {
	// Build a simple index: package -> []*Node
	idx := make(map[string][]*graph.Node)

	node1 := &graph.Node{
		ID:      "id1",
		Name:    "Service.Handle",
		Package: "pkg",
	}
	node2 := &graph.Node{
		ID:      "id2",
		Name:    "Service.Close",
		Package: "pkg",
	}
	node3 := &graph.Node{
		ID:      "id3",
		Name:    "OtherType.Method",
		Package: "other",
	}

	idx["pkg"] = []*graph.Node{node1, node2}
	idx["other"] = []*graph.Node{node3}
	methodIdx := buildMethodIndex(idx)

	// Should find Service.Handle
	id := findByTypedMethod(methodIdx, "Service", "Handle")
	if id != "id1" {
		t.Errorf("expected to find 'Service.Handle', got %q", id)
	}

	// Should find OtherType.Method
	id = findByTypedMethod(methodIdx, "OtherType", "Method")
	if id != "id3" {
		t.Errorf("expected to find 'OtherType.Method', got %q", id)
	}
}

// TestFindByTypedMethod_NotFound tests that findByTypedMethod returns empty
// when the method is not in the index.
func TestFindByTypedMethod_NotFound(t *testing.T) {
	idx := make(map[string][]*graph.Node)

	node1 := &graph.Node{
		ID:      "id1",
		Name:    "Service.Handle",
		Package: "pkg",
	}
	idx["pkg"] = []*graph.Node{node1}
	methodIdx := buildMethodIndex(idx)

	// Should not find NonExistent.Method
	id := findByTypedMethod(methodIdx, "NonExistent", "Method")
	if id != "" {
		t.Errorf("expected empty string for non-existent method, got %q", id)
	}

	// Should not find Service.NonExistent
	id = findByTypedMethod(methodIdx, "Service", "NonExistent")
	if id != "" {
		t.Errorf("expected empty string for non-existent method, got %q", id)
	}
}

// TestFindByTypedMethod_EmptyIndex tests that findByTypedMethod handles
// an empty index gracefully.
func TestFindByTypedMethod_EmptyIndex(t *testing.T) {
	idx := make(map[string][]*graph.Node)
	methodIdx := buildMethodIndex(idx)

	id := findByTypedMethod(methodIdx, "Any", "Method")
	if id != "" {
		t.Errorf("expected empty string for empty index, got %q", id)
	}
}

// TestFindByTypedMethod_MultipleNodesInPackage tests that findByTypedMethod
// correctly finds the right method even when multiple nodes are in a package.
func TestFindByTypedMethod_MultipleNodesInPackage(t *testing.T) {
	idx := make(map[string][]*graph.Node)

	nodes := []*graph.Node{
		{ID: "id1", Name: "First.Method", Package: "pkg"},
		{ID: "id2", Name: "Second.Method", Package: "pkg"},
		{ID: "id3", Name: "Third.Helper", Package: "pkg"},
		{ID: "id4", Name: "First.Other", Package: "pkg"},
	}
	idx["pkg"] = nodes
	methodIdx := buildMethodIndex(idx)

	// Should find the correct method among many
	id := findByTypedMethod(methodIdx, "Second", "Method")
	if id != "id2" {
		t.Errorf("expected id2 for 'Second.Method', got %q", id)
	}

	id = findByTypedMethod(methodIdx, "Third", "Helper")
	if id != "id3" {
		t.Errorf("expected id3 for 'Third.Helper', got %q", id)
	}
}

// TestFindByTypedMethod_PartialNameMatch tests that findByTypedMethod
// does not match partial names (must be exact TypeName.MethodName).
func TestFindByTypedMethod_PartialNameMatch(t *testing.T) {
	idx := make(map[string][]*graph.Node)

	node := &graph.Node{
		ID:      "id1",
		Name:    "Service.Handle",
		Package: "pkg",
	}
	idx["pkg"] = []*graph.Node{node}
	methodIdx := buildMethodIndex(idx)

	// Should not match partial names
	id := findByTypedMethod(methodIdx, "Serv", "Handle")
	if id != "" {
		t.Errorf("expected empty string for partial type name, got %q", id)
	}

	id = findByTypedMethod(methodIdx, "Service", "Hand")
	if id != "" {
		t.Errorf("expected empty string for partial method name, got %q", id)
	}
}


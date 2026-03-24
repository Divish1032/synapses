package graph_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestNodeCountsByDomain_Empty verifies an empty graph returns an empty map
// (not nil, not a map with zero-value entries).
func TestNodeCountsByDomain_Empty(t *testing.T) {
	g := graph.New("test")
	counts := g.NodeCountsByDomain()
	if len(counts) != 0 {
		t.Errorf("expected empty map for empty graph, got %v", counts)
	}
}

// TestNodeCountsByDomain_EmptyDomainNormalized verifies that nodes with an
// empty Domain field are counted under DomainCode, not under an empty-string key.
func TestNodeCountsByDomain_EmptyDomainNormalized(t *testing.T) {
	g := graph.New("test")
	id := g.MakeNodeID("main.go", "main")
	// Domain deliberately left empty (the zero value) — older parsed nodes.
	g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: "main", File: "main.go"})

	counts := g.NodeCountsByDomain()
	if _, hasEmpty := counts[""]; hasEmpty {
		t.Error("empty domain key must not appear in counts — must be normalized to DomainCode")
	}
	if counts[graph.DomainCode] != 1 {
		t.Errorf("expected DomainCode=1 for empty-domain node, got %v", counts)
	}
}

// TestNodeCountsByDomain_MultiDomain verifies each domain is counted correctly
// and the total matches the number of added nodes.
func TestNodeCountsByDomain_MultiDomain(t *testing.T) {
	g := graph.New("test")

	add := func(file, name string, domain graph.DomainType) {
		id := g.MakeNodeID(file, name)
		g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: name, File: file, Domain: domain})
	}

	add("cmd/main.go", "main", graph.DomainCode)
	add("cmd/init.go", "init", graph.DomainCode)
	add("infra/main.tf", "aws_vpc", graph.DomainInfra)
	add("api/spec.yaml", "POST /pay", graph.DomainAPI)
	add("README.md", "intro", graph.DomainDocs)

	counts := g.NodeCountsByDomain()

	if counts[graph.DomainCode] != 2 {
		t.Errorf("expected DomainCode=2, got %d", counts[graph.DomainCode])
	}
	if counts[graph.DomainInfra] != 1 {
		t.Errorf("expected DomainInfra=1, got %d", counts[graph.DomainInfra])
	}
	if counts[graph.DomainAPI] != 1 {
		t.Errorf("expected DomainAPI=1, got %d", counts[graph.DomainAPI])
	}
	if counts[graph.DomainDocs] != 1 {
		t.Errorf("expected DomainDocs=1, got %d", counts[graph.DomainDocs])
	}

	total := 0
	for _, v := range counts {
		total += v
	}
	if total != 5 {
		t.Errorf("expected total=5 nodes, got %d", total)
	}
}

// TestNodeCountsByDomain_MutationConsistency verifies counts update correctly
// after nodes are added and removed.
func TestNodeCountsByDomain_MutationConsistency(t *testing.T) {
	g := graph.New("test")

	idA := g.MakeNodeID("a.go", "A")
	idB := g.MakeNodeID("b.tf", "B")
	g.AddNode(&graph.Node{ID: idA, Type: graph.NodeFunction, Name: "A", File: "a.go", Domain: graph.DomainCode})
	g.AddNode(&graph.Node{ID: idB, Type: graph.NodeFunction, Name: "B", File: "b.tf", Domain: graph.DomainInfra})

	counts := g.NodeCountsByDomain()
	if counts[graph.DomainCode] != 1 || counts[graph.DomainInfra] != 1 {
		t.Fatalf("unexpected initial counts: %v", counts)
	}

	// Remove the infra node — DomainInfra should disappear.
	g.RemoveFile("b.tf")
	counts = g.NodeCountsByDomain()
	if _, present := counts[graph.DomainInfra]; present {
		t.Errorf("DomainInfra must be absent after removing its only node, got %v", counts)
	}
	if counts[graph.DomainCode] != 1 {
		t.Errorf("DomainCode must still be 1 after removing infra node, got %v", counts)
	}
}

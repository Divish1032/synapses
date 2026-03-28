package resolver_test

import (
	"context"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// multiMockEmbedResolver returns different vectors per node name so we can
// control which nodes are "similar" to each other.
type multiMockEmbedResolver struct {
	vectors map[string][]float32 // name → vector
	nodes   map[string]float64   // nodeID → score (returned by SearchByVector)
}

func (m *multiMockEmbedResolver) EmbedText(_ context.Context, text string) ([]float32, error) {
	// Return a vector based on the first word (the node name).
	for name, vec := range m.vectors {
		if len(text) >= len(name) && text[:len(name)] == name {
			return vec, nil
		}
	}
	return []float32{0, 0, 0}, nil
}

func (m *multiMockEmbedResolver) SearchByVector(_ []float32, _ int) []resolver.EmbedMatch {
	var results []resolver.EmbedMatch
	for id, score := range m.nodes {
		results = append(results, resolver.EmbedMatch{NodeID: id, Score: score})
	}
	return results
}

func TestDiscoverEmbedRelations_CreatesEdge(t *testing.T) {
	g := graph.New("test")

	// Create two knowledge nodes.
	idA := g.MakeNodeID("_knowledge", "knowledge:rate limiter")
	idB := g.MakeNodeID("_knowledge", "knowledge:token bucket")
	g.AddNode(&graph.Node{
		ID: idA, Type: graph.NodeConcept, Name: "rate limiter",
		Domain:   graph.DomainKnowledge,
		Metadata: map[string]string{"context": "controls throughput"},
	})
	g.AddNode(&graph.Node{
		ID: idB, Type: graph.NodeConcept, Name: "token bucket",
		Domain:   graph.DomainKnowledge,
		Metadata: map[string]string{"context": "algorithm for rate limiting"},
	})

	// Mock resolver: both nodes return high similarity to each other.
	er := &multiMockEmbedResolver{
		vectors: map[string][]float32{
			"rate limiter": {1, 0, 0},
			"token bucket": {0, 1, 0},
		},
		nodes: map[string]float64{
			string(idA): 0.75,
			string(idB): 0.75,
		},
	}

	created := resolver.DiscoverEmbedRelations(g, er, 0.5)

	if created == 0 {
		t.Error("expected at least one edge to be created")
	}

	// Check that RELATES_TO edge exists between the two nodes.
	edges := g.OutEdges(idA)
	found := false
	for _, e := range edges {
		if e.To == idB && e.Type == graph.EdgeRelatesTo {
			found = true
		}
	}
	if !found {
		t.Error("expected RELATES_TO edge from rate limiter to token bucket")
	}
}

func TestDiscoverEmbedRelations_SkipsBelowThreshold(t *testing.T) {
	g := graph.New("test")

	idA := g.MakeNodeID("_knowledge", "knowledge:redis")
	idB := g.MakeNodeID("_knowledge", "knowledge:postgresql")
	g.AddNode(&graph.Node{
		ID: idA, Type: graph.NodeConcept, Name: "redis",
		Domain: graph.DomainKnowledge,
	})
	g.AddNode(&graph.Node{
		ID: idB, Type: graph.NodeConcept, Name: "postgresql",
		Domain: graph.DomainKnowledge,
	})

	er := &multiMockEmbedResolver{
		vectors: map[string][]float32{
			"redis":      {1, 0, 0},
			"postgresql": {0, 1, 0},
		},
		nodes: map[string]float64{
			string(idA): 0.30, // below threshold
			string(idB): 0.30,
		},
	}

	created := resolver.DiscoverEmbedRelations(g, er, 0.5)
	if created != 0 {
		t.Errorf("expected 0 edges for below-threshold scores, got %d", created)
	}
}

func TestDiscoverEmbedRelations_NilResolver(t *testing.T) {
	g := graph.New("test")
	created := resolver.DiscoverEmbedRelations(g, nil, 0.5)
	if created != 0 {
		t.Errorf("expected 0 for nil resolver, got %d", created)
	}
}

func TestDiscoverEmbedRelations_SingleNode(t *testing.T) {
	g := graph.New("test")
	id := g.MakeNodeID("_knowledge", "knowledge:redis")
	g.AddNode(&graph.Node{
		ID: id, Type: graph.NodeConcept, Name: "redis",
		Domain: graph.DomainKnowledge,
	})

	er := &multiMockEmbedResolver{
		vectors: map[string][]float32{"redis": {1, 0, 0}},
		nodes:   map[string]float64{string(id): 0.99},
	}

	created := resolver.DiscoverEmbedRelations(g, er, 0.5)
	if created != 0 {
		t.Errorf("expected 0 edges for single node (no self-loops), got %d", created)
	}
}

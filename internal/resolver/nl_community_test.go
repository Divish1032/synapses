package resolver_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

func TestDetectCommunities_TwoClusters(t *testing.T) {
	g := graph.New("test")

	// Cluster 1: rate-limiter and token-bucket (connected).
	idA := g.MakeNodeID("_knowledge", "knowledge:rate limiter")
	idB := g.MakeNodeID("_knowledge", "knowledge:token bucket")
	g.AddNode(&graph.Node{ID: idA, Type: graph.NodeConcept, Name: "rate limiter", Domain: graph.DomainKnowledge, Metadata: map[string]string{}})
	g.AddNode(&graph.Node{ID: idB, Type: graph.NodeConcept, Name: "token bucket", Domain: graph.DomainKnowledge, Metadata: map[string]string{}})
	g.AddEdge(&graph.Edge{From: idA, To: idB, Type: graph.EdgeRelatesTo})

	// Cluster 2: redis and memcached (connected).
	idC := g.MakeNodeID("_knowledge", "knowledge:redis")
	idD := g.MakeNodeID("_knowledge", "knowledge:memcached")
	g.AddNode(&graph.Node{ID: idC, Type: graph.NodeConcept, Name: "redis", Domain: graph.DomainKnowledge, Metadata: map[string]string{}})
	g.AddNode(&graph.Node{ID: idD, Type: graph.NodeConcept, Name: "memcached", Domain: graph.DomainKnowledge, Metadata: map[string]string{}})
	g.AddEdge(&graph.Edge{From: idC, To: idD, Type: graph.EdgeRelatesTo})

	communities := resolver.DetectCommunities(g, 10)

	if communities != 2 {
		t.Errorf("expected 2 communities, got %d", communities)
	}

	// Nodes in same cluster should have same community ID.
	nA := g.GetNode(idA)
	nB := g.GetNode(idB)
	if nA.Metadata["community"] != nB.Metadata["community"] {
		t.Errorf("rate limiter and token bucket should be in same community, got %s and %s",
			nA.Metadata["community"], nB.Metadata["community"])
	}

	nC := g.GetNode(idC)
	nD := g.GetNode(idD)
	if nC.Metadata["community"] != nD.Metadata["community"] {
		t.Errorf("redis and memcached should be in same community, got %s and %s",
			nC.Metadata["community"], nD.Metadata["community"])
	}

	// Different clusters should have different community IDs.
	if nA.Metadata["community"] == nC.Metadata["community"] {
		t.Error("rate limiter cluster and redis cluster should have different community IDs")
	}
}

func TestDetectCommunities_NoNodes(t *testing.T) {
	g := graph.New("test")
	communities := resolver.DetectCommunities(g, 10)
	if communities != 0 {
		t.Errorf("expected 0 communities for empty graph, got %d", communities)
	}
}

func TestDetectCommunities_IsolatedNodes(t *testing.T) {
	g := graph.New("test")

	idA := g.MakeNodeID("_knowledge", "knowledge:redis")
	idB := g.MakeNodeID("_knowledge", "knowledge:kafka")
	g.AddNode(&graph.Node{ID: idA, Type: graph.NodeConcept, Name: "redis", Domain: graph.DomainKnowledge, Metadata: map[string]string{}})
	g.AddNode(&graph.Node{ID: idB, Type: graph.NodeConcept, Name: "kafka", Domain: graph.DomainKnowledge, Metadata: map[string]string{}})
	// No edges between them.

	communities := resolver.DetectCommunities(g, 10)

	// Each isolated node is its own community.
	if communities != 2 {
		t.Errorf("expected 2 communities for 2 isolated nodes, got %d", communities)
	}

	nA := g.GetNode(idA)
	nB := g.GetNode(idB)
	if nA.Metadata["community"] == nB.Metadata["community"] {
		t.Error("isolated nodes should have different community IDs")
	}
}

func TestDetectCommunities_WritesMetadata(t *testing.T) {
	g := graph.New("test")

	id := g.MakeNodeID("_knowledge", "knowledge:redis")
	g.AddNode(&graph.Node{ID: id, Type: graph.NodeConcept, Name: "redis", Domain: graph.DomainKnowledge, Metadata: map[string]string{}})

	resolver.DetectCommunities(g, 10)

	n := g.GetNode(id)
	if n.Metadata["community"] == "" {
		t.Error("expected community metadata to be set")
	}
}

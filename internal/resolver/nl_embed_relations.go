// Package resolver — nl_embed_relations.go discovers relationships between
// knowledge nodes using embedding similarity.
//
// After NL entity extraction creates knowledge nodes (NodeConcept, NodeEntity,
// NodeArtifact, NodeDecision), this pass runs over them and wires RELATES_TO
// edges between nodes that are semantically similar — even if no keyword-based
// relationship signal was found in the text.
//
// Pipeline position: runs AFTER ResolveNLEntities and AFTER the node embedding
// pass has populated vectors for knowledge nodes.
package resolver

import (
	"context"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// embedRelThreshold is the minimum cosine similarity between two knowledge
// nodes for an automatic RELATES_TO edge to be created.
const embedRelThreshold = 0.55

// embedRelTimeout is the per-node budget for embedding calls.
const embedRelTimeout = 2 * time.Second

// DiscoverEmbedRelations finds semantically similar pairs of knowledge nodes
// and wires RELATES_TO edges between them. Returns the number of edges created.
//
// For each knowledge node, it embeds the node's name+context, searches HNSW
// for similar knowledge nodes, and creates edges for pairs above threshold.
// Skips self-loops and duplicate edges (AddEdge is idempotent).
//
// er must be non-nil; callers should guard before calling.
func DiscoverEmbedRelations(g *graph.Graph, er EmbedResolver, threshold float64) int {
	if er == nil {
		return 0
	}
	if threshold <= 0 {
		threshold = embedRelThreshold
	}

	knowledgeTypes := []graph.NodeType{
		graph.NodeConcept, graph.NodeEntity,
		graph.NodeArtifact, graph.NodeDecision,
	}

	// Collect all knowledge nodes.
	var nodes []*graph.Node
	for _, nt := range knowledgeTypes {
		nodes = append(nodes, g.FindByType(nt)...)
	}
	if len(nodes) < 2 {
		return 0
	}

	// Build a set of knowledge node IDs for fast membership check.
	knowledgeIDs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		knowledgeIDs[string(n.ID)] = true
	}

	// Build existing edge set to avoid creating duplicates (even though
	// AddEdge is idempotent, we skip the embed call entirely).
	existingEdges := make(map[[2]string]bool)
	for _, n := range nodes {
		for _, e := range g.OutEdges(n.ID) {
			if knowledgeIDs[string(e.To)] {
				existingEdges[[2]string{string(e.From), string(e.To)}] = true
				existingEdges[[2]string{string(e.To), string(e.From)}] = true
			}
		}
	}

	created := 0
	for _, n := range nodes {
		embedText := n.Name
		if ctx, ok := n.Metadata["context"]; ok && ctx != "" {
			embedText = n.Name + " " + truncateContext(ctx, 100)
		}

		ctx, cancel := context.WithTimeout(context.Background(), embedRelTimeout)
		vec, err := er.EmbedText(ctx, embedText)
		cancel()
		if err != nil || len(vec) == 0 {
			continue
		}

		matches := er.SearchByVector(vec, 10)
		for _, m := range matches {
			// Skip self.
			if m.NodeID == string(n.ID) {
				continue
			}
			// Only link to other knowledge nodes.
			if !knowledgeIDs[m.NodeID] {
				continue
			}
			// Check threshold.
			if m.Score < threshold {
				continue
			}
			// Skip if edge already exists in either direction.
			pair := [2]string{string(n.ID), m.NodeID}
			if existingEdges[pair] {
				continue
			}

			g.AddEdge(&graph.Edge{
				From: n.ID,
				To:   graph.NodeID(m.NodeID),
				Type: graph.EdgeRelatesTo,
			})
			existingEdges[pair] = true
			existingEdges[[2]string{m.NodeID, string(n.ID)}] = true
			created++
		}
	}

	return created
}

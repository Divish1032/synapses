// Package resolver — nl_community.go implements topic clustering for knowledge nodes
// using label propagation.
//
// After NL entity extraction and optional embedding-based relationship discovery,
// this pass groups related knowledge nodes into communities. Each node gets a
// "community" metadata key containing its community label.
//
// Label propagation is a simple, fast, pure-Go algorithm that doesn't require
// any external dependencies. It converges in O(iterations × edges) time.
//
// Pipeline position: runs AFTER ResolveNLEntities and DiscoverEmbedRelations.
package resolver

import (
	"sort"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// knowledgeEdgeTypes is the set of edge types considered for community detection.
// These are all the NL-domain relationship types.
var knowledgeEdgeTypes = map[graph.EdgeType]bool{
	graph.EdgeRelatesTo:   true,
	graph.EdgeCausedBy:    true,
	graph.EdgeInstanceOf:  true,
	graph.EdgeContradicts: true,
}

// DetectCommunities runs label propagation over knowledge nodes and writes
// the community label into each node's Metadata["community"]. Returns the
// number of distinct communities found.
//
// maxIterations caps the propagation rounds. 10 is usually enough; the
// algorithm converges early when no labels change.
func DetectCommunities(g *graph.Graph, maxIterations int) int {
	if maxIterations <= 0 {
		maxIterations = 10
	}

	knowledgeTypes := []graph.NodeType{
		graph.NodeConcept, graph.NodeEntity,
		graph.NodeArtifact, graph.NodeDecision,
	}

	// Collect all knowledge nodes.
	var nodeIDs []graph.NodeID
	nodeSet := make(map[graph.NodeID]bool)
	for _, nt := range knowledgeTypes {
		for _, n := range g.FindByType(nt) {
			if !nodeSet[n.ID] {
				nodeIDs = append(nodeIDs, n.ID)
				nodeSet[n.ID] = true
			}
		}
	}
	if len(nodeIDs) == 0 {
		return 0
	}

	// Sort for deterministic iteration order (instead of random shuffle,
	// which would require a seed for reproducibility).
	sort.Slice(nodeIDs, func(i, j int) bool {
		return string(nodeIDs[i]) < string(nodeIDs[j])
	})

	// Build adjacency list from knowledge edges.
	adj := make(map[graph.NodeID][]graph.NodeID, len(nodeIDs))
	for _, nid := range nodeIDs {
		for _, e := range g.OutEdges(nid) {
			if knowledgeEdgeTypes[e.Type] && nodeSet[e.To] {
				adj[nid] = append(adj[nid], e.To)
			}
		}
		// Also add reverse edges (make graph undirected for community detection).
		for _, e := range g.InEdges(nid) {
			if knowledgeEdgeTypes[e.Type] && nodeSet[e.From] {
				adj[nid] = append(adj[nid], e.From)
			}
		}
	}

	// Initialize: each node's label = its own ID string.
	labels := make(map[graph.NodeID]string, len(nodeIDs))
	for _, nid := range nodeIDs {
		labels[nid] = string(nid)
	}

	// Label propagation iterations.
	for iter := 0; iter < maxIterations; iter++ {
		changed := false
		for _, nid := range nodeIDs {
			neighbors := adj[nid]
			if len(neighbors) == 0 {
				continue
			}

			// Count neighbor label frequencies.
			freq := make(map[string]int)
			for _, nb := range neighbors {
				freq[labels[nb]]++
			}

			// Find the most frequent label. Break ties by smallest label
			// for determinism.
			bestLabel := labels[nid]
			bestCount := 0
			for lbl, cnt := range freq {
				if cnt > bestCount || (cnt == bestCount && lbl < bestLabel) {
					bestLabel = lbl
					bestCount = cnt
				}
			}

			if bestLabel != labels[nid] {
				labels[nid] = bestLabel
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	// Write community labels to node metadata.
	// Assign sequential community IDs (0, 1, 2, ...) for readability.
	labelToID := make(map[string]string)
	nextID := 0
	for _, nid := range nodeIDs {
		lbl := labels[nid]
		if _, ok := labelToID[lbl]; !ok {
			labelToID[lbl] = itoa(nextID)
			nextID++
		}
	}

	for _, nid := range nodeIDs {
		n := g.GetNode(nid)
		if n == nil {
			continue
		}
		if n.Metadata == nil {
			n.Metadata = make(map[string]string)
		}
		n.Metadata["community"] = labelToID[labels[nid]]
	}

	return nextID
}

// itoa converts an int to a string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

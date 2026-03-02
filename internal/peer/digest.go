package peer

import (
	"crypto/sha256"
	"fmt"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ComputeIntersection returns the names of exported entities that appear in
// BOTH the local graph and the peer digest (matched by sig_hash). This lets
// the PeerManager quickly determine peer relevance without a full context query.
// Time complexity: O(n + m) where n = local exported nodes, m = peer digest entries.
func ComputeIntersection(localGraph *graph.Graph, peerDigest []DigestEntry) []string {
	// Build a set of local exported sig-hashes.
	localHashes := make(map[string]string) // sigHash → name
	for _, n := range localGraph.AllNodes() {
		if !n.Exported {
			continue
		}
		switch n.Type {
		case graph.NodeFunction, graph.NodeMethod, graph.NodeStruct, graph.NodeInterface:
		default:
			continue
		}
		sig := ""
		if n.Metadata != nil {
			sig = n.Metadata["signature"]
		}
		h := sha256.Sum256([]byte(sig))
		localHashes[fmt.Sprintf("%x", h[:8])] = n.Name
	}

	// Find peer digest entries whose sig_hash matches a local one.
	var shared []string
	seen := make(map[string]bool)
	for _, e := range peerDigest {
		if name, ok := localHashes[e.SigHash]; ok && !seen[name] {
			shared = append(shared, name)
			seen[name] = true
		}
	}
	return shared
}

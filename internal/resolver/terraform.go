package resolver

import (
	"log"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ResolveTerraformRefs drains all pending TerraformRefs from the graph and
// creates DEPENDS_ON edges between resource nodes. Returns the number of edges
// created.
//
// This enables cross-file Terraform dependency resolution: a resource defined
// in vpc.tf can have a DEPENDS_ON edge to a resource defined in compute.tf.
//
// Must be called after all .tf files have been parsed (i.e., after WalkDir).
func ResolveTerraformRefs(g *graph.Graph) int {
	refs := g.DrainTerraformRefs()
	if len(refs) == 0 {
		return 0
	}

	// Build a lookup index: resource ref name → NodeID.
	// We scan all nodes with domain=infra and kind in {resource, data, module}.
	refIndex := buildTerraformRefIndex(g)

	resolved := 0
	for _, ref := range refs {
		targetID, ok := refIndex[ref.RefName]
		if !ok {
			continue // cross-file ref to an unknown resource; skip
		}
		if targetID == ref.FromID {
			continue // self-reference guard
		}
		if g.HasEdge(ref.FromID, targetID, graph.EdgeDependsOn) {
			continue // already exists (e.g. emitted by a previous incremental pass)
		}
		g.AddEdge(&graph.Edge{
			From: ref.FromID,
			To:   targetID,
			Type: graph.EdgeDependsOn,
		})
		resolved++
	}
	// After the main loop, detect and break DEPENDS_ON cycles.
	// Terraform rejects circular depends_on at plan time, so cycles in our
	// graph indicate a spurious reference. Breaking them prevents PPR score
	// amplification.
	breakDependsOnCycles(g)

	return resolved
}

// buildTerraformRefIndex scans all graph nodes and returns a map from the
// canonical Terraform reference name to the node's NodeID. Only nodes with
// domain=infra are included; non-infra nodes are skipped for efficiency.
//
// Key format matches TerraformRef.RefName:
//   - "aws_instance.web"       for resource nodes
//   - "data.aws_ami.ubuntu"    for data source nodes
//   - "module.vpc"             for module nodes
func buildTerraformRefIndex(g *graph.Graph) map[string]graph.NodeID {
	nodes := g.AllNodesUnsorted()
	index := make(map[string]graph.NodeID, len(nodes)/4)
	for _, n := range nodes {
		if n.Domain != graph.DomainInfra {
			continue
		}
		kind := n.Metadata["kind"]
		switch kind {
		case "resource", "data", "module":
			// Node.Name already encodes the canonical ref name.
			index[n.Name] = n.ID
		}
	}
	return index
}

// breakDependsOnCycles detects and removes back-edges that create cycles in the
// DEPENDS_ON subgraph using iterative DFS. Returns the number of removed edges.
func breakDependsOnCycles(g *graph.Graph) int {
	// Build adjacency list from DEPENDS_ON edges only.
	adj := make(map[graph.NodeID][]graph.NodeID)
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeDependsOn {
			adj[e.From] = append(adj[e.From], e.To)
		}
	}
	if len(adj) == 0 {
		return 0
	}

	// Collect all nodes that participate in DEPENDS_ON edges.
	nodeSet := make(map[graph.NodeID]struct{})
	for from, tos := range adj {
		nodeSet[from] = struct{}{}
		for _, to := range tos {
			nodeSet[to] = struct{}{}
		}
	}

	// DFS state.
	const (
		white = 0 // unvisited
		gray  = 1 // in current DFS stack
		black = 2 // fully processed
	)
	color := make(map[graph.NodeID]int, len(nodeSet))

	type frame struct {
		node graph.NodeID
		idx  int // next neighbor index to visit
	}

	removed := 0

	for root := range nodeSet {
		if color[root] != white {
			continue
		}
		stack := []frame{{node: root, idx: 0}}
		color[root] = gray

		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			neighbors := adj[top.node]

			if top.idx >= len(neighbors) {
				// Done with this node.
				color[top.node] = black
				stack = stack[:len(stack)-1]
				continue
			}

			next := neighbors[top.idx]
			top.idx++

			switch color[next] {
			case white:
				color[next] = gray
				stack = append(stack, frame{node: next, idx: 0})
			case gray:
				// Back-edge: top.node → next creates a cycle. Remove it.
				g.RemoveEdge(top.node, next, graph.EdgeDependsOn)
				log.Printf("synapses: removed cyclic DEPENDS_ON edge %v → %v", top.node, next)
				removed++
				// black: cross/forward edge, ignore.
			}
		}
	}
	return removed
}

package resolver

import (
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
	nodes := g.AllNodes()
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

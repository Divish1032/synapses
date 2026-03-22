package resolver

import (
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ResolveHeritageEdges creates IMPLEMENTS edges from explicit heritage clauses
// (implements/extends) extracted during parsing of nominally-typed languages
// (TypeScript, Java, C#, Kotlin). These edges are based on explicit source
// declarations and are always correct — no structural heuristic needed.
//
// Returns the number of new IMPLEMENTS edges added.
func ResolveHeritageEdges(g *graph.Graph) int {
	nodes := g.AllNodes()

	// Build name → []NodeID index for all interface and struct nodes.
	// Multiple nodes may share the same name (different packages/files).
	type nodeRef struct {
		id  graph.NodeID
		pkg string
	}
	nameIndex := make(map[string][]nodeRef)
	for _, n := range nodes {
		if n.Type == graph.NodeInterface || n.Type == graph.NodeStruct {
			nameIndex[n.Name] = append(nameIndex[n.Name], nodeRef{id: n.ID, pkg: n.Package})
		}
	}

	// Track edges added in this batch. Existing edges checked via g.HasEdge.
	seen := make(map[string]bool)

	count := 0
	for _, n := range nodes {
		if n.Type != graph.NodeStruct {
			continue
		}

		// Combine heritage_implements and heritage_extends — both create IMPLEMENTS edges.
		var heritageNames []string
		if hi := n.Metadata["heritage_implements"]; hi != "" {
			heritageNames = append(heritageNames, strings.Split(hi, ",")...)
		}
		if he := n.Metadata["heritage_extends"]; he != "" {
			heritageNames = append(heritageNames, strings.Split(he, ",")...)
		}
		if len(heritageNames) == 0 {
			continue
		}

		for _, targetName := range heritageNames {
			targetName = strings.TrimSpace(targetName)
			if targetName == "" {
				continue
			}

			candidates := nameIndex[targetName]
			if len(candidates) == 0 {
				continue
			}

			// Prefer same-package match. If no same-package match, use first.
			var targetID graph.NodeID
			for _, c := range candidates {
				if c.pkg == n.Package {
					targetID = c.id
					break
				}
			}
			if targetID == "" {
				targetID = candidates[0].id
			}

			// Don't self-implement.
			if targetID == n.ID {
				continue
			}

			edgeKey := string(n.ID) + "->" + string(targetID)
			if seen[edgeKey] || g.HasEdge(n.ID, targetID, graph.EdgeImplements) {
				continue
			}
			seen[edgeKey] = true
			g.AddEdge(&graph.Edge{
				From: n.ID,
				To:   targetID,
				Type: graph.EdgeImplements,
			})
			count++
		}
	}
	return count
}

// ResolveImplementsEdges detects which structs satisfy which interfaces using
// a same-package structural heuristic: if a struct defines all methods listed
// in an interface's "methods" metadata, an IMPLEMENTS edge is added from the
// struct node to the interface node.
//
// Structs with "heritage_implements" or "heritage_extends" metadata are SKIPPED
// — they use nominal typing (TypeScript, Java, C#, Kotlin) and their IMPLEMENTS
// edges are resolved by ResolveHeritageEdges instead. Structural matching
// produces false positives for nominal type systems.
//
// This is an approximation. It only matches same-package pairs — cross-package
// interface satisfaction requires full type inference (go/types) which is not
// available here. It covers the dominant Go pattern where service types and
// their interfaces live in the same package.
//
// Returns the number of new IMPLEMENTS edges added.
func ResolveImplementsEdges(g *graph.Graph) int {
	nodes := g.AllNodes()

	// 1. Collect interfaces with required method sets (from "methods" metadata).
	type ifaceInfo struct {
		nodeID  graph.NodeID
		methods map[string]bool
	}
	// Key: "pkg::IfaceName"
	ifaces := make(map[string]ifaceInfo)
	for _, n := range nodes {
		if n.Type != graph.NodeInterface {
			continue
		}
		methodsStr := n.Metadata["methods"]
		if methodsStr == "" {
			continue
		}
		required := make(map[string]bool)
		for _, m := range strings.Split(methodsStr, ",") {
			if m = strings.TrimSpace(m); m != "" {
				required[m] = true
			}
		}
		if len(required) > 0 {
			ifaces[n.Package+"::"+n.Name] = ifaceInfo{nodeID: n.ID, methods: required}
		}
	}
	if len(ifaces) == 0 {
		return 0
	}

	// 2. Collect concrete method names per (pkg, receiverType) pair.
	//    Method node names have the form "ReceiverType.MethodName".
	//    Key: "pkg::ReceiverType" → set of method names
	//    Skip methods belonging to heritage-tagged structs (nominal typing).
	heritageStructs := make(map[string]bool)
	structMethods := make(map[string]map[string]bool)
	for _, n := range nodes {
		if n.Type == graph.NodeStruct {
			if n.Metadata["heritage_implements"] != "" || n.Metadata["heritage_extends"] != "" {
				heritageStructs[n.Package+"::"+n.Name] = true
			}
		}
		if n.Type != graph.NodeMethod {
			continue
		}
		dot := strings.IndexByte(n.Name, '.')
		if dot < 0 {
			continue
		}
		key := n.Package + "::" + n.Name[:dot]
		if structMethods[key] == nil {
			structMethods[key] = make(map[string]bool)
		}
		structMethods[key][n.Name[dot+1:]] = true
	}

	// 3. Map "pkg::StructName" → NodeID for struct nodes.
	structIDs := make(map[string]graph.NodeID)
	for _, n := range nodes {
		if n.Type == graph.NodeStruct {
			structIDs[n.Package+"::"+n.Name] = n.ID
		}
	}

	// 4. Track edges added in this batch. Existing edges checked via g.HasEdge.
	seen := make(map[string]bool)

	// 5. Match structs against interfaces in the same package.
	//    Skip structs with heritage metadata — they use nominal typing.
	count := 0
	for ifaceKey, iface := range ifaces {
		sepIdx := strings.Index(ifaceKey, "::")
		if sepIdx < 0 {
			continue
		}
		pkg := ifaceKey[:sepIdx]
		prefix := pkg + "::"
		for structKey, methods := range structMethods {
			if !strings.HasPrefix(structKey, prefix) {
				continue
			}
			// Skip heritage-tagged structs — nominal typing, not structural.
			if heritageStructs[structKey] {
				continue
			}
			// All required interface methods must be present on the struct.
			allPresent := true
			for m := range iface.methods {
				if !methods[m] {
					allPresent = false
					break
				}
			}
			if !allPresent {
				continue
			}
			sid, ok := structIDs[structKey]
			if !ok {
				continue
			}
			edgeKey := string(sid) + "->" + string(iface.nodeID)
			if seen[edgeKey] || g.HasEdge(sid, iface.nodeID, graph.EdgeImplements) {
				continue
			}
			seen[edgeKey] = true
			g.AddEdge(&graph.Edge{
				From: sid,
				To:   iface.nodeID,
				Type: graph.EdgeImplements,
			})
			count++
		}
	}
	return count
}

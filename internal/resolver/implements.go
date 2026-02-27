package resolver

import (
	"strings"

	"github.com/Divish1032/synapses/internal/graph"
)

// ResolveImplementsEdges detects which structs satisfy which interfaces using
// a same-package structural heuristic: if a struct defines all methods listed
// in an interface's "methods" metadata, an IMPLEMENTS edge is added from the
// struct node to the interface node.
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
	structMethods := make(map[string]map[string]bool)
	for _, n := range nodes {
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

	// 4. Avoid duplicating IMPLEMENTS edges that already exist.
	seen := make(map[string]bool)
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeImplements {
			seen[string(e.From)+"->"+string(e.To)] = true
		}
	}

	// 5. Match structs against interfaces in the same package.
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
			if seen[edgeKey] {
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

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
// RTA filtering is intentionally NOT applied here. Heritage clauses are nominal
// type declarations: if a class says "implements Runnable", that relationship is
// structurally true regardless of whether the class is instantiated. Filtering by
// instantiation would break abstract base class chains (e.g. AbstractBase
// implements Service, ConcreteImpl extends AbstractBase — filtering AbstractBase
// drops the Service edge and breaks transitive hierarchy traversal). The Go
// structural heuristic (ResolveImplementsEdges) is where RTA filtering is
// valuable because it may over-match; nominal declarations cannot over-match.
//
// Returns the number of new IMPLEMENTS edges added.
func ResolveHeritageEdges(g *graph.Graph) int {
	nodes := g.AllNodesUnsorted()

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
	// RTA: when instantiation data is available, skip structs never constructed.
	// For pure Go projects, GetInstantiatedTypes returns nil and this is a no-op.
	instantiated := g.GetInstantiatedTypes()

	// ── Single-pass node collection ─────────────────────────────────────
	// Collect all required data in one IterateNodes pass instead of 3+
	// separate AllNodes() calls (each of which allocates + sorts).
	type ifaceInfo struct {
		nodeID  graph.NodeID
		methods map[string]bool
	}

	// Interfaces grouped by package: pkg → ifaceName → ifaceInfo
	ifacesByPkg := make(map[string]map[string]ifaceInfo)

	// Struct method sets grouped by package: pkg → structName → methodSet
	structMethodsByPkg := make(map[string]map[string]map[string]bool)

	// Struct node IDs grouped by package: pkg → structName → NodeID
	structIDsByPkg := make(map[string]map[string]graph.NodeID)

	// Heritage-tagged structs (nominal typing): pkg → structName → true
	heritageByPkg := make(map[string]map[string]bool)

	// For nominal typing (step 6): interface name → NodeID
	ifaceByName := make(map[string]graph.NodeID)

	// Heritage structs needing nominal resolution: collected for step 6
	type heritageStruct struct {
		id       graph.NodeID
		heritage string
	}
	var heritageStructs []heritageStruct

	g.IterateNodes(func(n *graph.Node) {
		switch n.Type {
		case graph.NodeInterface:
			// Collect for nominal lookup (step 6).
			ifaceByName[n.Name] = n.ID
			if dot := strings.LastIndexByte(n.Name, '.'); dot >= 0 {
				ifaceByName[n.Name[dot+1:]] = n.ID
			}
			// Collect interfaces with required method sets.
			methodsStr := n.Metadata["methods"]
			if methodsStr == "" {
				return
			}
			required := make(map[string]bool)
			for _, m := range strings.Split(methodsStr, ",") {
				if m = strings.TrimSpace(m); m != "" {
					required[m] = true
				}
			}
			if len(required) == 0 {
				return
			}
			pkg := n.Package
			if ifacesByPkg[pkg] == nil {
				ifacesByPkg[pkg] = make(map[string]ifaceInfo)
			}
			ifacesByPkg[pkg][n.Name] = ifaceInfo{nodeID: n.ID, methods: required}

		case graph.NodeStruct:
			pkg := n.Package
			// Track struct IDs by package.
			if structIDsByPkg[pkg] == nil {
				structIDsByPkg[pkg] = make(map[string]graph.NodeID)
			}
			structIDsByPkg[pkg][n.Name] = n.ID
			// Track heritage-tagged structs.
			if n.Metadata["heritage_implements"] != "" || n.Metadata["heritage_extends"] != "" {
				if heritageByPkg[pkg] == nil {
					heritageByPkg[pkg] = make(map[string]bool)
				}
				heritageByPkg[pkg][n.Name] = true
			}
			// Collect heritage structs for nominal resolution (step 6).
			if hi := n.Metadata["heritage_implements"]; hi != "" {
				heritageStructs = append(heritageStructs, heritageStruct{id: n.ID, heritage: hi})
			}

		case graph.NodeMethod:
			dot := strings.IndexByte(n.Name, '.')
			if dot < 0 {
				return
			}
			pkg := n.Package
			receiverType := n.Name[:dot]
			methodName := n.Name[dot+1:]
			if structMethodsByPkg[pkg] == nil {
				structMethodsByPkg[pkg] = make(map[string]map[string]bool)
			}
			if structMethodsByPkg[pkg][receiverType] == nil {
				structMethodsByPkg[pkg][receiverType] = make(map[string]bool)
			}
			structMethodsByPkg[pkg][receiverType][methodName] = true
		}
	})

	if len(ifacesByPkg) == 0 {
		return 0
	}

	// ── Structural matching: same-package interfaces vs structs ──────────
	// For each package that has interfaces, match against structs in that
	// same package. This is O(I_pkg × S_pkg) per package, which is O(n)
	// overall since packages have bounded size.
	seen := make(map[string]bool)
	count := 0

	for pkg, pkgIfaces := range ifacesByPkg {
		pkgStructMethods := structMethodsByPkg[pkg]
		if len(pkgStructMethods) == 0 {
			continue
		}
		pkgStructIDs := structIDsByPkg[pkg]
		pkgHeritage := heritageByPkg[pkg]

		for _, iface := range pkgIfaces {
			for structName, methods := range pkgStructMethods {
				// Skip heritage-tagged structs — nominal typing, not structural.
				if pkgHeritage[structName] {
					continue
				}
				// RTA filter: skip structs never instantiated.
				if len(instantiated) > 0 && !instantiated[structName] {
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
				sid, ok := pkgStructIDs[structName]
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
	}

	// ── Nominal typing: Java/TS "implements InterfaceName" ───────────────
	// Heritage-tagged structs directly resolve interface names without
	// structural matching.
	for _, hs := range heritageStructs {
		for _, ifaceName := range strings.Split(hs.heritage, ",") {
			ifaceName = strings.TrimSpace(ifaceName)
			if ifaceName == "" {
				continue
			}
			ifaceID, ok := ifaceByName[ifaceName]
			if !ok {
				if dot := strings.LastIndexByte(ifaceName, '.'); dot >= 0 {
					ifaceID, ok = ifaceByName[ifaceName[dot+1:]]
				}
			}
			if !ok {
				continue
			}
			edgeKey := string(hs.id) + "->" + string(ifaceID)
			if seen[edgeKey] || g.HasEdge(hs.id, ifaceID, graph.EdgeImplements) {
				continue
			}
			seen[edgeKey] = true
			g.AddEdge(&graph.Edge{From: hs.id, To: ifaceID, Type: graph.EdgeImplements})
			count++
		}
	}

	return count
}

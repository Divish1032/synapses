package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// handleGetRepoMap returns a navigable text overview of the repository,
// grouped by package and sorted by connectivity. Two detail levels:
//
//   - compact (~500 tokens): top 3 entities per package
//   - full    (~2000 tokens): top 10 entities per package
//
// Results are stored in a dedicated orientation cache (not the shared 20-slot
// packet cache) so heavy get_context traffic cannot evict them.
func (s *Server) handleGetRepoMap(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	detail := stringArg(req, "detail")
	if detail == "" {
		detail = "compact"
	}

	// Read from dedicated orient cache — never evicted by LRU packet cache.
	s.orientMu.RLock()
	if detail == "full" {
		if s.orientRepoFull != nil {
			cached := *s.orientRepoFull
			s.orientMu.RUnlock()
			return mcp.NewToolResultText(cached), nil
		}
	} else {
		if s.orientRepoCompact != nil {
			cached := *s.orientRepoCompact
			s.orientMu.RUnlock()
			return mcp.NewToolResultText(cached), nil
		}
	}
	s.orientMu.RUnlock()

	nodes := s.graph.AllNodes()
	edges := s.graph.AllEdges()
	repoRoot := s.graph.Root()

	// Use connectivity map that excludes DEFINES/IMPORTS edges — same metric
	// as explain_codebase so scores are consistent across tools.
	refs := connectivityMap(edges)

	topN := 3
	if detail == "full" {
		topN = 10
	}

	result := buildRepoMap(nodes, refs, repoRoot, topN)

	s.orientMu.Lock()
	if detail == "full" {
		s.orientRepoFull = &result
	} else {
		s.orientRepoCompact = &result
	}
	s.orientMu.Unlock()

	return mcp.NewToolResultText(result), nil
}

// repoMapPackage holds one package's top entities for the map.
type repoMapPackage struct {
	dir       string // relative directory path
	layer     string // architectural layer label
	topNodes  []repoMapEntity
	totalRefs int
}

// repoMapEntity is a single entity line in the repo map.
type repoMapEntity struct {
	name string
	refs int
	typ  graph.NodeType
}

// buildRepoMap assembles the text repo map.
func buildRepoMap(
	nodes []*graph.Node,
	refs map[graph.NodeID]int,
	repoRoot string,
	topN int,
) string {
	// Group nodes by directory (relative path).
	type nodeScore struct {
		node *graph.Node
		refs int
	}
	dirNodes := make(map[string][]nodeScore)

	for _, n := range nodes {
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage || n.Type == graph.NodeRoute {
			continue
		}
		if strings.Contains(n.File, "_test.go") {
			continue
		}
		if n.Provenance == graph.ProvenanceVendored {
			continue
		}
		dir := relPath(repoRoot, dirOf(n.File))
		dirNodes[dir] = append(dirNodes[dir], nodeScore{
			node: n,
			refs: refs[n.ID],
		})
	}

	// Build per-directory packages sorted by total refs descending.
	var pkgs []repoMapPackage
	for dir, ns := range dirNodes {
		// Sort nodes in this dir by refs desc, then name asc for stability.
		sort.Slice(ns, func(i, j int) bool {
			if ns[i].refs != ns[j].refs {
				return ns[i].refs > ns[j].refs
			}
			return ns[i].node.Name < ns[j].node.Name
		})
		top := ns
		if len(top) > topN {
			top = top[:topN]
		}
		var entities []repoMapEntity
		var totalRefs int
		for _, s := range top {
			entities = append(entities, repoMapEntity{
				name: s.node.Name,
				refs: s.refs,
				typ:  s.node.Type,
			})
			totalRefs += s.refs
		}
		pkgs = append(pkgs, repoMapPackage{
			dir:       dir,
			layer:     detectLayerLabel(dir),
			topNodes:  entities,
			totalRefs: totalRefs,
		})
	}

	// Group packages by architectural layer, then sort within layer by totalRefs desc.
	layerOrder := []string{"[entry point]", "[api surface]", "[core logic]", "[persistence]", "[config]", "[test support]", "[external]"}
	layerGroups := make(map[string][]repoMapPackage)
	for _, p := range pkgs {
		layerGroups[p.layer] = append(layerGroups[p.layer], p)
	}
	for lbl := range layerGroups {
		sort.Slice(layerGroups[lbl], func(i, j int) bool {
			return layerGroups[lbl][i].totalRefs > layerGroups[lbl][j].totalRefs
		})
	}

	var sb strings.Builder
	sb.WriteString("# Repository Map\n\n")

	layerWritten := false
	for _, lbl := range layerOrder {
		group := layerGroups[lbl]
		if len(group) == 0 {
			continue
		}
		// Check whether any package in this layer actually has entities to show.
		anyVisible := false
		for _, p := range group {
			if len(p.topNodes) > 0 {
				anyVisible = true
				break
			}
		}
		if !anyVisible {
			continue
		}
		// Blank line between non-empty layers.
		if layerWritten {
			sb.WriteString("\n")
		}
		for _, p := range group {
			if len(p.topNodes) == 0 {
				continue
			}
			fmt.Fprintf(&sb, "%-60s %s\n", p.dir+"/", lbl)
			for _, e := range p.topNodes {
				if e.refs > 0 {
					fmt.Fprintf(&sb, "  %s (%d refs)\n", e.name, e.refs)
				} else {
					fmt.Fprintf(&sb, "  %s\n", e.name)
				}
			}
			layerWritten = true
		}
		delete(layerGroups, lbl)
	}

	// Any remaining layers not in layerOrder (shouldn't happen but be safe).
	for _, group := range layerGroups {
		for _, p := range group {
			if len(p.topNodes) == 0 {
				continue
			}
			if layerWritten {
				sb.WriteString("\n")
			}
			fmt.Fprintf(&sb, "%-60s\n", p.dir+"/")
			for _, e := range p.topNodes {
				if e.refs > 0 {
					fmt.Fprintf(&sb, "  %s (%d refs)\n", e.name, e.refs)
				} else {
					fmt.Fprintf(&sb, "  %s\n", e.name)
				}
			}
			layerWritten = true
		}
	}

	return sb.String()
}

// dirOf returns the directory component of a file path.
func dirOf(file string) string {
	idx := strings.LastIndexAny(file, "/\\")
	if idx < 0 {
		return "."
	}
	return file[:idx]
}

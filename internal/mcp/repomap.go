package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// repomapCacheKeyCompact / Full are packet-cache keys for get_repo_map.
const (
	repomapCacheKeyCompact = "repomap:compact"
	repomapCacheKeyFull    = "repomap:full"
)

// handleGetRepoMap returns a navigable text overview of the repository,
// grouped by package and sorted by fanin. Two detail levels:
//   - compact (~500 tokens): top 3 entities per package
//   - full    (~2000 tokens): top 10 entities per package
func (s *Server) handleGetRepoMap(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	detail := stringArg(req, "detail")
	if detail == "" {
		detail = "compact"
	}
	cacheKey := repomapCacheKeyCompact
	if detail == "full" {
		cacheKey = repomapCacheKeyFull
	}

	if cached := s.getPacketFromCache(cacheKey); cached != nil {
		if txt, ok := cached.(string); ok {
			return mcp.NewToolResultText(txt), nil
		}
	}

	nodes := s.graph.AllNodes()
	edges := s.graph.AllEdges()
	repoRoot := s.graph.Root()

	// Build fanin map.
	fanin := make(map[graph.NodeID]int, len(nodes))
	for _, e := range edges {
		fanin[e.To]++
	}

	topN := 3
	if detail == "full" {
		topN = 10
	}

	result := buildRepoMap(nodes, fanin, repoRoot, topN)

	s.setPacketCache(cacheKey, result)
	return mcp.NewToolResultText(result), nil
}

// repoMapPackage holds one package's top entities for the map.
type repoMapPackage struct {
	dir        string // relative directory path
	layer      string // architectural layer label
	topNodes   []repoMapEntity
	totalFanin int
}

// repoMapEntity is a single entity line in the repo map.
type repoMapEntity struct {
	name   string
	fanin  int
	fanout int
	typ    graph.NodeType
}

// buildRepoMap assembles the text repo map.
func buildRepoMap(
	nodes []*graph.Node,
	fanin map[graph.NodeID]int,
	repoRoot string,
	topN int,
) string {
	// Group nodes by directory (relative path).
	type nodeScore struct {
		node   *graph.Node
		fanin  int
		fanout int
	}
	dirNodes := make(map[string][]nodeScore)

	// Build fanout from nodes (count outgoing by iterating — we already have edges
	// snapshotted into fanin; for fanout we estimate from node metadata complexity
	// or just use 0; for the map display only fanin is required per spec).
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
			node:  n,
			fanin: fanin[n.ID],
		})
	}

	// Build per-directory packages sorted by total fanin descending.
	var pkgs []repoMapPackage
	for dir, ns := range dirNodes {
		// Sort nodes in this dir by fanin desc.
		sort.Slice(ns, func(i, j int) bool {
			if ns[i].fanin != ns[j].fanin {
				return ns[i].fanin > ns[j].fanin
			}
			return ns[i].node.Name < ns[j].node.Name
		})
		top := ns
		if len(top) > topN {
			top = top[:topN]
		}
		var entities []repoMapEntity
		var totalFanin int
		for _, ns := range top {
			entities = append(entities, repoMapEntity{
				name:  ns.node.Name,
				fanin: ns.fanin,
				typ:   ns.node.Type,
			})
			totalFanin += ns.fanin
		}
		pkgs = append(pkgs, repoMapPackage{
			dir:        dir,
			layer:      detectLayerLabel(dir),
			topNodes:   entities,
			totalFanin: totalFanin,
		})
	}

	// Group packages by architectural layer, then sort within layer by totalFanin desc.
	layerOrder := []string{"[entry point]", "[api surface]", "[core logic]", "[persistence]", "[config]", "[test support]", "[external]"}
	layerGroups := make(map[string][]repoMapPackage)
	for _, p := range pkgs {
		layerGroups[p.layer] = append(layerGroups[p.layer], p)
	}
	for lbl := range layerGroups {
		sort.Slice(layerGroups[lbl], func(i, j int) bool {
			return layerGroups[lbl][i].totalFanin > layerGroups[lbl][j].totalFanin
		})
	}

	var sb strings.Builder
	sb.WriteString("# Repository Map\n\n")

	written := 0
	for _, lbl := range layerOrder {
		group := layerGroups[lbl]
		if len(group) == 0 {
			continue
		}
		// Emit packages in this layer.
		for _, p := range group {
			if len(p.topNodes) == 0 {
				continue
			}
			line := fmt.Sprintf("%-60s %s\n", p.dir+"/", lbl)
			sb.WriteString(line)
			for _, e := range p.topNodes {
				if e.fanin > 0 {
					sb.WriteString(fmt.Sprintf("  %s (%d callers)\n", e.name, e.fanin))
				} else {
					sb.WriteString(fmt.Sprintf("  %s\n", e.name))
				}
			}
			written++
		}
		// Blank line between layers.
		if written > 0 {
			sb.WriteString("\n")
		}
		delete(layerGroups, lbl)
	}

	// Any remaining layers not in layerOrder (shouldn't happen but be safe).
	for _, group := range layerGroups {
		for _, p := range group {
			if len(p.topNodes) == 0 {
				continue
			}
			sb.WriteString(fmt.Sprintf("%-60s\n", p.dir+"/"))
			for _, e := range p.topNodes {
				sb.WriteString(fmt.Sprintf("  %s (%d callers)\n", e.name, e.fanin))
			}
		}
		sb.WriteString("\n")
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

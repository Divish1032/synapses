// Package dataflow implements source-to-sink data flow analysis on top of the
// existing CALLS graph. It heuristically tags function/method nodes as "sources"
// (data entry points: HTTP handlers, parsers, env-var accessors) or "sinks"
// (dangerous data exits: SQL exec, file writes, exec.Command), then creates
// DATA_FLOWS summary edges for each (source, sink) pair reachable within
// maxHops via CALLS edges.
//
// This is call-path-based taint analysis — not variable-level taint propagation.
// It delivers 80% of the security review value without the edge explosion and
// token-budget problems that full inter-procedural taint analysis would cause.
package dataflow

import (
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

const (
	maxDataFlowsPerSource = 20 // cap to prevent edge explosion on large codebases
	defaultMaxHops        = 4
)

// edgeKey is used to deduplicate DATA_FLOWS edges.
type edgeKey struct {
	from graph.NodeID
	to   graph.NodeID
}

// AnnotateGraph walks all function/method nodes, marks sources and sinks via
// built-in heuristics supplemented by cfg patterns, then creates DATA_FLOWS
// summary edges for each (source, sink) pair reachable within maxHops CALLS hops.
// Returns the number of DATA_FLOWS edges created.
func AnnotateGraph(g *graph.Graph, cfg *config.Config) int {
	maxHops := cfg.DataFlowMaxHops
	if maxHops <= 0 {
		maxHops = defaultMaxHops
	}

	nodes := g.AllNodes()

	// Phase 1: Annotate sources and sinks.
	var sources, sinks []*graph.Node
	for _, n := range nodes {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		role, label := detectRole(n, cfg)
		if role == "" {
			continue
		}
		// Mutate metadata under the graph write lock to avoid data races
		// with concurrent readers (e.g. CarveEgoGraph).
		g.UpdateNodeMetadata(n.ID, func(nn *graph.Node) {
			if nn.Metadata == nil {
				nn.Metadata = make(map[string]string)
			}
			nn.Metadata["data_role"] = role
			nn.Metadata["data_label"] = label
		})
		if role == "source" {
			sources = append(sources, n)
		} else {
			sinks = append(sinks, n)
		}
	}

	if len(sources) == 0 || len(sinks) == 0 {
		return 0
	}

	// Build a fast lookup set for sink node IDs.
	sinkSet := make(map[graph.NodeID]bool, len(sinks))
	for _, s := range sinks {
		sinkSet[s.ID] = true
	}

	// Phase 2: BFS from each source; create DATA_FLOWS edges to reachable sinks.
	// The seen set deduplicates across sources within this call; AddEdge
	// handles dedup against pre-existing edges in the graph.
	seen := make(map[edgeKey]bool)
	created := 0
	for _, src := range sources {
		reachable := bfsReachableSinks(g, src.ID, sinkSet, maxHops)
		count := 0
		for _, sinkID := range reachable {
			if count >= maxDataFlowsPerSource {
				break
			}
			k := edgeKey{src.ID, sinkID}
			if seen[k] {
				continue
			}
			seen[k] = true
			g.AddEdge(&graph.Edge{
				From: src.ID,
				To:   sinkID,
				Type: graph.EdgeDataFlows,
			})
			created++
			count++
		}
	}

	return created
}

// detectRole returns the data-flow role ("source"/"sink") and label for a node,
// or ("", "") if it matches neither. Config patterns are checked first (allowing
// overrides), then built-in heuristics.
func detectRole(n *graph.Node, cfg *config.Config) (role, label string) {
	// Config overrides first.
	for _, p := range cfg.DataFlowSources {
		if matchesPattern(n, p) {
			lbl := p.Label
			if lbl == "" {
				lbl = "custom_source"
			}
			return "source", lbl
		}
	}
	for _, p := range cfg.DataFlowSinks {
		if matchesPattern(n, p) {
			lbl := p.Label
			if lbl == "" {
				lbl = "custom_sink"
			}
			return "sink", lbl
		}
	}

	// Built-in source heuristics.
	sig := ""
	if n.Metadata != nil {
		sig = n.Metadata["signature"]
	}
	name := n.Name
	// Strip receiver for method names like "Server.handleGetContext" → "handleGetContext"
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		name = name[dot+1:]
	}
	nameLower := strings.ToLower(name)

	// HTTP framework inputs.
	if strings.Contains(sig, "*http.Request") ||
		strings.Contains(sig, "gin.Context") ||
		strings.Contains(sig, "echo.Context") ||
		strings.Contains(sig, "fiber.Ctx") {
		return "source", "http_input"
	}
	// Stream readers.
	if strings.Contains(sig, "io.Reader") {
		return "source", "stream_input"
	}
	// Parser / decoder entry points.
	if strings.HasPrefix(nameLower, "parse") ||
		strings.HasPrefix(nameLower, "decode") ||
		strings.HasPrefix(nameLower, "unmarshal") {
		return "source", "parser"
	}
	// Environment / config readers.
	if strings.Contains(nameLower, "getenv") ||
		strings.Contains(nameLower, "lookupenv") ||
		nameLower == "getenv" {
		return "source", "env_input"
	}

	// Built-in sink heuristics.
	// SQL execution.
	if strings.Contains(sig, "*sql.DB") || strings.Contains(sig, "*sql.Tx") {
		return "sink", "sql_sink"
	}
	switch nameLower {
	case "exec", "query", "queryrow", "querycontext", "execcontext",
		"prepare", "preparecontext":
		return "sink", "sql_sink"
	}
	// OS command execution.
	if strings.Contains(nameLower, "command") || nameLower == "run" || nameLower == "start" {
		if fileInPackage(n, "exec") {
			return "sink", "exec_sink"
		}
	}
	// File writes.
	if strings.Contains(sig, "*os.File") && strings.HasPrefix(nameLower, "write") {
		return "sink", "file_write_sink"
	}
	// Generic writer sinks.
	if strings.Contains(sig, "io.Writer") && strings.HasPrefix(nameLower, "write") {
		return "sink", "write_sink"
	}

	return "", ""
}

// matchesPattern checks a node against a DataFlowPattern.
// All non-empty fields are ANDed (every specified field must match).
func matchesPattern(n *graph.Node, p config.DataFlowPattern) bool {
	if p.NodeType != "" && n.Type != p.NodeType {
		return false
	}
	if p.NamePattern != "" {
		if !strings.Contains(strings.ToLower(n.Name), strings.ToLower(p.NamePattern)) {
			return false
		}
	}
	if p.FilePattern != "" {
		base := filepath.Base(n.File)
		matched, _ := filepath.Match(p.FilePattern, base)
		if !matched && !strings.Contains(n.File, p.FilePattern) {
			return false
		}
	}
	if p.SigPattern != "" {
		sig := ""
		if n.Metadata != nil {
			sig = n.Metadata["signature"]
		}
		if !strings.Contains(sig, p.SigPattern) {
			return false
		}
	}
	return true
}

// bfsReachableSinks does a forward BFS from sourceID following only CALLS edges,
// up to maxHops deep. Returns the IDs of any sink nodes encountered.
func bfsReachableSinks(g *graph.Graph, sourceID graph.NodeID, sinkSet map[graph.NodeID]bool, maxHops int) []graph.NodeID {
	type qItem struct {
		id  graph.NodeID
		hop int
	}

	visited := make(map[graph.NodeID]bool)
	visited[sourceID] = true
	queue := []qItem{{sourceID, 0}}
	var found []graph.NodeID

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if cur.hop >= maxHops {
			continue
		}

		for _, e := range g.OutEdges(cur.id) {
			if e.Type != graph.EdgeCalls {
				continue
			}
			if visited[e.To] {
				continue
			}
			visited[e.To] = true

			if sinkSet[e.To] {
				found = append(found, e.To)
			}

			queue = append(queue, qItem{e.To, cur.hop + 1})
		}
	}

	return found
}

// fileInPackage returns true if the node's file path contains the given package
// name as a directory component or file prefix. Used for exec.Command detection.
func fileInPackage(n *graph.Node, pkg string) bool {
	return strings.Contains(n.File, "/"+pkg+"/") ||
		strings.Contains(n.File, "/"+pkg+".") ||
		n.Package == pkg
}

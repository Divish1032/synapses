package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// explainCacheKey is the packet-cache key for explain_codebase results.
const explainCacheKey = "explain_codebase"

// handleExplainCodebase returns a ~1000 token natural-language orientation
// covering entry points, key types, detected architectural patterns, package
// structure, and tech stack. Built entirely from the live graph — no LLM
// required for the base case.
func (s *Server) handleExplainCodebase(
	_ context.Context,
	_ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	// Serve from packet cache when available (invalidated on graph structural change).
	if cached := s.getPacketFromCache(explainCacheKey); cached != nil {
		if txt, ok := cached.(string); ok {
			return mcp.NewToolResultText(txt), nil
		}
	}

	identity := s.graph.ProjectIdentity()
	nodes := s.graph.AllNodes()
	edges := s.graph.AllEdges()
	repoRoot := s.graph.Root()

	// Build fanin map from edge snapshot.
	fanin := make(map[graph.NodeID]int, len(nodes))
	for _, e := range edges {
		fanin[e.To]++
	}

	result := buildExplanation(identity, nodes, edges, fanin, repoRoot)

	s.setPacketCache(explainCacheKey, result)
	return mcp.NewToolResultText(result), nil
}

// buildExplanation assembles the narrative text for explain_codebase.
func buildExplanation(
	identity *graph.ProjectIdentity,
	nodes []*graph.Node,
	edges []*graph.Edge,
	fanin map[graph.NodeID]int,
	repoRoot string,
) string {
	var sb strings.Builder

	// ── Header ────────────────────────────────────────────────────────────────
	sb.WriteString("# Codebase Orientation\n\n")
	sb.WriteString(fmt.Sprintf(
		"**Scale**: %s (%d files, %d functions+methods, %d structs, %d edges)\n\n",
		identity.Scale,
		identity.Summary.Files,
		identity.Summary.Functions+identity.Summary.Methods,
		identity.Summary.Structs,
		identity.Summary.Edges,
	))

	// ── Tech Stack ────────────────────────────────────────────────────────────
	langs, externalImports := detectTechStack(nodes)
	if len(langs) > 0 {
		sb.WriteString("**Languages**: ")
		sb.WriteString(strings.Join(langs, ", "))
		sb.WriteString("\n")
	}
	if len(externalImports) > 0 {
		top := externalImports
		if len(top) > 8 {
			top = top[:8]
		}
		sb.WriteString("**Key dependencies**: ")
		sb.WriteString(strings.Join(top, ", "))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// ── Architectural Pattern ─────────────────────────────────────────────────
	pattern := detectArchPattern(nodes, edges)
	sb.WriteString(fmt.Sprintf("**Architectural pattern**: %s\n\n", pattern))

	// ── Entry Points ──────────────────────────────────────────────────────────
	if len(identity.EntryPoints) > 0 {
		sb.WriteString("## Entry Points\n\n")
		shown := identity.EntryPoints
		if len(shown) > 8 {
			shown = shown[:8]
		}
		for _, ep := range shown {
			relFile := relPath(repoRoot, ep.File)
			sb.WriteString(fmt.Sprintf("- `%s` — %s:%d\n", ep.Name, relFile, ep.Line))
		}
		sb.WriteString("\n")
	}

	// ── Key Types by Fanin ────────────────────────────────────────────────────
	sb.WriteString("## Key Types (highest connectivity)\n\n")
	type scored struct {
		node  *graph.Node
		score int
	}
	var candidates []scored
	for _, n := range nodes {
		if n.Type != graph.NodeStruct && n.Type != graph.NodeInterface {
			continue
		}
		if strings.Contains(n.File, "_test.go") {
			continue
		}
		if n.Provenance == graph.ProvenanceVendored || n.Provenance == graph.ProvenanceGenerated {
			continue
		}
		s := fanin[n.ID]
		if s > 0 {
			candidates = append(candidates, scored{n, s})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) > 10 {
		candidates = candidates[:10]
	}
	if len(candidates) == 0 {
		sb.WriteString("_(no highly-connected types found)_\n")
	} else {
		for _, c := range candidates {
			relFile := relPath(repoRoot, c.node.File)
			sb.WriteString(fmt.Sprintf("- `%s` (%s) — %d callers — %s:%d\n",
				c.node.Name, c.node.Type, c.score, relFile, c.node.Line))
		}
	}
	sb.WriteString("\n")

	// ── Package Structure ─────────────────────────────────────────────────────
	sb.WriteString("## Package Structure\n\n")
	pkgStats := buildPackageStats(nodes, fanin, repoRoot)
	// Sort by total fanin descending.
	sort.Slice(pkgStats, func(i, j int) bool {
		return pkgStats[i].totalFanin > pkgStats[j].totalFanin
	})
	shown := pkgStats
	if len(shown) > 12 {
		shown = shown[:12]
	}
	for _, ps := range shown {
		label := detectLayerLabel(ps.pkg)
		sb.WriteString(fmt.Sprintf("  %-50s %s\n", ps.pkg+"/", label))
	}
	sb.WriteString("\n")

	// ── Conventions / Memories ────────────────────────────────────────────────
	// (memories would be injected by the caller layer if available — left as
	// a hook; not implemented in this pure-graph pass)

	return sb.String()
}

// packageStats holds per-package aggregated connectivity data.
type packageStats struct {
	pkg        string
	nodeCount  int
	totalFanin int
}

// buildPackageStats groups non-test, non-vendored nodes by package and sums fanin.
func buildPackageStats(nodes []*graph.Node, fanin map[graph.NodeID]int, repoRoot string) []packageStats {
	pkgMap := make(map[string]*packageStats)
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
		// Group by relative directory path (unique per package location) rather
		// than n.Package (short name) to avoid merging unrelated packages that
		// happen to share the same short name (e.g. two "mcp" packages).
		pkg := relPath(repoRoot, filepath.Dir(n.File))
		if pkg == "." || pkg == "" {
			pkg = n.Package
		}
		ps, ok := pkgMap[pkg]
		if !ok {
			ps = &packageStats{pkg: pkg}
			pkgMap[pkg] = ps
		}
		ps.nodeCount++
		ps.totalFanin += fanin[n.ID]
	}
	out := make([]packageStats, 0, len(pkgMap))
	for _, ps := range pkgMap {
		out = append(out, *ps)
	}
	return out
}

// detectTechStack returns the list of unique languages and top external imports
// detected from the graph nodes.
func detectTechStack(nodes []*graph.Node) (langs []string, externalImports []string) {
	langSet := make(map[string]bool)
	importCounts := make(map[string]int)

	for _, n := range nodes {
		if n.Provenance == graph.ProvenanceVendored {
			continue
		}
		if n.File == "" {
			continue
		}
		ext := strings.ToLower(filepath.Ext(n.File))
		switch ext {
		case ".go":
			langSet["Go"] = true
		case ".ts", ".tsx":
			langSet["TypeScript"] = true
		case ".js", ".jsx":
			langSet["JavaScript"] = true
		case ".py":
			langSet["Python"] = true
		case ".rs":
			langSet["Rust"] = true
		case ".java":
			langSet["Java"] = true
		case ".rb":
			langSet["Ruby"] = true
		case ".cs":
			langSet["C#"] = true
		case ".cpp", ".cc", ".cxx":
			langSet["C++"] = true
		case ".c":
			langSet["C"] = true
		}

		// Collect external import names from IMPORTS-type nodes.
		if n.Type == graph.NodePackage && !strings.Contains(n.File, "vendor") {
			pkg := n.Name
			if isExternalPkg(pkg) {
				importCounts[pkg]++
			}
		}
	}

	for l := range langSet {
		langs = append(langs, l)
	}
	sort.Strings(langs)

	type kv struct {
		k string
		v int
	}
	var sorted []kv
	for k, v := range importCounts {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].v > sorted[j].v })
	for _, kv := range sorted {
		externalImports = append(externalImports, kv.k)
	}
	return langs, externalImports
}

// isExternalPkg returns true for import paths that look like third-party packages.
func isExternalPkg(pkg string) bool {
	if pkg == "" {
		return false
	}
	// stdlib packages have no dot in the first segment.
	parts := strings.SplitN(pkg, "/", 2)
	return strings.Contains(parts[0], ".")
}

// detectArchPattern heuristically identifies the high-level architecture type
// from import names present in the graph nodes.
func detectArchPattern(nodes []*graph.Node, _ []*graph.Edge) string {
	imports := make(map[string]bool)
	for _, n := range nodes {
		// NodePackage nodes have Name = full import path (e.g. "github.com/foo/bar")
		// as set by the Go parser. Other language parsers follow the same convention.
		if n.Type == graph.NodePackage {
			imports[n.Name] = true
		}
	}

	var patterns []string

	// Web server / HTTP API
	if imports["net/http"] || imports["github.com/gin-gonic/gin"] ||
		imports["github.com/gorilla/mux"] || imports["github.com/labstack/echo/v4"] ||
		imports["github.com/go-chi/chi/v5"] || imports["fastapi"] || imports["flask"] ||
		imports["express"] {
		patterns = append(patterns, "HTTP server / web API")
	}

	// CLI tool
	if imports["github.com/spf13/cobra"] || imports["github.com/urfave/cli/v2"] ||
		imports["flag"] || imports["os/exec"] {
		patterns = append(patterns, "CLI tool")
	}

	// gRPC service
	if imports["google.golang.org/grpc"] || imports["github.com/grpc-ecosystem/grpc-gateway/v2"] {
		patterns = append(patterns, "gRPC service")
	}

	// MCP server
	if imports["github.com/mark3labs/mcp-go/mcp"] || imports["github.com/mark3labs/mcp-go/server"] {
		patterns = append(patterns, "MCP server")
	}

	// Library / SDK
	if imports["testing"] {
		patterns = append(patterns, "library / SDK")
	}

	// Event-driven
	if imports["github.com/nats-io/nats.go"] || imports["github.com/confluentinc/confluent-kafka-go/kafka"] ||
		imports["github.com/streadway/amqp"] {
		patterns = append(patterns, "event-driven")
	}

	if len(patterns) == 0 {
		return "unknown — insufficient import signals"
	}
	return strings.Join(patterns, " + ")
}

// detectLayerLabel returns a short architectural-layer label for a package path.
func detectLayerLabel(pkg string) string {
	lower := strings.ToLower(pkg)
	switch {
	case strings.Contains(lower, "cmd") || strings.Contains(lower, "main"):
		return "[entry point]"
	case strings.Contains(lower, "store") || strings.Contains(lower, "db") ||
		strings.Contains(lower, "repository") || strings.Contains(lower, "dao") ||
		strings.Contains(lower, "sqlite") || strings.Contains(lower, "postgres"):
		return "[persistence]"
	case strings.Contains(lower, "handler") || strings.Contains(lower, "controller") ||
		strings.Contains(lower, "router") || strings.Contains(lower, "http") ||
		strings.Contains(lower, "api") || strings.Contains(lower, "mcp") ||
		strings.Contains(lower, "rpc") || strings.Contains(lower, "grpc"):
		return "[api surface]"
	case strings.Contains(lower, "config") || strings.Contains(lower, "settings"):
		return "[config]"
	case strings.Contains(lower, "test") || strings.Contains(lower, "mock") ||
		strings.Contains(lower, "fake"):
		return "[test support]"
	default:
		return "[core logic]"
	}
}

// relPath trims the repo root prefix from an absolute file path.
func relPath(root, abs string) string {
	if root == "" {
		return abs
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return rel
}

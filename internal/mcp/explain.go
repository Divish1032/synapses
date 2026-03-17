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

// handleExplainCodebase returns a ~1000 token natural-language orientation
// covering entry points, key types, detected architectural patterns, package
// structure, and tech stack. Built entirely from the live graph — no LLM
// required for the base case.
//
// Results are stored in a dedicated orientation cache (not the shared 20-slot
// packet cache) so that heavy get_context traffic cannot evict them. The cache
// is invalidated via InvalidatePacketCacheForFile on every graph structural change.
func (s *Server) handleExplainCodebase(
	_ context.Context,
	_ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	s.orientMu.RLock()
	cached := s.orientExplain
	s.orientMu.RUnlock()
	if cached != nil {
		return mcp.NewToolResultText(*cached), nil
	}

	identity := s.graph.ProjectIdentity()
	nodes := s.graph.AllNodes()
	edges := s.graph.AllEdges()
	repoRoot := s.graph.Root()

	refs := connectivityMap(edges)
	result := buildExplanation(identity, nodes, refs, repoRoot)

	s.orientMu.Lock()
	s.orientExplain = &result
	s.orientMu.Unlock()

	return mcp.NewToolResultText(result), nil
}

// connectivityMap builds a node→count map of meaningful incoming references.
// DEFINES (file→entity) and IMPORTS (file→package) edges are excluded because
// every entity has exactly one DEFINES edge regardless of its actual usage —
// including them would add uniform noise to every node's score and make the
// top-N selection meaningless.
func connectivityMap(edges []*graph.Edge) map[graph.NodeID]int {
	m := make(map[graph.NodeID]int, len(edges)/2)
	for _, e := range edges {
		if e.Type == graph.EdgeDefines || e.Type == graph.EdgeImports {
			continue
		}
		m[e.To]++
	}
	return m
}

// buildExplanation assembles the narrative text for explain_codebase.
func buildExplanation(
	identity *graph.ProjectIdentity,
	nodes []*graph.Node,
	refs map[graph.NodeID]int, // connectivity score per node (excl. DEFINES/IMPORTS)
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
	pattern := detectArchPattern(nodes)
	sb.WriteString(fmt.Sprintf("**Architectural pattern**: %s\n\n", pattern))

	// ── Entry Points ──────────────────────────────────────────────────────────
	if len(identity.EntryPoints) > 0 {
		sb.WriteString("## Entry Points\n\n")

		// IMP-EVAL-6: rank entry points so primary daemon/cmd entries appear first.
		// Priority tiers (lower = higher priority):
		//   0: main() in a cmd/ directory (the actual application entry point)
		//   1: main() anywhere else
		//   2: exported function not in archive/scripts/tools
		//   3: exported function in archive/scripts/tools (deprioritised)
		entryPointTier := func(ep graph.EntityRef) int {
			f := strings.ToLower(ep.File)
			isMain := ep.Name == "main"
			// Use path-segment-aware patterns to avoid false positives:
			//   "script"  would match "subscriptions", "description", "transcript"
			//   "archive" would match "archivist", "archiver"
			// Anchor each pattern to a path segment boundary (leading /, trailing /, or prefix).
			isArchived := strings.HasPrefix(f, "scripts/") || strings.Contains(f, "/scripts/") || strings.HasSuffix(f, "/scripts") ||
				strings.HasPrefix(f, "archive/") || strings.Contains(f, "/archive/") || strings.HasSuffix(f, "/archive") ||
				strings.HasPrefix(f, "archived/") || strings.Contains(f, "/archived/") ||
				strings.Contains(f, "_archive/") || strings.HasSuffix(f, "_archive") ||
				strings.HasPrefix(f, "tools/") || strings.Contains(f, "/tools/") || strings.HasSuffix(f, "/tools")
			inCmd := strings.HasPrefix(f, "cmd/") || strings.Contains(f, "/cmd/") || strings.HasSuffix(f, "/cmd")
			switch {
			case isMain && inCmd:
				return 0
			case isMain:
				return 1
			case !isArchived:
				return 2
			default:
				return 3
			}
		}
		eps := make([]graph.EntityRef, len(identity.EntryPoints))
		copy(eps, identity.EntryPoints)
		sort.Slice(eps, func(i, j int) bool {
			ti, tj := entryPointTier(eps[i]), entryPointTier(eps[j])
			if ti != tj {
				return ti < tj
			}
			return eps[i].File < eps[j].File // stable secondary sort by path
		})

		shown := eps
		if len(shown) > 8 {
			shown = shown[:8]
		}
		for _, ep := range shown {
			// ep.File is already relative (stripped by Graph.ProjectIdentity via
			// Graph.relPath). Using it directly avoids a double-relPath call that
			// would fail when the path is already relative.
			sb.WriteString(fmt.Sprintf("- `%s` — %s:%d\n", ep.Name, ep.File, ep.Line))
		}
		sb.WriteString("\n")
	}

	// ── Key Types by Connectivity ─────────────────────────────────────────────
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
		score := refs[n.ID]
		if score > 0 {
			candidates = append(candidates, scored{n, score})
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
			// "N refs" is accurate for all node types: structs get IMPLEMENTS/
			// DATA_FLOWS/EMBEDS references, not CALLS. Using "callers" would be
			// misleading for non-function types.
			sb.WriteString(fmt.Sprintf("- `%s` (%s) — %d refs — %s:%d\n",
				c.node.Name, c.node.Type, c.score, relFile, c.node.Line))
		}
	}
	sb.WriteString("\n")

	// ── Package Structure ─────────────────────────────────────────────────────
	sb.WriteString("## Package Structure\n\n")
	pkgStats := buildPackageStats(nodes, refs, repoRoot)
	sort.Slice(pkgStats, func(i, j int) bool {
		return pkgStats[i].totalRefs > pkgStats[j].totalRefs
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

	return sb.String()
}

// packageStats holds per-package aggregated connectivity data.
type packageStats struct {
	pkg       string
	nodeCount int
	totalRefs int
}

// buildPackageStats groups non-test, non-vendored nodes by relative directory
// path (unique per package location) and sums their connectivity scores.
func buildPackageStats(nodes []*graph.Node, refs map[graph.NodeID]int, repoRoot string) []packageStats {
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
		// Group by relative directory path — unique, unlike n.Package (short name)
		// which can be shared by unrelated packages in different directories.
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
		ps.totalRefs += refs[n.ID]
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

		// NodePackage nodes have Name = full import path (e.g. "github.com/foo/bar")
		// as set by the Go parser. Only count external (non-stdlib) packages.
		if n.Type == graph.NodePackage && !strings.Contains(n.File, "vendor") {
			if isExternalPkg(n.Name) {
				importCounts[n.Name]++
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
// stdlib packages have no dot in the first path segment (e.g. "net/http" → "net").
func isExternalPkg(pkg string) bool {
	if pkg == "" {
		return false
	}
	parts := strings.SplitN(pkg, "/", 2)
	return strings.Contains(parts[0], ".")
}

// detectArchPattern heuristically identifies the high-level architecture type
// from NodePackage nodes in the graph. NodePackage.Name is the full import path
// as set by the Go parser (and equivalent parsers for other languages).
func detectArchPattern(nodes []*graph.Node) string {
	imports := make(map[string]bool)
	for _, n := range nodes {
		if n.Type == graph.NodePackage {
			imports[n.Name] = true
		}
	}

	var patterns []string

	if imports["net/http"] || imports["github.com/gin-gonic/gin"] ||
		imports["github.com/gorilla/mux"] || imports["github.com/labstack/echo/v4"] ||
		imports["github.com/go-chi/chi/v5"] || imports["fastapi"] || imports["flask"] ||
		imports["express"] {
		patterns = append(patterns, "HTTP server / web API")
	}

	if imports["github.com/spf13/cobra"] || imports["github.com/urfave/cli/v2"] ||
		imports["flag"] || imports["os/exec"] {
		patterns = append(patterns, "CLI tool")
	}

	if imports["google.golang.org/grpc"] || imports["github.com/grpc-ecosystem/grpc-gateway/v2"] {
		patterns = append(patterns, "gRPC service")
	}

	if imports["github.com/mark3labs/mcp-go/mcp"] || imports["github.com/mark3labs/mcp-go/server"] {
		patterns = append(patterns, "MCP server")
	}

	if imports["testing"] {
		patterns = append(patterns, "library / SDK")
	}

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
// Returns abs unchanged when root is empty or when filepath.Rel returns an error
// (e.g. when abs is already a relative path).
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

// handlers_investigate.go implements the get_context(mode="investigate") handler.
// One call takes a problem statement and returns ranked code blocks with actual
// source, combining search + graph traversal + impact analysis + file reading.
package mcp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ─── Response types ─────────────────────────────────────────────────────────

type investigateBlock struct {
	File      string  `json:"file"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Entity    string  `json:"entity"`
	Type      string  `json:"type"`
	Relevance float64 `json:"relevance"`
	Reason    string  `json:"reason"`
	Source    string  `json:"source"`
}

type investigateResult struct {
	Summary       string             `json:"summary"`
	Blocks        []investigateBlock `json:"blocks"`
	AffectedFiles []string           `json:"affected_files,omitempty"`
	TestFiles     []string           `json:"test_files,omitempty"`
	GraphStats    map[string]int     `json:"graph_stats"`
}

// ─── Handler ────────────────────────────────────────────────────────────────

func (s *Server) handleInvestigate(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	start := time.Now()

	// Parse arguments.
	problem, _ := req.GetArguments()["problem"].(string)
	if problem == "" {
		return mcp.NewToolResultError(
			"problem is required — describe the bug, feature, or issue you're investigating. " +
				"Example: get_context(mode=\"investigate\", problem=\"authentication token not refreshed on expiry\")"), nil
	}
	target, _ := req.GetArguments()["target"].(string)
	maxBlocks := 5
	if mb, ok := req.GetArguments()["max_blocks"].(float64); ok && mb > 0 {
		maxBlocks = int(mb)
		if maxBlocks > 25 {
			maxBlocks = 25
		}
	}
	includeTests := false
	if it, ok := req.GetArguments()["include_tests"].(bool); ok {
		includeTests = it
	}

	g := s.graph
	repoRoot := g.Root()

	// Step 1: Entity resolution — find starting nodes for traversal.
	candidates := s.resolveInvestigateEntities(problem, target)
	if len(candidates) == 0 {
		return mcp.NewToolResultError(
			"no matching entities found. Try a more specific target or different problem description."), nil
	}

	// Step 2: Graph traversal — CarveEgoGraph + ImpactAnalysis for each candidate.
	type scoredNode struct {
		node       *graph.Node
		structural float64 // from BFS/PPR/impact
		depth      int     // hop distance
	}
	nodeMap := make(map[graph.NodeID]*scoredNode)
	allAffectedFiles := make(map[string]bool)
	allTestFiles := make(map[string]bool)
	entitiesTraversed := 0

	for _, rootNode := range candidates {
		// CarveEgoGraph — structural neighborhood.
		cfg := graph.DefaultCarveConfig()
		cfg.MaxDepth = 2
		cfg.TokenBudget = 8000
		cfg.ExcludeTestFiles = !includeTests

		sg, err := g.CarveEgoGraph(rootNode.ID, cfg)
		if err == nil && sg != nil {
			for _, cn := range sg.Nodes {
				entitiesTraversed++
				if existing, ok := nodeMap[cn.Node.ID]; ok {
					if cn.Relevance > existing.structural {
						existing.structural = cn.Relevance
					}
				} else {
					nodeMap[cn.Node.ID] = &scoredNode{
						node:       cn.Node,
						structural: cn.Relevance,
						depth:      cn.Hop,
					}
				}
			}
		}

		// ImpactAnalysis — reverse dependencies.
		impact, err := g.ImpactAnalysis(rootNode.ID, 2)
		if err == nil && impact != nil {
			for _, tier := range impact.Tiers {
				tierScore := 1.0
				if tier.Depth == 2 {
					tierScore = 0.6
				} else if tier.Depth >= 3 {
					tierScore = 0.3
				}
				for _, ref := range tier.Nodes {
					entitiesTraversed++
					n := g.GetNode(graph.NodeID(ref.ID))
					if n == nil {
						continue
					}
					if existing, ok := nodeMap[n.ID]; ok {
						if tierScore > existing.structural {
							existing.structural = tierScore
						}
					} else {
						nodeMap[n.ID] = &scoredNode{
							node:       n,
							structural: tierScore,
							depth:      tier.Depth,
						}
					}
				}
			}
			for _, f := range impact.AffectedFiles {
				rel := makeRelative(f, repoRoot)
				allAffectedFiles[rel] = true
			}
			for _, f := range impact.TestCoverage {
				rel := makeRelative(f, repoRoot)
				allTestFiles[rel] = true
			}
		}
	}

	// Step 3: Problem-aware relevance scoring.
	scorer := NewProblemScorer(problem)

	type rankedBlock struct {
		node       *graph.Node
		combined   float64
		structural float64
		keyword    float64
	}
	var ranked []rankedBlock
	for _, sn := range nodeMap {
		n := sn.node
		// Skip package and file nodes.
		if n.Type == graph.NodePackage || n.Type == graph.NodeFile {
			continue
		}
		// Skip test files unless requested.
		if !includeTests && isTestFile(n.File) {
			continue
		}
		// Skip documentation and non-code files — they inflate results
		// with keyword matches (e.g. "skip" in doctest.rst) but aren't
		// where bugs live.
		if isDocFile(n.File) || n.Domain == graph.DomainDocs {
			continue
		}
		kw := scorer.Score(n)
		combined := CombinedScore(sn.structural, kw)
		ranked = append(ranked, rankedBlock{
			node:       n,
			combined:   combined,
			structural: sn.structural,
			keyword:    kw,
		})
	}

	// Sort by combined score descending.
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].combined > ranked[j].combined
	})

	// Deduplicate by file+line (same entity from carve + impact).
	seen := make(map[string]bool)
	var deduped []rankedBlock
	for _, rb := range ranked {
		key := fmt.Sprintf("%s:%d", rb.node.File, rb.node.Line)
		if !seen[key] {
			seen[key] = true
			deduped = append(deduped, rb)
		}
	}
	if len(deduped) > maxBlocks {
		deduped = deduped[:maxBlocks]
	}

	// Step 4: Source code extraction.
	srcCache := newSourceCache(repoRoot)

	// Collect all nodes per file for end-line computation.
	fileNodes := make(map[string][]*graph.Node)
	for _, rb := range deduped {
		rel := makeRelative(rb.node.File, repoRoot)
		fileNodes[rel] = append(fileNodes[rel], rb.node)
	}
	// Also add neighboring nodes from graph for better end-line computation.
	for file := range fileNodes {
		allInFile := g.FindByFile(file)
		for _, n := range allInFile {
			fileNodes[file] = append(fileNodes[file], n)
		}
	}
	// Sort each file's nodes by line.
	for _, nodes := range fileNodes {
		sort.Slice(nodes, func(i, j int) bool {
			return nodes[i].Line < nodes[j].Line
		})
	}

	// Step 5: Build response blocks.
	var blocks []investigateBlock
	for _, rb := range deduped {
		n := rb.node
		rel := makeRelative(n.File, repoRoot)

		// Compute end line from neighboring nodes.
		lineCount, _ := strconv.Atoi(n.Metadata["line_count"])
		nextStart := 0
		for _, fn := range fileNodes[rel] {
			if fn.Line > n.Line {
				nextStart = fn.Line
				break
			}
		}
		fileLines := srcCache.TotalLines(rel)
		endLine := computeEndLine(n.Line, nextStart, lineCount, fileLines)

		// Extract source code.
		source := srcCache.Extract(rel, n.Line, endLine)

		reason := generateReason(n, rb.structural, rb.keyword, candidates)

		blocks = append(blocks, investigateBlock{
			File:      rel,
			StartLine: n.Line,
			EndLine:   endLine,
			Entity:    n.Name,
			Type:      string(n.Type),
			Relevance: rb.combined,
			Reason:    reason,
			Source:    source,
		})
	}

	// Step 6: Collect affected and test files.
	var affectedFiles []string
	for f := range allAffectedFiles {
		affectedFiles = append(affectedFiles, f)
	}
	sort.Strings(affectedFiles)

	var testFiles []string
	for f := range allTestFiles {
		testFiles = append(testFiles, f)
	}
	sort.Strings(testFiles)

	// Step 7: Build result.
	result := investigateResult{
		Summary: fmt.Sprintf("%d relevant code blocks across %d files (%.0fms)",
			len(blocks), countUniqueFiles(blocks), float64(time.Since(start).Microseconds())/1000),
		Blocks:        blocks,
		AffectedFiles: affectedFiles,
		TestFiles:     testFiles,
		GraphStats: map[string]int{
			"entities_traversed": entitiesTraversed,
			"candidates":        len(candidates),
			"files_scanned":     len(allAffectedFiles),
		},
	}

	return jsonResult(result)
}

// ─── Entity Resolution ──────────────────────────────────────────────────────

// resolveInvestigateEntities finds starting nodes from problem + target.
func (s *Server) resolveInvestigateEntities(problem, target string) []*graph.Node {
	g := s.graph
	seen := make(map[graph.NodeID]bool)
	var results []*graph.Node

	addUnique := func(nodes []*graph.Node) {
		for _, n := range nodes {
			if n != nil && !seen[n.ID] && n.Type != graph.NodePackage && n.Type != graph.NodeFile {
				seen[n.ID] = true
				results = append(results, n)
			}
		}
	}

	// Signal 1: Direct target match (highest confidence).
	if target != "" {
		addUnique(g.FindByName(target))
		if len(results) == 0 {
			addUnique(g.FindByPatternLimit(target, 10))
		}
	}

	// Signal 2: FTS search with problem keywords.
	if s.store != nil {
		keywords := extractSearchTerms(problem)
		for _, kw := range keywords {
			hits, err := s.store.SemanticSearch(kw, 5)
			if err != nil {
				continue
			}
			for _, h := range hits {
				n := g.GetNode(graph.NodeID(h.ID))
				if n != nil {
					addUnique([]*graph.Node{n})
				}
			}
		}
	}

	// Cap at 5 starting entities to keep traversal bounded.
	if len(results) > 5 {
		results = results[:5]
	}
	return results
}

// extractSearchTerms pulls significant keywords from a problem statement.
func extractSearchTerms(problem string) []string {
	words := strings.FieldsFunc(problem, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_')
	})

	var terms []string
	seen := make(map[string]bool)
	for _, w := range words {
		low := strings.ToLower(w)
		if len(low) < 4 || problemStopWords[low] || seen[low] {
			continue
		}
		seen[low] = true
		terms = append(terms, w) // keep original case for search
		if len(terms) >= 5 {
			break
		}
	}
	return terms
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// makeRelative converts an absolute path to repo-relative.
func makeRelative(path, root string) string {
	if root == "" || !strings.HasPrefix(path, "/") {
		return path
	}
	for _, prefix := range []string{root + "/", root} {
		if strings.HasPrefix(path, prefix) {
			return path[len(prefix):]
		}
	}
	return path
}

// isDocFile returns true for documentation and non-source files.
func isDocFile(path string) bool {
	low := strings.ToLower(path)
	return strings.HasSuffix(low, ".rst") ||
		strings.HasSuffix(low, ".md") ||
		strings.HasSuffix(low, ".txt") ||
		strings.HasSuffix(low, ".cfg") ||
		strings.HasSuffix(low, ".ini") ||
		strings.HasSuffix(low, ".toml") ||
		strings.HasPrefix(low, "doc/") ||
		strings.HasPrefix(low, "docs/") ||
		strings.Contains(low, "/doc/") ||
		strings.Contains(low, "/docs/") ||
		strings.HasPrefix(low, "changelog") ||
		strings.HasPrefix(low, "changes")
}

// generateReason creates a human-readable explanation for why a code block
// was included, based on its structural role and keyword relevance.
func generateReason(n *graph.Node, structural, keyword float64, candidates []*graph.Node) string {
	// Check if this is a root candidate.
	for _, c := range candidates {
		if c.ID == n.ID {
			return fmt.Sprintf("%s definition — direct target of investigation", n.Type)
		}
	}

	var parts []string
	if structural >= 0.8 {
		parts = append(parts, "structurally close")
	} else if structural >= 0.5 {
		parts = append(parts, "in dependency chain")
	} else if structural >= 0.2 {
		parts = append(parts, "transitively connected")
	}

	if keyword >= 0.3 {
		parts = append(parts, "strong keyword match")
	} else if keyword >= 0.1 {
		parts = append(parts, "partial keyword match")
	}

	if n.Metadata["doc"] != "" {
		docPreview := n.Metadata["doc"]
		if len(docPreview) > 60 {
			docPreview = docPreview[:60] + "..."
		}
		parts = append(parts, docPreview)
	}

	if len(parts) == 0 {
		return fmt.Sprintf("%s %s — related to investigation", n.Type, n.Name)
	}
	return strings.Join(parts, " — ")
}

func countUniqueFiles(blocks []investigateBlock) int {
	files := make(map[string]bool)
	for _, b := range blocks {
		files[b.File] = true
	}
	return len(files)
}

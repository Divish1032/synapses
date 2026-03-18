package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// tokensUsed estimates the token count of s.
// Code-heavy responses (identifiers, brackets, punctuation) tokenize more
// densely than prose: ~3.5 chars/token measured vs the naive 4 chars/token.
// Using 3.5 (integer: *2/7) gives a conservative estimate — we'd rather
// prune a few tokens early than silently over-budget on code contexts.
func tokensUsed(b *strings.Builder) int { return b.Len() * 2 / 7 }

// applyIntentCarveConfig stamps intent-specific edge weights and directional
// bias onto a CarveConfig returned by s.config.CarveConfig(). The per-intent
// weight maps are pre-allocated package-level vars (graph/types.go) — zero
// allocation. IntentID is set so the subgraph cache stores intent results
// separately, preventing cross-intent cache collisions.
func applyIntentCarveConfig(cfg *graph.CarveConfig, intent string) {
	cfg.EdgeWeights = graph.IntentCarveWeights(intent)
	cfg.DirectionBoost = graph.IntentDirectionBoost(intent)
	cfg.IntentID = intent
}

// aggregatedImpact runs ImpactAnalysis and, for struct/interface nodes, aggregates
// impact across all methods (same logic as handleGetImpact). This ensures plan/modify/review
// intents show meaningful blast radius for struct types.
func (s *Server) aggregatedImpact(node *graph.Node, depth int) *graph.ImpactResult {
	if node.Type == graph.NodeStruct || node.Type == graph.NodeInterface {
		methods := s.graph.FindByPattern(node.Name)
		merged := &graph.ImpactResult{Tiers: []graph.ImpactTier{}}
		seen := make(map[graph.NodeID]bool)
		for _, m := range methods {
			if m.Type != graph.NodeMethod || m.ID == node.ID {
				continue
			}
			r, err := s.graph.ImpactAnalysis(m.ID, depth)
			if err != nil || r == nil {
				continue
			}
			for _, tier := range r.Tiers {
				var tierNodes []graph.EntityRef
				for _, ref := range tier.Nodes {
					if !seen[ref.ID] {
						seen[ref.ID] = true
						tierNodes = append(tierNodes, ref)
					}
				}
				if len(tierNodes) == 0 {
					continue
				}
				found := false
				for i, mt := range merged.Tiers {
					if mt.Label == tier.Label {
						merged.Tiers[i].Nodes = append(merged.Tiers[i].Nodes, tierNodes...)
						merged.Tiers[i].TotalNodes += len(tierNodes)
						found = true
						break
					}
				}
				if !found {
					merged.Tiers = append(merged.Tiers, graph.ImpactTier{
						Label:      tier.Label,
						Depth:      tier.Depth,
						Confidence: tier.Confidence,
						Nodes:      tierNodes,
						TotalNodes: len(tierNodes),
					})
				}
				merged.AffectedFiles = append(merged.AffectedFiles, r.AffectedFiles...)
				merged.TotalAffected += len(tierNodes)
			}
		}
		seenFiles := make(map[string]bool)
		uniq := merged.AffectedFiles[:0]
		for _, f := range merged.AffectedFiles {
			if !seenFiles[f] {
				seenFiles[f] = true
				uniq = append(uniq, f)
			}
		}
		merged.AffectedFiles = uniq
		return merged
	}
	r, _ := s.graph.ImpactAnalysis(node.ID, depth)
	return r
}

// budgetLeft returns how many tokens remain given the budget and current output.
// Always returns at least 0.
func budgetLeft(b *strings.Builder, budget int) int {
	rem := budget - tokensUsed(b)
	if rem < 0 {
		return 0
	}
	return rem
}

// resolvedTarget holds the result of resolving a prepare_context target string.
// bestNode is the highest-connectivity match; candidates holds all matches when
// there are multiple (len > 1 = ambiguous).
type resolvedTarget struct {
	bestNode   *graph.Node
	candidates []*graph.Node // len > 1 = ambiguous
	file       string        // set when target resolved as a file path
	isFile     bool
	isConcept  bool // fell through to semantic search (no graph match)
}

// resolveTarget maps a free-form target string to graph nodes.
// Resolution order:
//  1. Exact entity name (FindByName)
//  2. If file hint given, filter to that file
//  3. File path (FindByFile)
//  4. Pattern / substring (FindByPattern)
//  5. Concept fallback — isConcept=true (caller may run semantic search)
//
// Never returns a hard error — callers always get a best-effort result.
func (s *Server) resolveTarget(target, fileHint string) *resolvedTarget {
	// 1. Exact / case-insensitive name match.
	nodes := s.graph.FindByName(target)

	// 2. Filter by file hint if provided.
	if len(nodes) > 0 && fileHint != "" {
		var filtered []*graph.Node
		for _, n := range nodes {
			if strings.HasSuffix(n.File, fileHint) || strings.Contains(n.File, fileHint) {
				filtered = append(filtered, n)
			}
		}
		if len(filtered) > 0 {
			nodes = filtered
		}
	}

	if len(nodes) > 0 {
		best := pickBestNode(nodes, s.graph)
		return &resolvedTarget{bestNode: best, candidates: nodes}
	}

	// 3. Try as file path.
	fileNodes := s.graph.FindByFile(target)
	if len(fileNodes) > 0 {
		return &resolvedTarget{
			candidates: fileNodes,
			bestNode:   fileNodes[0],
			file:       target,
			isFile:     true,
		}
	}

	// 4. Pattern / substring match.
	nodes = s.graph.FindByPattern(target)
	if len(nodes) > 0 {
		best := pickBestNode(nodes, s.graph)
		return &resolvedTarget{bestNode: best, candidates: nodes}
	}

	// 5. No match — signal concept fallback.
	return &resolvedTarget{isConcept: true}
}

// choiceMapHeader prepends a compact disambiguation block when the target is
// ambiguous (multiple candidates). Returns "" when unambiguous.
// The caller should prepend this to the assembled content.
func (s *Server) choiceMapHeader(resolved *resolvedTarget, target string) string {
	if resolved == nil || len(resolved.candidates) <= 1 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Ambiguous Target: %q (%d matches)\n", target, len(resolved.candidates))

	// Show up to 5 candidates — non-test nodes first, then test nodes.
	nonTest := make([]*graph.Node, 0, len(resolved.candidates))
	testNodes := make([]*graph.Node, 0)
	for _, n := range resolved.candidates {
		if strings.HasSuffix(n.File, "_test.go") {
			testNodes = append(testNodes, n)
		} else {
			nonTest = append(nonTest, n)
		}
	}
	shown := append(nonTest, testNodes...)
	if len(shown) > 5 {
		shown = shown[:5]
	}
	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	for i, n := range shown {
		relFile := strings.TrimPrefix(n.File, prefix)
		line := ""
		if n.Line > 0 {
			line = fmt.Sprintf(":%d", n.Line)
		}
		summary := ""
		if n.Metadata != nil {
			if doc := n.Metadata["doc"]; doc != "" {
				if len(doc) > 80 {
					doc = doc[:77] + "…"
				}
				summary = "   Summary: " + doc
			}
		}
		fmt.Fprintf(&b, "%d. [%s] %s · %s%s\n%s\n", i+1, n.Name, n.Type, relFile, line, summary)
	}

	best := resolved.bestNode
	bestFile := strings.TrimPrefix(best.File, prefix)
	fmt.Fprintf(&b, "\n→ Re-call with target=%q or file=%q to pin.\n\n",
		best.Name, bestFile)
	fmt.Fprintf(&b, "## Best-Guess Context (%s — highest connectivity):\n", best.Name)
	return b.String()
}

// intentDefaultBudget returns the default token budget for each intent.
func intentDefaultBudget(intent string) int {
	switch intent {
	case "modify", "review":
		return 3000
	case "debug":
		return 3500
	case "understand":
		return 2000
	case "add", "plan":
		return 2000
	default:
		return 2000
	}
}

// handlePrepareContext is the intent-based context assembly tool.
// It composes context from multiple internal sources in one round-trip,
// replacing chains like get_context→get_impact→get_violations.
func (s *Server) handlePrepareContext(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	intent := stringArg(req, "intent")
	target := stringArg(req, "target")
	fileHint := stringArg(req, "file")
	taskID := stringArg(req, "task_id")
	projectsParam := stringArg(req, "projects")

	if target == "" {
		return mcp.NewToolResultError("target is required"), nil
	}
	if intent == "" {
		intent = "understand"
	}

	tokenBudget := intentDefaultBudget(intent)
	if b, ok := req.GetArguments()["token_budget"].(float64); ok && b > 0 {
		tokenBudget = int(b)
	}

	resolved := s.resolveTarget(target, fileHint)

	if resolved.isConcept {
		// When projects= is specified and local resolution failed, try sibling stores.
		if projectsParam != "" && s.federationResolver != nil {
			var aliases []string
			if projectsParam != "*" {
				for _, a := range strings.Split(projectsParam, ",") {
					if a = strings.TrimSpace(a); a != "" {
						aliases = append(aliases, a)
					}
				}
			}
			fedCtx, fedCancel := context.WithTimeout(ctx, 2*time.Second)
			fedResults := s.federationResolver.FindEntities(fedCtx, target, aliases, 5)
			fedCancel()
			if len(fedResults) > 0 {
				var sb strings.Builder
				fmt.Fprintf(&sb, "## Not found locally: %q\n\nFound in sibling projects:\n", target)
				for _, fr := range fedResults {
					for _, r := range fr.Results {
						fmt.Fprintf(&sb, "- [%s] %s::%s\n", fr.Alias, fr.Alias, r.Name)
					}
				}
				sb.WriteString("\nUse get_context(entity=\"Name\", projects=\"alias\") to explore.")
				return mcp.NewToolResultText(sb.String()), nil
			}
		}
		// No graph match at all — return helpful hint.
		return mcp.NewToolResultText(fmt.Sprintf(
			"## No Match: %q\n\nNo entity or file found matching %q.\n"+
				"Try: search(query=%q, mode=\"semantic\") to find by concept, "+
				"or find_entity(query=%q) for exact names.",
			target, target, target, target,
		)), nil
	}

	var b strings.Builder

	// Prepend disambiguation header if ambiguous — skip for file targets (all entities in file are expected).
	if !resolved.isFile {
		b.WriteString(s.choiceMapHeader(resolved, target))
	}

	switch intent {
	case "modify":
		s.assembleModifyContext(ctx, &b, resolved, taskID, tokenBudget)
	case "review":
		s.assembleReviewContext(ctx, &b, resolved, taskID, tokenBudget)
	case "debug":
		s.assembleDebugContext(ctx, &b, resolved, taskID, tokenBudget)
	case "add":
		s.assembleAddContext(ctx, &b, resolved, taskID, tokenBudget)
	case "plan":
		s.assemblePlanContext(ctx, &b, resolved, taskID, tokenBudget)
	default: // "understand" and unknown intents
		s.assembleUnderstandContext(ctx, &b, resolved, taskID, tokenBudget)
	}

	return mcp.NewToolResultText(strings.TrimSpace(b.String())), nil
}

// ---------------------------------------------------------------------------
// Intent assemblers
// ---------------------------------------------------------------------------

// assembleModifyContext builds the "modify" intent response:
// target + blast radius + violations + callees + agent notes + checklist.
func (s *Server) assembleModifyContext(
	ctx context.Context,
	b *strings.Builder,
	resolved *resolvedTarget,
	taskID string,
	budget int,
) {
	node := resolved.bestNode
	cfg := s.config.CarveConfig()
	cfg.MaxDepth = 2
	applyIntentCarveConfig(&cfg, "modify")

	sg, err := s.graph.CarveEgoGraph(node.ID, cfg)
	if err != nil {
		fmt.Fprintf(b, "## Error\nCould not carve context: %v\n", err)
		return
	}
	sg = normalizeSubgraph(sg, s.graph.Root())
	dc := toDirectionalContext(sg)
	pkt := s.buildBrainPacket(ctx, node, dc, taskID)
	dc.ContextPacket = pkt

	// Load annotations.
	if s.store != nil {
		nodeIDs := make([]string, 0, len(sg.Nodes)+1)
		nodeIDs = append(nodeIDs, string(sg.Root))
		for _, cn := range sg.Nodes {
			nodeIDs = append(nodeIDs, string(cn.Node.ID))
		}
		if annMap, err2 := s.store.GetAnnotationsForNodes(nodeIDs); err2 == nil {
			dc.Annotations = annMap
		}
	}

	// Header.
	writeNodeHeader(b, node, getRootSummary(node, pkt))

	// Blast radius — always shown; use struct aggregation for struct/interface nodes.
	impact := s.aggregatedImpact(node, 2)
	if impact != nil {
		fmt.Fprintf(b, "\n## Blast Radius (%d affected)\n", impact.TotalAffected)
		if len(impact.Tiers) > 0 {
			for _, tier := range impact.Tiers {
				names := make([]string, 0, len(tier.Nodes))
				for _, ref := range tier.Nodes {
					names = append(names, ref.Name)
				}
				label := strings.ToUpper(tier.Label)
				fmt.Fprintf(b, "%s: %s\n", label, strings.Join(names, ", "))
			}
		}
	} else {
		b.WriteString("No compile-time callers tracked (may be invoked via interface or dispatcher).\n")
	}

	// Dependencies (callees) — always relevant, write directly.
	if budgetLeft(b, budget) > 200 && len(dc.Callees) > 0 {
		names := make([]string, 0, len(dc.Callees))
		for _, c := range dc.Callees {
			names = append(names, c.Node.Name)
		}
		fmt.Fprintf(b, "\n## Dependencies (callees)\n%s\n", strings.Join(names, " · "))
	}

	// Pre-edit checklist — always relevant, write directly.
	if budgetLeft(b, budget) > 80 {
		b.WriteString("\n## Pre-Edit Checklist\n")
		callerCount := impact.TotalAffected
		if callerCount == 0 {
			callerCount = len(dc.Callers)
		}
		if callerCount > 0 {
			fmt.Fprintf(b, "- %d caller(s) must remain compatible\n", callerCount)
		}
		if fileHasTests(node.File) {
			fmt.Fprintf(b, "- Test file exists: update tests accordingly\n")
		} else {
			fmt.Fprintf(b, "- ⚠ No test file found — consider adding tests before changing\n")
		}
	}

	// ── Tiered supplementary sections ────────────────────────────────────
	// Architecture rules, quality gaps, brain warnings, annotations, and
	// cross-project deps are all rendered via the tiered visibility system.
	// Critical items (error-severity violations, high/critical gaps, drifted
	// deps) are always shown. Relevant items are 1-line summaries within
	// budget. Available items are a single discover_tools hint.
	var sections []tieredSection
	if vs := collectViolationSection(s.config, node.File); vs != nil {
		sections = append(sections, *vs)
	}
	if gs := collectGapSection(s.store, string(node.ID)); gs != nil {
		sections = append(sections, *gs)
	}
	if bs := collectBrainSection(pkt); bs != nil {
		sections = append(sections, *bs)
	}
	if as := collectAnnotationSection(s.store, string(node.ID)); as != nil {
		sections = append(sections, *as)
	}
	sections = append(sections, s.collectCrossProjectSections(ctx, string(node.ID))...)
	// Available tier: historical failure episodes and full impact analysis.
	sections = append(sections, tieredSection{
		Tier: "available", Heading: "Historical failures, full impact analysis",
	})
	renderTiered(b, sections, budget)
}

// assembleUnderstandContext builds the "understand" intent response:
// target + structure + callees/callers + ADRs + summaries.
func (s *Server) assembleUnderstandContext(
	ctx context.Context,
	b *strings.Builder,
	resolved *resolvedTarget,
	taskID string,
	budget int,
) {
	node := resolved.bestNode
	cfg := s.config.CarveConfig()
	cfg.MaxDepth = 2
	applyIntentCarveConfig(&cfg, "understand")

	sg, err := s.graph.CarveEgoGraph(node.ID, cfg)
	if err != nil {
		fmt.Fprintf(b, "## Error\n%v\n", err)
		return
	}
	sg = normalizeSubgraph(sg, s.graph.Root())
	dc := toDirectionalContext(sg)
	pkt := s.buildBrainPacket(ctx, node, dc, taskID)
	dc.ContextPacket = pkt

	// ADRs.
	if bc := s.getBrainClient(); bc != nil && node.File != "" {
		if adrs, adrErr := bc.GetADRs(ctx, node.File); adrErr == nil && len(adrs) > 0 {
			if len(adrs) > 2 {
				adrs = adrs[:2]
			}
			dc.ADRs = adrs
		}
	}

	// Degrade detail level based on remaining budget.
	detailLevel := "full"
	if budget < 1500 {
		detailLevel = "neighbors"
	}
	if budget < 500 {
		detailLevel = "summary"
	}
	b.WriteString(serializeCompact(dc, detailLevel))

	// ── Tiered supplementary sections ────────────────────────────────────
	var sections []tieredSection
	if gs := collectGapSection(s.store, string(node.ID)); gs != nil {
		sections = append(sections, *gs)
	}
	if as := collectAnnotationSection(s.store, string(node.ID)); as != nil {
		sections = append(sections, *as)
	}
	sections = append(sections, s.collectCrossProjectSections(ctx, string(node.ID))...)
	sections = append(sections, tieredSection{
		Tier: "available", Heading: "Full impact analysis, historical failures, peer activity",
	})
	renderTiered(b, sections, budget)
}

// assembleReviewContext builds the "review" intent response:
// target + metadata + concerns + violations + blast radius.
func (s *Server) assembleReviewContext(
	ctx context.Context,
	b *strings.Builder,
	resolved *resolvedTarget,
	taskID string,
	budget int,
) {
	node := resolved.bestNode
	cfg := s.config.CarveConfig()
	cfg.MaxDepth = 1
	applyIntentCarveConfig(&cfg, "review")

	sg, err := s.graph.CarveEgoGraph(node.ID, cfg)
	if err != nil {
		fmt.Fprintf(b, "## Error\n%v\n", err)
		return
	}
	sg = normalizeSubgraph(sg, s.graph.Root())
	dc := toDirectionalContext(sg)
	pkt := s.buildBrainPacket(ctx, node, dc, taskID)

	// Header with metadata.
	var extras []string
	if node.Metadata != nil {
		if c := node.Metadata["complexity"]; c != "" && c != "0" && c != "1" {
			extras = append(extras, "complexity:"+c)
		}
	}
	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	relFile := strings.TrimPrefix(node.File, prefix)
	header := fmt.Sprintf("[%s] %s · %s", node.Name, node.Type, relFile)
	if node.Line > 0 {
		header = fmt.Sprintf("[%s] %s · %s:%d", node.Name, node.Type, relFile, node.Line)
	}
	if len(extras) > 0 {
		header += " · " + strings.Join(extras, " · ")
	}
	b.WriteString(header + "\n")
	if pkt != nil && pkt.RootSummary != "" {
		fmt.Fprintf(b, "Summary: %s\n", pkt.RootSummary)
	}

	// Coupling metrics — core content, always shown.
	fanIn := s.graph.Fanin(node.ID)
	b.WriteString("\n## Coupling\n")
	fmt.Fprintf(b, "Fan-in (callers): %d | Callees: %d | Related: %d\n",
		fanIn, len(dc.Callees), len(dc.Related))
	if hasTests := fileHasTests(node.File); hasTests {
		b.WriteString("Test coverage: test file exists\n")
	} else {
		b.WriteString("Test coverage: NO test file found\n")
	}

	// Blast radius — core content, always shown.
	impact := s.aggregatedImpact(node, 3)
	fmt.Fprintf(b, "\n## Blast Radius (%d total across %d files)\n",
		impact.TotalAffected, len(impact.AffectedFiles))
	for _, tier := range impact.Tiers {
		names := make([]string, 0, len(tier.Nodes))
		for _, ref := range tier.Nodes {
			names = append(names, ref.Name)
		}
		label := strings.ToUpper(tier.Label)
		fmt.Fprintf(b, "%s (%d): %s\n", label, tier.TotalNodes, strings.Join(names, ", "))
	}
	if len(impact.Tiers) == 0 && fanIn == 0 {
		b.WriteString("No compile-time callers tracked.\n")
	}
	_ = dc // used for pkt building above

	// ── Tiered supplementary sections ────────────────────────────────────
	var sections []tieredSection
	if vs := collectViolationSection(s.config, node.File); vs != nil {
		sections = append(sections, *vs)
	}
	if gs := collectGapSection(s.store, string(node.ID)); gs != nil {
		sections = append(sections, *gs)
	}
	if bs := collectBrainSection(pkt); bs != nil {
		sections = append(sections, *bs)
	}
	if as := collectAnnotationSection(s.store, string(node.ID)); as != nil {
		sections = append(sections, *as)
	}
	sections = append(sections, s.collectCrossProjectSections(ctx, string(node.ID))...)
	sections = append(sections, tieredSection{
		Tier: "available", Heading: "Historical failures, full peer activity, violation audit log",
	})
	renderTiered(b, sections, budget)
}

// assembleDebugContext builds the "debug" intent response:
// target + call paths + downstream effects.
func (s *Server) assembleDebugContext(
	ctx context.Context,
	b *strings.Builder,
	resolved *resolvedTarget,
	taskID string,
	budget int,
) {
	node := resolved.bestNode
	cfg := s.config.CarveConfig()
	cfg.MaxDepth = 3
	applyIntentCarveConfig(&cfg, "debug")

	sg, err := s.graph.CarveEgoGraph(node.ID, cfg)
	if err != nil {
		fmt.Fprintf(b, "## Error\n%v\n", err)
		return
	}
	sg = normalizeSubgraph(sg, s.graph.Root())
	dc := toDirectionalContext(sg)
	pkt := s.buildBrainPacket(ctx, node, dc, taskID)

	writeNodeHeader(b, node, getRootSummary(node, pkt))

	// Call path: callers show what calls into this (upstream).
	if len(dc.Callers) > 0 {
		b.WriteString("\n## Upstream (what calls this)\n")
		for _, c := range dc.Callers {
			root := s.graph.Root()
			prefix := root
			if prefix != "" && !strings.HasSuffix(prefix, "/") {
				prefix += "/"
			}
			relFile := strings.TrimPrefix(c.Node.File, prefix)
			fmt.Fprintf(b, "← %s (%s · %s)\n", c.Node.Name, c.Node.Type, relFile)
		}
	}

	// Optional tier 1: Downstream callees (strip when tight).
	if budgetLeft(b, budget) > 300 && len(dc.Callees) > 0 {
		b.WriteString("\n## Downstream (what this calls)\n")
		root := s.graph.Root()
		prefix := root
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		for _, c := range dc.Callees {
			relFile := strings.TrimPrefix(c.Node.File, prefix)
			sum := getDepSummary(c.Node.Name, pkt)
			if sum != "" {
				fmt.Fprintf(b, "→ %s (%s · %s)\n   %s\n", c.Node.Name, c.Node.Type, relFile, sum)
			} else {
				fmt.Fprintf(b, "→ %s (%s · %s)\n", c.Node.Name, c.Node.Type, relFile)
			}
		}
	}

	// Related state — core content for debug.
	if budgetLeft(b, budget) > 100 {
		b.WriteString("\n## Related State\n")
		if fileHasTests(node.File) {
			b.WriteString("Test file: exists\n")
		} else {
			b.WriteString("Test file: none\n")
		}
	}

	// ── Tiered supplementary sections ────────────────────────────────────
	var sections []tieredSection
	if bs := collectBrainSection(pkt); bs != nil {
		sections = append(sections, *bs)
	}
	if as := collectAnnotationSection(s.store, string(node.ID)); as != nil {
		sections = append(sections, *as)
	}
	sections = append(sections, s.collectCrossProjectSections(ctx, string(node.ID))...)
	sections = append(sections, tieredSection{
		Tier: "available", Heading: "Full impact analysis, architecture rules, quality gaps",
	})
	renderTiered(b, sections, budget)
}

// assembleAddContext builds the "add" intent response:
// file entity map + conventions + applicable rules.
func (s *Server) assembleAddContext(
	_ context.Context,
	b *strings.Builder,
	resolved *resolvedTarget,
	_ string,
	budget int,
) {
	node := resolved.bestNode
	targetFile := node.File

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	relFile := strings.TrimPrefix(targetFile, prefix)

	// All entities in this file.
	fileNodes := s.graph.FindByFile(targetFile)
	sort.Slice(fileNodes, func(i, j int) bool {
		return fileNodes[i].Line < fileNodes[j].Line
	})

	fmt.Fprintf(b, "## File: %s (%d entities)\n", relFile, len(fileNodes))
	for _, fn := range fileNodes {
		expMark := ""
		if fn.Exported {
			expMark = " (exported)"
		}
		fmt.Fprintf(b, "  %s %s · line %d%s\n", fn.Type, fn.Name, fn.Line, expMark)
	}

	// Detect conventions from existing methods.
	if node.Package != "" {
		fmt.Fprintf(b, "\n## Package: %s\n", node.Package)
	}

	// Architecture rules for this directory.
	rules := matchRulesForFile(s.config, targetFile)
	if len(rules) > 0 {
		b.WriteString("\n## Architecture Rules (apply here)\n")
		for _, r := range rules {
			fmt.Fprintf(b, "- [%s] %s: %s\n", r.Severity, r.RuleID, r.Description)
		}
	} else {
		b.WriteString("\n## Architecture Rules\nNone apply to this file.\n")
	}

	// Optional: Constitution principles (strip when budget is tight).
	if budgetLeft(b, budget) > 150 && s.config != nil &&
		s.config.Constitution.InjectInContext && len(s.config.Constitution.Principles) > 0 {
		b.WriteString("\n## Project Laws\n")
		for _, p := range s.config.Constitution.Principles {
			fmt.Fprintf(b, "- %s\n", p)
		}
	}
}

// assemblePlanContext builds the "plan" intent — a dry-run scope assessment.
// Returns the list of files, interfaces, rules, and risk level for a proposed
// change WITHOUT giving code context. "Think before you leap."
func (s *Server) assemblePlanContext(
	ctx context.Context,
	b *strings.Builder,
	resolved *resolvedTarget,
	_ string,
	budget int,
) {
	node := resolved.bestNode

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	relFile := strings.TrimPrefix(node.File, prefix)

	fmt.Fprintf(b, "## Change Plan: %s (%s)\n", node.Name, relFile)

	// Impact analysis (depth 3) to find all affected files — struct aggregation applied.
	impact := s.aggregatedImpact(node, 3)

	// Files you'll touch.
	b.WriteString("\n## Files You'll Touch\n")
	fileCount := 1
	fmt.Fprintf(b, "1. %s (target", relFile)
	fileNodes := s.graph.FindByFile(node.File)
	fmt.Fprintf(b, " — %d entities)\n", len(fileNodes))

	if fileHasTests(node.File) {
		fileCount++
		dir := filepath.Dir(relFile)
		base := filepath.Base(relFile)
		ext := filepath.Ext(base)
		stem := base[:len(base)-len(ext)]
		testFile := filepath.Join(dir, stem+"_test"+ext)
		fmt.Fprintf(b, "%d. %s (tests — update required)\n", fileCount, testFile)
	}

	// Callers grouped by file.
	callerFiles := make(map[string][]string) // relFile → []callerName
	if impact != nil {
		for _, tier := range impact.Tiers {
			for _, ref := range tier.Nodes {
				rf := strings.TrimPrefix(ref.File, prefix)
				if rf != relFile {
					callerFiles[rf] = append(callerFiles[rf], ref.Name)
				}
			}
		}
	}

	// Stable file order.
	orderedFiles := make([]string, 0, len(callerFiles))
	for f := range callerFiles {
		orderedFiles = append(orderedFiles, f)
	}
	sort.Strings(orderedFiles)

	for _, f := range orderedFiles {
		callers := callerFiles[f]
		fileCount++
		uses := "uses"
		if len(callers) == 1 {
			fmt.Fprintf(b, "%d. %s (caller — %s %s.%s)\n", fileCount, f, uses, node.Name, callers[0])
		} else {
			fmt.Fprintf(b, "%d. %s (%d callers: %s)\n", fileCount, f, len(callers), strings.Join(callers, ", "))
		}
	}

	// Interface contracts.
	cfg := s.config.CarveConfig()
	cfg.MaxDepth = 1
	applyIntentCarveConfig(&cfg, "plan")
	if sg, sgErr := s.graph.CarveEgoGraph(node.ID, cfg); sgErr == nil {
		var interfaces []string
		for _, cn := range sg.Nodes {
			if cn.Node.Type == graph.NodeInterface {
				rfIface := strings.TrimPrefix(cn.Node.File, prefix)
				interfaces = append(interfaces, fmt.Sprintf("%s (%s:%d)", cn.Node.Name, rfIface, cn.Node.Line))
			}
		}
		if len(interfaces) > 0 {
			b.WriteString("\n## Interfaces to Preserve\n")
			for _, iface := range interfaces {
				fmt.Fprintf(b, "- %s\n", iface)
			}
			b.WriteString("Any signature change requires updating the interface.\n")
		}
	}

	// Scope assessment and risk.
	b.WriteString("\n## Scope Assessment\n")
	directCallers := 0
	if impact != nil && len(impact.Tiers) > 0 {
		directCallers = impact.Tiers[0].TotalNodes
	}
	testMark := "none"
	if fileHasTests(node.File) {
		testMark = "1"
	}
	ifaceCount := 0
	if sg, sgErr := s.graph.CarveEgoGraph(node.ID, cfg); sgErr == nil {
		for _, cn := range sg.Nodes {
			if cn.Node.Type == graph.NodeInterface {
				ifaceCount++
			}
		}
	}
	fmt.Fprintf(b, "Files: %d · Direct callers: %d · Interfaces: %d · Test files: %s\n",
		fileCount, directCallers, ifaceCount, testMark)

	risk := "LOW"
	if directCallers >= 3 || ifaceCount > 0 {
		risk = "MEDIUM"
	}
	if directCallers >= 5 || (directCallers >= 3 && ifaceCount > 0) {
		risk = "HIGH"
	}
	fmt.Fprintf(b, "Risk: %s\n", risk)

	// Optional: Recommendation (strip only if extremely tight).
	if budgetLeft(b, budget) > 50 {
		b.WriteString("\n## Recommendation\n")
		fmt.Fprintf(b, "Consider using claim_work(scope=%q) before starting.\n", relFile)
		if risk == "HIGH" {
			b.WriteString("HIGH risk: run prepare_context(intent=\"modify\") before editing.\n")
		}
	}

	// ── Tiered supplementary sections ────────────────────────────────────
	var sections []tieredSection
	if vs := collectViolationSection(s.config, node.File); vs != nil {
		sections = append(sections, *vs)
	}
	if gs := collectGapSection(s.store, string(node.ID)); gs != nil {
		sections = append(sections, *gs)
	}
	sections = append(sections, s.collectCrossProjectSections(ctx, string(node.ID))...)
	sections = append(sections, tieredSection{
		Tier: "available", Heading: "Full impact analysis, historical failures",
	})
	renderTiered(b, sections, budget)
}

// ---------------------------------------------------------------------------
// Shared helpers used by multiple assemblers
// ---------------------------------------------------------------------------

// buildBrainPacket assembles a ContextPacket from the brain client.
// Checks the 30s packet cache first; falls back to nil if brain unavailable.
// This is extracted here so both handleGetContext and intent assemblers share it.
// Returns a cached packet if available, otherwise kicks off async enrichment
// and returns nil (caller proceeds without brain enrichment).
func (s *Server) buildBrainPacket(
	_ context.Context,
	node *graph.Node,
	dc *directionalContext,
	taskID string,
) *brain.ContextPacket {
	bc := s.getBrainClient()
	if bc == nil {
		return nil
	}

	cacheKey := fmt.Sprintf("%s:%d", node.Name, 2)
	if cached := s.getPacketFromCache(cacheKey); cached != nil {
		return cached.(*brain.ContextPacket)
	}

	// Async enrichment: fire background goroutine, return nil for this call.
	go s.asyncEnrichContext(bc, cacheKey, dc, node, taskID)
	return nil
}

// writeWarnings appends brain warnings/concerns to b, prefixed with ⚠.
func writeWarnings(b *strings.Builder, pkt *brain.ContextPacket) {
	if pkt == nil {
		return
	}
	all := append(pkt.GraphWarnings, pkt.Concerns...) //nolint:gocritic
	if len(all) > 0 {
		b.WriteString("\n## Warnings\n")
		for _, w := range all {
			fmt.Fprintf(b, "⚠ %s\n", w)
		}
	}
}

// writeAnnotations appends agent annotations to b, sorted by node ID then time.
func writeAnnotations(b *strings.Builder, annMap map[string][]store.Annotation, rootID graph.NodeID) {
	if len(annMap) == 0 {
		return
	}
	// Collect notes on the root node first, then others.
	var rootNotes []store.Annotation
	var otherNotes []store.Annotation
	for nodeID, anns := range annMap {
		for _, ann := range anns {
			if graph.NodeID(nodeID) == rootID {
				rootNotes = append(rootNotes, ann)
			} else {
				otherNotes = append(otherNotes, ann)
			}
		}
	}
	all := append(rootNotes, otherNotes...) //nolint:gocritic
	if len(all) == 0 {
		return
	}
	b.WriteString("\n## Agent Notes\n")
	for _, ann := range all {
		age := formatAge(ann.CreatedAt)
		fmt.Fprintf(b, "[%s, %s] %s\n", ann.AgentID, age, ann.Note)
	}
}

// formatAge returns a human-readable age string for a RFC3339 timestamp.
func formatAge(createdAt string) string {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// handlePlanContext is the single-call pre-implementation gate. It runs three
// checks in one round-trip that agents normally chain manually:
//  1. check_plan_safety  — searches failure episodes for past matches (500ms cap)
//  2. validate_plan      — checks proposed changes against architectural rules
//  3. prepare_context(intent=plan) — scope assessment: files, interfaces, risk level
//
// Verdict field summarises the overall result:
//   - "clear"      — no past failures, no violations
//   - "warnings"   — past failure match found (review rationale)
//   - "violations" — architectural rules broken
//   - "blocked"    — both warnings and violations present
func (s *Server) handlePlanContext(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	target := stringArg(req, "target")
	if target == "" {
		return mcp.NewToolResultError("target is required"), nil
	}
	fileHint := stringArg(req, "file")
	taskID := stringArg(req, "task_id")
	changesRaw := stringArg(req, "changes")
	planDesc := stringArg(req, "plan_description")

	result := map[string]interface{}{}

	// ── 1. Episodic safety check ─────────────────────────────────────────
	var safetyStatus string
	if s.store != nil {
		desc := planDesc
		if desc == "" {
			desc = target
		}
		type safetyRes struct {
			ep  *store.Episode
			err error
		}
		ch := make(chan safetyRes, 1)
		go func() {
			ep, err := s.store.CheckPlanSafety(desc, "")
			ch <- safetyRes{ep, err}
		}()
		safetyCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		select {
		case r := <-ch:
			if r.err == nil && r.ep != nil {
				safetyStatus = "warnings"
				result["safety_check"] = map[string]interface{}{
					"status": "warning",
					"match": map[string]interface{}{
						"episode_id": r.ep.ID,
						"decision":   r.ep.Decision,
						"outcome":    r.ep.Outcome,
						"rationale":  r.ep.Rationale,
					},
					"message": fmt.Sprintf("⚠ Past failure match: %q (outcome: %s). Review rationale before proceeding.", r.ep.Decision, r.ep.Outcome),
				}
			} else {
				safetyStatus = "clear"
				result["safety_check"] = map[string]interface{}{"status": "clear"}
			}
		case <-safetyCtx.Done():
			safetyStatus = "clear"
			result["safety_check"] = map[string]interface{}{"status": "clear", "note": "timed out (>500ms)"}
		}
	}

	// ── 2. Structural validation ──────────────────────────────────────────
	var violationsStatus string
	if changesRaw != "" {
		var changes []ProposedChange
		if err := json.Unmarshal([]byte(changesRaw), &changes); err == nil {
			overlay := cloneGraph(s.graph)
			var skipped []string
			for _, change := range changes {
				if change.AddsCallTo == "" {
					continue
				}
				callees := overlay.FindByName(change.AddsCallTo)
				if len(callees) == 0 {
					skipped = append(skipped, fmt.Sprintf("%q not in graph — skipped", change.AddsCallTo))
					continue
				}
				sources := overlay.FindByFile(change.File)
				if len(sources) == 0 {
					skipped = append(skipped, fmt.Sprintf("file %q not in graph — skipped", change.File))
					continue
				}
				for _, callee := range callees {
					overlay.AddEdge(&graph.Edge{From: sources[0].ID, To: callee.ID, Type: graph.EdgeCalls})
				}
			}
			s.rulesMu.RLock()
			violations := s.config.CheckViolations(overlay)
			s.rulesMu.RUnlock()

			if len(violations) > 0 {
				violationsStatus = "violations"
				result["validation"] = map[string]interface{}{
					"status":     "violations_found",
					"violations": violations,
				}
			} else {
				violationsStatus = "ok"
				validation := map[string]interface{}{"status": "ok"}
				if len(skipped) > 0 {
					validation["skipped"] = skipped
				}
				result["validation"] = validation
			}
		}
	}

	// ── 3. Scope assessment via plan intent ───────────────────────────────
	resolved := s.resolveTarget(target, fileHint)
	if !resolved.isConcept {
		tokenBudget := 2000
		var scopeBuilder strings.Builder
		s.assemblePlanContext(ctx, &scopeBuilder, resolved, taskID, tokenBudget)
		if scopeBuilder.Len() > 0 {
			result["scope_context"] = scopeBuilder.String()
		}
	}

	// ── Verdict ───────────────────────────────────────────────────────────
	hasViolations := violationsStatus == "violations"
	hasWarnings := safetyStatus == "warnings"
	switch {
	case hasViolations && hasWarnings:
		result["verdict"] = "blocked"
		result["verdict_message"] = "⛔ Both past failures and architectural violations detected. Do not proceed without resolving violations and reviewing the failure history."
	case hasViolations:
		result["verdict"] = "violations"
		result["verdict_message"] = "⛔ Architectural violations detected. Fix before implementing."
	case hasWarnings:
		result["verdict"] = "warnings"
		result["verdict_message"] = "⚠ Past failure match found. Review the safety_check entry — agent decides relevance."
	default:
		result["verdict"] = "clear"
		result["verdict_message"] = "✓ No past failures, no architectural violations. Safe to proceed."
	}

	return jsonResult(result)
}

// ── Tiered visibility ────────────────────────────────────────────────────
// Tiered visibility applies to ALL prepare_context responses, not just
// federation-enriched ones. Every supplementary section is tagged with a
// visibility tier:
//   - critical: always shown, never budget-trimmed
//   - relevant: 1-line summary within budget, agent decides to explore
//   - available: single hint line pointing to discover_tools
//
// The core BFS context (header + ego-graph) is always rendered first.
// Tiered sections are rendered after, in priority order.

// tieredSection represents one supplementary section in a prepare_context
// response, tagged with its visibility tier for budget-aware rendering.
type tieredSection struct {
	Tier    string // "critical" | "relevant" | "available"
	Heading string // section heading (e.g., "⚠ Alerts", "Quality Gaps")
	Content string // pre-rendered content for this section
}

// renderTiered writes tiered sections to the builder in priority order:
// all critical sections first (never trimmed), then relevant sections
// (within budget), then a single available hint if any sections exist.
func renderTiered(b *strings.Builder, sections []tieredSection, budget int) {
	if len(sections) == 0 {
		return
	}

	// Critical: always shown, never trimmed.
	for _, s := range sections {
		if s.Tier == "critical" {
			fmt.Fprintf(b, "\n## %s\n%s", s.Heading, s.Content)
		}
	}

	// Relevant: 1-line summaries within budget.
	for _, s := range sections {
		if s.Tier == "relevant" && budgetLeft(b, budget) > 50 {
			fmt.Fprintf(b, "\n## %s\n%s", s.Heading, s.Content)
		}
	}

	// Available: single hint line if there are any available-tier sections.
	var availNames []string
	for _, s := range sections {
		if s.Tier == "available" {
			availNames = append(availNames, s.Heading)
		}
	}
	if len(availNames) > 0 && budgetLeft(b, budget) > 30 {
		fmt.Fprintf(b, "\n## More Available\n- %s → discover_tools(query=\"...\")\n",
			strings.Join(availNames, ", "))
	}
}

// collectViolationSection builds a tiered section for architecture rule
// violations. Violations with severity "error" are critical; "warning" is relevant.
func collectViolationSection(cfg *config.Config, file string) *tieredSection {
	rules := matchRulesForFile(cfg, file)
	if len(rules) == 0 {
		return nil
	}
	var sb strings.Builder
	hasCritical := false
	for _, r := range rules {
		fmt.Fprintf(&sb, "%s [%s]: %s\n", strings.ToUpper(r.Severity), r.RuleID, r.Description)
		if r.Severity == "error" {
			hasCritical = true
		}
	}
	tier := "relevant"
	if hasCritical {
		tier = "critical"
	}
	return &tieredSection{Tier: tier, Heading: "Architecture Rules", Content: sb.String()}
}

// collectGapSection builds a tiered section for open quality gaps on an entity.
func collectGapSection(st *store.Store, nodeID string) *tieredSection {
	if st == nil {
		return nil
	}
	gaps, err := st.GetGaps(store.GapFilter{NodeID: nodeID, Status: "open"})
	if err != nil || len(gaps) == 0 {
		return nil
	}
	var sb strings.Builder
	for _, g := range gaps {
		sev := g.Severity
		if sev == "" {
			sev = "medium"
		}
		fmt.Fprintf(&sb, "- [%s] %s: %s\n", sev, g.GapID, g.Description)
	}
	tier := "relevant"
	for _, g := range gaps {
		if g.Severity == "critical" || g.Severity == "high" {
			tier = "critical"
			break
		}
	}
	return &tieredSection{Tier: tier, Heading: fmt.Sprintf("Quality Gaps (%d open)", len(gaps)), Content: sb.String()}
}

// collectBrainSection builds a tiered section for brain warnings/concerns.
func collectBrainSection(pkt *brain.ContextPacket) *tieredSection {
	if pkt == nil {
		return nil
	}
	var items []string
	for _, w := range pkt.GraphWarnings {
		items = append(items, w)
	}
	for _, c := range pkt.Concerns {
		items = append(items, c)
	}
	if len(items) == 0 {
		return nil
	}
	var sb strings.Builder
	for _, item := range items {
		fmt.Fprintf(&sb, "- %s\n", item)
	}
	return &tieredSection{Tier: "relevant", Heading: "Warnings & Concerns", Content: sb.String()}
}

// collectAnnotationSection builds a tiered section for agent annotations.
func collectAnnotationSection(st *store.Store, nodeID string) *tieredSection {
	if st == nil {
		return nil
	}
	annMap, err := st.GetAnnotationsForNodes([]string{nodeID})
	if err != nil || len(annMap) == 0 {
		return nil
	}
	var sb strings.Builder
	for _, anns := range annMap {
		for _, ann := range anns {
			fmt.Fprintf(&sb, "- %s\n", ann.Note)
		}
	}
	if sb.Len() == 0 {
		return nil
	}
	return &tieredSection{Tier: "relevant", Heading: "Agent Notes", Content: sb.String()}
}

// collectCrossProjectSection builds tiered sections for cross-project deps.
func (s *Server) collectCrossProjectSections(ctx context.Context, entityID string) []tieredSection {
	if s.federationResolver == nil || s.store == nil {
		return nil
	}
	fedCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	deps := s.federationResolver.GetDepsForEntity(fedCtx, entityID, s.store)
	if len(deps) == 0 {
		return nil
	}

	var sections []tieredSection
	var criticalDeps, relevantDeps []federation.CrossProjectDepStatus
	for _, dep := range deps {
		if dep.Drifted {
			criticalDeps = append(criticalDeps, dep)
		} else {
			relevantDeps = append(relevantDeps, dep)
		}
	}

	if len(criticalDeps) > 0 {
		var sb strings.Builder
		for _, dep := range criticalDeps {
			fmt.Fprintf(&sb, "- BREAKING: %s::%s — %s\n", dep.Project, dep.Entity, dep.DiffSummary)
		}
		sections = append(sections, tieredSection{
			Tier: "critical", Heading: "⚠ Cross-Project Alerts", Content: sb.String(),
		})
	}

	if len(relevantDeps) > 0 {
		var sb strings.Builder
		for _, dep := range relevantDeps {
			fmt.Fprintf(&sb, "- %s::%s (%s) [current]\n", dep.Project, dep.Entity, dep.File)
		}
		sections = append(sections, tieredSection{
			Tier:    "relevant",
			Heading: fmt.Sprintf("Cross-Project Dependencies (%d current)", len(relevantDeps)),
			Content: sb.String(),
		})
	}

	return sections
}



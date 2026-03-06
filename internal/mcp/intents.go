package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// tokensUsed estimates the token count of s using the ~4 chars/token heuristic.
func tokensUsed(b *strings.Builder) int { return b.Len() / 4 }

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

	// Show up to 5 candidates with one-line summaries.
	shown := resolved.candidates
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
		// No graph match at all — return helpful hint.
		return mcp.NewToolResultText(fmt.Sprintf(
			"## No Match: %q\n\nNo entity or file found matching %q.\n"+
				"Try: search(query=%q, mode=\"semantic\") to find by concept, "+
				"or find_entity(query=%q) for exact names.",
			target, target, target, target,
		)), nil
	}

	var b strings.Builder

	// Prepend disambiguation header if ambiguous.
	b.WriteString(s.choiceMapHeader(resolved, target))

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

	// Blast radius.
	impact, ierr := s.graph.ImpactAnalysis(node.ID, 2)
	if ierr == nil && len(impact.Tiers) > 0 {
		total := impact.TotalAffected
		fmt.Fprintf(b, "\n## Blast Radius (%d affected)\n", total)
		for _, tier := range impact.Tiers {
			names := make([]string, 0, len(tier.Nodes))
			for _, ref := range tier.Nodes {
				names = append(names, ref.Name)
			}
			label := strings.ToUpper(tier.Label)
			fmt.Fprintf(b, "%s: %s\n", label, strings.Join(names, ", "))
		}
	}

	// Architecture rules.
	rules := matchRulesForFile(s.config, node.File)
	b.WriteString("\n## Architecture Rules\n")
	if len(rules) == 0 {
		b.WriteString("OK: no rules apply to this file\n")
	} else {
		for _, r := range rules {
			fmt.Fprintf(b, "%s [%s]: %s\n", strings.ToUpper(r.Severity), r.RuleID, r.Description)
		}
	}

	// Optional tier 1: Dependencies (callees) — strip first when budget is tight.
	if budgetLeft(b, budget) > 200 && len(dc.Callees) > 0 {
		names := make([]string, 0, len(dc.Callees))
		for _, c := range dc.Callees {
			names = append(names, c.Node.Name)
		}
		fmt.Fprintf(b, "\n## Dependencies (callees)\n%s\n", strings.Join(names, " · "))
	}

	// Optional tier 1: Brain warnings + concerns.
	if budgetLeft(b, budget) > 150 {
		writeWarnings(b, pkt)
	}

	// Optional tier 1: Agent annotations.
	if budgetLeft(b, budget) > 150 {
		writeAnnotations(b, dc.Annotations, node.ID)
	}

	// Optional tier 2: Pre-edit checklist (compact — always fits if we got here).
	if budgetLeft(b, budget) > 80 {
		b.WriteString("\n## Pre-Edit Checklist\n")
		callerCount := len(dc.Callers)
		if impact != nil {
			callerCount = impact.TotalAffected
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

	// Concerns from brain.
	if pkt != nil && (len(pkt.Concerns) > 0 || len(pkt.GraphWarnings) > 0) {
		b.WriteString("\n## Concerns\n")
		for _, c := range pkt.Concerns {
			fmt.Fprintf(b, "- %s\n", c)
		}
		for _, w := range pkt.GraphWarnings {
			fmt.Fprintf(b, "- %s\n", w)
		}
	}

	// Rule violations.
	rules := matchRulesForFile(s.config, node.File)
	if len(rules) > 0 {
		b.WriteString("\n## Violations\n")
		for _, r := range rules {
			fmt.Fprintf(b, "%s [%s]: %s\n", strings.ToUpper(r.Severity), r.RuleID, r.Description)
		}
	}

	// Blast radius (broad: depth 3).
	impact, ierr := s.graph.ImpactAnalysis(node.ID, 3)
	if ierr == nil && len(impact.Tiers) > 0 {
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
	}

	// Optional: Agent annotations (strip when budget is tight).
	if budgetLeft(b, budget) > 150 && s.store != nil {
		nodeIDs := []string{string(node.ID)}
		if annMap, annErr := s.store.GetAnnotationsForNodes(nodeIDs); annErr == nil {
			writeAnnotations(b, annMap, node.ID)
		}
	}
	_ = dc // used for pkt building above
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

	// Optional tier 2: Related state + annotations.
	if budgetLeft(b, budget) > 100 {
		b.WriteString("\n## Related State\n")
		if fileHasTests(node.File) {
			b.WriteString("Test file: exists\n")
		} else {
			b.WriteString("Test file: none\n")
		}
		if pkt != nil && len(pkt.GraphWarnings) > 0 {
			for _, w := range pkt.GraphWarnings {
				fmt.Fprintf(b, "⚠ %s\n", w)
			}
		}
	}

	// Annotations can hold past debugging notes.
	if budgetLeft(b, budget) > 150 && s.store != nil {
		nodeIDs := []string{string(node.ID)}
		if annMap, annErr := s.store.GetAnnotationsForNodes(nodeIDs); annErr == nil {
			writeAnnotations(b, annMap, node.ID)
		}
	}
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
	_ context.Context,
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

	// Impact analysis (depth 3) to find all affected files.
	impact, ierr := s.graph.ImpactAnalysis(node.ID, 3)

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
	if ierr == nil {
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

	// Architecture rules.
	rules := matchRulesForFile(s.config, node.File)
	if len(rules) > 0 {
		b.WriteString("\n## Architecture Rules\n")
		for _, r := range rules {
			fmt.Fprintf(b, "- %s: %s\n", r.RuleID, r.Description)
		}
	}

	// Scope assessment and risk.
	b.WriteString("\n## Scope Assessment\n")
	directCallers := 0
	if ierr == nil && len(impact.Tiers) > 0 {
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
}

// ---------------------------------------------------------------------------
// Shared helpers used by multiple assemblers
// ---------------------------------------------------------------------------

// buildBrainPacket assembles a ContextPacket from the brain client.
// Checks the 30s packet cache first; falls back to nil if brain unavailable.
// This is extracted here so both handleGetContext and intent assemblers share it.
func (s *Server) buildBrainPacket(
	ctx context.Context,
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

	calleeNames := make([]string, 0, len(dc.Callees))
	for _, c := range dc.Callees {
		calleeNames = append(calleeNames, c.Node.Name)
	}
	callerNames := make([]string, 0, len(dc.Callers))
	for _, c := range dc.Callers {
		callerNames = append(callerNames, c.Node.Name)
	}

	rules := matchRulesForFile(s.config, node.File)

	var claims []brain.ClaimInput
	if s.store != nil {
		if allClaims, err := s.store.GetAllClaims(); err == nil {
			for _, c := range allClaims {
				claims = append(claims, brain.ClaimInput{
					AgentID:   c.AgentID,
					Scope:     c.Scope,
					ScopeType: c.ScopeType,
					ExpiresAt: c.ExpiresAt.Format(time.RFC3339),
				})
			}
		}
	}

	pkt := bc.BuildContextPacket(ctx, brain.ContextPacketRequest{
		Snapshot: brain.SnapshotInput{
			RootNodeID:      string(node.ID),
			RootName:        node.Name,
			RootType:        string(node.Type),
			RootFile:        node.File,
			RootDoc:         node.Metadata["doc"],
			CalleeNames:     calleeNames,
			CallerNames:     callerNames,
			ApplicableRules: rules,
			ActiveClaims:    claims,
			TaskID:          taskID,
			HasTests:        fileHasTests(node.File),
			FanIn:           s.graph.Fanin(node.ID),
		},
		EnableLLM: s.config.Brain.EnableLLM,
	})
	if pkt != nil {
		s.setPacketCache(cacheKey, pkt)
	}
	return pkt
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

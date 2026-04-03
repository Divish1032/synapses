package mcp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/metrics"
)

// serializeCompact converts a directionalContext to a compact natural-language briefing.
// Token usage varies by detail level:
//   - "summary":   ~50 tokens  — root entity header + summary + warnings only
//   - "neighbors": ~200 tokens — summary + Calls/Called-by name lists (no callee blocks)
//   - "full":      ~400-600 tokens — full briefing with callee detail blocks (default)
//
// Format (full):
//
//	[entityName] type · file.go:line · complexity:N
//	Summary: "prose briefing from brain or AST doc"
//	Calls: callee1 · callee2 · callee3
//	Called by: caller1 · caller2
//	⚠ concern1 · graph_warning2
//	Insight: "architectural insight if available"
//
//	[callee_name] type · file.go:line
//	Summary: "callee summary if available in brain"
func serializeCompact(dc *directionalContext, detailLevel string) string {
	var b strings.Builder

	// Root entity header + summary.
	writeNodeHeader(&b, dc.Root, getRootSummary(dc.Root, dc.ContextPacket))

	// BUG-EVAL-9: disambiguation — list all candidate files so the agent has
	// exact file= values to copy into a follow-up get_context call.
	// dc.Root.File is repo-relative (normalizeSubgraph strips repoRoot prefix);
	// OtherCandidates["file"] is also repo-relative (stripped at build time).
	if len(dc.OtherCandidates) > 0 {
		fmt.Fprintf(&b, "⚠ %d entities named %q — re-call with file= to pin:\n", len(dc.OtherCandidates), dc.Root.Name)
		for _, c := range dc.OtherCandidates {
			file, _ := c["file"].(string)
			marker := ""
			if file == dc.Root.File {
				marker = " ← shown"
			}
			fmt.Fprintf(&b, "  • file=%q%s\n", file, marker)
		}
	}

	// === BEGINNING: Safety-critical content (high LLM attention) ===
	// "Lost in the Middle" (Liu et al., NeurIPS 2023): LLMs attend 30%+ better
	// to the beginning and end of context. Place violations and quality gaps
	// here so agents never miss them.

	// R19: Proactive rule alerts — actual violations found in the carved subgraph.
	if dc.Enrichment != nil && len(dc.Enrichment.RuleAlerts) > 0 {
		fmt.Fprintf(&b, "⚠ %d rule violation(s) in context:\n", len(dc.Enrichment.RuleAlerts))
		for _, ra := range dc.Enrichment.RuleAlerts {
			fromShort := shortName(ra.FromNode)
			toShort := shortName(ra.ToNode)
			fmt.Fprintf(&b, "  [%s] %s: %s → %s (%s)\n", ra.Severity, ra.RuleID, fromShort, toShort, ra.EdgeType)
			if ra.SuggestedFix != "" {
				fmt.Fprintf(&b, "    fix: %s\n", ra.SuggestedFix)
			}
		}
	}

	// Sprint 23.6: Security constraints — proactive NL briefings for add/modify intent.
	// Placed in the high-attention beginning zone so agents see constraints BEFORE writing.
	// Only populated by the enrichment goroutine when intent=add or intent=modify.
	if dc.Enrichment != nil && len(dc.Enrichment.SecurityConstraints) > 0 {
		fmt.Fprintf(&b, "🔒 Security constraints (%d):\n", len(dc.Enrichment.SecurityConstraints))
		for _, sc := range dc.Enrichment.SecurityConstraints {
			fmt.Fprintf(&b, "  %s\n", sc)
		}
	}

	// R32: Open quality gaps.
	if len(dc.QualityGaps) > 0 {
		fmt.Fprintf(&b, "⚠ %d open quality gap(s):\n", len(dc.QualityGaps))
		for _, g := range dc.QualityGaps {
			fmt.Fprintf(&b, "  [%s] %s — %s\n", g.Severity, g.GapID, g.Description)
		}
	}

	// Sprint 29.4: failure avoidance — entity-specific warnings from cross-session
	// failure patterns. Only present when this entity matches a known failure keyword.
	if len(dc.FailureWarnings) > 0 {
		fmt.Fprintf(&b, "⚠ failure history (%d):\n", len(dc.FailureWarnings))
		for _, w := range dc.FailureWarnings {
			fmt.Fprintf(&b, "  • %s\n", w)
		}
	}

	// Sprint 23.1: entity memories — institutional knowledge attached to this entity.
	// Rendered near the beginning (high-attention zone) so agents never miss prior
	// session findings. "summary" level skips these to respect the ~50-token budget.
	if detailLevel != "summary" {
		for _, m := range dc.EntityMemories {
			content := m.Content
			if content == "" {
				continue // skip empty-content memories (avoids blank 💡 lines)
			}
			if len(content) > 200 {
				content = content[:200] + "…"
			}
			fmt.Fprintf(&b, "💡 %s\n", content)
		}
	}

	// === MIDDLE: Supplementary content (lower LLM attention zone) ===

	// R3: Git blame line — who last touched this function and how stale it is.
	if dc.Root.Metadata != nil {
		if author := dc.Root.Metadata["blame_author"]; author != "" {
			age := metrics.BlameAgeLabel(dc.Root.Metadata["blame_date"])
			subject := dc.Root.Metadata["blame_subject"]
			staleness := "low"
			if s, err := strconv.ParseFloat(dc.Root.Metadata["staleness_score"], 64); err == nil {
				staleness = metrics.StalenessLabel(s)
			}
			if age != "" && subject != "" {
				fmt.Fprintf(&b, "⚑ @%s, %s: %q — staleness: %s\n", author, age, subject, staleness)
			} else if age != "" {
				fmt.Fprintf(&b, "⚑ @%s, %s — staleness: %s\n", author, age, staleness)
			}
		}
	}

	// R34: Commit context — the "why" layer: last 3 commit subjects + optional body.
	if cc := dc.Root.Metadata["commit_context"]; cc != "" {
		var commits []metrics.CommitInfo
		if json.Unmarshal([]byte(cc), &commits) == nil && len(commits) > 0 {
			subjects := make([]string, 0, len(commits))
			for _, c := range commits {
				if c.Message != "" {
					subjects = append(subjects, fmt.Sprintf("%q", c.Message))
				}
			}
			if len(subjects) > 0 {
				fmt.Fprintf(&b, "📝 last changes: %s\n", strings.Join(subjects, " · "))
			}
			// Surface body of the most recent commit when it adds context beyond the subject.
			if commits[0].Body != "" {
				fmt.Fprintf(&b, "  └ %q\n", commits[0].Body)
			}
		}
	}

	// Annotations: show agent/system notes for this entity (multi-agent visibility).
	if anns, ok := dc.Annotations[string(dc.Root.ID)]; ok && len(anns) > 0 {
		for _, a := range anns {
			fmt.Fprintf(&b, "\U0001f4dd %s\n", a.Note)
		}
	}

	// R31: Documentation sections linked to this code entity.
	// Each entry shows: title (file) — body_preview so agents can read the content.
	for _, d := range dc.Documentation {
		title := d.Node.Metadata["title"]
		if title == "" {
			title = d.Node.Name
		}
		preview := d.Node.Metadata["body_preview"]
		if preview == "" {
			preview = d.Node.Metadata["body"]
		}
		if len(preview) > 120 {
			preview = preview[:120] + "…"
		}
		if preview != "" {
			fmt.Fprintf(&b, "📖 %q (%s): %s\n", title, filepath.Base(d.Node.File), preview)
		} else {
			fmt.Fprintf(&b, "📖 %q (%s)\n", title, filepath.Base(d.Node.File))
		}
	}

	// Brain hint: tell agents whether brain enrichment is in progress or unavailable.
	if dc.BrainHint != "" && dc.ContextPacket == nil {
		fmt.Fprintf(&b, "brain: %s\n", dc.BrainHint)
	}

	// "summary" level: just the root header + warnings. Stop here.
	if detailLevel == "summary" {
		var warnings []string
		if dc.ContextPacket != nil {
			warnings = append(warnings, dc.ContextPacket.GraphWarnings...)
			warnings = append(warnings, dc.ContextPacket.Concerns...)
		}
		if len(warnings) > 0 {
			fmt.Fprintf(&b, "⚠ %s\n", strings.Join(warnings, " · "))
		}
		// Sprint 15 #6: confidence must appear at every detail level — agents
		// using "summary" need the advisory as much as those using "full".
		// Use ConfidenceHint as the sentinel for "was computed" so that a
		// legitimately-zero confidence (extreme qs + both staleness flags)
		// is not silently suppressed by the Confidence > 0 guard.
		if dc.Confidence > 0 || dc.ConfidenceHint != "" {
			if dc.ConfidenceHint != "" {
				fmt.Fprintf(&b, "⚠ confidence:%.2f — %s\n", dc.Confidence, dc.ConfidenceHint)
			} else {
				fmt.Fprintf(&b, "confidence:%.2f\n", dc.Confidence)
			}
		}
		if dc.EntityHash != "" {
			fmt.Fprintf(&b, "\nentity_hash:%s\n", dc.EntityHash)
		}
		return strings.TrimSpace(b.String())
	}

	// "neighbors" and "full": sandwich ordering continues.

	// === BEGINNING (cont.): Warnings and confidence alerts ===

	// Warnings: combine brain concerns + graph warnings.
	var warnings []string
	if dc.ContextPacket != nil {
		warnings = append(warnings, dc.ContextPacket.GraphWarnings...)
		warnings = append(warnings, dc.ContextPacket.Concerns...)
	}
	if len(warnings) > 0 {
		fmt.Fprintf(&b, "⚠ %s\n", strings.Join(warnings, " · "))
	}

	// DIAG-3: caller-count confidence warning.
	if dc.CallerCountWarning != "" {
		fmt.Fprintf(&b, "%s\n", dc.CallerCountWarning)
	}

	// Sprint 15 #6: context confidence score. Present when computed (non-zero
	// or when a hint is set — hint fires at confidence < 0.5, including the
	// edge case of confidence == 0.0 from extreme qs + both staleness flags).
	if dc.Confidence > 0 || dc.ConfidenceHint != "" {
		if dc.ConfidenceHint != "" {
			fmt.Fprintf(&b, "⚠ confidence:%.2f — %s\n", dc.Confidence, dc.ConfidenceHint)
		} else {
			fmt.Fprintf(&b, "confidence:%.2f\n", dc.Confidence)
		}
	}

	// === MIDDLE: Supplementary content (lowest LLM attention zone) ===

	// IMP-IMPL-2: For struct nodes, list field names and types from metadata.
	// Fields are stored as "Name type,Name type,..." by the Go parser.
	if dc.Root.Type == graph.NodeStruct {
		if fieldMeta, ok := dc.Root.Metadata["fields"]; ok && fieldMeta != "" {
			parts := strings.Split(fieldMeta, ",")
			b.WriteString("Fields:")
			total := 0
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				fmt.Fprintf(&b, "\n  %s", p)
				total++
			}
			if total == 15 && len(parts) > 15 {
				fmt.Fprintf(&b, "\n  ... and more")
			}
			b.WriteString("\n")
		}
	}

	// Hot Constitution: append project principles as a Laws line.
	if len(dc.Principles) > 0 {
		laws := strings.Join(dc.Principles, " · ")
		if len(laws) > 120 {
			laws = laws[:117] + "…"
		}
		fmt.Fprintf(&b, "📋 Laws: %s\n", laws)
	}

	// ADRs: show up to 2 accepted ADRs relevant to this entity's file.
	for _, adr := range dc.ADRs {
		fmt.Fprintf(&b, "[ADR] %s (%s)\n", adr.Title, adr.Status)
	}

	// Active prompts: show the first line of each body as a compact convention hint.
	// Full bodies are also included in the JSON representation (dc.ActivePrompts).
	for _, ap := range dc.ActivePrompts {
		hint := ap.Body
		if nl := strings.IndexByte(hint, '\n'); nl > 0 {
			hint = hint[:nl]
		}
		hint = strings.TrimSpace(hint)
		if len(hint) > 120 {
			hint = hint[:117] + "…"
		}
		fmt.Fprintf(&b, "📚 [%s] %s\n", ap.ID, hint)
	}

	// === END: Actionable items (second-highest LLM attention) ===

	// Calls: list callee names with inline purpose fragments.
	// Sprint 23.1: "relationship descriptions" — include a brief purpose clause for
	// each callee when nl_description or doc is available (e.g. from enrichNodesWithNL).
	// Format: "Calls: validateInput (validate input) · checkPassword (check password)"
	// This is the deterministic, zero-LLM version of "A calls B which calls C".
	if len(dc.Callees) > 0 {
		parts := make([]string, 0, len(dc.Callees))
		for _, c := range dc.Callees {
			// Priority: brain summary first clause > nl_description first clause > doc first clause > name only.
			// Mirrors the callee detail block priority so the Calls: line and block are consistent.
			var purpose string
			if bs := getDepSummary(c.Node.Name, dc.ContextPacket); bs != "" {
				purpose = firstClause(bs, 40)
			} else {
				purpose = calleeShortPurpose(c.Node)
			}
			if purpose != "" {
				parts = append(parts, c.Node.Name+" ("+purpose+")")
			} else {
				parts = append(parts, c.Node.Name)
			}
		}
		fmt.Fprintf(&b, "Calls: %s\n", strings.Join(parts, " · "))
	}

	// Called by: list caller names.
	if len(dc.Callers) > 0 {
		names := make([]string, 0, len(dc.Callers))
		for _, c := range dc.Callers {
			names = append(names, c.Node.Name)
		}
		fmt.Fprintf(&b, "Called by: %s\n", strings.Join(names, " · "))
	} else if len(dc.Callees) == 0 {
		// Neither callers nor callees — standalone entity.
		b.WriteString("Called by: (none)\n")
	}

	// "neighbors" level: stop after actionable call lists.
	if detailLevel == "neighbors" {
		if dc.EntityHash != "" {
			fmt.Fprintf(&b, "\nentity_hash:%s\n", dc.EntityHash)
		}
		return strings.TrimSpace(b.String())
	}

	// "full" level (default): add remaining middle + actionable callee detail blocks.

	// Architectural insight from brain (LLM-generated, only when brain available).
	if dc.ContextPacket != nil && dc.ContextPacket.Insight != "" {
		fmt.Fprintf(&b, "Insight: %s\n", dc.ContextPacket.Insight)
	}

	// Sprint 16 #4: Cross-domain neighbors — infra, API, config, knowledge nodes.
	// Rendered before same-domain related nodes so agents immediately see cross-domain
	// context (e.g. Terraform resources, OpenAPI endpoints) without scrolling past code.
	if dc.CrossDomain != nil && !dc.CrossDomain.IsEmpty() {
		renderCDBucket := func(label string, nodes []graph.CarvedNode) {
			if len(nodes) == 0 {
				return
			}
			names := make([]string, 0, len(nodes))
			for _, n := range nodes {
				names = append(names, n.Node.Name)
			}
			fmt.Fprintf(&b, "%s: %s\n", label, strings.Join(names, " · "))
		}
		renderCDBucket("Deploys", dc.CrossDomain.Deploys)
		renderCDBucket("Consumes", dc.CrossDomain.Consumes)
		renderCDBucket("Configured by", dc.CrossDomain.ConfiguredBy)
		renderCDBucket("Documented in", dc.CrossDomain.DocumentedIn)
		renderCDBucket("Mentioned in", dc.CrossDomain.Mentions)
		renderCDBucket("Manual links", dc.CrossDomain.Manual)
		renderCDBucket("Cross-domain related", dc.CrossDomain.Related)
	}

	// Show related nodes with brain summaries or nl_description fallback.
	// Sprint 23.1: use nl_description (computed by enrichNodesWithNL) when brain
	// is unavailable so agents still get a meaningful description of related entities.
	for _, r := range dc.Related {
		depSummary := getDepSummary(r.Node.Name, dc.ContextPacket)
		if depSummary == "" {
			depSummary = r.Node.Metadata["nl_description"]
		}
		if depSummary == "" {
			continue // only show related nodes if we have a summary for them
		}
		b.WriteString("\n")
		writeNodeHeader(&b, r.Node, depSummary)
	}

	// Callee detail blocks: always show all callees in "full" mode so the output
	// is visibly richer than "neighbors" even when the brain cache is cold.
	// Sprint 23.1: use nl_description as fallback when brain DependencySummaries
	// is empty — enrichNodesWithNL pre-populates this for all code nodes.
	if len(dc.Callees) > 0 {
		for _, c := range dc.Callees {
			depSummary := getDepSummary(c.Node.Name, dc.ContextPacket)
			if depSummary == "" {
				depSummary = c.Node.Metadata["nl_description"]
			}
			b.WriteString("\n")
			writeNodeHeader(&b, c.Node, depSummary)
		}
	}

	if dc.Truncated {
		fmt.Fprintf(&b, "\n[%d additional nodes omitted by token budget]\n", dc.TruncatedCount)
	}

	// F17: surface adaptive expansion hint so agents know why they received
	// deeper context than the configured default.
	if dc.AdaptiveHint != "" {
		fmt.Fprintf(&b, "\n%s\n", dc.AdaptiveHint)
	}

	// R14: append entity_hash so agents using compact format can still do
	// hash-based caching by passing known_hash on the next get_context call.
	if dc.EntityHash != "" {
		fmt.Fprintf(&b, "\nentity_hash:%s\n", dc.EntityHash)
	}

	return strings.TrimSpace(b.String())
}

// writeNodeHeader writes a compact entity header line + optional summary.
// Format: [name] type · basename.go:line [ · complexity:N] [ · ⚠ provenance]
//
//	Summary: "..."
//
// R1: route nodes (NodeRoute / inferred=true) are prefixed with ⚡ inferred:
// to make synthesised framework routing edges visually distinct from AST edges.
func writeNodeHeader(b *strings.Builder, n *graph.Node, summary string) {
	var extras []string
	if c := n.Metadata["complexity"]; c != "" && c != "0" && c != "1" {
		extras = append(extras, "complexity:"+c)
	}
	// R28: surface non-user-authored provenance so agents know when they are
	// looking at generated or vendored code (lower trust for architectural decisions).
	switch n.Provenance {
	case graph.ProvenanceGenerated:
		extras = append(extras, "⚠ generated")
	case graph.ProvenanceVendored:
		extras = append(extras, "⚠ vendored")
	case graph.ProvenanceExternal:
		extras = append(extras, "⚠ external")
	}

	// OF-H1: surface domain for non-code nodes so agents know they are looking at
	// infrastructure/API/doc/issue context rather than source code.
	// Speed: two string comparisons per node — O(1), negligible vs. BFS traversal cost.
	if n.Domain != "" && n.Domain != graph.DomainCode {
		extras = append(extras, "domain:"+string(n.Domain))
	}

	// R1: surface confidence for inferred route nodes.
	if n.Type == graph.NodeRoute {
		if conf := n.Metadata["confidence"]; conf != "" {
			extras = append(extras, "conf:"+conf)
		}
	}

	file := filepath.Base(n.File)
	header := fmt.Sprintf("[%s] %s · %s", n.Name, n.Type, file)
	if n.Line > 0 {
		header = fmt.Sprintf("[%s] %s · %s:%d", n.Name, n.Type, file, n.Line)
	}
	if len(extras) > 0 {
		header += " · " + strings.Join(extras, " · ")
	}

	// R1: prefix synthesised route nodes with ⚡ to distinguish from AST nodes.
	if n.Type == graph.NodeRoute || (n.Metadata != nil && n.Metadata["inferred"] == "true") {
		b.WriteString("⚡ inferred: " + header + "\n")
	} else {
		b.WriteString(header + "\n")
	}

	// Sprint 23.1: entity signatures — the ONE piece of "code" Synapses provides.
	// Shown for function/method/struct/interface/class nodes only; skipped for
	// route, file, and package nodes where a signature is meaningless.
	// Caps at 120 chars to prevent enormous generic signatures from flooding output.
	if n.Metadata != nil {
		if sig := n.Metadata["signature"]; sig != "" {
			switch n.Type {
			case graph.NodeFunction, graph.NodeMethod, graph.NodeStruct,
				graph.NodeInterface:
				if len(sig) > 120 {
					sig = sig[:120] + "…"
				}
				fmt.Fprintf(b, "Signature: %s\n", sig)
			}
		}
	}

	if summary != "" {
		fmt.Fprintf(b, "Summary: %s\n", summary)
	}
}

// filterInferredNodes removes synthetic route / inferred nodes from a carved-node slice.
// Used by handleGetContext when include_inferred=false so agents receive only AST-proven edges.
func filterInferredNodes(nodes []graph.CarvedNode) []graph.CarvedNode {
	out := nodes[:0] // reuse backing array; safe because we only shrink
	for _, cn := range nodes {
		if cn.Node.Type == graph.NodeRoute {
			continue
		}
		if cn.Node.Metadata != nil && cn.Node.Metadata["inferred"] == "true" {
			continue
		}
		out = append(out, cn)
	}
	return out
}

// getRootSummary returns the best available prose summary for the root node.
// Priority: brain RootSummary > AST doc metadata > "".
// shortName extracts a readable name from a full node ID (e.g. "repo::file.go::Func" → "Func").
func shortName(nodeID string) string {
	if idx := strings.LastIndex(nodeID, "::"); idx >= 0 {
		return nodeID[idx+2:]
	}
	return nodeID
}

func getRootSummary(n *graph.Node, pkt *brain.ContextPacket) string {
	if pkt != nil && pkt.RootSummary != "" {
		return pkt.RootSummary
	}
	if n.Metadata != nil {
		if doc := n.Metadata["doc"]; doc != "" {
			if len(doc) > 250 {
				doc = doc[:250] + "…"
			}
			return doc
		}
	}
	return ""
}

// calleeShortPurpose extracts a brief purpose fragment for inline use in Calls: lines.
// Tries nl_description first clause, then doc first sentence. Returns "" when neither
// is available so the caller can fall back to name-only.
// Example: nl_description "validate input, given req. Returns error" → "validate input"
func calleeShortPurpose(n *graph.Node) string {
	if n.Metadata == nil {
		return ""
	}
	if nl := n.Metadata["nl_description"]; nl != "" {
		return firstClause(nl, 40)
	}
	if doc := n.Metadata["doc"]; doc != "" {
		return firstClause(doc, 40)
	}
	return ""
}

// firstClause returns the first clause of s — truncates at '.', ',', or maxLen chars.
// Used to extract brief purpose fragments from nl_description and doc strings.
func firstClause(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	for i, r := range s {
		if i >= maxLen {
			return s[:maxLen]
		}
		if r == '.' || r == ',' {
			return strings.TrimSpace(s[:i])
		}
	}
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}

// getDepSummary returns the brain dependency summary for a named entity, or "".
// DependencySummaries is keyed by entity name (not node ID).
func getDepSummary(name string, pkt *brain.ContextPacket) string {
	if pkt == nil || len(pkt.DependencySummaries) == 0 {
		return ""
	}
	return pkt.DependencySummaries[name]
}

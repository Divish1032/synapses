package mcp

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/graph"
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

	// R32: Open quality gaps — surface before annotations so agents see known issues first.
	if len(dc.QualityGaps) > 0 {
		fmt.Fprintf(&b, "⚠ %d open quality gap(s):\n", len(dc.QualityGaps))
		for _, g := range dc.QualityGaps {
			fmt.Fprintf(&b, "  [%s] %s — %s\n", g.Severity, g.GapID, g.Description)
		}
	}

	// Annotations: show agent/system notes for this entity (multi-agent visibility).
	if anns, ok := dc.Annotations[string(dc.Root.ID)]; ok && len(anns) > 0 {
		for _, a := range anns {
			fmt.Fprintf(&b, "\U0001f4dd %s\n", a.Note)
		}
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
		if dc.EntityHash != "" {
			fmt.Fprintf(&b, "\nentity_hash:%s\n", dc.EntityHash)
		}
		return strings.TrimSpace(b.String())
	}

	// "neighbors" and "full": add Calls / Called-by name lists.

	// Calls: list callee names.
	if len(dc.Callees) > 0 {
		names := make([]string, 0, len(dc.Callees))
		for _, c := range dc.Callees {
			names = append(names, c.Node.Name)
		}
		fmt.Fprintf(&b, "Calls: %s\n", strings.Join(names, " · "))
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

	// Warnings: combine brain concerns + graph warnings.
	var warnings []string
	if dc.ContextPacket != nil {
		warnings = append(warnings, dc.ContextPacket.GraphWarnings...)
		warnings = append(warnings, dc.ContextPacket.Concerns...)
	}
	if len(warnings) > 0 {
		fmt.Fprintf(&b, "⚠ %s\n", strings.Join(warnings, " · "))
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

	// "neighbors" level: stop after caller/callee names. No callee blocks.
	if detailLevel == "neighbors" {
		if dc.EntityHash != "" {
			fmt.Fprintf(&b, "\nentity_hash:%s\n", dc.EntityHash)
		}
		return strings.TrimSpace(b.String())
	}

	// "full" level (default): add insight + callee detail blocks.

	// Architectural insight from brain (LLM-generated, only when brain available).
	if dc.ContextPacket != nil && dc.ContextPacket.Insight != "" {
		fmt.Fprintf(&b, "Insight: %s\n", dc.ContextPacket.Insight)
	}

	// Callee detail blocks: always show all callees in "full" mode so the output
	// is visibly richer than "neighbors" even when the brain cache is cold.
	// Without summaries, only the entity header line is written (no Summary line).
	if len(dc.Callees) > 0 {
		for _, c := range dc.Callees {
			depSummary := getDepSummary(c.Node.Name, dc.ContextPacket)
			b.WriteString("\n")
			writeNodeHeader(&b, c.Node, depSummary)
		}
	}

	// Show related nodes with brain summaries (often interface implementations, types).
	for _, r := range dc.Related {
		depSummary := getDepSummary(r.Node.Name, dc.ContextPacket)
		if depSummary == "" {
			continue // only show related nodes if we have a summary for them
		}
		b.WriteString("\n")
		writeNodeHeader(&b, r.Node, depSummary)
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

	if summary != "" {
		fmt.Fprintf(b, "Summary: %s\n", summary)
	}
}

// getRootSummary returns the best available prose summary for the root node.
// Priority: brain RootSummary > AST doc metadata > "".
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

// getDepSummary returns the brain dependency summary for a named entity, or "".
// DependencySummaries is keyed by entity name (not node ID).
func getDepSummary(name string, pkt *brain.ContextPacket) string {
	if pkt == nil || len(pkt.DependencySummaries) == 0 {
		return ""
	}
	return pkt.DependencySummaries[name]
}

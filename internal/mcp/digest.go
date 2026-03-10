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

	// "neighbors" level: stop after caller/callee names. No callee blocks.
	if detailLevel == "neighbors" {
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

	return strings.TrimSpace(b.String())
}

// writeNodeHeader writes a compact entity header line + optional summary.
// Format: [name] type · basename.go:line [ · complexity:N]
//
//	Summary: "..."
func writeNodeHeader(b *strings.Builder, n *graph.Node, summary string) {
	var extras []string
	if c := n.Metadata["complexity"]; c != "" && c != "0" && c != "1" {
		extras = append(extras, "complexity:"+c)
	}

	file := filepath.Base(n.File)
	header := fmt.Sprintf("[%s] %s · %s", n.Name, n.Type, file)
	if n.Line > 0 {
		header = fmt.Sprintf("[%s] %s · %s:%d", n.Name, n.Type, file, n.Line)
	}
	if len(extras) > 0 {
		header += " · " + strings.Join(extras, " · ")
	}
	b.WriteString(header + "\n")

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

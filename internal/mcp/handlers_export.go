package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// handleExportKnowledge serializes all durable agent-generated knowledge for
// the current project to a portable JSON snapshot. Intended for backup and
// migration. The export is project-scoped — only the current project's
// memories, episodes, rules, annotations, and quality gaps are included.
//
// Graph nodes/edges are deliberately excluded (regenerable from source code).
// Transient tables (tool_calls, web_cache, sessions, agent_messages) are also
// excluded — they contain operational logs, not persistent knowledge.
func (s *Server) handleExportKnowledge(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError(
			"knowledge store unavailable: run 'synapses start' or 'synapses index' first",
		), nil
	}

	format := stringArg(req, "format")
	if format == "" {
		format = "json"
	}
	if format != "json" {
		return mcp.NewToolResultError(
			fmt.Sprintf("unsupported format %q — only \"json\" is supported", format),
		), nil
	}

	export, err := s.store.ExportKnowledge(s.projectID)
	if err != nil {
		return toolError("export_knowledge", err)
	}

	b, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return toolError("marshal export", err)
	}

	// Large exports are valid (10K memories × ~500 bytes = ~5 MB). We bypass
	// the jsonResult() 2 MiB check and return raw JSON text — the agent can
	// write it to a file. Warn when the payload is large.
	const warnThreshold = 2 * 1024 * 1024 // 2 MiB
	suffix := ""
	if len(b) > warnThreshold {
		suffix = fmt.Sprintf(
			"\n\n# Note: export is %d bytes. Write to a file before processing.",
			len(b),
		)
	}

	// Return summary metadata as a preamble so the agent can see counts without
	// parsing the full JSON payload. The full JSON follows on the next line.
	preamble := fmt.Sprintf(
		"export_knowledge complete\n"+
			"memories: %d  versions: %d  anchors: %d  embeddings: %d\n"+
			"episodes: %d  rules: %d  annotations: %d  gaps: %d\n\n",
		export.Summary.MemoryCount,
		export.Summary.MemoryVersionCount,
		export.Summary.MemoryAnchorCount,
		export.Summary.MemoryEmbeddingCount,
		export.Summary.EpisodeCount,
		export.Summary.DynamicRuleCount,
		export.Summary.AnnotationCount,
		export.Summary.QualityGapCount,
	)

	return mcp.NewToolResultText(preamble + string(b) + suffix), nil
}

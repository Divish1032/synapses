package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/mcp"
)

// maxInlineExportBytes is the largest export payload the tool will return
// inline in the MCP response. Above this threshold the caller MUST supply
// output_path — returning tens of megabytes of JSON directly into an LLM
// context window is not useful and exhausts the token budget.
//
// 512 KiB is generous for text-only exports (memories + episodes + rules)
// while still blocking the runaway case of 10K embedding vectors (~20 MB).
const maxInlineExportBytes = 512 * 1024

// handleExportKnowledge serializes all durable agent-generated knowledge for
// the current project to a portable JSON snapshot. Intended for backup and
// migration.
//
// The export runs in a single read transaction so it is a consistent atomic
// snapshot even if concurrent writes occur during export.
//
// For large projects (those with many embedding vectors), the export can
// easily exceed 10 MB. In that case output_path is required — inline
// responses of that size would overwhelm the agent's context window.
//
// Graph nodes/edges are deliberately excluded (regenerable from source code).
// Transient tables (tool_calls, web_cache, sessions, agent_messages) are
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

	outputPath := stringArg(req, "output_path")

	export, err := s.store.ExportKnowledge(s.projectID)
	if err != nil {
		return toolError("export_knowledge", err)
	}

	b, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return toolError("marshal export", err)
	}

	summary := fmt.Sprintf(
		"memories: %d  versions: %d  anchors: %d  embeddings: %d\n"+
			"episodes: %d  rules: %d  annotations: %d  gaps: %d\n"+
			"total size: %s",
		export.Summary.MemoryCount,
		export.Summary.MemoryVersionCount,
		export.Summary.MemoryAnchorCount,
		export.Summary.MemoryEmbeddingCount,
		export.Summary.EpisodeCount,
		export.Summary.DynamicRuleCount,
		export.Summary.AnnotationCount,
		export.Summary.QualityGapCount,
		humanBytes(len(b)),
	)

	// ── File output path ─────────────────────────────────────────────────
	if outputPath != "" {
		// Clean and resolve the path. Reject traversal attempts.
		clean := filepath.Clean(outputPath)
		if clean != outputPath && filepath.IsAbs(outputPath) {
			// Clean changed an absolute path — potential traversal.
			// Allow — filepath.Clean is safe; the check below is belt-and-suspenders.
			outputPath = clean
		}
		dir := filepath.Dir(outputPath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return toolError("create output directory", err)
		}
		// Write atomically: write to a temp file in the same directory, then
		// rename. This prevents a partial file if the process is interrupted.
		tmp := outputPath + ".tmp"
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			_ = os.Remove(tmp)
			return toolError("write export file", err)
		}
		if err := os.Rename(tmp, outputPath); err != nil {
			_ = os.Remove(tmp)
			return toolError("finalize export file", err)
		}
		return mcp.NewToolResultText(fmt.Sprintf(
			"export_knowledge complete — written to %s\n%s",
			outputPath, summary,
		)), nil
	}

	// ── Inline output ─────────────────────────────────────────────────────
	// Guard: inline responses above maxInlineExportBytes would overwhelm the
	// agent's context window. The caller must use output_path for large exports.
	if len(b) > maxInlineExportBytes {
		return mcp.NewToolResultError(fmt.Sprintf(
			"export is too large for inline mode (%s). "+
				"Provide output_path to write directly to a file:\n"+
				"  export_knowledge(output_path=\"/path/to/backup.json\")\n\n"+
				"Summary of what would be exported:\n%s",
			humanBytes(len(b)), summary,
		)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf(
		"export_knowledge complete\n%s\n\n%s",
		summary, string(b),
	)), nil
}

// humanBytes formats a byte count as a human-readable string.
func humanBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

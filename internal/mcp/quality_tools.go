package mcp

import (
	"context"
	"fmt"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/store"
)

// handleUpsertGap creates or updates a quality gap on a code entity.
// Quality gaps are agent-discovered findings — things that require reasoning
// to find, unlike architecture violations (which are deterministic rule checks).
// They persist across sessions so future agents never re-discover the same issue.
func (s *Server) handleUpsertGap(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("store not available (run synapses start, not synapses index)"), nil
	}

	nodeID := stringArg(req, "node_id")
	gapID := stringArg(req, "gap_id")
	description := stringArg(req, "description")

	if nodeID == "" {
		return mcp.NewToolResultError("node_id is required"), nil
	}
	if gapID == "" {
		return mcp.NewToolResultError("gap_id is required (use a short slug, e.g. \"dist-relative-path\")"), nil
	}
	if description == "" {
		return mcp.NewToolResultError("description is required"), nil
	}

	severity := stringArg(req, "severity")
	if severity == "" {
		severity = "medium"
	}
	switch severity {
	case "low", "medium", "high", "critical":
	default:
		return mcp.NewToolResultError("severity must be one of: low, medium, high, critical"), nil
	}

	status := stringArg(req, "status")
	if status == "" {
		status = "open"
	}
	switch status {
	case "open", "fixed", "wontfix":
	default:
		return mcp.NewToolResultError("status must be one of: open, fixed, wontfix"), nil
	}

	// Resolve node_id to canonical graph ID (format "{repoID}::{file}::{name}").
	// Agents commonly pass a bare function name; FindByName maps it to the full
	// canonical ID so the gap surfaces correctly in get_context() queries.
	resolvedNodeID := nodeID
	nodeIDWarning := ""
	if s.graph != nil && !strings.Contains(nodeID, "::") {
		// Bare name: try to resolve to a canonical node ID.
		// Use pickBestNode (same deterministic scoring as handleGetContext) so
		// the same node is chosen every time, even when multiple packages define
		// a function with the same name.
		if nodes := s.graph.FindByName(nodeID); len(nodes) > 0 {
			resolvedNodeID = string(pickBestNode(nodes, s.graph).ID)
		} else {
			nodeIDWarning = " Warning: node_id not found in graph — gap stored but will not surface in get_context()."
		}
	}

	gap := store.QualityGap{
		NodeID:      resolvedNodeID,
		GapID:       gapID,
		Description: description,
		Severity:    severity,
		Status:      status,
		FoundBy:     stringArg(req, "agent_id"),
		FixNotes:    stringArg(req, "fix_notes"),
	}

	saved, err := s.store.UpsertGap(gap)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("upsert gap failed: %v", err)), nil
	}

	statusVerb := "recorded"
	if status == "fixed" {
		statusVerb = "marked fixed"
	} else if status == "wontfix" {
		statusVerb = "marked wontfix"
	}

	return jsonResult(map[string]interface{}{
		"id":          saved.ID,
		"node_id":     saved.NodeID,
		"gap_id":      saved.GapID,
		"description": saved.Description,
		"severity":    saved.Severity,
		"status":      saved.Status,
		"found_at":    saved.FoundAt,
		"updated_at":  saved.UpdatedAt,
		"status_msg":  fmt.Sprintf("Quality gap %q on node %q %s. Visible in get_violations() and get_context().%s", gapID, resolvedNodeID, statusVerb, nodeIDWarning),
	})
}

// handleGetGaps queries quality gaps with optional filters.
// Default returns only open gaps. Pass status="all" to see everything.
func (s *Server) handleGetGaps(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("store not available (run synapses start, not synapses index)"), nil
	}

	f := store.GapFilter{
		NodeID:   stringArg(req, "node_id"),
		File:     stringArg(req, "file"),
		Severity: stringArg(req, "severity"),
		Status:   stringArg(req, "status"),
	}

	gaps, err := s.store.GetGaps(f)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get gaps failed: %v", err)), nil
	}

	displayStatus := f.Status
	if displayStatus == "" {
		displayStatus = "open"
	}

	return jsonResult(map[string]interface{}{
		"gaps":    gaps,
		"count":   len(gaps),
		"filter":  displayStatus,
		"hint":    "Use upsert_gap(status=\"fixed\", fix_notes=\"...\") to close a gap after fixing it.",
	})
}

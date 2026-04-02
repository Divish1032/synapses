package mcp

// handlers_rejected_approaches.go — Sprint 24.6: Rejected approach memory.
//
// Rejected approaches are immutable records of explicitly abandoned
// implementation paths: what was tried, why it failed, what specific error
// or blocker was hit. Unlike failure episodes (unstructured append-only),
// these have structured fields purpose-built for compaction recovery and
// session_init warnings.
//
// Future sessions see: "A previous session tried this approach and abandoned
// it because [failure_reason]." — injected without agent action (Tier 1/2).
//
// Agent records via:  memory(action="abandon")
// Agent retrieves via: memory(action="list_rejected")

import (
	"context"
	"fmt"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/store"
)

// handleAbandon records a new rejected approach. approach and failure_reason
// are required; blocker and context are optional enrichment fields.
//
// Params: agent_id (required), approach (required), failure_reason (required),
// blocker (optional), context (optional).
func (s *Server) handleAbandon(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("rejected approach memory unavailable: run 'synapses start' to create a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}

	approach, approachErr := stringArgLimited(req, "approach", maxArgLengthDecision)
	if approachErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(approachErr.Error())), nil
	}
	if approach == "" {
		return mcp.NewToolResultError("approach is required — describe what was tried (e.g. 'Implement caching with Redis for session storage')"), nil
	}

	failureReason, failureErr := stringArgLimited(req, "failure_reason", maxArgLengthRationale)
	if failureErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(failureErr.Error())), nil
	}
	if failureReason == "" {
		return mcp.NewToolResultError("failure_reason is required — describe why this approach was abandoned (e.g. 'Redis not available in the deployment environment')"), nil
	}

	blocker, blockerErr := stringArgLimited(req, "blocker", maxArgLengthRationale)
	if blockerErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(blockerErr.Error())), nil
	}

	rejContext, ctxErr := stringArgLimited(req, "context", maxArgLengthDecision)
	if ctxErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(ctxErr.Error())), nil
	}

	// Scan all text fields for prompt injection.
	for fieldName, fieldVal := range map[string]*string{
		"approach":       &approach,
		"failure_reason": &failureReason,
		"blocker":        &blocker,
		"context":        &rejContext,
	} {
		if *fieldVal == "" {
			continue
		}
		scanResult, scanErr := s.scanContent(fieldName, *fieldVal)
		if scanErr != nil {
			return mcp.NewToolResultError(stripInternalPaths(scanErr.Error())), nil
		}
		*fieldVal = scanResult.sanitized
	}

	r := store.RejectedApproach{
		AgentID:       agentID,
		ProjectID:     s.projectID,
		Approach:      approach,
		FailureReason: failureReason,
		Blocker:       blocker,
		Context:       rejContext,
	}

	id, err := s.store.InsertRejectedApproach(r)
	if err != nil {
		return toolError("record rejected approach", err)
	}

	return jsonResult(map[string]interface{}{
		"rejected_approach_id": id,
		"approach":             approach,
		"message": fmt.Sprintf(
			"Rejected approach recorded (id=%s). Future sessions will be warned: %q was abandoned because %q.",
			id, summariseDecision(approach), summariseDecision(failureReason),
		),
	})
}

// handleListRejectedApproaches returns rejected approaches for the current
// project/agent. Supports optional keyword search across approach, failure_reason,
// blocker, and context fields.
//
// Params: agent_id (required), query (optional, keyword search), limit (optional, default 20).
func (s *Server) handleListRejectedApproaches(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("rejected approach memory unavailable: run 'synapses start' to create a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}

	query := stringArg(req, "query")
	limit := intArgDefault(req, "limit", 20)
	if limit < 1 || limit > 100 {
		limit = 20
	}

	approaches, err := s.store.SearchRejectedApproaches(agentID, s.projectID, query, limit)
	if err != nil {
		return toolError("list rejected approaches", err)
	}

	type approachItem struct {
		ID            string `json:"id"`
		Approach      string `json:"approach"`
		FailureReason string `json:"failure_reason"`
		Blocker       string `json:"blocker,omitempty"`
		Context       string `json:"context,omitempty"`
		CreatedAt     int64  `json:"created_at"`
	}
	items := make([]approachItem, 0, len(approaches))
	for _, r := range approaches {
		items = append(items, approachItem{
			ID:            r.ID,
			Approach:      r.Approach,
			FailureReason: r.FailureReason,
			Blocker:       r.Blocker,
			Context:       r.Context,
			CreatedAt:     r.CreatedAt,
		})
	}

	searchNote := "chronological browse (no query)"
	if query != "" {
		searchNote = fmt.Sprintf("keyword search: %q", query)
	}

	return jsonResult(map[string]interface{}{
		"rejected_approaches": items,
		"count":               len(items),
		"search":              searchNote,
		"note":                "Rejected approaches are permanent audit trail entries. Use memory(action='abandon') to record new ones.",
	})
}

// summariseDecision (defined in handlers_decisions.go) is reused here to
// truncate approach and failure_reason text to ~60 runes for inline messages.

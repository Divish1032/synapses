package mcp

// handlers_decisions.go — Sprint 24.5: Decision journaling in memory.
//
// Decisions are immutable structured records of architectural/implementation
// choices: what was decided, what alternatives were evaluated, why this choice
// was made, and the context. Unlike hypotheses (mutable state machine),
// decisions are permanent — once recorded they stand as an audit trail.
//
// Future sessions retrieve decisions via:
//   memory(action="list_decisions")        — list/search
//   memory(action="decide")                — create (write path)
//
// Decisions also appear in the compaction recovery packet so the agent
// doesn't re-derive past architectural choices after context compaction.

import (
	"context"
	"fmt"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/store"
)

// handleDecide records a new decision. All fields except choice and agent_id
// are optional — a minimal decision is just "I chose X."
//
// Params: agent_id (required), choice (required), alternatives, reasoning, context.
func (s *Server) handleDecide(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("decision journaling unavailable: run 'synapses start' to create a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}

	choice, choiceErr := stringArgLimited(req, "choice", maxArgLengthDecision)
	if choiceErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(choiceErr.Error())), nil
	}
	if choice == "" {
		return mcp.NewToolResultError("choice is required — describe what was decided (e.g. 'Use JWT with RS256 for authentication')"), nil
	}

	alternatives, altErr := stringArgLimited(req, "alternatives", maxArgLengthRationale)
	if altErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(altErr.Error())), nil
	}

	reasoning, reasonErr := stringArgLimited(req, "reasoning", maxArgLengthRationale)
	if reasonErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(reasonErr.Error())), nil
	}

	decContext, ctxErr := stringArgLimited(req, "context", maxArgLengthDecision)
	if ctxErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(ctxErr.Error())), nil
	}

	// Scan all text fields for prompt injection.
	for fieldName, fieldVal := range map[string]*string{
		"choice":       &choice,
		"alternatives": &alternatives,
		"reasoning":    &reasoning,
		"context":      &decContext,
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

	d := store.Decision{
		AgentID:      agentID,
		ProjectID:    s.projectID,
		Choice:       choice,
		Alternatives: alternatives,
		Reasoning:    reasoning,
		Context:      decContext,
	}

	id, err := s.store.InsertDecision(d)
	if err != nil {
		return toolError("record decision", err)
	}

	return jsonResult(map[string]interface{}{
		"decision_id": id,
		"choice":      choice,
		"message":     fmt.Sprintf("Decision recorded (id=%s). Future sessions will see: %q when this area is revisited.", id, summariseDecision(choice)),
	})
}

// handleListDecisions returns decisions for the current project/agent.
// Supports optional keyword search across choice, reasoning, context, and alternatives.
//
// Params: agent_id (required), query (optional, keyword search), limit (optional, default 20).
func (s *Server) handleListDecisions(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("decision journaling unavailable: run 'synapses start' to create a persistent store"), nil
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

	decisions, err := s.store.SearchDecisions(agentID, s.projectID, query, limit)
	if err != nil {
		return toolError("list decisions", err)
	}

	type decisionItem struct {
		ID           string `json:"id"`
		Choice       string `json:"choice"`
		Alternatives string `json:"alternatives,omitempty"`
		Reasoning    string `json:"reasoning,omitempty"`
		Context      string `json:"context,omitempty"`
		CreatedAt    int64  `json:"created_at"`
	}
	items := make([]decisionItem, 0, len(decisions))
	for _, d := range decisions {
		items = append(items, decisionItem{
			ID:           d.ID,
			Choice:       d.Choice,
			Alternatives: d.Alternatives,
			Reasoning:    d.Reasoning,
			Context:      d.Context,
			CreatedAt:    d.CreatedAt,
		})
	}

	searchNote := "chronological browse (no query)"
	if query != "" {
		searchNote = fmt.Sprintf("keyword search: %q", query)
	}

	return jsonResult(map[string]interface{}{
		"decisions": items,
		"count":     len(items),
		"search":    searchNote,
		"note":      "Decisions are immutable audit trail entries. Use memory(action='decide') to record new decisions.",
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

// summariseDecision truncates choice text to ~60 runes for inline messages.
// Uses rune count to avoid splitting multi-byte UTF-8 characters.
func summariseDecision(choice string) string {
	const maxLen = 60
	runes := []rune(choice)
	if len(runes) <= maxLen {
		return choice
	}
	return string(runes[:maxLen]) + "…"
}

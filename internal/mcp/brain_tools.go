package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Divish1032/synapses/internal/brain"
)

// getBrainClient type-asserts the stored brainClient to *brain.Client.
// Returns nil if no brain client is configured.
func (s *Server) getBrainClient() *brain.Client {
	if s.brainClient == nil {
		return nil
	}
	bc, _ := s.brainClient.(*brain.Client)
	return bc
}

// handleSDLC dispatches between get and set based on the action param.
func (s *Server) handleSDLC(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action := stringArg(req, "action")
	if action == "" {
		action = "get"
	}
	switch action {
	case "get":
		return s.handleGetSDLCConfig(ctx, req)
	case "set":
		return s.handleSetSDLCPhase(ctx, req)
	default:
		return mcp.NewToolResultError("action must be 'get' or 'set'"), nil
	}
}

// handleLogDecision records an agent decision to the intelligence service.
func (s *Server) handleLogDecision(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	bc := s.getBrainClient()
	if bc == nil {
		return jsonResult(map[string]interface{}{"status": "brain not configured"})
	}

	agentID := stringArg(req, "agent_id")
	entityName := stringArg(req, "entity_name")
	action := stringArg(req, "action")
	if agentID == "" || entityName == "" || action == "" {
		return mcp.NewToolResultError("agent_id, entity_name, and action are required"), nil
	}

	relatedRaw := stringArg(req, "related_entities")
	var related []string
	if relatedRaw != "" {
		related = splitCSV(relatedRaw)
	}

	bc.LogDecision(context.Background(), brain.DecisionRequest{
		AgentID:         agentID,
		Phase:           stringArg(req, "phase"),
		EntityName:      entityName,
		Action:          action,
		RelatedEntities: related,
		Outcome:         stringArg(req, "outcome"),
		Notes:           stringArg(req, "notes"),
	})
	return jsonResult(map[string]interface{}{"status": "recorded"})
}

// handleSetSDLCPhase updates the SDLC phase on the intelligence service.
func (s *Server) handleSetSDLCPhase(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	bc := s.getBrainClient()
	if bc == nil {
		return mcp.NewToolResultError("brain not configured — add brain.url to synapses.json"), nil
	}

	phase := stringArg(req, "phase")
	if phase == "" {
		return mcp.NewToolResultError("phase is required (planning|development|testing|review|deployment)"), nil
	}
	validPhases := map[string]bool{
		"planning": true, "development": true, "testing": true,
		"review": true, "deployment": true,
	}
	if !validPhases[phase] {
		return mcp.NewToolResultError(fmt.Sprintf("invalid phase %q — must be one of: planning, development, testing, review, deployment", phase)), nil
	}

	result, err := bc.SetPhase(context.Background(), brain.SetPhaseRequest{
		Phase:   phase,
		AgentID: stringArg(req, "agent_id"),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("set phase: %v", err)), nil
	}
	return jsonResult(result)
}

// handleGetSDLCConfig returns the current SDLC phase and quality mode.
func (s *Server) handleGetSDLCConfig(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	bc := s.getBrainClient()
	if bc == nil {
		return jsonResult(map[string]interface{}{
			"phase":        "development",
			"quality_mode": "standard",
			"hint":         "brain not configured — these are defaults. Add brain.url to synapses.json to manage SDLC phase.",
		})
	}

	phase, qualityMode := bc.GetSDLC(context.Background())
	return jsonResult(map[string]interface{}{
		"phase":        phase,
		"quality_mode": qualityMode,
	})
}

// splitCSV splits a comma-separated string into a trimmed slice.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := make([]string, 0)
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			part := trimSpaces(s[start:i])
			if part != "" {
				parts = append(parts, part)
			}
			start = i + 1
		}
	}
	return parts
}

func trimSpaces(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

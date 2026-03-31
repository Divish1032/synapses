package mcp

// handlers_merged.go — Phase 5 dispatcher handlers that route merged tools
// to their original handler implementations based on action/mode/phase params.

import (
	"context"
	"fmt"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// ── Merge 1: search + find_entity ──────────────────────────────────────────

func (s *Server) handleSearchDispatch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	mode, _ := req.GetArguments()["mode"].(string)
	if mode == "" {
		mode = "keyword"
	}
	if mode == "exact" {
		return s.handleFindEntity(ctx, req)
	}
	result, err := s.handleSearch(ctx, req)
	if err != nil || result == nil {
		return result, err
	}
	// Sprint 27.3: inject reactive suggestions into search results.
	s.injectSearchSuggestions(ctx, result, req)
	return result, nil
}

// ── Merge 2: get_context + prepare_context + get_call_chain ────────────────

func (s *Server) handleGetContextDispatch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	mode, _ := req.GetArguments()["mode"].(string)
	if mode == "" {
		mode = "context"
	}
	switch mode {
	case "path":
		return s.handleGetCallChain(ctx, req)
	case "intent":
		return s.handlePrepareContext(ctx, req)
	case "investigate":
		return s.handleInvestigate(ctx, req)
	default:
		return s.handleGetContext(ctx, req)
	}
}

// ── Merge 3: validate ──────────────────────────────────────────────────────

func (s *Server) handleValidateDispatch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	phase, _ := req.GetArguments()["phase"].(string)
	if phase == "" {
		phase = "pre"
	}

	// Guard: phases that require a code graph must not be called in knowledge mode.
	// handleValidatePlan has its own nil-graph guard, but handleVerifyImplementation,
	// handleGetViolations, and handlePlanContext do not — they would panic.
	switch phase {
	case "post", "list", "full":
		if s.graph == nil {
			return mcp.NewToolResultError(fmt.Sprintf(
				"validate(phase=%q) requires a code graph. In knowledge mode, only phase=\"safety\" is available.", phase)), nil
		}
	}

	var result *mcp.CallToolResult
	var err error
	switch phase {
	case "pre":
		result, err = s.handleValidatePlan(ctx, req)
	case "post":
		result, err = s.handleVerifyImplementation(ctx, req)
	case "list":
		result, err = s.handleGetViolations(ctx, req)
	case "full":
		result, err = s.handlePlanContext(ctx, req)
	case "safety":
		result, err = s.handleCheckPlanSafety(ctx, req)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unknown validate phase: %q (valid: pre, post, list, full, safety)", phase)), nil
	}
	if err == nil && result != nil {
		// Sprint 27.3: inject reactive suggestions into validate results.
		s.injectValidateSuggestions(ctx, result, req)
	}
	return result, err
}

// ── Merge 4: memory ────────────────────────────────────────────────────────

func (s *Server) handleMemoryDispatch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	action, _ := req.GetArguments()["action"].(string)
	if action == "" {
		return mcp.NewToolResultError("action is required (valid: save, search, list)"), nil
	}
	switch action {
	case "save":
		return s.handleRemember(ctx, req)
	case "search":
		return s.handleRecall(ctx, req)
	case "list":
		return s.handleGetEpisodes(ctx, req)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unknown memory action: %q (valid: save, search, list)", action)), nil
	}
}

// ── Merge 5: tasks ─────────────────────────────────────────────────────────

func (s *Server) handleTasksDispatch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	action, _ := req.GetArguments()["action"].(string)
	if action == "" {
		return mcp.NewToolResultError("action is required (valid: create_plan, list_plans, pending, update, save_state, get_state, link_nodes)"), nil
	}
	switch action {
	case "create_plan":
		return s.handleCreatePlan(ctx, req)
	case "list_plans":
		return s.handleGetPlans(ctx, req)
	case "pending":
		return s.handleGetPendingTasks(ctx, req)
	case "update":
		return s.handleUpdateTask(ctx, req)
	case "save_state":
		return s.handleSaveSessionState(ctx, req)
	case "get_state":
		return s.handleGetSessionState(ctx, req)
	case "link_nodes":
		return s.handleLinkTaskNodes(ctx, req)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unknown tasks action: %q (valid: create_plan, list_plans, pending, update, save_state, get_state, link_nodes)", action)), nil
	}
}

// ── Merge 6: rules ─────────────────────────────────────────────────────────

func (s *Server) handleRulesDispatch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	action, _ := req.GetArguments()["action"].(string)
	if action == "" {
		return mcp.NewToolResultError("action is required (valid: upsert, delete, candidates, upsert_adr, list_adrs)"), nil
	}
	switch action {
	case "upsert":
		return s.handleUpsertRule(ctx, req)
	case "delete":
		return s.handleDeleteRule(ctx, req)
	case "candidates":
		return s.handleGetRuleCandidates(ctx, req)
	case "upsert_adr":
		return s.handleUpsertADR(ctx, req)
	case "list_adrs":
		return s.handleGetADRs(ctx, req)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unknown rules action: %q (valid: upsert, delete, candidates, upsert_adr, list_adrs)", action)), nil
	}
}

// ── Merge 7: annotate ──────────────────────────────────────────────────────

func (s *Server) handleAnnotateDispatch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	action, _ := req.GetArguments()["action"].(string)
	if action == "" {
		action = "add"
	}
	switch action {
	case "add":
		return s.handleAnnotateNode(ctx, req)
	case "add_web":
		return s.handleWebAnnotate(ctx, req)
	case "add_gap":
		return s.handleUpsertGap(ctx, req)
	case "list_gaps":
		return s.handleGetGaps(ctx, req)
	case "history":
		return s.handleGetEntityHistory(ctx, req)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unknown annotate action: %q (valid: add, add_web, add_gap, list_gaps, history)", action)), nil
	}
}

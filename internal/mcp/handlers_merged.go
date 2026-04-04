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
	// handleValidatePreWrite has its own nil-graph guard (returns advisory).
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
	case "pre_write":
		// Sprint 27.2: mid-write validation — natural-language description of proposed
		// changes checked against security patterns, architectural rules, and observed norms.
		result, err = s.handleValidatePreWrite(ctx, req)
	case "post":
		result, err = s.handleVerifyImplementation(ctx, req)
	case "list":
		result, err = s.handleGetViolations(ctx, req)
	case "full":
		result, err = s.handlePlanContext(ctx, req)
	case "safety":
		result, err = s.handleCheckPlanSafety(ctx, req)
	// Sprint 23.9: rules management merged into validate.
	case "upsert_rule":
		result, err = s.handleUpsertRule(ctx, req)
	case "delete_rule":
		result, err = s.handleDeleteRule(ctx, req)
	case "candidates":
		result, err = s.handleGetRuleCandidates(ctx, req)
	case "upsert_adr":
		result, err = s.handleUpsertADR(ctx, req)
	case "list_adrs":
		result, err = s.handleGetADRs(ctx, req)
	default:
		return mcp.NewToolResultError(fmt.Sprintf(
			"unknown validate phase: %q (valid: pre, pre_write, post, list, full, safety, upsert_rule, delete_rule, candidates, upsert_adr, list_adrs)", phase)), nil
	}
	if err == nil && result != nil {
		// Sprint 27.3: inject reactive suggestions into validate results.
		s.injectValidateSuggestions(ctx, result, req)
	}

	// Sprint 30.3: CRITICAL enforcement — phases that run security scanning must
	// block when action_required is "block" (i.e. ≥1 CRITICAL finding). The agent
	// must pass override=true to proceed. Overrides are logged as episodes so there
	// is an audit trail of every time a CRITICAL finding was bypassed.
	//
	// Only phases that produce security findings can block. Management and info
	// phases (list, safety, upsert_rule, …) never block.
	if err == nil && result != nil && !result.IsError {
		// "full" maps to handlePlanContext which never emits action_required — exclude it.
		blockingPhase := phase == "pre" || phase == "pre_write" || phase == "post"
		if blockingPhase {
			if reason, isBlocked := extractBlockReason(result); isBlocked {
				override, _ := req.GetArguments()["override"].(bool)
				if !override {
					return mcp.NewToolResultError(
						fmt.Sprintf("BLOCKED: %s. Pass override=true to proceed.", reason),
					), nil
				}
				// override=true: log an audit episode so the bypass is traceable, then let
				// the normal result through. Episode failure must never block the override.
				s.logCriticalOverride(ctx, req, phase, reason)
			}
		}
	}

	// Sprint 30.1: KV format — compact labeled output for validate responses.
	if err == nil && result != nil && !result.IsError {
		if format, _ := req.GetArguments()["format"].(string); format == "kv" {
			detailLevel, _ := req.GetArguments()["detail_level"].(string)
			if detailLevel == "" {
				detailLevel = "summary"
			}
			tokenBudget := 300
			if tb, ok := req.GetArguments()["token_budget"].(float64); ok && tb > 0 {
				tokenBudget = int(tb)
			}
			result = reformatValidateKV(result, phase, req, detailLevel, tokenBudget)
		}
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
		return mcp.NewToolResultError("action is required (valid: save, search, list, annotate, annotate_web, add_gap, list_gaps, history, hypothesize, list_hypotheses, decide, list_decisions, abandon, list_rejected)"), nil
	}
	switch action {
	case "save":
		result, err := s.handleRemember(ctx, req)
		// Sprint 29.6: detect preference signals in the decision text and record
		// user_preference session observations. Fire-and-forget — errors silently
		// dropped so they never affect the memory save result.
		if err == nil && result != nil && !result.IsError {
			s.maybeRecordUserPrefObs(ctx, req)
		}
		return result, err
	case "search":
		return s.handleRecall(ctx, req)
	case "list":
		return s.handleGetEpisodes(ctx, req)
	// Sprint 23.9: annotate actions merged into memory.
	case "annotate":
		return s.handleAnnotateNode(ctx, req)
	case "annotate_web":
		return s.handleWebAnnotate(ctx, req)
	case "add_gap":
		return s.handleUpsertGap(ctx, req)
	case "list_gaps":
		return s.handleGetGaps(ctx, req)
	case "history":
		return s.handleGetEntityHistory(ctx, req)
	// Sprint 24.4: hypothesis tracking.
	case "hypothesize":
		return s.handleHypothesize(ctx, req)
	case "list_hypotheses":
		return s.handleListHypotheses(ctx, req)
	// Sprint 24.5: decision journaling.
	case "decide":
		return s.handleDecide(ctx, req)
	case "list_decisions":
		return s.handleListDecisions(ctx, req)
	// Sprint 24.6: rejected approach memory.
	case "abandon":
		return s.handleAbandon(ctx, req)
	case "list_rejected":
		return s.handleListRejectedApproaches(ctx, req)
	default:
		return mcp.NewToolResultError(fmt.Sprintf(
			"unknown memory action: %q (valid: save, search, list, annotate, annotate_web, add_gap, list_gaps, history, hypothesize, list_hypotheses, decide, list_decisions, abandon, list_rejected)", action)), nil
	}
}

// ── Merge 5: tasks ─────────────────────────────────────────────────────────

func (s *Server) handleTasksDispatch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	action, _ := req.GetArguments()["action"].(string)
	if action == "" {
		return mcp.NewToolResultError("action is required (valid: create_plan, list_plans, pending, get, update, save_state, get_state, link_nodes, update_spec_item, set_tracked_files)"), nil
	}
	switch action {
	case "create_plan":
		return s.handleCreatePlan(ctx, req)
	case "list_plans":
		return s.handleGetPlans(ctx, req)
	case "pending":
		return s.handleGetPendingTasks(ctx, req)
	// Sprint 29.7 (Concern 3): fetch a single task with prior learning memories.
	case "get":
		return s.handleGetTask(ctx, req)
	case "update":
		return s.handleUpdateTask(ctx, req)
	case "save_state":
		return s.handleSaveSessionState(ctx, req)
	case "get_state":
		return s.handleGetSessionState(ctx, req)
	case "link_nodes":
		return s.handleLinkTaskNodes(ctx, req)
	// Sprint 25.1: Spec coverage tracking — mark individual spec items done/pending.
	case "update_spec_item":
		return s.handleUpdateSpecItem(ctx, req)
	// Sprint 25.2: Multi-file change tracking — register files expected to be modified.
	case "set_tracked_files":
		return s.handleSetTrackedFiles(ctx, req)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unknown tasks action: %q (valid: create_plan, list_plans, pending, get, update, save_state, get_state, link_nodes, update_spec_item, set_tracked_files)", action)), nil
	}
}

// handleGetTask returns a single task by ID including up to 2 prior learning
// memories. Agents use this to review what a previous session accomplished or
// attempted before starting new work. Introduced in Sprint 29.7 (Concern 3).
//
// Required: task_id
func (s *Server) handleGetTask(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}
	taskID := stringArg(req, "task_id")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}
	task, err := s.store.GetTask(taskID)
	if err != nil || task == nil {
		return mcp.NewToolResultError(fmt.Sprintf("task %q not found", taskID)), nil
	}

	result := map[string]interface{}{
		"task": task,
	}

	// Attach prior learnings when they exist. Cap at 2 entries, each at 500 runes,
	// so the response stays usable without overwhelming the context window.
	if mems, merr := s.store.GetMemoriesByTaskID(taskID); merr == nil && len(mems) > 0 {
		type learningEntry struct {
			Content   string `json:"content"`
			CreatedAt string `json:"created_at"`
		}
		limit := 2
		if len(mems) < limit {
			limit = len(mems)
		}
		entries := make([]learningEntry, limit)
		for i := 0; i < limit; i++ {
			content := mems[i].Content
			if runes := []rune(content); len(runes) > 500 {
				content = string(runes[:500]) + "…"
			}
			entries[i] = learningEntry{Content: content, CreatedAt: mems[i].CreatedAt}
		}
		result["prior_learnings"] = entries
		result["prior_learnings_count"] = len(mems)
	}

	return jsonResult(result)
}

// Sprint 23.9: handleRulesDispatch removed — rules management merged into handleValidateDispatch.
// Sprint 23.9: handleAnnotateDispatch removed — annotation merged into handleMemoryDispatch.

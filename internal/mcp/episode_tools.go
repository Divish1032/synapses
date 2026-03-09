package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/store"
)

// handleRemember persists an episode (decision or failure) so future sessions
// can recall it. Fires a failure_recorded event when episode_type='failure'.
func (s *Server) handleRemember(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("episodic memory unavailable: server started without a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}
	decision := stringArg(req, "decision")
	if decision == "" {
		return mcp.NewToolResultError("decision is required"), nil
	}

	episodeType := stringArg(req, "episode_type")
	if episodeType == "" {
		episodeType = "decision"
	}
	validTypes := map[string]bool{"decision": true, "failure": true, "pattern": true, "rule_proposal": true}
	if !validTypes[episodeType] {
		return mcp.NewToolResultError("episode_type must be one of: decision, failure, pattern, rule_proposal"), nil
	}

	outcome := stringArg(req, "outcome")
	if outcome == "" {
		outcome = "unknown"
	}
	validOutcomes := map[string]bool{"success": true, "failure": true, "partial": true, "unknown": true}
	if !validOutcomes[outcome] {
		return mcp.NewToolResultError("outcome must be one of: success, failure, partial, unknown"), nil
	}

	importance := 0.5
	if v, ok := req.GetArguments()["importance"].(float64); ok && v >= 0 && v <= 1 {
		importance = v
	}

	e := store.Episode{
		AgentID:       agentID,
		ProjectID:     stringArg(req, "project_id"),
		CreatedAt:     time.Now().Unix(),
		EpisodeType:   episodeType,
		Outcome:       outcome,
		Trigger:       stringArg(req, "trigger"),
		Decision:      decision,
		Rationale:     stringArg(req, "rationale"),
		AffectedFiles: stringArgDefault(req, "affected_files", "[]"),
		AffectedNodes: stringArgDefault(req, "affected_nodes", "[]"),
		Tags:          stringArgDefault(req, "tags", "[]"),
		Importance:    importance,
	}

	s.upsertAgentIfNeeded(agentID)

	id, err := s.store.RememberEpisode(e)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("remember episode: %v", err)), nil
	}

	if episodeType == "failure" {
		_ = s.store.AppendEvent("failure_recorded", agentID,
			fmt.Sprintf(`{"episode_id":%q,"outcome":%q,"trigger":%q}`,
				id, outcome, e.Trigger))
	}

	return jsonResult(map[string]interface{}{
		"episode_id":   id,
		"episode_type": episodeType,
		"outcome":      outcome,
		"message":      "Episode recorded. Use recall() to surface similar past episodes in future sessions.",
	})
}

// handleRecall performs FTS5 BM25 search over episodes and returns the top
// matches. v1 uses top-N without a score threshold — caller decides relevance.
func (s *Server) handleRecall(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("episodic memory unavailable: server started without a persistent store"), nil
	}

	query := stringArg(req, "query")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	limit := 5
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	sinceDays := 0
	if v, ok := req.GetArguments()["since_days"].(float64); ok && v > 0 {
		sinceDays = int(v)
	}
	_ = sinceDays // passed to GetEpisodes; RecallEpisodes uses FTS rank ordering

	episodes, err := s.store.RecallEpisodes(
		query,
		stringArg(req, "project_id"),
		stringArg(req, "agent_id"),
		stringArg(req, "episode_type"),
		stringArg(req, "outcome_filter"),
		limit,
	)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("recall episodes: %v", err)), nil
	}

	// Surface any dynamic rules derived from matching failure episodes.
	var relatedRules []string
	for _, ep := range episodes {
		if ep.PromotedRule != "" && ep.EpisodeType == "failure" {
			relatedRules = append(relatedRules, ep.PromotedRule)
		}
	}

	summary := "no matching episodes"
	if len(episodes) > 0 {
		summary = fmt.Sprintf("%d episode(s) matching %q", len(episodes), query)
	}

	result := map[string]interface{}{
		"summary":       summary,
		"episodes":      episodes,
		"related_rules": relatedRules,
		"hint":          "Episodes ordered by relevance (best match first). Check related_rules for constraints derived from similar past failures.",
	}
	return jsonResult(result)
}

// handleGetEpisodes lists episodes with optional filters, ordered by
// created_at DESC. Does not perform FTS search — use recall() for that.
func (s *Server) handleGetEpisodes(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("episodic memory unavailable: server started without a persistent store"), nil
	}

	limit := 20
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	sinceDays := 0
	if v, ok := req.GetArguments()["since_days"].(float64); ok && v > 0 {
		sinceDays = int(v)
	}

	// Parse tags: accept comma-separated string or JSON array.
	var tags []string
	if raw := stringArg(req, "tags"); raw != "" {
		for _, t := range strings.Split(raw, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
	}

	episodes, err := s.store.GetEpisodes(
		stringArg(req, "project_id"),
		stringArg(req, "agent_id"),
		stringArg(req, "episode_type"),
		tags,
		limit,
		sinceDays,
	)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get episodes: %v", err)), nil
	}

	summary := "no episodes found"
	if len(episodes) > 0 {
		summary = fmt.Sprintf("%d episode(s)", len(episodes))
	}
	return jsonResult(map[string]interface{}{
		"summary":  summary,
		"episodes": episodes,
		"hint":     "Use recall(query=...) for semantic search. Use remember() to record new decisions or failures.",
	})
}

// handleCheckPlanSafety searches failure episodes for the closest match to the
// proposed plan and returns a Recovery Packet if found. Non-blocking by design:
// enforces a 500ms timeout so a slow FTS query never delays the agent.
// Returns "clear" on timeout or search error.
func (s *Server) handleCheckPlanSafety(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("episodic memory unavailable: server started without a persistent store"), nil
	}

	planDesc := stringArg(req, "plan_description")
	if planDesc == "" {
		return mcp.NewToolResultError("plan_description is required"), nil
	}

	agentID := stringArg(req, "agent_id")
	projectID := stringArg(req, "project_id")

	// 500ms hard cap: safety check must never block the agent.
	safetyCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	type result struct {
		ep  *store.Episode
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ep, err := s.store.CheckPlanSafety(planDesc, projectID)
		ch <- result{ep, err}
	}()

	var match *store.Episode
	var err error
	select {
	case r := <-ch:
		match, err = r.ep, r.err
	case <-safetyCtx.Done():
		return jsonResult(map[string]interface{}{
			"status": "clear",
			"hint":   "Safety check timed out (>500ms). Proceed with validate_plan().",
		})
	}

	_ = safetyCtx
	if err != nil {
		// Fail-safe: don't block the agent on a search error.
		return jsonResult(map[string]interface{}{
			"status":  "clear",
			"message": fmt.Sprintf("Safety check skipped (search error: %v). Proceed with validate_plan().", err),
		})
	}

	if match == nil {
		return jsonResult(map[string]interface{}{
			"status": "clear",
			"hint":   "No failure episodes recorded yet. Record failures with remember(episode_type='failure') to build the Hall of Shame.",
		})
	}

	// Store the interjection itself as a new episode so the pattern is reinforced.
	if agentID != "" {
		interjection := store.Episode{
			AgentID:     agentID,
			ProjectID:   projectID,
			EpisodeType: "pattern",
			Outcome:     "unknown",
			Trigger:     fmt.Sprintf("check_plan_safety matched episode %s", match.ID),
			Decision:    fmt.Sprintf("Plan resembles past failure: %s", match.Decision),
			Rationale:   "Reactive interjection fired; agent was warned before execution.",
			Tags:        `["interjection"]`,
			Importance:  0.6,
		}
		_, _ = s.store.RememberEpisode(interjection)
	}

	return jsonResult(map[string]interface{}{
		"status": "warning",
		"match": map[string]interface{}{
			"episode_id": match.ID,
			"trigger":    match.Trigger,
			"decision":   match.Decision,
			"outcome":    match.Outcome,
			"rationale":  match.Rationale,
			"tags":       match.Tags,
			"created_at": match.CreatedAt,
		},
		"message": fmt.Sprintf(
			"⚠ Past failure match found [%s]: %q (outcome: %s). Review rationale before proceeding. Then run validate_plan() for structural checks.",
			match.ID, match.Decision, match.Outcome,
		),
		"hint": "If this failure is not relevant to your plan, proceed. If it is, revise your approach or add a safety check.",
	})
}

// handleGetRuleCandidates returns failure episodes that have appeared ≥N times
// and have not yet been promoted to a dynamic rule. Agents can review these and
// call upsert_rule() + mark_episode_promoted() to close the feedback loop.
func (s *Server) handleGetRuleCandidates(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("episodic memory unavailable: server started without a persistent store"), nil
	}

	minOccurrences := 2
	if v, ok := req.GetArguments()["min_occurrences"].(float64); ok && v > 0 {
		minOccurrences = int(v)
	}

	candidates, err := s.store.GetRuleCandidates(minOccurrences)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get rule candidates: %v", err)), nil
	}

	summary := fmt.Sprintf("no failure patterns with ≥%d occurrences yet", minOccurrences)
	if len(candidates) > 0 {
		summary = fmt.Sprintf("%d rule candidate(s) ready for promotion", len(candidates))
	}
	return jsonResult(map[string]interface{}{
		"summary":    summary,
		"candidates": candidates,
		"hint":       "For each candidate: call upsert_rule() to enforce it structurally, then call mark_episode_promoted(episode_id, rule_id) to close the loop.",
	})
}

// stringArgDefault returns the string value of a named argument, or def if absent/empty.
func stringArgDefault(req mcp.CallToolRequest, name, def string) string {
	v := stringArg(req, name)
	if v == "" {
		return def
	}
	return v
}

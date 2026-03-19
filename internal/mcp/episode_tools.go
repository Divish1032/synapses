package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

	// AM-1: Parse optional anchor_nodes for binding memories to graph nodes.
	var anchorNodes []string
	if raw := stringArg(req, "anchor_nodes"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &anchorNodes); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("anchor_nodes must be a JSON array of strings: %v", err)), nil
		}
		// Validate node ID format: must contain "::" separator (e.g. "repo::file.go::FuncName").
		for _, nid := range anchorNodes {
			if nid != "" && !strings.Contains(nid, "::") {
				return mcp.NewToolResultError(fmt.Sprintf("invalid anchor node ID %q: must contain '::' separator (e.g. 'repo::pkg/file.go::FuncName')", nid)), nil
			}
		}
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

	// ── Dual-write to unified memories table ──
	// Failures with affected nodes → entity-tier memories.
	// All episodes → project-tier memory (decisions, patterns, failures).
	memContent := e.Decision
	if e.Rationale != "" {
		memContent += " — " + e.Rationale
	}
	memTags := fmt.Sprintf(`["episode","%s"]`, episodeType)

	// Entity-tier: one memory per affected node (failures & patterns only).
	if episodeType == "failure" || episodeType == "pattern" {
		var affectedNodes []string
		_ = json.Unmarshal([]byte(e.AffectedNodes), &affectedNodes)
		for _, nodeID := range affectedNodes {
			_, _ = s.store.InsertMemoryWithAnchors(store.Memory{
				Tier:     store.TierEntity,
				Content:  memContent,
				EntityID: nodeID,
				AgentID:  agentID,
				TaskID:   e.ProjectID,
				Source:   store.SourceManual,
				Tags:     memTags,
			}, anchorNodes)
		}
	}

	// Project-tier: always write the episode as project knowledge.
	_, _ = s.store.InsertMemoryWithAnchors(store.Memory{
		Tier:    store.TierProject,
		Content: memContent,
		AgentID: agentID,
		TaskID:  e.ProjectID,
		Source:  store.SourceManual,
		Tags:    memTags,
	}, anchorNodes)

	if episodeType == "failure" {
		if err := s.store.AppendEvent("failure_recorded", agentID,
			fmt.Sprintf(`{"episode_id":%q,"outcome":%q,"trigger":%q}`,
				id, outcome, e.Trigger)); err != nil {
			fmt.Fprintf(os.Stderr, "synapses: append failure_recorded event: %v\n", err)
		}
	}

	resp := map[string]interface{}{
		"episode_id":   id,
		"episode_type": episodeType,
		"outcome":      outcome,
		"message":      "Episode recorded. Use recall() to surface similar past episodes in future sessions.",
	}
	if len(anchorNodes) > 0 {
		resp["anchored_to"] = len(anchorNodes)
	} else {
		resp["tier_hint"] = "If this memory describes a code entity, architecture fact, or task context derived from the graph, add anchor_nodes=[\"node_id\"] so it auto-invalidates when the code changes. Use find_entity() to get a node ID first."
	}

	// F12: Auto-create a fix task when:
	//   • episode_type is "failure", AND
	//   • create_fix_task=true is explicitly requested, OR importance >= 0.7
	// The fix task is linked to the episode's affected_nodes so agents can jump
	// straight to the relevant code via get_context(task_id=...).
	createFixTask, _ := req.GetArguments()["create_fix_task"].(bool)
	if s.store != nil && episodeType == "failure" && (createFixTask || importance >= 0.7) {
		var affectedNodes []string
		_ = json.Unmarshal([]byte(e.AffectedNodes), &affectedNodes)

		fixTitle := "Fix: " + e.Decision
		if len(fixTitle) > 120 {
			fixTitle = fixTitle[:117] + "..."
		}
		fixDesc := fmt.Sprintf("Auto-created from failure episode %s.\nFailure: %s", id, e.Decision)
		if e.Rationale != "" {
			fixDesc += "\nRationale: " + e.Rationale
		}
		planID, _, perr := s.store.CreatePlan(fixTitle, fixDesc, agentID, []store.TaskInput{{
			Title:       fixTitle,
			Description: fixDesc,
			Priority:    "p1",
			LinkedNodes: affectedNodes,
		}})
		if perr == nil {
			// Retrieve the newly created task ID (one task per auto-fix plan).
			if tasks, terr := s.store.GetPendingTasks(planID, ""); terr == nil && len(tasks) > 0 {
				resp["fix_task_id"] = tasks[0].ID
				resp["fix_plan_id"] = planID
				resp["message"] = resp["message"].(string) + fmt.Sprintf(" Fix task created (id=%s).", tasks[0].ID)
			}
		}
	}

	return jsonResult(resp)
}

// handleRecall searches episodic memory.
// When query is provided: FTS5 BM25 semantic search, results ordered by relevance.
// When query is empty: chronological browse (newest first), same as deprecated get_episodes.
func (s *Server) handleRecall(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("episodic memory unavailable: server started without a persistent store"), nil
	}

	query := stringArg(req, "query")

	limit := 20
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	sinceDays := 0
	if v, ok := req.GetArguments()["since_days"].(float64); ok && v > 0 {
		sinceDays = int(v)
	}

	includeStale, _ := req.GetArguments()["include_stale"].(bool)

	// Browse mode: empty query = list chronologically (newest first).
	if query == "" {
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

		// Also surface recent memories from the unified table.
		// Filter by agentID when provided (agent browsing their own history),
		// otherwise return recent project/entity memories across all agents.
		agentID := stringArg(req, "agent_id")
		var recentMems []store.Memory
		if includeStale {
			recentMems, _ = s.store.QueryMemoriesIncludingStale("", "", agentID, limit)
		} else {
			recentMems, _ = s.store.QueryMemories("", "", agentID, limit)
		}

		summary := "no episodes found"
		if len(episodes) > 0 || len(recentMems) > 0 {
			summary = fmt.Sprintf("%d episode(s), %d memory/memories", len(episodes), len(recentMems))
		}
		resp := map[string]interface{}{
			"summary":  summary,
			"episodes": episodes,
			"mode":     "browse",
			"hint":     "Ordered by creation time (newest first). 'memories' includes auto-captured memories from end_session and annotate_node. Pass query=... for relevance-ranked search.",
		}
		if len(recentMems) > 0 {
			resp["memories"] = recentMems
		}
		return jsonResult(resp)
	}

	// Search mode: FTS5 BM25.
	searchLimit := limit
	if searchLimit > 5 {
		searchLimit = 5 // default cap for search mode
	}
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		searchLimit = int(v) // explicit override
	}

	episodes, err := s.store.RecallEpisodes(
		query,
		stringArg(req, "project_id"),
		stringArg(req, "agent_id"),
		stringArg(req, "episode_type"),
		stringArg(req, "outcome_filter"),
		searchLimit,
		sinceDays,
	)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("recall episodes: %v", err)), nil
	}

	// Search unified memories table (session_log, entity, project tiers).
	// This surfaces everything written by end_session, annotate_node, remember(),
	// which is invisible to the episodes-only search above.
	var memories []store.Memory
	if includeStale {
		memories, _ = s.store.SearchMemoriesIncludingStale(query, searchLimit)
	} else {
		memories, _ = s.store.SearchMemories(query, searchLimit)
	}

	// Touch surfaced memories in background to renew TTL.
	if len(memories) > 0 {
		ids := make([]string, len(memories))
		for i, m := range memories {
			ids[i] = m.ID
		}
		go func() {
			for _, id := range ids {
				s.store.TouchMemory(id)
			}
		}()
	}

	// Cross-project episode search when projects= is provided.
	var crossProjectEpisodes []map[string]interface{}
	if projectsParam := stringArg(req, "projects"); projectsParam != "" && s.federationResolver != nil {
		var aliases []string
		for _, a := range strings.Split(projectsParam, ",") {
			if a = strings.TrimSpace(a); a != "" {
				aliases = append(aliases, a)
			}
		}
		if len(aliases) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			fedEpisodes := s.federationResolver.SearchEpisodes(ctx, query, aliases, searchLimit)
			for _, fe := range fedEpisodes {
				crossProjectEpisodes = append(crossProjectEpisodes, map[string]interface{}{
					"source":       fmt.Sprintf("[%s]", fe.Alias),
					"id":           fe.Episode.ID,
					"decision":     fe.Episode.Decision,
					"rationale":    fe.Episode.Rationale,
					"episode_type": fe.Episode.EpisodeType,
					"outcome":      fe.Episode.Outcome,
					"trigger":      fe.Episode.Trigger,
					"tags":         fe.Episode.Tags,
					"created_at":   fe.Episode.CreatedAt,
				})
			}
		}
	}

	// Cross-project recall via daemon project registry (covers all registered projects,
	// not just explicitly federated ones). This is the primary cross-project path.
	if projectsParam := stringArg(req, "projects"); projectsParam != "" && s.projectRegistry != nil {
		stores, notFound := s.resolveProjectStores(projectsParam)
		if len(notFound) > 0 {
			crossProjectEpisodes = append(crossProjectEpisodes, map[string]interface{}{
				"_error": fmt.Sprintf("unknown project(s): %s. Available: %s", strings.Join(notFound, ", "), strings.Join(s.projectRegistry.ListProjects(), ", ")),
			})
		}
		for projName, projStore := range stores {
			// Skip projects already covered by federation.
			alreadyCovered := false
			for _, ep := range crossProjectEpisodes {
				if src, ok := ep["source"].(string); ok && src == fmt.Sprintf("[%s]", projName) {
					alreadyCovered = true
					break
				}
			}
			if alreadyCovered {
				continue
			}

			eps, err := projStore.RecallEpisodes(query, "", "", "", "", searchLimit, sinceDays)
			if err == nil {
				for _, ep := range eps {
					crossProjectEpisodes = append(crossProjectEpisodes, map[string]interface{}{
						"source":       fmt.Sprintf("[%s]", projName),
						"id":           ep.ID,
						"decision":     ep.Decision,
						"rationale":    ep.Rationale,
						"episode_type": ep.EpisodeType,
						"outcome":      ep.Outcome,
						"trigger":      ep.Trigger,
						"tags":         ep.Tags,
						"created_at":   ep.CreatedAt,
					})
				}
			}
			// Also search memories.
			mems, _ := projStore.SearchMemories(query, searchLimit)
			for _, m := range mems {
				crossProjectEpisodes = append(crossProjectEpisodes, map[string]interface{}{
					"source":     fmt.Sprintf("[%s]", projName),
					"id":         m.ID,
					"decision":   m.Content,
					"tier":       m.Tier,
					"created_at": m.CreatedAt,
				})
			}
		}
	}

	// Surface any dynamic rules derived from matching failure episodes.
	var relatedRules []string
	for _, ep := range episodes {
		if ep.PromotedRule != "" && ep.EpisodeType == "failure" {
			relatedRules = append(relatedRules, ep.PromotedRule)
		}
	}

	totalMatches := len(episodes) + len(memories) + len(crossProjectEpisodes)
	summary := "no matching results"
	if totalMatches > 0 {
		summary = fmt.Sprintf("%d result(s) matching %q (%d local episode(s), %d memory/memories, %d cross-project)",
			totalMatches, query, len(episodes), len(memories), len(crossProjectEpisodes))
	}

	resp := map[string]interface{}{
		"summary":       summary,
		"episodes":      episodes,
		"related_rules": relatedRules,
		"mode":          "search",
		"hint":          "Results ordered by relevance. 'episodes' = explicit remember() calls. 'memories' = auto-captured from end_session, annotate_node, and remember() across all tiers (session_log, entity, project). Check related_rules for constraints from past failures.",
	}
	if len(memories) > 0 {
		resp["memories"] = memories
	}
	if len(crossProjectEpisodes) > 0 {
		resp["cross_project_episodes"] = crossProjectEpisodes
	}
	return jsonResult(resp)
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
		if _, err := s.store.RememberEpisode(interjection); err != nil {
			fmt.Fprintf(os.Stderr, "synapses: record interjection episode: %v\n", err)
		}
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

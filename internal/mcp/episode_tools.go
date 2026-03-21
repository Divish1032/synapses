package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/store"
)

// handleRemember persists an episode (decision or failure) so future sessions
// can recall it. Fires a failure_recorded event when episode_type='failure'.
func (s *Server) handleRemember(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("episodic memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required (e.g., 'implementer', 'reviewer')"), nil
	}
	decision, err := stringArgLimited(req, "decision", maxArgLengthDecision)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if decision == "" {
		return mcp.NewToolResultError("decision is required (e.g., 'switched auth to OAuth 2.0')"), nil
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

	// rationale is concatenated with decision before embedding — needs same tight limit.
	rationale, rationaleErr := stringArgLimited(req, "rationale", maxArgLengthRationale)
	if rationaleErr != nil {
		return mcp.NewToolResultError(rationaleErr.Error()), nil
	}

	// OF-S2: scan externally-sourced content for prompt injection patterns.
	// Covers decision and rationale — the two fields persisted to SQLite and embedded.
	var injectionWarning string
	if scanResult, scanErr := s.scanContent("decision", decision); scanErr != nil {
		return mcp.NewToolResultError(scanErr.Error()), nil
	} else {
		decision = scanResult.sanitized
		if scanResult.warning != "" {
			injectionWarning = scanResult.warning
		}
	}
	if rationale != "" {
		if scanResult, scanErr := s.scanContent("rationale", rationale); scanErr != nil {
			return mcp.NewToolResultError(scanErr.Error()), nil
		} else {
			rationale = scanResult.sanitized
			if scanResult.warning != "" && injectionWarning == "" {
				injectionWarning = scanResult.warning
			}
		}
	}

	// OF-E3: cross-project write approval gate.
	// When project_id is explicitly set and differs from the current project,
	// this is a cross-project write that requires user approval.
	reqProjectID := stringArg(req, "project_id")
	if reqProjectID != "" {
		currentProject := ""
		if s.graph != nil {
			currentProject = s.graph.RepoID()
		}
		if currentProject == "" {
			currentProject = filepath.Base(s.projectPath)
		}
		if reqProjectID != currentProject {
			// OF-E3: check for an out-of-band user-approved approval file.
			// The agent never sees the token — only the user can approve via `synapses approve`.
			if !s.approvals.checkAndConsumeApproval("cross_project_remember", agentID) {
				return s.approvals.requestApproval(
					"cross_project_remember",
					fmt.Sprintf("Agent %q writing memory to project %q (current project: %q)", agentID, reqProjectID, currentProject),
					agentID,
				), nil
			}
		}
	}

	e := store.Episode{
		AgentID:       agentID,
		ProjectID:     reqProjectID,
		CreatedAt:     time.Now().Unix(),
		EpisodeType:   episodeType,
		Outcome:       outcome,
		Trigger:       stringArg(req, "trigger"),
		Decision:      decision,
		Rationale:     rationale,
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

	// Collect written memory IDs for embedding.
	var memoryIDs []string

	// memory_importance: forwarded from caller when explicitly set.
	// "pinned" exempts from decay; float strings set the weight multiplier.
	// When empty, prepareMemory auto-computes via A-MAC admission control
	// (content_type_prior × novelty_factor) at write time.
	memImportance := stringArg(req, "memory_importance")

	// Entity-tier: one memory per affected node (failures & patterns only).
	if episodeType == "failure" || episodeType == "pattern" {
		var affectedNodes []string
		if err := json.Unmarshal([]byte(e.AffectedNodes), &affectedNodes); err != nil {
			logutil.Debug("synapses: episodes: unmarshal affected_nodes for episode %q: %v\n", id, err)
		}
		for _, nodeID := range affectedNodes {
			mid, merr := s.store.InsertMemoryWithAnchors(store.Memory{
				Tier:       store.TierEntity,
				Content:    memContent,
				EntityID:   nodeID,
				AgentID:    agentID,
				TaskID:     e.ProjectID,
				Source:     store.SourceManual,
				Tags:       memTags,
				Importance: memImportance,
			}, anchorNodes)
			if merr == nil && mid != "" {
				memoryIDs = append(memoryIDs, mid)
			}
		}
	}

	// Project-tier: always write the episode as project knowledge.
	mid, merr := s.store.InsertMemoryWithAnchors(store.Memory{
		Tier:       store.TierProject,
		Content:    memContent,
		AgentID:    agentID,
		TaskID:     e.ProjectID,
		Source:     store.SourceManual,
		Tags:       memTags,
		Importance: memImportance,
	}, anchorNodes)
	if merr == nil && mid != "" {
		memoryIDs = append(memoryIDs, mid)
	}

	// Fire-and-forget: embed newly written memories in background.
	// Timeout protects against slow model init (first call downloads ~23MB).
	if s.memoryEmbedder != nil && len(memoryIDs) > 0 {
		embedder := s.memoryEmbedder
		st := s.store
		content := memContent
		ids := make([]string, len(memoryIDs))
		copy(ids, memoryIDs)
		s.goBackground(func() {
			for _, memID := range ids {
				s.embedMemory(embedder, st, memID, content)
			}
		})
	}

	if episodeType == "failure" {
		payload, _ := json.Marshal(map[string]string{
			"episode_id": id, "outcome": outcome, "trigger": e.Trigger,
		})
		if err := s.store.AppendEvent("failure_recorded", agentID, string(payload)); err != nil {
			logutil.Warn("synapses: append failure_recorded event: %v\n", err)
		}
	}

	resp := map[string]interface{}{
		"episode_id":   id,
		"episode_type": episodeType,
		"outcome":      outcome,
		"message":      "Episode recorded. Use recall() to surface similar past episodes in future sessions.",
	}
	if injectionWarning != "" {
		resp["injection_warning"] = injectionWarning
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
		if err := json.Unmarshal([]byte(e.AffectedNodes), &affectedNodes); err != nil {
			logutil.Debug("synapses: episodes: unmarshal affected_nodes for fix task (episode %q): %v\n", id, err)
		}

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
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("episodic memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
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

	// Parse absolute time bounds: since / until (Sprint 10 #5).
	// Accepted formats: RFC3339 ("2026-03-01T00:00:00Z") or date-only ("2026-03-01").
	var sinceTime, untilTime *time.Time
	if sinceStr := stringArg(req, "since"); sinceStr != "" {
		t, err := parseFlexibleTime(sinceStr, false)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("since: %v", err)), nil
		}
		sinceTime = &t
		// Auto-derive sinceDays if the caller didn't set since_days explicitly.
		// This feeds the temporal channel with the right lookback window.
		if sinceDays == 0 {
			days := int(time.Since(t).Hours()/24) + 1
			if days > 0 {
				sinceDays = days
			}
		}
	}
	if untilStr := stringArg(req, "until"); untilStr != "" {
		t, err := parseFlexibleTime(untilStr, true) // true = end-of-day for date-only
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("until: %v", err)), nil
		}
		untilTime = &t
	}

	// Validate ordering: since must be strictly before until.
	if sinceTime != nil && untilTime != nil && !sinceTime.Before(*untilTime) {
		return mcp.NewToolResultError(fmt.Sprintf(
			"since (%s) must be before until (%s)",
			sinceTime.Format("2006-01-02"), untilTime.Format("2006-01-02"),
		)), nil
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
			raw, _ := s.store.QueryMemories("", "", agentID, limit)
			// Apply decay visibility threshold — same filter as search mode.
			// Pinned memories always pass; decayed memories are demoted but not deleted.
			for _, m := range raw {
				if store.DecayedImportanceScore(m, 0) >= store.DecayVisibilityThreshold {
					recentMems = append(recentMems, m)
				}
			}
		}

		summary := "no episodes found"
		if len(episodes) > 0 || len(recentMems) > 0 {
			summary = fmt.Sprintf("%d episode(s), %d memory/memories", len(episodes), len(recentMems))
		}
		hint := "Ordered by creation time (newest first). 'memories' includes auto-captured memories from end_session and annotate_node. Pass query=... for relevance-ranked search."
		if stringArg(req, "as_of") != "" {
			hint += " Note: as_of is only applied in search mode (with query=). Browse mode always shows current content."
		}
		if sinceTime != nil || untilTime != nil {
			hint += " Note: since/until are only applied in search mode (with query=). In browse mode, use since_days= to limit the lookback window."
		}
		resp := map[string]interface{}{
			"summary":  summary,
			"episodes": episodes,
			"mode":     "browse",
			"hint":     hint,
		}
		if len(recentMems) > 0 {
			resp["memories"] = recentMems
		}
		return jsonResult(resp)
	}

	// Sprint 10.1: parse optional as_of parameter for temporal versioned recall.
	var asOfTime *time.Time
	if asOfStr := stringArg(req, "as_of"); asOfStr != "" {
		parsed, parseErr := time.Parse(time.RFC3339, asOfStr)
		if parseErr != nil {
			// Try date-only format as fallback (e.g. "2026-03-15").
			parsed, parseErr = time.Parse("2006-01-02", asOfStr)
			if parseErr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("as_of must be RFC3339 (e.g. '2026-03-15T12:00:00Z') or date (e.g. '2026-03-15'): %v", parseErr)), nil
			}
			// Set to end of day in UTC for date-only format.
			parsed = parsed.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
		}
		asOfTime = &parsed
	}

	// Read depth for graph channel multi-hop traversal (Sprint 10 #8).
	// 0 = default (2 hops). Negative values clamp to 1 inside quadRecallSearch.
	depth := 0
	if v, ok := req.GetArguments()["depth"].(float64); ok && v > 0 {
		depth = int(v)
	}

	// Search mode: quad-channel recall (BM25 + semantic + graph + temporal).
	searchLimit := limit
	if searchLimit > 5 {
		searchLimit = 5 // default cap for search mode
	}
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		searchLimit = int(v) // explicit override
	}

	// Sprint 10.5: inflate episode fetch limit when time bounds are active,
	// same logic as quadLimit for memories. Without inflation, the top-N
	// episodes by BM25 relevance could all fall outside the time window,
	// returning 0 episodes even though in-window episodes exist at rank N+1.
	episodeLimit := searchLimit
	if sinceTime != nil || untilTime != nil {
		episodeLimit = searchLimit * 10
		if episodeLimit < 50 {
			episodeLimit = 50
		}
	}

	// Episodes: still searched via FTS5 BM25 separately (not part of RRF).
	episodes, err := s.store.RecallEpisodes(
		query,
		stringArg(req, "project_id"),
		stringArg(req, "agent_id"),
		stringArg(req, "episode_type"),
		stringArg(req, "outcome_filter"),
		episodeLimit,
		sinceDays,
	)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("recall episodes: %v", err)), nil
	}

	// Sprint 10.5: when time bounds are active, inflate the internal fetch limit
	// so post-filtering has enough candidates. Without this, in-range memories
	// ranked below the normal channel limit would be silently excluded.
	// A 10× multiplier (min 50) covers typical Synapses scale (hundreds of memories).
	quadLimit := searchLimit
	if sinceTime != nil || untilTime != nil {
		quadLimit = searchLimit * 10
		if quadLimit < 50 {
			quadLimit = 50
		}
	}

	// Quad-channel recall: 4 parallel channels merged via RRF.
	// Replaces the old sequential BM25 + vector search path.
	memories, _, staleEmbIDs, traversalInfo := s.quadRecallSearch(ctx, query, quadLimit, includeStale, sinceDays, untilTime, depth)

	// Sprint 10.5: apply absolute time bounds (since / until) as post-filters.
	// sinceTime and untilTime are only set when the caller provided since= / until=.
	// We parse m.CreatedAt as time.Time for comparison — string comparison of
	// RFC3339 is lexicographically safe for UTC but fragile if the format varies.
	if sinceTime != nil || untilTime != nil {
		filtered := memories[:0]
		for _, m := range memories {
			t, parseErr := time.Parse(time.RFC3339, m.CreatedAt)
			if parseErr != nil {
				// Unparseable created_at — keep the memory to avoid silently dropping data.
				filtered = append(filtered, m)
				continue
			}
			if sinceTime != nil && t.Before(*sinceTime) {
				continue
			}
			if untilTime != nil && t.After(*untilTime) {
				continue
			}
			filtered = append(filtered, m)
		}
		memories = filtered

		// Re-cap to searchLimit. The inflated quadLimit was only for fetching
		// enough candidates before filtering — never for returning more than
		// the user requested. Without this cap, limit=5 with 40 in-range
		// memories would return 40, violating the limit contract.
		if len(memories) > searchLimit {
			memories = memories[:searchLimit]
		}

		// Reconcile staleEmbIDs and traversalInfo.Paths to the post-cap set.
		// They were computed from the pre-filter result; IDs no longer in memories
		// (filtered out OR past the limit cap) must be removed to avoid confusing
		// agents with references to absent memories.
		if len(staleEmbIDs) > 0 || traversalInfo != nil {
			survivingIDs := make(map[string]bool, len(memories))
			for _, m := range memories {
				survivingIDs[m.ID] = true
			}
			if len(staleEmbIDs) > 0 {
				kept := staleEmbIDs[:0]
				for _, id := range staleEmbIDs {
					if survivingIDs[id] {
						kept = append(kept, id)
					}
				}
				staleEmbIDs = kept
			}
			if traversalInfo != nil && len(traversalInfo.Paths) > 0 {
				keptPaths := traversalInfo.Paths[:0]
				for _, p := range traversalInfo.Paths {
					if survivingIDs[p.MemoryID] {
						keptPaths = append(keptPaths, p)
					}
				}
				traversalInfo.Paths = keptPaths
			}
		}

		// Filter episodes by absolute time bounds (CreatedAt is Unix seconds).
		filteredEp := episodes[:0]
		for _, ep := range episodes {
			t := time.Unix(ep.CreatedAt, 0).UTC()
			if sinceTime != nil && t.Before(*sinceTime) {
				continue
			}
			if untilTime != nil && t.After(*untilTime) {
				continue
			}
			filteredEp = append(filteredEp, ep)
		}
		episodes = filteredEp
		// Re-cap episodes at searchLimit (inflated episodeLimit was for candidate fetch only).
		if len(episodes) > searchLimit {
			episodes = episodes[:searchLimit]
		}
	}

	// Sprint 10.1: apply temporal versioning — swap content with historical version.
	if asOfTime != nil && len(memories) > 0 {
		memIDs := make([]string, len(memories))
		for i, m := range memories {
			memIDs[i] = m.ID
		}
		versioned, verr := s.store.GetMemoryAsOf(memIDs, *asOfTime)
		if verr == nil && len(versioned) > 0 {
			memories = versioned
		}
	}

	// Touch surfaced memories in background to renew TTL.
	if len(memories) > 0 {
		ids := make([]string, len(memories))
		for i, m := range memories {
			ids[i] = m.ID
		}
		s.goBackground(func() {
			for _, id := range ids {
				s.store.TouchMemory(id)
			}
		})
	}

	// Emit knowledge_accessed lifecycle event when results are found.
	if len(memories) > 0 || len(episodes) > 0 {
		agentID := stringArg(req, "agent_id")
		mCount, eCount := len(memories), len(episodes)
		s.goBackground(func() {
			if err := s.store.AppendEvent("knowledge_accessed", agentID,
				fmt.Sprintf(`{"query":%q,"memories":%d,"episodes":%d}`, query, mCount, eCount)); err != nil {
				logutil.Warn("synapses: recall: append knowledge_accessed event: %v\n", err)
			}
		})
	}

	// P3-4: emit recall hit/miss event.
	if pc := s.getPulseClient(); pc != nil {
		totalResults := len(episodes) + len(memories)
		op := "recall_miss"
		if totalResults > 0 {
			op = "recall_hit"
		}
		recallAgentID := stringArg(req, "agent_id")
		projID := s.projectID
		pc.RecordMemoryOp(pulse.MemoryOperationEvent{
			Operation:   op,
			Tier:        "episodic",
			Source:      "manual",
			ResultCount: totalResults,
			AgentID:     recallAgentID,
			ProjectID:   projID,
		})
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
				// Sprint 10.5: apply time bounds to federation results.
				if sinceTime != nil && time.Unix(fe.Episode.CreatedAt, 0).UTC().Before(*sinceTime) {
					continue
				}
				if untilTime != nil && time.Unix(fe.Episode.CreatedAt, 0).UTC().After(*untilTime) {
					continue
				}
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
				"_error": fmt.Sprintf("unknown project(s): %s. Available: %s", strings.Join(notFound, ", "), strings.Join(s.allowedProjectNames(), ", ")),
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

			// Use inflated limits for cross-project search when time bounds active —
			// same rationale as local search: avoid missing in-window results
			// ranked just below the unfiltered searchLimit.
			eps, err := projStore.RecallEpisodes(query, "", "", "", "", episodeLimit, sinceDays)
			if err == nil {
				for _, ep := range eps {
					// Sprint 10.5: apply time bounds to registry episode results.
					epTime := time.Unix(ep.CreatedAt, 0).UTC()
					if sinceTime != nil && epTime.Before(*sinceTime) {
						continue
					}
					if untilTime != nil && epTime.After(*untilTime) {
						continue
					}
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
			// Also search memories; apply decay filter and time bounds for consistency.
			mems, _ := projStore.SearchMemories(query, quadLimit)
			for _, m := range mems {
				if store.DecayedImportanceScore(m, 0) < store.DecayVisibilityThreshold {
					continue // skip decayed memories in cross-project results
				}
				// Sprint 10.5: apply time bounds to cross-project memories.
				if sinceTime != nil || untilTime != nil {
					mt, parseErr := time.Parse(time.RFC3339, m.CreatedAt)
					if parseErr == nil {
						if sinceTime != nil && mt.Before(*sinceTime) {
							continue
						}
						if untilTime != nil && mt.After(*untilTime) {
							continue
						}
					}
				}
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
		"hint":          "Results ordered by relevance. 'episodes' = explicit remember() calls. 'memories' = auto-captured from end_session, annotate_node, and remember() across all tiers (session_log, entity, project). Check related_rules for constraints from past failures. stale_embedding_ids lists memories whose anchored code entity changed since the memory was written — verify before trusting.",
	}
	if len(memories) > 0 {
		resp["memories"] = memories
	}
	// Sprint 10.7: surface stale embedding IDs so agents know which memories
	// are about code entities that changed since the memory was written.
	// Agents should verify these memories before trusting their content.
	if len(staleEmbIDs) > 0 {
		resp["stale_embedding_ids"] = staleEmbIDs
	}
	if len(crossProjectEpisodes) > 0 {
		resp["cross_project_episodes"] = crossProjectEpisodes
	}
	// Sprint 10.1: annotate response when as_of filtering was applied.
	if asOfTime != nil {
		resp["as_of"] = asOfTime.Format(time.RFC3339)
		resp["as_of_note"] = "Memory content shown as it existed at the specified time. Memories with version > 0 show historical content."
	}
	// Sprint 10.5: annotate response when absolute time bounds were applied.
	// Tells the agent exactly what window was searched so it can reason about completeness.
	if sinceTime != nil || untilTime != nil {
		tf := map[string]interface{}{}
		if sinceTime != nil {
			tf["since"] = sinceTime.UTC().Format(time.RFC3339)
		}
		if untilTime != nil {
			tf["until"] = untilTime.UTC().Format(time.RFC3339)
		}
		tf["note"] = "Results are bounded to the specified time window. All result sources (local memories, episodes, cross-project) were filtered by this range."
		resp["time_filter"] = tf
	}
	// Sprint 10.8: surface graph traversal info when graph channel was active.
	// graph_traversal.paths shows the structural connections that led to each
	// graph-attributed memory — e.g. "AuthService -[CALLS]- TokenValidator".
	if traversalInfo != nil {
		resp["graph_traversal"] = traversalInfo
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
		return mcp.NewToolResultError("episodic memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
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
		return mcp.NewToolResultError("episodic memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}

	planDesc := stringArg(req, "plan_description")
	if planDesc == "" {
		return mcp.NewToolResultError("plan_description is required (e.g., 'refactor auth module to use OAuth 2.0')"), nil
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
			logutil.Warn("synapses: record interjection episode: %v\n", err)
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
		return mcp.NewToolResultError("episodic memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
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

// parseFlexibleTime parses a time string in either RFC3339 or date-only "2006-01-02" format.
// When endOfDay is true and a date-only string is given, the returned time is set to 23:59:59
// of that day (useful for "until" bounds). Returns an error if neither format matches.
func parseFlexibleTime(s string, endOfDay bool) (time.Time, error) {
	// Try RFC3339 first (most precise).
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	// Fall back to date-only "YYYY-MM-DD".
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected RFC3339 (e.g. '2026-03-01T00:00:00Z') or date (e.g. '2026-03-01'), got %q", s)
	}
	if endOfDay {
		t = t.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
	}
	return t.UTC(), nil
}

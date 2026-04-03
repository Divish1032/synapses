package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/brain/archivist"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/store"
)

// endSessionResult is the response from end_session.
type endSessionResult struct {
	Status              string                 `json:"status"`
	AgentID             string                 `json:"agent_id"`
	SessionDuration     string                 `json:"session_duration,omitempty"`
	MemoriesSaved       int                    `json:"memories_saved"`
	SessionSummary      *sessionSummary        `json:"session_summary,omitempty"`
	MemoriesExpired     int64                  `json:"memories_expired"`
	Retrospective       *store.ToolCallSummary `json:"retrospective,omitempty"`
	EffectivenessReport *EffectivenessReport   `json:"effectiveness_report,omitempty"`
}

// EffectivenessReport summarises session quality and compares to recent history (Sprint 15 #5).
type EffectivenessReport struct {
	// ContextHitRate is the fraction of context deliveries served from cache.
	ContextHitRate float64 `json:"context_hit_rate"`
	// TaskCompletionRate is 1 - tool_call_error_rate (proxy: successful calls / total calls).
	// Nil when no tool-call data is available for this session (not 0% — unknown).
	TaskCompletionRate *float64 `json:"task_completion_rate,omitempty"`
	// ToolCalls is the total number of MCP tool calls in this session.
	ToolCalls int `json:"tool_calls"`
	// TotalDeliveries is the total number of context deliveries in this session.
	TotalDeliveries int `json:"total_deliveries"`
	// FirstFetchRight is the number of deliveries that required no correction re-fetch.
	FirstFetchRight int `json:"first_fetch_right"`
	// TokensSaved is the estimated tokens saved vs full-file grep (baseline - response).
	// Omitted from JSON when zero so consumers can distinguish "no savings" from "no deliveries".
	TokensSaved int `json:"tokens_saved,omitempty"`
	// KnowledgeGrowth is the number of memories created or updated during this session.
	KnowledgeGrowth int `json:"knowledge_growth"`
	// DurationMs is the session wall-clock duration in milliseconds.
	DurationMs int64 `json:"duration_ms"`
	// Prev7d is the 7-day historical average across all previous sessions (omitted when no history).
	Prev7d *prev7dSummary `json:"prev_7d,omitempty"`
	// RecallEffectivenessRate is the fraction of recalls where the agent acted
	// on entities from the recalled memories within 5 minutes. Nil when no
	// recall data is available (not 0% — unknown).
	RecallEffectivenessRate *float64 `json:"recall_effectiveness_rate,omitempty"`
	// Message is the human-readable effectiveness summary.
	Message string `json:"message"`
}

// prev7dSummary holds the 7-day rolling averages for cross-session comparison.
type prev7dSummary struct {
	Sessions                int     `json:"sessions"`
	AvgContextHitRate       float64 `json:"avg_context_hit_rate"`
	AvgTaskCompletion       float64 `json:"avg_task_completion"`
	TotalTokensSaved        int     `json:"total_tokens_saved"`
	TotalKnowledgeGrowth    int     `json:"total_knowledge_growth"`
	AvgRecallEffectiveness  float64 `json:"avg_recall_effectiveness,omitempty"`
}

// sessionSummary captures the structured extraction from a session.
type sessionSummary struct {
	FilesTouched     []string `json:"files_touched,omitempty"`
	EntitiesExamined []string `json:"entities_examined,omitempty"`
	TasksUpdated     []string `json:"tasks_updated,omitempty"`
}

// PackageWork is one entry in the per-package work summary produced at
// end_session. Grouped by package directory so agents can see at a glance
// which packages were touched and which specific entities were examined there.
type PackageWork struct {
	Package  string   `json:"package"`
	Files    []string `json:"files"`
	Entities []string `json:"entities,omitempty"`
}

// workSummaryEnvelope wraps the per-package work list with a session timestamp.
// The SessionAt field makes every work-summary JSON payload unique across
// sessions — without it, two sessions that touched identical files and entities
// would produce byte-for-byte identical content, triggering the Jaccard-based
// dedup in InsertMemory (threshold 0.85) and silently discarding the second
// work summary.
// SessionAt is stored as a Unix timestamp (int64 seconds). A large opaque
// integer tokenizes to a single unique token in the Jaccard word-overlap
// similarity function, guaranteeing distinctness between any two sessions
// (unlike ISO 8601 strings which share most tokens and can still exceed 0.85).
type workSummaryEnvelope struct {
	Packages  []PackageWork `json:"packages"`
	SessionAt int64         `json:"session_at"` // Unix seconds — uniqueness nonce
}

// buildPackageWorkSummary groups the session's file and entity data by package
// (directory). Only files from sessSummary.FilesTouched are included; entities
// are assigned to a package when their graph node lives in a touched file.
// Returns nil when no files were touched (nothing meaningful to store).
func buildPackageWorkSummary(sess *sessionSummary, g *graph.Graph) []PackageWork {
	if sess == nil || len(sess.FilesTouched) == 0 {
		return nil
	}

	// Build a set of touched files for O(1) lookup.
	touchedSet := make(map[string]bool, len(sess.FilesTouched))
	for _, f := range sess.FilesTouched {
		touchedSet[f] = true
	}

	// Map package-dir → *PackageWork (accumulator).
	pkgMap := make(map[string]*PackageWork, len(sess.FilesTouched))
	for _, f := range sess.FilesTouched {
		dir := filepath.Dir(f)
		if dir == "." {
			dir = "<root>"
		}
		pw, ok := pkgMap[dir]
		if !ok {
			pw = &PackageWork{Package: dir}
			pkgMap[dir] = pw
		}
		pw.Files = append(pw.Files, f)
	}

	// Assign entities to packages via graph lookup.
	if g != nil {
		for _, entityName := range sess.EntitiesExamined {
			nodes := g.FindByName(entityName)
			if len(nodes) == 0 {
				continue
			}
			node := nodes[0]
			if node.File == "" || !touchedSet[node.File] {
				continue
			}
			dir := filepath.Dir(node.File)
			if dir == "." {
				dir = "<root>"
			}
			if pw, ok := pkgMap[dir]; ok {
				pw.Entities = append(pw.Entities, entityName)
			}
		}
	}

	// Sort files and entities within each package, then sort packages.
	result := make([]PackageWork, 0, len(pkgMap))
	for _, pw := range pkgMap {
		sort.Strings(pw.Files)
		sort.Strings(pw.Entities)
		result = append(result, *pw)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Package < result[j].Package
	})

	// Cap at 20 packages to bound memory content size.
	if len(result) > 20 {
		result = result[:20]
	}
	return result
}

// sessionCallEntry tracks per-connection tool call depth for RX1 auto-end detection
// and Sprint 24.7/24.8 memory-save nudging.
// One entry exists per (sessionID + agentID) pair while the session is active.
type sessionCallEntry struct {
	agentID   string
	callCount int
	startedAt time.Time
	// startSeq is the event sequence number at session start, used to scope
	// extractSessionSummary to only this session's events.
	startSeq int64
	// autoLogged is set after the first auto-log fires, preventing re-trigger until
	// the count resets (manual end_session or reconnect).
	autoLogged bool

	// Sprint 24.7: count-based memory save nudge.
	// callsSinceLastSave counts tool calls since the last memory-save action.
	// countNudgeSent prevents the count-based nudge from firing more than once
	// per save cycle.
	callsSinceLastSave int
	countNudgeSent     bool

	// Sprint 24.8: token-budget-based memory save nudge.
	// cumulativeOutputTokens accumulates estimated token count of all Synapses
	// tool responses in this session (responseBytes / 4 approximation).
	// budgetNudgeSent prevents the budget nudge from firing more than once
	// per save cycle.
	cumulativeOutputTokens int
	budgetNudgeSent        bool
}

// handleEndSession captures session knowledge and persists it as memories.
// This is the key tool for the "coordination and memory infrastructure" pivot:
// agents call this at session end, and institutional knowledge accumulates
// automatically without manual remember() calls.
func (s *Server) handleEndSession(
	ctx context.Context,
	req mcplib.CallToolRequest,
) (*mcplib.CallToolResult, error) {
	agentID, _ := req.GetArguments()["agent_id"].(string)
	if agentID == "" {
		return mcplib.NewToolResultError("agent_id is required (e.g., 'implementer', 'reviewer')"), nil
	}

	taskID, _ := req.GetArguments()["task_id"].(string)
	summary, _ := req.GetArguments()["summary"].(string)

	if s.store == nil {
		return mcplib.NewToolResultError("store not available: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}

	// RX1: clear call counter so that the auto-log doesn't fire again for this
	// agent after a manual end_session call. The existing dedup logic in
	// InsertMemory (stringSimilarity) handles the case where an auto-log was
	// already written — manual end_session just touches it rather than duplicating.
	// clearAndGetStartTime captures startedAt and deletes the entry atomically,
	// avoiding two separate lock acquisitions.
	mcpSessionID := SessionIDFromContext(ctx)
	sessionStartedAt, sessionStartSeq := s.clearAndGetStartTime(mcpSessionID, agentID)

	// Session Intelligence: close the Synapses session record.
	// Outcome is "unknown" by default; the agent may provide context via summary.
	outcome := "unknown"
	if summary != "" {
		outcome = "success" // agent provided a summary — treat as intentional close
	} else if taskID != "" {
		// D4: agent explicitly linked a task_id — treat as a success signal so
		// the adaptive edge weight refinement accumulates positive reinforcement
		// even for agents that don't write a free-form summary.
		outcome = "success"
	}
	synapseSessionID := s.getSynapseSessionID(mcpSessionID)
	// Sprint 27.3: clear per-session tool counts.
	// Sprint 25.6: clear goal reinforcer counter so resumed sessions start fresh.
	// Sprint 27.10: clear finding queue — undelivered findings are dropped on session end.
	if synapseSessionID != "" {
		s.toolTracker.clear(synapseSessionID)
		s.goalReinforcer.clear(synapseSessionID)
		s.findingQueue.Clear(synapseSessionID)
	}
	var retro *store.ToolCallSummary
	if s.store != nil && synapseSessionID != "" {
		_ = s.store.EndSession(synapseSessionID, "clean", outcome, summary)
		// Build retrospective from the tool call audit trail.
		if ts, err := s.store.GetToolCallSummary(synapseSessionID); err == nil && ts.TotalCalls > 0 {
			retro = &ts
		}
		// Sprint 27.5: Analyze co-access patterns from session work ledger.
		if bc := s.brainClient; bc != nil {
			sid := synapseSessionID
			s.goBackground(func() {
				analyzeCoAccess(s.store, bc, sid)
			})
		}

		// Sprint 15 #1: emit "task_abandoned" signals for entities that received
		// context but have no task completion outcome. Must run BEFORE
		// CorrelateSessionOutcome so we can identify task_outcome='' rows.
		// Outcome "unknown" means the agent ended without calling end_session
		// with a summary — a strong negative signal.
		if outcome == "unknown" {
			s.emitAbandonedContextSignals(synapseSessionID, agentID, s.projectID)
		}
		// Sprint 6.7: correlate all context deliveries for this session with the outcome.
		// Synchronous — must complete before session record is cleared.
		_, _ = s.store.CorrelateSessionOutcome(synapseSessionID, outcome)
		// Sprint 15 #3: apply BFS/PPR edge weight refinements based on this
		// session's outcome. Background: acquires graph.RLocks and writes SQLite.
		// Must run AFTER CorrelateSessionOutcome (uses the session_id index, not
		// task_outcome, so ordering is safe — but clearing session below would
		// not affect it since we pass synapseSessionID by value).
		sessIDForRefinement := synapseSessionID
		outcomeForRefinement := outcome
		s.goBackground(func() { s.applyEdgeWeightRefinements(sessIDForRefinement, outcomeForRefinement) })
		s.ClearSynapseSession(mcpSessionID)
		// Sprint 24: clean up Work Ledger watermark to prevent unbounded memory growth.
		s.clearLedgerWatermark(synapseSessionID)
	}

	// Pulse: record session end event using the Synapses session UUID.
	if pc := s.getPulseClient(); pc != nil {
		pc.RecordSessionEventWithID(synapseSessionID, agentID, s.projectID, "end")
	}

	// ── Phase 6: absorb report_usage ─────────────────────────────────────
	// If the agent provided model/token data, report it to pulse in one call.
	// This saves agents from needing a separate report_usage call at session end.
	if pc := s.getPulseClient(); pc != nil {
		usageModel, _ := req.GetArguments()["model"].(string)
		usageProvider, _ := req.GetArguments()["provider"].(string)
		inputTokens, _ := req.GetArguments()["input_tokens"].(float64)
		outputTokens, _ := req.GetArguments()["output_tokens"].(float64)
		costUSD, _ := req.GetArguments()["cost_usd"].(float64)
		if usageModel != "" {
			sessID := synapseSessionID // Use main store UUID instead of synthetic ID.
			if sessID == "" {
				sessID = agentID + ":" + s.projectID + ":" + time.Now().UTC().Format("2006-01-02")
			}
			evt := pulse.AgentLLMUsageEvent{
				SessionID:    sessID,
				AgentID:      agentID,
				ProjectID:    s.projectID,
				Model:        usageModel,
				Provider:     usageProvider,
				InputTokens:  int(inputTokens),
				OutputTokens: int(outputTokens),
				CostUSD:      costUSD,
			}
			s.goBackground(func() { pc.RecordAgentLLMUsage(evt) })
		}
	}

	result := endSessionResult{
		Status:  "ok",
		AgentID: agentID,
	}

	var memoriesSaved int

	// ── Step 1: Structured extraction from events ──
	sessSummary := s.extractSessionSummary(agentID, sessionStartedAt, sessionStartSeq)
	result.SessionSummary = sessSummary

	// ── Step 1b: Package-grouped work summary (RX4) ──
	// Group file changes by package directory. Stored as a separate session-log
	// memory so session_init can surface it as previous_session_work on the next
	// session — a continuity signal, not a task.
	pkgWork := buildPackageWorkSummary(sessSummary, s.graph)
	if len(pkgWork) > 0 && s.store != nil {
		env := workSummaryEnvelope{
			Packages:  pkgWork,
			SessionAt: time.Now().Unix(),
		}
		if pkgJSON, jsonErr := json.Marshal(env); jsonErr == nil {
			_, _ = s.store.InsertMemory(store.Memory{
				Tier:          store.TierSessionLog,
				Content:       string(pkgJSON),
				AgentID:       agentID,
				TaskID:        taskID,
				Source:        store.SourceAuto,
				Tags:          `["work_summary","session_end","auto"]`,
				SourceProject: s.projectID,
			})
			// memoriesSaved is intentionally not incremented — this is a
			// structured record, not a human-readable institutional memory.
		}
	}

	// ── Step 1c: Session handoff memory (Sprint 25.5) ──
	// Build a structured handoff payload: what was accomplished, what remains,
	// key decisions, active hypotheses. Stored as a session-log memory with the
	// "handoff" tag so session_init can retrieve it as primary context on the
	// next session. Tier 1 auto-capture: no agent action required.
	if handoffContent := s.buildHandoffPayload(agentID, summary, sessSummary); handoffContent != "" {
		_, _ = s.store.InsertMemory(store.Memory{
			Tier:          store.TierSessionLog,
			Content:       handoffContent,
			AgentID:       agentID,
			TaskID:        taskID,
			Source:        store.SourceAuto,
			Tags:          `["handoff","session_end","auto"]`,
			SourceProject: s.projectID,
		})
		// memoriesSaved is intentionally not incremented — this is a structured
		// metadata record, not a human-readable institutional memory.
	}

	// ── Step 2: Save session-log memory ──
	sessionContent := buildSessionLogContent(agentID, taskID, summary, sessSummary)
	if sessionContent != "" {
		_, err := s.store.InsertMemory(store.Memory{
			Tier:          store.TierSessionLog,
			Content:       sessionContent,
			AgentID:       agentID,
			TaskID:        taskID,
			Source:        store.SourceAuto,
			Tags:          `["session_end","auto"]`,
			SourceProject: s.projectID,
		})
		if err == nil {
			memoriesSaved++
		}
	}

	// ── Step 3: Extract entity memories ──
	// Only for entities examined AND whose files were modified (anti-noise).
	// FilesTouched is already the agent-attributed intersection, computed in
	// extractSessionSummary. Reuse it here instead of querying watcher again.
	if sessSummary != nil && s.graph != nil {
		for _, entityName := range sessSummary.EntitiesExamined {
			// Best-effort lookup: find the node by name.
			nodes := s.graph.FindByName(entityName)
			if len(nodes) == 0 {
				continue
			}
			node := nodes[0]
			if !containsFile(sessSummary.FilesTouched, node.File) {
				continue
			}

			content := fmt.Sprintf("Agent %s examined and modified %s", agentID, node.Name)
			if summary != "" {
				content += ": " + summary
			}

			_, err := s.store.InsertMemory(store.Memory{
				Tier:          store.TierEntity,
				Content:       content,
				EntityID:      string(node.ID),
				AgentID:       agentID,
				TaskID:        taskID,
				Source:        store.SourceAuto,
				Tags:          `["session_end","entity_change","auto"]`,
				SourceProject: s.projectID,
			})
			if err == nil {
				memoriesSaved++
			}
		}
	}

	// ── D1: Co-occurrence learning — LogDecision for each modified entity ──
	// Feeds the PatternHints pipeline so future context packets surface
	// "entities commonly edited together" suggestions.
	// Fire-and-forget: brain unavailable → no-op.
	// Caps at 20 entities to bound O(n²) related-entity allocation and per-task
	// worker time. Each LogDecision call uses a 3s timeout to avoid pinning a
	// background worker on a slow/hung brain server.
	if s.brainClient != nil && sessSummary != nil && len(sessSummary.EntitiesExamined) > 0 {
		const maxD1Entities = 20
		bc := s.brainClient
		src := sessSummary.EntitiesExamined
		if len(src) > maxD1Entities {
			src = src[:maxD1Entities]
		}
		entities := make([]string, len(src))
		copy(entities, src)
		sessOutcome := outcome
		sessAgentID := agentID
		sessTaskID := taskID
		s.goBackground(func() {
			for _, entityName := range entities {
				// related = all other entities examined this session
				related := make([]string, 0, len(entities)-1)
				for _, other := range entities {
					if other != entityName {
						related = append(related, other)
					}
				}
				dctx, dcancel := context.WithTimeout(ctx, 3*time.Second)
				bc.LogDecision(dctx, brain.DecisionRequest{
					AgentID:         sessAgentID,
					Phase:           "implementation",
					EntityName:      entityName,
					Action:          "edit",
					RelatedEntities: related,
					Outcome:         sessOutcome,
					Notes:           sessTaskID,
				})
				dcancel()
			}
		})
	}

	// ── Step 4: Save user-provided summary as project memory ──
	if summary != "" && len(summary) >= 10 {
		_, err := s.store.InsertMemory(store.Memory{
			Tier:          store.TierProject,
			Content:       summary,
			AgentID:       agentID,
			TaskID:        taskID,
			Source:        store.SourceManual,
			Tags:          `["session_end","summary"]`,
			SourceProject: s.projectID,
		})
		if err == nil {
			memoriesSaved++
		}
	}

	// ── D4: Archivist session memory synthesis ──
	// Fire-and-forget: calls the Archivist LLM to synthesize the session into
	// durable institutional memories. No-op if brain unavailable or no events.
	// archEvents is capped at 50 total to bound the LLM payload size — the
	// archivist gets the most-recent events (entities first, then files).
	// A 30s timeout prevents a slow brain from pinning a background worker.
	if s.brainClient != nil && sessSummary != nil {
		const maxArchEvents = 50
		bc := s.brainClient
		var archEvents []archivist.SessionEvent
		for _, entity := range sessSummary.EntitiesExamined {
			archEvents = append(archEvents, archivist.SessionEvent{
				Tool:   "get_context",
				Entity: entity,
			})
		}
		for _, f := range sessSummary.FilesTouched {
			archEvents = append(archEvents, archivist.SessionEvent{
				Tool:   "edit",
				Entity: f,
			})
		}
		if summary != "" {
			archEvents = append(archEvents, archivist.SessionEvent{
				Tool:   "end_session",
				Result: summary,
			})
		}
		if len(archEvents) > maxArchEvents {
			archEvents = archEvents[len(archEvents)-maxArchEvents:]
		}
		if len(archEvents) > 0 {
			archReq := archivist.MemorizeRequest{SessionEvents: archEvents}
			sessStore := s.store
			sessAgentID2 := agentID
			sessTaskID2 := taskID
			s.goBackground(func() {
				mctx, mcancel := context.WithTimeout(ctx, 30*time.Second)
				defer mcancel()
				resp, err := bc.Memorize(mctx, archReq)
				if err != nil || sessStore == nil {
					return
				}
				for _, m := range resp.NewMemories {
					if m.Content == "" {
						continue
					}
					_, _ = sessStore.InsertMemory(store.Memory{
						Tier:          store.TierProject,
						Content:       m.Content,
						AgentID:       sessAgentID2,
						TaskID:        sessTaskID2,
						Source:        store.SourceAuto,
						Tags:          `["archivist","session_synthesis","auto"]`,
						SourceProject: s.projectID,
					})
				}
			})
		}
	}

	// ── Step 5: Run memory expiry ──
	expired, _ := s.store.ExpireMemories()
	// P7-6: emit memory op for expired memories.
	if expired > 0 {
		if pc := s.getPulseClient(); pc != nil {
			pc.RecordMemoryOp(pulse.MemoryOperationEvent{
				Operation: "expire", Tier: "episodic",
				Count: int(expired), ProjectID: s.projectID, AgentID: agentID,
			})
		}
	}
	result.MemoriesExpired = expired
	result.MemoriesSaved = memoriesSaved
	result.Retrospective = retro

	// ── Step 6: Compute session duration from session start time ──
	// sessionStartedAt is captured atomically via clearAndGetStartTime — it records
	// when session_init first fired for this (sessionID, agentID) pair.
	// Using a.LastSeen was wrong: LastSeen is the last heartbeat, not session start.
	var durationMs int64
	if !sessionStartedAt.IsZero() {
		durationMs = time.Since(sessionStartedAt).Milliseconds()
		result.SessionDuration = time.Since(sessionStartedAt).Round(time.Second).String()
	}

	// P5 — Item 32: record session termination reason and duration.
	if pc := s.getPulseClient(); pc != nil && synapseSessionID != "" {
		reason := "clean"
		if summary == "" {
			reason = "no_summary"
		}
		s.goBackground(func() { pc.SetSessionTermination(synapseSessionID, reason) })
	}

	// Sprint 15 #5: compute and record session effectiveness report, then surface it in the response.
	if pc := s.getPulseClient(); pc != nil && synapseSessionID != "" {
		var toolCalls int
		var taskCompRate *float64 // nil = no measurement, not "0% success"
		if retro != nil {
			toolCalls = retro.TotalCalls
			// Use (1 - error_rate) as a proxy for task completion rate.
			// Only set when we have actual call data — avoids showing 0% when there's
			// simply no measurement (e.g. session ended without any tool calls recorded).
			v := 1.0 - retro.ErrorRate
			taskCompRate = &v
		}
		contextHitRate := pc.GetSessionContextHitRate(synapseSessionID)
		totalDel, firstFetch, tokensSaved := pc.GetSessionDeliveryStats(synapseSessionID)

		// Read the 7-day trend BEFORE queuing the insert so this session's data
		// cannot race into its own Prev7d comparison. goBackground submits to a
		// worker pool that may execute on an idle goroutine before this goroutine
		// continues — reading trend first eliminates that race entirely.
		var prev7d *prev7dSummary
		if trend := pc.GetRecentEffectivenessTrend(7, agentID); len(trend) > 0 {
			p := &prev7dSummary{}
			for _, d := range trend {
				p.Sessions += d.Sessions
				p.AvgContextHitRate += d.AvgContextHitRate * float64(d.Sessions)
				p.AvgTaskCompletion += d.AvgTaskCompletion * float64(d.Sessions)
				p.TotalTokensSaved += d.TotalTokensSaved
				p.TotalKnowledgeGrowth += d.TotalKnowledgeGrowth
				p.AvgRecallEffectiveness += d.AvgRecallEffectiveness * float64(d.Sessions)
			}
			if p.Sessions > 0 {
				p.AvgContextHitRate /= float64(p.Sessions)
				p.AvgTaskCompletion /= float64(p.Sessions)
				p.AvgRecallEffectiveness /= float64(p.Sessions)
				prev7d = p
			}
		}

		// Queue the insert after the trend read — order matters (see above).
		taskCompRateF64 := 0.0
		if taskCompRate != nil {
			taskCompRateF64 = *taskCompRate
		}
		recallRate, _, _ := s.getRecallEffectiveness(synapseSessionID)
		eff := pulse.SessionEffectiveness{
			SessionID:               synapseSessionID,
			AgentID:                 agentID,
			ProjectID:               s.projectID,
			ContextHitRate:          contextHitRate,
			TaskCompletionRate:      taskCompRateF64,
			TokensSaved:             tokensSaved,
			ToolCalls:               toolCalls,
			DurationMs:              durationMs,
			KnowledgeGrowth:         memoriesSaved,
			RecallEffectivenessRate: recallRate,
		}
		s.goBackground(func() { pc.InsertSessionEffectiveness(eff) })

		report := &EffectivenessReport{
			ContextHitRate:     contextHitRate,
			TaskCompletionRate: taskCompRate,
			ToolCalls:          toolCalls,
			TotalDeliveries:    totalDel,
			FirstFetchRight:    firstFetch,
			TokensSaved:        tokensSaved,
			DurationMs:         durationMs,
			KnowledgeGrowth:    memoriesSaved,
			Prev7d:             prev7d,
		}
		// Recall effectiveness: fraction of recalls where agent acted on recalled entities.
		if rate, total, _ := s.getRecallEffectiveness(synapseSessionID); total > 0 {
			report.RecallEffectivenessRate = &rate
		}
		s.clearRecallFootprints(synapseSessionID)
		report.Message = buildEffectivenessMessage(report)
		result.EffectivenessReport = report
	}

	return jsonResult(result)
}

// extractSessionSummary collects structured data about what happened during
// this agent's session from the event log.
// sessionStart is the time the session began; it widens the watcher look-back
// window to the full session duration. Pass zero time to use the 30-minute
// default (used by triggerAutoSessionLog which doesn't have a start time).
func (s *Server) extractSessionSummary(agentID string, sessionStart time.Time, sinceSeq int64) *sessionSummary {
	summary := &sessionSummary{}

	if s.store == nil {
		return summary
	}

	// Get events filtered by agent ID and starting from the session's
	// start sequence, avoiding fetching all-time history.
	events, _, err := s.store.GetEvents(sinceSeq, nil, agentID, 200)
	if err != nil {
		return summary
	}

	entitiesSet := make(map[string]bool)
	tasksSet := make(map[string]bool)

	for _, ev := range events {
		switch ev.Type {
		case "agent_examining":
			var payload struct {
				Entity string `json:"entity"`
			}
			if json.Unmarshal([]byte(ev.Payload), &payload) == nil && payload.Entity != "" {
				entitiesSet[payload.Entity] = true
			}
		case "task_updated":
			var payload struct {
				TaskID string `json:"task_id"`
			}
			if json.Unmarshal([]byte(ev.Payload), &payload) == nil && payload.TaskID != "" {
				tasksSet[payload.TaskID] = true
			}
		}
	}

	// Derive FilesTouched from entities this agent examined whose files were
	// recently modified. This gives proper agent attribution: file_change events
	// from the watcher are unattributed (any agent or external tool), so we
	// intersect agent-examined entities with the watcher's recent-change list.
	modifiedFiles := s.getRecentlyModifiedFiles(sessionStart)
	filesSet := make(map[string]bool)
	if s.graph != nil {
		for entityName := range entitiesSet {
			nodes := s.graph.FindByName(entityName)
			if len(nodes) > 0 && nodes[0].File != "" && containsFile(modifiedFiles, nodes[0].File) {
				filesSet[nodes[0].File] = true
			}
		}
	}

	for f := range filesSet {
		summary.FilesTouched = append(summary.FilesTouched, f)
	}
	sort.Strings(summary.FilesTouched)
	for e := range entitiesSet {
		summary.EntitiesExamined = append(summary.EntitiesExamined, e)
	}
	sort.Strings(summary.EntitiesExamined)
	for t := range tasksSet {
		summary.TasksUpdated = append(summary.TasksUpdated, t)
	}
	sort.Strings(summary.TasksUpdated)

	return summary
}

// buildSessionLogContent creates a concise structured log of the session.
func buildSessionLogContent(agentID, taskID, summary string, sess *sessionSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session by %s", agentID)
	if taskID != "" {
		fmt.Fprintf(&b, " (task: %s)", taskID)
	}
	fmt.Fprintf(&b, " at %s.", time.Now().UTC().Format("2006-01-02 15:04"))

	if sess != nil {
		if len(sess.FilesTouched) > 0 {
			fmt.Fprintf(&b, " Files: %s.", strings.Join(truncateSlice(sess.FilesTouched, 5), ", "))
		}
		if len(sess.EntitiesExamined) > 0 {
			fmt.Fprintf(&b, " Examined: %s.", strings.Join(truncateSlice(sess.EntitiesExamined, 5), ", "))
		}
		if len(sess.TasksUpdated) > 0 {
			fmt.Fprintf(&b, " Tasks: %s.", strings.Join(sess.TasksUpdated, ", "))
		}
	}

	if summary != "" {
		b.WriteString(" Summary: " + summary)
	}

	content := b.String()
	if len(content) < 10 {
		return ""
	}
	return content
}

// ── RX1: Auto end_session on context pressure ──────────────────────────────

// trackSessionCall increments the call counter for (sessionID, agentID) and
// fires triggerAutoSessionLog asynchronously when the configured threshold is
// exceeded. Safe to call from the AddAfterCallTool hook concurrently.
// nextToolPosition returns and increments the per-session tool sequence position (P5 — SA-C1).
// Uses the existing sessionCallsMu to avoid adding a new lock.
func (s *Server) nextToolPosition(mcpSessionID string) int {
	s.sessionCallsMu.Lock()
	pos := s.toolPositions[mcpSessionID]
	s.toolPositions[mcpSessionID] = pos + 1
	s.sessionCallsMu.Unlock()
	return pos
}

func (s *Server) trackSessionCall(sessionID, agentID string) {
	threshold := 0
	if s.config != nil {
		threshold = s.config.Session.AutoEndThresholdCalls
	}
	if threshold <= 0 {
		return // auto-end disabled
	}

	key := sessionID + "::" + agentID

	s.sessionCallsMu.Lock()
	entry, ok := s.sessionCalls[key]
	if !ok {
		// Capture the current latest event seq so extractSessionSummary
		// only processes events from this session, not all-time history.
		var startSeq int64
		if s.store != nil {
			if _, seq, err := s.store.GetEvents(0, nil, "", 0); err == nil {
				startSeq = seq
			}
		}
		entry = &sessionCallEntry{
			agentID:   agentID,
			startedAt: time.Now(),
			startSeq:  startSeq,
		}
		s.sessionCalls[key] = entry
	}
	entry.callCount++
	shouldFire := entry.callCount >= threshold && !entry.autoLogged
	if shouldFire {
		entry.autoLogged = true
	}
	s.sessionCallsMu.Unlock()

	if shouldFire {
		aid := agentID
		s.goBackground(func() { s.triggerAutoSessionLog(aid) })
	}
}

// triggerAutoSessionLog performs session memory extraction without an explicit
// end_session call. Captures entities examined, files touched, and tasks updated
// using the same extraction logic as handleEndSession.
// Tags the memory with "auto_session_log" to distinguish it from manual logs.
// Errors are silently discarded — this is best-effort, non-blocking.
func (s *Server) triggerAutoSessionLog(agentID string) {
	if s.store == nil || agentID == "" {
		return
	}

	sessSummary := s.extractSessionSummary(agentID, time.Time{}, 0) // zero = 30-min fallback, 0 = no seq filter
	content := buildSessionLogContent(agentID, "", "", sessSummary)
	if content == "" {
		return
	}

	// Tag as auto_session_log so callers can filter if needed.
	_, _ = s.store.InsertMemory(store.Memory{
		Tier:          store.TierSessionLog,
		Content:       content,
		AgentID:       agentID,
		Source:        store.SourceAuto,
		Tags:          `["auto_session_log","auto"]`,
		SourceProject: s.projectID,
	})

	// P5 — Item 32: record auto-end termination reason for any active Synapses session.
	if pc := s.getPulseClient(); pc != nil {
		// We don't have the MCP sessionID here, so iterate synapsesSessions looking
		// for one belonging to this agentID. Best-effort — fire-and-forget.
		s.synapseSessionsMu.RLock()
		for _, entry := range s.synapsesSessions {
			if entry != nil && entry.agentID == agentID && entry.id != "" {
				pc.SetSessionTermination(entry.id, "auto_threshold")
			}
		}
		s.synapseSessionsMu.RUnlock()
	}
}

// clearAndGetStartTime removes the call counter for (sessionID, agentID) and
// returns the session's startedAt time in a single atomic lock acquisition.
// Returns zero time if no entry exists.
func (s *Server) clearAndGetStartTime(sessionID, agentID string) (time.Time, int64) {
	if sessionID == "" && agentID == "" {
		return time.Time{}, 0
	}
	key := sessionID + "::" + agentID
	s.sessionCallsMu.Lock()
	defer s.sessionCallsMu.Unlock()
	var t time.Time
	var seq int64
	if entry, ok := s.sessionCalls[key]; ok {
		t = entry.startedAt
		seq = entry.startSeq
		delete(s.sessionCalls, key)
	}
	return t, seq
}

// ── end RX1 ────────────────────────────────────────────────────────────────

// ── Sprint 24.7 / 24.8: memory save nudge ──────────────────────────────────

// checkNudgeMessage accumulates response token estimates for the session and
// returns a non-empty nudge message when the agent should be reminded to save
// working state to memory.
//
// Strategy (in priority order):
//  1. Token-budget nudge (preferred): when the model is known, fire once when
//     cumulative Synapses output tokens reach TokenBudgetPct of the context
//     window. Switches nudge strategy from count-based to budget-based.
//  2. Count-based nudge (fallback): when the model is unknown, fire once when
//     the agent has made NudgeThreshold tool calls without saving to memory.
//
// Either nudge fires at most once per save-cycle. The cycle resets when the
// agent calls memory with a save-type action (see resetSaveCounter).
//
// Returns "" when no nudge should be emitted (threshold not reached, already
// fired, or both features disabled via config).
func (s *Server) checkNudgeMessage(sessionID, agentID, model string, responseTokens int) string {
	nudgeThreshold := 0
	budgetPct := 0.0
	if s.config != nil {
		nudgeThreshold = s.config.Session.NudgeThreshold
		budgetPct = s.config.Session.TokenBudgetPct
	}
	bothDisabled := nudgeThreshold <= 0 && budgetPct <= 0
	if bothDisabled || (sessionID == "" && agentID == "") {
		return ""
	}

	key := sessionID + "::" + agentID

	s.sessionCallsMu.Lock()
	entry, ok := s.sessionCalls[key]
	if !ok {
		// Entry may not exist when AutoEndThresholdCalls is 0 (auto-end disabled).
		// Create a minimal entry to track nudge state independently.
		var startSeq int64
		if s.store != nil {
			if _, seq, err := s.store.GetEvents(0, nil, "", 0); err == nil {
				startSeq = seq
			}
		}
		entry = &sessionCallEntry{
			agentID:   agentID,
			startedAt: time.Now(),
			startSeq:  startSeq,
		}
		s.sessionCalls[key] = entry
	}

	// Accumulate tokens regardless of nudge state (used for budget tracking).
	entry.cumulativeOutputTokens += responseTokens
	entry.callsSinceLastSave++

	// ── Prefer token-budget nudge when model is known and window is resolved ──
	if model != "" && budgetPct > 0 {
		window := modelContextWindow(model)
		if window > 0 {
			// Window known: use token budget exclusively.
			if !entry.budgetNudgeSent {
				used := float64(entry.cumulativeOutputTokens) / float64(window) * 100
				if used >= budgetPct {
					entry.budgetNudgeSent = true
					pct := int(used)
					s.sessionCallsMu.Unlock()
					return fmt.Sprintf(
						"Context budget ~%d%% consumed by Synapses tool responses this session. "+
							"Recommend calling memory(action=\"save\") to persist working state before context is lost.",
						pct,
					)
				}
			}
			// Threshold not yet crossed (or already fired): no count-based nudge.
			s.sessionCallsMu.Unlock()
			return ""
		}
		// Window unknown (unrecognised model): fall through to count-based nudge.
	}

	// ── Fallback: count-based nudge when model unknown ─────────────────────
	if nudgeThreshold > 0 && !entry.countNudgeSent && entry.callsSinceLastSave >= nudgeThreshold {
		entry.countNudgeSent = true
		calls := entry.callsSinceLastSave
		s.sessionCallsMu.Unlock()
		return fmt.Sprintf(
			"You've made %d tool calls this session without saving to memory. "+
				"Consider calling memory(action=\"save\") to protect findings against context loss.",
			calls,
		)
	}

	s.sessionCallsMu.Unlock()
	return ""
}

// resetSaveCounter resets the per-session nudge counters after a memory save
// action. This allows the nudge to fire again if the agent continues working
// without saving for another full cycle.
func (s *Server) resetSaveCounter(sessionID, agentID string) {
	if sessionID == "" && agentID == "" {
		return
	}
	key := sessionID + "::" + agentID
	s.sessionCallsMu.Lock()
	if entry, ok := s.sessionCalls[key]; ok {
		entry.callsSinceLastSave = 0
		entry.countNudgeSent = false
		entry.budgetNudgeSent = false
	}
	s.sessionCallsMu.Unlock()
}

// modelContextWindow returns the context window size in tokens for a known
// model name. Returns 0 for unrecognised models.
func modelContextWindow(model string) int {
	switch model {
	// Claude 4 family
	case "claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5", "claude-haiku-4-5-20251001":
		return 200000
	// Claude 3 family
	case "claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022", "claude-3-opus-20240229":
		return 200000
	// GPT-4 family
	case "gpt-4o", "gpt-4o-mini":
		return 128000
	case "gpt-4-turbo":
		return 128000
	case "gpt-4":
		return 8192
	case "o1", "o3-mini":
		return 200000
	// Gemini
	case "gemini-2.0-flash":
		return 1000000
	case "gemini-1.5-pro":
		return 2000000
	}
	return 0
}

// injectNudgeIntoResult appends a memory_nudge field to the first TextContent
// of result when nudgeMsg is non-empty. If Content[0] is valid JSON, the field
// is added to the object. Otherwise the message is silently dropped to avoid
// corrupting structured responses. Error results are never modified.
func injectNudgeIntoResult(result *mcplib.CallToolResult, nudgeMsg string) {
	if nudgeMsg == "" || result == nil || result.IsError || len(result.Content) == 0 {
		return
	}
	tc, ok := result.Content[0].(mcplib.TextContent)
	if !ok {
		return
	}
	// Only inject into JSON object responses to avoid breaking plain-text handlers.
	var obj map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &obj); err != nil {
		return
	}
	obj["memory_nudge"] = nudgeMsg
	b, err := json.Marshal(obj)
	if err != nil {
		return
	}
	result.Content[0] = mcplib.TextContent{Type: "text", Text: string(b)}
}

// ── end Sprint 24.7 / 24.8 ─────────────────────────────────────────────────

// getRecentlyModifiedFiles returns files changed since the session started (from
// watcher events). When sessionStart is zero the window defaults to 30 minutes.
// Passing the actual session start ensures long sessions (> 30 min) don't miss
// file changes made at the beginning of the session.
func (s *Server) getRecentlyModifiedFiles(sessionStart time.Time) []string {
	if s.changeSource == nil {
		return nil
	}
	windowMin := 30
	if !sessionStart.IsZero() {
		// Add a 5-minute buffer so changes made just before session_init
		// (e.g. editor startup writing config files) are included.
		elapsed := int(time.Since(sessionStart).Minutes()) + 5
		if elapsed > windowMin {
			windowMin = elapsed
		}
	}
	changes := s.changeSource.RecentChanges(windowMin)
	files := make([]string, 0, len(changes))
	for _, c := range changes {
		files = append(files, c.File)
	}
	return files
}

// containsFile checks if a file path is in the list (suffix match).
func containsFile(files []string, file string) bool {
	for _, f := range files {
		if f == file || strings.HasSuffix(f, "/"+file) || strings.HasSuffix(file, "/"+f) {
			return true
		}
	}
	return false
}

// truncateSlice returns at most n items from s, appending "..." if truncated.
func truncateSlice(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	result := make([]string, n+1)
	copy(result, s[:n])
	result[n] = fmt.Sprintf("(+%d more)", len(s)-n)
	return result
}

// buildEffectivenessMessage returns a human-readable one-liner summarising the session.
// Examples:
//
//	"First-fetch context: 14/16 deliveries required no correction (87%). Context hit rate: 85%. 47 tool calls in 4m0s."
//	"No context deliveries this session. 12 tool calls in 30s."
func buildEffectivenessMessage(r *EffectivenessReport) string {
	dur := time.Duration(r.DurationMs) * time.Millisecond
	durStr := dur.Round(time.Second).String()
	if r.TotalDeliveries == 0 {
		return fmt.Sprintf("No context deliveries this session. %d tool calls in %s.", r.ToolCalls, durStr)
	}
	pct := 100.0 * float64(r.FirstFetchRight) / float64(r.TotalDeliveries)
	msg := fmt.Sprintf(
		"First-fetch context: %d/%d deliveries required no correction (%.0f%%). Context hit rate: %.0f%%. %d tool calls in %s.",
		r.FirstFetchRight, r.TotalDeliveries, pct,
		r.ContextHitRate*100, r.ToolCalls, durStr,
	)
	if r.TokensSaved > 0 {
		msg += fmt.Sprintf(" ~%d tokens saved.", r.TokensSaved)
	}
	if r.KnowledgeGrowth > 0 {
		msg += fmt.Sprintf(" %d memories created.", r.KnowledgeGrowth)
	}
	return msg
}

// buildSessionTrend converts a slice of DailyEffectiveness rows (oldest-first,
// from GetRecentEffectivenessTrend) into a summary map suitable for embedding
// in session_init responses. windowDays is the query window (e.g. 7 for 7-day
// lookback) and is included in the response and note for clarity.
// Returns nil when there are fewer than 2 total sessions — insufficient data.
//
// Trend direction requires at least 2 distinct calendar days to compare halves:
//
//	second_half_avg - first_half_avg > +0.05 → "improving"
//	first_half_avg  - second_half_avg > 0.05 → "declining"
//	otherwise (or only 1 active day)         → "stable"
func buildSessionTrend(days []pulse.DailyEffectiveness, windowDays int) map[string]interface{} {
	if len(days) == 0 {
		return nil
	}
	// Aggregate totals across all active days.
	var totalSessions int
	var weightedHitRate float64
	var totalTokensSaved int
	var totalKnowledgeGrowth int
	for _, d := range days {
		totalSessions += d.Sessions
		weightedHitRate += d.AvgContextHitRate * float64(d.Sessions)
		totalTokensSaved += d.TotalTokensSaved
		totalKnowledgeGrowth += d.TotalKnowledgeGrowth
	}
	if totalSessions < 2 {
		return nil
	}
	avgHitRate := weightedHitRate / float64(totalSessions)
	activeDays := len(days)

	// Compute trend direction when there are ≥2 distinct calendar days.
	// With a single active day (all sessions on the same date), we cannot
	// determine direction — "stable" is the neutral default, noted as such.
	trend := "stable"
	canCompareTrend := activeDays >= 2
	if canCompareTrend {
		mid := activeDays / 2
		var firstHalfHit, firstHalfSess float64
		var secondHalfHit, secondHalfSess float64
		for _, d := range days[:mid] {
			firstHalfHit += d.AvgContextHitRate * float64(d.Sessions)
			firstHalfSess += float64(d.Sessions)
		}
		for _, d := range days[mid:] {
			secondHalfHit += d.AvgContextHitRate * float64(d.Sessions)
			secondHalfSess += float64(d.Sessions)
		}
		if firstHalfSess > 0 && secondHalfSess > 0 {
			firstAvg := firstHalfHit / firstHalfSess
			secondAvg := secondHalfHit / secondHalfSess
			switch {
			case secondAvg-firstAvg > 0.05:
				trend = "improving"
			case firstAvg-secondAvg > 0.05:
				trend = "declining"
			}
		}
	}

	// Human-readable note. Uses the query window (windowDays) not activeDays so
	// agents understand the full analysis period, not just the days with data.
	hitPct := int(avgHitRate * 100)
	dayWord := "days"
	if windowDays == 1 {
		dayWord = "day"
	}
	var note string
	if !canCompareTrend {
		// Only 1 active day — cannot assess trend direction.
		note = fmt.Sprintf("Context quality: hit rate ~%d%% (%d sessions today).", hitPct, totalSessions)
	} else {
		switch trend {
		case "improving":
			note = fmt.Sprintf("Context quality improving over %d %s: hit rate ~%d%%, %d sessions.",
				windowDays, dayWord, hitPct, totalSessions)
		case "declining":
			note = fmt.Sprintf("Context quality declining over %d %s: hit rate ~%d%%, %d sessions. Consider increasing depth on first call.",
				windowDays, dayWord, hitPct, totalSessions)
		default:
			note = fmt.Sprintf("Context quality stable over %d %s: hit rate ~%d%%, %d sessions.",
				windowDays, dayWord, hitPct, totalSessions)
		}
	}
	if totalTokensSaved > 0 {
		note += fmt.Sprintf(" %d tokens saved.", totalTokensSaved)
	}
	if totalKnowledgeGrowth > 0 {
		note += fmt.Sprintf(" %d memories created.", totalKnowledgeGrowth)
	}

	out := map[string]interface{}{
		"window_days":            windowDays,
		"active_days":            activeDays,
		"sessions":               totalSessions,
		"avg_context_hit_rate":   avgHitRate,
		"total_tokens_saved":     totalTokensSaved,
		"total_knowledge_growth": totalKnowledgeGrowth,
		"trend":                  trend,
		"note":                   note,
	}
	return out
}

// parseExaminedEntities extracts entity names from a session log content string.
// Session logs from buildSessionLogContent embed examined entities as:
//
//	"Examined: FuncA, Store.Close, Graph.New."
//
// Entity names can contain dots (e.g. "Store.Close"), so we stop at a
// sentence-boundary period: a period followed by a space or end-of-string.
// Returns the slice of trimmed entity names, or nil if no "Examined:" section is found.
// Used by R14C (stale context hints) to build the active entity register on session resume.
func parseExaminedEntities(content string) []string {
	const marker = "Examined: "
	idx := strings.Index(content, marker)
	if idx < 0 {
		return nil
	}
	rest := content[idx+len(marker):]
	// Find the sentence-ending period (period at EOL or followed by a space).
	// This correctly handles entity names that contain dots (e.g. "Store.Close").
	for i := 0; i < len(rest); i++ {
		if rest[i] == '.' && (i+1 == len(rest) || rest[i+1] == ' ') {
			rest = rest[:i]
			break
		}
	}
	var names []string
	for _, part := range strings.Split(rest, ",") {
		if name := strings.TrimSpace(part); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// Ensure graph.NodeID is used (imported for FindByName return type).
var _ graph.NodeID

// ── Sprint 25.5: Session Handoff Protocol ────────────────────────────────────

// handoffPayload is the structured reasoning-state object stored at end_session
// and retrieved by the next session's session_init. It captures the agent's
// working context — what was done, what remains, decisions made, active theories —
// so the next session starts with full situational awareness instead of zero.
//
// All fields are natural language strings (no source code). Compliant with the
// Communication Protocol (gives intelligence, never gives code).
type handoffPayload struct {
	AgentSummary     string   `json:"agent_summary,omitempty"`    // agent-provided free-text summary
	Accomplished     []string `json:"accomplished,omitempty"`     // tasks completed this session
	Remaining        []string `json:"remaining,omitempty"`        // pending/in_progress tasks at handoff
	KeyDecisions     []string `json:"key_decisions,omitempty"`    // recent decisions (choice field, NL)
	OpenHypotheses   []string `json:"open_hypotheses,omitempty"`  // active hypotheses (content field)
	ExploredEntities []string `json:"explored_entities,omitempty"` // key entities examined this session
	SessionAt        int64    `json:"session_at"`                 // Unix seconds — uniqueness nonce
}

// buildHandoffPayload assembles a structured handoff payload for the just-ending
// session and returns it as a JSON string. Returns "" when there is nothing
// meaningful to persist (no work done, no summary, no decisions, no hypotheses).
//
// Data sources (all fast SQLite reads — no network calls):
//   - Accomplished: tasks in sessSummary.TasksUpdated whose current status is "completed"
//   - Remaining:    pending/in_progress tasks via GetPendingTasks for this agent
//   - KeyDecisions: 3 most recent decisions via GetRecentDecisions
//   - OpenHypotheses: active hypotheses via GetHypotheses(state="active")
//   - ExploredEntities: sessSummary.EntitiesExamined, capped at 5
func (s *Server) buildHandoffPayload(agentID, summary string, sessSummary *sessionSummary) string {
	if s.store == nil {
		return ""
	}

	// Cap agent_summary to 500 chars — consistent with the exploration log's
	// 300-char FindingSummary cap. Prevents unbounded payload growth when agents
	// pass very long summaries.
	agentSummary := summary
	if len([]rune(agentSummary)) > 500 {
		agentSummary = string([]rune(agentSummary)[:500]) + "…"
	}

	payload := handoffPayload{
		SessionAt:    time.Now().Unix(),
		AgentSummary: agentSummary,
	}

	// Accomplished: resolve tasks updated this session and keep completed ones.
	// Capped at 5 to bound payload size — mirrors the Remaining cap.
	if sessSummary != nil {
		for _, tid := range sessSummary.TasksUpdated {
			if len(payload.Accomplished) >= 5 {
				break
			}
			t, err := s.store.GetTask(tid)
			if err != nil || t == nil {
				continue
			}
			if t.Status == "completed" {
				title := strings.TrimSpace(t.Title)
				if nl := strings.IndexByte(title, '\n'); nl >= 0 {
					title = strings.TrimRight(title[:nl], "\r")
				}
				if len([]rune(title)) > 80 {
					title = string([]rune(title)[:80]) + "…"
				}
				if title != "" {
					payload.Accomplished = append(payload.Accomplished, title)
				}
			}
		}
	}

	// Remaining: pending and in_progress tasks for this agent across all plans.
	if pending, err := s.store.GetPendingTasks("", agentID); err == nil {
		for _, t := range pending {
			if t.Status != "pending" && t.Status != "in_progress" {
				continue
			}
			title := strings.TrimSpace(t.Title)
			if nl := strings.IndexByte(title, '\n'); nl >= 0 {
				title = strings.TrimRight(title[:nl], "\r")
			}
			if len([]rune(title)) > 80 {
				title = string([]rune(title)[:80]) + "…"
			}
			if title != "" {
				payload.Remaining = append(payload.Remaining, title)
			}
			if len(payload.Remaining) >= 5 {
				break
			}
		}
	}

	// Key decisions: 3 most recent decisions for this agent+project.
	if decisions, err := s.store.GetRecentDecisions(agentID, s.projectID, 3); err == nil {
		for _, d := range decisions {
			choice := strings.TrimSpace(d.Choice)
			if choice != "" {
				// Optionally append context to give anchoring without code.
				if d.Context != "" {
					choice = choice + " (context: " + strings.TrimSpace(d.Context) + ")"
				}
				// Cap individual decision strings to prevent unbounded payload growth.
				if len([]rune(choice)) > 200 {
					choice = string([]rune(choice)[:200]) + "…"
				}
				payload.KeyDecisions = append(payload.KeyDecisions, choice)
			}
		}
	}

	// Open hypotheses: active working theories for this agent+project.
	if hyps, err := s.store.GetHypotheses(agentID, s.projectID, "active", 3); err == nil {
		for _, h := range hyps {
			content := strings.TrimSpace(h.Content)
			if content != "" {
				if len([]rune(content)) > 200 {
					content = string([]rune(content)[:200]) + "…"
				}
				payload.OpenHypotheses = append(payload.OpenHypotheses, content)
			}
		}
	}

	// Explored entities: key entities examined this session, capped at 5.
	if sessSummary != nil {
		capLeft := 5
		for _, e := range sessSummary.EntitiesExamined {
			if e != "" {
				payload.ExploredEntities = append(payload.ExploredEntities, e)
				capLeft--
				if capLeft <= 0 {
					break
				}
			}
		}
	}

	// Skip persisting if there is nothing meaningful to hand off.
	if payload.AgentSummary == "" &&
		len(payload.Accomplished) == 0 &&
		len(payload.Remaining) == 0 &&
		len(payload.KeyDecisions) == 0 &&
		len(payload.OpenHypotheses) == 0 &&
		len(payload.ExploredEntities) == 0 {
		return ""
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(data)
}

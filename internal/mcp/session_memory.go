package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/store"
)

// endSessionResult is the response from end_session.
type endSessionResult struct {
	Status          string                  `json:"status"`
	AgentID         string                  `json:"agent_id"`
	SessionDuration string                  `json:"session_duration,omitempty"`
	MemoriesSaved   int                     `json:"memories_saved"`
	SessionSummary  *sessionSummary         `json:"session_summary,omitempty"`
	MemoriesExpired int64                   `json:"memories_expired"`
	Retrospective   *store.ToolCallSummary  `json:"retrospective,omitempty"`
	ClaimsReleased  bool                    `json:"claims_released"`
}

// sessionSummary captures the structured extraction from a session.
type sessionSummary struct {
	FilesTouched     []string `json:"files_touched,omitempty"`
	EntitiesExamined []string `json:"entities_examined,omitempty"`
	TasksUpdated     []string `json:"tasks_updated,omitempty"`
}

// sessionCallEntry tracks per-connection tool call depth for RX1 auto-end detection.
// One entry exists per (sessionID + agentID) pair while the session is active.
type sessionCallEntry struct {
	agentID   string
	callCount int
	startedAt time.Time
	// autoLogged is set after the first auto-log fires, preventing re-trigger until
	// the count resets (manual end_session or reconnect).
	autoLogged bool
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
		return mcplib.NewToolResultError("agent_id is required"), nil
	}

	taskID, _ := req.GetArguments()["task_id"].(string)
	summary, _ := req.GetArguments()["summary"].(string)

	if s.store == nil {
		return mcplib.NewToolResultError("store not available"), nil
	}

	// RX1: clear call counter so that the auto-log doesn't fire again for this
	// agent after a manual end_session call. The existing dedup logic in
	// InsertMemory (stringSimilarity) handles the case where an auto-log was
	// already written — manual end_session just touches it rather than duplicating.
	// clearAndGetStartTime captures startedAt and deletes the entry atomically,
	// avoiding two separate lock acquisitions.
	mcpSessionID := SessionIDFromContext(ctx)
	sessionStartedAt := s.clearAndGetStartTime(mcpSessionID, agentID)

	// Session Intelligence: close the Synapses session record.
	// Outcome is "unknown" by default; the agent may provide context via summary.
	outcome := "unknown"
	if summary != "" {
		outcome = "success" // agent provided a summary — treat as intentional close
	}
	synapseSessionID := s.getSynapseSessionID(mcpSessionID)
	var retro *store.ToolCallSummary
	if s.store != nil && synapseSessionID != "" {
		_ = s.store.EndSession(synapseSessionID, "clean", outcome, summary)
		// Build retrospective from the tool call audit trail.
		if ts, err := s.store.GetToolCallSummary(synapseSessionID); err == nil && ts.TotalCalls > 0 {
			retro = &ts
		}
		s.ClearSynapseSession(mcpSessionID)
	}

	// ── Phase 6: absorb release_claims ───────────────────────────────────
	// Automatically release all work claims for this agent at session end.
	// This saves agents from needing a separate release_claims call.
	var claimsReleased bool
	if err := s.store.ReleaseClaims(agentID); err == nil {
		claimsReleased = true
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
			sessionID := agentID + ":" + s.projectID + ":" + time.Now().UTC().Format("2006-01-02")
			go pc.RecordAgentLLMUsage(pulse.AgentLLMUsageEvent{
				SessionID:    sessionID,
				AgentID:      agentID,
				ProjectID:    s.projectID,
				Model:        usageModel,
				Provider:     usageProvider,
				InputTokens:  int(inputTokens),
				OutputTokens: int(outputTokens),
				CostUSD:      costUSD,
			})
		}
	}

	result := endSessionResult{
		Status:         "ok",
		AgentID:        agentID,
		ClaimsReleased: claimsReleased,
	}

	var memoriesSaved int

	// ── Step 1: Structured extraction from events ──
	sessSummary := s.extractSessionSummary(agentID)
	result.SessionSummary = sessSummary

	// ── Step 2: Save session-log memory ──
	sessionContent := buildSessionLogContent(agentID, taskID, summary, sessSummary)
	if sessionContent != "" {
		_, err := s.store.InsertMemory(store.Memory{
			Tier:    store.TierSessionLog,
			Content: sessionContent,
			AgentID: agentID,
			TaskID:  taskID,
			Source:  store.SourceAuto,
			Tags:    `["session_end","auto"]`,
		})
		if err == nil {
			memoriesSaved++
		}
	}

	// ── Step 3: Extract entity memories ──
	// Only for entities examined AND whose files were modified (anti-noise).
	// FilesTouched is already the agent-attributed intersection, computed in
	// extractSessionSummary. Reuse it here instead of querying watcher again.
	if sessSummary != nil {
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
				Tier:     store.TierEntity,
				Content:  content,
				EntityID: string(node.ID),
				AgentID:  agentID,
				TaskID:   taskID,
				Source:   store.SourceAuto,
				Tags:     `["session_end","entity_change","auto"]`,
			})
			if err == nil {
				memoriesSaved++
			}
		}
	}

	// ── Step 4: Save user-provided summary as project memory ──
	if summary != "" && len(summary) >= 10 {
		_, err := s.store.InsertMemory(store.Memory{
			Tier:    store.TierProject,
			Content: summary,
			AgentID: agentID,
			TaskID:  taskID,
			Source:  store.SourceManual,
			Tags:    `["session_end","summary"]`,
		})
		if err == nil {
			memoriesSaved++
		}
	}

	// ── Step 5: Run memory expiry ──
	expired, _ := s.store.ExpireMemories()
	result.MemoriesExpired = expired
	result.MemoriesSaved = memoriesSaved
	result.Retrospective = retro

	// ── Step 6: Compute session duration from session start time ──
	// sessionStartedAt is captured atomically via clearAndGetStartTime — it records
	// when session_init first fired for this (sessionID, agentID) pair.
	// Using a.LastSeen was wrong: LastSeen is the last heartbeat, not session start.
	if !sessionStartedAt.IsZero() {
		result.SessionDuration = time.Since(sessionStartedAt).Round(time.Second).String()
	}

	return jsonResult(result)
}

// extractSessionSummary collects structured data about what happened during
// this agent's session from the event log.
func (s *Server) extractSessionSummary(agentID string) *sessionSummary {
	summary := &sessionSummary{}

	if s.store == nil {
		return summary
	}

	// Get recent events (all types, last 200).
	events, _, err := s.store.GetEvents(0, nil, "", 200)
	if err != nil {
		return summary
	}

	entitiesSet := make(map[string]bool)
	tasksSet := make(map[string]bool)

	for _, ev := range events {
		// Only process events attributed to this agent.
		if ev.AgentID != agentID {
			continue
		}
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
	modifiedFiles := s.getRecentlyModifiedFiles()
	filesSet := make(map[string]bool)
	for entityName := range entitiesSet {
		nodes := s.graph.FindByName(entityName)
		if len(nodes) > 0 && nodes[0].File != "" && containsFile(modifiedFiles, nodes[0].File) {
			filesSet[nodes[0].File] = true
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
	b.WriteString(fmt.Sprintf("Session by %s", agentID))
	if taskID != "" {
		b.WriteString(fmt.Sprintf(" (task: %s)", taskID))
	}
	b.WriteString(fmt.Sprintf(" at %s.", time.Now().UTC().Format("2006-01-02 15:04")))

	if sess != nil {
		if len(sess.FilesTouched) > 0 {
			b.WriteString(fmt.Sprintf(" Files: %s.", strings.Join(truncateSlice(sess.FilesTouched, 5), ", ")))
		}
		if len(sess.EntitiesExamined) > 0 {
			b.WriteString(fmt.Sprintf(" Examined: %s.", strings.Join(truncateSlice(sess.EntitiesExamined, 5), ", ")))
		}
		if len(sess.TasksUpdated) > 0 {
			b.WriteString(fmt.Sprintf(" Tasks: %s.", strings.Join(sess.TasksUpdated, ", ")))
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
		entry = &sessionCallEntry{
			agentID:   agentID,
			startedAt: time.Now(),
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
		go s.triggerAutoSessionLog(agentID)
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

	sessSummary := s.extractSessionSummary(agentID)
	content := buildSessionLogContent(agentID, "", "", sessSummary)
	if content == "" {
		return
	}

	// Tag as auto_session_log so callers can filter if needed.
	_, _ = s.store.InsertMemory(store.Memory{
		Tier:    store.TierSessionLog,
		Content: content,
		AgentID: agentID,
		Source:  store.SourceAuto,
		Tags:    `["auto_session_log","auto"]`,
	})
}

// clearAndGetStartTime removes the call counter for (sessionID, agentID) and
// returns the session's startedAt time in a single atomic lock acquisition.
// Returns zero time if no entry exists.
func (s *Server) clearAndGetStartTime(sessionID, agentID string) time.Time {
	if sessionID == "" && agentID == "" {
		return time.Time{}
	}
	key := sessionID + "::" + agentID
	s.sessionCallsMu.Lock()
	defer s.sessionCallsMu.Unlock()
	var t time.Time
	if entry, ok := s.sessionCalls[key]; ok {
		t = entry.startedAt
		delete(s.sessionCalls, key)
	}
	return t
}

// ── end RX1 ────────────────────────────────────────────────────────────────

// getRecentlyModifiedFiles returns files changed recently (from watcher events).
func (s *Server) getRecentlyModifiedFiles() []string {
	if s.changeSource == nil {
		return nil
	}
	changes := s.changeSource.RecentChanges(30) // last 30 minutes
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

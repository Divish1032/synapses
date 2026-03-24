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

// sessionCallEntry tracks per-connection tool call depth for RX1 auto-end detection.
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
	}
	synapseSessionID := s.getSynapseSessionID(mcpSessionID)
	var retro *store.ToolCallSummary
	if s.store != nil && synapseSessionID != "" {
		_ = s.store.EndSession(synapseSessionID, "clean", outcome, summary)
		// Build retrospective from the tool call audit trail.
		if ts, err := s.store.GetToolCallSummary(synapseSessionID); err == nil && ts.TotalCalls > 0 {
			retro = &ts
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
		Status:         "ok",
		AgentID:        agentID,
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
				Tier:    store.TierSessionLog,
				Content: string(pkgJSON),
				AgentID: agentID,
				TaskID:  taskID,
				Source:  store.SourceAuto,
				Tags:    `["work_summary","session_end","auto"]`,
			})
			// memoriesSaved is intentionally not incremented — this is a
			// structured record, not a human-readable institutional memory.
		}
	}

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

	// P5 — Item 13: compute and record session effectiveness report.
	if pc := s.getPulseClient(); pc != nil && synapseSessionID != "" {
		var toolCalls int
		var taskCompRate float64
		if retro != nil {
			toolCalls = retro.TotalCalls
			// Use (1 - error_rate) as a proxy for task completion rate.
			taskCompRate = 1.0 - retro.ErrorRate
		}
		contextHitRate := pc.GetSessionContextHitRate(synapseSessionID)
		eff := pulse.SessionEffectiveness{
			SessionID:          synapseSessionID,
			AgentID:            agentID,
			ProjectID:          s.projectID,
			ContextHitRate:     contextHitRate,
			TaskCompletionRate: taskCompRate,
			ToolCalls:          toolCalls,
			DurationMs:         durationMs,
		}
		s.goBackground(func() { pc.InsertSessionEffectiveness(eff) })
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
		Tier:    store.TierSessionLog,
		Content: content,
		AgentID: agentID,
		Source:  store.SourceAuto,
		Tags:    `["auto_session_log","auto"]`,
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

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/git"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/pulse"
	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
	"github.com/SynapsesOS/synapses/internal/store"
)

// buildNameIndex creates a name→nodeID index for all semantic nodes in the graph.
// Used by autoLinkNodes and linkNodesWithIndex. Callers that link multiple texts
// should call this once and reuse the index.
func (s *Server) buildNameIndex() map[string]string {
	if s.graph == nil {
		return nil
	}
	skip := map[graph.NodeType]bool{
		graph.NodeFile:    true,
		graph.NodePackage: true,
	}

	nameIndex := make(map[string]string) // name → first nodeID
	for _, n := range s.graph.AllNodes() {
		if skip[n.Type] || len(n.Name) < 3 {
			continue
		}
		id := string(n.ID)
		if _, exists := nameIndex[n.Name]; !exists {
			nameIndex[n.Name] = id
		}
		// Also index the bare method name (e.g. "AddEdge" for "Graph.AddEdge").
		if idx := strings.LastIndex(n.Name, "."); idx >= 0 {
			bare := n.Name[idx+1:]
			if len(bare) >= 3 {
				if _, exists := nameIndex[bare]; !exists {
					nameIndex[bare] = id
				}
			}
		}
	}
	return nameIndex
}

// linkNodesWithIndex scans text for node names using a pre-built name index
// and returns their IDs. Results are capped at 10.
func linkNodesWithIndex(text string, nameIndex map[string]string) []string {
	if text == "" || len(nameIndex) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	var result []string
	for _, word := range strings.FieldsFunc(text, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '.'
	}) {
		if len(word) < 3 {
			continue
		}
		if id, ok := nameIndex[word]; ok {
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				result = append(result, id)
				if len(result) >= 10 {
					return result
				}
			}
		}
	}
	return result
}

// autoLinkNodes scans text for node names that exist in the graph and returns
// their IDs. Only semantic node types (function, method, struct, interface) are
// considered — files and packages produce too many false positives. Names
// shorter than 3 characters are skipped. Results are capped at 10.
//
// For linking multiple texts, use buildNameIndex() + linkNodesWithIndex() to
// avoid rebuilding the index on each call.
func (s *Server) autoLinkNodes(text string) []string {
	return linkNodesWithIndex(text, s.buildNameIndex())
}

// mergeNodeIDs merges two slices of node ID strings, deduplicating the result.
func mergeNodeIDs(existing, detected []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(detected))
	out := make([]string, 0, len(existing)+len(detected))
	for _, id := range append(existing, detected...) {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// upsertAgentIfNeeded registers an agent side-effect when agent_id is present.
func (s *Server) upsertAgentIfNeeded(agentID string) {
	if s.store != nil && agentID != "" {
		_ = s.store.UpsertAgent(agentID, nil) // non-fatal; best-effort
	}
}

// upsertAgentWithActivity updates the agent registry with current activity info.
// Only non-empty fields in activity replace existing values.
func (s *Server) upsertAgentWithActivity(agentID string, activity *store.AgentActivity) {
	if s.store != nil && agentID != "" {
		_ = s.store.UpsertAgent(agentID, activity) // non-fatal; best-effort
	}
}

// handleCreatePlan persists a new plan and its tasks to the store so future
// LLM sessions can resume the agreed work via get_pending_tasks.
func (s *Server) handleCreatePlan(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}

	title, _ := req.GetArguments()["title"].(string)
	if title == "" {
		return mcp.NewToolResultError("title is required (e.g., 'Implement OAuth flow', 'Fix auth token bug')"), nil
	}
	description := stringArg(req, "description")
	agentID, _ := req.GetArguments()["agent_id"].(string)

	var taskInputs []store.TaskInput
	switch tv := req.GetArguments()["tasks"].(type) {
	case string:
		// LLM sent tasks as a JSON-encoded string (legacy path).
		if tv == "" {
			return mcp.NewToolResultError("tasks is required (JSON array of task objects)"), nil
		}
		if err := json.Unmarshal([]byte(tv), &taskInputs); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid tasks JSON: %v", stripInternalPaths(err.Error()))), nil
		}
	case []interface{}:
		// LLM sent tasks as a native JSON array (normal MCP path).
		b, _ := json.Marshal(tv)
		if err := json.Unmarshal(b, &taskInputs); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid tasks array: %v", stripInternalPaths(err.Error()))), nil
		}
	default:
		return mcp.NewToolResultError("tasks is required (JSON array of task objects)"), nil
	}
	if len(taskInputs) == 0 {
		return mcp.NewToolResultError("tasks array must not be empty"), nil
	}

	s.upsertAgentIfNeeded(agentID)

	// Auto-detect code nodes mentioned in each task's text and merge with any
	// explicitly provided linked_nodes — bridges "work to be done" with "code
	// to be changed" without requiring the caller to know node IDs upfront.
	// Build the name index once and reuse across all tasks to avoid O(N×tasks).
	nameIdx := s.buildNameIndex()
	for i := range taskInputs {
		detected := linkNodesWithIndex(taskInputs[i].Title+" "+taskInputs[i].Description, nameIdx)
		taskInputs[i].LinkedNodes = mergeNodeIDs(taskInputs[i].LinkedNodes, detected)
	}

	// R29: detect mid-session replan — emit only when the agent has an
	// in_progress task that was recently started (within 2 hours). Stale
	// in_progress tasks from previous sessions that were never marked done
	// are excluded; they represent abandoned work, not an active replan.
	if pc := s.getPulseClient(); pc != nil && agentID != "" {
		const replanWindow = 2 * time.Hour
		if existing, err := s.store.GetPendingTasks("", agentID); err == nil {
			for _, t := range existing {
				if t.Status != "in_progress" {
					continue
				}
				updatedAt, parseErr := time.Parse(time.RFC3339, t.UpdatedAt)
				if parseErr != nil || time.Since(updatedAt) > replanWindow {
					continue // stale — not an active session replan
				}
				evt := pulse.OutcomeSignalEvent{
					ProjectID:  s.projectID,
					AgentID:    agentID,
					SignalType: "replan",
				}
				s.goBackground(func() { pc.RecordOutcomeSignal(evt) })
				break
			}
		}
	}

	planID, taskIDs, err := s.store.CreatePlan(title, description, agentID, taskInputs)
	if err != nil {
		return toolError("create plan", err)
	}

	// Session Intelligence: link each created task to the current session.
	// Fire-and-forget: silently skipped when no active session (e.g. tests).
	if mcpSessionID := SessionIDFromContext(ctx); s.store != nil {
		if synapseSessionID := s.getSynapseSessionID(mcpSessionID); synapseSessionID != "" {
			for _, taskID := range taskIDs {
				s.store.LinkSessionTask(synapseSessionID, taskID, store.SessionTaskCreated)
			}
		}
	}

	return jsonResult(map[string]interface{}{
		"plan_id":    planID,
		"title":      title,
		"task_count": len(taskInputs),
		"message":    "Plan saved. Call get_pending_tasks() at the start of future sessions to resume.",
	})
}

// handleGetPendingTasks returns all pending and in-progress tasks, ordered by
// priority. This is the key session-resume tool: call it at the start of every
// session to discover what was agreed in previous sessions.
// For in_progress tasks, the session_state is included inline so the next LLM
// can resume from exactly where the previous session stopped.
func (s *Server) handleGetPendingTasks(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}

	planID, _ := req.GetArguments()["plan_id"].(string)
	agentID, _ := req.GetArguments()["agent_id"].(string)

	s.upsertAgentIfNeeded(agentID)

	tasks, err := s.store.GetPendingTasks(planID, agentID)
	if err != nil {
		return toolError("get pending tasks", err)
	}

	// Collect IDs of in_progress tasks to fetch their session state.
	type taskWithState struct {
		store.Task
		SessionState *store.SessionState `json:"session_state,omitempty"`
	}
	inProgressIDs := make([]string, 0)
	for _, t := range tasks {
		if t.Status == "in_progress" {
			inProgressIDs = append(inProgressIDs, t.ID)
		}
	}
	stateMap, _ := s.store.GetSessionStateForTasks(inProgressIDs)

	result := make([]taskWithState, len(tasks))
	for i, t := range tasks {
		result[i] = taskWithState{Task: t}
		if stateMap != nil {
			result[i].SessionState = stateMap[t.ID]
		}
	}

	summary := "no pending tasks"
	if len(tasks) > 0 {
		summary = fmt.Sprintf("%d task(s) pending/in-progress", len(tasks))
	}

	resp := map[string]interface{}{
		"summary":  summary,
		"tasks":    result,
		"reminder": "Call update_task(id, 'in_progress') before starting a task and update_task(id, 'done', notes) immediately when finished. Never batch completions.",
	}

	// suggest_next: when requested, surface the top unblocked pending task so
	// agents don't have to scan the full task list themselves.
	if suggestNext, _ := req.GetArguments()["suggest_next"].(bool); suggestNext {
		for _, t := range tasks {
			if len(t.DependsOn) == 0 && t.Status == "pending" {
				resp["suggested_next"] = map[string]interface{}{
					"id":       t.ID,
					"title":    t.Title,
					"priority": t.Priority,
				}
				break
			}
		}
	}

	return jsonResult(resp)
}

// handleSaveSessionState upserts the working state for an in-progress task.
// Call this at regular intervals while working on a task so future sessions
// can resume from the exact point where this session stopped.
func (s *Server) handleSaveSessionState(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}

	taskID := stringArg(req, "task_id")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required (use get_pending_tasks to list task IDs)"), nil
	}

	state := store.SessionState{
		TaskID:          taskID,
		AgentID:         stringArg(req, "agent_id"),
		Approach:        stringArg(req, "approach"),
		ContextSnapshot: stringArg(req, "context_snapshot"),
	}

	// Parse JSON array fields; accept both raw arrays and JSON strings.
	parseStrArr := func(key string) []string {
		switch v := req.GetArguments()[key].(type) {
		case []interface{}:
			out := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok {
					out = append(out, s)
				}
			}
			return out
		case string:
			if v == "" {
				return nil
			}
			var arr []string
			if err := json.Unmarshal([]byte(v), &arr); err != nil {
				logutil.Debug("synapses: tasks: unmarshal session state field from request: %v\n", err)
			}
			return arr
		}
		return nil
	}

	state.FilesModified = parseStrArr("files_modified")
	state.CompletedSteps = parseStrArr("completed_steps")
	state.RemainingSteps = parseStrArr("remaining_steps")
	state.Blockers = parseStrArr("blockers")
	state.Decisions = parseStrArr("decisions")

	if err := s.store.UpsertSessionState(state); err != nil {
		return toolError("save session state", err)
	}

	return jsonResult(map[string]interface{}{
		"task_id": taskID,
		"message": "Session state saved. The next session will see this state via get_pending_tasks().",
	})
}

// handleGetSessionState returns the saved session state for a task, enabling
// exact-moment resumption of work started in a previous LLM session.
//
// F12: Also returns failure_context — recent failure episodes for the task's
// assigned agent (last 7 days) so the resuming session knows what went wrong.
func (s *Server) handleGetSessionState(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}

	taskID := stringArg(req, "task_id")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required (use get_pending_tasks to list task IDs)"), nil
	}

	state, err := s.store.GetSessionState(taskID)
	if err != nil {
		return toolError("get session state", err)
	}

	// F12: Fetch recent failure episodes for the task's assigned agent so the
	// resuming session can see what previously went wrong (failure-point resumption).
	type failureSummary struct {
		Decision  string `json:"decision"`
		Outcome   string `json:"outcome"`
		Trigger   string `json:"trigger,omitempty"`
		CreatedAt int64  `json:"created_at"`
	}
	var failureCtx []failureSummary
	if task, terr := s.store.GetTask(taskID); terr == nil && task.AssignedTo != "" {
		if eps, eerr := s.store.GetEpisodes("", task.AssignedTo, "failure", nil, 5, 7); eerr == nil {
			for _, ep := range eps {
				failureCtx = append(failureCtx, failureSummary{
					Decision:  ep.Decision,
					Outcome:   ep.Outcome,
					Trigger:   ep.Trigger,
					CreatedAt: ep.CreatedAt,
				})
			}
		}
	}

	if state == nil {
		resp := map[string]interface{}{
			"task_id": taskID,
			"found":   false,
			"message": "No session state saved for this task yet. Call save_session_state() while working to enable resumption.",
		}
		if len(failureCtx) > 0 {
			resp["failure_context"] = failureCtx
		}
		return jsonResult(resp)
	}

	resp := map[string]interface{}{
		"task_id": taskID,
		"found":   true,
		"state":   state,
	}
	if len(failureCtx) > 0 {
		resp["failure_context"] = failureCtx
	}
	return jsonResult(resp)
}

// handleUpdateTask marks a task as done, in_progress, or cancelled, and
// optionally appends timestamped notes for the next session to read.
func (s *Server) handleUpdateTask(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}

	id, _ := req.GetArguments()["id"].(string)
	if id == "" {
		return mcp.NewToolResultError("id is required (task ID — use get_pending_tasks to list)"), nil
	}
	status, _ := req.GetArguments()["status"].(string)
	if status == "" {
		return mcp.NewToolResultError("status is required (pending | in_progress | done | cancelled)"), nil
	}
	validStatuses := map[string]bool{
		"pending": true, "in_progress": true, "done": true, "cancelled": true,
	}
	if !validStatuses[status] {
		return mcp.NewToolResultError(fmt.Sprintf("invalid status %q — must be one of: pending, in_progress, done, cancelled", status)), nil
	}

	notes := stringArg(req, "notes")
	agentID, _ := req.GetArguments()["agent_id"].(string)
	intent, _ := req.GetArguments()["intent"].(string)

	s.upsertAgentIfNeeded(agentID)
	// B29: store declared intent so peers can see what this agent is working on.
	if agentID != "" && intent != "" {
		s.upsertAgentWithActivity(agentID, &store.AgentActivity{Intent: intent})
	}

	// R21: Commit-to-task linking — capture git state before status transitions.
	// For done/cancelled: read start_commit now (before UpdateTask) so we can compute
	// the range log. This is safe because UpdateTask never modifies start_commit.
	var taskStartCommit string
	if (status == "done" || status == "cancelled") && s.projectPath != "" {
		if t, gtErr := s.store.GetTask(id); gtErr == nil {
			taskStartCommit = t.StartCommit
		}
	}

	// P3B-5b: replan detection — check if task is reverting from in_progress back to pending.
	var oldStatus string
	if status == "pending" {
		if t, err := s.store.GetTask(id); err == nil {
			oldStatus = t.Status
		}
	}

	unblocked, planCompleted, err := s.store.UpdateTask(id, status, notes, agentID)
	if err != nil {
		return toolError("update task", err)
	}

	// R21: Capture HEAD SHA on in_progress; capture git log range on done/cancelled.
	// Both are best-effort: git errors are silently ignored so the task update
	// always succeeds regardless of git availability.
	var capturedCommits []string
	if s.projectPath != "" {
		switch status {
		case "in_progress":
			if sha := git.HeadSHA(s.projectPath); sha != "" {
				_ = s.store.SetTaskStartCommit(id, sha)
			}
		case "done", "cancelled":
			capturedCommits = git.LogSince(s.projectPath, taskStartCommit)
			_ = s.store.SetTaskCommits(id, capturedCommits)
		}
	}

	// Session Intelligence: record session-task relationship.
	if mcpSessionID := SessionIDFromContext(ctx); s.store != nil {
		if synapseSessionID := s.getSynapseSessionID(mcpSessionID); synapseSessionID != "" {
			var action store.SessionTaskAction
			switch status {
			case "in_progress":
				action = store.SessionTaskClaimed
			case "done":
				action = store.SessionTaskCompleted
			case "cancelled":
				action = store.SessionTaskAbandoned
			}
			if action != "" {
				s.store.LinkSessionTask(synapseSessionID, id, action)
			}
		}
	}

	// Track agent activity: update or clear current task fields.
	if agentID != "" {
		switch status {
		case "in_progress":
			// Fetch the task title so peers can see what this agent is working on.
			if task, err := s.store.GetTask(id); err == nil {
				s.upsertAgentWithActivity(agentID, &store.AgentActivity{
					TaskID:    id,
					TaskTitle: task.Title,
				})
			}
		case "done", "cancelled":
			_ = s.store.ClearAgentTask(agentID)
			// R29: emit one outcome signal per linked entity so effectiveness
			// scores are computed per-entity. Without linked entities the signal
			// is emitted without an entity as a fallback for aggregate metrics.
			if pc := s.getPulseClient(); pc != nil {
				signalType := "task_done"
				if status == "cancelled" {
					signalType = "task_cancelled"
				}
				projID := s.projectID
				taskID := id
				sg := s.graph
				st := s.store
				aid := agentID
				sig := signalType
				// P6-3: capture pulse session ID so outcome signals can be linked to sessions.
				pulseSessID := s.getSynapseSessionID(SessionIDFromContext(ctx))
				s.goBackground(func() {
					// P3B-5a: compute task duration from CreatedAt for task_done signals.
					var durationMs int
					task, taskErr := st.GetTask(taskID)
					if taskErr == nil && sig == "task_done" {
						if created, parseErr := time.Parse(time.RFC3339, task.CreatedAt); parseErr == nil {
							durationMs = int(time.Since(created).Milliseconds())
						}
					}

					// P8-8: extract task priority for outcome signal correlation.
					var taskPriority string
					if taskErr == nil {
						taskPriority = task.Priority
					}

					// Sprint 15 #1: signal quality weight for per-entity quality scoring.
					sigWeight := pulsetypes.SignalWeightTaskDone
					if sig == "task_cancelled" {
						sigWeight = pulsetypes.SignalWeightTaskCancelled
					}

					var emitted bool
					if sg != nil && taskErr == nil {
						for _, nodeID := range task.LinkedNodes {
							if n := sg.GetNode(graph.NodeID(nodeID)); n != nil && n.Name != "" {
								entity := entityWithPath(n.Name, n.File)
								// P6-11: compute tool calls between last delivery and this outcome.
								toolsBetween := pc.CountToolCallsSinceDelivery(pulseSessID, entity)
								pc.RecordOutcomeSignal(pulse.OutcomeSignalEvent{
									ProjectID:        projID,
									AgentID:          aid,
									Entity:           entity,
									SignalType:       sig,
									Count:            1,
									SessionID:        pulseSessID,
									TimeToOutcomeMs:  int64(durationMs),
									ToolCallsBetween: toolsBetween,
									Priority:         taskPriority,
									SignalWeight:     sigWeight,
								})
								// P5 — Item 10: recompute entity quality score after outcome.
								pc.UpdateEntityQualityScore(entity, projID)
								// P5 — Item 11: link most recent delivery to this outcome.
								if sig == "task_done" {
									if did := pc.GetMostRecentDeliveryID(entity); did > 0 {
										pc.InsertDeliveryOutcome(did, pulseSessID, entity, sig, toolsBetween, true)
									}
								}
								emitted = true
							}
						}
					}
					if !emitted {
						pc.RecordOutcomeSignal(pulse.OutcomeSignalEvent{
							ProjectID:       projID,
							AgentID:         aid,
							SignalType:      sig,
							Count:           1,
							SessionID:       pulseSessID,
							TimeToOutcomeMs: int64(durationMs),
							Priority:        taskPriority,
							SignalWeight:    sigWeight,
						})
					}
				})
			}
		}
	}

	// P3B-5b: emit replan signal when task reverts from in_progress back to pending.
	if status == "pending" && oldStatus == "in_progress" {
		if pc := s.getPulseClient(); pc != nil {
			projCopy := s.projectID
			aidCopy := agentID
			replanSessID := s.getSynapseSessionID(SessionIDFromContext(ctx))
			s.goBackground(func() {
				pc.RecordOutcomeSignal(pulse.OutcomeSignalEvent{
					ProjectID:  projCopy,
					AgentID:    aidCopy,
					SignalType: "replan",
					SessionID:  replanSessID,
				})
			})
		}
	}

	// Emit lifecycle events so other agents polling get_events see the update.
	eventType := "task_update"
	switch status {
	case "in_progress":
		eventType = "agent_task_started"
	case "done":
		eventType = "agent_task_completed"
	}
	if err := s.store.AppendEvent(eventType, agentID,
		fmt.Sprintf(`{"task_id":%q,"status":%q}`, id, status)); err != nil {
		logutil.Warn("synapses: append %s event: %v\n", eventType, err)
	}

	// F11: If this task completion closed the entire plan, emit a plan_completed
	// event so agents polling get_events see the milestone.
	if planCompleted {
		// Best-effort: fetch plan title for a richer event payload.
		planTitle := ""
		if task, err := s.store.GetTask(id); err == nil {
			if plans, err := s.store.GetPlans(); err == nil {
				for _, p := range plans {
					if p.ID == task.PlanID {
						planTitle = p.Title
						break
					}
				}
			}
		}
		if err := s.store.AppendEvent("plan_completed", agentID,
			fmt.Sprintf(`{"task_id":%q,"plan_title":%q}`, id, planTitle)); err != nil {
			logutil.Warn("synapses: append plan_completed event: %v\n", err)
		}
	}

	// B1: Reflective Synthesis — when a task is marked done, annotate its
	// linked nodes with a retrospective note so future agents see the task
	// history in get_context. Runs in a goroutine so it never delays the
	// response. Fail-silent: annotation errors are discarded.
	if status == "done" {
		taskID := id
		aid := agentID
		n := notes
		s.goBackground(func() { s.writeRetrospectiveAnnotations(taskID, aid, n) })
	}

	result := map[string]interface{}{
		"id":      id,
		"status":  status,
		"message": fmt.Sprintf("Task updated to %q.", status),
	}
	if len(unblocked) > 0 {
		result["newly_unblocked"] = unblocked
		result["message"] = fmt.Sprintf("Task updated to %q. %d task(s) are now unblocked: %v", status, len(unblocked), unblocked)
	}
	if planCompleted {
		result["plan_completed"] = true
		result["message"] = result["message"].(string) + " All tasks in the plan are now complete."
	}
	// R21: surface commits made during this task so agents see what shipped.
	if len(capturedCommits) > 0 {
		result["commits_since_start"] = capturedCommits
	}
	return jsonResult(result)
}

// writeRetrospectiveAnnotations is the B1 Reflective Synthesis auditor.
// When a task is marked done it fetches the task's linked_nodes and writes a
// system annotation on each one recording which task touched it, by whom, and
// any completion notes. This builds a "Diary of the Codebase" visible in
// get_context and find_entity responses.
//
// Coupling filter: only nodes with fanin > 3 (called by more than 3 other
// nodes) are annotated — low-connectivity nodes are typically leaf utilities
// that accumulate noisy history with little navigation value.
//
// All errors are silently discarded (fail-silent contract).
func (s *Server) writeRetrospectiveAnnotations(taskID, agentID, completionNotes string) {
	if s.store == nil || s.graph == nil {
		return
	}
	task, err := s.store.GetTask(taskID)
	if err != nil || len(task.LinkedNodes) == 0 {
		return
	}

	// Build the annotation text once — shared across all linked nodes.
	var note strings.Builder
	note.WriteString("[Auditor] Task done: ")
	note.WriteString(task.Title)
	if agentID != "" {
		note.WriteString(" (by ")
		note.WriteString(agentID)
		note.WriteString(")")
	}
	if completionNotes != "" {
		note.WriteString(". Notes: ")
		note.WriteString(completionNotes)
	}
	noteStr := note.String()

	const faninThreshold = 3
	for _, rawID := range task.LinkedNodes {
		nodeID := graph.NodeID(rawID)
		// Only annotate nodes that are genuinely coupled (called by many others).
		// Low-fanin nodes are typically leaf utilities; annotating them adds noise.
		if s.graph.Fanin(nodeID) <= faninThreshold {
			continue
		}
		if _, err := s.store.AddSystemAnnotation(rawID, noteStr); err != nil {
			log.Printf("mcp: add system annotation: %v", err)
		}
	}
}

// entityWithPath returns "name@dir/file" for disambiguation when the same
// function name exists in multiple packages. Uses the last two path components.
// Example: "Health@internal/api/server.go" → unambiguous across packages.
func entityWithPath(name, filePath string) string {
	if filePath == "" {
		return name
	}
	parts := strings.Split(strings.ReplaceAll(filePath, "\\", "/"), "/")
	var short string
	if len(parts) <= 2 {
		short = filePath
	} else {
		short = parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return name + "@" + short
}

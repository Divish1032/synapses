package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// autoLinkNodes scans text for node names that exist in the graph and returns
// their IDs. Only semantic node types (function, method, struct, interface) are
// considered — files and packages produce too many false positives. Names
// shorter than 3 characters are skipped. Results are capped at 10.
//
// Uses a name→nodeID index for O(words_in_text) lookups instead of O(nodes × text).
func (s *Server) autoLinkNodes(text string) []string {
	if s.graph == nil || text == "" {
		return nil
	}
	skip := map[graph.NodeType]bool{
		graph.NodeFile:    true,
		graph.NodePackage: true,
	}

	// Build name→nodeID index (includes both full name and bare method name).
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

	// Extract words from text and look up in the index.
	seen := make(map[string]struct{})
	var result []string
	// Split on common delimiters to get candidate tokens.
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
		_ = s.store.UpsertAgent(agentID) // non-fatal; best-effort
	}
}

// handleCreatePlan persists a new plan and its tasks to the store so future
// LLM sessions can resume the agreed work via get_pending_tasks.
func (s *Server) handleCreatePlan(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}

	title, _ := req.GetArguments()["title"].(string)
	if title == "" {
		return mcp.NewToolResultError("title is required"), nil
	}
	description, _ := req.GetArguments()["description"].(string)
	agentID, _ := req.GetArguments()["agent_id"].(string)

	var taskInputs []store.TaskInput
	switch tv := req.GetArguments()["tasks"].(type) {
	case string:
		// LLM sent tasks as a JSON-encoded string (legacy path).
		if tv == "" {
			return mcp.NewToolResultError("tasks is required (JSON array of task objects)"), nil
		}
		if err := json.Unmarshal([]byte(tv), &taskInputs); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid tasks JSON: %v", err)), nil
		}
	case []interface{}:
		// LLM sent tasks as a native JSON array (normal MCP path).
		b, _ := json.Marshal(tv)
		if err := json.Unmarshal(b, &taskInputs); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid tasks array: %v", err)), nil
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
	for i := range taskInputs {
		detected := s.autoLinkNodes(taskInputs[i].Title + " " + taskInputs[i].Description)
		taskInputs[i].LinkedNodes = mergeNodeIDs(taskInputs[i].LinkedNodes, detected)
	}

	planID, err := s.store.CreatePlan(title, description, agentID, taskInputs)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create plan: %v", err)), nil
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
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}

	planID, _ := req.GetArguments()["plan_id"].(string)
	agentID, _ := req.GetArguments()["agent_id"].(string)

	s.upsertAgentIfNeeded(agentID)

	tasks, err := s.store.GetPendingTasks(planID, agentID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get pending tasks: %v", err)), nil
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
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}

	taskID := stringArg(req, "task_id")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
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
			_ = json.Unmarshal([]byte(v), &arr)
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
		return mcp.NewToolResultError(fmt.Sprintf("save session state: %v", err)), nil
	}

	return jsonResult(map[string]interface{}{
		"task_id": taskID,
		"message": "Session state saved. The next session will see this state via get_pending_tasks().",
	})
}

// handleGetSessionState returns the saved session state for a task, enabling
// exact-moment resumption of work started in a previous LLM session.
func (s *Server) handleGetSessionState(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}

	taskID := stringArg(req, "task_id")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	state, err := s.store.GetSessionState(taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get session state: %v", err)), nil
	}
	if state == nil {
		return jsonResult(map[string]interface{}{
			"task_id": taskID,
			"found":   false,
			"message": "No session state saved for this task yet. Call save_session_state() while working to enable resumption.",
		})
	}

	return jsonResult(map[string]interface{}{
		"task_id": taskID,
		"found":   true,
		"state":   state,
	})
}

// handleUpdateTask marks a task as done, in_progress, or cancelled, and
// optionally appends timestamped notes for the next session to read.
func (s *Server) handleUpdateTask(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}

	id, _ := req.GetArguments()["id"].(string)
	if id == "" {
		return mcp.NewToolResultError("id is required"), nil
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

	notes, _ := req.GetArguments()["notes"].(string)
	agentID, _ := req.GetArguments()["agent_id"].(string)

	s.upsertAgentIfNeeded(agentID)

	unblocked, err := s.store.UpdateTask(id, status, notes, agentID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("update task: %v", err)), nil
	}

	// Emit event so other agents polling get_events see the update.
	if err := s.store.AppendEvent("task_update", agentID,
		fmt.Sprintf(`{"task_id":%q,"status":%q}`, id, status)); err != nil {
		fmt.Fprintf(os.Stderr, "synapses: append task_update event: %v\n", err)
	}

	// B1: Reflective Synthesis — when a task is marked done, annotate its
	// linked nodes with a retrospective note so future agents see the task
	// history in get_context. Runs in a goroutine so it never delays the
	// response. Fail-silent: annotation errors are discarded.
	if status == "done" {
		go s.writeRetrospectiveAnnotations(id, agentID, notes)
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

// handleHandoffTask transfers a task from one agent to another, preserving
// session state so the receiving agent can resume seamlessly.
func (s *Server) handleHandoffTask(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}

	taskID, _ := req.GetArguments()["task_id"].(string)
	fromAgent, _ := req.GetArguments()["from_agent"].(string)
	toAgent, _ := req.GetArguments()["to_agent"].(string)
	notes, _ := req.GetArguments()["notes"].(string)

	if taskID == "" || fromAgent == "" || toAgent == "" {
		return mcp.NewToolResultError("task_id, from_agent, and to_agent are all required"), nil
	}

	// Verify task exists.
	task, err := s.store.GetTask(taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("task not found: %v", err)), nil
	}

	// Re-assign the task to the new agent.
	handoffNote := fmt.Sprintf("Handed off from %s to %s", fromAgent, toAgent)
	if notes != "" {
		handoffNote += ". " + notes
	}
	if _, err := s.store.UpdateTask(taskID, "in_progress", handoffNote, toAgent); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("update task: %v", err)), nil
	}

	// Emit event for multi-agent awareness.
	payload, _ := json.Marshal(map[string]string{
		"task_id":    taskID,
		"task_title": task.Title,
		"from_agent": fromAgent,
		"to_agent":   toAgent,
	})
	if err := s.store.AppendEvent("task_handoff", fromAgent, string(payload)); err != nil {
		log.Printf("mcp: append task_handoff event: %v", err)
	}

	// Register both agents.
	s.upsertAgentIfNeeded(fromAgent)
	s.upsertAgentIfNeeded(toAgent)

	// Retrieve session state if it exists, so we can confirm it's available.
	var stateAvailable bool
	if ss, err := s.store.GetSessionState(taskID); err == nil && ss != nil {
		stateAvailable = true
	}

	return jsonResult(map[string]interface{}{
		"status":          "handed_off",
		"task_id":         taskID,
		"from":            fromAgent,
		"to":              toAgent,
		"session_state":   stateAvailable,
		"hint":            fmt.Sprintf("Agent %s can call get_session_state(task_id=%q) to resume from the exact state.", toAgent, taskID),
	})
}

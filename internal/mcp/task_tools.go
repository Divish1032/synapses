package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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

	title, _ := req.Params.Arguments["title"].(string)
	if title == "" {
		return mcp.NewToolResultError("title is required"), nil
	}
	description, _ := req.Params.Arguments["description"].(string)
	agentID, _ := req.Params.Arguments["agent_id"].(string)

	var taskInputs []store.TaskInput
	switch tv := req.Params.Arguments["tasks"].(type) {
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

	planID, _ := req.Params.Arguments["plan_id"].(string)
	agentID, _ := req.Params.Arguments["agent_id"].(string)

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
		switch v := req.Params.Arguments[key].(type) {
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

	id, _ := req.Params.Arguments["id"].(string)
	if id == "" {
		return mcp.NewToolResultError("id is required"), nil
	}
	status, _ := req.Params.Arguments["status"].(string)
	if status == "" {
		return mcp.NewToolResultError("status is required (pending | in_progress | done | cancelled)"), nil
	}
	validStatuses := map[string]bool{
		"pending": true, "in_progress": true, "done": true, "cancelled": true,
	}
	if !validStatuses[status] {
		return mcp.NewToolResultError(fmt.Sprintf("invalid status %q — must be one of: pending, in_progress, done, cancelled", status)), nil
	}

	notes, _ := req.Params.Arguments["notes"].(string)
	agentID, _ := req.Params.Arguments["agent_id"].(string)

	s.upsertAgentIfNeeded(agentID)

	unblocked, err := s.store.UpdateTask(id, status, notes, agentID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("update task: %v", err)), nil
	}

	// Emit event so other agents polling get_events see the update.
	_ = s.store.AppendEvent("task_update", agentID,
		fmt.Sprintf(`{"task_id":%q,"status":%q}`, id, status))

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

// handleGetPlans returns all plans with task completion summaries.
func (s *Server) handleGetPlans(
	_ context.Context,
	_ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}

	plans, err := s.store.GetPlans()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get plans: %v", err)), nil
	}

	return jsonResult(map[string]interface{}{
		"count": len(plans),
		"plans": plans,
	})
}

// handleGetMyTasks is a convenience wrapper for agents: returns only the
// calling agent's unblocked pending tasks, plus a suggested "next task"
// (highest-priority unblocked task not currently in_progress by another agent).
func (s *Server) handleGetMyTasks(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}

	agentID, _ := req.Params.Arguments["agent_id"].(string)
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}
	planID, _ := req.Params.Arguments["plan_id"].(string)

	s.upsertAgentIfNeeded(agentID)

	tasks, err := s.store.GetPendingTasks(planID, agentID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get tasks: %v", err)), nil
	}

	// Filter out blocked tasks for the focused view.
	var unblocked []interface{}
	var suggested interface{}
	for _, t := range tasks {
		if t.Blocked {
			continue
		}
		taskCopy := t
		unblocked = append(unblocked, taskCopy)
		// Suggest the first in_progress task; fall back to first pending.
		if suggested == nil || t.Status == "in_progress" {
			suggested = taskCopy
		}
	}

	return jsonResult(map[string]interface{}{
		"agent_id":        agentID,
		"unblocked_count": len(unblocked),
		"tasks":           unblocked,
		"suggested_next":  suggested,
	})
}

// handleLinkTaskNodes explicitly links a task to code graph nodes. If query is
// provided, it searches for nodes whose name matches the query and links them.
// If query is empty, the task's own title+description+notes are scanned for
// node name mentions. The result is MERGED with any existing linked_nodes.
func (s *Server) handleLinkTaskNodes(
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
	query := stringArg(req, "query")

	task, err := s.store.GetTask(taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get task: %v", err)), nil
	}

	var detected []string
	if query != "" {
		// Explicit query: find nodes whose name contains the query string.
		if s.graph != nil {
			for _, n := range s.graph.FindByName(query) {
				detected = append(detected, string(n.ID))
			}
		}
	} else {
		// Auto-scan: detect node names mentioned in the task text.
		text := task.Title + " " + task.Description + " " + task.Notes
		detected = s.autoLinkNodes(text)
	}

	merged := mergeNodeIDs(task.LinkedNodes, detected)
	if err := s.store.UpdateLinkedNodes(taskID, merged); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("update linked nodes: %v", err)), nil
	}

	newCount := len(merged) - len(task.LinkedNodes)
	return jsonResult(map[string]interface{}{
		"task_id":      taskID,
		"linked_nodes": merged,
		"added":        newCount,
		"message":      fmt.Sprintf("Task now linked to %d node(s) (%+d from this call).", len(merged), newCount),
	})
}

// handleGetAgents returns all agents that have interacted with Synapses,
// ordered by last-seen timestamp. Useful for multi-agent coordination to
// discover which agents are active.
func (s *Server) handleGetAgents(
	_ context.Context,
	_ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}

	agents, err := s.store.GetAgents()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get agents: %v", err)), nil
	}

	return jsonResult(map[string]interface{}{
		"count":  len(agents),
		"agents": agents,
	})
}

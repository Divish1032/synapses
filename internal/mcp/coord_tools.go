package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// handleGetPlans lists all plans with task completion counts.
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
	summary := "no plans found"
	if len(plans) > 0 {
		summary = fmt.Sprintf("%d plan(s)", len(plans))
	}
	return jsonResult(map[string]interface{}{
		"summary": summary,
		"plans":   plans,
	})
}

// handleGetMyTasks returns unblocked pending tasks for a specific agent.
// Requires agent_id. Optionally scoped to a plan_id.
func (s *Server) handleGetMyTasks(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}
	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}
	planID := stringArg(req, "plan_id")

	s.upsertAgentIfNeeded(agentID)

	tasks, err := s.store.GetPendingTasks(planID, agentID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get tasks: %v", err)), nil
	}

	// Pick the top unblocked task as the suggested next task.
	var suggested interface{}
	for _, t := range tasks {
		if len(t.DependsOn) == 0 && t.Status == "pending" {
			suggested = map[string]interface{}{
				"id":       t.ID,
				"title":    t.Title,
				"priority": t.Priority,
			}
			break
		}
	}

	summary := fmt.Sprintf("0 tasks for agent %q", agentID)
	if len(tasks) > 0 {
		summary = fmt.Sprintf("%d task(s) for agent %q", len(tasks), agentID)
	}
	return jsonResult(map[string]interface{}{
		"summary":        summary,
		"tasks":          tasks,
		"suggested_next": suggested,
	})
}

// handleLinkTaskNodes explicitly links a task to graph node IDs.
// Replaces existing links with the provided list.
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

	// Accept node_ids as a JSON array string or a raw []interface{}.
	var nodeIDs []string
	switch v := req.GetArguments()["node_ids"].(type) {
	case string:
		_ = json.Unmarshal([]byte(v), &nodeIDs)
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				nodeIDs = append(nodeIDs, s)
			}
		}
	}
	if len(nodeIDs) == 0 {
		return mcp.NewToolResultError("node_ids must be a non-empty array of node ID strings"), nil
	}

	if err := s.store.UpdateLinkedNodes(taskID, nodeIDs); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("link task nodes: %v", err)), nil
	}
	return jsonResult(map[string]interface{}{
		"task_id":  taskID,
		"linked":   len(nodeIDs),
		"node_ids": nodeIDs,
		"message":  "Nodes linked. get_context(task_id=) will now boost these nodes in relevance ranking.",
	})
}

// handleGetAgents returns all agents that have interacted with Synapses,
// ordered by last-seen timestamp descending.
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
	summary := "no agents seen yet"
	if len(agents) > 0 {
		summary = fmt.Sprintf("%d agent(s) known", len(agents))
	}
	return jsonResult(map[string]interface{}{
		"summary": summary,
		"agents":  agents,
	})
}

// handleClaimWork registers active work on a scope (file, package, directory,
// or entity). Returns any conflicting claims by other agents immediately so the
// caller can decide whether to proceed or coordinate.
func (s *Server) handleClaimWork(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}
	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}
	scope := stringArg(req, "scope")
	if scope == "" {
		return mcp.NewToolResultError("scope is required (e.g. 'internal/auth' or 'cmd/server/main.go')"), nil
	}
	scopeType := stringArg(req, "scope_type")
	if scopeType == "" {
		scopeType = "path"
	}
	ttl := 30
	if v, ok := req.GetArguments()["ttl_minutes"].(float64); ok && v > 0 {
		ttl = int(v)
	}

	s.upsertAgentIfNeeded(agentID)

	conflicts, err := s.store.ClaimWork(agentID, scope, scopeType, ttl)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("claim work: %v", err)), nil
	}

	// Emit event so agents polling get_events see claim activity.
	if err := s.store.AppendEvent("claim_work", agentID,
		fmt.Sprintf(`{"scope":%q,"scope_type":%q,"ttl_minutes":%d,"conflicts":%d}`, scope, scopeType, ttl, len(conflicts))); err != nil {
		fmt.Fprintf(os.Stderr, "synapses: append claim_work event: %v\n", err)
	}

	msg := fmt.Sprintf("Claimed %q for %d minutes.", scope, ttl)
	if len(conflicts) > 0 {
		msg += fmt.Sprintf(" WARNING: %d conflicting claim(s) by other agents — review before editing.", len(conflicts))
	}
	return jsonResult(map[string]interface{}{
		"claimed":   scope,
		"agent_id":  agentID,
		"ttl":       ttl,
		"conflicts": conflicts,
		"message":   msg,
	})
}

// handleReleaseClaims removes all active work claims for the given agent.
// Call this when editing is complete to free scopes for other agents.
func (s *Server) handleReleaseClaims(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}
	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}
	if err := s.store.ReleaseClaims(agentID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("release claims: %v", err)), nil
	}

	// Emit event so agents polling get_events see claim releases.
	if err := s.store.AppendEvent("claim_released", agentID,
		fmt.Sprintf(`{"agent_id":%q}`, agentID)); err != nil {
		fmt.Fprintf(os.Stderr, "synapses: append claim_released event: %v\n", err)
	}

	return jsonResult(map[string]interface{}{
		"agent_id": agentID,
		"message":  "All work claims released.",
	})
}

// handleGetConflicts returns all work claims by other agents that overlap with
// any scope the calling agent currently holds.
func (s *Server) handleGetConflicts(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}
	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}
	s.upsertAgentIfNeeded(agentID)

	conflicts, err := s.store.GetConflicts(agentID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get conflicts: %v", err)), nil
	}
	summary := "no conflicts"
	if len(conflicts) > 0 {
		summary = fmt.Sprintf("%d conflicting claim(s) detected", len(conflicts))
	}
	return jsonResult(map[string]interface{}{
		"summary":   summary,
		"conflicts": conflicts,
	})
}

// handleGetEvents returns events from the pull-based event log with seq >
// since_seq. Use the latest_event_seq from session_init as the starting cursor.
// Event types: file_change, task_update, annotation_added, agent_activity.
func (s *Server) handleGetEvents(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}

	var sinceSeq int64
	if v, ok := req.GetArguments()["since_seq"].(float64); ok {
		sinceSeq = int64(v)
	}

	limit := 50
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	// Parse optional types filter — accepts JSON array string or []interface{}.
	var types []string
	switch v := req.GetArguments()["types"].(type) {
	case string:
		if v != "" {
			for _, t := range strings.Split(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					types = append(types, t)
				}
			}
		}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				types = append(types, s)
			}
		}
	}

	events, latestSeq, err := s.store.GetEvents(sinceSeq, types, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get events: %v", err)), nil
	}

	summary := "no new events"
	if len(events) > 0 {
		summary = fmt.Sprintf("%d event(s) since seq %d", len(events), sinceSeq)
	}
	return jsonResult(map[string]interface{}{
		"summary":    summary,
		"events":     events,
		"latest_seq": latestSeq,
		"hint":       "Store latest_seq and pass as since_seq on next poll to get only new events.",
	})
}

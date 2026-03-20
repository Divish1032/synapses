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
		if err := json.Unmarshal([]byte(v), &nodeIDs); err != nil {
			fmt.Fprintf(os.Stderr, "DEBUG: synapses: coord: unmarshal node_ids from request: %v\n", err)
		}
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
// ordered by last-seen timestamp descending. Includes presence classification
// and current task/focus.
func (s *Server) handleGetAgents(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}
	agents, err := s.store.GetAgents()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get agents: %v", err)), nil
	}

	// Cross-project agents via daemon registry.
	var crossProjectAgents []map[string]interface{}
	if projectsParam := stringArg(req, "projects"); projectsParam != "" && s.projectRegistry != nil {
		stores, notFound := s.resolveProjectStores(projectsParam)
		for projName, projStore := range stores {
			projAgents, pErr := projStore.GetAgents()
			if pErr != nil {
				continue
			}
			for _, a := range projAgents {
				crossProjectAgents = append(crossProjectAgents, map[string]interface{}{
					"source":    fmt.Sprintf("[%s]", projName),
					"agent_id":  a.ID,
					"presence":  a.Presence,
					"last_seen": a.LastSeen,
					"intent":    a.Intent,
				})
			}
		}
		if len(notFound) > 0 {
			crossProjectAgents = append(crossProjectAgents, map[string]interface{}{
				"_error": fmt.Sprintf("unknown project(s): %s. Available: %s", strings.Join(notFound, ", "), strings.Join(s.allowedProjectNames(), ", ")),
			})
		}
	}

	active := 0
	for _, a := range agents {
		if a.Presence == "active" || a.Presence == "idle" {
			active++
		}
	}

	summary := "no agents seen yet"
	if len(agents) > 0 {
		summary = fmt.Sprintf("%d agent(s) known (%d currently active/idle)", len(agents), active)
	}
	resp := map[string]interface{}{
		"summary": summary,
		"agents":  agents,
	}
	if len(crossProjectAgents) > 0 {
		resp["cross_project_agents"] = crossProjectAgents
	}
	return jsonResult(resp)
}

// handleGetEvents returns events from the pull-based event log with seq >
// since_seq. Use the latest_event_seq from session_init as the starting cursor.
// Emitted event types: file_change, agent_examining, agent_message,
// agent_session_start, task_update, task_node_changed, plan_completed,
// annotation_added, failure_recorded, rule_violation.
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

	// Optional agent_id filter: returns only events emitted by that agent.
	// Use for on-demand peer activity stream (Tier 3).
	agentIDFilter := stringArg(req, "agent_id")

	events, latestSeq, err := s.store.GetEvents(sinceSeq, types, agentIDFilter, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get events: %v", err)), nil
	}

	// Cross-project events via daemon registry.
	// Returned in a separate field so latest_seq remains a clean local cursor.
	var crossProjectEvents []map[string]interface{}
	if projectsParam := stringArg(req, "projects"); projectsParam != "" && s.projectRegistry != nil {
		stores, notFound := s.resolveProjectStores(projectsParam)
		for projName, projStore := range stores {
			projEvents, _, pErr := projStore.GetEvents(sinceSeq, types, agentIDFilter, limit)
			if pErr != nil {
				continue
			}
			for _, e := range projEvents {
				crossProjectEvents = append(crossProjectEvents, map[string]interface{}{
					"source":     fmt.Sprintf("[%s]", projName),
					"seq":        e.Seq,
					"type":       e.Type,
					"agent_id":   e.AgentID,
					"payload":    e.Payload,
					"created_at": e.CreatedAt,
				})
			}
		}
		if len(notFound) > 0 {
			crossProjectEvents = append(crossProjectEvents, map[string]interface{}{
				"_error": fmt.Sprintf("unknown project(s): %s. Available: %s", strings.Join(notFound, ", "), strings.Join(s.allowedProjectNames(), ", ")),
			})
		}
	}

	summary := "no new events"
	if len(events) > 0 {
		summary = fmt.Sprintf("%d event(s) since seq %d", len(events), sinceSeq)
	}
	resp := map[string]interface{}{
		"summary":    summary,
		"events":     events,
		"latest_seq": latestSeq,
		"hint":       "Store latest_seq and pass as since_seq on next poll to get only new events.",
	}
	if len(crossProjectEvents) > 0 {
		resp["cross_project_events"] = crossProjectEvents
	}
	return jsonResult(resp)
}

package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// handleGetPlans lists all plans with task completion counts.
func (s *Server) handleGetPlans(
	_ context.Context,
	_ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}
	plans, err := s.store.GetPlans()
	if err != nil {
		return toolError("get plans", err)
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
		return mcp.NewToolResultError("task memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}
	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required (e.g., 'implementer', 'reviewer')"), nil
	}
	planID := stringArg(req, "plan_id")

	s.upsertAgentIfNeeded(agentID)

	tasks, err := s.store.GetPendingTasks(planID, agentID)
	if err != nil {
		return toolError("get tasks", err)
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
		return mcp.NewToolResultError("task memory unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}
	taskID := stringArg(req, "task_id")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required (use tasks(action=\"pending\") to list task IDs)"), nil
	}

	// Accept node_ids as a JSON array string or a raw []interface{}.
	var nodeIDs []string
	switch v := req.GetArguments()["node_ids"].(type) {
	case string:
		if err := json.Unmarshal([]byte(v), &nodeIDs); err != nil {
			logutil.Debug("synapses: coord: unmarshal node_ids from request: %v\n", err)
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
		return toolError("link task nodes", err)
	}
	return jsonResult(map[string]interface{}{
		"task_id":  taskID,
		"linked":   len(nodeIDs),
		"node_ids": nodeIDs,
		"message":  "Nodes linked. get_context(task_id=) will now boost these nodes in relevance ranking.",
	})
}

// Sprint 24: handleGetAgents and handleGetEvents removed.
// Cross-session awareness is now handled by the Work Ledger.

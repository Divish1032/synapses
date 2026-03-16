package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/store"
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
// ordered by last-seen timestamp descending. Includes presence classification,
// current task/focus, and active work claims per agent.
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

	// Attach active claims per agent in a single query.
	allClaims, _ := s.store.GetAllClaims()
	claimMap := make(map[string][]store.WorkClaim)
	for _, c := range allClaims {
		claimMap[c.AgentID] = append(claimMap[c.AgentID], c)
	}

	type agentWithClaims struct {
		store.Agent
		Claims []store.WorkClaim `json:"claims,omitempty"`
	}
	enriched := make([]agentWithClaims, len(agents))
	active := 0
	for i, a := range agents {
		enriched[i] = agentWithClaims{Agent: a}
		if cls, ok := claimMap[a.ID]; ok {
			enriched[i].Claims = cls
		}
		if a.Presence == "active" || a.Presence == "idle" {
			active++
		}
	}

	summary := "no agents seen yet"
	if len(agents) > 0 {
		summary = fmt.Sprintf("%d agent(s) known (%d currently active/idle)", len(agents), active)
	}
	return jsonResult(map[string]interface{}{
		"summary": summary,
		"agents":  enriched,
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

	// R28: Semantic Firewall — warn when claiming vendored/generated paths.
	// Agents should not be editing these files; the warning surfaces the issue
	// without blocking legitimate maintenance tasks (e.g. "update vendor deps").
	scopeNorm := filepath.ToSlash(scope)
	for _, seg := range []string{"/vendor/", "/node_modules/", "/third_party/"} {
		if strings.Contains(scopeNorm+"/", seg) || strings.HasPrefix(scopeNorm, strings.TrimPrefix(seg, "/")) {
			msg += " ⚠ Provenance warning: scope appears to be a vendored or generated path. " +
				"Editing third-party code is usually the wrong approach — consider patching via your dependency manager instead."
			break
		}
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
// Emitted event types: file_change, agent_examining, claim_work, claim_released,
// agent_message, agent_session_start, agent_task_started, agent_task_completed,
// task_handoff, annotation_added, failure_recorded, rule_violation,
// intent_received, intent_broadcast, proposal_created, proposal_voted, proposal_resolved.
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

// handleGetPeerActivity returns a structured digest of a specific peer agent's
// recent actions. This is the Tier 3 on-demand signal: call it explicitly when
// you want to understand what a peer has been doing. It is never injected into
// session_init automatically — use conflicts/dependency_alerts for proactive signals.
func (s *Server) handleGetPeerActivity(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("task memory unavailable: server started without a persistent store"), nil
	}
	peerID := stringArg(req, "agent_id")
	if peerID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}

	var sinceSeq int64
	if v, ok := req.GetArguments()["since_seq"].(float64); ok {
		sinceSeq = int64(v)
	}
	limit := 10
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	// Fetch agent profile.
	agents, err := s.store.GetAgents()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get agents: %v", err)), nil
	}
	var profile *store.Agent
	for i := range agents {
		if agents[i].ID == peerID {
			profile = &agents[i]
			break
		}
	}
	if profile == nil {
		return jsonResult(map[string]interface{}{
			"error": fmt.Sprintf("agent %q not found", peerID),
		})
	}

	// Fetch active scopes for this peer. GetMyClaims is targeted (indexed by agent_id)
	// whereas GetAllClaims fetches all agents' claims and filters inline.
	scopes := make([]string, 0)
	if claims, err := s.store.GetMyClaims(peerID); err == nil {
		for _, c := range claims {
			scopes = append(scopes, c.Scope)
		}
	}

	// Fetch recent events from this peer. Note: file_change events are emitted by the
	// file watcher with agent_id="" so they can't be filtered by peer — excluded here.
	// The peer's active scopes (work_claims) already convey what files they own.
	activityTypes := []string{"agent_examining", "agent_task_started", "claim_work"}
	events, latestSeq, _ := s.store.GetEvents(sinceSeq, activityTypes, peerID, limit)

	// Assemble recent_actions digest.
	type action struct {
		Type   string `json:"type"`
		Entity string `json:"entity,omitempty"`
		File   string `json:"file,omitempty"`
		Task   string `json:"task,omitempty"`
		Scope  string `json:"scope,omitempty"`
		At     string `json:"at"`
	}
	recentActions := make([]action, 0, len(events))
	for _, e := range events {
		a := action{Type: e.Type, At: e.CreatedAt}
		// Parse common payload fields.
		var p map[string]interface{}
		if err := json.Unmarshal([]byte(e.Payload), &p); err == nil {
			if v, ok := p["entity"].(string); ok {
				a.Entity = v
			}
			if v, ok := p["file"].(string); ok {
				a.File = v
			}
			if v, ok := p["task"].(string); ok {
				a.Task = v
			}
			if v, ok := p["scope"].(string); ok {
				a.Scope = v
			}
		}
		recentActions = append(recentActions, a)
	}

	return jsonResult(map[string]interface{}{
		"agent_id":        peerID,
		"intent":          profile.Intent,
		"presence":        profile.Presence,
		"focus":           profile.CurrentFocus,
		"focus_file":      profile.CurrentFocusFile,
		"focus_age_seconds": func() int {
			if profile.CurrentFocusSince == "" {
				return 0
			}
			t, err := time.Parse(time.RFC3339, profile.CurrentFocusSince)
			if err != nil {
				return 0
			}
			return int(time.Since(t).Seconds())
		}(),
		"task":           profile.CurrentTaskTitle,
		"scopes":         scopes,
		"recent_actions": recentActions,
		"latest_seq":     latestSeq,
		"hint":           "Pass latest_seq as since_seq on next call to get only new activity.",
	})
}

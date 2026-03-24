package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/pulse"
)

// handleSendMessage sends a message from one agent to another (or broadcasts
// it to all agents) via the SQLite message bus.
func (s *Server) handleSendMessage(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("message bus unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}

	fromAgent := stringArg(req, "from_agent")
	if fromAgent == "" {
		return mcp.NewToolResultError("from_agent is required (e.g., 'implementer', 'reviewer')"), nil
	}
	topic := stringArg(req, "topic")
	if topic == "" {
		return mcp.NewToolResultError("topic is required (e.g. 'api_changed', 'task_blocked')"), nil
	}

	toAgent := strings.TrimSpace(stringArg(req, "to_agent")) // empty = broadcast
	projectID := stringArg(req, "project_id")
	if projectID == "" {
		if s.graph != nil {
			projectID = s.graph.RepoID()
		} else {
			projectID = filepath.Base(s.projectPath)
		}
	}

	// payload must be valid JSON; default to empty object if omitted.
	payload, payloadErr := stringArgLimited(req, "payload", maxArgLengthPayload)
	if payloadErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(payloadErr.Error())), nil
	}
	if payload == "" {
		payload = "{}"
	}
	// Validate that payload is parseable JSON to catch agent mistakes early.
	if !json.Valid([]byte(payload)) {
		return mcp.NewToolResultError("payload must be valid JSON (e.g. '{\"key\":\"value\"}')"), nil
	}

	// OF-S2: scan payload for prompt injection patterns.
	var injectionWarning string
	if scanResult, scanErr := s.scanContent("payload", payload); scanErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(scanErr.Error())), nil
	} else if scanResult.warning != "" {
		injectionWarning = scanResult.warning
		// P7-1: emit guard event for injection scan trigger.
		if pc := s.getPulseClient(); pc != nil {
			pc.RecordGuardEvent(pulse.GuardEvent{
				GuardType: "injection_scan", ToolName: "send_message",
				Category: "warn", AgentID: fromAgent, ProjectID: s.projectID,
			})
		}
		// In truncate mode, stripping regex matches from JSON can produce
		// invalid JSON. Fall back to original content (warn behavior) if the
		// sanitized payload is no longer valid JSON.
		if json.Valid([]byte(scanResult.sanitized)) {
			payload = scanResult.sanitized
		}
		// else: keep original payload, warning is still surfaced
	}

	// OF-E3: cross-project write approval gate for broadcast messages.
	// Broadcasts are visible to all agents on all projects via get_messages(projects="*").
	// Require explicit user approval before sending.
	if toAgent == "" {
		// OF-E3: check for an out-of-band user-approved approval file.
		// The agent never sees the token — only the user can approve via `synapses approve`.
		if !s.approvals.checkAndConsumeApproval("broadcast_message", fromAgent) {
			// P7-2: emit guard event for approval gate request.
			if pc := s.getPulseClient(); pc != nil {
				pc.RecordGuardEvent(pulse.GuardEvent{
					GuardType: "approval_gate", ToolName: "send_message",
					Category: "requested", AgentID: fromAgent, ProjectID: s.projectID,
				})
			}
			return s.approvals.requestApproval(
				"broadcast_message",
				fmt.Sprintf("Broadcast message from agent %q with topic %q to all agents across all projects", fromAgent, topic),
				fromAgent,
			), nil
		}
		// P7-2: emit guard event for approval gate consumption.
		if pc := s.getPulseClient(); pc != nil {
			pc.RecordGuardEvent(pulse.GuardEvent{
				GuardType: "approval_gate", ToolName: "send_message",
				Category: "consumed", AgentID: fromAgent, ProjectID: s.projectID,
			})
		}
	}

	s.upsertAgentIfNeeded(fromAgent)

	msgID, err := s.store.SendMessage(fromAgent, toAgent, topic, payload, projectID)
	if err != nil {
		return toolError("send message", err)
	}

	// Emit event so agents polling get_events see the message immediately
	// without needing to poll get_messages as well.
	msgPayload, _ := json.Marshal(map[string]string{
		"message_id": msgID, "topic": topic, "to_agent": toAgent, "project_id": projectID,
	})
	if err := s.store.AppendEvent("agent_message", fromAgent, string(msgPayload)); err != nil {
		logutil.Warn("synapses: append agent_message event: %v\n", err)
	}

	audience := "all agents (broadcast)"
	if toAgent != "" {
		audience = fmt.Sprintf("agent %q", toAgent)
	}
	resp := map[string]interface{}{
		"message_id": msgID,
		"from_agent": fromAgent,
		"to_agent":   toAgent,
		"topic":      topic,
		"audience":   audience,
		"message":    fmt.Sprintf("Message sent to %s. Recipient can retrieve it via get_messages(unread_only=true).", audience),
	}
	if injectionWarning != "" {
		resp["injection_warning"] = injectionWarning
	}
	return jsonResult(resp)
}

// handleGetMessages retrieves messages visible to an agent from the message bus.
// Visible = directly addressed OR broadcast (to_agent NULL).
func (s *Server) handleGetMessages(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("message bus unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required (e.g., 'implementer', 'reviewer')"), nil
	}

	var sinceSeq int64
	if v, ok := req.GetArguments()["since_seq"].(float64); ok && v > 0 {
		sinceSeq = int64(v)
	}

	topicFilter := stringArg(req, "topic_filter")

	// unread_only defaults to true — the most common use case is checking
	// for new messages at session start without re-processing old ones.
	unreadOnly := true
	if v, ok := req.GetArguments()["unread_only"].(bool); ok {
		unreadOnly = v
	}

	limit := 50
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	s.upsertAgentIfNeeded(agentID)

	msgs, latestSeq, err := s.store.GetMessages(agentID, sinceSeq, topicFilter, unreadOnly, limit)
	if err != nil {
		return toolError("get messages", err)
	}

	// mark_read_ids: batch-acknowledge messages in the same call, eliminating
	// the need for a separate mark_read round-trip.
	var markedCount int
	switch v := req.GetArguments()["mark_read_ids"].(type) {
	case string:
		if v != "" {
			var ids []string
			if json.Unmarshal([]byte(v), &ids) == nil {
				for _, id := range ids {
					if e := s.store.MarkRead(id, agentID); e == nil {
						markedCount++
					}
				}
			}
		}
	case []interface{}:
		for _, item := range v {
			if id, ok := item.(string); ok {
				if e := s.store.MarkRead(id, agentID); e == nil {
					markedCount++
				}
			}
		}
	}

	// Cross-project messages via daemon registry.
	var crossProjectMsgs []map[string]interface{}
	if projectsParam := stringArg(req, "projects"); projectsParam != "" && s.projectRegistry != nil {
		stores, notFound := s.resolveProjectStores(projectsParam)
		for projName, projStore := range stores {
			projMsgs, _, pErr := projStore.GetMessages(agentID, sinceSeq, topicFilter, unreadOnly, limit)
			if pErr != nil {
				continue
			}
			for _, m := range projMsgs {
				crossProjectMsgs = append(crossProjectMsgs, map[string]interface{}{
					"source":     fmt.Sprintf("[%s]", projName),
					"id":         m.ID,
					"from_agent": m.FromAgent,
					"to_agent":   m.ToAgent,
					"topic":      m.Topic,
					"payload":    m.Payload,
					"created_at": m.CreatedAt,
				})
			}
		}
		if len(notFound) > 0 {
			crossProjectMsgs = append(crossProjectMsgs, map[string]interface{}{
				"_error": fmt.Sprintf("unknown project(s): %s. Available: %s", strings.Join(notFound, ", "), strings.Join(s.allowedProjectNames(), ", ")),
			})
		}
	}

	// BUG-011: Output-path injection scanning for messages.
	for i := range msgs {
		msgs[i].Payload = s.scanOutputContent(msgs[i].Payload)
	}

	summary := fmt.Sprintf("no messages for agent %q", agentID)
	if len(msgs) > 0 {
		summary = fmt.Sprintf("%d message(s) for agent %q", len(msgs), agentID)
	}
	resp := map[string]interface{}{
		"summary":    summary,
		"messages":   msgs,
		"latest_seq": latestSeq,
		"hint":       "Pass latest_seq as since_seq on next call to receive only new messages. Pass mark_read_ids=[\"id1\",\"id2\"] to acknowledge messages in the same call.",
	}
	if markedCount > 0 {
		resp["marked_read"] = markedCount
	}
	if len(crossProjectMsgs) > 0 {
		resp["cross_project_messages"] = crossProjectMsgs
	}
	return jsonResult(resp)
}


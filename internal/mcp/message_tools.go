package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// handleSendMessage sends a message from one agent to another (or broadcasts
// it to all agents) via the SQLite message bus.
func (s *Server) handleSendMessage(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("message bus unavailable: server started without a persistent store"), nil
	}

	fromAgent := stringArg(req, "from_agent")
	if fromAgent == "" {
		return mcp.NewToolResultError("from_agent is required"), nil
	}
	topic := stringArg(req, "topic")
	if topic == "" {
		return mcp.NewToolResultError("topic is required (e.g. 'api_changed', 'task_blocked')"), nil
	}

	toAgent := stringArg(req, "to_agent") // empty = broadcast
	projectID := stringArg(req, "project_id")
	if projectID == "" {
		projectID = s.graph.RepoID()
	}

	// payload must be valid JSON; default to empty object if omitted.
	payload := stringArg(req, "payload")
	if payload == "" {
		payload = "{}"
	}
	// Validate that payload is parseable JSON to catch agent mistakes early.
	if !json.Valid([]byte(payload)) {
		return mcp.NewToolResultError("payload must be valid JSON (e.g. '{\"key\":\"value\"}')"), nil
	}

	s.upsertAgentIfNeeded(fromAgent)

	msgID, err := s.store.SendMessage(fromAgent, toAgent, topic, payload, projectID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("send message: %v", err)), nil
	}

	// Emit event so agents polling get_events see the message immediately
	// without needing to poll get_messages as well.
	if err := s.store.AppendEvent("agent_message", fromAgent,
		fmt.Sprintf(`{"message_id":%q,"topic":%q,"to_agent":%q,"project_id":%q}`,
			msgID, topic, toAgent, projectID)); err != nil {
		fmt.Fprintf(os.Stderr, "synapses: append agent_message event: %v\n", err)
	}

	audience := "all agents (broadcast)"
	if toAgent != "" {
		audience = fmt.Sprintf("agent %q", toAgent)
	}
	return jsonResult(map[string]interface{}{
		"message_id": msgID,
		"from_agent": fromAgent,
		"to_agent":   toAgent,
		"topic":      topic,
		"audience":   audience,
		"message":    fmt.Sprintf("Message sent to %s. Recipient can retrieve it via get_messages(unread_only=true).", audience),
	})
}

// handleGetMessages retrieves messages visible to an agent from the message bus.
// Visible = directly addressed OR broadcast (to_agent NULL).
func (s *Server) handleGetMessages(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("message bus unavailable: server started without a persistent store"), nil
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
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
		return mcp.NewToolResultError(fmt.Sprintf("get messages: %v", err)), nil
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
	return jsonResult(resp)
}

// handleMarkRead marks a message as read by the calling agent.
// Idempotent: calling it on an already-read message is safe.
func (s *Server) handleMarkRead(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("message bus unavailable: server started without a persistent store"), nil
	}

	messageID := stringArg(req, "message_id")
	if messageID == "" {
		return mcp.NewToolResultError("message_id is required"), nil
	}
	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		return mcp.NewToolResultError("agent_id is required"), nil
	}

	s.upsertAgentIfNeeded(agentID)

	if err := s.store.MarkRead(messageID, agentID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("mark read: %v", err)), nil
	}
	return jsonResult(map[string]interface{}{
		"message_id": messageID,
		"agent_id":   agentID,
		"message":    "Message marked as read.",
	})
}

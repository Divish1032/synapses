package mcp

import (
	"testing"
)

// ── handleSendMessage ─────────────────────────────────────────────────────────

func TestHandleSendMessage_Basic(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSendMessage(ctx, callTool(map[string]any{
		"from_agent": "sender",
		"to_agent":   "receiver",
		"topic":      "review",
		"payload":    `{"note":"please check this"}`,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "message_id")
}

func TestHandleSendMessage_MissingFrom_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSendMessage(ctx, callTool(map[string]any{
		"to_agent": "receiver",
		"topic":    "ping",
		"payload":  "{}",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleSendMessage_MissingTopic_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSendMessage(ctx, callTool(map[string]any{
		"from_agent": "sender",
		"to_agent":   "receiver",
		"payload":    "{}",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleSendMessage_BroadcastNoToAgent(t *testing.T) {
	s := newTestServer(t)
	// Broadcasting: to_agent is empty → requires approval (OF-E3).
	// Step 1: get approval token.
	res, err := s.handleSendMessage(ctx, callTool(map[string]any{
		"from_agent": "broadcaster",
		"to_agent":   "",
		"topic":      "announcement",
		"payload":    `{"msg":"all hands"}`,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "approval_token")
	token := m["approval_token"].(string)

	// Step 2: re-call with approval token.
	res, err = s.handleSendMessage(ctx, callTool(map[string]any{
		"from_agent":     "broadcaster",
		"to_agent":       "",
		"topic":          "announcement",
		"payload":        `{"msg":"all hands"}`,
		"approval_token": token,
	}))
	m = mustResult(t, res, err)
	hasKey(t, m, "message_id")
}

// ── handleGetMessages ─────────────────────────────────────────────────────────

func TestHandleGetMessages_Unread(t *testing.T) {
	s := newTestServer(t)

	// Send a message to "inbox-agent".
	res, err := s.handleSendMessage(ctx, callTool(map[string]any{
		"from_agent": "sender",
		"to_agent":   "inbox-agent",
		"topic":      "work",
		"payload":    `{"task":"do this"}`,
	}))
	mustResult(t, res, err)

	res2, err2 := s.handleGetMessages(ctx, callTool(map[string]any{
		"agent_id":   "inbox-agent",
		"unread_only": true,
	}))
	m2 := mustResult(t, res2, err2)
	hasKey(t, m2, "messages")
	messages, _ := m2["messages"].([]any)
	if len(messages) == 0 {
		t.Error("expected at least 1 unread message for inbox-agent")
	}
}

func TestHandleGetMessages_TopicFilter(t *testing.T) {
	s := newTestServer(t)

	for _, topic := range []string{"alpha", "beta", "alpha"} {
		res, err := s.handleSendMessage(ctx, callTool(map[string]any{
			"from_agent": "src",
			"to_agent":   "dst",
			"topic":      topic,
			"payload":    "{}",
		}))
		mustResult(t, res, err)
	}

	res, err := s.handleGetMessages(ctx, callTool(map[string]any{
		"agent_id":     "dst",
		"topic_filter": "alpha",
		"unread_only":  false,
	}))
	m := mustResult(t, res, err)
	messages, _ := m["messages"].([]any)
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		if topic, _ := msg["topic"].(string); topic != "alpha" {
			t.Errorf("topic filter returned message with topic=%q", topic)
		}
	}
}

func TestHandleGetMessages_MissingAgentID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetMessages(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleMarkRead ────────────────────────────────────────────────────────────

func TestHandleMarkRead_MarksMessage(t *testing.T) {
	s := newTestServer(t)

	// Send a message and get its ID.
	res, err := s.handleSendMessage(ctx, callTool(map[string]any{
		"from_agent": "sender",
		"to_agent":   "reader",
		"topic":      "ping",
		"payload":    "{}",
	}))
	m := mustResult(t, res, err)
	msgID, _ := m["message_id"].(string)
	if msgID == "" {
		t.Skip("no message_id returned")
	}

	// Mark it as read.
	res2, err2 := s.handleMarkRead(ctx, callTool(map[string]any{
		"message_id": msgID,
		"agent_id":   "reader",
	}))
	mustResult(t, res2, err2)

	// Unread count should now be 0.
	unread, _ := s.store.CountUnreadMessages("reader")
	if unread != 0 {
		t.Errorf("expected 0 unread after mark_read, got %d", unread)
	}
}

func TestHandleMarkRead_MissingMessageID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleMarkRead(ctx, callTool(map[string]any{"agent_id": "reader"}))
	mustErrorResult(t, res, err)
}

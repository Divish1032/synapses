package mcp

import (
	"context"
	"testing"
	"time"
)

// ── Unit tests for the approvalStore ─────────────────────────────────────────

func TestApprovalStore_RequestAndConsume(t *testing.T) {
	as := newApprovalStore()

	// Request an approval — should return a valid token.
	result := as.requestApproval("broadcast_message", "test broadcast", "agent-1")
	if result == nil {
		t.Fatal("requestApproval returned nil")
	}
	data := mustResult(t, result, nil)

	token, ok := data["approval_token"].(string)
	if !ok || token == "" {
		t.Fatal("expected non-empty approval_token in response")
	}
	if data["requires_approval"] != true {
		t.Error("expected requires_approval=true")
	}
	if data["operation"] != "broadcast_message" {
		t.Errorf("expected operation=broadcast_message, got %v", data["operation"])
	}

	// Consume the token — should succeed.
	if !as.validateAndConsume(token) {
		t.Error("expected valid token to be accepted")
	}

	// Consuming again — should fail (single-use).
	if as.validateAndConsume(token) {
		t.Error("expected consumed token to be rejected on second use")
	}
}

func TestApprovalStore_ExpiredToken(t *testing.T) {
	as := newApprovalStore()

	token := generateToken()
	as.mu.Lock()
	as.approvals[token] = &crossProjectApproval{
		token:     token,
		operation: "test",
		details:   "test",
		agentID:   "agent-1",
		expiresAt: time.Now().Add(-1 * time.Minute), // already expired
	}
	as.mu.Unlock()

	if as.validateAndConsume(token) {
		t.Error("expected expired token to be rejected")
	}
}

func TestApprovalStore_InvalidToken(t *testing.T) {
	as := newApprovalStore()

	if as.validateAndConsume("nonexistent-token") {
		t.Error("expected unknown token to be rejected")
	}
}

func TestApprovalStore_GC(t *testing.T) {
	as := newApprovalStore()

	// Add an expired token.
	as.mu.Lock()
	as.approvals["expired"] = &crossProjectApproval{
		token:     "expired",
		expiresAt: time.Now().Add(-1 * time.Minute),
	}
	// Add a valid token.
	as.approvals["valid"] = &crossProjectApproval{
		token:     "valid",
		expiresAt: time.Now().Add(5 * time.Minute),
	}
	as.mu.Unlock()

	// Request a new approval (triggers GC).
	as.requestApproval("test", "test", "agent-1")

	as.mu.Lock()
	_, expiredExists := as.approvals["expired"]
	_, validExists := as.approvals["valid"]
	as.mu.Unlock()

	if expiredExists {
		t.Error("expected expired token to be GC'd")
	}
	if !validExists {
		t.Error("expected valid token to survive GC")
	}
}

// ── Integration tests for send_message approval gate ─────────────────────────

func TestSendMessage_Broadcast_RequiresApproval(t *testing.T) {
	s := newTestServer(t)

	// Broadcast (no to_agent) without approval_token — should require approval.
	result, err := s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "test-agent",
		"topic":      "api_changed",
	}))
	data := mustResult(t, result, err)

	if data["requires_approval"] != true {
		t.Error("expected broadcast without token to require approval")
	}
	token, ok := data["approval_token"].(string)
	if !ok || token == "" {
		t.Fatal("expected approval_token in response")
	}
	if data["operation"] != "broadcast_message" {
		t.Errorf("expected operation=broadcast_message, got %v", data["operation"])
	}
}

func TestSendMessage_Broadcast_WithValidToken(t *testing.T) {
	s := newTestServer(t)

	// First call: get approval token.
	result, err := s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "test-agent",
		"topic":      "api_changed",
	}))
	data := mustResult(t, result, err)
	token := data["approval_token"].(string)

	// Second call: provide the token — should succeed.
	result, err = s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent":     "test-agent",
		"topic":          "api_changed",
		"approval_token": token,
	}))
	data = mustResult(t, result, err)

	if _, ok := data["message_id"]; !ok {
		t.Error("expected message_id in successful send_message response")
	}
}

func TestSendMessage_Broadcast_ExpiredToken(t *testing.T) {
	s := newTestServer(t)

	// Manually insert an expired token.
	token := generateToken()
	s.approvals.mu.Lock()
	s.approvals.approvals[token] = &crossProjectApproval{
		token:     token,
		operation: "broadcast_message",
		expiresAt: time.Now().Add(-1 * time.Minute),
	}
	s.approvals.mu.Unlock()

	// Call with expired token — should return error.
	result, err := s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent":     "test-agent",
		"topic":          "api_changed",
		"approval_token": token,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Error("expected error result for expired approval token")
	}
}

func TestSendMessage_Broadcast_TokenReuse(t *testing.T) {
	s := newTestServer(t)

	// Get approval token.
	result, _ := s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "test-agent",
		"topic":      "api_changed",
	}))
	data := mustResult(t, result, nil)
	token := data["approval_token"].(string)

	// First use: succeeds.
	result, _ = s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent":     "test-agent",
		"topic":          "api_changed",
		"approval_token": token,
	}))
	data = mustResult(t, result, nil)
	if _, ok := data["message_id"]; !ok {
		t.Fatal("expected first use to succeed")
	}

	// Second use: fails (single-use token).
	result, err := s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent":     "test-agent",
		"topic":          "api_changed",
		"approval_token": token,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Error("expected error on token reuse")
	}
}

func TestSendMessage_Directed_NoApproval(t *testing.T) {
	s := newTestServer(t)

	// Directed message (to_agent set) — should proceed without approval.
	result, err := s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "test-agent",
		"topic":      "api_changed",
		"to_agent":   "other-agent",
	}))
	data := mustResult(t, result, err)

	if _, ok := data["message_id"]; !ok {
		t.Error("expected directed message to succeed without approval")
	}
	if data["requires_approval"] == true {
		t.Error("directed message should not require approval")
	}
}

// ── Integration tests for remember approval gate ─────────────────────────────

func TestRemember_CrossProject_RequiresApproval(t *testing.T) {
	s := newTestServer(t)

	// remember() with project_id different from current project.
	result, err := s.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id":   "test-agent",
		"decision":   "test decision",
		"project_id": "other-project",
	}))
	data := mustResult(t, result, err)

	if data["requires_approval"] != true {
		t.Error("expected cross-project remember to require approval")
	}
	token, ok := data["approval_token"].(string)
	if !ok || token == "" {
		t.Fatal("expected approval_token in response")
	}
	if data["operation"] != "cross_project_remember" {
		t.Errorf("expected operation=cross_project_remember, got %v", data["operation"])
	}
}

func TestRemember_CrossProject_WithValidToken(t *testing.T) {
	s := newTestServer(t)

	// First call: get approval token.
	result, err := s.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id":   "test-agent",
		"decision":   "test decision",
		"project_id": "other-project",
	}))
	data := mustResult(t, result, err)
	token := data["approval_token"].(string)

	// Second call: provide the token — should succeed.
	result, err = s.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id":       "test-agent",
		"decision":       "test decision",
		"project_id":     "other-project",
		"approval_token": token,
	}))
	data = mustResult(t, result, err)

	if _, ok := data["episode_id"]; !ok {
		t.Error("expected episode_id in successful remember response")
	}
}

func TestRemember_SameProject_NoApproval(t *testing.T) {
	s := newTestServer(t)

	// remember() without project_id (uses current project) — no gate.
	result, err := s.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id": "test-agent",
		"decision": "test decision",
	}))
	data := mustResult(t, result, err)

	if _, ok := data["episode_id"]; !ok {
		t.Error("expected same-project remember to succeed without approval")
	}
	if data["requires_approval"] == true {
		t.Error("same-project remember should not require approval")
	}
}

func TestRemember_MatchingProjectID_NoApproval(t *testing.T) {
	s := newTestServer(t)

	// remember() with project_id matching the current project — no gate.
	// The test graph's repoID is "test-repo".
	result, err := s.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id":   "test-agent",
		"decision":   "test decision",
		"project_id": "test-repo",
	}))
	data := mustResult(t, result, err)

	if _, ok := data["episode_id"]; !ok {
		t.Error("expected matching project_id remember to succeed without approval")
	}
	if data["requires_approval"] == true {
		t.Error("matching project_id should not require approval")
	}
}

func TestRemember_CrossProject_InvalidToken(t *testing.T) {
	s := newTestServer(t)

	// Call with an invalid token.
	result, err := s.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id":       "test-agent",
		"decision":       "test decision",
		"project_id":     "other-project",
		"approval_token": "bogus-token",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Error("expected error result for invalid approval token")
	}
}

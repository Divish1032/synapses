package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

// findPendingFor returns the pending approval matching op+agentID.
// Fails the test if not found.
func findPendingFor(t *testing.T, op, agentID string) PendingApproval {
	t.Helper()
	pending, err := ListPendingApprovals()
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	for _, p := range pending {
		if p.Operation == op && p.AgentID == agentID {
			return p
		}
	}
	t.Fatalf("no pending approval for operation=%q agentID=%q (total pending: %d)", op, agentID, len(pending))
	return PendingApproval{}
}

// ── Unit tests for the approvalStore ─────────────────────────────────────────

func TestApprovalStore_RequestDoesNotLeakToken(t *testing.T) {
	as := newApprovalStore()
	result := as.requestApproval("broadcast_message", "test broadcast", "agent-1")
	if result == nil {
		t.Fatal("requestApproval returned nil")
	}
	data := mustResult(t, result, nil)

	// Token must NOT be in the MCP response — that is the core security invariant.
	if _, hasToken := data["approval_token"]; hasToken {
		t.Fatal("approval_token must not appear in the MCP tool response (out-of-band delivery)")
	}
	if data["requires_approval"] != true {
		t.Error("expected requires_approval=true")
	}
	if data["operation"] != "broadcast_message" {
		t.Errorf("expected operation=broadcast_message, got %v", data["operation"])
	}
	hasKey(t, data, "message")
	hasKey(t, data, "ttl_seconds")
}

func TestApprovalStore_RequestWritesFileToApprovalDir(t *testing.T) {
	as := newApprovalStore()
	as.requestApproval("cross_project_remember", "writing to other project", "agent-1")

	// Verify approval file was created.
	pending, err := ListPendingApprovals()
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("expected at least one pending approval file after requestApproval")
	}
	found := false
	for _, p := range pending {
		if p.Operation == "cross_project_remember" && p.AgentID == "agent-1" {
			found = true
			// Verify the file has Approved=false.
			rec, err := readApprovalFile(p.FilePath)
			if err != nil {
				t.Fatalf("readApprovalFile: %v", err)
			}
			if rec.Approved {
				t.Error("newly created approval must not be pre-approved")
			}
		}
	}
	if !found {
		t.Error("expected pending approval for cross_project_remember / agent-1")
	}
}

func TestApprovalStore_CheckAndConsume_NotApproved_ReturnsFalse(t *testing.T) {
	as := newApprovalStore()
	as.requestApproval("broadcast_message", "test", "agent-x")

	// Without user approval, checkAndConsumeApproval must return false.
	if as.checkAndConsumeApproval("broadcast_message", "agent-x") {
		t.Error("expected false: approval exists on disk but has not been approved by user")
	}
}

func TestApprovalStore_CheckAndConsume_AfterApprove_ReturnsTrue(t *testing.T) {
	as := newApprovalStore()
	as.requestApproval("broadcast_message", "test broadcast", "agent-consume-y")

	// Simulate user running `synapses approve` (finds the right approval by op+agent).
	p := findPendingFor(t, "broadcast_message", "agent-consume-y")
	if err := ApproveRequest(p.Token); err != nil {
		t.Fatalf("ApproveRequest: %v", err)
	}

	// Now the agent retries — checkAndConsumeApproval must succeed.
	if !as.checkAndConsumeApproval("broadcast_message", "agent-consume-y") {
		t.Error("expected true: approval was user-approved")
	}
}

func TestApprovalStore_CheckAndConsume_SingleUse(t *testing.T) {
	as := newApprovalStore()
	as.requestApproval("broadcast_message", "test", "agent-single-use-z")

	p := findPendingFor(t, "broadcast_message", "agent-single-use-z")
	_ = ApproveRequest(p.Token)

	// First consume: succeeds.
	if !as.checkAndConsumeApproval("broadcast_message", "agent-single-use-z") {
		t.Error("first consume should succeed")
	}
	// Second consume: fails (file deleted).
	if as.checkAndConsumeApproval("broadcast_message", "agent-single-use-z") {
		t.Error("second consume must fail — approval is single-use")
	}
}

func TestApprovalStore_CheckAndConsume_WrongOperation_ReturnsFalse(t *testing.T) {
	as := newApprovalStore()
	as.requestApproval("cross_project_remember", "test", "agent-wrong-op-a")

	p := findPendingFor(t, "cross_project_remember", "agent-wrong-op-a")
	_ = ApproveRequest(p.Token)
	_ = p

	// Checking with a different operation must return false.
	if as.checkAndConsumeApproval("broadcast_message", "agent-wrong-op-a") {
		t.Error("expected false: operation does not match")
	}
}

func TestApprovalStore_CheckAndConsume_WrongAgent_ReturnsFalse(t *testing.T) {
	as := newApprovalStore()
	as.requestApproval("broadcast_message", "test", "agent-owner-unique")

	p := findPendingFor(t, "broadcast_message", "agent-owner-unique")
	_ = ApproveRequest(p.Token)
	_ = p

	// Checking with a different agentID must return false.
	if as.checkAndConsumeApproval("broadcast_message", "different-agent-unique") {
		t.Error("expected false: agentID does not match")
	}
}

func TestApprovalStore_ExpiredApproval_GCedOnCheck(t *testing.T) {
	// Write an expired approval file directly.
	dir, err := approvalDir()
	if err != nil {
		t.Fatalf("approvalDir: %v", err)
	}
	token := generateToken()
	rec := approvalRecord{
		Token:     token,
		Operation: "broadcast_message",
		Details:   "test",
		AgentID:   "agent-expired",
		ExpiresAt: time.Now().Add(-1 * time.Minute),
		Approved:  true,
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	filePath := filepath.Join(dir, token+".json")
	_ = os.WriteFile(filePath, data, 0o600)

	as := newApprovalStore()
	// checkAndConsumeApproval should NOT consume the expired file.
	if as.checkAndConsumeApproval("broadcast_message", "agent-expired") {
		t.Error("expected false: approval file is expired")
	}
	// File should be cleaned up.
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Error("expected expired approval file to be deleted during GC")
	}
}

func TestApprovalStore_GC_RemovesExpiredFiles(t *testing.T) {
	as := newApprovalStore()

	// Create an expired file directly.
	dir, _ := approvalDir()
	expiredToken := generateToken()
	expired := approvalRecord{
		Token:     expiredToken,
		Operation: "test",
		AgentID:   "gc-test",
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	}
	expiredData, _ := json.MarshalIndent(expired, "", "  ")
	expiredPath := filepath.Join(dir, expiredToken+".json")
	_ = os.WriteFile(expiredPath, expiredData, 0o600)

	// Triggering requestApproval runs gcFilesLocked.
	as.requestApproval("broadcast_message", "gc trigger", "gc-agent")

	// The expired file should be gone.
	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Error("expected expired approval to be GC'd when new approval is requested")
	}
}

func TestApprovalStore_Idempotent_DoesNotDuplicatePending(t *testing.T) {
	as := newApprovalStore()

	// Call requestApproval twice for the same operation+agentID.
	as.requestApproval("broadcast_message", "test", "agent-idem")
	as.requestApproval("broadcast_message", "test again", "agent-idem")

	pending, err := ListPendingApprovals()
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	count := 0
	for _, p := range pending {
		if p.Operation == "broadcast_message" && p.AgentID == "agent-idem" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 pending approval, got %d", count)
	}
}

func TestApproveRequest_InvalidToken_ReturnsError(t *testing.T) {
	err := ApproveRequest("nonexistent-token-xyz-9999")
	if err == nil {
		t.Error("expected error for nonexistent token")
	}
}

func TestApproveRequest_ExpiredToken_ReturnsError(t *testing.T) {
	dir, _ := approvalDir()
	token := generateToken()
	rec := approvalRecord{
		Token:     token,
		Operation: "test",
		AgentID:   "test-agent",
		ExpiresAt: time.Now().Add(-1 * time.Minute), // expired
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, token+".json"), data, 0o600)

	err := ApproveRequest(token)
	if err == nil {
		t.Error("expected error for expired approval")
	}
}

// ── Integration tests for send_message approval gate ─────────────────────────

func TestSendMessage_Broadcast_RequiresApproval(t *testing.T) {
	s := newTestServer(t)
	result, err := s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "test-agent",
		"topic":      "api_changed",
	}))
	data := mustResult(t, result, err)

	if data["requires_approval"] != true {
		t.Error("expected broadcast without prior approval to require approval")
	}
	// Token must NOT leak into MCP response.
	if _, hasToken := data["approval_token"]; hasToken {
		t.Fatal("approval_token must not appear in tool response")
	}
	if data["operation"] != "broadcast_message" {
		t.Errorf("expected operation=broadcast_message, got %v", data["operation"])
	}
}

func TestSendMessage_Broadcast_AfterApprove_Succeeds(t *testing.T) {
	s := newTestServer(t)

	// Step 1: request approval.
	_, _ = s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "approve-test-agent",
		"topic":      "api_changed",
	}))

	// Step 2: user approves via CLI (find the specific pending for this agent).
	p := findPendingFor(t, "broadcast_message", "approve-test-agent")
	if err := ApproveRequest(p.Token); err != nil {
		t.Fatalf("ApproveRequest: %v", err)
	}

	// Step 3: agent retries (no approval_token needed).
	result, err := s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "approve-test-agent",
		"topic":      "api_changed",
	}))
	data := mustResult(t, result, err)

	if _, ok := data["message_id"]; !ok {
		t.Error("expected message_id in successful send_message response")
	}
}

func TestSendMessage_Broadcast_SingleUse(t *testing.T) {
	s := newTestServer(t)

	// Get and approve.
	_, _ = s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "single-use-agent",
		"topic":      "api_changed",
	}))
	p := findPendingFor(t, "broadcast_message", "single-use-agent")
	_ = ApproveRequest(p.Token)

	// First retry: succeeds.
	result, _ := s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "single-use-agent",
		"topic":      "api_changed",
	}))
	data := mustResult(t, result, nil)
	if _, ok := data["message_id"]; !ok {
		t.Fatal("first retry should succeed")
	}

	// Second retry: needs a new approval (single-use consumed above).
	result, err := s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "single-use-agent",
		"topic":      "api_changed",
	}))
	data2 := mustResult(t, result, err)
	if data2["requires_approval"] != true {
		t.Error("second retry should require a new approval (prior approval was consumed)")
	}
}

func TestSendMessage_Directed_NoApproval(t *testing.T) {
	s := newTestServer(t)
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

func TestSendMessage_WhitespaceToAgent_TreatedAsBroadcast(t *testing.T) {
	s := newTestServer(t)
	result, err := s.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "test-agent",
		"topic":      "test",
		"to_agent":   "   ",
	}))
	data := mustResult(t, result, err)
	if data["requires_approval"] != true {
		t.Error("whitespace-only to_agent should be treated as broadcast and require approval")
	}
}

// ── Integration tests for remember approval gate ─────────────────────────────

func TestRemember_CrossProject_RequiresApproval(t *testing.T) {
	s := newTestServer(t)
	result, err := s.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id":   "test-agent",
		"decision":   "test decision",
		"project_id": "other-project",
	}))
	data := mustResult(t, result, err)
	if data["requires_approval"] != true {
		t.Error("expected cross-project remember to require approval")
	}
	if _, hasToken := data["approval_token"]; hasToken {
		t.Fatal("approval_token must not appear in tool response")
	}
	if data["operation"] != "cross_project_remember" {
		t.Errorf("expected operation=cross_project_remember, got %v", data["operation"])
	}
}

func TestRemember_CrossProject_AfterApprove_Succeeds(t *testing.T) {
	s := newTestServer(t)

	// Step 1: request approval.
	_, _ = s.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id":   "remember-approve-agent",
		"decision":   "test decision",
		"project_id": "other-project",
	}))

	// Step 2: user approves.
	p := findPendingFor(t, "cross_project_remember", "remember-approve-agent")
	if err := ApproveRequest(p.Token); err != nil {
		t.Fatalf("ApproveRequest: %v", err)
	}

	// Step 3: agent retries.
	result, err := s.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id":   "remember-approve-agent",
		"decision":   "test decision",
		"project_id": "other-project",
	}))
	data := mustResult(t, result, err)
	if _, ok := data["episode_id"]; !ok {
		t.Error("expected episode_id in successful remember response")
	}
}

func TestRemember_SameProject_NoApproval(t *testing.T) {
	s := newTestServer(t)
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

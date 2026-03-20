package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// approvalTTL is the time-to-live for cross-project write approval tokens.
// After this duration, the token is expired and must be re-requested.
const approvalTTL = 5 * time.Minute

// crossProjectApproval represents a pending approval for a cross-project write.
type crossProjectApproval struct {
	token     string
	operation string // "broadcast_message" | "cross_project_remember"
	details   string // human-readable description
	agentID   string
	expiresAt time.Time
}

// approvalStore manages cross-project write approval tokens.
// All tokens are in-memory and session-scoped — they do not survive restart.
type approvalStore struct {
	mu        sync.Mutex
	approvals map[string]*crossProjectApproval // token → approval
}

func newApprovalStore() *approvalStore {
	return &approvalStore{
		approvals: make(map[string]*crossProjectApproval),
	}
}

// generateToken creates a cryptographically random hex token.
func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use timestamp (extremely unlikely to reach here).
		return fmt.Sprintf("approval-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// requestApproval creates a new approval token and returns a tool result
// indicating that user approval is required before proceeding.
func (a *approvalStore) requestApproval(operation, details, agentID string) *mcp.CallToolResult {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Lazy GC: remove expired tokens.
	a.gcLocked()

	token := generateToken()
	a.approvals[token] = &crossProjectApproval{
		token:     token,
		operation: operation,
		details:   details,
		agentID:   agentID,
		expiresAt: time.Now().Add(approvalTTL),
	}

	return toolResult(map[string]interface{}{
		"requires_approval": true,
		"approval_token":    token,
		"operation":         operation,
		"details":           details,
		"ttl_seconds":       int(approvalTTL.Seconds()),
		"message": fmt.Sprintf(
			"Cross-project write requires user approval. "+
				"Operation: %s. Details: %s. "+
				"Show this to the user and re-call with approval_token=%q after confirmation. "+
				"Token expires in %d minutes.",
			operation, details, token, int(approvalTTL.Minutes()),
		),
	})
}

// validateAndConsume checks whether the given token is valid and not expired.
// If valid, it consumes the token (single-use) and returns true.
// If invalid or expired, returns false.
func (a *approvalStore) validateAndConsume(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	approval, ok := a.approvals[token]
	if !ok {
		return false
	}

	if time.Now().After(approval.expiresAt) {
		delete(a.approvals, token)
		return false
	}

	// Consume: single-use token.
	delete(a.approvals, token)

	// Audit trail.
	fmt.Fprintf(os.Stderr, "synapses: cross-project write approved — op=%s agent=%s details=%s\n",
		approval.operation, approval.agentID, approval.details)

	return true
}

// gcLocked removes expired tokens. Must be called with a.mu held.
func (a *approvalStore) gcLocked() {
	now := time.Now()
	for token, approval := range a.approvals {
		if now.After(approval.expiresAt) {
			delete(a.approvals, token)
		}
	}
}

// toolResult wraps jsonResult for approval responses.
// Uses isError=false because approval-needed is informational, not an error.
func toolResult(data map[string]interface{}) *mcp.CallToolResult {
	res, _ := jsonResult(data)
	return res
}

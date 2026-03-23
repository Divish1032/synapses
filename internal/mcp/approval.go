package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// approvalTTL is the time-to-live for cross-project write approvals.
const approvalTTL = 5 * time.Minute

// approvalRecord is the JSON structure persisted to disk for each pending approval.
// The token lives ONLY on disk — it is never returned in MCP tool responses.
// This prevents a prompt-injected agent from self-approving by receiving and
// immediately resubmitting the token.
type approvalRecord struct {
	Token     string    `json:"token"`
	Operation string    `json:"operation"`
	Details   string    `json:"details"`
	AgentID   string    `json:"agent_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Approved  bool      `json:"approved"`
}

// PendingApproval is the public view of a pending approval, surfaced by the CLI.
type PendingApproval struct {
	Token     string
	Operation string
	Details   string
	AgentID   string
	ExpiresAt time.Time
	FilePath  string
}

// approvalStore manages cross-project write approvals using file-based out-of-band delivery.
// Approvals are written to ~/.synapses/approvals/<token>.json and approved by the user
// running `synapses approve` in their terminal. Tokens are never returned in MCP responses.
type approvalStore struct {
	mu sync.Mutex
}

func newApprovalStore() *approvalStore {
	return &approvalStore{}
}

// approvalDir returns the path to the approvals directory, ensuring it exists.
// Respects SYNAPSES_CACHE_DIR for test isolation (same pattern as the store).
func approvalDir() (string, error) {
	base := os.Getenv("SYNAPSES_CACHE_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".synapses")
	}
	dir := filepath.Join(base, "approvals")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create approvals dir: %w", err)
	}
	return dir, nil
}

// generateToken creates a cryptographically random hex token.
func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("approval-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// requestApproval creates a pending approval file on disk and returns a tool result
// instructing the agent to ask the user to run `synapses approve`.
// The approval token is NOT included in the MCP response — it lives only in the
// approval file, so only the human user (not the agent) can retrieve and act on it.
func (a *approvalStore) requestApproval(operation, details, agentID string) *mcpgo.CallToolResult {
	a.mu.Lock()
	defer a.mu.Unlock()

	// GC stale files before writing a new one.
	a.gcFilesLocked()

	dir, err := approvalDir()
	if err != nil {
		return mcpgo.NewToolResultError(
			fmt.Sprintf("cross-project write blocked: cannot create approvals directory: %v", stripInternalPaths(err.Error())),
		)
	}

	// Idempotent: if there is already a pending (not yet approved, not expired) approval
	// for this operation+agentID, do not create a duplicate.
	if a.hasPendingLocked(dir, operation, agentID) {
		return toolResult(map[string]interface{}{
			"requires_approval": true,
			"operation":         operation,
			"details":           details,
			"ttl_seconds":       int(approvalTTL.Seconds()),
			"message": fmt.Sprintf(
				"A cross-project approval for %q is already pending. "+
					"Ask the user to run `synapses approve` in their terminal, then retry.",
				operation,
			),
		})
	}

	token := generateToken()
	rec := approvalRecord{
		Token:     token,
		Operation: operation,
		Details:   details,
		AgentID:   agentID,
		ExpiresAt: time.Now().Add(approvalTTL),
		Approved:  false,
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	filePath := filepath.Join(dir, token+".json")
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		return mcpgo.NewToolResultError(
			fmt.Sprintf("cross-project write blocked: cannot write approval file: %v", stripInternalPaths(err.Error())),
		)
	}

	return toolResult(map[string]interface{}{
		"requires_approval": true,
		"operation":         operation,
		"details":           details,
		"ttl_seconds":       int(approvalTTL.Seconds()),
		"message": fmt.Sprintf(
			"Cross-project write requires user approval. "+
				"Operation: %s. Details: %s. "+
				"Ask the user to run `synapses approve` in their terminal to review and approve. "+
				"Then retry this operation. Approval expires in %d minutes.",
			operation, details, int(approvalTTL.Minutes()),
		),
	})
}

// checkAndConsumeApproval scans the approvals directory for an approved (user-confirmed)
// pending approval matching the given operation and agentID. If found, the file is
// deleted (single-use) and true is returned. Returns false otherwise.
func (a *approvalStore) checkAndConsumeApproval(operation, agentID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	dir, err := approvalDir()
	if err != nil {
		return false
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		rec, err := readApprovalFile(filePath)
		if err != nil {
			continue
		}
		// Expire stale files.
		if now.After(rec.ExpiresAt) {
			os.Remove(filePath) //nolint:errcheck
			continue
		}
		if rec.Operation == operation && rec.AgentID == agentID && rec.Approved {
			// Single-use: consume by deleting.
			os.Remove(filePath) //nolint:errcheck
			logutil.Info("synapses: cross-project write approved — op=%s agent=%s details=%s\n",
				rec.Operation, rec.AgentID, rec.Details)
			return true
		}
	}
	return false
}

// hasPendingLocked reports whether there is already a non-expired, unapproved
// approval for this operation+agentID. Must be called with a.mu held.
func (a *approvalStore) hasPendingLocked(dir, operation, agentID string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		rec, err := readApprovalFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if now.After(rec.ExpiresAt) {
			continue
		}
		if rec.Operation == operation && rec.AgentID == agentID && !rec.Approved {
			return true
		}
	}
	return false
}

// gcFilesLocked removes expired approval files. Must be called with a.mu held.
func (a *approvalStore) gcFilesLocked() {
	dir, err := approvalDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		rec, err := readApprovalFile(filePath)
		if err != nil {
			continue
		}
		if now.After(rec.ExpiresAt) {
			os.Remove(filePath) //nolint:errcheck
		}
	}
}

// readApprovalFile reads and unmarshals a single approval file.
func readApprovalFile(path string) (approvalRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return approvalRecord{}, err
	}
	var rec approvalRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return approvalRecord{}, err
	}
	return rec, nil
}

// ListPendingApprovals returns all non-expired, unapproved approvals from disk.
// Used by the `synapses approve` CLI command.
func ListPendingApprovals() ([]PendingApproval, error) {
	dir, err := approvalDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	now := time.Now()
	var out []PendingApproval
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		rec, err := readApprovalFile(filePath)
		if err != nil {
			continue
		}
		if now.After(rec.ExpiresAt) {
			os.Remove(filePath) //nolint:errcheck
			continue
		}
		if !rec.Approved {
			out = append(out, PendingApproval{
				Token:     rec.Token,
				Operation: rec.Operation,
				Details:   rec.Details,
				AgentID:   rec.AgentID,
				ExpiresAt: rec.ExpiresAt,
				FilePath:  filePath,
			})
		}
	}
	return out, nil
}

// ApproveRequest marks the approval with the given token as approved on disk.
// The token is read from disk by `synapses approve` — it is never passed through agents.
func ApproveRequest(token string) error {
	if filepath.Base(token) != token || strings.ContainsAny(token, `/\`) || strings.Contains(token, "..") || strings.IndexByte(token, 0) >= 0 {
		return fmt.Errorf("invalid token")
	}
	dir, err := approvalDir()
	if err != nil {
		return err
	}
	filePath := filepath.Join(dir, token+".json")
	rec, err := readApprovalFile(filePath)
	if err != nil {
		return fmt.Errorf("approval not found: %w", err)
	}
	if time.Now().After(rec.ExpiresAt) {
		os.Remove(filePath) //nolint:errcheck
		return fmt.Errorf("approval has expired")
	}
	rec.Approved = true
	updated, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, updated, 0o600)
}

// toolResult wraps jsonResult for approval responses.
// Uses isError=false because approval-needed is informational, not an error.
func toolResult(data map[string]interface{}) *mcpgo.CallToolResult {
	res, _ := jsonResult(data)
	return res
}

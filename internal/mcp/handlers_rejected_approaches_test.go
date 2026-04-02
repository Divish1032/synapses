package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestHandleAbandon_Create verifies that creating a rejected approach returns
// a rejected_approach_id and a confirmation message.
func TestHandleAbandon_Create(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":         "abandon",
		"agent_id":       "tester",
		"approach":       "Implement caching with Redis for session storage",
		"failure_reason": "Redis not available in the deployment environment",
		"blocker":        "dial tcp: connection refused on port 6379",
		"context":        "Adding session management to /api/auth",
	}))
	m := mustResult(t, result, err)

	if id, _ := m["rejected_approach_id"].(string); id == "" {
		t.Error("expected non-empty rejected_approach_id")
	}
	if approach, _ := m["approach"].(string); approach != "Implement caching with Redis for session storage" {
		t.Errorf("expected approach echoed back, got %q", approach)
	}
	if msg, _ := m["message"].(string); !strings.Contains(msg, "Rejected approach recorded") {
		t.Errorf("unexpected confirmation message: %q", msg)
	}
}

// TestHandleAbandon_MinimalFields verifies that only agent_id, approach, and
// failure_reason are required — blocker and context are optional.
func TestHandleAbandon_MinimalFields(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":         "abandon",
		"agent_id":       "tester",
		"approach":       "Use table-scan queries for search",
		"failure_reason": "Too slow — full-table scan on 10M rows is unacceptable",
	}))
	m := mustResult(t, result, err)

	if id, _ := m["rejected_approach_id"].(string); id == "" {
		t.Error("expected non-empty rejected_approach_id for minimal record")
	}
}

// TestHandleAbandon_MissingAgentID verifies that omitting agent_id returns an error.
func TestHandleAbandon_MissingAgentID(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":         "abandon",
		"approach":       "something tried",
		"failure_reason": "it failed",
	}))
	if result == nil || !result.IsError {
		t.Error("expected tool error for missing agent_id")
	}
}

// TestHandleAbandon_MissingApproach verifies that omitting approach returns an error.
func TestHandleAbandon_MissingApproach(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":         "abandon",
		"agent_id":       "tester",
		"failure_reason": "it failed",
	}))
	if result == nil || !result.IsError {
		t.Error("expected tool error for missing approach")
	}
}

// TestHandleAbandon_MissingFailureReason verifies that omitting failure_reason
// returns an error.
func TestHandleAbandon_MissingFailureReason(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "abandon",
		"agent_id": "tester",
		"approach": "something tried",
	}))
	if result == nil || !result.IsError {
		t.Error("expected tool error for missing failure_reason")
	}
}

// TestHandleAbandon_NilStore verifies that a nil store returns a tool error,
// not a panic.
func TestHandleAbandon_NilStore(t *testing.T) {
	srv := newTestServer(t)
	srv.store = nil

	result, err := srv.handleAbandon(ctx, callTool(map[string]any{
		"agent_id":       "tester",
		"approach":       "test",
		"failure_reason": "because",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Error("expected tool error result when store is nil")
	}
}

// TestHandleListRejectedApproaches_Basic verifies that rejected approaches
// recorded via handleAbandon are returned by handleListRejectedApproaches.
func TestHandleListRejectedApproaches_Basic(t *testing.T) {
	srv := newTestServer(t)

	// Record two rejected approaches.
	_, _ = srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":         "abandon",
		"agent_id":       "tester",
		"approach":       "Use Redis for caching",
		"failure_reason": "Redis not available",
	}))
	_, _ = srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":         "abandon",
		"agent_id":       "tester",
		"approach":       "Store sessions in cookies",
		"failure_reason": "Cookie size limit exceeded (4KB)",
	}))

	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "list_rejected",
		"agent_id": "tester",
	}))
	m := mustResult(t, result, err)

	approaches, ok := m["rejected_approaches"].([]interface{})
	if !ok {
		t.Fatalf("expected rejected_approaches array, got %T", m["rejected_approaches"])
	}
	if len(approaches) != 2 {
		t.Errorf("expected 2 rejected approaches, got %d", len(approaches))
	}
	if count, _ := m["count"].(float64); int(count) != 2 {
		t.Errorf("expected count=2, got %v", m["count"])
	}
}

// TestHandleListRejectedApproaches_Search verifies keyword filtering.
func TestHandleListRejectedApproaches_Search(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":         "abandon",
		"agent_id":       "tester",
		"approach":       "Use Redis for caching",
		"failure_reason": "Redis not available",
	}))
	_, _ = srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":         "abandon",
		"agent_id":       "tester",
		"approach":       "Store sessions in PostgreSQL",
		"failure_reason": "Too slow under load",
	}))

	// Search "Redis" — should return only the first.
	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "list_rejected",
		"agent_id": "tester",
		"query":    "Redis",
	}))
	m := mustResult(t, result, err)

	approaches, _ := m["rejected_approaches"].([]interface{})
	if len(approaches) != 1 {
		t.Errorf("expected 1 Redis match, got %d", len(approaches))
	}
	first, _ := approaches[0].(map[string]interface{})
	if app, _ := first["approach"].(string); app != "Use Redis for caching" {
		t.Errorf("expected Redis approach, got %q", app)
	}
}

// TestHandleListRejectedApproaches_NilStore verifies nil store returns error.
func TestHandleListRejectedApproaches_NilStore(t *testing.T) {
	srv := newTestServer(t)
	srv.store = nil

	result, err := srv.handleListRejectedApproaches(ctx, callTool(map[string]any{
		"agent_id": "tester",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Error("expected tool error result when store is nil")
	}
}

// TestHandleListRejectedApproaches_MissingAgentID verifies error on missing
// agent_id.
func TestHandleListRejectedApproaches_MissingAgentID(t *testing.T) {
	srv := newTestServer(t)

	result, _ := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action": "list_rejected",
	}))
	if result == nil || !result.IsError {
		t.Error("expected tool error for missing agent_id")
	}
}

// TestBuildCompactionRecovery_IncludesRejectedApproaches verifies that
// rejected approaches recorded via InsertRejectedApproach appear in the
// compaction recovery packet under "rejected_approaches", newest-first,
// up to 3 entries.
func TestBuildCompactionRecovery_IncludesRejectedApproaches(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "rej-agent",
		"scope":    "standard",
	}))
	sessionID := srv.getSynapseSessionID(SessionIDFromContext(ctx))
	if sessionID == "" {
		t.Skip("no session ID — skipping compaction recovery test")
	}

	base := time.Now().Unix()
	// Insert 3 rejected approaches with distinct timestamps.
	for i, approach := range []string{
		"Use Redis for caching",
		"Implement full-text search with LIKE queries",
		"Store binary blobs in SQLite",
	} {
		_, err := srv.store.InsertRejectedApproach(store.RejectedApproach{
			AgentID:       "rej-agent",
			ProjectID:     srv.projectID,
			Approach:      approach,
			FailureReason: "did not work in production",
			CreatedAt:     base + int64(i),
		})
		if err != nil {
			t.Fatalf("InsertRejectedApproach[%d]: %v", i, err)
		}
	}

	recovery := srv.buildCompactionRecovery("rej-agent", sessionID)
	if recovery == nil {
		t.Fatal("expected non-nil recovery packet")
	}

	raw, ok := recovery["rejected_approaches"]
	if !ok {
		t.Fatal("expected rejected_approaches in recovery packet")
	}

	// Assert via JSON round-trip.
	b, jsonErr := json.Marshal(raw)
	if jsonErr != nil {
		t.Fatalf("marshal rejected_approaches: %v", jsonErr)
	}
	var items []map[string]interface{}
	if jsonErr := json.Unmarshal(b, &items); jsonErr != nil {
		t.Fatalf("unmarshal rejected_approaches: %v", jsonErr)
	}
	if len(items) != 3 {
		t.Errorf("expected 3 rejected approaches in recovery, got %d", len(items))
	}
	// Newest first — approach[2] = "Store binary blobs in SQLite"
	if items[0]["approach"] != "Store binary blobs in SQLite" {
		t.Errorf("expected newest approach first, got %v", items[0]["approach"])
	}
	for _, item := range items {
		if _, hasApproach := item["approach"]; !hasApproach {
			t.Error("each rejected approach entry must have 'approach' field")
		}
		if _, hasReason := item["failure_reason"]; !hasReason {
			t.Error("each rejected approach entry must have 'failure_reason' field")
		}
	}
}

// TestHandleAbandon_MessageContainsSummary verifies that the confirmation
// message from handleAbandon includes truncated approach and failure_reason text.
func TestHandleAbandon_MessageContainsSummary(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":         "abandon",
		"agent_id":       "tester",
		"approach":       "Implement a distributed caching layer using Redis Cluster with automatic sharding across 6 nodes",
		"failure_reason": "Redis Cluster not supported in the cloud provider managed Redis offering — would require self-managed setup",
	}))
	m := mustResult(t, result, err)

	msg, _ := m["message"].(string)
	if msg == "" {
		t.Fatal("expected non-empty message")
	}
	// Message must contain "Rejected approach recorded".
	if !strings.Contains(msg, "Rejected approach recorded") {
		t.Errorf("expected 'Rejected approach recorded' in message, got: %q", msg)
	}
	// Message must not exceed a reasonable length (truncation applied).
	if len(msg) > 300 {
		t.Errorf("message too long (%d chars) — truncation may not be applied", len(msg))
	}
}

// TestHandleSessionInit_IncludesRejectedApproaches verifies that rejected
// approaches are surfaced in the session_init response under "rejected_approaches"
// when they exist for the agent and project.
func TestHandleSessionInit_IncludesRejectedApproaches(t *testing.T) {
	srv := newTestServer(t)

	// Seed a rejected approach directly.
	_, err := srv.store.InsertRejectedApproach(store.RejectedApproach{
		AgentID:       "rej-init-agent",
		ProjectID:     srv.projectID,
		Approach:      "Use full-text search via LIKE queries",
		FailureReason: "Performance degraded to 5s on 10M rows",
		Blocker:       "no index on text column",
	})
	if err != nil {
		t.Fatalf("InsertRejectedApproach: %v", err)
	}

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "rej-init-agent",
		"scope":    "full",
	}))
	m := mustResult(t, res, err)

	rejSection, ok := m["rejected_approaches"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected rejected_approaches map in session_init response, got %T", m["rejected_approaches"])
	}
	count, _ := rejSection["count"].(float64)
	if int(count) != 1 {
		t.Errorf("expected count=1, got %v", rejSection["count"])
	}
	entries, _ := rejSection["entries"].([]interface{})
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	entry, _ := entries[0].(map[string]interface{})
	if app, _ := entry["approach"].(string); app != "Use full-text search via LIKE queries" {
		t.Errorf("unexpected approach: %q", app)
	}
	// Warning must be present.
	if _, hasWarning := rejSection["warning"]; !hasWarning {
		t.Error("expected 'warning' field in rejected_approaches section")
	}
}

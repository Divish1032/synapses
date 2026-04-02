package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// ── compactDetectState unit tests ─────────────────────────────────────────────

// TestCompactDetectState_ShouldInject_Initially verifies a fresh state allows
// injection (no cooldown applied when never injected).
func TestCompactDetectState_ShouldInject_Initially(t *testing.T) {
	s := &compactDetectState{explored: make(map[string]struct{})}
	if !s.shouldInject() {
		t.Error("expected shouldInject=true on fresh state")
	}
}

// TestCompactDetectState_Cooldown verifies that marking injected suppresses
// subsequent injection attempts within the cooldown window.
func TestCompactDetectState_Cooldown(t *testing.T) {
	s := &compactDetectState{explored: make(map[string]struct{})}
	s.markInjected()
	if s.shouldInject() {
		t.Error("expected shouldInject=false immediately after markInjected")
	}
}

// TestCompactDetectState_CooldownExpiry verifies injection is allowed again
// after the cooldown has elapsed (backdate injectedAt).
func TestCompactDetectState_CooldownExpiry(t *testing.T) {
	s := &compactDetectState{explored: make(map[string]struct{})}
	// Backdate injection so the cooldown has expired.
	s.injectedAt = time.Now().Add(-(compactCooldown + time.Second))
	if !s.shouldInject() {
		t.Error("expected shouldInject=true after cooldown expired")
	}
}

// TestCompactDetectState_TryMarkInjected_AtomicLock verifies that tryMarkInjected
// combines the check and the mark in one operation (no race window).
func TestCompactDetectState_TryMarkInjected_AtomicLock(t *testing.T) {
	s := &compactDetectState{explored: make(map[string]struct{})}
	if !s.tryMarkInjected() {
		t.Error("expected tryMarkInjected=true on fresh state")
	}
	if s.tryMarkInjected() {
		t.Error("expected tryMarkInjected=false immediately after first call")
	}
}

// TestCompactDetectState_Unmark verifies that unmarkInjected releases the slot,
// allowing a subsequent tryMarkInjected to succeed (used when recovery is nil).
func TestCompactDetectState_Unmark(t *testing.T) {
	s := &compactDetectState{explored: make(map[string]struct{})}
	s.tryMarkInjected() // reserve slot
	s.unmarkInjected()  // release because recovery was nil
	if !s.tryMarkInjected() {
		t.Error("expected tryMarkInjected=true after unmarkInjected")
	}
}

// TestCompactDetectState_FirstExploration verifies that the first query for an
// entity is NOT a re-exploration (agent is discovering, not re-discovering).
func TestCompactDetectState_FirstExploration(t *testing.T) {
	s := &compactDetectState{explored: make(map[string]struct{})}
	if s.isReExplored("AuthLogin") {
		t.Error("expected isReExplored=false on first exploration")
	}
}

// TestCompactDetectState_ReExploration verifies that a second query for the
// same entity is detected as re-exploration.
func TestCompactDetectState_ReExploration(t *testing.T) {
	s := &compactDetectState{explored: make(map[string]struct{})}
	s.markExplored("AuthLogin")
	if !s.isReExplored("AuthLogin") {
		t.Error("expected isReExplored=true after markExplored")
	}
}

// TestCompactDetectState_EmptyEntity verifies that empty strings are never
// treated as re-explorations (avoids false positives on tools that omit entity).
func TestCompactDetectState_EmptyEntity(t *testing.T) {
	s := &compactDetectState{explored: make(map[string]struct{})}
	s.markExplored("")
	if s.isReExplored("") {
		t.Error("expected isReExplored=false for empty entity")
	}
}

// TestCompactDetectState_IndependentEntities verifies that re-exploration of
// one entity doesn't affect another.
func TestCompactDetectState_IndependentEntities(t *testing.T) {
	s := &compactDetectState{explored: make(map[string]struct{})}
	s.markExplored("EntityA")
	if s.isReExplored("EntityB") {
		t.Error("isReExplored(EntityB) should be false when only EntityA was explored")
	}
}

// ── extractQueryEntity unit tests ─────────────────────────────────────────────

func TestExtractQueryEntity_GetContext(t *testing.T) {
	entity := extractQueryEntity("get_context", map[string]interface{}{"entity": "AuthLogin"})
	if entity != "AuthLogin" {
		t.Errorf("expected 'AuthLogin', got %q", entity)
	}
}

func TestExtractQueryEntity_Search(t *testing.T) {
	entity := extractQueryEntity("search", map[string]interface{}{"query": "handlePayment"})
	if entity != "handlePayment" {
		t.Errorf("expected 'handlePayment', got %q", entity)
	}
}

func TestExtractQueryEntity_OtherTool(t *testing.T) {
	entity := extractQueryEntity("validate", map[string]interface{}{"entity": "should-ignore"})
	if entity != "" {
		t.Errorf("expected empty string for non-exploration tool, got %q", entity)
	}
}

// ── injectCompactionRecovery unit tests ───────────────────────────────────────

// TestInjectCompactionRecovery_AppendsBlock verifies that a recovery packet is
// appended as a new content block, preserving the original block.
func TestInjectCompactionRecovery_AppendsBlock(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{mcp.NewTextContent(`{"status":"ok"}`)},
	}
	recovery := map[string]interface{}{
		"work_summary": "Working on auth module",
		"hint":         "Context was lost",
	}
	injectCompactionRecovery(result, recovery, "re-init")

	if len(result.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(result.Content))
	}
	// Original block must be untouched.
	if tc, ok := result.Content[0].(mcp.TextContent); !ok || tc.Text != `{"status":"ok"}` {
		t.Error("original content block was modified")
	}
	// Second block must contain recovery label and signal.
	second, ok := result.Content[1].(mcp.TextContent)
	if !ok {
		t.Fatal("expected TextContent in second block")
	}
	if !strings.Contains(second.Text, "re-init") {
		t.Error("recovery block should contain signal label 're-init'")
	}
	if !strings.Contains(second.Text, "work_summary") {
		t.Error("recovery block should contain recovery JSON keys")
	}
}

// TestInjectCompactionRecovery_NilResult verifies nil result is handled safely.
func TestInjectCompactionRecovery_NilResult(t *testing.T) {
	// Should not panic.
	injectCompactionRecovery(nil, map[string]interface{}{"x": "y"}, "re-init")
}

// TestInjectCompactionRecovery_EmptyRecovery verifies empty map is skipped.
func TestInjectCompactionRecovery_EmptyRecovery(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{mcp.NewTextContent(`{}`)},
	}
	injectCompactionRecovery(result, map[string]interface{}{}, "re-init")
	if len(result.Content) != 1 {
		t.Error("expected no block appended for empty recovery")
	}
}

// ── getCompactDetectState unit tests ─────────────────────────────────────────

// TestGetCompactDetectState_CreatesOnDemand verifies that the server creates a
// new state entry for an unknown session rather than returning nil.
func TestGetCompactDetectState_CreatesOnDemand(t *testing.T) {
	srv := newTestServer(t)
	cs := srv.getCompactDetectState("session-xyz")
	if cs == nil {
		t.Fatal("expected non-nil compactDetectState")
	}
}

// TestGetCompactDetectState_SameInstance verifies two calls for the same session
// return the same in-memory state (not independent copies).
func TestGetCompactDetectState_SameInstance(t *testing.T) {
	srv := newTestServer(t)
	cs1 := srv.getCompactDetectState("session-abc")
	cs1.markExplored("Foo")
	cs2 := srv.getCompactDetectState("session-abc")
	if !cs2.isReExplored("Foo") {
		t.Error("expected same state instance for the same session ID")
	}
}

// ── Signal 1: re-init integration test ───────────────────────────────────────

// TestSignal1_ReInit_InjectsRecovery verifies that calling session_init a second
// time on the same connection (Phase 1 resume) triggers compaction recovery.
func TestSignal1_ReInit_InjectsRecovery(t *testing.T) {
	srv := newTestServer(t)

	// First call — creates a new session.
	res1, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "reinit-agent",
		"scope":    "standard",
	}))
	if err != nil {
		t.Fatalf("first session_init: %v", err)
	}
	if res1 == nil {
		t.Fatal("nil result from first session_init")
	}

	// Verify no recovery on fresh session.
	var m1 map[string]any
	if len(res1.Content) > 0 {
		if tc, ok := res1.Content[0].(mcp.TextContent); ok {
			_ = json.Unmarshal([]byte(tc.Text), &m1)
		}
	}
	if _, hasRecovery := m1["compaction_recovery"]; hasRecovery {
		t.Error("first session_init should not inject recovery")
	}

	// Second call on the same connection (same ctx → same MCP session ID "stdio").
	// GetOrResumeSession returns resumed=true, hibernateCtx=nil → Signal 1 fires.
	res2, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "reinit-agent",
		"scope":    "standard",
	}))
	if err != nil {
		t.Fatalf("second session_init: %v", err)
	}
	if res2 == nil {
		t.Fatal("nil result from second session_init")
	}

	// Recovery must appear in the response map OR as an injected text block.
	found := false
	for _, c := range res2.Content {
		tc, ok := c.(mcp.TextContent)
		if !ok {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(tc.Text), &m); err == nil {
			if _, ok := m["compaction_recovery"]; ok {
				found = true
				break
			}
		}
		// Also check for inline injection format.
		if strings.Contains(tc.Text, "Compaction Recovery") && strings.Contains(tc.Text, "re-init") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected compaction_recovery in second session_init response (Signal 1)")
	}
}

// TestSignal1_ReInit_Cooldown verifies that a third session_init call within
// the cooldown window does NOT inject a duplicate recovery.
func TestSignal1_ReInit_Cooldown(t *testing.T) {
	srv := newTestServer(t)

	// Establish session.
	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "cooldown-agent"}))

	// Second call — triggers Signal 1 and marks injectedAt.
	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "cooldown-agent"}))

	// Third call — within cooldown, recovery must NOT appear again.
	res3, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "cooldown-agent",
		"scope":    "standard",
	}))
	if err != nil {
		t.Fatalf("third session_init: %v", err)
	}
	// Check that no recovery appears in content blocks beyond first.
	recoveryCount := 0
	for _, c := range res3.Content {
		tc, ok := c.(mcp.TextContent)
		if !ok {
			continue
		}
		if strings.Contains(tc.Text, "Compaction Recovery") {
			recoveryCount++
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(tc.Text), &m); err == nil {
			if _, ok := m["compaction_recovery"]; ok {
				recoveryCount++
			}
		}
	}
	if recoveryCount > 0 {
		t.Errorf("expected no recovery on third call within cooldown, got %d recovery blocks", recoveryCount)
	}
}

// TestSignal1_ExplicitCompactionMode_NoDoubleInject verifies that when the
// agent explicitly passes scope="compaction", Signal 1 is skipped to avoid
// double-injecting the recovery packet.
func TestSignal1_ExplicitCompactionMode_NoDoubleInject(t *testing.T) {
	srv := newTestServer(t)

	// First call — establish session.
	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "explicit-agent"}))

	// Second call with explicit scope=compaction — existing path handles it.
	// Signal 1 must NOT also fire (would duplicate recovery).
	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "explicit-agent",
		"scope":    "compaction",
	}))
	if err != nil {
		t.Fatalf("compaction session_init: %v", err)
	}

	// Count recovery blocks — should be exactly 1 (from explicit compaction, not Signal 1).
	recoveryCount := 0
	for _, c := range res.Content {
		tc, ok := c.(mcp.TextContent)
		if !ok {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(tc.Text), &m); err == nil {
			if _, ok := m["compaction_recovery"]; ok {
				recoveryCount++
			}
		}
	}
	// Only the explicit compaction path may inject (≤1 block total).
	if recoveryCount > 1 {
		t.Errorf("expected at most 1 recovery block, got %d", recoveryCount)
	}
}

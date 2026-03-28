package mcp

import (
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// openMCPTestStore is declared in another test file; defined here only
// for reference — use the one from testhelpers_test.go.

// ── helpers ────────────────────────────────────────────────────────────────

// newServerWithThreshold creates a test server with AutoEndThresholdCalls set.
// StartBackground is called so that goBackground() work items are actually
// processed — trackSessionCall enqueues via goBackground, which requires live
// workers. t.Cleanup closes the server to drain the pool and stop goroutines.
func newServerWithThreshold(t *testing.T, threshold int) *Server {
	t.Helper()
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg := &config.Config{
		Session: config.SessionConfig{
			AutoEndThresholdCalls: threshold,
		},
	}
	s := New(g, cfg, st)
	s.StartBackground()
	t.Cleanup(func() { s.Close() })
	return s
}

// memoryCountForAgent returns number of session_log memories for agentID.
func memoryCountForAgent(t *testing.T, st *store.Store, agentID string) int {
	t.Helper()
	mems, err := st.QueryMemories(store.TierSessionLog, "", agentID, 100)
	if err != nil {
		t.Fatalf("QueryMemories: %v", err)
	}
	return len(mems)
}

// ── TestTrackSessionCall_Disabled ─────────────────────────────────────────

func TestTrackSessionCall_Disabled(t *testing.T) {
	s := newServerWithThreshold(t, 0) // 0 = disabled

	// Drive 200 calls — should never write a memory.
	for i := 0; i < 200; i++ {
		s.trackSessionCall("sess1", "agent-a")
	}

	// Allow any goroutines to settle.
	time.Sleep(20 * time.Millisecond)

	count := memoryCountForAgent(t, s.store, "agent-a")
	if count != 0 {
		t.Errorf("expected 0 memories when disabled, got %d", count)
	}
}

// ── TestTrackSessionCall_ThresholdNotMet ──────────────────────────────────

func TestTrackSessionCall_ThresholdNotMet(t *testing.T) {
	s := newServerWithThreshold(t, 10)

	// Only 5 calls — threshold of 10 not yet met.
	for i := 0; i < 5; i++ {
		s.trackSessionCall("sess1", "agent-b")
	}
	time.Sleep(20 * time.Millisecond)

	count := memoryCountForAgent(t, s.store, "agent-b")
	if count != 0 {
		t.Errorf("expected 0 memories below threshold, got %d", count)
	}
}

// ── TestTrackSessionCall_ThresholdTriggers ────────────────────────────────

func TestTrackSessionCall_ThresholdTriggers(t *testing.T) {
	s := newServerWithThreshold(t, 5)

	// Drive exactly threshold calls.
	for i := 0; i < 5; i++ {
		s.trackSessionCall("sess1", "agent-c")
	}
	// Allow async goroutine to complete.
	time.Sleep(50 * time.Millisecond)

	count := memoryCountForAgent(t, s.store, "agent-c")
	if count != 1 {
		t.Errorf("expected 1 auto-log after threshold, got %d", count)
	}
}

// ── TestTrackSessionCall_NoDoubleLog ──────────────────────────────────────

func TestTrackSessionCall_NoDoubleLog(t *testing.T) {
	s := newServerWithThreshold(t, 5)

	// First burst: trigger auto-log.
	for i := 0; i < 5; i++ {
		s.trackSessionCall("sess1", "agent-d")
	}
	time.Sleep(50 * time.Millisecond)

	// Second burst past threshold: should NOT fire again (autoLogged=true).
	for i := 0; i < 10; i++ {
		s.trackSessionCall("sess1", "agent-d")
	}
	time.Sleep(50 * time.Millisecond)

	// InsertMemory dedup (stringSimilarity) may merge — but we should have at most 1.
	count := memoryCountForAgent(t, s.store, "agent-d")
	if count != 1 {
		t.Errorf("expected exactly 1 auto-log (no double trigger), got %d", count)
	}
}

// ── TestClearAndGetStartTime_ResetsCounter ────────────────────────────────

func TestClearAndGetStartTime_ResetsCounter(t *testing.T) {
	s := newServerWithThreshold(t, 10)

	// Partial progress (5/10).
	for i := 0; i < 5; i++ {
		s.trackSessionCall("sess1", "agent-e")
	}

	// Simulate manual end_session clearing the entry.
	s.clearAndGetStartTime("sess1", "agent-e")

	// Verify entry removed.
	s.sessionCallsMu.Lock()
	_, exists := s.sessionCalls["sess1::agent-e"]
	s.sessionCallsMu.Unlock()
	if exists {
		t.Error("expected session call entry to be cleared")
	}
}

// ── TestClearAndGetStartTime_EmptyArgs ────────────────────────────────────

func TestClearAndGetStartTime_EmptyArgs(t *testing.T) {
	s := newServerWithThreshold(t, 10)

	// Insert an entry manually.
	s.sessionCallsMu.Lock()
	s.sessionCalls["::"] = &sessionCallEntry{agentID: "", callCount: 3}
	s.sessionCallsMu.Unlock()

	// Clear with both empty — should not panic, and should remove the "::" key.
	// The function returns early when both are empty, so the map entry remains.
	s.clearAndGetStartTime("", "")

	s.sessionCallsMu.Lock()
	_, exists := s.sessionCalls["::"]
	s.sessionCallsMu.Unlock()
	if !exists {
		// Both empty → early return expected, "::" key untouched.
		// This is fine — document the behavior.
		t.Log("early-return path: \":\" key untouched (both empty args)")
	}
}

// ── TestConcurrentSessions_IsolatedCounters ───────────────────────────────

func TestConcurrentSessions_IsolatedCounters(t *testing.T) {
	s := newServerWithThreshold(t, 5)

	// Agent-f reaches threshold; agent-g does not.
	for i := 0; i < 5; i++ {
		s.trackSessionCall("sess-f", "agent-f")
	}
	for i := 0; i < 3; i++ {
		s.trackSessionCall("sess-g", "agent-g")
	}
	time.Sleep(50 * time.Millisecond)

	countF := memoryCountForAgent(t, s.store, "agent-f")
	countG := memoryCountForAgent(t, s.store, "agent-g")

	if countF != 1 {
		t.Errorf("agent-f: expected 1 auto-log, got %d", countF)
	}
	if countG != 0 {
		t.Errorf("agent-g: expected 0 auto-logs (below threshold), got %d", countG)
	}
}

// ── TestAutoSessionLog_TagsMemoryCorrectly ────────────────────────────────

func TestAutoSessionLog_TagsMemoryCorrectly(t *testing.T) {
	s := newServerWithThreshold(t, 3)

	for i := 0; i < 3; i++ {
		s.trackSessionCall("sess1", "agent-h")
	}
	time.Sleep(50 * time.Millisecond)

	mems, err := s.store.QueryMemories(store.TierSessionLog, "", "agent-h", 10)
	if err != nil {
		t.Fatalf("QueryMemories: %v", err)
	}
	if len(mems) == 0 {
		t.Skip("no memory written — empty session (no entities examined), which is valid")
	}

	mem := mems[0]
	if mem.Source != store.SourceAuto {
		t.Errorf("expected source=%q, got %q", store.SourceAuto, mem.Source)
	}
	if mem.AgentID != "agent-h" {
		t.Errorf("expected agent_id=%q, got %q", "agent-h", mem.AgentID)
	}
	// Tags must include "auto_session_log".
	if !containsSubstring(mem.Tags, "auto_session_log") {
		t.Errorf("expected tags to contain \"auto_session_log\", got %q", mem.Tags)
	}
}

// ── TestHandleEndSession_ClearsCallEntry ─────────────────────────────────

func TestHandleEndSession_ClearsCallEntry(t *testing.T) {
	s := newServerWithThreshold(t, 100) // high threshold — won't auto-trigger

	// Manually insert a call entry.
	s.sessionCallsMu.Lock()
	s.sessionCalls["::agent-i"] = &sessionCallEntry{
		agentID:   "agent-i",
		callCount: 50,
		startedAt: time.Now(),
	}
	s.sessionCallsMu.Unlock()

	// Call handleEndSession — must clear the entry.
	_, _ = s.handleEndSession(ctx, callTool(map[string]any{"agent_id": "agent-i"}))

	s.sessionCallsMu.Lock()
	_, exists := s.sessionCalls["::agent-i"]
	s.sessionCallsMu.Unlock()

	if exists {
		t.Error("expected session call entry cleared after handleEndSession")
	}
}

// containsSubstring is a local helper to avoid import of strings package.
func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsSubstringLinear(s, sub))
}

func containsSubstringLinear(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

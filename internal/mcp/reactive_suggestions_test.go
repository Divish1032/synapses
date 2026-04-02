package mcp

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain"
)

func TestSuppressOverused_FiltersFrequentTools(t *testing.T) {
	suggestions := []toolSuggestion{
		{Tool: "get_impact", Reason: "r1"},
		{Tool: "validate", Reason: "r2"},
		{Tool: "get_call_chain", Reason: "r3"},
	}
	counts := map[string]int{"get_impact": 5, "validate": 1}
	got := suppressOverused(suggestions, counts, 4)
	if len(got) != 2 {
		t.Fatalf("expected 2 suggestions, got %d", len(got))
	}
	if got[0].Tool != "validate" {
		t.Errorf("first suggestion should be validate, got %s", got[0].Tool)
	}
	if got[1].Tool != "get_call_chain" {
		t.Errorf("second suggestion should be get_call_chain, got %s", got[1].Tool)
	}
}

func TestSuppressOverused_CapsAtMax(t *testing.T) {
	suggestions := []toolSuggestion{
		{Tool: "a", Reason: "r"},
		{Tool: "b", Reason: "r"},
		{Tool: "c", Reason: "r"},
		{Tool: "d", Reason: "r"},
	}
	got := suppressOverused(suggestions, nil, 2)
	if len(got) != 2 {
		t.Errorf("expected 2, got %d", len(got))
	}
}

func TestSuppressOverused_Empty(t *testing.T) {
	got := suppressOverused(nil, nil, 4)
	if len(got) != 0 {
		t.Errorf("expected 0, got %d", len(got))
	}
}

func TestPhaseSuggestions_Testing(t *testing.T) {
	got := phaseSuggestions(brain.PhaseTesting, "AuthService")
	if len(got) == 0 {
		t.Fatal("expected suggestions for testing phase")
	}
	if got[0].Tool != "validate" {
		t.Errorf("expected validate, got %s", got[0].Tool)
	}
}

func TestPhaseSuggestions_Review(t *testing.T) {
	got := phaseSuggestions(brain.PhaseReview, "AuthService")
	if len(got) == 0 {
		t.Fatal("expected suggestions for review phase")
	}
	if got[0].Tool != "get_impact" {
		t.Errorf("expected get_impact, got %s", got[0].Tool)
	}
}

func TestPhaseSuggestions_Development_NoSuggestions(t *testing.T) {
	got := phaseSuggestions(brain.PhaseDevelopment, "X")
	if len(got) != 0 {
		t.Errorf("expected no suggestions for development phase, got %d", len(got))
	}
}

func TestSuggestAfterValidate(t *testing.T) {
	got := suggestAfterValidate(3, "AuthService")
	if len(got) != 1 || got[0].Tool != "get_context" {
		t.Errorf("expected get_context suggestion, got %v", got)
	}
}

func TestSuggestAfterValidate_NoViolations(t *testing.T) {
	got := suggestAfterValidate(0, "AuthService")
	if len(got) != 0 {
		t.Errorf("expected no suggestions, got %d", len(got))
	}
}

func TestSuggestAfterSearch(t *testing.T) {
	got := suggestAfterSearch("AuthService", 5)
	if len(got) != 1 || got[0].Tool != "get_context" {
		t.Errorf("expected get_context suggestion, got %v", got)
	}
}

func TestSuggestAfterSearch_NoResults(t *testing.T) {
	got := suggestAfterSearch("", 0)
	if len(got) != 0 {
		t.Errorf("expected no suggestions, got %d", len(got))
	}
}

func TestSessionToolTracker(t *testing.T) {
	tracker := newSessionToolTracker()

	tracker.record("s1", "get_context")
	tracker.record("s1", "get_context")
	tracker.record("s1", "validate")

	counts := tracker.get("s1")
	if counts["get_context"] != 2 {
		t.Errorf("expected 2 get_context calls, got %d", counts["get_context"])
	}
	if counts["validate"] != 1 {
		t.Errorf("expected 1 validate call, got %d", counts["validate"])
	}
	if tracker.totalCalls("s1") != 3 {
		t.Errorf("expected 3 total calls, got %d", tracker.totalCalls("s1"))
	}

	tracker.clear("s1")
	if tracker.totalCalls("s1") != 0 {
		t.Error("expected 0 after clear")
	}
}

func TestSessionToolTracker_MetaToolsExcluded(t *testing.T) {
	tracker := newSessionToolTracker()

	// Sprint 23.9: get_compaction_guide removed from metaTools.
	tracker.record("s1", "session_init")
	tracker.record("s1", "end_session")
	tracker.record("s1", "get_context") // only this should count

	if tracker.totalCalls("s1") != 1 {
		t.Errorf("meta tools should be excluded, expected 1 call, got %d", tracker.totalCalls("s1"))
	}
}

func TestSessionToolTracker_IsolatedSessions(t *testing.T) {
	tracker := newSessionToolTracker()
	tracker.record("s1", "a")
	tracker.record("s2", "b")

	if tracker.totalCalls("s1") != 1 || tracker.totalCalls("s2") != 1 {
		t.Error("sessions should be isolated")
	}

	tracker.clear("s1")
	if tracker.totalCalls("s2") != 1 {
		t.Error("clearing s1 should not affect s2")
	}
}

// ── memorySaveNudger tests (Sprint 24.7) ──────────────────────────────────────

func makeNudgerArgs(action string) map[string]any {
	if action == "" {
		return nil
	}
	return map[string]any{"action": action}
}

// callN calls a non-memory tool N times and returns messages from each call.
func callNonMemory(n *memorySaveNudger, sessionID string, count int) []string {
	var msgs []string
	for i := 0; i < count; i++ {
		msg := n.record(sessionID, "get_context", nil)
		msgs = append(msgs, msg)
	}
	return msgs
}

func TestMemorySaveNudger_FiresAtThreshold(t *testing.T) {
	n := newMemorySaveNudger()
	msgs := callNonMemory(n, "s1", saveNudgeThreshold)
	// Only the 10th call should produce a nudge.
	for i, msg := range msgs[:saveNudgeThreshold-1] {
		if msg != "" {
			t.Errorf("call %d should not nudge, got: %q", i+1, msg)
		}
	}
	if msgs[saveNudgeThreshold-1] == "" {
		t.Error("expected nudge on 10th call, got empty string")
	}
}

func TestMemorySaveNudger_MetaToolsExcluded(t *testing.T) {
	n := newMemorySaveNudger()
	// Fill up with meta tools — should never nudge.
	for i := 0; i < saveNudgeThreshold*3; i++ {
		msg := n.record("s1", "session_init", nil)
		if msg != "" {
			t.Errorf("meta tool should never nudge, got: %q", msg)
		}
		msg = n.record("s1", "end_session", nil)
		if msg != "" {
			t.Errorf("meta tool should never nudge, got: %q", msg)
		}
	}
}

func TestMemorySaveNudger_ResetOnSave(t *testing.T) {
	n := newMemorySaveNudger()
	// Make 5 calls, then save — counter should reset.
	callNonMemory(n, "s1", saveNudgeThreshold/2)
	n.record("s1", "memory", makeNudgerArgs("save"))
	// Another saveNudgeThreshold-1 calls should NOT nudge (counter was reset).
	msgs := callNonMemory(n, "s1", saveNudgeThreshold-1)
	for i, msg := range msgs {
		if msg != "" {
			t.Errorf("call %d after save should not nudge, got: %q", i+1, msg)
		}
	}
	// The saveNudgeThreshold-th call after the save should nudge.
	msg := n.record("s1", "get_context", nil)
	if msg == "" {
		t.Error("expected nudge after saveNudgeThreshold calls post-save, got empty string")
	}
}

func TestMemorySaveNudger_Suppressible(t *testing.T) {
	n := newMemorySaveNudger()
	// First nudge fires at call 10.
	callNonMemory(n, "s1", saveNudgeThreshold)
	// Calls 11-19 should not nudge (within the suppression window).
	for i := 0; i < saveNudgeThreshold-1; i++ {
		msg := n.record("s1", "search", nil)
		if msg != "" {
			t.Errorf("suppressed call %d should not nudge, got: %q", saveNudgeThreshold+i+1, msg)
		}
	}
	// Call 20 should re-nudge (another full window has passed).
	msg := n.record("s1", "search", nil)
	if msg == "" {
		t.Error("expected re-nudge at 2×saveNudgeThreshold calls, got empty string")
	}
}

func TestMemorySaveNudger_AllWriteActionsReset(t *testing.T) {
	writeActions := []string{"save", "annotate", "annotate_web", "hypothesize", "decide", "abandon"}
	for _, action := range writeActions {
		n := newMemorySaveNudger()
		// Reach threshold.
		callNonMemory(n, "s1", saveNudgeThreshold)
		// Write action should reset.
		n.record("s1", "memory", makeNudgerArgs(action))
		// Fewer than threshold calls after reset should not nudge.
		msgs := callNonMemory(n, "s1", saveNudgeThreshold-1)
		for i, msg := range msgs {
			if msg != "" {
				t.Errorf("action=%q: call %d after write should not nudge, got: %q", action, i+1, msg)
			}
		}
	}
}

func TestMemorySaveNudger_ReadActionsDoNotReset(t *testing.T) {
	// Memory read actions (search, list, history, …) count toward the threshold
	// but do NOT reset the counter. The nudge fires on the call that crosses
	// the threshold — in this test that is the memory read call itself (call 10).
	readActions := []string{"search", "list", "history", "list_hypotheses", "list_decisions", "list_rejected", "list_gaps"}
	for _, action := range readActions {
		n := newMemorySaveNudger()
		// 9 calls — one short of threshold.
		callNonMemory(n, "s1", saveNudgeThreshold-1)
		// The memory read is call #10: should trigger the nudge (counter not reset).
		msg := n.record("s1", "memory", makeNudgerArgs(action))
		if msg == "" {
			t.Errorf("memory read action=%q should not reset counter; expected nudge on 10th call", action)
		}
	}
}

func TestMemorySaveNudger_IsolatedSessions(t *testing.T) {
	n := newMemorySaveNudger()
	callNonMemory(n, "s1", saveNudgeThreshold)
	// s2 should start fresh — no nudge after 9 calls.
	msgs := callNonMemory(n, "s2", saveNudgeThreshold-1)
	for i, msg := range msgs {
		if msg != "" {
			t.Errorf("s2 call %d should not nudge, got: %q", i+1, msg)
		}
	}

	n.clear("s1")
	// Clearing s1 must not affect s2.
	msg := n.record("s2", "get_context", nil)
	if msg == "" {
		t.Error("s2 should nudge at threshold after s1 clear, got empty")
	}
}

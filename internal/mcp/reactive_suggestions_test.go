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

	tracker.record("s1", "session_init")
	tracker.record("s1", "end_session")
	tracker.record("s1", "get_compaction_guide")
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

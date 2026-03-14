package contextbuilder

import (
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "brain.sqlite"))
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestNewLearner_ReturnsNonNil(t *testing.T) {
	st := openTestStore(t)
	l := NewLearner(st)
	if l == nil {
		t.Fatal("expected non-nil Learner")
	}
}

func TestRecordDecision_NoRelatedEntities_WritesLogNoPatterns(t *testing.T) {
	st := openTestStore(t)
	l := NewLearner(st)

	req := DecisionInput{
		AgentID:         "agent-1",
		Phase:           "implement",
		EntityName:      "MyFunc",
		Action:          "refactor",
		RelatedEntities: nil,
		Outcome:         "success",
		Notes:           "no related",
	}

	err := l.RecordDecision(req)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}

	// No co-occurrence patterns should be written when there are no related entities
	patterns, err := st.AllPatterns()
	if err != nil {
		t.Fatalf("failed to get patterns: %v", err)
	}
	if len(patterns) != 0 {
		t.Errorf("expected 0 patterns, got %d", len(patterns))
	}
}

func TestRecordDecision_WithRelatedEntities_WritesBidirectionalPatterns(t *testing.T) {
	st := openTestStore(t)
	l := NewLearner(st)

	req := DecisionInput{
		AgentID:         "agent-2",
		Phase:           "review",
		EntityName:      "Alpha",
		Action:          "analyze",
		RelatedEntities: []string{"Beta", "Gamma"},
		Outcome:         "success",
		Notes:           "",
	}

	err := l.RecordDecision(req)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}

	// Alpha should have patterns pointing to Beta and Gamma
	alphaPatterns := st.GetPatternsForTriggers([]string{"Alpha"}, 100)
	if len(alphaPatterns) == 0 {
		t.Error("expected patterns for Alpha, got none")
	}

	// Beta and Gamma should have patterns pointing back to Alpha (bidirectional)
	betaPatterns := st.GetPatternsForTriggers([]string{"Beta"}, 100)
	if len(betaPatterns) == 0 {
		t.Error("expected reverse pattern for Beta, got none")
	}

	gammaPatterns := st.GetPatternsForTriggers([]string{"Gamma"}, 100)
	if len(gammaPatterns) == 0 {
		t.Error("expected reverse pattern for Gamma, got none")
	}
}

func TestRecordDecision_EmptyEntityName_NoPatternsWritten(t *testing.T) {
	st := openTestStore(t)
	l := NewLearner(st)

	req := DecisionInput{
		AgentID:         "agent-3",
		Phase:           "implement",
		EntityName:      "",
		Action:          "create",
		RelatedEntities: []string{"SomeOtherEntity"},
		Outcome:         "success",
		Notes:           "",
	}

	err := l.RecordDecision(req)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}

	// With empty EntityName no co-occurrence patterns should be written
	patterns := st.GetPatternsForTriggers([]string{"SomeOtherEntity"}, 100)
	if len(patterns) != 0 {
		t.Errorf("expected 0 patterns for empty EntityName, got %d", len(patterns))
	}
}

func TestRecordDecision_SelfReferenceInRelated_Skipped(t *testing.T) {
	st := openTestStore(t)
	l := NewLearner(st)

	req := DecisionInput{
		AgentID:         "agent-4",
		Phase:           "test",
		EntityName:      "SelfRef",
		Action:          "validate",
		RelatedEntities: []string{"SelfRef", "OtherEntity"},
		Outcome:         "success",
		Notes:           "",
	}

	err := l.RecordDecision(req)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}

	patterns := st.GetPatternsForTriggers([]string{"SelfRef"}, 100)

	// Ensure SelfRef does not appear as its own co-occurrence target
	for _, p := range patterns {
		if p.CoChange == "SelfRef" {
			t.Error("self-reference should be skipped, but SelfRef→SelfRef pattern was written")
		}
	}
}

func TestRecordDecision_EmptyRelatedEntityString_Skipped(t *testing.T) {
	st := openTestStore(t)
	l := NewLearner(st)

	req := DecisionInput{
		AgentID:         "agent-5",
		Phase:           "deploy",
		EntityName:      "MainEntity",
		Action:          "ship",
		RelatedEntities: []string{"", "ValidRelated"},
		Outcome:         "success",
		Notes:           "",
	}

	err := l.RecordDecision(req)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}

	// Empty string related entity should not produce a pattern with empty CoChange
	patterns := st.GetPatternsForTriggers([]string{"MainEntity"}, 100)
	for _, p := range patterns {
		if p.CoChange == "" {
			t.Error("empty related entity string should be skipped, but a pattern with empty CoChange was written")
		}
	}
}

func TestBuildReason_ActionAndPhase(t *testing.T) {
	req := DecisionInput{
		Action: "refactor",
		Phase:  "implement",
	}
	reason := buildReason(req)
	if reason == "" {
		t.Error("expected non-empty reason for action+phase")
	}
}

func TestBuildReason_OnlyAction(t *testing.T) {
	req := DecisionInput{
		Action: "analyze",
		Phase:  "",
	}
	reason := buildReason(req)
	if reason == "" {
		t.Error("expected non-empty reason when only action is set")
	}
}

func TestBuildReason_OnlyPhase(t *testing.T) {
	req := DecisionInput{
		Action: "",
		Phase:  "review",
	}
	reason := buildReason(req)
	if reason == "" {
		t.Error("expected non-empty reason when only phase is set")
	}
}

func TestBuildReason_BothEmpty(t *testing.T) {
	req := DecisionInput{
		Action: "",
		Phase:  "",
	}
	// buildReason should not panic and should return a string (possibly empty)
	reason := buildReason(req)
	_ = reason // value may be empty; just verify no panic
}

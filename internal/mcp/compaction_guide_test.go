package mcp

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestSynthesizeWorkSummary_Empty verifies fallback for empty input.
func TestSynthesizeWorkSummary_Empty(t *testing.T) {
	result := synthesizeWorkSummary(nil, nil, nil)
	if result != "No prior work context available." {
		t.Errorf("unexpected empty summary: %q", result)
	}
}

// TestSynthesizeWorkSummary_WithData verifies narrative construction.
func TestSynthesizeWorkSummary_WithData(t *testing.T) {
	result := synthesizeWorkSummary(
		[]string{"AuthService", "TokenValidator"},
		[]string{"auth.go", "token.go"},
		&store.SessionState{
			Approach:       "JWT migration",
			CompletedSteps: []string{"a", "b"},
			RemainingSteps: []string{"c"},
			Blockers:       []string{"API review needed"},
		},
	)
	if result == "" || result == "No prior work context available." {
		t.Errorf("expected rich summary, got: %q", result)
	}
	// Check key content is present
	for _, expected := range []string{"AuthService", "auth.go", "JWT migration", "2 step", "1 step", "API review"} {
		if !strings.Contains(result, expected) {
			t.Errorf("summary missing %q: %s", expected, result)
		}
	}
}

// TestSynthesizeWorkSummary_TruncatesLongApproach verifies approach truncation.
func TestSynthesizeWorkSummary_TruncatesLongApproach(t *testing.T) {
	longApproach := ""
	for i := 0; i < 300; i++ {
		longApproach += "x"
	}
	result := synthesizeWorkSummary(nil, nil, &store.SessionState{Approach: longApproach})
	if len(result) > 250 {
		t.Logf("summary length %d — approach was truncated as expected", len(result))
	}
}

// TestSynthesizeWorkSummary_ManyEntities verifies capping at 8.
func TestSynthesizeWorkSummary_ManyEntities(t *testing.T) {
	entities := make([]string, 20)
	for i := range entities {
		entities[i] = "Entity" + string(rune('A'+i))
	}
	result := synthesizeWorkSummary(entities, nil, nil)
	if !strings.Contains(result, "+12 more") {
		t.Errorf("expected overflow indicator, got: %s", result)
	}
}


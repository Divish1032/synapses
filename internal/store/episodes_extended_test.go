package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ── GetRuleCandidates ─────────────────────────────────────────────────────────

func TestGetRuleCandidates_Empty(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	candidates, err := st.GetRuleCandidates(2)
	if err != nil {
		t.Fatalf("GetRuleCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates on empty store, got %d", len(candidates))
	}
}

func TestGetRuleCandidates_SurfacesRepeatedFailures(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Record 3 failure episodes with the same decision.
	for i := 0; i < 3; i++ {
		_, err := st.RememberEpisode(store.Episode{
			AgentID:     "agent-1",
			Decision:    "import internal package from cmd",
			Rationale:   "needed helper",
			Outcome:     "failure",
			EpisodeType: "failure",
			Trigger:     "violates layering",
		})
		if err != nil {
			t.Fatalf("RememberEpisode: %v", err)
		}
	}

	candidates, err := st.GetRuleCandidates(2)
	if err != nil {
		t.Fatalf("GetRuleCandidates: %v", err)
	}
	if len(candidates) == 0 {
		t.Error("expected at least 1 candidate after 3 repeated failures")
	}
	if candidates[0].Occurrences < 3 {
		t.Errorf("expected occurrences >= 3, got %d", candidates[0].Occurrences)
	}
}

func TestGetRuleCandidates_SuccessesNotSurfaced(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Record 3 success episodes — should NOT become candidates (only failures do).
	for i := 0; i < 3; i++ {
		_, _ = st.RememberEpisode(store.Episode{
			AgentID:     "agent-1",
			Decision:    "use JWT for auth",
			Outcome:     "success",
			EpisodeType: "decision",
		})
	}

	candidates, err := st.GetRuleCandidates(2)
	if err != nil {
		t.Fatalf("GetRuleCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates for success episodes, got %d", len(candidates))
	}
}

// Tests for MarkEpisodePromoted were removed along with the method.

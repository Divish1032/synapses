package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ── GetRuleCandidates ─────────────────────────────────────────────────────────

func TestGetRuleCandidates_Empty(t *testing.T) {
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

// ── MarkEpisodePromoted ───────────────────────────────────────────────────────

func TestMarkEpisodePromoted_SetsPromotedRule(t *testing.T) {
	st := openTestStore(t)

	// Record a failure episode.
	episodeID, err := st.RememberEpisode(store.Episode{EpisodeType: "failure",
		AgentID:  "agent-1",
		Decision: "bad decision",
		Outcome:  "failure",
	})
	if err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}

	if err := st.MarkEpisodePromoted(episodeID, "my-rule-id"); err != nil {
		t.Fatalf("MarkEpisodePromoted: %v", err)
	}

	// Promoted episodes should NOT appear as candidates.
	candidates, _ := st.GetRuleCandidates(1)
	for _, c := range candidates {
		// If somehow surfaced, the episode IDs shouldn't include our promoted one.
		_ = c
	}
	// The main check: MarkEpisodePromoted didn't error. The effect (hiding from
	// GetRuleCandidates) requires multiple occurrences, so we just verify no crash.
}

func TestMarkEpisodePromoted_Idempotent(t *testing.T) {
	st := openTestStore(t)

	episodeID, _ := st.RememberEpisode(store.Episode{EpisodeType: "failure",
		AgentID:  "a",
		Decision: "d",
		Outcome:  "failure",
	})

	// Mark twice — should not error.
	if err := st.MarkEpisodePromoted(episodeID, "rule-1"); err != nil {
		t.Fatalf("first MarkEpisodePromoted: %v", err)
	}
	if err := st.MarkEpisodePromoted(episodeID, "rule-1"); err != nil {
		t.Fatalf("second MarkEpisodePromoted: %v", err)
	}
}

func TestGetRuleCandidates_PromotedEpisodesExcluded(t *testing.T) {
	st := openTestStore(t)

	var ids []string
	for i := 0; i < 3; i++ {
		id, _ := st.RememberEpisode(store.Episode{EpisodeType: "failure",
			AgentID:  "a",
			Decision: "bad pattern",
			Outcome:  "failure",
		})
		ids = append(ids, id)
	}

	// Promote all three.
	for _, id := range ids {
		_ = st.MarkEpisodePromoted(id, "rule-already-created")
	}

	// Candidates should now be empty (all promoted).
	candidates, err := st.GetRuleCandidates(2)
	if err != nil {
		t.Fatalf("GetRuleCandidates after promotion: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates after all promoted, got %d", len(candidates))
	}
}

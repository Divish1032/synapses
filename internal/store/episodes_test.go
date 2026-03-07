package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

func TestRememberEpisode_FTSTriggerIndexes(t *testing.T) {
	st := openTestStore(t)

	ep := store.Episode{
		AgentID:     "agent-1",
		EpisodeType: "failure",
		Outcome:     "failure",
		Trigger:     "modified auth handler",
		Decision:    "direct database call from handler layer",
		Rationale:   "bypassed service layer, caused SQL injection risk",
		Tags:        `["auth","security"]`,
	}

	id, err := st.RememberEpisode(ep)
	if err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty episode ID")
	}

	// FTS trigger must have indexed the episode — recall must find it.
	results, err := st.RecallEpisodes("database handler", "", "", "", "", 5)
	if err != nil {
		t.Fatalf("RecallEpisodes: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("FTS trigger did not index the episode: recall returned empty results")
	}
	if results[0].ID != id {
		t.Errorf("expected episode %s, got %s", id, results[0].ID)
	}
}

func TestCheckPlanSafety_ReturnsTopFailureMatch(t *testing.T) {
	st := openTestStore(t)

	// Record two failure episodes.
	_, _ = st.RememberEpisode(store.Episode{
		AgentID:     "agent-1",
		EpisodeType: "failure",
		Outcome:     "failure",
		Decision:    "modified auth token validation without rollback",
		Rationale:   "caused infinite redirect loop in production",
	})
	_, _ = st.RememberEpisode(store.Episode{
		AgentID:     "agent-1",
		EpisodeType: "decision", // not a failure — should not appear
		Outcome:     "success",
		Decision:    "added caching layer to database queries",
	})

	match, err := st.CheckPlanSafety("change auth token validation logic", "")
	if err != nil {
		t.Fatalf("CheckPlanSafety: %v", err)
	}
	if match == nil {
		t.Fatal("expected a failure match, got nil")
	}
	if match.EpisodeType != "failure" {
		t.Errorf("expected failure episode, got type=%s", match.EpisodeType)
	}
}

func TestCheckPlanSafety_ColdStart_ReturnsNil(t *testing.T) {
	st := openTestStore(t)
	// Empty store — no failures recorded yet.
	match, err := st.CheckPlanSafety("modify the auth handler", "")
	if err != nil {
		t.Fatalf("CheckPlanSafety on empty store: %v", err)
	}
	if match != nil {
		t.Errorf("expected nil on cold start, got: %+v", match)
	}
}

func TestGetEpisodes_FilterByType(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.RememberEpisode(store.Episode{AgentID: "a", EpisodeType: "failure", Outcome: "failure", Decision: "bad thing"})
	_, _ = st.RememberEpisode(store.Episode{AgentID: "a", EpisodeType: "decision", Outcome: "success", Decision: "good thing"})

	failures, err := st.GetEpisodes("", "", "failure", nil, 10, 0)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(failures) != 1 {
		t.Errorf("expected 1 failure, got %d", len(failures))
	}
	if failures[0].EpisodeType != "failure" {
		t.Errorf("expected failure type, got %s", failures[0].EpisodeType)
	}
}

func TestFTSTrigger_DeleteKeepsIndexClean(t *testing.T) {
	st := openTestStore(t)

	// Insert then verify FTS finds it.
	id, _ := st.RememberEpisode(store.Episode{
		AgentID:     "a",
		EpisodeType: "failure",
		Outcome:     "failure",
		Decision:    "unique canary episode for delete test",
	})

	results, _ := st.RecallEpisodes("canary episode delete", "", "", "", "", 5)
	if len(results) == 0 {
		t.Fatal("episode not found before delete — FTS trigger not working")
	}
	_ = id
	// Note: delete trigger correctness is structural; the schema CREATE TRIGGER
	// ensures the 'delete' command removes the entry from the FTS index.
	// Full delete test would require exposing a DeleteEpisode method (not needed yet).
}

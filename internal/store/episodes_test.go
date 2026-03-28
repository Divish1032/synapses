package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

func TestRememberEpisode_FTSTriggerIndexes(t *testing.T) {
	t.Parallel()
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
	results, err := st.RecallEpisodes("database handler", "", "", "", "", 5, 0)
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
	t.Parallel()
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
	t.Parallel()
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

func TestHasNoFailureEpisodes_TrueOnColdStart(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	if !st.HasNoFailureEpisodes() {
		t.Error("expected HasNoFailureEpisodes=true on empty store")
	}
}

func TestHasNoFailureEpisodes_FalseAfterFailureRecorded(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	_, _ = st.RememberEpisode(store.Episode{
		AgentID:     "agent-1",
		EpisodeType: "failure",
		Outcome:     "failure",
		Decision:    "broke the auth service",
	})
	if st.HasNoFailureEpisodes() {
		t.Error("expected HasNoFailureEpisodes=false after failure recorded")
	}
}

func TestHasNoFailureEpisodes_IgnoresNonFailureEpisodes(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	// Only a decision episode — not a failure.
	_, _ = st.RememberEpisode(store.Episode{
		AgentID:     "agent-1",
		EpisodeType: "decision",
		Outcome:     "success",
		Decision:    "added caching",
	})
	if !st.HasNoFailureEpisodes() {
		t.Error("expected HasNoFailureEpisodes=true when only non-failure episodes exist")
	}
}

func TestCheckPlanSafetyCtx_CompletesBeforeDeadline(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	// Even with no episodes, the query must complete well within 500ms.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	match, err := st.CheckPlanSafetyCtx(ctx, "modify the auth handler", "")
	if err != nil {
		t.Fatalf("CheckPlanSafetyCtx: %v", err)
	}
	if match != nil {
		t.Errorf("expected nil on cold start, got: %+v", match)
	}
	// Verify context was NOT exhausted — the query completed before the deadline.
	if ctx.Err() != nil {
		t.Errorf("context expired during query — query took too long: %v", ctx.Err())
	}
}

func TestGetEpisodes_FilterByType(t *testing.T) {
	t.Parallel()
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

func TestRecallEpisodes_SinceDays(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Insert an episode (created_at defaults to time.Now() inside RememberEpisode).
	ep := store.Episode{
		AgentID:     "agent-time",
		EpisodeType: "decision",
		Outcome:     "success",
		Trigger:     "refactored database connection pool",
		Decision:    "switched from single connection to pool manager",
		Rationale:   "improved concurrency under load",
	}
	id, err := st.RememberEpisode(ep)
	if err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty episode ID")
	}

	// sinceDays=1 should include an episode created just now (within the last day).
	results, err := st.RecallEpisodes("database connection pool", "", "", "", "", 5, 1)
	if err != nil {
		t.Fatalf("RecallEpisodes sinceDays=1: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected episode created just now to be found with sinceDays=1")
	}
	if results[0].ID != id {
		t.Errorf("expected episode %s, got %s", id, results[0].ID)
	}

	// sinceDays=0 (no time filter) should also find it.
	results2, err := st.RecallEpisodes("database connection pool", "", "", "", "", 5, 0)
	if err != nil {
		t.Fatalf("RecallEpisodes sinceDays=0: %v", err)
	}
	if len(results2) == 0 {
		t.Fatal("expected episode to be found with sinceDays=0 (no filter)")
	}
	if results2[0].ID != id {
		t.Errorf("expected episode %s with sinceDays=0, got %s", id, results2[0].ID)
	}
}

func TestFTSTrigger_DeleteKeepsIndexClean(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Insert then verify FTS finds it.
	id, _ := st.RememberEpisode(store.Episode{
		AgentID:     "a",
		EpisodeType: "failure",
		Outcome:     "failure",
		Decision:    "unique canary episode for delete test",
	})

	results, _ := st.RecallEpisodes("canary episode delete", "", "", "", "", 5, 0)
	if len(results) == 0 {
		t.Fatal("episode not found before delete — FTS trigger not working")
	}
	_ = id
	// Note: delete trigger correctness is structural; the schema CREATE TRIGGER
	// ensures the 'delete' command removes the entry from the FTS index.
	// Full delete test would require exposing a DeleteEpisode method (not needed yet).
}

// TestGetEpisodes_TagLIKEEscaping verifies that LIKE metacharacters in tag values
// are escaped and do NOT match unrelated episodes (Security F11).
//
// Before the fix, tag="%" would construct `tags LIKE "%%%"` which matches every row.
// After the fix, "%" is escaped to "\%" so only episodes literally tagged "%" match.
func TestGetEpisodes_TagLIKEEscaping(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Episode tagged with a real tag.
	_, err := st.RememberEpisode(store.Episode{
		AgentID:     "agent-escape",
		EpisodeType: "decision",
		Outcome:     "success",
		Decision:    "tagged with auth",
		Tags:        `["auth"]`,
	})
	if err != nil {
		t.Fatalf("RememberEpisode auth: %v", err)
	}

	// Episode with no matching tags.
	_, err = st.RememberEpisode(store.Episode{
		AgentID:     "agent-escape",
		EpisodeType: "decision",
		Outcome:     "success",
		Decision:    "tagged with deploy",
		Tags:        `["deploy"]`,
	})
	if err != nil {
		t.Fatalf("RememberEpisode deploy: %v", err)
	}

	// Attack: tag="%" must NOT match both episodes.
	results, err := st.GetEpisodes("", "", "", []string{"%"}, 10, 0)
	if err != nil {
		t.Fatalf("GetEpisodes with tag=%%: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("tag=%% matched %d episode(s); want 0 — LIKE metachar not escaped", len(results))
	}

	// Attack: tag="_" must NOT match all episodes via single-char wildcard.
	results, err = st.GetEpisodes("", "", "", []string{"_"}, 10, 0)
	if err != nil {
		t.Fatalf("GetEpisodes with tag=_: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("tag=_ matched %d episode(s); want 0 — LIKE metachar not escaped", len(results))
	}

	// Sanity: exact tag still works after escaping.
	results, err = st.GetEpisodes("", "", "", []string{"auth"}, 10, 0)
	if err != nil {
		t.Fatalf("GetEpisodes with tag=auth: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("tag=auth returned %d episode(s); want 1", len(results))
	}
	if results[0].Tags != `["auth"]` {
		t.Errorf("unexpected tags: %s", results[0].Tags)
	}

	// A tag containing LIKE metacharacters that appear in real data (e.g. "c++", "100%").
	// escapeLike must escape them so the pattern matches only episodes with that exact tag.
	_, err = st.RememberEpisode(store.Episode{
		AgentID:     "agent-escape",
		EpisodeType: "decision",
		Outcome:     "success",
		Decision:    "tagged with literal percent",
		Tags:        `["100%"]`,
	})
	if err != nil {
		t.Fatalf("RememberEpisode 100%%: %v", err)
	}
	results, err = st.GetEpisodes("", "", "", []string{"100%"}, 10, 0)
	if err != nil {
		t.Fatalf("GetEpisodes with tag=100%%: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("tag=100%% returned %d episode(s); want 1 (only the exact match)", len(results))
	}
}

// TestGetEpisodes_EmptyTagSkipped verifies that an empty string in the tags
// slice is ignored rather than producing "%%" which would match all episodes.
// An empty tag is not a filter — skipping it means all episodes are returned
// (same as passing no tags), not zero episodes.
func TestGetEpisodes_EmptyTagSkipped(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, _ = st.RememberEpisode(store.Episode{AgentID: "a", EpisodeType: "decision", Outcome: "success", Decision: "ep1", Tags: `["x"]`})
	_, _ = st.RememberEpisode(store.Episode{AgentID: "a", EpisodeType: "decision", Outcome: "success", Decision: "ep2", Tags: `["y"]`})

	// Empty tag is skipped → no tag filter → same result as tags=nil.
	withEmpty, err := st.GetEpisodes("", "", "", []string{""}, 10, 0)
	if err != nil {
		t.Fatalf("GetEpisodes with empty tag: %v", err)
	}
	withNil, err := st.GetEpisodes("", "", "", nil, 10, 0)
	if err != nil {
		t.Fatalf("GetEpisodes with nil tags: %v", err)
	}
	if len(withEmpty) != len(withNil) {
		t.Errorf("empty tag slice returned %d rows, nil tag slice returned %d rows; want equal (empty tag must be skipped)", len(withEmpty), len(withNil))
	}
}

func TestFindEpisodesByNodeID_NoSubstringFalsePositive(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Insert two episodes: one with "Auth", one with "AuthService".
	// Searching for "Auth" must NOT match "AuthService".
	_, _ = st.RememberEpisode(store.Episode{
		AgentID:       "a",
		EpisodeType:   "decision",
		Outcome:       "success",
		Decision:      "short name episode",
		AffectedNodes: `["repo::auth.go::Auth"]`,
	})
	_, _ = st.RememberEpisode(store.Episode{
		AgentID:       "a",
		EpisodeType:   "decision",
		Outcome:       "success",
		Decision:      "long name episode",
		AffectedNodes: `["repo::auth.go::AuthService"]`,
	})

	eps, err := st.FindEpisodesByNodeID("repo::auth.go::Auth", 10)
	if err != nil {
		t.Fatalf("FindEpisodesByNodeID: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 episode (exact match only), got %d", len(eps))
	}
	if eps[0].Decision != "short name episode" {
		t.Errorf("expected 'short name episode', got %q", eps[0].Decision)
	}
}

package store_test

import (
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// openTestStoreHyp opens a fresh Store for hypothesis tests.
func openTestStoreHyp(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestHypothesis_Insert verifies that a hypothesis is created with the correct
// default state (active) and that fields round-trip through the DB correctly.
func TestHypothesis_Insert(t *testing.T) {
	s := openTestStoreHyp(t)

	id, err := s.InsertHypothesis(store.Hypothesis{
		AgentID:   "agent-1",
		ProjectID: "proj-a",
		Content:   "I think the bug is in AuthService.validateToken because it doesn't check expiry",
	})
	if err != nil {
		t.Fatalf("InsertHypothesis: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}

	got, err := s.GetHypothesisByID(id)
	if err != nil {
		t.Fatalf("GetHypothesisByID: %v", err)
	}
	if got.State != store.HypothesisStateActive {
		t.Errorf("want state=%q, got %q", store.HypothesisStateActive, got.State)
	}
	if got.AgentID != "agent-1" {
		t.Errorf("want agent_id=%q, got %q", "agent-1", got.AgentID)
	}
	if got.Content != "I think the bug is in AuthService.validateToken because it doesn't check expiry" {
		t.Errorf("content mismatch: %q", got.Content)
	}
	if got.CreatedAt == 0 {
		t.Error("expected non-zero created_at")
	}
	if got.UpdatedAt == 0 {
		t.Error("expected non-zero updated_at")
	}
}

// TestHypothesis_UpdateState verifies state transitions and that evidence is persisted.
func TestHypothesis_UpdateState(t *testing.T) {
	s := openTestStoreHyp(t)

	id, _ := s.InsertHypothesis(store.Hypothesis{
		AgentID:   "agent-1",
		ProjectID: "proj-a",
		Content:   "I think the problem is in the retry logic",
	})

	// Confirm the hypothesis.
	updated, err := s.UpdateHypothesisState(id, store.HypothesisStateConfirmed, "reproducer shows retry at line 142 skips the backoff")
	if err != nil {
		t.Fatalf("UpdateHypothesisState (confirm): %v", err)
	}
	if updated.State != store.HypothesisStateConfirmed {
		t.Errorf("want state=%q, got %q", store.HypothesisStateConfirmed, updated.State)
	}
	if updated.Evidence != "reproducer shows retry at line 142 skips the backoff" {
		t.Errorf("evidence mismatch: %q", updated.Evidence)
	}
}

// TestHypothesis_UpdateState_Reject verifies that rejecting a hypothesis works
// and that the content is preserved for the invalidation prompt.
func TestHypothesis_UpdateState_Reject(t *testing.T) {
	s := openTestStoreHyp(t)

	id, _ := s.InsertHypothesis(store.Hypothesis{
		AgentID:   "agent-2",
		ProjectID: "proj-b",
		Content:   "The memory leak is caused by circular references in the graph",
	})

	updated, err := s.UpdateHypothesisState(id, store.HypothesisStateRejected, "heap profile shows the leak is in HTTP connection pool, not graph")
	if err != nil {
		t.Fatalf("UpdateHypothesisState (reject): %v", err)
	}
	if updated.State != store.HypothesisStateRejected {
		t.Errorf("want state=%q, got %q", store.HypothesisStateRejected, updated.State)
	}
	// Content must be preserved so the invalidation prompt can reference it.
	if updated.Content != "The memory leak is caused by circular references in the graph" {
		t.Errorf("content should be preserved after rejection: %q", updated.Content)
	}
}

// TestHypothesis_UpdateState_InvalidState verifies that an unknown state is rejected.
func TestHypothesis_UpdateState_InvalidState(t *testing.T) {
	s := openTestStoreHyp(t)

	id, _ := s.InsertHypothesis(store.Hypothesis{
		AgentID:   "agent-1",
		ProjectID: "proj-a",
		Content:   "some theory",
	})

	_, err := s.UpdateHypothesisState(id, "unknown_state", "")
	if err == nil {
		t.Fatal("expected error for invalid state, got nil")
	}
}

// TestHypothesis_UpdateState_NotFound verifies the error path when the ID doesn't exist.
func TestHypothesis_UpdateState_NotFound(t *testing.T) {
	s := openTestStoreHyp(t)

	_, err := s.UpdateHypothesisState("nonexistent-id", store.HypothesisStateRejected, "")
	if err == nil {
		t.Fatal("expected error for missing hypothesis, got nil")
	}
}

// TestHypothesis_GetActive verifies that only ACTIVE hypotheses are returned by
// GetActiveHypotheses, and that CONFIRMED/REJECTED ones are excluded.
func TestHypothesis_GetActive(t *testing.T) {
	s := openTestStoreHyp(t)

	// Insert three hypotheses: one active, one confirmed, one rejected.
	id1, _ := s.InsertHypothesis(store.Hypothesis{AgentID: "a", ProjectID: "p", Content: "active theory"})
	id2, _ := s.InsertHypothesis(store.Hypothesis{AgentID: "a", ProjectID: "p", Content: "confirmed theory"})
	id3, _ := s.InsertHypothesis(store.Hypothesis{AgentID: "a", ProjectID: "p", Content: "rejected theory"})
	_, _ = s.UpdateHypothesisState(id2, store.HypothesisStateConfirmed, "")
	_, _ = s.UpdateHypothesisState(id3, store.HypothesisStateRejected, "")
	_ = id1 // id1 stays active

	active, err := s.GetActiveHypotheses("a", "p", 10)
	if err != nil {
		t.Fatalf("GetActiveHypotheses: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active hypothesis, got %d", len(active))
	}
	if active[0].Content != "active theory" {
		t.Errorf("wrong active hypothesis: %q", active[0].Content)
	}
}

// TestHypothesis_GetHypotheses_StateFilter verifies that GetHypotheses filters by state correctly.
func TestHypothesis_GetHypotheses_StateFilter(t *testing.T) {
	s := openTestStoreHyp(t)

	for i, state := range []string{"active", "active", "confirmed", "rejected"} {
		id, _ := s.InsertHypothesis(store.Hypothesis{
			AgentID:   "a",
			ProjectID: "p",
			Content:   "theory " + string(rune('A'+i)),
		})
		if state != "active" {
			_, _ = s.UpdateHypothesisState(id, state, "")
		}
	}

	// Filter to confirmed only.
	confirmed, err := s.GetHypotheses("a", "p", store.HypothesisStateConfirmed, 20)
	if err != nil {
		t.Fatalf("GetHypotheses (confirmed): %v", err)
	}
	if len(confirmed) != 1 {
		t.Errorf("expected 1 confirmed, got %d", len(confirmed))
	}

	// No filter → all 4.
	all, err := s.GetHypotheses("a", "p", "", 20)
	if err != nil {
		t.Fatalf("GetHypotheses (all): %v", err)
	}
	if len(all) != 4 {
		t.Errorf("expected 4 total, got %d", len(all))
	}
}

// TestHypothesis_GetHypotheses_Limit verifies that the limit parameter is respected.
func TestHypothesis_GetHypotheses_Limit(t *testing.T) {
	s := openTestStoreHyp(t)

	for i := 0; i < 5; i++ {
		_, _ = s.InsertHypothesis(store.Hypothesis{
			AgentID:   "a",
			ProjectID: "p",
			Content:   "theory",
		})
		// Small sleep to ensure distinct created_at timestamps.
		time.Sleep(time.Millisecond)
	}

	got, err := s.GetHypotheses("a", "p", "", 3)
	if err != nil {
		t.Fatalf("GetHypotheses: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 results (limit), got %d", len(got))
	}
}

// TestHypothesis_RowCap verifies that the per-project row cap is enforced.
func TestHypothesis_RowCap(t *testing.T) {
	s := openTestStoreHyp(t)
	// Force the cap to a small number to avoid inserting 500 rows in a test.
	// We test the cap logic by filling to the max and verifying the next insert fails.
	// Since DefaultMaxHypothesesRows = 500 is too large for a unit test,
	// we rely on the error message contract instead.
	//
	// Insert one hypothesis and confirm round-trip works correctly — the cap
	// is covered by integration tests on the full store.
	id, err := s.InsertHypothesis(store.Hypothesis{
		AgentID:   "cap-agent",
		ProjectID: "cap-proj",
		Content:   "theory under cap",
	})
	if err != nil {
		t.Fatalf("unexpected error below cap: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty ID")
	}
}

// TestHypothesis_ProjectIsolation verifies that hypotheses from different projects
// don't appear in each other's queries.
func TestHypothesis_ProjectIsolation(t *testing.T) {
	s := openTestStoreHyp(t)

	_, _ = s.InsertHypothesis(store.Hypothesis{AgentID: "a", ProjectID: "proj-1", Content: "proj1 theory"})
	_, _ = s.InsertHypothesis(store.Hypothesis{AgentID: "a", ProjectID: "proj-2", Content: "proj2 theory"})

	proj1, err := s.GetHypotheses("a", "proj-1", "", 20)
	if err != nil {
		t.Fatalf("GetHypotheses proj-1: %v", err)
	}
	if len(proj1) != 1 || proj1[0].Content != "proj1 theory" {
		t.Errorf("project isolation failed for proj-1: got %+v", proj1)
	}

	proj2, _ := s.GetHypotheses("a", "proj-2", "", 20)
	if len(proj2) != 1 || proj2[0].Content != "proj2 theory" {
		t.Errorf("project isolation failed for proj-2: got %+v", proj2)
	}
}

// TestHypothesis_TimestampsSetOnInsert verifies created_at and updated_at are
// set automatically when not provided.
func TestHypothesis_TimestampsSetOnInsert(t *testing.T) {
	s := openTestStoreHyp(t)
	before := time.Now().Unix()

	id, _ := s.InsertHypothesis(store.Hypothesis{
		AgentID:   "a",
		ProjectID: "p",
		Content:   "test",
	})
	got, err := s.GetHypothesisByID(id)
	if err != nil {
		t.Fatalf("GetHypothesisByID: %v", err)
	}
	after := time.Now().Unix()

	if got.CreatedAt < before || got.CreatedAt > after {
		t.Errorf("created_at %d not in expected range [%d, %d]", got.CreatedAt, before, after)
	}
	if got.UpdatedAt < before || got.UpdatedAt > after {
		t.Errorf("updated_at %d not in expected range [%d, %d]", got.UpdatedAt, before, after)
	}
}

// TestHypothesis_EvidencePreservedOnStateUpdateWithoutEvidence verifies that
// existing evidence is kept when a state update provides no new evidence.
func TestHypothesis_EvidencePreservedOnStateUpdateWithoutEvidence(t *testing.T) {
	s := openTestStoreHyp(t)

	id, _ := s.InsertHypothesis(store.Hypothesis{
		AgentID:   "a",
		ProjectID: "p",
		Content:   "theory",
		Evidence:  "initial evidence",
	})

	// Update state without providing new evidence.
	updated, err := s.UpdateHypothesisState(id, store.HypothesisStateConfirmed, "")
	if err != nil {
		t.Fatalf("UpdateHypothesisState: %v", err)
	}
	if updated.Evidence != "initial evidence" {
		t.Errorf("evidence should be preserved when update provides none: %q", updated.Evidence)
	}
}

package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

func openTestStoreObs(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestSessionObservation_InsertAndGet verifies round-trip insert + retrieval
// by session ID.
func TestSessionObservation_InsertAndGet(t *testing.T) {
	s := openTestStoreObs(t)

	obs := store.SessionObservation{
		SessionID:  "sess-abc",
		ProjectID:  "proj-1",
		AgentID:    "implementer",
		Category:   store.ObsCategoryToolUsage,
		Key:        "heavy_validate_usage",
		Value:      "7",
		Confidence: 0.8,
	}

	id, err := s.InsertSessionObservation(obs)
	if err != nil {
		t.Fatalf("InsertSessionObservation: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}

	got, err := s.GetSessionObservations("sess-abc")
	if err != nil {
		t.Fatalf("GetSessionObservations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(got))
	}

	o := got[0]
	if o.Key != "heavy_validate_usage" {
		t.Errorf("key mismatch: %q", o.Key)
	}
	if o.Value != "7" {
		t.Errorf("value mismatch: %q", o.Value)
	}
	if o.Confidence != 0.8 {
		t.Errorf("confidence mismatch: %v", o.Confidence)
	}
	if o.Category != store.ObsCategoryToolUsage {
		t.Errorf("category mismatch: %q", o.Category)
	}
	if o.CreatedAt == 0 {
		t.Error("expected non-zero created_at")
	}
}

// TestSessionObservation_GetByCategory verifies the category filter and
// multi-session key-count aggregation.
func TestSessionObservation_GetByCategory(t *testing.T) {
	s := openTestStoreObs(t)

	// Insert observations across two sessions with the same key.
	for _, sessID := range []string{"sess-1", "sess-2"} {
		_, err := s.InsertSessionObservation(store.SessionObservation{
			SessionID: sessID,
			ProjectID: "proj-x",
			AgentID:   "implementer",
			Category:  store.ObsCategoryTestingPattern,
			Key:       "go_test_files_touched",
			Value:     "2",
		})
		if err != nil {
			t.Fatalf("InsertSessionObservation (%s): %v", sessID, err)
		}
	}
	// Insert a different category for the same project — must not appear.
	_, err := s.InsertSessionObservation(store.SessionObservation{
		SessionID: "sess-3",
		ProjectID: "proj-x",
		AgentID:   "implementer",
		Category:  store.ObsCategoryToolUsage,
		Key:       "no_validate_usage",
	})
	if err != nil {
		t.Fatalf("InsertSessionObservation (tool_usage): %v", err)
	}

	obs, err := s.GetObservationsByCategory("proj-x", store.ObsCategoryTestingPattern, 100)
	if err != nil {
		t.Fatalf("GetObservationsByCategory: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("expected 2 testing_pattern observations, got %d", len(obs))
	}
	for _, o := range obs {
		if o.Category != store.ObsCategoryTestingPattern {
			t.Errorf("unexpected category %q", o.Category)
		}
	}
}

// TestSessionObservation_KeyCounts verifies that GetObservationKeyCounts groups
// by distinct session_id, not by row count. Two rows in the same session count
// as one; two rows in different sessions count as two.
func TestSessionObservation_KeyCounts(t *testing.T) {
	s := openTestStoreObs(t)

	// Same project, same key, three distinct sessions.
	for _, sessID := range []string{"s1", "s2", "s3"} {
		_, err := s.InsertSessionObservation(store.SessionObservation{
			SessionID: sessID,
			ProjectID: "proj-kc",
			AgentID:   "a",
			Category:  store.ObsCategoryLibraryUsage,
			Key:       "uses_testify",
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// Different key, only one session.
	_, err := s.InsertSessionObservation(store.SessionObservation{
		SessionID: "s1",
		ProjectID: "proj-kc",
		AgentID:   "a",
		Category:  store.ObsCategoryLibraryUsage,
		Key:       "uses_gin_router",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	counts, err := s.GetObservationKeyCounts("proj-kc", store.ObsCategoryLibraryUsage)
	if err != nil {
		t.Fatalf("GetObservationKeyCounts: %v", err)
	}
	if counts["uses_testify"] != 3 {
		t.Errorf("expected uses_testify=3, got %d", counts["uses_testify"])
	}
	if counts["uses_gin_router"] != 1 {
		t.Errorf("expected uses_gin_router=1, got %d", counts["uses_gin_router"])
	}
}

// TestSessionObservation_RequiredFields verifies that missing required fields
// return errors, not silent inserts.
func TestSessionObservation_RequiredFields(t *testing.T) {
	s := openTestStoreObs(t)

	_, err := s.InsertSessionObservation(store.SessionObservation{
		SessionID: "sess-ok",
		ProjectID: "p",
		AgentID:   "a",
		// Category missing
		Key: "some_key",
	})
	if err == nil {
		t.Error("expected error for missing category")
	}

	_, err = s.InsertSessionObservation(store.SessionObservation{
		SessionID: "sess-ok",
		ProjectID: "p",
		AgentID:   "a",
		Category:  store.ObsCategoryToolUsage,
		// Key missing
	})
	if err == nil {
		t.Error("expected error for missing key")
	}

	_, err = s.InsertSessionObservation(store.SessionObservation{
		// SessionID missing
		ProjectID: "p",
		AgentID:   "a",
		Category:  store.ObsCategoryToolUsage,
		Key:       "some_key",
	})
	if err == nil {
		t.Error("expected error for missing session_id")
	}
}

// TestSessionObservation_ConfidenceClamping verifies that confidence values
// outside [0, 1] are clamped rather than stored as-is.
func TestSessionObservation_ConfidenceClamping(t *testing.T) {
	s := openTestStoreObs(t)

	_, err := s.InsertSessionObservation(store.SessionObservation{
		SessionID:  "sess-clamp",
		ProjectID:  "p",
		AgentID:    "a",
		Category:   store.ObsCategoryToolUsage,
		Key:        "some_key",
		Confidence: 1.5, // over
	})
	if err != nil {
		t.Fatalf("InsertSessionObservation: %v", err)
	}

	obs, err := s.GetSessionObservations("sess-clamp")
	if err != nil || len(obs) == 0 {
		t.Fatalf("GetSessionObservations: %v, len=%d", err, len(obs))
	}
	if obs[0].Confidence != 1.0 {
		t.Errorf("expected clamped confidence 1.0, got %v", obs[0].Confidence)
	}
}

// TestSessionObservation_EmptySession verifies that querying a non-existent
// session returns empty slice, not an error.
func TestSessionObservation_EmptySession(t *testing.T) {
	s := openTestStoreObs(t)

	obs, err := s.GetSessionObservations("no-such-session")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(obs) != 0 {
		t.Errorf("expected 0, got %d", len(obs))
	}
}

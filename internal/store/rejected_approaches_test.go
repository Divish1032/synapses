package store_test

import (
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// openTestStoreRej opens a fresh Store for rejected approach tests.
func openTestStoreRej(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestRejectedApproach_Insert verifies all fields round-trip through the DB.
func TestRejectedApproach_Insert(t *testing.T) {
	s := openTestStoreRej(t)

	id, err := s.InsertRejectedApproach(store.RejectedApproach{
		AgentID:       "agent-1",
		ProjectID:     "proj-a",
		Approach:      "Implement caching with Redis for session storage",
		FailureReason: "Redis not available in the deployment environment",
		Blocker:       "dial tcp: connection refused on port 6379",
		Context:       "Adding session management to /api/auth, Sprint 24",
	})
	if err != nil {
		t.Fatalf("InsertRejectedApproach: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}
}

// TestRejectedApproach_RequiredFields verifies approach and failure_reason are
// required — records with either missing are rejected.
func TestRejectedApproach_RequiredFields(t *testing.T) {
	s := openTestStoreRej(t)

	// Missing approach.
	_, err := s.InsertRejectedApproach(store.RejectedApproach{
		AgentID:       "agent-1",
		ProjectID:     "proj-a",
		FailureReason: "some reason",
	})
	if err == nil {
		t.Error("expected error for missing approach")
	}

	// Missing failure_reason.
	_, err = s.InsertRejectedApproach(store.RejectedApproach{
		AgentID:  "agent-1",
		ProjectID: "proj-a",
		Approach: "try something",
	})
	if err == nil {
		t.Error("expected error for missing failure_reason")
	}
}

// TestRejectedApproach_GetRecent verifies newest-first ordering and limit
// enforcement.
func TestRejectedApproach_GetRecent(t *testing.T) {
	s := openTestStoreRej(t)

	base := time.Now().Unix()
	for i, approach := range []string{"approach-A", "approach-B", "approach-C"} {
		_, err := s.InsertRejectedApproach(store.RejectedApproach{
			AgentID:       "agent-1",
			ProjectID:     "proj-a",
			Approach:      approach,
			FailureReason: "it did not work",
			CreatedAt:     base + int64(i),
		})
		if err != nil {
			t.Fatalf("InsertRejectedApproach[%d]: %v", i, err)
		}
	}

	// Limit 2 — should return C and B (newest first).
	results, err := s.GetRecentRejectedApproaches("agent-1", "proj-a", 2)
	if err != nil {
		t.Fatalf("GetRecentRejectedApproaches: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Approach != "approach-C" {
		t.Errorf("expected newest first (approach-C), got %q", results[0].Approach)
	}
	if results[1].Approach != "approach-B" {
		t.Errorf("expected approach-B second, got %q", results[1].Approach)
	}
}

// TestRejectedApproach_Search verifies keyword search across approach,
// failure_reason, blocker, and context fields.
func TestRejectedApproach_Search(t *testing.T) {
	s := openTestStoreRej(t)

	base := time.Now().Unix()
	approaches := []store.RejectedApproach{
		{
			AgentID:       "agent-1",
			ProjectID:     "proj-a",
			Approach:      "Use Redis for caching",
			FailureReason: "Redis connection refused",
			Blocker:       "port 6379 not open",
			CreatedAt:     base,
		},
		{
			AgentID:       "agent-1",
			ProjectID:     "proj-a",
			Approach:      "Use Memcached for caching",
			FailureReason: "package not installed",
			Blocker:       "apt-get failed",
			CreatedAt:     base + 1,
		},
		{
			AgentID:       "agent-1",
			ProjectID:     "proj-a",
			Approach:      "Store sessions in PostgreSQL",
			FailureReason: "too slow under load",
			Context:       "load test showed 1200ms latency",
			CreatedAt:     base + 2,
		},
	}
	for i, r := range approaches {
		if _, err := s.InsertRejectedApproach(r); err != nil {
			t.Fatalf("InsertRejectedApproach[%d]: %v", i, err)
		}
	}

	// Search "caching" — matches Redis and Memcached approaches.
	results, err := s.SearchRejectedApproaches("agent-1", "proj-a", "caching", 20)
	if err != nil {
		t.Fatalf("SearchRejectedApproaches: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 caching matches, got %d", len(results))
	}

	// Search "6379" — matches blocker field.
	results, err = s.SearchRejectedApproaches("agent-1", "proj-a", "6379", 20)
	if err != nil {
		t.Fatalf("SearchRejectedApproaches blocker: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 blocker match, got %d", len(results))
	}

	// Search "load test" — matches context field.
	results, err = s.SearchRejectedApproaches("agent-1", "proj-a", "load test", 20)
	if err != nil {
		t.Fatalf("SearchRejectedApproaches context: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 context match, got %d", len(results))
	}

	// Empty query — returns all 3.
	results, err = s.SearchRejectedApproaches("agent-1", "proj-a", "", 20)
	if err != nil {
		t.Fatalf("SearchRejectedApproaches empty: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 total, got %d", len(results))
	}
}

// TestRejectedApproach_ProjectIsolation verifies that results from one project
// do not leak into another project's queries.
func TestRejectedApproach_ProjectIsolation(t *testing.T) {
	s := openTestStoreRej(t)

	_, err := s.InsertRejectedApproach(store.RejectedApproach{
		AgentID:       "agent-1",
		ProjectID:     "proj-a",
		Approach:      "approach in proj-a",
		FailureReason: "failed",
	})
	if err != nil {
		t.Fatalf("InsertRejectedApproach proj-a: %v", err)
	}

	results, err := s.GetRecentRejectedApproaches("agent-1", "proj-b", 10)
	if err != nil {
		t.Fatalf("GetRecentRejectedApproaches proj-b: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for proj-b isolation, got %d", len(results))
	}
}

// TestRejectedApproach_ZeroLimit uses the default limit when limit <= 0.
func TestRejectedApproach_ZeroLimit(t *testing.T) {
	s := openTestStoreRej(t)

	_, _ = s.InsertRejectedApproach(store.RejectedApproach{
		AgentID:       "agent-1",
		ProjectID:     "proj-a",
		Approach:      "something tried",
		FailureReason: "it failed",
	})

	// limit=0 should use default (5), not return 0 results.
	results, err := s.GetRecentRejectedApproaches("agent-1", "proj-a", 0)
	if err != nil {
		t.Fatalf("GetRecentRejectedApproaches: %v", err)
	}
	if len(results) == 0 {
		t.Error("limit=0 should fall back to default, not return 0 results")
	}
}

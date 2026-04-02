package store_test

import (
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// openTestStoreElog opens a Store backed by a temp dir for exploration log tests.
func openTestStoreElog(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestExplorationLog_AppendAndRetrieve(t *testing.T) {
	s := openTestStoreElog(t)

	entries := []store.ExplorationEntry{
		{
			SessionID:      "sess-1",
			ProjectID:      "proj-1",
			ToolName:       "get_context",
			EntityQueried:  "AuthService",
			QueryContext:   "modify",
			FindingSummary: "AuthService: 5 caller(s), 3 callee(s), 1 security constraint(s)",
		},
		{
			SessionID:      "sess-1",
			ProjectID:      "proj-1",
			ToolName:       "search",
			EntityQueried:  "auth login",
			FindingSummary: "3 result(s) for \"auth login\": top results: handleLogin, validateCreds, AuthService",
		},
		{
			SessionID:      "sess-1",
			ProjectID:      "proj-1",
			ToolName:       "get_impact",
			EntityQueried:  "UserRepo",
			FindingSummary: "UserRepo affects 8 entity(s) across 3 packages",
		},
	}

	for _, e := range entries {
		if err := s.AppendExplorationEntry(e); err != nil {
			t.Fatalf("AppendExplorationEntry: %v", err)
		}
	}

	got, err := s.GetSessionExplorationLog("sess-1", 10)
	if err != nil {
		t.Fatalf("GetSessionExplorationLog: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	// Most-recent-first ordering: last inserted is first.
	if got[0].ToolName != "get_impact" {
		t.Errorf("expected most recent entry to be get_impact, got %q", got[0].ToolName)
	}
	if got[0].EntityQueried != "UserRepo" {
		t.Errorf("EntityQueried mismatch: got %q", got[0].EntityQueried)
	}
	if got[0].FindingSummary == "" {
		t.Error("FindingSummary should not be empty for get_impact entry")
	}
}

func TestExplorationLog_Limit(t *testing.T) {
	s := openTestStoreElog(t)

	for i := 0; i < 15; i++ {
		_ = s.AppendExplorationEntry(store.ExplorationEntry{
			SessionID:     "sess-lim",
			ProjectID:     "proj-1",
			ToolName:      "search",
			EntityQueried: "query",
		})
	}

	got, err := s.GetSessionExplorationLog("sess-lim", 5)
	if err != nil {
		t.Fatalf("GetSessionExplorationLog: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("expected 5 entries (limit), got %d", len(got))
	}
}

func TestExplorationLog_GetExploredEntitySet(t *testing.T) {
	s := openTestStoreElog(t)

	toInsert := []string{"AuthService", "UserRepo", "PaymentProcessor", "AuthService"} // duplicate
	for _, e := range toInsert {
		_ = s.AppendExplorationEntry(store.ExplorationEntry{
			SessionID:     "sess-eset",
			ProjectID:     "proj-1",
			ToolName:      "get_context",
			EntityQueried: e,
		})
	}
	// Also insert an entry with empty entity — should be excluded.
	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID: "sess-eset",
		ProjectID: "proj-1",
		ToolName:  "validate",
	})

	entities, err := s.GetExploredEntitySet("sess-eset")
	if err != nil {
		t.Fatalf("GetExploredEntitySet: %v", err)
	}
	// AuthService appears twice but should deduplicate to 1.
	if len(entities) != 3 {
		t.Errorf("expected 3 unique entities, got %d: %v", len(entities), entities)
	}
	if _, ok := entities["AuthService"]; !ok {
		t.Error("expected AuthService in entity set")
	}
	if _, ok := entities["UserRepo"]; !ok {
		t.Error("expected UserRepo in entity set")
	}
	if _, ok := entities["PaymentProcessor"]; !ok {
		t.Error("expected PaymentProcessor in entity set")
	}
}

func TestExplorationLog_EmptySession(t *testing.T) {
	s := openTestStoreElog(t)

	got, err := s.GetSessionExplorationLog("nonexistent-session", 10)
	if err != nil {
		t.Fatalf("GetSessionExplorationLog: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 entries for unknown session, got %d", len(got))
	}

	entities, err := s.GetExploredEntitySet("nonexistent-session")
	if err != nil {
		t.Fatalf("GetExploredEntitySet: %v", err)
	}
	if len(entities) != 0 {
		t.Errorf("expected empty entity set for unknown session, got %d entries", len(entities))
	}
}

func TestExplorationLog_SessionIsolation(t *testing.T) {
	s := openTestStoreElog(t)

	// Session A entries.
	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:     "sess-a",
		ProjectID:     "proj-1",
		ToolName:      "get_context",
		EntityQueried: "AuthService",
	})
	// Session B entries.
	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:     "sess-b",
		ProjectID:     "proj-1",
		ToolName:      "search",
		EntityQueried: "PaymentProcessor",
	})

	gotA, err := s.GetSessionExplorationLog("sess-a", 10)
	if err != nil {
		t.Fatalf("GetSessionExplorationLog sess-a: %v", err)
	}
	if len(gotA) != 1 || gotA[0].EntityQueried != "AuthService" {
		t.Errorf("sess-a: unexpected entries %v", gotA)
	}

	gotB, err := s.GetSessionExplorationLog("sess-b", 10)
	if err != nil {
		t.Fatalf("GetSessionExplorationLog sess-b: %v", err)
	}
	if len(gotB) != 1 || gotB[0].EntityQueried != "PaymentProcessor" {
		t.Errorf("sess-b: unexpected entries %v", gotB)
	}
}

func TestExplorationLog_Prune(t *testing.T) {
	s := openTestStoreElog(t)

	// Insert an entry then prune with age=0 (everything older than now).
	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:     "sess-prune",
		ProjectID:     "proj-1",
		ToolName:      "search",
		EntityQueried: "stale",
	})

	// Wait 10ms so the entry has a clearly older timestamp than now.
	time.Sleep(10 * time.Millisecond)

	n, err := s.PruneExplorationLog(0)
	if err != nil {
		t.Fatalf("PruneExplorationLog: %v", err)
	}
	if n == 0 {
		t.Error("expected at least 1 row pruned")
	}

	got, err := s.GetSessionExplorationLog("sess-prune", 10)
	if err != nil {
		t.Fatalf("GetSessionExplorationLog after prune: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 entries after prune, got %d", len(got))
	}
}

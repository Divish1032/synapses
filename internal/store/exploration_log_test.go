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

// ── Sprint 25.4: cross-session exploration dedup tests ───────────────────────

func TestGetCrossSessionExplorations_BasicDedup(t *testing.T) {
	s := openTestStoreElog(t)

	// Prior session explored AuthService twice with findings.
	for i := 0; i < 2; i++ {
		_ = s.AppendExplorationEntry(store.ExplorationEntry{
			SessionID:      "prior-sess",
			ProjectID:      "proj-dedup",
			ToolName:       "get_context",
			EntityQueried:  "AuthService",
			FindingSummary: "AuthService: 5 caller(s), 1 security constraint(s)",
		})
	}
	// Different entity in same prior session — should NOT appear.
	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:      "prior-sess",
		ProjectID:      "proj-dedup",
		ToolName:       "search",
		EntityQueried:  "OtherEntity",
		FindingSummary: "OtherEntity: 2 results",
	})
	// Current session querying the same entity — should be EXCLUDED.
	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:      "current-sess",
		ProjectID:      "proj-dedup",
		ToolName:       "get_context",
		EntityQueried:  "AuthService",
		FindingSummary: "current session finding",
	})

	got, err := s.GetCrossSessionExplorations("proj-dedup", "current-sess", "AuthService", 5)
	if err != nil {
		t.Fatalf("GetCrossSessionExplorations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 prior-session entries for AuthService, got %d", len(got))
	}
	for _, e := range got {
		if e.EntityQueried != "AuthService" {
			t.Errorf("expected only AuthService entries, got %q", e.EntityQueried)
		}
		// current-sess must not appear
		if e.SessionID == "current-sess" {
			t.Error("current session must be excluded from cross-session results")
		}
	}
}

func TestGetCrossSessionExplorations_NonEmptyFindingFirst(t *testing.T) {
	s := openTestStoreElog(t)

	// Insert two prior-session entries: one with finding, one without.
	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:      "prior-a",
		ProjectID:      "proj-order",
		ToolName:       "search",
		EntityQueried:  "TokenValidator",
		FindingSummary: "", // no finding
	})
	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:      "prior-b",
		ProjectID:      "proj-order",
		ToolName:       "get_context",
		EntityQueried:  "TokenValidator",
		FindingSummary: "TokenValidator: 3 caller(s), validates JWT expiry",
	})

	got, err := s.GetCrossSessionExplorations("proj-order", "current", "TokenValidator", 5)
	if err != nil {
		t.Fatalf("GetCrossSessionExplorations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	// Entry with finding_summary should come first.
	if got[0].FindingSummary == "" {
		t.Errorf("expected entry with finding_summary first, got empty first: %q", got[0].FindingSummary)
	}
}

func TestGetCrossSessionExplorations_EmptyExcludeSession(t *testing.T) {
	s := openTestStoreElog(t)

	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:      "any-sess",
		ProjectID:      "proj-noexcl",
		ToolName:       "get_context",
		EntityQueried:  "PaymentProcessor",
		FindingSummary: "PaymentProcessor: 8 callers",
	})

	// Empty excludeSessionID → all sessions included.
	got, err := s.GetCrossSessionExplorations("proj-noexcl", "", "PaymentProcessor", 5)
	if err != nil {
		t.Fatalf("GetCrossSessionExplorations empty exclude: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 entry with empty exclude, got %d", len(got))
	}
}

func TestGetCrossSessionExplorations_EmptyResult(t *testing.T) {
	s := openTestStoreElog(t)

	got, err := s.GetCrossSessionExplorations("proj-empty", "sess-x", "NonExistentEntity", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %d entries", len(got))
	}
}

func TestGetTopExploredEntities_AggregatesCorrectly(t *testing.T) {
	s := openTestStoreElog(t)

	// AuthService: explored 4 times across 2 sessions.
	for i := 0; i < 2; i++ {
		_ = s.AppendExplorationEntry(store.ExplorationEntry{
			SessionID:      "sess-old-1",
			ProjectID:      "proj-top",
			ToolName:       "get_context",
			EntityQueried:  "AuthService",
			FindingSummary: "AuthService: 5 callers (old finding)",
		})
	}
	for i := 0; i < 2; i++ {
		_ = s.AppendExplorationEntry(store.ExplorationEntry{
			SessionID:      "sess-old-2",
			ProjectID:      "proj-top",
			ToolName:       "get_context",
			EntityQueried:  "AuthService",
			FindingSummary: "AuthService: 5 callers, 1 security constraint",
		})
	}
	// UserRepo: explored 2 times in one session.
	for i := 0; i < 2; i++ {
		_ = s.AppendExplorationEntry(store.ExplorationEntry{
			SessionID:      "sess-old-1",
			ProjectID:      "proj-top",
			ToolName:       "get_context",
			EntityQueried:  "UserRepo",
			FindingSummary: "UserRepo affects 8 entities",
		})
	}
	// TrivialEntity: explored only once — should be excluded with minHits=2.
	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:     "sess-old-1",
		ProjectID:     "proj-top",
		ToolName:      "search",
		EntityQueried: "TrivialEntity",
	})
	// Current session — must be excluded.
	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:      "current-sess",
		ProjectID:      "proj-top",
		ToolName:       "get_context",
		EntityQueried:  "AuthService",
		FindingSummary: "current session should not appear",
	})

	got, err := s.GetTopExploredEntities("proj-top", "current-sess", 2, 10)
	if err != nil {
		t.Fatalf("GetTopExploredEntities: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entities (AuthService + UserRepo), got %d: %v", len(got), got)
	}
	// AuthService has 4 hits — should be first.
	if got[0].Entity != "AuthService" {
		t.Errorf("expected AuthService first (highest hit count), got %q", got[0].Entity)
	}
	if got[0].HitCount != 4 {
		t.Errorf("AuthService hit_count: got %d, want 4", got[0].HitCount)
	}
	if got[0].SessionCount != 2 {
		t.Errorf("AuthService session_count: got %d, want 2", got[0].SessionCount)
	}
	if got[0].TopFinding == "" {
		t.Error("AuthService top_finding should not be empty")
	}
	// Top finding must not be from the current session.
	if got[0].TopFinding == "current session should not appear" {
		t.Error("current session finding must not appear in top_finding")
	}
	// TrivialEntity must not appear (minHits=2, it has 1 hit).
	for _, e := range got {
		if e.Entity == "TrivialEntity" {
			t.Error("TrivialEntity should be filtered by minHits=2")
		}
	}
}

func TestGetTopExploredEntities_RespectsLimit(t *testing.T) {
	s := openTestStoreElog(t)

	// Insert 5 entities each explored 2 times.
	entities := []string{"A", "B", "C", "D", "E"}
	for _, name := range entities {
		for i := 0; i < 2; i++ {
			_ = s.AppendExplorationEntry(store.ExplorationEntry{
				SessionID:     "prior",
				ProjectID:     "proj-limit",
				ToolName:      "get_context",
				EntityQueried: name,
			})
		}
	}

	got, err := s.GetTopExploredEntities("proj-limit", "current", 1, 3)
	if err != nil {
		t.Fatalf("GetTopExploredEntities: %v", err)
	}
	if len(got) > 3 {
		t.Errorf("expected at most 3 results (limit=3), got %d", len(got))
	}
}

func TestGetTopExploredEntities_EmptyResult(t *testing.T) {
	s := openTestStoreElog(t)

	got, err := s.GetTopExploredEntities("proj-empty", "sess-x", 1, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %d entries", len(got))
	}
}

func TestGetTopExploredEntities_ProjectIsolation(t *testing.T) {
	s := openTestStoreElog(t)

	// Entries for proj-A.
	for i := 0; i < 3; i++ {
		_ = s.AppendExplorationEntry(store.ExplorationEntry{
			SessionID:     "sess-a",
			ProjectID:     "proj-A",
			ToolName:      "get_context",
			EntityQueried: "SharedEntity",
		})
	}
	// Entries for proj-B.
	_ = s.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:     "sess-b",
		ProjectID:     "proj-B",
		ToolName:      "get_context",
		EntityQueried: "SharedEntity",
	})

	gotA, err := s.GetTopExploredEntities("proj-A", "", 1, 10)
	if err != nil {
		t.Fatalf("proj-A: %v", err)
	}
	if len(gotA) != 1 || gotA[0].HitCount != 3 {
		t.Errorf("proj-A: expected 1 entity with 3 hits, got %v", gotA)
	}

	gotB, err := s.GetTopExploredEntities("proj-B", "", 1, 10)
	if err != nil {
		t.Fatalf("proj-B: %v", err)
	}
	if len(gotB) != 1 || gotB[0].HitCount != 1 {
		t.Errorf("proj-B: expected 1 entity with 1 hit, got %v", gotB)
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

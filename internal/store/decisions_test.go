package store_test

import (
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// openTestStoreDec opens a fresh Store for decision tests.
func openTestStoreDec(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestDecision_Insert verifies that a decision is created with all fields
// round-tripping through the DB correctly.
func TestDecision_Insert(t *testing.T) {
	s := openTestStoreDec(t)

	id, err := s.InsertDecision(store.Decision{
		AgentID:      "agent-1",
		ProjectID:    "proj-a",
		Choice:       "Use repository pattern for database access",
		Alternatives: "direct DB calls from handlers; active record",
		Reasoning:    "Testability: repository pattern allows mock injection without global state. Used in 12/14 existing packages.",
		Context:      "Adding user management service, Sprint 24",
	})
	if err != nil {
		t.Fatalf("InsertDecision: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}

	got, err := s.GetDecisionByID(id)
	if err != nil {
		t.Fatalf("GetDecisionByID: %v", err)
	}
	if got.Choice != "Use repository pattern for database access" {
		t.Errorf("choice mismatch: %q", got.Choice)
	}
	if got.Alternatives != "direct DB calls from handlers; active record" {
		t.Errorf("alternatives mismatch: %q", got.Alternatives)
	}
	if got.Reasoning != "Testability: repository pattern allows mock injection without global state. Used in 12/14 existing packages." {
		t.Errorf("reasoning mismatch: %q", got.Reasoning)
	}
	if got.Context != "Adding user management service, Sprint 24" {
		t.Errorf("context mismatch: %q", got.Context)
	}
	if got.AgentID != "agent-1" {
		t.Errorf("agent_id mismatch: %q", got.AgentID)
	}
	if got.CreatedAt == 0 {
		t.Error("expected non-zero created_at")
	}
}

// TestDecision_Insert_MissingChoice verifies that omitting the choice field returns an error.
func TestDecision_Insert_MissingChoice(t *testing.T) {
	s := openTestStoreDec(t)

	_, err := s.InsertDecision(store.Decision{
		AgentID:   "agent-1",
		ProjectID: "proj-a",
		// Choice intentionally omitted
		Reasoning: "some reasoning",
	})
	if err == nil {
		t.Fatal("expected error for missing choice, got nil")
	}
}

// TestDecision_GetRecent verifies that GetRecentDecisions returns decisions
// newest-first and that the limit parameter is respected.
func TestDecision_GetRecent(t *testing.T) {
	s := openTestStoreDec(t)

	// Use explicit Unix timestamps (seconds) to guarantee stable ordering.
	base := time.Now().Unix()
	choices := []string{"decision A", "decision B", "decision C", "decision D", "decision E"}
	for i, choice := range choices {
		_, err := s.InsertDecision(store.Decision{
			AgentID:   "a",
			ProjectID: "p",
			Choice:    choice,
			Reasoning: "reason",
			CreatedAt: base + int64(i), // strictly increasing
		})
		if err != nil {
			t.Fatalf("InsertDecision %d: %v", i, err)
		}
	}

	// Limit to 3 — should return the 3 most recent (D, E … C in desc order).
	got, err := s.GetRecentDecisions("a", "p", 3)
	if err != nil {
		t.Fatalf("GetRecentDecisions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 results (limit), got %d", len(got))
	}
	// Newest first: last inserted is "decision E" (highest created_at).
	if got[0].Choice != "decision E" {
		t.Errorf("expected newest-first ordering; first result: %q", got[0].Choice)
	}
}

// TestDecision_Search verifies that SearchDecisions finds decisions by keyword
// across choice, reasoning, context, and alternatives fields.
func TestDecision_Search(t *testing.T) {
	s := openTestStoreDec(t)

	_, _ = s.InsertDecision(store.Decision{
		AgentID:   "a",
		ProjectID: "p",
		Choice:    "Use JWT for authentication",
		Reasoning: "RS256 asymmetric keys — public key verifiable without secret",
		Context:   "API gateway auth refactor",
	})
	_, _ = s.InsertDecision(store.Decision{
		AgentID:   "a",
		ProjectID: "p",
		Choice:    "Use PostgreSQL for persistent storage",
		Reasoning: "ACID compliance needed for financial transactions",
		Context:   "payment service implementation",
	})
	_, _ = s.InsertDecision(store.Decision{
		AgentID:      "a",
		ProjectID:    "p",
		Choice:       "Use Redis for session caching",
		Alternatives: "in-memory map; PostgreSQL sessions; JWT stateless",
		Reasoning:    "Sub-millisecond reads required for auth middleware hot path",
		Context:      "session management",
	})

	// Search by choice text
	res, err := s.SearchDecisions("a", "p", "JWT", 20)
	if err != nil {
		t.Fatalf("SearchDecisions (JWT): %v", err)
	}
	// Matches "Use JWT for authentication" in choice AND "JWT stateless" in alternatives of Redis decision
	if len(res) < 1 {
		t.Errorf("expected at least 1 JWT match, got %d", len(res))
	}
	// First result should be one of the JWT matches
	foundJWT := false
	for _, d := range res {
		if d.Choice == "Use JWT for authentication" {
			foundJWT = true
		}
	}
	if !foundJWT {
		t.Error("expected to find 'Use JWT for authentication' in JWT search results")
	}

	// Search by reasoning text
	res2, err := s.SearchDecisions("a", "p", "ACID", 20)
	if err != nil {
		t.Fatalf("SearchDecisions (ACID): %v", err)
	}
	if len(res2) != 1 {
		t.Fatalf("expected 1 ACID match, got %d", len(res2))
	}
	if res2[0].Choice != "Use PostgreSQL for persistent storage" {
		t.Errorf("wrong match: %q", res2[0].Choice)
	}

	// Empty query → returns all (chronological browse)
	all, err := s.SearchDecisions("a", "p", "", 20)
	if err != nil {
		t.Fatalf("SearchDecisions (empty): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 results for empty query, got %d", len(all))
	}
}

// TestDecision_ProjectIsolation verifies that decisions from different projects
// don't appear in each other's queries.
func TestDecision_ProjectIsolation(t *testing.T) {
	s := openTestStoreDec(t)

	_, _ = s.InsertDecision(store.Decision{AgentID: "a", ProjectID: "proj-1", Choice: "proj1 decision"})
	_, _ = s.InsertDecision(store.Decision{AgentID: "a", ProjectID: "proj-2", Choice: "proj2 decision"})

	proj1, err := s.GetRecentDecisions("a", "proj-1", 20)
	if err != nil {
		t.Fatalf("GetRecentDecisions proj-1: %v", err)
	}
	if len(proj1) != 1 || proj1[0].Choice != "proj1 decision" {
		t.Errorf("project isolation failed for proj-1: %+v", proj1)
	}

	proj2, _ := s.GetRecentDecisions("a", "proj-2", 20)
	if len(proj2) != 1 || proj2[0].Choice != "proj2 decision" {
		t.Errorf("project isolation failed for proj-2: %+v", proj2)
	}
}

// TestDecision_TimestampsSetOnInsert verifies that created_at is set automatically.
func TestDecision_TimestampsSetOnInsert(t *testing.T) {
	s := openTestStoreDec(t)
	before := time.Now().Unix()

	id, _ := s.InsertDecision(store.Decision{
		AgentID:   "a",
		ProjectID: "p",
		Choice:    "test decision",
	})
	got, err := s.GetDecisionByID(id)
	if err != nil {
		t.Fatalf("GetDecisionByID: %v", err)
	}
	after := time.Now().Unix()

	if got.CreatedAt < before || got.CreatedAt > after {
		t.Errorf("created_at %d not in expected range [%d, %d]", got.CreatedAt, before, after)
	}
}

// TestDecision_RowCap verifies insert succeeds below the cap (cap enforcement
// is covered by the integration path; unit test validates happy path).
func TestDecision_RowCap(t *testing.T) {
	s := openTestStoreDec(t)

	id, err := s.InsertDecision(store.Decision{
		AgentID:   "cap-agent",
		ProjectID: "cap-proj",
		Choice:    "decision below cap",
	})
	if err != nil {
		t.Fatalf("unexpected error below cap: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty ID")
	}
}

// TestDecision_GetDecisionByID_NotFound verifies the error path for a missing ID.
func TestDecision_GetDecisionByID_NotFound(t *testing.T) {
	s := openTestStoreDec(t)

	_, err := s.GetDecisionByID("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for missing decision, got nil")
	}
}

// TestDecision_MinimalFields verifies that only Choice is required and all other
// fields default to empty strings gracefully.
func TestDecision_MinimalFields(t *testing.T) {
	s := openTestStoreDec(t)

	id, err := s.InsertDecision(store.Decision{
		AgentID:   "a",
		ProjectID: "p",
		Choice:    "Use table-driven tests",
	})
	if err != nil {
		t.Fatalf("InsertDecision minimal: %v", err)
	}

	got, err := s.GetDecisionByID(id)
	if err != nil {
		t.Fatalf("GetDecisionByID: %v", err)
	}
	if got.Alternatives != "" {
		t.Errorf("alternatives should default to empty, got %q", got.Alternatives)
	}
	if got.Reasoning != "" {
		t.Errorf("reasoning should default to empty, got %q", got.Reasoning)
	}
	if got.Context != "" {
		t.Errorf("context should default to empty, got %q", got.Context)
	}
}

// TestDecision_Search_LikeEscape verifies that % and _ metacharacters in the
// search query are treated as literals, not SQLite LIKE wildcards.
func TestDecision_Search_LikeEscape(t *testing.T) {
	s := openTestStoreDec(t)

	// Insert a decision whose choice contains a literal underscore.
	_, _ = s.InsertDecision(store.Decision{
		AgentID:   "a",
		ProjectID: "p",
		Choice:    "Use jwt_rs256 token format",
	})
	// Insert a decoy that would match "_" as a wildcard (any single char).
	_, _ = s.InsertDecision(store.Decision{
		AgentID:   "a",
		ProjectID: "p",
		Choice:    "Use jwtXrs256 token format",
	})

	// Searching for "jwt_rs256" should only find the first (literal underscore).
	res, err := s.SearchDecisions("a", "p", "jwt_rs256", 20)
	if err != nil {
		t.Fatalf("SearchDecisions: %v", err)
	}
	if len(res) != 1 {
		t.Errorf("expected 1 literal-underscore match, got %d (wildcard escaping failed)", len(res))
	}
	if len(res) > 0 && res[0].Choice != "Use jwt_rs256 token format" {
		t.Errorf("wrong match: %q", res[0].Choice)
	}
}

package store_test

import (
	"testing"
)

// ── GetConflicts ──────────────────────────────────────────────────────────────

func TestGetConflicts_NoClaimsReturnsNil(t *testing.T) {
	st := openTestStore(t)

	conflicts, err := st.GetConflicts("lonely-agent")
	if err != nil {
		t.Fatalf("GetConflicts: %v", err)
	}
	if len(conflicts) != 0 {
		t.Errorf("expected 0 conflicts for agent with no claims, got %d", len(conflicts))
	}
}

func TestGetConflicts_OverlappingScopes(t *testing.T) {
	st := openTestStore(t)

	// Agent A claims pkg/auth.
	_, err := st.ClaimWork("agent-a", "pkg/auth", "directory", 30)
	if err != nil {
		t.Fatalf("ClaimWork agent-a: %v", err)
	}

	// Agent B claims pkg/auth (same scope).
	_, _ = st.ClaimWork("agent-b", "pkg/auth", "directory", 30)

	conflicts, err := st.GetConflicts("agent-a")
	if err != nil {
		t.Fatalf("GetConflicts: %v", err)
	}
	if len(conflicts) == 0 {
		t.Error("expected conflict for overlapping pkg/auth claim")
	}
	for _, c := range conflicts {
		if c.AgentID != "agent-b" {
			t.Errorf("unexpected conflicting agent %q", c.AgentID)
		}
	}
}

// ── ReleaseClaims ─────────────────────────────────────────────────────────────

func TestReleaseClaims_ClearsAllScopes(t *testing.T) {
	st := openTestStore(t)

	agentID := "release-me"
	_, _ = st.ClaimWork(agentID, "pkg/auth", "directory", 30)
	_, _ = st.ClaimWork(agentID, "pkg/api", "directory", 30)

	if err := st.ReleaseClaims(agentID); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}

	claims, err := st.GetMyClaims(agentID)
	if err != nil {
		t.Fatalf("GetMyClaims after release: %v", err)
	}
	if len(claims) != 0 {
		t.Errorf("expected 0 claims after release, got %d", len(claims))
	}
}

func TestReleaseClaims_OnlyReleasesOwnClaims(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.ClaimWork("owner", "pkg/auth", "directory", 30)
	_, _ = st.ClaimWork("other", "pkg/api", "directory", 30)

	_ = st.ReleaseClaims("owner")

	// other agent's claim should remain.
	otherClaims, _ := st.GetMyClaims("other")
	if len(otherClaims) == 0 {
		t.Error("other agent's claims should not be released")
	}
}

// ── GetMyClaims ───────────────────────────────────────────────────────────────

func TestGetMyClaims_ReturnsClaimed(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.ClaimWork("my-agent", "pkg/auth", "directory", 30)
	_, _ = st.ClaimWork("my-agent", "cmd/server", "directory", 30)

	claims, err := st.GetMyClaims("my-agent")
	if err != nil {
		t.Fatalf("GetMyClaims: %v", err)
	}
	if len(claims) != 2 {
		t.Errorf("expected 2 claims, got %d", len(claims))
	}
}

func TestGetMyClaims_Empty(t *testing.T) {
	st := openTestStore(t)

	claims, err := st.GetMyClaims("nobody")
	if err != nil {
		t.Fatalf("GetMyClaims empty: %v", err)
	}
	if len(claims) != 0 {
		t.Errorf("expected 0 claims, got %d", len(claims))
	}
}

// ── GetAllClaims ──────────────────────────────────────────────────────────────

func TestGetAllClaims_AcrossAgents(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.ClaimWork("agent-1", "pkg/auth", "directory", 30)
	_, _ = st.ClaimWork("agent-2", "pkg/api", "directory", 30)
	_, _ = st.ClaimWork("agent-3", "cmd/server", "directory", 30)

	all, err := st.GetAllClaims()
	if err != nil {
		t.Fatalf("GetAllClaims: %v", err)
	}
	if len(all) < 3 {
		t.Errorf("expected at least 3 claims across agents, got %d", len(all))
	}
}

func TestGetAllClaims_Empty(t *testing.T) {
	st := openTestStore(t)

	all, err := st.GetAllClaims()
	if err != nil {
		t.Fatalf("GetAllClaims empty: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected 0 claims on fresh store, got %d", len(all))
	}
}

// ── GetEvents ─────────────────────────────────────────────────────────────────

func TestGetEvents_AfterAppend(t *testing.T) {
	st := openTestStore(t)

	_ = st.AppendEvent("session_start", "agent-1", `{"project":"test"}`)
	_ = st.AppendEvent("file_change", "agent-1", `{"file":"auth.go"}`)
	_ = st.AppendEvent("task_update", "agent-2", `{"task_id":"xyz"}`)

	events, latestSeq, err := st.GetEvents(0, nil, 50)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) < 3 {
		t.Errorf("expected at least 3 events, got %d", len(events))
	}
	if latestSeq <= 0 {
		t.Error("expected latestSeq > 0")
	}
}

func TestGetEvents_TypeFilter(t *testing.T) {
	st := openTestStore(t)

	_ = st.AppendEvent("session_start", "a", `{}`)
	_ = st.AppendEvent("file_change", "a", `{}`)
	_ = st.AppendEvent("session_start", "b", `{}`)

	events, _, err := st.GetEvents(0, []string{"session_start"}, 50)
	if err != nil {
		t.Fatalf("GetEvents typed: %v", err)
	}
	for _, e := range events {
		if e.Type != "session_start" {
			t.Errorf("type filter leaked event type=%q", e.Type)
		}
	}
	if len(events) < 2 {
		t.Errorf("expected at least 2 session_start events, got %d", len(events))
	}
}

func TestGetEvents_SinceCursor(t *testing.T) {
	st := openTestStore(t)

	_ = st.AppendEvent("e1", "a", `{}`)
	_ = st.AppendEvent("e2", "a", `{}`)
	_ = st.AppendEvent("e3", "a", `{}`)

	all, _, _ := st.GetEvents(0, nil, 50)
	firstSeq := all[0].Seq

	after, _, err := st.GetEvents(firstSeq, nil, 50)
	if err != nil {
		t.Fatalf("GetEvents since cursor: %v", err)
	}
	if len(after) != len(all)-1 {
		t.Errorf("expected %d events after seq %d, got %d", len(all)-1, firstSeq, len(after))
	}
}

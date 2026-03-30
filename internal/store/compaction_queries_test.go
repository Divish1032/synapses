package store

import (
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── GetEpisodesByTimeWindow ─────────────────────────────────────────────────

func TestGetEpisodesByTimeWindow_Empty(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	eps, err := st.GetEpisodesByTimeWindow("agent-1", "", time.Now().Add(-1*time.Hour), time.Now(), "", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(eps) != 0 {
		t.Errorf("expected 0 episodes, got %d", len(eps))
	}
}

func TestGetEpisodesByTimeWindow_FiltersCorrectly(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	// Insert episodes at different times
	st.RememberEpisode(Episode{AgentID: "a1", CreatedAt: now.Add(-30 * time.Minute).Unix(), EpisodeType: "decision", Decision: "chose JWT"})
	st.RememberEpisode(Episode{AgentID: "a1", CreatedAt: now.Add(-2 * time.Hour).Unix(), EpisodeType: "decision", Decision: "old decision"})
	st.RememberEpisode(Episode{AgentID: "a1", CreatedAt: now.Add(-10 * time.Minute).Unix(), EpisodeType: "failure", Decision: "nil panic"})
	st.RememberEpisode(Episode{AgentID: "a2", CreatedAt: now.Add(-10 * time.Minute).Unix(), EpisodeType: "decision", Decision: "other agent"})

	// Query last hour for a1 decisions only
	eps, err := st.GetEpisodesByTimeWindow("a1", "", now.Add(-1*time.Hour), now, "decision", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 episode, got %d", len(eps))
	}
	if eps[0].Decision != "chose JWT" {
		t.Errorf("unexpected decision: %s", eps[0].Decision)
	}

	// Query all types for a1
	eps, err = st.GetEpisodesByTimeWindow("a1", "", now.Add(-1*time.Hour), now, "", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(eps) != 2 {
		t.Errorf("expected 2 episodes (decision + failure), got %d", len(eps))
	}
}

func TestGetEpisodesByTimeWindow_ProjectIDPreventsLeakage(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	// Same agent, different projects
	st.RememberEpisode(Episode{AgentID: "a1", ProjectID: "proj-1", CreatedAt: now.Add(-5 * time.Minute).Unix(), EpisodeType: "decision", Decision: "proj1 decision"})
	st.RememberEpisode(Episode{AgentID: "a1", ProjectID: "proj-2", CreatedAt: now.Add(-5 * time.Minute).Unix(), EpisodeType: "decision", Decision: "proj2 decision"})

	// Query with project filter should only return proj-1
	eps, err := st.GetEpisodesByTimeWindow("a1", "proj-1", now.Add(-1*time.Hour), now, "", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 episode for proj-1, got %d", len(eps))
	}
	if eps[0].Decision != "proj1 decision" {
		t.Errorf("unexpected decision: %s", eps[0].Decision)
	}
}

// ── GetMemoriesForEntitySet ─────────────────────────────────────────────────

func TestGetMemoriesForEntitySet_Empty(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mems, err := st.GetMemoriesForEntitySet(nil, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mems) != 0 {
		t.Errorf("expected 0 memories for nil input, got %d", len(mems))
	}

	mems, err = st.GetMemoriesForEntitySet([]string{}, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mems) != 0 {
		t.Errorf("expected 0 memories for empty input, got %d", len(mems))
	}
}

func TestGetMemoriesForEntitySet_ReturnsMatches(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	st.InsertMemory(Memory{Tier: TierEntity, Content: "AuthService is critical", EntityID: "AuthService", Source: SourceManual})
	st.InsertMemory(Memory{Tier: TierEntity, Content: "TokenStore uses Redis", EntityID: "TokenStore", Source: SourceManual})
	st.InsertMemory(Memory{Tier: TierProject, Content: "project memory", Source: SourceManual})

	mems, err := st.GetMemoriesForEntitySet([]string{"AuthService", "TokenStore", "Missing"}, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mems) != 2 {
		t.Errorf("expected 2 entity memories, got %d", len(mems))
	}
}

// ── GetRulesForFiles ────────────────────────────────────────────────────────

func TestGetRulesForFiles_Empty(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rules, err := st.GetRulesForFiles(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected 0 rules for nil input, got %d", len(rules))
	}
}

func TestGetRulesForFiles_MatchesPattern(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	st.UpsertDynamicRule(config.Rule{
		ID:          "no-handler-db",
		Description: "Handlers must not access DB directly",
		Severity:    "error",
		ForbiddenEdge: config.ForbiddenEdge{
			FromFilePattern: "pkg/api/*.go",
			ToFilePattern:   "pkg/repo/*.go",
		},
	})

	rules, err := st.GetRulesForFiles([]string{"pkg/api/handler.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 1 {
		t.Errorf("expected 1 matching rule, got %d", len(rules))
	}

	// No match
	rules, err = st.GetRulesForFiles([]string{"pkg/service/auth.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected 0 matching rules, got %d", len(rules))
	}
}

// ── GetViolationsForFiles ───────────────────────────────────────────────────

func TestGetViolationsForFiles_Empty(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	vs, err := st.GetViolationsForFiles(nil, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vs) != 0 {
		t.Errorf("expected 0 violations, got %d", len(vs))
	}
}

func TestGetViolationsForFiles_FindsByFile(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	st.LogViolations([]config.Violation{
		{RuleID: "r1", Severity: "error", FromNode: graph.NodeID("Handler"), ToNode: graph.NodeID("Repo"),
			FromFile: "api/handler.go", ToFile: "repo/user.go"},
		{RuleID: "r2", Severity: "warning", FromNode: graph.NodeID("A"), ToNode: graph.NodeID("B"),
			FromFile: "other/file.go", ToFile: "other/file2.go"},
	})

	vs, err := st.GetViolationsForFiles([]string{"api/handler.go"}, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vs) != 1 {
		t.Errorf("expected 1 violation matching api/handler.go, got %d", len(vs))
	}
	if len(vs) > 0 && vs[0].RuleID != "r1" {
		t.Errorf("expected rule_id=r1, got %s", vs[0].RuleID)
	}
}

// ── GetSession ──────────────────────────────────────────────────────────────

func TestGetSession_NotFound(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	info, err := st.GetSession("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info != nil {
		t.Error("expected nil for nonexistent session")
	}
}

func TestGetSession_EmptyID(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	info, err := st.GetSession("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info != nil {
		t.Error("expected nil for empty session ID")
	}
}

// ── SessionLedgerEntityCounts ───────────────────────────────────────────────

func TestSessionLedgerEntityCounts_Empty(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	counts, err := st.SessionLedgerEntityCounts("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(counts) != 0 {
		t.Errorf("expected empty counts, got %d", len(counts))
	}
}

func TestSessionLedgerEntityCounts_CountsCorrectly(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Insert multiple entries with overlapping entities
	st.AppendLedger(LedgerEntry{SessionID: "s1", ToolName: "get_context", EntityIDs: []string{"Auth", "Token"}})
	st.AppendLedger(LedgerEntry{SessionID: "s1", ToolName: "get_impact", EntityIDs: []string{"Auth"}})
	st.AppendLedger(LedgerEntry{SessionID: "s2", ToolName: "get_context", EntityIDs: []string{"Auth"}}) // different session

	counts, err := st.SessionLedgerEntityCounts("s1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counts["Auth"] != 2 {
		t.Errorf("expected Auth count=2, got %d", counts["Auth"])
	}
	if counts["Token"] != 1 {
		t.Errorf("expected Token count=1, got %d", counts["Token"])
	}
}

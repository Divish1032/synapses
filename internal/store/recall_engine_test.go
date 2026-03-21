package store

import (
	"fmt"
	"testing"
	"time"
)

func openRecallTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// ── RecencyDecayScore ─────────────────────────────────────────────────────────

func TestRecencyDecayScore_JustCreated(t *testing.T) {
	t.Parallel()
	score := RecencyDecayScore(time.Now(), 168, 1)
	if score < 0.99 || score > 1.01 {
		t.Errorf("just-created score = %f, want ~1.0", score)
	}
}

func TestRecencyDecayScore_OneHalfLife(t *testing.T) {
	t.Parallel()
	created := time.Now().Add(-168 * time.Hour) // 1 week ago
	score := RecencyDecayScore(created, 168, 1)
	if score < 0.49 || score > 0.51 {
		t.Errorf("one half-life score = %f, want ~0.5", score)
	}
}

func TestRecencyDecayScore_FutureTimestamp(t *testing.T) {
	t.Parallel()
	created := time.Now().Add(1 * time.Hour) // future
	score := RecencyDecayScore(created, 168, 1)
	if score < 0.99 || score > 1.01 {
		t.Errorf("future timestamp score = %f, want ~1.0", score)
	}
}

func TestRecencyDecayScore_ZeroHalfLife_DefaultsTo168(t *testing.T) {
	t.Parallel()
	created := time.Now().Add(-168 * time.Hour)
	score := RecencyDecayScore(created, 0, 1)
	if score < 0.49 || score > 0.51 {
		t.Errorf("zero half-life (default) score = %f, want ~0.5", score)
	}
}

func TestRecencyDecayScore_NegativeHalfLife_DefaultsTo168(t *testing.T) {
	t.Parallel()
	score := RecencyDecayScore(time.Now(), -10, 1)
	if score < 0.99 || score > 1.01 {
		t.Errorf("negative half-life score = %f, want ~1.0", score)
	}
}

// ── ACT-R frequency-weighted decay ───────────────────────────────────────────

func TestRecencyDecayScore_FrequencyBoost_20xOutranks1x(t *testing.T) {
	t.Parallel()
	// Core ACT-R requirement: a memory accessed 20 times outranks one accessed once at same age.
	created := time.Now().Add(-168 * time.Hour) // 1 week ago
	score1 := RecencyDecayScore(created, 168, 1)
	score20 := RecencyDecayScore(created, 168, 20)
	if score20 <= score1 {
		t.Errorf("20-access score (%f) should exceed 1-access score (%f)", score20, score1)
	}
	// 20 accesses at 1 halfLife should be significantly higher than ~0.5.
	if score20 < 0.75 {
		t.Errorf("20-access score at 1 halfLife = %f, want ≥0.75", score20)
	}
}

func TestRecencyDecayScore_FrequencyBoost_Monotonic(t *testing.T) {
	t.Parallel()
	// More accesses → higher score at the same age (monotonically increasing).
	created := time.Now().Add(-168 * time.Hour)
	prev := 0.0
	for _, n := range []int{1, 2, 5, 10, 50, 100} {
		score := RecencyDecayScore(created, 168, n)
		if score <= prev {
			t.Errorf("access_count=%d score=%f should exceed previous %f", n, score, prev)
		}
		prev = score
	}
}

func TestRecencyDecayScore_ZeroAccessCount_BackwardCompat(t *testing.T) {
	t.Parallel()
	// access_count=0 should behave identically to access_count=1.
	// Use a large age to minimize wall-clock drift between calls.
	created := time.Now().Add(-168 * time.Hour)
	score0 := RecencyDecayScore(created, 168, 0)
	score1 := RecencyDecayScore(created, 168, 1)
	diff := score0 - score1
	if diff < 0 {
		diff = -diff
	}
	if diff > 1e-9 {
		t.Errorf("access_count=0 score (%f) != access_count=1 score (%f), diff=%e", score0, score1, diff)
	}
}

// ── TierHalfLife ─────────────────────────────────────────────────────────────

func TestTierHalfLife_SessionLog(t *testing.T) {
	t.Parallel()
	if h := TierHalfLife(TierSessionLog, SourceAuto); h != 72 {
		t.Errorf("session_log half-life = %f, want 72", h)
	}
	if h := TierHalfLife(TierSessionLog, SourceManual); h != 72 {
		t.Errorf("session_log+manual half-life = %f, want 72 (source irrelevant for session_log)", h)
	}
}

func TestTierHalfLife_Project(t *testing.T) {
	t.Parallel()
	if h := TierHalfLife(TierProject, SourceManual); h != 336 {
		t.Errorf("project half-life = %f, want 336", h)
	}
}

func TestTierHalfLife_EntityAuto(t *testing.T) {
	t.Parallel()
	if h := TierHalfLife(TierEntity, SourceAuto); h != 168 {
		t.Errorf("entity+auto half-life = %f, want 168", h)
	}
	if h := TierHalfLife(TierEntity, SourceExtracted); h != 168 {
		t.Errorf("entity+extracted half-life = %f, want 168", h)
	}
}

func TestTierHalfLife_EntityManual(t *testing.T) {
	t.Parallel()
	if h := TierHalfLife(TierEntity, SourceManual); h != 504 {
		t.Errorf("entity+manual half-life = %f, want 504", h)
	}
}

func TestTierHalfLife_UnknownTier(t *testing.T) {
	t.Parallel()
	if h := TierHalfLife("unknown", ""); h != 168 {
		t.Errorf("unknown tier half-life = %f, want 168 (default)", h)
	}
}

// ── DecayedImportanceScore: differential tier decay ──────────────────────────

func TestDecayedImportanceScore_SessionLogDecaysFaster(t *testing.T) {
	t.Parallel()
	// At the same age, session_log (72h half-life) should score lower than
	// entity+auto (168h half-life) because it decays faster.
	age := time.Now().UTC().Add(-168 * time.Hour).Format(time.RFC3339) // 1 week old
	sessionMem := Memory{Tier: TierSessionLog, Source: SourceAuto, LastAccessedAt: age, AccessCount: 1}
	entityMem := Memory{Tier: TierEntity, Source: SourceAuto, LastAccessedAt: age, AccessCount: 1}

	sessionScore := DecayedImportanceScore(sessionMem, 0)
	entityScore := DecayedImportanceScore(entityMem, 0)

	if sessionScore >= entityScore {
		t.Errorf("session_log score (%f) should be lower than entity+auto score (%f) at same age", sessionScore, entityScore)
	}
}

func TestDecayedImportanceScore_ProjectDecaysSlower(t *testing.T) {
	t.Parallel()
	age := time.Now().UTC().Add(-168 * time.Hour).Format(time.RFC3339)
	entityMem := Memory{Tier: TierEntity, Source: SourceAuto, LastAccessedAt: age, AccessCount: 1}
	projectMem := Memory{Tier: TierProject, Source: SourceManual, LastAccessedAt: age, AccessCount: 1}

	entityScore := DecayedImportanceScore(entityMem, 0)
	projectScore := DecayedImportanceScore(projectMem, 0)

	if projectScore <= entityScore {
		t.Errorf("project score (%f) should be higher than entity+auto score (%f) at same age", projectScore, entityScore)
	}
}

func TestDecayedImportanceScore_ManualEntityDecaysSlowest(t *testing.T) {
	t.Parallel()
	age := time.Now().UTC().Add(-336 * time.Hour).Format(time.RFC3339) // 2 weeks old
	autoMem := Memory{Tier: TierEntity, Source: SourceAuto, LastAccessedAt: age, AccessCount: 1}
	manualMem := Memory{Tier: TierEntity, Source: SourceManual, LastAccessedAt: age, AccessCount: 1}

	autoScore := DecayedImportanceScore(autoMem, 0)
	manualScore := DecayedImportanceScore(manualMem, 0)

	if manualScore <= autoScore {
		t.Errorf("entity+manual score (%f) should be higher than entity+auto score (%f) at same age", manualScore, autoScore)
	}
}

func TestDecayedImportanceScore_ExplicitHalfLifeOverridesTier(t *testing.T) {
	t.Parallel()
	age := time.Now().UTC().Add(-168 * time.Hour).Format(time.RFC3339)
	m := Memory{Tier: TierSessionLog, Source: SourceAuto, LastAccessedAt: age, AccessCount: 1}

	// With explicit 168h, session_log should score ~0.5 (not use its 72h tier default).
	score := DecayedImportanceScore(m, 168)
	if score < 0.49 || score > 0.51 {
		t.Errorf("explicit halfLife=168 score = %f, want ~0.5", score)
	}
}

// ── RRFMerge ──────────────────────────────────────────────────────────────────

func TestRRFMerge_SingleChannel(t *testing.T) {
	t.Parallel()
	channels := map[string][]string{
		"bm25": {"a", "b", "c"},
	}
	ids, attr := RRFMerge(channels, 3, 60)
	if len(ids) != 3 {
		t.Fatalf("got %d results, want 3", len(ids))
	}
	if ids[0] != "a" {
		t.Errorf("first result = %q, want %q", ids[0], "a")
	}
	if len(attr["a"]) != 1 || attr["a"][0] != "bm25" {
		t.Errorf("attribution for a = %v, want [bm25]", attr["a"])
	}
}

func TestRRFMerge_MultiChannelBoost(t *testing.T) {
	t.Parallel()
	// "a" appears in both channels → should score higher than "b" (only bm25).
	channels := map[string][]string{
		"bm25":     {"b", "a", "c"},
		"semantic": {"a", "d"},
	}
	ids, attr := RRFMerge(channels, 5, 60)
	if len(ids) == 0 {
		t.Fatal("got zero results")
	}
	// "a" should be first (appears in 2 channels).
	if ids[0] != "a" {
		t.Errorf("first result = %q, want %q (multi-channel boost)", ids[0], "a")
	}
	if len(attr["a"]) != 2 {
		t.Errorf("attribution for a has %d channels, want 2", len(attr["a"]))
	}
}

func TestRRFMerge_LimitRespected(t *testing.T) {
	t.Parallel()
	channels := map[string][]string{
		"bm25": {"a", "b", "c", "d", "e"},
	}
	ids, _ := RRFMerge(channels, 2, 60)
	if len(ids) != 2 {
		t.Errorf("got %d results, want 2", len(ids))
	}
}

func TestRRFMerge_EmptyChannels(t *testing.T) {
	t.Parallel()
	channels := map[string][]string{}
	ids, attr := RRFMerge(channels, 10, 60)
	if len(ids) != 0 {
		t.Errorf("got %d results from empty channels, want 0", len(ids))
	}
	if len(attr) != 0 {
		t.Errorf("got %d attributions from empty channels, want 0", len(attr))
	}
}

func TestRRFMerge_DefaultK(t *testing.T) {
	t.Parallel()
	channels := map[string][]string{
		"bm25": {"a"},
	}
	ids, _ := RRFMerge(channels, 1, 0)
	if len(ids) != 1 || ids[0] != "a" {
		t.Errorf("default k: got %v, want [a]", ids)
	}
}

func TestRRFMerge_DeterministicTieBreaking(t *testing.T) {
	t.Parallel()
	// "a" and "b" both in exactly one channel at rank 0.
	// Tie should be broken alphabetically (a < b).
	channels := map[string][]string{
		"ch1": {"b"},
		"ch2": {"a"},
	}
	ids, _ := RRFMerge(channels, 2, 60)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("tie-breaking: got %v, want [a b]", ids)
	}
}

func TestRRFMerge_FourChannels(t *testing.T) {
	t.Parallel()
	channels := map[string][]string{
		"bm25":     {"m1", "m2", "m3"},
		"semantic": {"m2", "m4"},
		"graph":    {"m3", "m5", "m1"},
		"temporal": {"m6", "m1"},
	}
	ids, attr := RRFMerge(channels, 10, 60)
	// m1 appears in 3 channels → should rank highest.
	if ids[0] != "m1" {
		t.Errorf("m1 should rank first (3 channels), got %q", ids[0])
	}
	if len(attr["m1"]) != 3 {
		t.Errorf("m1 attribution = %v, want 3 channels", attr["m1"])
	}
	// m2 and m3 both appear in 2 channels.
	if len(attr["m2"]) != 2 || len(attr["m3"]) != 2 {
		t.Errorf("m2 attr=%v, m3 attr=%v, both want 2 channels", attr["m2"], attr["m3"])
	}
}

// ── RecentMemories ────────────────────────────────────────────────────────────

func TestRecentMemories_ReturnsRecentOnly(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	// Insert a recent memory (just now).
	_, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "recent project note",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	mems, err := st.RecentMemories(10, 7, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) == 0 {
		t.Fatal("expected at least 1 recent memory")
	}
	if mems[0].Content != "recent project note" {
		t.Errorf("got content %q, want %q", mems[0].Content, "recent project note")
	}
}

func TestRecentMemories_DefaultSinceDays(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	_, _ = st.InsertMemory(Memory{
		Tier:    TierEntity,
		Content: "test memory for default window",
		AgentID: "agent-1",
		Source:  SourceAuto,
	})

	// sinceDays=0 should default to 7.
	mems, err := st.RecentMemories(10, 0, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) == 0 {
		t.Fatal("expected recent memory with default sinceDays=7")
	}
}

func TestRecentMemories_ExcludesStaleByDefault(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	id, _ := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "stale memory",
		EntityID: "repo::test.go::Foo",
		AgentID:  "agent-1",
		Source:   SourceAuto,
	})
	_ = st.MarkEntityMemoriesStaleForNodes([]string{"repo::test.go::Foo"}, "node removed")
	_ = id

	mems, err := st.RecentMemories(10, 7, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mems {
		if m.Content == "stale memory" {
			t.Error("stale memory should be excluded when includeStale=false")
		}
	}
}

func TestRecentMemories_IncludesStaleWhenRequested(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	_, _ = st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "stale but included memory",
		EntityID: "repo::test.go::Bar",
		AgentID:  "agent-1",
		Source:   SourceAuto,
	})
	_ = st.MarkEntityMemoriesStaleForNodes([]string{"repo::test.go::Bar"}, "changed")

	mems, err := st.RecentMemories(10, 7, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mems {
		if m.Content == "stale but included memory" {
			found = true
		}
	}
	if !found {
		t.Error("stale memory should be included when includeStale=true")
	}
}

// ── RecentMemories: until bound (Sprint 10 #5) ────────────────────────────────

func TestRecentMemories_UntilBound_ExcludesFuture(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	// Fix: set cutoff first, then give the memory an explicit created_at
	// that is deterministically AFTER the cutoff (2 seconds later).
	// This avoids any RFC3339 second-precision timing races.
	cutoff := time.Now().UTC()
	_, _ = st.InsertMemory(Memory{
		Tier:      TierProject,
		Content:   "future memory — should be excluded",
		AgentID:   "agent-1",
		Source:    SourceManual,
		CreatedAt: cutoff.Add(2 * time.Second).Format(time.RFC3339),
	})

	// until=cutoff → the memory (created 2s after cutoff) must not appear.
	mems, err := st.RecentMemories(10, 7, &cutoff, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mems {
		if m.Content == "future memory — should be excluded" {
			t.Error("memory created after until= should be excluded")
		}
	}
}

func TestRecentMemories_UntilBound_IncludesWithinRange(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	_, _ = st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "within-range memory",
		AgentID: "agent-1",
		Source:  SourceManual,
	})

	// until = 1 second from now → the just-inserted memory should be included.
	cutoff := time.Now().UTC().Add(1 * time.Second)
	mems, err := st.RecentMemories(10, 7, &cutoff, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mems {
		if m.Content == "within-range memory" {
			found = true
		}
	}
	if !found {
		t.Error("memory within the until= bound should be included")
	}
}

func TestRecentMemories_NilUntilBound_BehavesLikeBefore(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	_, _ = st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "nil-until memory",
		AgentID: "agent-1",
		Source:  SourceManual,
	})

	// nil until should not filter anything — same as old 3-arg behavior.
	mems, err := st.RecentMemories(10, 7, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mems {
		if m.Content == "nil-until memory" {
			found = true
		}
	}
	if !found {
		t.Error("nil until= should not exclude any memories")
	}
}

// ── GetAnchorNodesByFTSQuery ──────────────────────────────────────────────────

func TestGetAnchorNodesByFTSQuery_FindsAnchorNodes(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	// Insert a memory with anchor nodes.
	_, err := st.InsertMemoryWithAnchors(Memory{
		Tier:    TierEntity,
		Content: "AuthService refactored to use JWT tokens",
		AgentID: "agent-1",
		Source:  SourceManual,
	}, []string{"repo::auth.go::AuthService"})
	if err != nil {
		t.Fatal(err)
	}

	nodes, err := st.GetAnchorNodesByFTSQuery("AuthService JWT", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 {
		t.Fatal("expected anchor node IDs for matching memory")
	}
	if nodes[0] != "repo::auth.go::AuthService" {
		t.Errorf("got node %q, want %q", nodes[0], "repo::auth.go::AuthService")
	}
}

func TestGetAnchorNodesByFTSQuery_EmptyQuery(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)
	nodes, err := st.GetAnchorNodesByFTSQuery("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Errorf("empty query should return no nodes, got %d", len(nodes))
	}
}

func TestGetAnchorNodesByFTSQuery_NoMatch(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)
	_, _ = st.InsertMemoryWithAnchors(Memory{
		Tier:    TierEntity,
		Content: "database connection pooling",
		AgentID: "agent-1",
		Source:  SourceManual,
	}, []string{"repo::db.go::Pool"})

	nodes, err := st.GetAnchorNodesByFTSQuery("quantum computing", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Errorf("non-matching query should return no nodes, got %d", len(nodes))
	}
}

// ── GetMemoriesByAnchorNodes ──────────────────────────────────────────────────

func TestGetMemoriesByAnchorNodes_FindsMemories(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	_, _ = st.InsertMemoryWithAnchors(Memory{
		Tier:    TierEntity,
		Content: "memory anchored to node A",
		AgentID: "agent-1",
		Source:  SourceManual,
	}, []string{"repo::a.go::FuncA"})

	_, _ = st.InsertMemoryWithAnchors(Memory{
		Tier:    TierEntity,
		Content: "memory anchored to node B",
		AgentID: "agent-1",
		Source:  SourceManual,
	}, []string{"repo::b.go::FuncB"})

	mems, err := st.GetMemoriesByAnchorNodes(
		[]string{"repo::a.go::FuncA", "repo::b.go::FuncB"},
		10, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 2 {
		t.Fatalf("got %d memories, want 2", len(mems))
	}
}

func TestGetMemoriesByAnchorNodes_Deduplicates(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	// Memory anchored to two nodes.
	_, _ = st.InsertMemoryWithAnchors(Memory{
		Tier:    TierEntity,
		Content: "shared memory across two nodes",
		AgentID: "agent-1",
		Source:  SourceManual,
	}, []string{"repo::a.go::FuncA", "repo::b.go::FuncB"})

	mems, err := st.GetMemoriesByAnchorNodes(
		[]string{"repo::a.go::FuncA", "repo::b.go::FuncB"},
		10, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Errorf("got %d memories, want 1 (deduplicated)", len(mems))
	}
}

func TestGetMemoriesByAnchorNodes_EmptyInput(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)
	mems, err := st.GetMemoriesByAnchorNodes(nil, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 0 {
		t.Errorf("empty input should return nil, got %d", len(mems))
	}
}

func TestGetMemoriesByAnchorNodes_LimitRespected(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	for i := 0; i < 5; i++ {
		content := fmt.Sprintf("distinct memory number %d about topic %c with unique details xyz%d", i, rune('A'+i), i*1000)
		_, err := st.InsertMemoryWithAnchors(Memory{
			Tier:    TierEntity,
			Content: content,
			AgentID: "agent-1",
			Source:  SourceManual,
		}, []string{"repo::shared.go::Shared"})
		if err != nil {
			t.Fatalf("insert memory %d: %v", i, err)
		}
	}

	mems, err := st.GetMemoriesByAnchorNodes(
		[]string{"repo::shared.go::Shared"},
		2, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 2 {
		t.Errorf("got %d memories, want 2 (limit)", len(mems))
	}
}


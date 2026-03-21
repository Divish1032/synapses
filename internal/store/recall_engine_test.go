package store

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func openRecallTestStore(t *testing.T) *Store {
	t.Helper()
	return openFromTemplate(t)
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
	_ = st.MarkEntityMemoriesStale("repo::test.go::Foo", "node removed")
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
	_ = st.MarkEntityMemoriesStale("repo::test.go::Bar", "changed")

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

// ── ConvexMerge ────────────────────────────────────────────────────────────────

func TestConvexMerge_SingleChannel_BM25(t *testing.T) {
	t.Parallel()
	channels := map[string]*ChannelScores{
		"bm25": {IDs: []string{"a", "b", "c"}, Scores: []float64{10.0, 5.0, 1.0}},
	}
	ids, attr := ConvexMerge(channels, 3, DefaultConvexWeights)
	if len(ids) != 3 {
		t.Fatalf("got %d results, want 3", len(ids))
	}
	// "a" has highest BM25 score → should rank first.
	if ids[0] != "a" {
		t.Errorf("first = %q, want a (highest BM25)", ids[0])
	}
	if ids[2] != "c" {
		t.Errorf("last = %q, want c (lowest BM25)", ids[2])
	}
	if len(attr["a"]) != 1 || attr["a"][0] != "bm25" {
		t.Errorf("attribution for a = %v, want [bm25]", attr["a"])
	}
}

func TestConvexMerge_TwoChannels_ScoreMagnitudeMatters(t *testing.T) {
	t.Parallel()
	// "a" has low BM25 but very high semantic. "b" has high BM25 but low semantic.
	// With α=0.5 (equal weight), the one with higher total normalized score wins.
	channels := map[string]*ChannelScores{
		"bm25":     {IDs: []string{"a", "b"}, Scores: []float64{1.0, 10.0}},
		"semantic": {IDs: []string{"a", "b"}, Scores: []float64{0.95, 0.30}},
	}
	// α=0.5: bm25 weight=0.5, semantic weight=0.5
	// a: 0.5*(0/9) + 0.5*(1.0) = 0.0 + 0.5 = 0.5
	// b: 0.5*(9/9) + 0.5*(0.0) = 0.5 + 0.0 = 0.5
	// Tie → broken alphabetically: a < b → a first.
	ids, _ := ConvexMerge(channels, 2, DefaultConvexWeights)
	if len(ids) != 2 {
		t.Fatalf("got %d results, want 2", len(ids))
	}
	// Exact tie with equal weights → alphabetical order.
	if ids[0] != "a" {
		t.Errorf("first = %q, want a (alphabetical tie-break)", ids[0])
	}
}

func TestConvexMerge_AlphaShiftsFavorToBM25(t *testing.T) {
	t.Parallel()
	channels := map[string]*ChannelScores{
		"bm25":     {IDs: []string{"a", "b"}, Scores: []float64{1.0, 10.0}},
		"semantic": {IDs: []string{"a", "b"}, Scores: []float64{0.99, 0.30}},
	}
	// α=0.9: strongly favor BM25.
	// b has much higher BM25 → should win.
	weights := ConvexWeights{Alpha: 0.9, GraphBonus: 0.3, TemporalBonus: 0.2}
	ids, _ := ConvexMerge(channels, 2, weights)
	if ids[0] != "b" {
		t.Errorf("first = %q, want b (high-alpha favors BM25)", ids[0])
	}
}

func TestConvexMerge_AlphaShiftsFavorToSemantic(t *testing.T) {
	t.Parallel()
	channels := map[string]*ChannelScores{
		"bm25":     {IDs: []string{"a", "b"}, Scores: []float64{1.0, 10.0}},
		"semantic": {IDs: []string{"a", "b"}, Scores: []float64{0.99, 0.30}},
	}
	// α=0.1: strongly favor semantic.
	// a has much higher semantic → should win.
	weights := ConvexWeights{Alpha: 0.1, GraphBonus: 0.3, TemporalBonus: 0.2}
	ids, _ := ConvexMerge(channels, 2, weights)
	if ids[0] != "a" {
		t.Errorf("first = %q, want a (low-alpha favors semantic)", ids[0])
	}
}

func TestConvexMerge_GraphBonusLiftsGraphResults(t *testing.T) {
	t.Parallel()
	// "a" only in BM25, "b" in BM25 + graph. Same BM25 score.
	// Graph bonus should lift "b" above "a".
	channels := map[string]*ChannelScores{
		"bm25":  {IDs: []string{"a", "b"}, Scores: []float64{5.0, 5.0}},
		"graph": {IDs: []string{"b"}, Scores: []float64{0.8}},
	}
	ids, _ := ConvexMerge(channels, 2, DefaultConvexWeights)
	if ids[0] != "b" {
		t.Errorf("first = %q, want b (graph bonus lifts it)", ids[0])
	}
}

func TestConvexMerge_TemporalBonusLiftsRecentResults(t *testing.T) {
	t.Parallel()
	// "a" only in BM25, "b" in BM25 + temporal. Same BM25 score.
	channels := map[string]*ChannelScores{
		"bm25":     {IDs: []string{"a", "b"}, Scores: []float64{5.0, 5.0}},
		"temporal": {IDs: []string{"b"}, Scores: []float64{0.9}},
	}
	ids, _ := ConvexMerge(channels, 2, DefaultConvexWeights)
	if ids[0] != "b" {
		t.Errorf("first = %q, want b (temporal bonus lifts it)", ids[0])
	}
}

func TestConvexMerge_FourChannels_HighScoresWin(t *testing.T) {
	t.Parallel()
	// "m1" appears in all 4 channels with the HIGHEST score in each.
	// Unlike RRF, ConvexMerge rewards score magnitude — being top-scored
	// in multiple channels should dominate.
	channels := map[string]*ChannelScores{
		"bm25":     {IDs: []string{"m1", "m2"}, Scores: []float64{10.0, 3.0}},
		"semantic": {IDs: []string{"m1", "m3"}, Scores: []float64{0.95, 0.3}},
		"graph":    {IDs: []string{"m1", "m4"}, Scores: []float64{0.9, 0.4}},
		"temporal": {IDs: []string{"m1", "m5"}, Scores: []float64{0.95, 0.2}},
	}
	ids, attr := ConvexMerge(channels, 5, DefaultConvexWeights)
	if ids[0] != "m1" {
		t.Errorf("first = %q, want m1 (highest score in all 4 channels)", ids[0])
	}
	if len(attr["m1"]) != 4 {
		t.Errorf("m1 channels = %d, want 4", len(attr["m1"]))
	}
}

func TestConvexMerge_LimitRespected(t *testing.T) {
	t.Parallel()
	channels := map[string]*ChannelScores{
		"bm25": {IDs: []string{"a", "b", "c", "d", "e"}, Scores: []float64{5, 4, 3, 2, 1}},
	}
	ids, _ := ConvexMerge(channels, 2, DefaultConvexWeights)
	if len(ids) != 2 {
		t.Errorf("got %d results, want 2", len(ids))
	}
}

func TestConvexMerge_EmptyChannels(t *testing.T) {
	t.Parallel()
	channels := map[string]*ChannelScores{}
	ids, attr := ConvexMerge(channels, 10, DefaultConvexWeights)
	if len(ids) != 0 {
		t.Errorf("got %d results from empty channels, want 0", len(ids))
	}
	if len(attr) != 0 {
		t.Errorf("got %d attributions from empty channels, want 0", len(attr))
	}
}

func TestConvexMerge_NilChannelScores(t *testing.T) {
	t.Parallel()
	channels := map[string]*ChannelScores{
		"bm25":     nil,
		"semantic": {IDs: []string{"a"}, Scores: []float64{0.9}},
	}
	ids, _ := ConvexMerge(channels, 5, DefaultConvexWeights)
	if len(ids) != 1 || ids[0] != "a" {
		t.Errorf("got %v, want [a]", ids)
	}
}

func TestConvexMerge_AllEqualScores_NormalizesToOne(t *testing.T) {
	t.Parallel()
	// All same score → all normalize to 1.0 → rank alphabetically.
	channels := map[string]*ChannelScores{
		"bm25": {IDs: []string{"c", "a", "b"}, Scores: []float64{5.0, 5.0, 5.0}},
	}
	ids, _ := ConvexMerge(channels, 3, DefaultConvexWeights)
	if ids[0] != "a" || ids[1] != "b" || ids[2] != "c" {
		t.Errorf("equal scores should sort alphabetically, got %v", ids)
	}
}

func TestConvexMerge_DeterministicTieBreaking(t *testing.T) {
	t.Parallel()
	// Same setup as RRF test: "a" and "b" with equal scores.
	channels := map[string]*ChannelScores{
		"ch1": {IDs: []string{"b"}, Scores: []float64{1.0}},
		"ch2": {IDs: []string{"a"}, Scores: []float64{1.0}},
	}
	ids, _ := ConvexMerge(channels, 2, ConvexWeights{Alpha: 0.5, GraphBonus: 0.5, TemporalBonus: 0.5})
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("tie-breaking: got %v, want [a b]", ids)
	}
}

// ── minMaxNormalize ────────────────────────────────────────────────────────────

func TestMinMaxNormalize_BasicRange(t *testing.T) {
	t.Parallel()
	norm := minMaxNormalize([]float64{1.0, 5.0, 10.0})
	// min=1, max=10, spread=9
	// (1-1)/9 = 0.0, (5-1)/9 ≈ 0.444, (10-1)/9 = 1.0
	if norm[0] != 0.0 {
		t.Errorf("norm[0] = %f, want 0.0", norm[0])
	}
	if norm[2] != 1.0 {
		t.Errorf("norm[2] = %f, want 1.0", norm[2])
	}
	if norm[1] < 0.44 || norm[1] > 0.45 {
		t.Errorf("norm[1] = %f, want ~0.444", norm[1])
	}
}

func TestMinMaxNormalize_SingleElement(t *testing.T) {
	t.Parallel()
	norm := minMaxNormalize([]float64{42.0})
	if norm[0] != 1.0 {
		t.Errorf("single element should normalize to 1.0, got %f", norm[0])
	}
}

func TestMinMaxNormalize_AllEqual(t *testing.T) {
	t.Parallel()
	norm := minMaxNormalize([]float64{3.0, 3.0, 3.0})
	for i, v := range norm {
		if v != 1.0 {
			t.Errorf("norm[%d] = %f, want 1.0 (all equal)", i, v)
		}
	}
}

func TestMinMaxNormalize_Empty(t *testing.T) {
	t.Parallel()
	norm := minMaxNormalize(nil)
	if norm != nil {
		t.Errorf("empty input should return nil, got %v", norm)
	}
}

func TestMinMaxNormalize_NegativeScores(t *testing.T) {
	t.Parallel()
	// BM25 can return negative scores in raw form.
	norm := minMaxNormalize([]float64{-10.0, -5.0, 0.0})
	if norm[0] != 0.0 {
		t.Errorf("norm[0] = %f, want 0.0 (most negative)", norm[0])
	}
	if norm[2] != 1.0 {
		t.Errorf("norm[2] = %f, want 1.0 (highest)", norm[2])
	}
	if norm[1] != 0.5 {
		t.Errorf("norm[1] = %f, want 0.5", norm[1])
	}
}

// ── SearchMemoriesWithScores ──────────────────────────────────────────────────

func TestSearchMemoriesWithScores_ReturnsBM25Scores(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	_, err := st.InsertMemory(Memory{
		Tier:    TierEntity,
		Content: "authentication token validation middleware",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	scored, err := st.SearchMemoriesWithScores("authentication", 10, false)
	if err != nil {
		t.Fatalf("SearchMemoriesWithScores: %v", err)
	}
	if len(scored) == 0 {
		t.Fatal("expected at least 1 scored result")
	}
	if scored[0].Score <= 0 {
		t.Errorf("BM25 score should be positive, got %f", scored[0].Score)
	}
	if scored[0].Memory.Content == "" {
		t.Error("memory content should be populated")
	}
}

func TestSearchMemoriesWithScores_OrderedByScore(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	// Insert two memories: one highly relevant, one marginally.
	_, _ = st.InsertMemory(Memory{
		Tier:    TierEntity,
		Content: "authentication authentication authentication token",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	_, _ = st.InsertMemory(Memory{
		Tier:    TierEntity,
		Content: "database connection pooling with authentication check",
		AgentID: "agent-1",
		Source:  SourceManual,
	})

	scored, err := st.SearchMemoriesWithScores("authentication", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(scored) < 2 {
		t.Fatalf("expected 2 results, got %d", len(scored))
	}
	// First result should have higher or equal score.
	if scored[0].Score < scored[1].Score {
		t.Errorf("results not ordered by score: %f < %f", scored[0].Score, scored[1].Score)
	}
}

func TestSearchMemoriesWithScores_EmptyQuery(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)
	scored, err := st.SearchMemoriesWithScores("", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(scored) != 0 {
		t.Errorf("empty query should return nil, got %d", len(scored))
	}
}

func TestSearchMemoriesWithScores_IncludeStale(t *testing.T) {
	t.Parallel()
	st := openRecallTestStore(t)

	_, _ = st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "stale authentication middleware",
		EntityID: "repo::auth.go::Auth",
		AgentID:  "agent-1",
		Source:   SourceManual,
	})
	_ = st.MarkEntityMemoriesStale("repo::auth.go::Auth", "changed")

	// Without stale: should not find it.
	scored, _ := st.SearchMemoriesWithScores("authentication", 10, false)
	for _, s := range scored {
		if s.Memory.Content == "stale authentication middleware" {
			t.Error("stale memory should be excluded when includeStale=false")
		}
	}

	// With stale: should find it.
	scored, _ = st.SearchMemoriesWithScores("authentication", 10, true)
	found := false
	for _, s := range scored {
		if s.Memory.Content == "stale authentication middleware" {
			found = true
		}
	}
	if !found {
		t.Error("stale memory should be included when includeStale=true")
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

// ── Production edge cases: NaN/Inf, mismatched lengths, zero weights ────────

func TestMinMaxNormalize_NaN_SanitizedToZero(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	norm := minMaxNormalize([]float64{nan, 5.0, 10.0})
	// NaN → 0.0; min=0, max=10; (0-0)/10=0, (5-0)/10=0.5, (10-0)/10=1.0
	if norm[0] != 0.0 {
		t.Errorf("NaN should sanitize to 0.0, got %f", norm[0])
	}
	if norm[2] != 1.0 {
		t.Errorf("norm[2] = %f, want 1.0", norm[2])
	}
}

func TestMinMaxNormalize_Inf_SanitizedToZero(t *testing.T) {
	t.Parallel()
	inf := math.Inf(1)
	norm := minMaxNormalize([]float64{inf, 5.0, 10.0})
	// +Inf → 0.0; min=0, max=10.
	if norm[0] != 0.0 {
		t.Errorf("+Inf should sanitize to 0.0, got %f", norm[0])
	}
}

func TestMinMaxNormalize_AllNaN_NormalizesToOne(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	norm := minMaxNormalize([]float64{nan, nan, nan})
	// All NaN → all 0.0 → all equal → all 1.0.
	for i, v := range norm {
		if v != 1.0 {
			t.Errorf("norm[%d] = %f, want 1.0 (all-NaN → all-equal → 1.0)", i, v)
		}
	}
}

func TestConvexMerge_MismatchedIDsScores_SkipsChannel(t *testing.T) {
	t.Parallel()
	// IDs has 3 elements but Scores has 2 → channel should be skipped.
	channels := map[string]*ChannelScores{
		"bm25":     {IDs: []string{"a", "b", "c"}, Scores: []float64{10.0, 5.0}},
		"semantic": {IDs: []string{"d"}, Scores: []float64{0.9}},
	}
	ids, _ := ConvexMerge(channels, 5, DefaultConvexWeights)
	// bm25 skipped (mismatched), semantic survives.
	if len(ids) != 1 || ids[0] != "d" {
		t.Errorf("got %v, want [d] (bm25 skipped due to length mismatch)", ids)
	}
}

func TestConvexMerge_ZeroAlpha_AllSemantic(t *testing.T) {
	t.Parallel()
	// α=0 → BM25 weight=0, semantic weight=1.
	channels := map[string]*ChannelScores{
		"bm25":     {IDs: []string{"a", "b"}, Scores: []float64{100.0, 1.0}},
		"semantic": {IDs: []string{"a", "b"}, Scores: []float64{0.1, 0.9}},
	}
	weights := ConvexWeights{Alpha: 0.0, GraphBonus: 0, TemporalBonus: 0}
	ids, _ := ConvexMerge(channels, 2, weights)
	// BM25 contributes 0. Semantic: a=0.0, b=1.0 → b first.
	if ids[0] != "b" {
		t.Errorf("first = %q, want b (alpha=0, all semantic)", ids[0])
	}
}

func TestConvexMerge_OneAlpha_AllBM25(t *testing.T) {
	t.Parallel()
	// α=1 → BM25 weight=1, semantic weight=0.
	channels := map[string]*ChannelScores{
		"bm25":     {IDs: []string{"a", "b"}, Scores: []float64{100.0, 1.0}},
		"semantic": {IDs: []string{"a", "b"}, Scores: []float64{0.1, 0.9}},
	}
	weights := ConvexWeights{Alpha: 1.0, GraphBonus: 0, TemporalBonus: 0}
	ids, _ := ConvexMerge(channels, 2, weights)
	// Semantic contributes 0. BM25: a=1.0, b=0.0 → a first.
	if ids[0] != "a" {
		t.Errorf("first = %q, want a (alpha=1, all BM25)", ids[0])
	}
}

func TestConvexMerge_NaNScores_DoNotCrash(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	channels := map[string]*ChannelScores{
		"bm25": {IDs: []string{"a", "b"}, Scores: []float64{nan, 5.0}},
	}
	// Must not panic.
	ids, _ := ConvexMerge(channels, 2, DefaultConvexWeights)
	if len(ids) != 2 {
		t.Errorf("got %d results, want 2", len(ids))
	}
}

func TestConvexMerge_EmptyScoresSlice(t *testing.T) {
	t.Parallel()
	channels := map[string]*ChannelScores{
		"bm25": {IDs: []string{}, Scores: []float64{}},
	}
	ids, _ := ConvexMerge(channels, 5, DefaultConvexWeights)
	if len(ids) != 0 {
		t.Errorf("got %d results from empty scores, want 0", len(ids))
	}
}


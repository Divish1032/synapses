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
	score := RecencyDecayScore(time.Now(), 168)
	if score < 0.99 || score > 1.01 {
		t.Errorf("just-created score = %f, want ~1.0", score)
	}
}

func TestRecencyDecayScore_OneHalfLife(t *testing.T) {
	created := time.Now().Add(-168 * time.Hour) // 1 week ago
	score := RecencyDecayScore(created, 168)
	if score < 0.49 || score > 0.51 {
		t.Errorf("one half-life score = %f, want ~0.5", score)
	}
}

func TestRecencyDecayScore_FutureTimestamp(t *testing.T) {
	created := time.Now().Add(1 * time.Hour) // future
	score := RecencyDecayScore(created, 168)
	if score < 0.99 || score > 1.01 {
		t.Errorf("future timestamp score = %f, want ~1.0", score)
	}
}

func TestRecencyDecayScore_ZeroHalfLife_DefaultsTo168(t *testing.T) {
	created := time.Now().Add(-168 * time.Hour)
	score := RecencyDecayScore(created, 0)
	if score < 0.49 || score > 0.51 {
		t.Errorf("zero half-life (default) score = %f, want ~0.5", score)
	}
}

func TestRecencyDecayScore_NegativeHalfLife_DefaultsTo168(t *testing.T) {
	score := RecencyDecayScore(time.Now(), -10)
	if score < 0.99 || score > 1.01 {
		t.Errorf("negative half-life score = %f, want ~1.0", score)
	}
}

// ── RRFMerge ──────────────────────────────────────────────────────────────────

func TestRRFMerge_SingleChannel(t *testing.T) {
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
	channels := map[string][]string{
		"bm25": {"a", "b", "c", "d", "e"},
	}
	ids, _ := RRFMerge(channels, 2, 60)
	if len(ids) != 2 {
		t.Errorf("got %d results, want 2", len(ids))
	}
}

func TestRRFMerge_EmptyChannels(t *testing.T) {
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
	channels := map[string][]string{
		"bm25": {"a"},
	}
	ids, _ := RRFMerge(channels, 1, 0)
	if len(ids) != 1 || ids[0] != "a" {
		t.Errorf("default k: got %v, want [a]", ids)
	}
}

func TestRRFMerge_DeterministicTieBreaking(t *testing.T) {
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

	mems, err := st.RecentMemories(10, 7, false)
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
	st := openRecallTestStore(t)

	_, _ = st.InsertMemory(Memory{
		Tier:    TierEntity,
		Content: "test memory for default window",
		AgentID: "agent-1",
		Source:  SourceAuto,
	})

	// sinceDays=0 should default to 7.
	mems, err := st.RecentMemories(10, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) == 0 {
		t.Fatal("expected recent memory with default sinceDays=7")
	}
}

func TestRecentMemories_ExcludesStaleByDefault(t *testing.T) {
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

	mems, err := st.RecentMemories(10, 7, false)
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
	st := openRecallTestStore(t)

	_, _ = st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "stale but included memory",
		EntityID: "repo::test.go::Bar",
		AgentID:  "agent-1",
		Source:   SourceAuto,
	})
	_ = st.MarkEntityMemoriesStale("repo::test.go::Bar", "changed")

	mems, err := st.RecentMemories(10, 7, true)
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

// ── GetAnchorNodesByFTSQuery ──────────────────────────────────────────────────

func TestGetAnchorNodesByFTSQuery_FindsAnchorNodes(t *testing.T) {
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


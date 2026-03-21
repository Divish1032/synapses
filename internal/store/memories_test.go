package store

import (
	"strings"
	"testing"
	"time"
)

func openMemTestStore(t *testing.T) *Store {
	t.Helper()
	return openFromTemplate(t)
}

// testGetMemoryAnchors queries the memory_anchors table directly for testing.
func testGetMemoryAnchors(st *Store, memoryID string) []string {
	rows, err := st.knowledgeDB.Query(`SELECT node_id FROM memory_anchors WHERE memory_id = ? ORDER BY node_id`, memoryID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var anchors []string
	for rows.Next() {
		var nid string
		rows.Scan(&nid)
		anchors = append(anchors, nid)
	}
	return anchors
}

func TestInsertMemory_BasicRoundTrip(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "Store.Close was refactored to accept projectID parameter",
		EntityID: "repo::store.go::Store.Close",
		AgentID:  "agent-1",
		Source:   SourceAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}

	mems, err := st.QueryMemories(TierEntity, "repo::store.go::Store.Close", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(mems))
	}
	if mems[0].Content != "Store.Close was refactored to accept projectID parameter" {
		t.Errorf("unexpected content: %q", mems[0].Content)
	}
	if mems[0].Source != SourceAuto {
		t.Errorf("expected source=%q, got %q", SourceAuto, mems[0].Source)
	}
}

func TestInsertMemory_RejectsShortContent(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	_, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "short",
	})
	if err == nil {
		t.Fatal("expected error for short content")
	}
}

func TestInsertMemory_TruncatesLongContent(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	long := make([]byte, 3000)
	for i := range long {
		long[i] = 'a'
	}

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: string(long),
	})
	if err != nil {
		t.Fatal(err)
	}

	mems, err := st.QueryMemories(TierProject, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(mems))
	}
	_ = id
	if len(mems[0].Content) > 2020 {
		t.Errorf("expected truncated content, got %d chars", len(mems[0].Content))
	}
}

func TestInsertMemory_Dedup(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id1, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "Store.Close was refactored to accept projectID parameter",
		EntityID: "repo::store.go::Store.Close",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Insert near-duplicate — should return existing ID.
	id2, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "Store.Close was refactored to accept projectID parameter today",
		EntityID: "repo::store.go::Store.Close",
	})
	if err != nil {
		t.Fatal(err)
	}

	if id1 != id2 {
		t.Errorf("expected dedup to return same ID, got %q and %q", id1, id2)
	}

	mems, err := st.QueryMemories(TierEntity, "repo::store.go::Store.Close", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Errorf("expected 1 memory after dedup, got %d", len(mems))
	}
}

func TestQueryMemories_FiltersByTier(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	st.InsertMemory(Memory{Tier: TierEntity, Content: "entity memory content here", EntityID: "node1"})
	st.InsertMemory(Memory{Tier: TierProject, Content: "project memory content here"})
	st.InsertMemory(Memory{Tier: TierSessionLog, Content: "session log memory content", AgentID: "a1"})

	entity, _ := st.QueryMemories(TierEntity, "", "", 10)
	if len(entity) != 1 {
		t.Errorf("expected 1 entity memory, got %d", len(entity))
	}

	project, _ := st.QueryMemories(TierProject, "", "", 10)
	if len(project) != 1 {
		t.Errorf("expected 1 project memory, got %d", len(project))
	}

	all, _ := st.QueryMemories("", "", "", 10)
	if len(all) != 3 {
		t.Errorf("expected 3 total memories, got %d", len(all))
	}
}

func TestTouchMemory_ExtendsExpiry(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, _ := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "project convention: use repository pattern everywhere",
	})

	// Read initial expires_at.
	var expiresBefore string
	st.knowledgeDB.QueryRow(`SELECT expires_at FROM memories WHERE id = ?`, id).Scan(&expiresBefore)

	// Touch it.
	time.Sleep(10 * time.Millisecond) // ensure time advances
	err := st.TouchMemory(id)
	if err != nil {
		t.Fatal(err)
	}

	var expiresAfter, accessedAt string
	st.knowledgeDB.QueryRow(`SELECT expires_at, last_accessed_at FROM memories WHERE id = ?`, id).
		Scan(&expiresAfter, &accessedAt)

	if expiresAfter == expiresBefore {
		t.Error("expected expires_at to be extended after touch")
	}
	if accessedAt == "" {
		t.Error("expected last_accessed_at to be updated")
	}
}

func TestTouchMemory_IncrementsAccessCount(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, _ := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "memory to verify access count increments on touch",
	})

	// Initial access_count should be 0.
	var count int
	st.knowledgeDB.QueryRow(`SELECT access_count FROM memories WHERE id = ?`, id).Scan(&count)
	if count != 0 {
		t.Fatalf("initial access_count = %d, want 0", count)
	}

	// Touch 3 times.
	for i := 0; i < 3; i++ {
		if err := st.TouchMemory(id); err != nil {
			t.Fatal(err)
		}
	}

	st.knowledgeDB.QueryRow(`SELECT access_count FROM memories WHERE id = ?`, id).Scan(&count)
	if count != 3 {
		t.Errorf("access_count after 3 touches = %d, want 3", count)
	}
}

func TestTouchMemory_EntityMemory_IncrementsAccessCount(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, _ := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "entity memory to verify access count via entity touch path",
		EntityID: "repo::test.go::Foo",
	})

	if err := st.TouchMemory(id); err != nil {
		t.Fatal(err)
	}

	var count int
	st.knowledgeDB.QueryRow(`SELECT access_count FROM memories WHERE id = ?`, id).Scan(&count)
	if count != 1 {
		t.Errorf("entity memory access_count after touch = %d, want 1", count)
	}
}

func TestExpireMemories_DeletesExpired(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Insert a memory with already-expired timestamp.
	past := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	st.knowledgeDB.Exec(`INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags,
		created_at, expires_at, last_accessed_at, source) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"expired-1", TierSessionLog, "old session data that should expire",
		"", "agent-1", "", "[]", past, past, past, SourceAuto)

	// Insert a valid memory.
	st.InsertMemory(Memory{Tier: TierProject, Content: "still valid project memory here"})

	deleted, err := st.ExpireMemories()
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}

	remaining, _ := st.QueryMemories("", "", "", 10)
	if len(remaining) != 1 {
		t.Errorf("expected 1 remaining, got %d", len(remaining))
	}
}

func TestQueryMemoriesForEntities_MultipleEntities(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	st.InsertMemory(Memory{Tier: TierEntity, Content: "memory about Store.Close function", EntityID: "node-close"})
	st.InsertMemory(Memory{Tier: TierEntity, Content: "memory about Graph.New function here", EntityID: "node-new"})
	st.InsertMemory(Memory{Tier: TierEntity, Content: "another Store.Close memory content", EntityID: "node-close"})

	result, err := st.QueryMemoriesForEntities([]string{"node-close", "node-new"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(result["node-close"]) != 2 {
		t.Errorf("expected 2 memories for node-close, got %d", len(result["node-close"]))
	}
	if len(result["node-new"]) != 1 {
		t.Errorf("expected 1 memory for node-new, got %d", len(result["node-new"]))
	}
}

func TestMarkEntityMemoriesStale(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	st.InsertMemory(Memory{Tier: TierEntity, Content: "memory for a node that will be deleted", EntityID: "dead-node"})

	err := st.MarkEntityMemoriesStaleForNodes([]string{"dead-node"}, "entity node removed")
	if err != nil {
		t.Fatal(err)
	}

	// Must set stale=1 and stale_reason.
	var stale int
	var reason, expiresAt string
	st.knowledgeDB.QueryRow(`SELECT stale, stale_reason, expires_at FROM memories WHERE entity_id = ?`, "dead-node").Scan(&stale, &reason, &expiresAt)
	if stale != 1 {
		t.Errorf("expected stale=1, got %d", stale)
	}
	if reason != "entity node removed" {
		t.Errorf("unexpected stale_reason: %q", reason)
	}
	// TTL shortened to ~30 days.
	exp, _ := time.Parse(time.RFC3339, expiresAt)
	daysUntilExpiry := time.Until(exp).Hours() / 24
	if daysUntilExpiry < 28 || daysUntilExpiry > 32 {
		t.Errorf("expected ~30 days until expiry, got %.0f", daysUntilExpiry)
	}
}

// TestMarkEntityMemoriesStaleForNodes_BatchCoversEntityIDMemories verifies that
// entity-tier memories written with entity_id (no anchors) are staled by the
// batch function — the Gap 4 fix.
func TestMarkEntityMemoriesStaleForNodes_BatchCoversEntityIDMemories(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Three entity memories: two in the removed batch, one surviving.
	id1, _ := st.InsertMemory(Memory{Tier: TierEntity, Content: "Store.Close serializes shutdown via reparseMu.", EntityID: "node-A", Source: SourceAuto})
	id2, _ := st.InsertMemory(Memory{Tier: TierEntity, Content: "Graph.New allocates adjacency list structures.", EntityID: "node-B", Source: SourceAuto})
	id3, _ := st.InsertMemory(Memory{Tier: TierEntity, Content: "Walker.ParseFile parses a single source file.", EntityID: "node-C", Source: SourceAuto})

	err := st.MarkEntityMemoriesStaleForNodes([]string{"node-A", "node-B"}, "entity node removed")
	if err != nil {
		t.Fatalf("MarkEntityMemoriesStaleForNodes: %v", err)
	}

	check := func(id string, wantStale int) {
		t.Helper()
		var stale int
		st.knowledgeDB.QueryRow(`SELECT stale FROM memories WHERE id = ?`, id).Scan(&stale)
		if stale != wantStale {
			t.Errorf("id=%s: expected stale=%d, got %d", id, wantStale, stale)
		}
	}
	check(id1, 1) // removed — must be stale
	check(id2, 1) // removed — must be stale
	check(id3, 0) // surviving — must stay fresh
}

// TestMarkEntityMemoriesStaleForNodes_EmptyIsNoop ensures an empty slice does
// not error or affect any rows.
func TestMarkEntityMemoriesStaleForNodes_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	if err := st.MarkEntityMemoriesStaleForNodes(nil, "test"); err != nil {
		t.Fatalf("expected nil error for empty slice, got %v", err)
	}
}

// TestMarkEntityMemoriesStaleForNodes_NonEntityTierUntouched verifies that
// project-tier and session-tier memories with a matching entity_id are NOT
// staled — the function is scoped to tier='entity' only.
func TestMarkEntityMemoriesStaleForNodes_NonEntityTierUntouched(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Project-tier memory with entity_id set (unusual but possible).
	idProj, _ := st.InsertMemory(Memory{Tier: TierProject, Content: "Project-tier memory referencing a node.", EntityID: "node-X", AgentID: "a", Source: SourceManual, Tags: `[]`})

	if err := st.MarkEntityMemoriesStaleForNodes([]string{"node-X"}, "entity node removed"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var stale int
	st.knowledgeDB.QueryRow(`SELECT stale FROM memories WHERE id = ?`, idProj).Scan(&stale)
	if stale != 0 {
		t.Errorf("project-tier memory should be untouched, got stale=%d", stale)
	}
}

func TestStringSimilarity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		a, b     string
		min, max float64
	}{
		{"identical", "hello world", "hello world", 1.0, 1.0},
		{"disjoint", "hello world", "goodbye moon", 0.0, 0.1},
		{"partial overlap", "Store.Close was refactored", "Store.Close was refactored to accept projectID", 0.5, 0.9},
		{"empty a", "", "something", 0.0, 0.0},
		// Punctuation normalization: "management." must equal "management".
		// Without stripping, these score 0 (different words). With stripping, 1.0.
		{"trailing period", "session management.", "session management", 1.0, 1.0},
		{"mixed punctuation", "auth-module (JWT)", "auth module JWT", 1.0, 1.0},
		// Sentences that differ only by trailing period should score 1.0.
		{"sentence period", "The auth module uses JWT tokens.", "The auth module uses JWT tokens", 1.0, 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sim := stringSimilarity(tt.a, tt.b)
			if sim < tt.min || sim > tt.max {
				t.Errorf("stringSimilarity(%q, %q) = %.3f, want [%.2f, %.2f]", tt.a, tt.b, sim, tt.min, tt.max)
			}
		})
	}
}

// TestInsertMemory_UTF8Truncation verifies that size-capping truncates at
// Unicode code-point boundaries, not raw bytes. Emoji are 4 bytes each —
// truncating at byte 2000 would split them and produce invalid UTF-8.
func TestInsertMemory_UTF8Truncation(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	// Build content with 2100 emoji (2100 runes > 2000 rune cap; 8400 bytes).
	// If we truncated at bytes, slicing at byte 2000 would split a 4-byte emoji.
	// If we truncate at runes, we get exactly 2000 emoji with a clean boundary.
	longContent := strings.Repeat("😀", 2100) // 8400 bytes, 2100 runes
	if len([]rune(longContent)) <= 2000 {
		t.Skip("test requires content > 2000 runes")
	}

	_, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: longContent, AgentID: "utf8-agent", Source: SourceManual, Tags: `[]`,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	mems, _ := st.QueryMemories(TierProject, "", "utf8-agent", 1)
	if len(mems) == 0 {
		t.Fatal("no memory found")
	}
	// Verify the stored content is valid UTF-8.
	if !strings.ContainsRune(mems[0].Content, '😀') {
		t.Error("stored content lost emoji — likely invalid UTF-8 from byte truncation")
	}
	if len([]rune(strings.TrimSuffix(mems[0].Content, "…[truncated]"))) > 2000 {
		t.Errorf("content exceeds 2000 runes after truncation: %d runes", len([]rune(mems[0].Content)))
	}
	if !strings.Contains(mems[0].Content, "[truncated]") {
		t.Error("truncation marker missing")
	}
}

func TestCountMemories_ViaSQL(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	st.InsertMemory(Memory{Tier: TierEntity, Content: "entity memory for counting test", EntityID: "n1"})
	st.InsertMemory(Memory{Tier: TierEntity, Content: "another entity memory for count", EntityID: "n2"})
	st.InsertMemory(Memory{Tier: TierProject, Content: "project memory for counting test"})

	var entityCount, projectCount int
	st.knowledgeDB.QueryRow(`SELECT COUNT(*) FROM memories WHERE tier = ?`, TierEntity).Scan(&entityCount)
	st.knowledgeDB.QueryRow(`SELECT COUNT(*) FROM memories WHERE tier = ?`, TierProject).Scan(&projectCount)
	if entityCount != 2 {
		t.Errorf("expected 2 entity memories, got %d", entityCount)
	}
	if projectCount != 1 {
		t.Errorf("expected 1 project memory, got %d", projectCount)
	}
}

// TestInsertMemory_ProjectTierDedup verifies that nearly identical project-tier
// memories written by the same agent are deduplicated rather than duplicated.
// This is the key regression test for end_session being called multiple times.
//
// The Jaccard similarity threshold is 0.85. Adding 1–2 new unique words to an
// 9-word sentence keeps similarity above 0.85 (9/10 = 0.90, 9/11 = 0.82).
// Content is chosen so the second insert is within the dedup window.
func TestInsertMemory_ProjectTierDedup(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	// 9 unique words. Adding " Verified." → 10 words, similarity = 9/10 = 0.90 > 0.85.
	content := "The auth module uses JWT tokens for session management."

	id1, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: content, AgentID: "agent-x", Source: SourceAuto, Tags: `[]`,
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// One new word added → similarity = 9/10 = 0.90 → should dedup.
	id2, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: content + " Verified.", AgentID: "agent-x", Source: SourceAuto, Tags: `[]`,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}

	if id1 != id2 {
		t.Errorf("expected same ID (dedup), got id1=%s id2=%s", id1, id2)
	}

	mems, _ := st.QueryMemories(TierProject, "", "agent-x", 10)
	if len(mems) != 1 {
		t.Errorf("expected 1 project memory after dedup, got %d", len(mems))
	}
}

// TestInsertMemory_BelowDedupThreshold verifies that sufficiently different content
// is NOT deduped (new memories are allowed when content diverges).
// 11-word base + 4 new words → similarity = 11/15 = 0.73 < 0.85 → NOT deduped.
func TestInsertMemory_BelowDedupThreshold(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	content := "The auth module uses JWT tokens for session management across services."

	_, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: content, AgentID: "agent-y", Source: SourceAuto, Tags: `[]`,
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// 4 new unique words → similarity = 11/15 = 0.73 → below threshold, NOT deduped.
	_, err = st.InsertMemory(Memory{
		Tier: TierProject, Content: content + " Confirmed by code review.", AgentID: "agent-y", Source: SourceAuto, Tags: `[]`,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}

	mems, _ := st.QueryMemories(TierProject, "", "agent-y", 10)
	if len(mems) != 2 {
		t.Errorf("expected 2 distinct memories (below dedup threshold), got %d", len(mems))
	}
}

// TestInsertMemory_SemanticDedup_InconclusiveJaccard verifies that when Jaccard
// similarity is in [0.5, 0.85) and the semantic dedup function reports high
// cosine similarity (>0.9), the memory is deduplicated. This catches paraphrased
// duplicates like "auth middleware uses JWT RS256" vs "JWT RS256 signing in auth
// middleware" that have different word order but identical meaning.
func TestInsertMemory_SemanticDedup_InconclusiveJaccard(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Wire a mock embedder that returns distinct but very similar vectors.
	// Candidate vector and new vector will have dot product > 0.9.
	candidateVec := make([]float32, 4)
	candidateVec[0], candidateVec[1], candidateVec[2], candidateVec[3] = 1, 0, 0, 0
	newVec := make([]float32, 4)
	newVec[0], newVec[1], newVec[2], newVec[3] = 0.96, 0.28, 0, 0 // dot=0.96 with candidateVec after normalization

	st.SetSemanticDedupFunc(func(text string) ([]float32, error) {
		return []float32{0.96, 0.28, 0, 0}, nil
	})

	// Content pair chosen so Jaccard is ~0.6 (inconclusive range).
	// "auth middleware uses JWT RS256 tokens" (6 words)
	// "JWT RS256 signing in auth middleware system" (7 words)
	// Overlap: {auth, middleware, JWT, RS256} = 4, Union = 9 → 4/9 ≈ 0.44
	// Let's use something that hits [0.5, 0.85) more reliably:
	contentA := "the authentication middleware uses JWT RS256 tokens for secure session management"
	contentB := "JWT RS256 tokens used by the authentication middleware for secure session handling"
	// Overlap: {the, authentication, middleware, uses/used→different, JWT, RS256, tokens, for, secure, session} ≈ high overlap
	// Jaccard should be in the inconclusive range.

	id1, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentA, AgentID: "sem-agent", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Store an embedding for the first memory (simulates async embed pipeline).
	if err := st.UpsertMemoryEmbedding(id1, "test-model", candidateVec); err != nil {
		t.Fatalf("upsert embedding: %v", err)
	}

	id2, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentB, AgentID: "sem-agent", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}

	if id1 != id2 {
		t.Errorf("expected semantic dedup (same ID), got id1=%s id2=%s", id1, id2)
	}
	mems, _ := st.QueryMemories(TierProject, "", "sem-agent", 10)
	if len(mems) != 1 {
		t.Errorf("expected 1 memory after semantic dedup, got %d", len(mems))
	}
}

// TestInsertMemory_SemanticDedup_LowCosine verifies that when Jaccard is
// inconclusive but cosine similarity is below 0.9, the memory is NOT deduped.
func TestInsertMemory_SemanticDedup_LowCosine(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Return a vector that will have low cosine with the candidate.
	st.SetSemanticDedupFunc(func(text string) ([]float32, error) {
		return []float32{0, 0, 1, 0}, nil // orthogonal to candidate → cosine ≈ 0
	})

	contentA := "the authentication middleware uses JWT RS256 tokens for secure session management"
	contentB := "JWT RS256 tokens used by the authentication middleware for secure session handling"

	id1, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentA, AgentID: "sem-lo", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	candidateVec := []float32{1, 0, 0, 0}
	if err := st.UpsertMemoryEmbedding(id1, "test-model", candidateVec); err != nil {
		t.Fatalf("upsert embedding: %v", err)
	}

	id2, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentB, AgentID: "sem-lo", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}

	if id1 == id2 {
		t.Errorf("expected NO dedup (low cosine), but got same ID %s", id1)
	}
	mems, _ := st.QueryMemories(TierProject, "", "sem-lo", 10)
	if len(mems) != 2 {
		t.Errorf("expected 2 memories (no semantic dedup), got %d", len(mems))
	}
}

// TestInsertMemory_SemanticDedup_NoEmbedder verifies graceful degradation:
// when no semantic dedup function is set, inconclusive Jaccard memories are NOT
// deduped (same behavior as pre-Sprint-11).
func TestInsertMemory_SemanticDedup_NoEmbedder(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	// No SetSemanticDedupFunc — nil by default.

	contentA := "the authentication middleware uses JWT RS256 tokens for secure session management"
	contentB := "JWT RS256 tokens used by the authentication middleware for secure session handling"

	id1, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentA, AgentID: "sem-none", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	id2, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentB, AgentID: "sem-none", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}

	if id1 == id2 {
		t.Errorf("expected NO dedup (no embedder), but got same ID %s", id1)
	}
}

// TestInsertMemory_SemanticDedup_NoCandidateEmbedding verifies that semantic
// dedup is skipped when the candidate memory has no stored embedding.
func TestInsertMemory_SemanticDedup_NoCandidateEmbedding(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	st.SetSemanticDedupFunc(func(text string) ([]float32, error) {
		return []float32{1, 0, 0, 0}, nil
	})

	contentA := "the authentication middleware uses JWT RS256 tokens for secure session management"
	contentB := "JWT RS256 tokens used by the authentication middleware for secure session handling"

	id1, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentA, AgentID: "sem-no-emb", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Do NOT insert embedding for id1 — no candidate embedding available.

	id2, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentB, AgentID: "sem-no-emb", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}

	if id1 == id2 {
		t.Errorf("expected NO dedup (no candidate embedding), but got same ID %s", id1)
	}
}

// TestInsertMemory_SemanticDedup_DimensionMismatch verifies that dimension
// mismatch between candidate and new embedding (e.g., model upgrade) doesn't
// cause a false positive — dotSimilarity returns 0 for length mismatches.
func TestInsertMemory_SemanticDedup_DimensionMismatch(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// New embedder returns 8-dim vectors, but candidate was embedded with 4-dim.
	st.SetSemanticDedupFunc(func(text string) ([]float32, error) {
		return []float32{1, 0, 0, 0, 0, 0, 0, 0}, nil
	})

	contentA := "the authentication middleware uses JWT RS256 tokens for secure session management"
	contentB := "JWT RS256 tokens used by the authentication middleware for secure session handling"

	id1, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentA, AgentID: "sem-dim", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := st.UpsertMemoryEmbedding(id1, "old-model", []float32{1, 0, 0, 0}); err != nil {
		t.Fatalf("upsert embedding: %v", err)
	}

	id2, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentB, AgentID: "sem-dim", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}

	if id1 == id2 {
		t.Errorf("expected NO dedup (dimension mismatch), but got same ID %s", id1)
	}
}

// TestInsertMemory_SemanticDedup_EmbedderPanic verifies that a panicking
// embedder does not crash the server — the memory is inserted without dedup.
func TestInsertMemory_SemanticDedup_EmbedderPanic(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	st.SetSemanticDedupFunc(func(text string) ([]float32, error) {
		panic("onnx runtime crash")
	})

	contentA := "the authentication middleware uses JWT RS256 tokens for secure session management"
	contentB := "JWT RS256 tokens used by the authentication middleware for secure session handling"

	id1, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentA, AgentID: "sem-panic", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	candidateVec := []float32{1, 0, 0, 0}
	if err := st.UpsertMemoryEmbedding(id1, "test-model", candidateVec); err != nil {
		t.Fatalf("upsert embedding: %v", err)
	}

	// This should NOT panic — the panic is recovered and dedup is skipped.
	id2, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: contentB, AgentID: "sem-panic", Source: SourceAuto,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}

	if id1 == id2 {
		t.Errorf("expected NO dedup after embedder panic, but got same ID %s", id1)
	}
}

// TestInsertMemory_IdenticalProjectMemory_Deduped verifies exact-same content
// from the same agent isn't written twice (end_session retry scenario).
func TestInsertMemory_IdenticalProjectMemory_Deduped(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	content := "Refactored store module to add connection pooling for high throughput."

	for i := 0; i < 3; i++ {
		_, err := st.InsertMemory(Memory{
			Tier: TierProject, Content: content, AgentID: "agent-retry", Source: SourceManual, Tags: `[]`,
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	mems, _ := st.QueryMemories(TierProject, "", "agent-retry", 10)
	if len(mems) != 1 {
		t.Errorf("expected 1 project memory after 3 identical inserts, got %d", len(mems))
	}
}

// TestInsertMemory_SizeCap verifies that content over 2000 chars is truncated.
func TestInsertMemory_SizeCap(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	longContent := strings.Repeat("word ", 600) // ~3000 chars

	id, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: longContent, AgentID: "agent-cap", Source: SourceManual, Tags: `[]`,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	mems, _ := st.QueryMemories(TierProject, "", "agent-cap", 1)
	if len(mems) == 0 {
		t.Fatal("no memories found after insert")
	}
	if len(mems[0].Content) > 2020 {
		t.Errorf("content not truncated: len=%d", len(mems[0].Content))
	}
	if !strings.Contains(mems[0].Content, "[truncated]") {
		t.Errorf("truncation marker missing from content")
	}
	_ = id
}

// TestTouchMemory_NonExistentID verifies TouchMemory returns an error rather
// than silently succeeding when the ID doesn't exist.
func TestTouchMemory_NonExistentID(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	err := st.TouchMemory("nonexistent-id-xyz")
	if err == nil {
		t.Error("expected error for non-existent ID, got nil")
	}
}

// TestQueryMemories_EmptyEntityID_NoEntityFilter verifies that passing an
// empty entityID returns all entity memories rather than filtering to none.
// This is documented behavior — callers must pass explicit entityID to filter.
func TestQueryMemories_EmptyEntityID_NoEntityFilter(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	st.InsertMemory(Memory{Tier: TierEntity, Content: "Memory about node alpha here.", EntityID: "node-alpha", Source: SourceAuto, Tags: `[]`})
	st.InsertMemory(Memory{Tier: TierEntity, Content: "Memory about node beta here.", EntityID: "node-beta", Source: SourceAuto, Tags: `[]`})

	// Empty entityID → returns all entity memories (no entity filter applied).
	all, _ := st.QueryMemories(TierEntity, "", "", 10)
	if len(all) != 2 {
		t.Errorf("expected 2 entity memories with empty entityID filter, got %d", len(all))
	}

	// Specific entityID → filtered to just that node.
	nodeAlpha, _ := st.QueryMemories(TierEntity, "node-alpha", "", 10)
	if len(nodeAlpha) != 1 {
		t.Errorf("expected 1 memory for node-alpha, got %d", len(nodeAlpha))
	}
}

// TestSearchMemories_FindsByContent verifies FTS5 search finds a memory by
// keyword in its content.
func TestSearchMemories_FindsByContent(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	_, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "Switched from bcrypt to argon2id for password hashing due to timing attacks.",
		AgentID: "agent-search",
		Source:  SourceManual,
		Tags:    `[]`,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	results, err := st.SearchMemories("argon2id", 10)
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 result for 'argon2id', got 0")
	}
	if !strings.Contains(results[0].Content, "argon2id") {
		t.Errorf("expected content to contain 'argon2id', got: %s", results[0].Content)
	}
}

// TestSearchMemories_EmptyQuery_ReturnsNil verifies empty query returns
// nil (not an error) — callers use browse mode for listing without a query.
func TestSearchMemories_EmptyQuery_ReturnsNil(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	st.InsertMemory(Memory{
		Tier: TierProject, Content: "Some important project decision made here.", AgentID: "a", Source: SourceManual, Tags: `[]`,
	})

	results, err := st.SearchMemories("", 10)
	if err != nil {
		t.Fatalf("unexpected error for empty query: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results for empty query, got %d", len(results))
	}
}

// TestSearchMemories_CrossTier verifies FTS5 search returns memories from
// multiple tiers in a single query.
func TestSearchMemories_CrossTier(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	st.InsertMemory(Memory{
		Tier: TierProject, Content: "JWT authentication refactored to use refresh tokens.", AgentID: "a", Source: SourceManual, Tags: `[]`,
	})
	st.InsertMemory(Memory{
		Tier: TierEntity, Content: "AuthLogin validates JWT token expiry on every request.", EntityID: "node-auth", Source: SourceAuto, Tags: `[]`,
	})
	st.InsertMemory(Memory{
		Tier: TierSessionLog, Content: "Session worked on JWT token rotation logic.", AgentID: "a", Source: SourceAuto, Tags: `[]`,
	})
	// Unrelated memory — should NOT appear.
	st.InsertMemory(Memory{
		Tier: TierProject, Content: "Database schema uses UUID primary keys everywhere.", AgentID: "a", Source: SourceManual, Tags: `[]`,
	})

	results, err := st.SearchMemories("JWT", 10)
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	if len(results) < 3 {
		t.Errorf("expected at least 3 JWT memories across tiers, got %d", len(results))
	}

	// Verify all results contain JWT (case-insensitive — FTS5 is case-insensitive by default).
	for _, m := range results {
		if !strings.Contains(strings.ToLower(m.Content), "jwt") {
			t.Errorf("result does not contain 'jwt': %s", m.Content)
		}
	}
}

// ── AM-1: Memory Anchors ────────────────────────────────────────────────────

func TestInsertMemoryAnchors_BasicRoundTrip(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	id, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: "AuthService handles all auth flows", AgentID: "a", Source: SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertMemoryAnchors(id, []string{"repo::auth.go::AuthService", "repo::auth.go::Login"}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.knowledgeDB.Query(`SELECT node_id FROM memory_anchors WHERE memory_id = ? ORDER BY node_id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var anchors []string
	for rows.Next() {
		var nid string
		rows.Scan(&nid)
		anchors = append(anchors, nid)
	}
	if len(anchors) != 2 {
		t.Fatalf("expected 2 anchors, got %d", len(anchors))
	}
	if anchors[0] != "repo::auth.go::AuthService" || anchors[1] != "repo::auth.go::Login" {
		t.Errorf("unexpected anchors: %v", anchors)
	}
}

func TestInsertMemoryAnchors_EmptySlice_NoOp(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	id, _ := st.InsertMemory(Memory{
		Tier: TierProject, Content: "some project fact with no anchors", AgentID: "a", Source: SourceManual,
	})
	if err := st.InsertMemoryAnchors(id, nil); err != nil {
		t.Fatal(err)
	}
	anchors := testGetMemoryAnchors(st, id)
	if len(anchors) != 0 {
		t.Errorf("expected 0 anchors, got %d", len(anchors))
	}
}

func TestInsertMemoryAnchors_DuplicateIgnored(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	id, _ := st.InsertMemory(Memory{
		Tier: TierProject, Content: "fact anchored to same node twice", AgentID: "a", Source: SourceManual,
	})
	if err := st.InsertMemoryAnchors(id, []string{"repo::a.go::Foo", "repo::a.go::Foo"}); err != nil {
		t.Fatal(err)
	}
	anchors := testGetMemoryAnchors(st, id)
	if len(anchors) != 1 {
		t.Errorf("expected 1 anchor (deduped), got %d", len(anchors))
	}
}

func TestInsertMemoryAnchors_EmptyNodeIDSkipped(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	id, _ := st.InsertMemory(Memory{
		Tier: TierProject, Content: "fact with empty anchor nodes mixed in", AgentID: "a", Source: SourceManual,
	})
	if err := st.InsertMemoryAnchors(id, []string{"repo::a.go::Foo", "", "repo::b.go::Bar"}); err != nil {
		t.Fatal(err)
	}
	anchors := testGetMemoryAnchors(st, id)
	if len(anchors) != 2 {
		t.Errorf("expected 2 anchors (empty skipped), got %d", len(anchors))
	}
}

func TestExpireMemories_CleansOrphanedAnchors(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	// Insert a memory with a very short TTL (already expired).
	id, err := st.InsertMemory(Memory{
		Tier:      TierProject,
		Content:   "this memory will expire and its anchors should be cleaned",
		AgentID:   "a",
		Source:    SourceManual,
		ExpiresAt: time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertMemoryAnchors(id, []string{"repo::a.go::Foo"}); err != nil {
		t.Fatal(err)
	}
	// Verify anchor exists before expiry.
	anchors := testGetMemoryAnchors(st, id)
	if len(anchors) != 1 {
		t.Fatalf("expected 1 anchor before expiry, got %d", len(anchors))
	}
	// Expire memories — should also clean up anchors.
	n, err := st.ExpireMemories()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 expired memory, got %d", n)
	}
	// Verify anchor was cleaned up.
	anchors = testGetMemoryAnchors(st, id)
	if len(anchors) != 0 {
		t.Errorf("expected 0 anchors after expiry cleanup, got %d", len(anchors))
	}
}

func TestInsertMemoryAnchors_DedupedMemory_AddsAnchors(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	// Insert original memory with anchors.
	id1, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: "AuthService handles all authentication flows in the system", AgentID: "a", Source: SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertMemoryAnchors(id1, []string{"repo::auth.go::AuthService"}); err != nil {
		t.Fatal(err)
	}
	// Insert near-duplicate memory — should dedup and return id1.
	id2, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: "AuthService handles all authentication flows in the system today", AgentID: "a", Source: SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Fatalf("expected dedup to return same ID %s, got %s", id1, id2)
	}
	// Add additional anchors to the deduped memory.
	if err := st.InsertMemoryAnchors(id2, []string{"repo::auth.go::Login"}); err != nil {
		t.Fatal(err)
	}
	// Both anchors should exist.
	anchors := testGetMemoryAnchors(st, id1)
	if len(anchors) != 2 {
		t.Errorf("expected 2 anchors (original + dedup additive), got %d: %v", len(anchors), anchors)
	}
}

func TestInsertMemoryWithAnchors_DedupPath_AtomicTouchAndAnchors(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Insert original memory.
	id1, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: "AuthService handles all authentication flows in the codebase", AgentID: "a", Source: SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Insert near-duplicate WITH anchors — should dedup and atomically touch + anchor.
	id2, err := st.InsertMemoryWithAnchors(Memory{
		Tier: TierProject, Content: "AuthService handles all authentication flows in the codebase currently", AgentID: "a", Source: SourceManual,
	}, []string{"repo::auth.go::AuthService", "repo::auth.go::Login"})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Fatalf("expected dedup, got new ID %s vs %s", id2, id1)
	}

	// Atomicity proof: if anchors exist, the tx committed — which means the
	// touch (UPDATE last_accessed_at) in the same tx also committed.
	// If the tx had rolled back, neither touch nor anchors would be present.
	anchors := testGetMemoryAnchors(st, id1)
	if len(anchors) != 2 {
		t.Fatalf("expected 2 anchors on deduped memory (proves tx committed), got %d: %v", len(anchors), anchors)
	}

	// Verify the memory still exists and is queryable (touch didn't corrupt it).
	mems, _ := st.QueryMemories(TierProject, "", "a", 1)
	if len(mems) == 0 {
		t.Fatal("expected memory to still be queryable after dedup+touch+anchor tx")
	}
	if mems[0].ID != id1 {
		t.Errorf("expected original memory ID %s, got %s", id1, mems[0].ID)
	}
}

func TestInsertMemoryWithAnchors_RollsBackOnAnchorFailure(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	// Drop the memory_anchors table so anchor INSERT fails inside the tx.
	if _, err := st.knowledgeDB.Exec(`DROP TABLE memory_anchors`); err != nil {
		t.Fatal(err)
	}
	_, err := st.InsertMemoryWithAnchors(Memory{
		Tier: TierProject, Content: "this memory should NOT exist after rollback", AgentID: "a", Source: SourceManual,
	}, []string{"repo::file.go::Func"})
	if err == nil {
		t.Fatal("expected error when memory_anchors table is missing")
	}
	// Verify the memory was NOT inserted (tx rolled back).
	mems, _ := st.QueryMemories(TierProject, "", "a", 10)
	for _, m := range mems {
		if m.Content == "this memory should NOT exist after rollback" {
			t.Error("memory should not exist after tx rollback — atomicity violated")
		}
	}
}

// --- AM-2: MarkAnchoredMemoriesStale tests ---

// TestMarkAnchoredMemoriesStale_EmptyNodeIDs is a no-op — no SQL should run.
func TestMarkAnchoredMemoriesStale_EmptyNodeIDs(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	if err := st.MarkAnchoredMemoriesStale(nil, "reason"); err != nil {
		t.Fatalf("unexpected error on empty nodeIDs: %v", err)
	}
}

// TestMarkAnchoredMemoriesStale_NoAnchors verifies that a node with no anchored
// memories produces no error and marks nothing.
func TestMarkAnchoredMemoriesStale_NoAnchors(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	id, _ := st.InsertMemory(Memory{
		Tier: TierProject, Content: "fact with no anchor", AgentID: "a", Source: SourceManual,
	})
	if err := st.MarkAnchoredMemoriesStale([]string{"repo::x.go::Foo"}, "anchor node removed"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Memory should not be stale.
	rows, err := st.knowledgeDB.Query(`SELECT stale FROM memories WHERE id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var stale int
		_ = rows.Scan(&stale)
		if stale != 0 {
			t.Errorf("expected stale=0 for unanchored memory, got %d", stale)
		}
	}
}

// TestMarkAnchoredMemoriesStale_MarksAnchored verifies that a memory anchored
// to the given node ID is flagged stale with the correct reason.
func TestMarkAnchoredMemoriesStale_MarksAnchored(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	id, _ := st.InsertMemory(Memory{
		Tier: TierProject, Content: "Store.Close has 96 callers", AgentID: "a", Source: SourceManual,
	})
	if err := st.InsertMemoryAnchors(id, []string{"repo::store.go::Store.Close"}); err != nil {
		t.Fatal(err)
	}

	if err := st.MarkAnchoredMemoriesStale([]string{"repo::store.go::Store.Close"}, "anchor node removed"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rows, err := st.knowledgeDB.Query(`SELECT stale, stale_reason FROM memories WHERE id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("memory not found")
	}
	var stale int
	var reason string
	_ = rows.Scan(&stale, &reason)
	if stale != 1 {
		t.Errorf("expected stale=1, got %d", stale)
	}
	if reason != "anchor node removed" {
		t.Errorf("expected stale_reason='anchor node removed', got %q", reason)
	}
}

// TestMarkAnchoredMemoriesStale_BatchNodes verifies multiple node IDs in one call.
func TestMarkAnchoredMemoriesStale_BatchNodes(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	id1, _ := st.InsertMemory(Memory{Tier: TierProject, Content: "fact about Foo", AgentID: "a", Source: SourceManual})
	id2, _ := st.InsertMemory(Memory{Tier: TierProject, Content: "fact about Bar", AgentID: "a", Source: SourceManual})
	id3, _ := st.InsertMemory(Memory{Tier: TierProject, Content: "unrelated fact", AgentID: "a", Source: SourceManual})

	_ = st.InsertMemoryAnchors(id1, []string{"repo::a.go::Foo"})
	_ = st.InsertMemoryAnchors(id2, []string{"repo::b.go::Bar"})

	if err := st.MarkAnchoredMemoriesStale([]string{"repo::a.go::Foo", "repo::b.go::Bar"}, "anchor node removed"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ id string; wantStale int }{
		{id1, 1}, {id2, 1}, {id3, 0},
	} {
		var stale int
		_ = st.knowledgeDB.QueryRow(`SELECT stale FROM memories WHERE id = ?`, tc.id).Scan(&stale)
		if stale != tc.wantStale {
			t.Errorf("id=%s: expected stale=%d, got %d", tc.id, tc.wantStale, stale)
		}
	}
}

// TestMarkAnchoredMemoriesStale_Idempotent verifies calling twice does not error.
func TestMarkAnchoredMemoriesStale_Idempotent(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	id, _ := st.InsertMemory(Memory{Tier: TierProject, Content: "fact about Foo", AgentID: "a", Source: SourceManual})
	_ = st.InsertMemoryAnchors(id, []string{"repo::a.go::Foo"})

	if err := st.MarkAnchoredMemoriesStale([]string{"repo::a.go::Foo"}, "anchor node removed"); err != nil {
		t.Fatal(err)
	}
	// Call again — should not error.
	if err := st.MarkAnchoredMemoriesStale([]string{"repo::a.go::Foo"}, "anchor node removed"); err != nil {
		t.Fatalf("idempotent second call failed: %v", err)
	}
	var stale int
	_ = st.knowledgeDB.QueryRow(`SELECT stale FROM memories WHERE id = ?`, id).Scan(&stale)
	if stale != 1 {
		t.Errorf("expected stale=1 after idempotent call, got %d", stale)
	}
}

// TestSearchMemories_NoResults verifies a query with no matches returns empty
// slice (not an error).
func TestSearchMemories_NoResults(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	st.InsertMemory(Memory{
		Tier: TierProject, Content: "Database migration added new index on users table.", AgentID: "a", Source: SourceManual, Tags: `[]`,
	})

	results, err := st.SearchMemories("xyznonexistent", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for nonsense query, got %d", len(results))
	}
}

// TestInsertMemory_Dedup_SkipsStaleMemories verifies that deduplication does NOT
// match against stale (AM-2 invalidated) memories — Gap 2 fix.
// Without the fix, a new write similar to a stale memory would call TouchMemory
// on the stale ID, extending its TTL and resurrecting invalidated data.
func TestInsertMemory_Dedup_SkipsStaleMemories(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Insert a memory, then mark it stale via a fake anchor cascade.
	content := "Store.Close serializes shutdown through reparseMu lock."
	id1, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: content, AgentID: "agent-z", Source: SourceAuto, Tags: `[]`,
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Simulate AM-2 cascade: manually set stale=1 on the memory.
	_, err = st.knowledgeDB.Exec(`UPDATE memories SET stale = 1, stale_reason = 'anchor node removed' WHERE id = ?`, id1)
	if err != nil {
		t.Fatalf("force stale: %v", err)
	}

	// Insert a semantically similar memory. Without Gap 2 fix this would dedup
	// to id1 (the stale memory) and extend its TTL. With the fix it must get a
	// new ID because stale memories are excluded from dedup candidates.
	// Add one word to stay above 0.85 similarity threshold but still very close.
	id2, err := st.InsertMemory(Memory{
		Tier: TierProject, Content: content + " Confirmed.", AgentID: "agent-z", Source: SourceAuto, Tags: `[]`,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}

	if id1 == id2 {
		t.Errorf("dedup matched a stale memory (id=%s) — stale memory was resurrected", id1)
	}

	// Verify id1 is still stale (TouchMemory was NOT called on it).
	var stale int
	st.knowledgeDB.QueryRow(`SELECT stale FROM memories WHERE id = ?`, id1).Scan(&stale)
	if stale != 1 {
		t.Errorf("stale memory should remain stale=1, got stale=%d", stale)
	}
}

// TestMarkAnchoredMemoriesStale_LargeBatch verifies that batching works correctly
// for nodeID slices exceeding SQLite's 999-variable limit — Gap 5 fix.
func TestMarkAnchoredMemoriesStale_LargeBatch(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Insert one memory and anchor it to node "node-0".
	id, err := st.InsertMemoryWithAnchors(
		Memory{Tier: TierProject, Content: "Large batch test memory for variable limit.", AgentID: "a", Source: SourceAuto, Tags: `[]`},
		[]string{"node-0"},
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Build a nodeID slice of 600 entries (> 500 batch size, > 999/2).
	// node-0 is the real anchor; the rest are phantom IDs that match nothing.
	nodeIDs := make([]string, 600)
	nodeIDs[0] = "node-0"
	for i := 1; i < 600; i++ {
		nodeIDs[i] = strings.Repeat("x", 10) + string(rune('a'+i%26)) // unique phantoms
	}

	// Must not fail with "too many SQL variables".
	if err := st.MarkAnchoredMemoriesStale(nodeIDs, "large batch test"); err != nil {
		t.Fatalf("MarkAnchoredMemoriesStale with 600 nodes: %v", err)
	}

	// Memory anchored to node-0 must be stale.
	var stale int
	var reason string
	st.knowledgeDB.QueryRow(`SELECT stale, stale_reason FROM memories WHERE id = ?`, id).Scan(&stale, &reason)
	if stale != 1 {
		t.Errorf("expected stale=1, got %d", stale)
	}
	if reason != "large batch test" {
		t.Errorf("unexpected stale_reason: %q", reason)
	}
}

// ── AM-3: QueryInvalidatedMemories + MarkMemoriesSurfaced ──────────────

func TestQueryInvalidatedMemories_ReturnsStaleUnsurfaced(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Insert a memory, then mark it stale.
	id, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "synapses-intelligence is an active sidecar at localhost:11435",
		EntityID: "repo::intel/main.go::main",
		AgentID:  "agent-1",
		Source:   SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkEntityMemoriesStaleForNodes([]string{"repo::intel/main.go::main"}, "anchor node removed"); err != nil {
		t.Fatal(err)
	}

	// Query should return 1 invalidated memory.
	mems, err := st.QueryInvalidatedMemories("agent-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Fatalf("expected 1, got %d", len(mems))
	}
	if mems[0].ID != id {
		t.Errorf("expected id=%q, got %q", id, mems[0].ID)
	}
	if mems[0].StaleReason != "anchor node removed" {
		t.Errorf("unexpected stale_reason: %q", mems[0].StaleReason)
	}
	if mems[0].Tier != TierEntity {
		t.Errorf("expected tier=%q, got %q", TierEntity, mems[0].Tier)
	}
}

func TestQueryInvalidatedMemories_EmptyWhenNoneStale(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Insert a non-stale memory.
	_, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "this project uses Go modules for dependency management",
		Source:  SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	mems, err := st.QueryInvalidatedMemories("agent-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 0 {
		t.Fatalf("expected 0, got %d", len(mems))
	}
}

func TestMarkMemoriesSurfaced_PreventsRequery(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "Store.Close has 96 callers — high blast radius refactor target",
		EntityID: "repo::store.go::Store.Close",
		AgentID:  "agent-1",
		Source:   SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkEntityMemoriesStaleForNodes([]string{"repo::store.go::Store.Close"}, "node changed"); err != nil {
		t.Fatal(err)
	}

	// First query returns the memory.
	mems, err := st.QueryInvalidatedMemories("agent-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Fatalf("expected 1, got %d", len(mems))
	}

	// Mark surfaced.
	if err := st.MarkMemoriesSurfaced("agent-1", []string{id}); err != nil {
		t.Fatal(err)
	}

	// Second query returns empty — surfaced_at is set.
	mems, err = st.QueryInvalidatedMemories("agent-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 0 {
		t.Fatalf("expected 0 after surfacing, got %d", len(mems))
	}
}

func TestMarkMemoriesSurfaced_EmptyIDsNoop(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	if err := st.MarkMemoriesSurfaced("agent-1", nil); err != nil {
		t.Fatal(err)
	}
}

// TestQueryInvalidatedMemories_PerAgentIsolation verifies that surfacing
// for agent-A does NOT prevent agent-B from seeing the same invalidated memory.
func TestQueryInvalidatedMemories_PerAgentIsolation(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	_, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "auth middleware uses JWT tokens for session management in this codebase",
		EntityID: "repo::auth/middleware.go::AuthMiddleware",
		Source:   SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkEntityMemoriesStaleForNodes([]string{"repo::auth/middleware.go::AuthMiddleware"}, "node removed"); err != nil {
		t.Fatal(err)
	}

	// Both agents see the invalidated memory initially.
	memsA, err := st.QueryInvalidatedMemories("agent-A", 10)
	if err != nil {
		t.Fatal(err)
	}
	memsB, err := st.QueryInvalidatedMemories("agent-B", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(memsA) != 1 || len(memsB) != 1 {
		t.Fatalf("both agents should see 1 invalidated memory; A=%d B=%d", len(memsA), len(memsB))
	}

	// Agent-A surfaces it.
	if err := st.MarkMemoriesSurfaced("agent-A", []string{memsA[0].ID}); err != nil {
		t.Fatal(err)
	}

	// Agent-A no longer sees it, but Agent-B still does.
	memsA, err = st.QueryInvalidatedMemories("agent-A", 10)
	if err != nil {
		t.Fatal(err)
	}
	memsB, err = st.QueryInvalidatedMemories("agent-B", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(memsA) != 0 {
		t.Errorf("agent-A should see 0 after surfacing, got %d", len(memsA))
	}
	if len(memsB) != 1 {
		t.Errorf("agent-B should still see 1 (not surfaced for them), got %d", len(memsB))
	}
}

// TestQueryInvalidatedMemories_StaledAtOrdering verifies that memories are
// ordered by staled_at (when invalidated), not created_at (when written).
func TestQueryInvalidatedMemories_StaledAtOrdering(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Insert two memories at different times. Both get staled, but in reverse order.
	id1, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "first memory written earlier but invalidated later to test ordering",
		EntityID: "repo::first.go::First",
		Source:   SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "second memory written later but invalidated first to test ordering",
		EntityID: "repo::second.go::Second",
		Source:   SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Stale id2 first, then id1 — so id1 has a LATER staled_at.
	if err := st.MarkEntityMemoriesStaleForNodes([]string{"repo::second.go::Second"}, "removed first"); err != nil {
		t.Fatal(err)
	}
	// Small delay to ensure staled_at differs.
	time.Sleep(10 * time.Millisecond)
	if err := st.MarkEntityMemoriesStaleForNodes([]string{"repo::first.go::First"}, "removed second"); err != nil {
		t.Fatal(err)
	}

	mems, err := st.QueryInvalidatedMemories("agent-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 2 {
		t.Fatalf("expected 2, got %d", len(mems))
	}
	// Most recently staled should be first (id1 was staled second).
	if mems[0].ID != id1 {
		t.Errorf("expected most recently staled (id1=%s) first, got %s", id1, mems[0].ID)
	}
	if mems[1].ID != id2 {
		t.Errorf("expected earlier staled (id2=%s) second, got %s", id2, mems[1].ID)
	}
}

// TestQueryInvalidatedMemories_AnonymousFallback verifies that anonymous
// sessions (empty agentID) use the legacy surfaced_at path correctly, AND that
// a named agent surfacing does NOT poison the anonymous path.
func TestQueryInvalidatedMemories_AnonymousFallback(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	_, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "config service uses gRPC for inter-service communication in this project",
		EntityID: "repo::config/service.go::ConfigService",
		Source:   SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkEntityMemoriesStaleForNodes([]string{"repo::config/service.go::ConfigService"}, "node removed"); err != nil {
		t.Fatal(err)
	}

	// Named agent surfaces the memory.
	memsNamed, err := st.QueryInvalidatedMemories("agent-named", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(memsNamed) != 1 {
		t.Fatalf("named agent should see 1, got %d", len(memsNamed))
	}
	if err := st.MarkMemoriesSurfaced("agent-named", []string{memsNamed[0].ID}); err != nil {
		t.Fatal(err)
	}

	// Anonymous session should STILL see the memory (legacy surfaced_at not set).
	memsAnon, err := st.QueryInvalidatedMemories("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(memsAnon) != 1 {
		t.Fatalf("anonymous session should still see 1 after named agent surfaced, got %d", len(memsAnon))
	}

	// Anonymous session surfaces it.
	if err := st.MarkMemoriesSurfaced("", []string{memsAnon[0].ID}); err != nil {
		t.Fatal(err)
	}

	// Now anonymous should see 0.
	memsAnon2, err := st.QueryInvalidatedMemories("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(memsAnon2) != 0 {
		t.Fatalf("anonymous session should see 0 after own surfacing, got %d", len(memsAnon2))
	}
}

// TestExpireMemories_CleansOrphanedSurfacedRows verifies that ExpireMemories
// removes orphaned memory_surfaced rows when their parent memory is deleted.
func TestExpireMemories_CleansOrphanedSurfacedRows(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "Store.Close has 96 callers and is a high blast radius refactor target",
		EntityID: "repo::store.go::Store.Close",
		Source:   SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Mark stale (shortens TTL to 30d).
	if err := st.MarkEntityMemoriesStaleForNodes([]string{"repo::store.go::Store.Close"}, "removed"); err != nil {
		t.Fatal(err)
	}
	// Surface for an agent — creates a memory_surfaced row.
	if err := st.MarkMemoriesSurfaced("agent-1", []string{id}); err != nil {
		t.Fatal(err)
	}

	// Verify memory_surfaced row exists.
	var surfCount int
	st.knowledgeDB.QueryRow(`SELECT count(*) FROM memory_surfaced WHERE memory_id = ?`, id).Scan(&surfCount)
	if surfCount != 1 {
		t.Fatalf("expected 1 memory_surfaced row, got %d", surfCount)
	}

	// Force-expire by setting expires_at to the past.
	st.knowledgeDB.Exec(`UPDATE memories SET expires_at = '2020-01-01T00:00:00Z' WHERE id = ?`, id)

	n, err := st.ExpireMemories()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 expired, got %d", n)
	}

	// memory_surfaced row should be cleaned up.
	st.knowledgeDB.QueryRow(`SELECT count(*) FROM memory_surfaced WHERE memory_id = ?`, id).Scan(&surfCount)
	if surfCount != 0 {
		t.Fatalf("expected 0 orphaned memory_surfaced rows, got %d", surfCount)
	}
}

// TestQueryMemories_ExcludesStaleMemories verifies that QueryMemories,
// QueryMemoriesForEntities, and SearchMemories all filter out stale=1 memories.
// This is the Gap 1 fix: stale memories must NOT appear as active truth.
func TestQueryMemories_ExcludesStaleMemories(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	entityID := "repo::store.go::Store.Close"
	_, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "Store.Close was refactored to accept projectID parameter for this entity",
		EntityID: entityID,
		AgentID:  "agent-1",
		Source:   SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Before staling: all queries return the memory.
	mems, err := st.QueryMemories(TierEntity, entityID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Fatalf("before staling: expected 1 from QueryMemories, got %d", len(mems))
	}

	entMems, err := st.QueryMemoriesForEntities([]string{entityID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entMems[entityID]) != 1 {
		t.Fatalf("before staling: expected 1 from QueryMemoriesForEntities, got %d", len(entMems[entityID]))
	}

	// Mark stale.
	if err := st.MarkEntityMemoriesStaleForNodes([]string{entityID}, "node removed"); err != nil {
		t.Fatal(err)
	}

	// After staling: all queries must return 0.
	mems, err = st.QueryMemories(TierEntity, entityID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 0 {
		t.Fatalf("after staling: expected 0 from QueryMemories, got %d", len(mems))
	}

	entMems, err = st.QueryMemoriesForEntities([]string{entityID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entMems[entityID]) != 0 {
		t.Fatalf("after staling: expected 0 from QueryMemoriesForEntities, got %d", len(entMems[entityID]))
	}

	// SearchMemories should also exclude stale.
	searchRes, err := st.SearchMemories("refactored projectID", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(searchRes) != 0 {
		t.Fatalf("after staling: expected 0 from SearchMemories, got %d", len(searchRes))
	}
}

// ── AM-4: include_stale audit methods ────────────────────────────────────────

// TestQueryMemoriesIncludingStale_ReturnsStaledMemory verifies that
// QueryMemoriesIncludingStale surfaces stale memories that QueryMemories hides.
func TestQueryMemoriesIncludingStale_ReturnsStaledMemory(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	entityID := "repo::audit.go::AuditFunc"
	_, err := st.InsertMemory(Memory{
		Tier:     TierEntity,
		Content:  "AuditFunc was refactored to drop the legacy parameter",
		EntityID: entityID,
		AgentID:  "agent-audit",
		Source:   SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mark stale: normal query returns 0, audit query returns 1.
	if err := st.MarkEntityMemoriesStaleForNodes([]string{entityID}, "node removed"); err != nil {
		t.Fatal(err)
	}

	normal, err := st.QueryMemories(TierEntity, entityID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(normal) != 0 {
		t.Fatalf("QueryMemories: expected 0 stale results, got %d", len(normal))
	}

	audit, err := st.QueryMemoriesIncludingStale(TierEntity, entityID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 {
		t.Fatalf("QueryMemoriesIncludingStale: expected 1, got %d", len(audit))
	}
}

// TestQueryMemoriesIncludingStale_EmptyDB_ReturnsNil verifies no panic on empty store.
func TestQueryMemoriesIncludingStale_EmptyDB_ReturnsNil(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	mems, err := st.QueryMemoriesIncludingStale("", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 0 {
		t.Fatalf("expected 0, got %d", len(mems))
	}
}

// TestSearchMemoriesIncludingStale_ReturnsStaledMemory verifies FTS also surfaces stale
// when using the audit variant.
func TestSearchMemoriesIncludingStale_ReturnsStaledMemory(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	_, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "legacy auth handler was archived and removed from active codebase",
		AgentID: "agent-audit",
		Source:  SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Stale the memory directly via SQL (no entity anchor).
	_, dbErr := st.knowledgeDB.Exec(`UPDATE memories SET stale = 1 WHERE content LIKE '%legacy auth handler%'`)
	if dbErr != nil {
		t.Fatal(dbErr)
	}

	// Normal search: 0 results.
	normal, err := st.SearchMemories("legacy auth handler", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(normal) != 0 {
		t.Fatalf("SearchMemories: expected 0 stale results, got %d", len(normal))
	}

	// Audit search: 1 result.
	audit, err := st.SearchMemoriesIncludingStale("legacy auth handler", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 {
		t.Fatalf("SearchMemoriesIncludingStale: expected 1, got %d", len(audit))
	}
}

// TestQueryRecentSessionMemoriesIncludingStale_ReturnsStaledSession verifies the
// session-log audit variant surfaces stale session memories.
func TestQueryRecentSessionMemoriesIncludingStale_ReturnsStaledSession(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	_, err := st.InsertMemory(Memory{
		Tier:    TierSessionLog,
		Content: "session log: worked on deprecated auth module",
		AgentID: "agent-audit",
		Source:  SourceAuto,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Manually stale the session-log memory.
	_, dbErr := st.knowledgeDB.Exec(`UPDATE memories SET stale = 1 WHERE tier = 'session_log'`)
	if dbErr != nil {
		t.Fatal(dbErr)
	}

	normal, err := st.QueryRecentSessionMemories("agent-audit", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(normal) != 0 {
		t.Fatalf("QueryRecentSessionMemories: expected 0 stale, got %d", len(normal))
	}

	// Query including stale via direct SQL (QueryRecentSessionMemoriesIncludingStale was removed).
	var auditCount int
	st.knowledgeDB.QueryRow(`SELECT COUNT(*) FROM memories WHERE tier = 'session_log' AND agent_id = ?`, "agent-audit").Scan(&auditCount)
	if auditCount != 1 {
		t.Fatalf("expected 1 session-log memory (including stale), got %d", auditCount)
	}
}

// TestMarkAnchoredMemoriesStale_PartialAnchorSurvival verifies that a memory with
// two anchors is NOT staled when only one anchor is removed (the surviving anchor
// means the belief is still partially valid).
func TestMarkAnchoredMemoriesStale_PartialAnchorSurvival(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	memID, err := st.InsertMemoryWithAnchors(Memory{
		Tier:    TierProject,
		Content: "Store.Close and Graph.New share a lifecycle dependency",
		Source:  SourceManual,
	}, []string{"nodeA", "nodeB"})
	if err != nil {
		t.Fatal(err)
	}

	// Remove only nodeA — nodeB still exists. Memory must NOT stale.
	if err := st.MarkAnchoredMemoriesStale([]string{"nodeA"}, "nodeA removed"); err != nil {
		t.Fatal(err)
	}

	mems, err := st.QueryMemories(TierProject, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mems {
		if m.ID == memID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("memory with a surviving anchor should NOT be staled when only one anchor is removed")
	}
}

// TestMarkAnchoredMemoriesStale_AllAnchorsRemoved verifies that a memory IS staled
// when ALL its anchor nodes are in the removal batch.
func TestMarkAnchoredMemoriesStale_AllAnchorsRemoved(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	memID, err := st.InsertMemoryWithAnchors(Memory{
		Tier:    TierProject,
		Content: "Store.Close and Graph.New share a lifecycle dependency",
		Source:  SourceManual,
	}, []string{"nodeA", "nodeB"})
	if err != nil {
		t.Fatal(err)
	}

	// Remove both nodeA and nodeB in one batch — memory must stale.
	if err := st.MarkAnchoredMemoriesStale([]string{"nodeA", "nodeB"}, "both nodes removed"); err != nil {
		t.Fatal(err)
	}

	mems, err := st.QueryMemories(TierProject, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mems {
		if m.ID == memID {
			t.Fatal("memory with all anchors removed should be staled")
		}
	}

	// Confirm it appears in audit query.
	audit, err := st.QueryMemoriesIncludingStale(TierProject, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range audit {
		if m.ID == memID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("staled memory should appear in QueryMemoriesIncludingStale")
	}
}

// TestMarkAnchoredMemoriesStale_SingleAnchorStales verifies that a memory with
// exactly one anchor IS staled when that anchor is removed (single anchor = all anchors).
func TestMarkAnchoredMemoriesStale_SingleAnchorStales(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	memID, err := st.InsertMemoryWithAnchors(Memory{
		Tier:    TierProject,
		Content: "Only anchored to nodeX",
		Source:  SourceManual,
	}, []string{"nodeX"})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.MarkAnchoredMemoriesStale([]string{"nodeX"}, "nodeX removed"); err != nil {
		t.Fatal(err)
	}

	mems, err := st.QueryMemories(TierProject, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mems {
		if m.ID == memID {
			t.Fatal("single-anchor memory should be staled when its anchor is removed")
		}
	}
}

func TestQueryInvalidatedMemories_CapsAt10(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Insert 12 memories and mark all stale.
	for i := 0; i < 12; i++ {
		_, err := st.InsertMemory(Memory{
			Tier:     TierEntity,
			Content:  strings.Repeat("memory content for invalidation test ", 3) + string(rune('A'+i)),
			EntityID: "repo::entity" + string(rune('A'+i)),
			Source:   SourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.MarkEntityMemoriesStaleForNodes([]string{"repo::entity"+string(rune('A'+i))}, "removed"); err != nil {
			t.Fatal(err)
		}
	}

	mems, err := st.QueryInvalidatedMemories("agent-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 10 {
		t.Fatalf("expected cap at 10, got %d", len(mems))
	}
}

// TestSearchMemories_FTS5Injection verifies that malicious FTS5 syntax in the
// query does not cause a crash or SQL error in SearchMemories and
// SearchMemoriesIncludingStale. Before the fix, raw queries like "NOT *" were
// passed directly to FTS5 MATCH, which caused a parse error in SQLite.
func TestSearchMemories_FTS5Injection(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Seed one memory so the FTS index is non-empty and queries execute fully.
	_, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "auth service switched to OAuth2",
		AgentID: "agent-sec",
		Source:  SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	maliciousQueries := []string{
		"NOT *",
		`"unclosed`,
		`(unbalanced`,
		`* * *`,
		`-- DROP TABLE memories`,
		`OR OR OR`,
		`auth) AND (1=1`,
		`\x00null`,
	}

	for _, q := range maliciousQueries {
		q := q
		t.Run("SearchMemories/"+q, func(t *testing.T) {
			results, err := st.SearchMemories(q, 10)
			if err != nil {
				t.Errorf("SearchMemories(%q) returned error: %v", q, err)
			}
			_ = results // nil or empty is fine — no crash is the requirement
		})
		t.Run("SearchMemoriesIncludingStale/"+q, func(t *testing.T) {
			results, err := st.SearchMemoriesIncludingStale(q, 10)
			if err != nil {
				t.Errorf("SearchMemoriesIncludingStale(%q) returned error: %v", q, err)
			}
			_ = results
		})
	}
}

// TestSearchMemories_FTS5InjectionAttackVector confirms that the specific
// attack vector from the council finding (query: "NOT *") is closed.
// Without the fix, this would cause: "fts5: syntax error near "*"".
func TestSearchMemories_FTS5InjectionAttackVector(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	_, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "sensitive data that should not be dumped",
		AgentID: "agent-sec",
		Source:  SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	// "NOT *" is the council-specified attack vector. Before the fix it caused:
	// "fts5: syntax error near "NOT"" — an error that could crash callers.
	// After the fix, "NOT *" is sanitized to a quoted prefix search "NOT"*
	// (words starting with "NOT"), which is valid FTS5 and returns at most
	// the rows that genuinely contain such a word — not all rows.
	results, err := st.SearchMemories("NOT *", 100)
	if err != nil {
		t.Fatalf("SearchMemories(\"NOT *\") must not return error, got: %v", err)
	}
	// The store has 1 seeded memory. "NOT *" → "NOT"* may match "not" in the content
	// (legitimate prefix match), but must never exceed the total document count.
	if len(results) > 1 {
		t.Errorf("SearchMemories(\"NOT *\") returned %d results, expected ≤1 (only 1 document seeded)", len(results))
	}

	results, err = st.SearchMemoriesIncludingStale("NOT *", 100)
	if err != nil {
		t.Fatalf("SearchMemoriesIncludingStale(\"NOT *\") must not return error, got: %v", err)
	}
	if len(results) > 1 {
		t.Errorf("SearchMemoriesIncludingStale(\"NOT *\") returned %d results, expected ≤1 (only 1 document seeded)", len(results))
	}
}

// ── GetMemoriesByAnchorNode ─────────────────────────────────────────────────

func TestGetMemoriesByAnchorNode_BasicRoundTrip(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	nodeID := "repo::auth.go::AuthService"

	memID, _ := st.InsertMemory(Memory{
		Tier: TierProject, Content: "Auth redesign planned", AgentID: "a", Source: SourceManual,
	})
	_ = st.InsertMemoryAnchors(memID, []string{nodeID})

	mems, err := st.GetMemoriesByAnchorNode(nodeID, 10)
	if err != nil {
		t.Fatalf("GetMemoriesByAnchorNode: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(mems))
	}
	if mems[0].Content != "Auth redesign planned" {
		t.Errorf("expected content match, got %q", mems[0].Content)
	}
}

func TestGetMemoriesByAnchorNode_EmptyNodeID(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	mems, err := st.GetMemoriesByAnchorNode("", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mems != nil {
		t.Errorf("expected nil for empty nodeID, got %v", mems)
	}
}

func TestGetMemoriesByAnchorNode_ExcludesStale(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	nodeID := "repo::auth.go::AuthService"

	memID, _ := st.InsertMemory(Memory{
		Tier: TierProject, Content: "Stale memory", AgentID: "a", Source: SourceManual,
	})
	_ = st.InsertMemoryAnchors(memID, []string{nodeID})

	// Mark memory as stale.
	_, _ = st.knowledgeDB.Exec(`UPDATE memories SET stale = 1 WHERE id = ?`, memID)

	mems, err := st.GetMemoriesByAnchorNode(nodeID, 10)
	if err != nil {
		t.Fatalf("GetMemoriesByAnchorNode: %v", err)
	}
	if len(mems) != 0 {
		t.Errorf("expected 0 memories (stale excluded), got %d", len(mems))
	}
}

func TestGetMemoriesByAnchorNode_NoMatch(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	memID, _ := st.InsertMemory(Memory{
		Tier: TierProject, Content: "Unrelated memory", AgentID: "a", Source: SourceManual,
	})
	_ = st.InsertMemoryAnchors(memID, []string{"repo::other.go::Other"})

	mems, err := st.GetMemoriesByAnchorNode("repo::auth.go::AuthService", 10)
	if err != nil {
		t.Fatalf("GetMemoriesByAnchorNode: %v", err)
	}
	if len(mems) != 0 {
		t.Errorf("expected 0 memories for unmatched node, got %d", len(mems))
	}
}

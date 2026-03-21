package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// testMemoryEmbeddingCount counts rows in memory_embeddings via direct SQL (replaces removed method).
func testMemoryEmbeddingCount(st *Store) int {
	var count int
	st.knowledgeDB.QueryRow(`SELECT COUNT(*) FROM memory_embeddings`).Scan(&count)
	return count
}

// testGetStaleEmbeddingMemoryIDs queries stale embedding memory IDs via direct SQL (replaces removed method).
func testGetStaleEmbeddingMemoryIDs(st *Store, limit int) ([]string, error) {
	rows, err := st.knowledgeDB.Query(`
		SELECT e.memory_id FROM memory_embeddings e
		JOIN memories m ON e.memory_id = m.id
		WHERE e.stale = 1 AND m.stale = 0
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, nil
}

// testDeleteMemoryEmbeddings deletes embedding rows by memory IDs via direct SQL (replaces removed method).
func testDeleteMemoryEmbeddings(st *Store, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, err := st.knowledgeDB.Exec(`DELETE FROM memory_embeddings WHERE memory_id IN (`+placeholders+`)`, args...)
	return err
}

// openMemEmbedTestStore creates a temporary Store with a seeded memory for embedding tests.
// Uses the pre-initialized template DB from TestMain for fast setup.
func openMemEmbedTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	st := openFromTemplate(t)

	// Insert a test memory.
	memID, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "AuthService uses JWT tokens for session management with RS256 signing",
		AgentID: "test-agent",
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}
	return st, memID
}

// seedMultipleMemories inserts several memories and returns their IDs.
func seedMultipleMemories(t *testing.T, st *Store) []string {
	t.Helper()
	contents := []string{
		"AuthService handles JWT token validation and session management",
		"CacheLayer implements Redis-backed caching with TTL expiry",
		"DatabaseMigrator runs schema migrations on startup with rollback support",
		"PaymentProcessor integrates with Stripe API for payment processing",
	}
	var ids []string
	for _, c := range contents {
		id, err := st.InsertMemory(Memory{
			Tier:    TierProject,
			Content: c,
			AgentID: "test-agent",
		})
		if err != nil {
			t.Fatalf("InsertMemory(%q): %v", c, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestUpsertMemoryEmbedding_StoresCount(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	if err := st.UpsertMemoryEmbedding(memID, "all-MiniLM-L6-v2", []float32{0.1, 0.2, 0.3}); err != nil {
		t.Fatalf("UpsertMemoryEmbedding: %v", err)
	}
	if count := testMemoryEmbeddingCount(st); count != 1 {
		t.Errorf("expected 1 embedding, got %d", count)
	}
}

func TestUpsertMemoryEmbedding_Idempotent(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	_ = st.UpsertMemoryEmbedding(memID, "model-a", []float32{1, 0, 0})
	_ = st.UpsertMemoryEmbedding(memID, "model-b", []float32{0, 1, 0})

	if count := testMemoryEmbeddingCount(st); count != 1 {
		t.Errorf("expected 1 after upsert (not 2), got %d", count)
	}
}

func TestUpsertMemoryEmbedding_ClearsStaleFlag(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	// Insert embedding, then mark stale, then upsert again.
	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{1, 0})
	_ = st.MarkMemoryEmbeddingsStale([]string{memID})

	// Verify it's stale.
	var stale int
	_ = st.knowledgeDB.QueryRow(`SELECT stale FROM memory_embeddings WHERE memory_id = ?`, memID).Scan(&stale)
	if stale != 1 {
		t.Fatalf("expected stale=1, got %d", stale)
	}

	// Re-upsert should clear stale.
	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{0, 1})
	_ = st.knowledgeDB.QueryRow(`SELECT stale FROM memory_embeddings WHERE memory_id = ?`, memID).Scan(&stale)
	if stale != 0 {
		t.Errorf("expected stale=0 after re-upsert, got %d", stale)
	}
}

func TestUpsertMemoryEmbedding_EmptyID_ReturnsError(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	err := st.UpsertMemoryEmbedding("", "test", []float32{1, 0})
	if err == nil {
		t.Error("expected error for empty memory_id, got nil")
	}
}

func TestUpsertMemoryEmbedding_EmptyVec_ReturnsError(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)
	err := st.UpsertMemoryEmbedding(memID, "test", nil)
	if err == nil {
		t.Error("expected error for empty vector, got nil")
	}
}

func TestMemoryEmbedding_EmptyStoreHasZeroRows(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	if count := testMemoryEmbeddingCount(st); count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestGetMemoriesWithoutEmbeddings_ReturnsUnembedded(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	// Embed only the first two.
	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0, 1, 0})

	missing, err := st.GetMemoriesWithoutEmbeddings(100)
	if err != nil {
		t.Fatalf("GetMemoriesWithoutEmbeddings: %v", err)
	}
	// Should include ids[2], ids[3], plus the original memory from openMemEmbedTestStore.
	if len(missing) < 2 {
		t.Errorf("expected at least 2 unembedded memories, got %d", len(missing))
	}
	// Verify embedded ones are NOT in the list.
	for _, m := range missing {
		if m == ids[0] || m == ids[1] {
			t.Errorf("embedded memory %s should not appear in missing list", m)
		}
	}
}

func TestGetMemoriesWithoutEmbeddings_DetectsStaleHash(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	// Embed the memory.
	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{1, 0, 0})

	// Verify it's NOT in the missing list.
	missing, _ := st.GetMemoriesWithoutEmbeddings(100)
	for _, m := range missing {
		if m == memID {
			t.Fatal("freshly embedded memory should not be in missing list")
		}
	}

	// Now change the memory content directly (simulating a memory update).
	_, _ = st.knowledgeDB.Exec(`UPDATE memories SET content = ? WHERE id = ?`,
		"AuthService now uses OAuth2 with PKCE flow instead of JWT", memID)

	// Should now appear as needing re-embedding.
	missing, err := st.GetMemoriesWithoutEmbeddings(100)
	if err != nil {
		t.Fatalf("GetMemoriesWithoutEmbeddings: %v", err)
	}
	found := false
	for _, m := range missing {
		if m == memID {
			found = true
			break
		}
	}
	if !found {
		t.Error("memory with changed content should appear in missing list")
	}
}

func TestGetMemoriesWithoutEmbeddings_LimitRespected(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	_ = seedMultipleMemories(t, st)

	missing, err := st.GetMemoriesWithoutEmbeddings(2)
	if err != nil {
		t.Fatalf("GetMemoriesWithoutEmbeddings: %v", err)
	}
	if len(missing) > 2 {
		t.Errorf("expected at most 2, got %d", len(missing))
	}
}

func TestGetMemoryTextForEmbedding_ReturnsContent(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	text, ok := st.GetMemoryTextForEmbedding(memID)
	if !ok {
		t.Fatal("expected ok=true, got false")
	}
	if text == "" {
		t.Error("expected non-empty text")
	}
}

func TestGetMemoryTextForEmbedding_NonExistent_ReturnsFalse(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	_, ok := st.GetMemoryTextForEmbedding("nonexistent-id")
	if ok {
		t.Error("expected ok=false for nonexistent memory")
	}
}

func TestMarkMemoryEmbeddingsStale_SetsFlag(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{1, 0, 0})
	if err := st.MarkMemoryEmbeddingsStale([]string{memID}); err != nil {
		t.Fatalf("MarkMemoryEmbeddingsStale: %v", err)
	}

	var stale int
	_ = st.knowledgeDB.QueryRow(`SELECT stale FROM memory_embeddings WHERE memory_id = ?`, memID).Scan(&stale)
	if stale != 1 {
		t.Errorf("expected stale=1, got %d", stale)
	}
}

func TestMarkMemoryEmbeddingsStale_EmptyIDs_Noop(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	err := st.MarkMemoryEmbeddingsStale(nil)
	if err != nil {
		t.Errorf("expected nil error for empty IDs, got: %v", err)
	}
}

func TestMarkMemoryEmbeddingsStale_Idempotent(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)
	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{1, 0})

	_ = st.MarkMemoryEmbeddingsStale([]string{memID})
	_ = st.MarkMemoryEmbeddingsStale([]string{memID}) // second call should not error
	var stale int
	_ = st.knowledgeDB.QueryRow(`SELECT stale FROM memory_embeddings WHERE memory_id = ?`, memID).Scan(&stale)
	if stale != 1 {
		t.Errorf("expected stale=1, got %d", stale)
	}
}

func TestDeleteMemoryEmbeddings_RemovesRows(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)
	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{1, 0})

	if count := testMemoryEmbeddingCount(st); count != 1 {
		t.Fatalf("expected 1 before delete, got %d", count)
	}

	if err := testDeleteMemoryEmbeddings(st,[]string{memID}); err != nil {
		t.Fatalf("DeleteMemoryEmbeddings: %v", err)
	}
	if count := testMemoryEmbeddingCount(st); count != 0 {
		t.Errorf("expected 0 after delete, got %d", count)
	}
}

func TestDeleteMemoryEmbeddings_EmptyIDs_Noop(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	err := testDeleteMemoryEmbeddings(st,nil)
	if err != nil {
		t.Errorf("expected nil error for empty IDs, got: %v", err)
	}
}

func TestMemoryVectorSearch_ReturnsMostSimilar(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	// Embed with distinct directions.
	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0, 0, 0})   // auth
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0, 1, 0, 0})   // cache
	_ = st.UpsertMemoryEmbedding(ids[2], "test", []float32{0, 0, 1, 0})   // database
	_ = st.UpsertMemoryEmbedding(ids[3], "test", []float32{0, 0, 0, 1})   // payment

	// Query closest to auth.
	results, err := st.MemoryVectorSearchWithThreshold([]float32{0.9, 0.1, 0, 0}, 5, 0.0)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results, got none")
	}
	if results[0].MemoryID != ids[0] {
		t.Errorf("expected auth memory first, got %s", results[0].MemoryID)
	}
	if results[0].Score <= 0 {
		t.Errorf("expected positive score, got %f", results[0].Score)
	}
}

func TestMemoryVectorSearch_IncludesStaleEmbeddings_WithFlag(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0.9, 0.1})

	// Mark first as stale (anchored entity changed).
	_ = st.MarkMemoryEmbeddingsStale([]string{ids[0]})

	results, err := st.MemoryVectorSearchWithThreshold([]float32{1, 0}, 5, 0.0)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	// Stale embedding should be INCLUDED (Sprint 10.7 redesign) with flag set.
	found := false
	for _, r := range results {
		if r.MemoryID == ids[0] {
			found = true
			if !r.StaleEmbedding {
				t.Error("stale embedding should have StaleEmbedding=true")
			}
		}
		if r.MemoryID == ids[1] {
			if r.StaleEmbedding {
				t.Error("fresh embedding should have StaleEmbedding=false")
			}
		}
	}
	if !found {
		t.Error("stale embedding should appear in search results with StaleEmbedding flag")
	}
}

func TestMemoryVectorSearchWithThreshold_StaleEmbedding_FlagSet(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0.9, 0.1})

	_ = st.MarkMemoryEmbeddingsStale([]string{ids[0]})

	results, err := st.MemoryVectorSearchWithThreshold([]float32{1, 0}, 5, 0.3)
	if err != nil {
		t.Fatalf("MemoryVectorSearchWithThreshold: %v", err)
	}
	found := false
	for _, r := range results {
		if r.MemoryID == ids[0] {
			found = true
			if !r.StaleEmbedding {
				t.Error("stale embedding should have StaleEmbedding=true in threshold search")
			}
		}
	}
	if !found {
		t.Error("stale embedding should appear in threshold search results")
	}
}

func TestMemoryVectorSearch_ExcludesExpiredMemories(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0.9, 0.1})

	// Expire the first memory.
	past := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	_, _ = st.knowledgeDB.Exec(`UPDATE memories SET expires_at = ? WHERE id = ?`, past, ids[0])

	results, err := st.MemoryVectorSearchWithThreshold([]float32{1, 0}, 5, 0.0)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	for _, r := range results {
		if r.MemoryID == ids[0] {
			t.Error("expired memory should not appear in search results")
		}
	}
}

func TestMemoryVectorSearch_ExcludesStaleMemories(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0.9, 0.1})

	// Mark the memory itself as stale (not just the embedding).
	_, _ = st.knowledgeDB.Exec(`UPDATE memories SET stale = 1 WHERE id = ?`, ids[0])

	results, err := st.MemoryVectorSearchWithThreshold([]float32{1, 0}, 5, 0.0)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	for _, r := range results {
		if r.MemoryID == ids[0] {
			t.Error("stale memory should not appear in search results")
		}
	}
}

func TestMemoryVectorSearch_EmptyStore_ReturnsNil(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	results, err := st.MemoryVectorSearchWithThreshold([]float32{1, 0, 0}, 5, 0.0)
	if err != nil {
		t.Fatalf("MemoryVectorSearch on empty store: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty, got %d results", len(results))
	}
}

func TestMemoryVectorSearch_EmptyQuery_ReturnsNil(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	results, err := st.MemoryVectorSearchWithThreshold(nil, 5, 0.0)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results, got %v", results)
	}
}

func TestMemoryVectorSearch_LimitRespected(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	for i, id := range ids {
		vec := make([]float32, 4)
		vec[i] = 1
		_ = st.UpsertMemoryEmbedding(id, "test", vec)
	}

	results, err := st.MemoryVectorSearchWithThreshold([]float32{0.5, 0.5, 0.5, 0.5}, 2, 0.0)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	if len(results) > 2 {
		t.Errorf("expected at most 2, got %d", len(results))
	}
}

func TestMemoryVectorSearchWithThreshold_FiltersLowScores(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	// One very similar, one orthogonal.
	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0, 1})

	// High threshold — only very similar should pass.
	results, err := st.MemoryVectorSearchWithThreshold([]float32{1, 0}, 10, 0.9)
	if err != nil {
		t.Fatalf("MemoryVectorSearchWithThreshold: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result above threshold, got %d", len(results))
	}
	if len(results) > 0 && results[0].MemoryID != ids[0] {
		t.Errorf("expected auth memory, got %s", results[0].MemoryID)
	}
}

func TestMemoryEmbeddingDimensions_EmptyStore(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	dim := st.memoryEmbeddingDimensions()
	if dim != 0 {
		t.Errorf("expected 0 dimensions for empty store, got %d", dim)
	}
}

func TestMemoryEmbeddingDimensions_Returns384(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	vec := make([]float32, 384)
	vec[0] = 1.0
	_ = st.UpsertMemoryEmbedding(memID, "test", vec)

	dim := st.memoryEmbeddingDimensions()
	if dim != 384 {
		t.Errorf("expected 384, got %d", dim)
	}
}

func TestExpireMemories_CleansEmbeddings(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{1, 0})
	if count := testMemoryEmbeddingCount(st); count != 1 {
		t.Fatalf("expected 1 embedding, got %d", count)
	}

	// Expire the memory.
	past := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	_, _ = st.knowledgeDB.Exec(`UPDATE memories SET expires_at = ? WHERE id = ?`, past, memID)

	n, err := st.ExpireMemories()
	if err != nil {
		t.Fatalf("ExpireMemories: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 expired, got %d", n)
	}

	// Embedding should be cleaned up.
	if count := testMemoryEmbeddingCount(st); count != 0 {
		t.Errorf("expected 0 embeddings after expire, got %d", count)
	}
}

func TestMemoryVectorSearch_DimensionMismatch_ReturnsNoResults(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	// Store 4-dim embeddings.
	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0, 0, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0, 1, 0, 0})

	// Query with 2-dim vector — dimension mismatch.
	results, err := st.MemoryVectorSearchWithThreshold([]float32{1, 0}, 5, 0.01)
	if err != nil {
		t.Fatalf("MemoryVectorSearch with dim mismatch: %v", err)
	}
	// cosineSimilarity returns 0 for mismatched dims → all filtered by score <= 0.
	if len(results) != 0 {
		t.Errorf("expected 0 results for dimension mismatch, got %d", len(results))
	}
}

func TestMemoryVectorSearchWithThreshold_ScansAllCandidates(t *testing.T) {
	t.Parallel()
	// Regression test: WithThreshold must examine ALL embeddings, not just top-N.
	// If the old limit*2 hack were used, this would fail by missing valid results.
	st, _ := openMemEmbedTestStore(t)

	// Create 8 memories with maximally distinct content to avoid dedup.
	// Each describes a completely different domain to keep Jaccard similarity < 0.85.
	distinctContents := []string{
		"Kubernetes pod autoscaler watches CPU utilization thresholds on deployments",
		"PostgreSQL vacuum analyzes table bloat and reclaims dead tuple storage pages",
		"Redis sentinel monitors primary replica failover with quorum voting protocol",
		"Terraform provider lifecycle manages resource creation drift detection reconciliation",
		"GraphQL resolver batching aggregates DataLoader queries into single database call",
		"Prometheus alertmanager routes firing alerts through inhibition silencing grouping",
		"Elasticsearch shard allocation rebalances replicas across cluster data nodes",
		"RabbitMQ consumer prefetch acknowledgement ensures exactly once message delivery",
	}

	var allIDs []string
	for _, c := range distinctContents {
		id, err := st.InsertMemory(Memory{
			Tier:    TierProject,
			Content: c,
			AgentID: "test-agent",
		})
		if err != nil {
			t.Fatalf("InsertMemory: %v", err)
		}
		allIDs = append(allIDs, id)
		// All embeddings point mostly in the same direction with slight variation.
		vec := []float32{1.0, float32(len(allIDs)) * 0.01, 0, 0}
		_ = st.UpsertMemoryEmbedding(id, "test", vec)
	}

	// Query with limit=3, threshold=0.9. All 8 should have score > 0.9 (similar direction).
	results, err := st.MemoryVectorSearchWithThreshold([]float32{1, 0, 0, 0}, 3, 0.9)
	if err != nil {
		t.Fatalf("MemoryVectorSearchWithThreshold: %v", err)
	}
	// Must return exactly 3 (the limit), not fewer due to truncation bug.
	if len(results) != 3 {
		t.Errorf("expected 3 results (limit), got %d", len(results))
	}
	// All scores should be above threshold.
	for _, r := range results {
		if r.Score < 0.9 {
			t.Errorf("result score %f below threshold 0.9", r.Score)
		}
	}
}

func TestMarkMemoryEmbeddingsStale_LargeBatch(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)

	// Insert 550 rows directly into memories + memory_embeddings to avoid dedup.
	// This bypasses InsertMemory's similarity check, which is correct because
	// we're testing the embedding batch logic, not memory insertion.
	var ids []string
	now := time.Now().UTC()
	expires := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)
	nowStr := now.Format(time.RFC3339)
	for i := 0; i < 550; i++ {
		id := fmt.Sprintf("batch-stale-%d", i)
		ids = append(ids, id)
		st.knowledgeDB.Exec(`INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags,
			created_at, expires_at, last_accessed_at, source)
			VALUES (?, 'project', ?, '', 'test', '', '[]', ?, ?, ?, 'manual')`,
			id, fmt.Sprintf("Unique content for stale batch test item number %d with extra words to be distinct", i),
			nowStr, expires, nowStr)
		st.knowledgeDB.Exec(`INSERT INTO memory_embeddings (memory_id, model, embedding, content_hash, stale, embedded_at)
			VALUES (?, 'test', ?, 'hash', 0, ?)`,
			id, vecToBlob([]float32{1, 0}), now.Unix())
	}
	if count := testMemoryEmbeddingCount(st); count != 550 {
		t.Fatalf("expected 550 embeddings, got %d", count)
	}

	// Mark all stale — this crosses the 500-item batch boundary.
	if err := st.MarkMemoryEmbeddingsStale(ids); err != nil {
		t.Fatalf("MarkMemoryEmbeddingsStale large batch: %v", err)
	}

	// Verify all are stale.
	var staleCount int
	_ = st.knowledgeDB.QueryRow(`SELECT COUNT(*) FROM memory_embeddings WHERE stale = 1`).Scan(&staleCount)
	if staleCount != 550 {
		t.Errorf("expected 550 stale, got %d", staleCount)
	}
}

func TestDeleteMemoryEmbeddings_LargeBatch(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)

	// Insert 550 rows directly to avoid dedup (same approach as stale batch test).
	var ids []string
	now := time.Now().UTC()
	expires := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)
	nowStr := now.Format(time.RFC3339)
	for i := 0; i < 550; i++ {
		id := fmt.Sprintf("batch-delete-%d", i)
		ids = append(ids, id)
		st.knowledgeDB.Exec(`INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags,
			created_at, expires_at, last_accessed_at, source)
			VALUES (?, 'project', ?, '', 'test', '', '[]', ?, ?, ?, 'manual')`,
			id, fmt.Sprintf("Unique content for delete batch test item number %d with additional unique words", i),
			nowStr, expires, nowStr)
		st.knowledgeDB.Exec(`INSERT INTO memory_embeddings (memory_id, model, embedding, content_hash, stale, embedded_at)
			VALUES (?, 'test', ?, 'hash', 0, ?)`,
			id, vecToBlob([]float32{0, 1}), now.Unix())
	}
	if count := testMemoryEmbeddingCount(st); count != 550 {
		t.Fatalf("expected 550 embeddings, got %d", count)
	}

	if err := testDeleteMemoryEmbeddings(st,ids); err != nil {
		t.Fatalf("DeleteMemoryEmbeddings large batch: %v", err)
	}
	if count := testMemoryEmbeddingCount(st); count != 0 {
		t.Errorf("expected 0 after large delete, got %d", count)
	}
}

func TestUpsertMemoryEmbedding_NonExistentMemory_NoOrphanInSearch(t *testing.T) {
	t.Parallel()
	// Even if an embedding is created for a nonexistent memory_id (shouldn't happen
	// in practice), MemoryVectorSearch filters it out via JOIN with memories table.
	st, _ := openMemEmbedTestStore(t)

	// Force-insert an orphan embedding (bypassing validation).
	_, _ = st.knowledgeDB.Exec(`
		INSERT INTO memory_embeddings (memory_id, model, embedding, content_hash, stale, embedded_at)
		VALUES (?, ?, ?, ?, 0, ?)`,
		"nonexistent-memory", "test", vecToBlob([]float32{1, 0}), "deadbeef", time.Now().Unix())

	if count := testMemoryEmbeddingCount(st); count != 1 {
		t.Fatalf("expected 1 orphan embedding, got %d", count)
	}

	// Search should return nothing — orphan is filtered by JOIN.
	results, err := st.MemoryVectorSearchWithThreshold([]float32{1, 0}, 5, 0.0)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("orphan embedding should not appear in search results, got %d results", len(results))
	}
}

func TestMemoryVectorSearch_LargeDataset_NoCap(t *testing.T) {
	t.Parallel()
	// Verify that all embeddings are searched without any truncation cap.
	// The two-pass min-heap approach searches ALL rows with bounded memory.
	st, _ := openMemEmbedTestStore(t)

	const totalRows = 500 // large enough to exceed any legacy cap thinking
	now := time.Now().UTC()
	expires := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)
	nowStr := now.Format(time.RFC3339)

	tx, err := st.knowledgeDB.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	memStmt, err := tx.Prepare(`INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags,
		created_at, expires_at, last_accessed_at, source)
		VALUES (?, 'project', ?, '', 'test', '', '[]', ?, ?, ?, 'manual')`)
	if err != nil {
		t.Fatalf("prepare mem stmt: %v", err)
	}
	embStmt, err := tx.Prepare(`INSERT INTO memory_embeddings (memory_id, model, embedding, content_hash, stale, embedded_at)
		VALUES (?, 'test', ?, 'hash', 0, ?)`)
	if err != nil {
		t.Fatalf("prepare emb stmt: %v", err)
	}

	// Insert totalRows embeddings. The LAST one has the best score.
	for i := 0; i < totalRows; i++ {
		id := fmt.Sprintf("scale-test-%d", i)
		if _, err := memStmt.Exec(id, fmt.Sprintf("scale test content %d", i), nowStr, expires, nowStr); err != nil {
			t.Fatalf("insert memory %d: %v", i, err)
		}
		// All point roughly in {1, 0} direction, but the last one is closest.
		// Vectors are pre-normalized (unit length) as required by dotSimilarity.
		vec := normalizeVec([]float32{1.0, float32(totalRows-i) * 0.001})
		if _, err := embStmt.Exec(id, vecToBlob(vec), now.Unix()); err != nil {
			t.Fatalf("insert embedding %d: %v", i, err)
		}
	}
	memStmt.Close()
	embStmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Search — the best match (closest to {1,0}) should be the last inserted.
	results, err := st.MemoryVectorSearchWithThreshold([]float32{1, 0}, 5, 0.0)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}
	// The top result should be the last row (smallest offset from {1,0}).
	if results[0].MemoryID != fmt.Sprintf("scale-test-%d", totalRows-1) {
		t.Errorf("expected last-inserted memory as top result, got %s", results[0].MemoryID)
	}
	// Verify content was fetched correctly (two-pass: content comes from pass 2).
	if results[0].Content == "" {
		t.Error("expected non-empty content from second pass fetch")
	}
}

func TestMemoryVectorSearchWithThreshold_LargeDataset_NoCap(t *testing.T) {
	t.Parallel()
	// Same verification for threshold variant — all rows searched, no truncation.
	st, _ := openMemEmbedTestStore(t)

	const totalRows = 500
	now := time.Now().UTC()
	expires := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)
	nowStr := now.Format(time.RFC3339)

	tx, err := st.knowledgeDB.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	memStmt, err := tx.Prepare(`INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags,
		created_at, expires_at, last_accessed_at, source)
		VALUES (?, 'project', ?, '', 'test', '', '[]', ?, ?, ?, 'manual')`)
	if err != nil {
		t.Fatalf("prepare mem stmt: %v", err)
	}
	embStmt, err := tx.Prepare(`INSERT INTO memory_embeddings (memory_id, model, embedding, content_hash, stale, embedded_at)
		VALUES (?, 'test', ?, 'hash', 0, ?)`)
	if err != nil {
		t.Fatalf("prepare emb stmt: %v", err)
	}
	for i := 0; i < totalRows; i++ {
		id := fmt.Sprintf("scale-thresh-%d", i)
		if _, err := memStmt.Exec(id, fmt.Sprintf("scale threshold test content %d", i), nowStr, expires, nowStr); err != nil {
			t.Fatalf("insert memory %d: %v", i, err)
		}
		if _, err := embStmt.Exec(id, vecToBlob([]float32{1, 0}), now.Unix()); err != nil {
			t.Fatalf("insert embedding %d: %v", i, err)
		}
	}
	memStmt.Close()
	embStmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	results, err := st.MemoryVectorSearchWithThreshold([]float32{1, 0}, 5, 0.5)
	if err != nil {
		t.Fatalf("MemoryVectorSearchWithThreshold: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}
	// All scores should be above threshold.
	for _, r := range results {
		if r.Score < 0.5 {
			t.Errorf("result score %f below threshold 0.5", r.Score)
		}
	}
	// Verify content was fetched (two-pass correctness).
	if results[0].Content == "" {
		t.Error("expected non-empty content from second pass fetch")
	}
}

// contains is a simple helper for substring checks in test assertions.
func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func TestMemoryContentHash_Deterministic(t *testing.T) {
	t.Parallel()
	h1 := memoryContentHash("AuthService uses JWT tokens")
	h2 := memoryContentHash("AuthService uses JWT tokens")
	if h1 != h2 {
		t.Errorf("hash should be deterministic: %s != %s", h1, h2)
	}
}

func TestMemoryContentHash_DifferentContent(t *testing.T) {
	t.Parallel()
	h1 := memoryContentHash("AuthService uses JWT tokens")
	h2 := memoryContentHash("AuthService uses OAuth2 tokens")
	if h1 == h2 {
		t.Error("different content should produce different hashes")
	}
}

// ── GetMemoryIDsByAnchorNodes tests ────────────────────────────────────────

func TestGetMemoryIDsByAnchorNodes_EmptyNodeIDs(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids, err := st.GetMemoryIDsByAnchorNodes(nil, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty result for nil nodeIDs, got %d", len(ids))
	}
}

func TestGetMemoryIDsByAnchorNodes_ZeroLimit(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids, err := st.GetMemoryIDsByAnchorNodes([]string{"node-1"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty result for limit=0, got %d", len(ids))
	}
}

func TestGetMemoryIDsByAnchorNodes_ReturnsAnchoredMemories(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)

	// Insert a memory anchored to node-a.
	memID, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "AuthService caches tokens in Redis with 15-minute TTL",
		AgentID: "test",
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}
	if err := st.InsertMemoryAnchors(memID, []string{"node-a"}); err != nil {
		t.Fatalf("InsertMemoryAnchors: %v", err)
	}

	ids, err := st.GetMemoryIDsByAnchorNodes([]string{"node-a"}, 100)
	if err != nil {
		t.Fatalf("GetMemoryIDsByAnchorNodes: %v", err)
	}
	if len(ids) != 1 || ids[0] != memID {
		t.Errorf("expected [%s], got %v", memID, ids)
	}
}

func TestGetMemoryIDsByAnchorNodes_StaleMemoryExcluded(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)

	memID, _ := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "Stale auth memory that should not appear in embedding invalidation",
		AgentID: "test",
	})
	_ = st.InsertMemoryAnchors(memID, []string{"node-stale"})
	// Mark the memory itself stale directly (simulates anchor-node removal).
	st.knowledgeDB.Exec(`UPDATE memories SET stale = 1 WHERE id = ?`, memID)

	ids, err := st.GetMemoryIDsByAnchorNodes([]string{"node-stale"}, 100)
	if err != nil {
		t.Fatalf("GetMemoryIDsByAnchorNodes: %v", err)
	}
	for _, id := range ids {
		if id == memID {
			t.Error("stale memory should not be returned for embedding invalidation")
		}
	}
}

func TestGetMemoryIDsByAnchorNodes_UnknownNodeReturnsEmpty(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids, err := st.GetMemoryIDsByAnchorNodes([]string{"no-such-node"}, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty result for unknown node, got %v", ids)
	}
}

// ── GetStaleEmbeddingMemoryIDs tests ──────────────────────────────────────

func TestGetStaleEmbeddingMemoryIDs_EmptyStore(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids, err := testGetStaleEmbeddingMemoryIDs(st,50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty result on store with no stale embeddings, got %d", len(ids))
	}
}

func TestGetStaleEmbeddingMemoryIDs_ReturnsStaleOnly(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)

	// Insert two memories, embed both, mark one stale.
	freshID, _ := st.InsertMemory(Memory{Tier: TierProject, Content: "Fresh embedding content", AgentID: "t"})
	staleID, _ := st.InsertMemory(Memory{Tier: TierProject, Content: "Stale embedding content — entity changed", AgentID: "t"})

	_ = st.UpsertMemoryEmbedding(freshID, "test", []float32{1, 0})
	_ = st.UpsertMemoryEmbedding(staleID, "test", []float32{0, 1})
	_ = st.MarkMemoryEmbeddingsStale([]string{staleID})

	ids, err := testGetStaleEmbeddingMemoryIDs(st,50)
	if err != nil {
		t.Fatalf("GetStaleEmbeddingMemoryIDs: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == freshID {
			t.Errorf("fresh embedding should not be in stale list")
		}
		if id == staleID {
			found = true
		}
	}
	if !found {
		t.Errorf("stale embedding %s not found in results", staleID)
	}
}

func TestGetStaleEmbeddingMemoryIDs_StaleMemoryExcluded(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)

	// Memory is stale (e.g., anchor removed) AND has a stale embedding.
	// GetStaleEmbeddingMemoryIDs should NOT return it — there's no point
	// re-embedding a memory that is already dead.
	memID, _ := st.InsertMemory(Memory{Tier: TierEntity, Content: "Dead entity memory", AgentID: "t"})
	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{1, 0})
	_ = st.MarkMemoryEmbeddingsStale([]string{memID})
	// Mark the memory record itself stale (anchor removed) — no point re-embedding it.
	st.knowledgeDB.Exec(`UPDATE memories SET stale = 1 WHERE id = ?`, memID)

	ids, err := testGetStaleEmbeddingMemoryIDs(st,50)
	if err != nil {
		t.Fatalf("GetStaleEmbeddingMemoryIDs: %v", err)
	}
	for _, id := range ids {
		if id == memID {
			t.Error("dead (stale memory) should not appear in stale embeddings list")
		}
	}
}

func TestGetStaleEmbeddingMemoryIDs_LimitRespected(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)

	// Insert 20 memories, embed all, mark all stale.
	now := time.Now().UTC()
	expires := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)
	nowStr := now.Format(time.RFC3339)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("stale-embed-limit-%d", i)
		st.knowledgeDB.Exec(`INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags,
			created_at, expires_at, last_accessed_at, source)
			VALUES (?, 'project', ?, '', 'test', '', '[]', ?, ?, ?, 'manual')`,
			id, fmt.Sprintf("Embedding limit test content item %d for stale detection", i),
			nowStr, expires, nowStr)
		st.knowledgeDB.Exec(`INSERT INTO memory_embeddings (memory_id, model, embedding, content_hash, stale, embedded_at)
			VALUES (?, 'test', ?, 'hash', 1, ?)`,
			id, vecToBlob([]float32{float32(i), 0}), now.Unix())
	}

	ids, err := testGetStaleEmbeddingMemoryIDs(st,5)
	if err != nil {
		t.Fatalf("GetStaleEmbeddingMemoryIDs: %v", err)
	}
	if len(ids) != 5 {
		t.Errorf("expected exactly 5 results with limit=5, got %d", len(ids))
	}
}

// ── InvalidateEmbeddingsByModel (model upgrade migration) ──────────────────

func TestInvalidateEmbeddingsByModel_MarksOldModelStale(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	// Embed with old model.
	_ = st.UpsertMemoryEmbedding(memID, "all-MiniLM-L6-v2", []float32{1, 0, 0})

	// Add a second memory with old model.
	ids := seedMultipleMemories(t, st)
	_ = st.UpsertMemoryEmbedding(ids[0], "all-MiniLM-L6-v2", []float32{0, 1, 0})

	// Invalidate: switch to nomic.
	n, err := st.InvalidateEmbeddingsByModel("nomic-embed-text-v1.5")
	if err != nil {
		t.Fatalf("InvalidateEmbeddingsByModel: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 invalidated, got %d", n)
	}

	// Both should now appear in GetMemoriesWithoutEmbeddings (stale=1 picked up).
	missing, err := st.GetMemoriesWithoutEmbeddings(0)
	if err != nil {
		t.Fatalf("GetMemoriesWithoutEmbeddings: %v", err)
	}
	foundOrig, foundSeed := false, false
	for _, m := range missing {
		if m == memID {
			foundOrig = true
		}
		if m == ids[0] {
			foundSeed = true
		}
	}
	if !foundOrig {
		t.Error("original memory should appear in missing list after model invalidation")
	}
	if !foundSeed {
		t.Error("seeded memory should appear in missing list after model invalidation")
	}
}

func TestInvalidateEmbeddingsByModel_SkipsCurrentModel(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	// Embed with current model.
	_ = st.UpsertMemoryEmbedding(memID, "nomic-embed-text-v1.5", []float32{1, 0, 0})

	// Invalidate with same model — should be a no-op.
	n, err := st.InvalidateEmbeddingsByModel("nomic-embed-text-v1.5")
	if err != nil {
		t.Fatalf("InvalidateEmbeddingsByModel: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 invalidated (same model), got %d", n)
	}
}

func TestInvalidateEmbeddingsByModel_SkipsAlreadyStale(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	// Embed with old model and mark stale manually.
	_ = st.UpsertMemoryEmbedding(memID, "all-MiniLM-L6-v2", []float32{1, 0, 0})
	_ = st.MarkMemoryEmbeddingsStale([]string{memID})

	// Invalidate — already stale, should not count again.
	n, err := st.InvalidateEmbeddingsByModel("nomic-embed-text-v1.5")
	if err != nil {
		t.Fatalf("InvalidateEmbeddingsByModel: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 (already stale), got %d", n)
	}
}

func TestInvalidateEmbeddingsByModel_EmptyModelNoOp(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)
	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{1, 0, 0})

	n, err := st.InvalidateEmbeddingsByModel("")
	if err != nil {
		t.Fatalf("InvalidateEmbeddingsByModel: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 for empty model, got %d", n)
	}
}

func TestInvalidateEmbeddingsByModel_MixedModels(t *testing.T) {
	t.Parallel()
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	// Two with old model, two with new model.
	_ = st.UpsertMemoryEmbedding(ids[0], "all-MiniLM-L6-v2", []float32{1, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "all-MiniLM-L6-v2", []float32{0, 1})
	_ = st.UpsertMemoryEmbedding(ids[2], "nomic-embed-text-v1.5", []float32{1, 1})
	_ = st.UpsertMemoryEmbedding(ids[3], "nomic-embed-text-v1.5", []float32{0, 0})

	n, err := st.InvalidateEmbeddingsByModel("nomic-embed-text-v1.5")
	if err != nil {
		t.Fatalf("InvalidateEmbeddingsByModel: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 (old model only), got %d", n)
	}
}

func TestGetMemoriesWithoutEmbeddings_IncludesStaleEmbeddings(t *testing.T) {
	t.Parallel()
	st, memID := openMemEmbedTestStore(t)

	// Embed normally.
	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{1, 0, 0})

	// Verify NOT in missing list.
	missing, _ := st.GetMemoriesWithoutEmbeddings(0)
	for _, m := range missing {
		if m == memID {
			t.Fatal("freshly embedded memory should not be in missing list")
		}
	}

	// Mark stale (simulates model upgrade invalidation).
	_ = st.MarkMemoryEmbeddingsStale([]string{memID})

	// Should now appear as needing re-embedding.
	missing, err := st.GetMemoriesWithoutEmbeddings(0)
	if err != nil {
		t.Fatalf("GetMemoriesWithoutEmbeddings: %v", err)
	}
	found := false
	for _, m := range missing {
		if m == memID {
			found = true
			break
		}
	}
	if !found {
		t.Error("memory with stale embedding should appear in missing list")
	}
}

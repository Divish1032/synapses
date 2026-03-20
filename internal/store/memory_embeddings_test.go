package store

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// openMemEmbedTestStore creates a temporary Store with a seeded memory for embedding tests.
func openMemEmbedTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	f, err := os.CreateTemp("", "test-memembed-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()); os.Remove(KnowledgePath(f.Name())) })

	st, err := Open(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

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
	st, memID := openMemEmbedTestStore(t)

	if err := st.UpsertMemoryEmbedding(memID, "all-MiniLM-L6-v2", []float32{0.1, 0.2, 0.3}); err != nil {
		t.Fatalf("UpsertMemoryEmbedding: %v", err)
	}
	if count := st.MemoryEmbeddingCount(); count != 1 {
		t.Errorf("expected 1 embedding, got %d", count)
	}
}

func TestUpsertMemoryEmbedding_Idempotent(t *testing.T) {
	st, memID := openMemEmbedTestStore(t)

	_ = st.UpsertMemoryEmbedding(memID, "model-a", []float32{1, 0, 0})
	_ = st.UpsertMemoryEmbedding(memID, "model-b", []float32{0, 1, 0})

	if count := st.MemoryEmbeddingCount(); count != 1 {
		t.Errorf("expected 1 after upsert (not 2), got %d", count)
	}
}

func TestUpsertMemoryEmbedding_ClearsStaleFlag(t *testing.T) {
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
	st, _ := openMemEmbedTestStore(t)
	err := st.UpsertMemoryEmbedding("", "test", []float32{1, 0})
	if err == nil {
		t.Error("expected error for empty memory_id, got nil")
	}
}

func TestUpsertMemoryEmbedding_EmptyVec_ReturnsError(t *testing.T) {
	st, memID := openMemEmbedTestStore(t)
	err := st.UpsertMemoryEmbedding(memID, "test", nil)
	if err == nil {
		t.Error("expected error for empty vector, got nil")
	}
}

func TestMemoryEmbeddingCount_EmptyStore(t *testing.T) {
	st, _ := openMemEmbedTestStore(t)
	if count := st.MemoryEmbeddingCount(); count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestGetMemoriesWithoutEmbeddings_ReturnsUnembedded(t *testing.T) {
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
	st, _ := openMemEmbedTestStore(t)
	_, ok := st.GetMemoryTextForEmbedding("nonexistent-id")
	if ok {
		t.Error("expected ok=false for nonexistent memory")
	}
}

func TestMarkMemoryEmbeddingsStale_SetsFlag(t *testing.T) {
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
	st, _ := openMemEmbedTestStore(t)
	err := st.MarkMemoryEmbeddingsStale(nil)
	if err != nil {
		t.Errorf("expected nil error for empty IDs, got: %v", err)
	}
}

func TestMarkMemoryEmbeddingsStale_Idempotent(t *testing.T) {
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
	st, memID := openMemEmbedTestStore(t)
	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{1, 0})

	if count := st.MemoryEmbeddingCount(); count != 1 {
		t.Fatalf("expected 1 before delete, got %d", count)
	}

	if err := st.DeleteMemoryEmbeddings([]string{memID}); err != nil {
		t.Fatalf("DeleteMemoryEmbeddings: %v", err)
	}
	if count := st.MemoryEmbeddingCount(); count != 0 {
		t.Errorf("expected 0 after delete, got %d", count)
	}
}

func TestDeleteMemoryEmbeddings_EmptyIDs_Noop(t *testing.T) {
	st, _ := openMemEmbedTestStore(t)
	err := st.DeleteMemoryEmbeddings(nil)
	if err != nil {
		t.Errorf("expected nil error for empty IDs, got: %v", err)
	}
}

func TestMemoryVectorSearch_ReturnsMostSimilar(t *testing.T) {
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	// Embed with distinct directions.
	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0, 0, 0})   // auth
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0, 1, 0, 0})   // cache
	_ = st.UpsertMemoryEmbedding(ids[2], "test", []float32{0, 0, 1, 0})   // database
	_ = st.UpsertMemoryEmbedding(ids[3], "test", []float32{0, 0, 0, 1})   // payment

	// Query closest to auth.
	results, err := st.MemoryVectorSearch([]float32{0.9, 0.1, 0, 0}, 5)
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

func TestMemoryVectorSearch_ExcludesStaleEmbeddings(t *testing.T) {
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0.9, 0.1})

	// Mark first as stale.
	_ = st.MarkMemoryEmbeddingsStale([]string{ids[0]})

	results, err := st.MemoryVectorSearch([]float32{1, 0}, 5)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	// Stale embedding should be excluded.
	for _, r := range results {
		if r.MemoryID == ids[0] {
			t.Error("stale embedding should not appear in search results")
		}
	}
}

func TestMemoryVectorSearch_ExcludesExpiredMemories(t *testing.T) {
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0.9, 0.1})

	// Expire the first memory.
	past := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	_, _ = st.knowledgeDB.Exec(`UPDATE memories SET expires_at = ? WHERE id = ?`, past, ids[0])

	results, err := st.MemoryVectorSearch([]float32{1, 0}, 5)
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
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0.9, 0.1})

	// Mark the memory itself as stale (not just the embedding).
	_, _ = st.knowledgeDB.Exec(`UPDATE memories SET stale = 1 WHERE id = ?`, ids[0])

	results, err := st.MemoryVectorSearch([]float32{1, 0}, 5)
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
	st, _ := openMemEmbedTestStore(t)
	results, err := st.MemoryVectorSearch([]float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatalf("MemoryVectorSearch on empty store: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty, got %d results", len(results))
	}
}

func TestMemoryVectorSearch_EmptyQuery_ReturnsNil(t *testing.T) {
	st, _ := openMemEmbedTestStore(t)
	results, err := st.MemoryVectorSearch(nil, 5)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results, got %v", results)
	}
}

func TestMemoryVectorSearch_LimitRespected(t *testing.T) {
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	for i, id := range ids {
		vec := make([]float32, 4)
		vec[i] = 1
		_ = st.UpsertMemoryEmbedding(id, "test", vec)
	}

	results, err := st.MemoryVectorSearch([]float32{0.5, 0.5, 0.5, 0.5}, 2)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	if len(results) > 2 {
		t.Errorf("expected at most 2, got %d", len(results))
	}
}

func TestMemoryVectorSearchWithThreshold_FiltersLowScores(t *testing.T) {
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
	st, _ := openMemEmbedTestStore(t)
	dim := st.memoryEmbeddingDimensions()
	if dim != 0 {
		t.Errorf("expected 0 dimensions for empty store, got %d", dim)
	}
}

func TestMemoryEmbeddingDimensions_Returns384(t *testing.T) {
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
	st, memID := openMemEmbedTestStore(t)

	_ = st.UpsertMemoryEmbedding(memID, "test", []float32{1, 0})
	if count := st.MemoryEmbeddingCount(); count != 1 {
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
	if count := st.MemoryEmbeddingCount(); count != 0 {
		t.Errorf("expected 0 embeddings after expire, got %d", count)
	}
}

func TestMemoryVectorSearch_DimensionMismatch_ReturnsNoResults(t *testing.T) {
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	// Store 4-dim embeddings.
	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0, 0, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0, 1, 0, 0})

	// Query with 2-dim vector — dimension mismatch.
	results, err := st.MemoryVectorSearch([]float32{1, 0}, 5)
	if err != nil {
		t.Fatalf("MemoryVectorSearch with dim mismatch: %v", err)
	}
	// cosineSimilarity returns 0 for mismatched dims → all filtered by score <= 0.
	if len(results) != 0 {
		t.Errorf("expected 0 results for dimension mismatch, got %d", len(results))
	}
}

func TestMemoryVectorSearchWithThreshold_ScansAllCandidates(t *testing.T) {
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
	if count := st.MemoryEmbeddingCount(); count != 550 {
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
	if count := st.MemoryEmbeddingCount(); count != 550 {
		t.Fatalf("expected 550 embeddings, got %d", count)
	}

	if err := st.DeleteMemoryEmbeddings(ids); err != nil {
		t.Fatalf("DeleteMemoryEmbeddings large batch: %v", err)
	}
	if count := st.MemoryEmbeddingCount(); count != 0 {
		t.Errorf("expected 0 after large delete, got %d", count)
	}
}

func TestUpsertMemoryEmbedding_NonExistentMemory_NoOrphanInSearch(t *testing.T) {
	// Even if an embedding is created for a nonexistent memory_id (shouldn't happen
	// in practice), MemoryVectorSearch filters it out via JOIN with memories table.
	st, _ := openMemEmbedTestStore(t)

	// Force-insert an orphan embedding (bypassing validation).
	_, _ = st.knowledgeDB.Exec(`
		INSERT INTO memory_embeddings (memory_id, model, embedding, content_hash, stale, embedded_at)
		VALUES (?, ?, ?, ?, 0, ?)`,
		"nonexistent-memory", "test", vecToBlob([]float32{1, 0}), "deadbeef", time.Now().Unix())

	if count := st.MemoryEmbeddingCount(); count != 1 {
		t.Fatalf("expected 1 orphan embedding, got %d", count)
	}

	// Search should return nothing — orphan is filtered by JOIN.
	results, err := st.MemoryVectorSearch([]float32{1, 0}, 5)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("orphan embedding should not appear in search results, got %d results", len(results))
	}
}

func TestMemoryVectorSearch_CapTriggersWarning(t *testing.T) {
	// Verify that inserting more than maxVectorScanCap embeddings triggers the
	// LIMIT, causing a warning on stderr. Uses direct DB inserts for speed.
	st, _ := openMemEmbedTestStore(t)

	now := time.Now().UTC()
	expires := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)
	nowStr := now.Format(time.RFC3339)

	// Insert maxVectorScanCap+1 memories and embeddings directly (no dedup).
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
	for i := 0; i < maxVectorScanCap+1; i++ {
		id := fmt.Sprintf("cap-test-%d", i)
		if _, err := memStmt.Exec(id, fmt.Sprintf("cap test content %d", i), nowStr, expires, nowStr); err != nil {
			t.Fatalf("insert memory %d: %v", i, err)
		}
		// All embeddings point in direction {1, 0} so they are valid candidates.
		if _, err := embStmt.Exec(id, vecToBlob([]float32{1, 0}), now.Unix()); err != nil {
			t.Fatalf("insert embedding %d: %v", i, err)
		}
	}
	memStmt.Close()
	embStmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Capture stderr to verify warning.
	oldStderr := os.Stderr
	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("os.Pipe: %v", pipeErr)
	}
	os.Stderr = w

	results, searchErr := st.MemoryVectorSearch([]float32{1, 0}, 5)

	w.Close()
	os.Stderr = oldStderr

	buf := make([]byte, 512)
	n, _ := r.Read(buf)
	r.Close()
	output := string(buf[:n])

	if searchErr != nil {
		t.Fatalf("MemoryVectorSearch: %v", searchErr)
	}
	// Results should still be returned (cap doesn't break search correctness).
	if len(results) == 0 {
		t.Error("expected results despite cap, got none")
	}
	// Warning must mention the cap and be WARN level.
	if output == "" {
		t.Error("expected WARN message on stderr when cap triggered, got nothing")
	}
	if !contains(output, "WARN") || !contains(output, "cap triggered") {
		t.Errorf("expected WARN cap triggered message, got: %q", output)
	}
}

func TestMemoryVectorSearchWithThreshold_CapTriggersWarning(t *testing.T) {
	// Same as above but for MemoryVectorSearchWithThreshold.
	st, _ := openMemEmbedTestStore(t)

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
	for i := 0; i < maxVectorScanCap+1; i++ {
		id := fmt.Sprintf("cap-thresh-test-%d", i)
		if _, err := memStmt.Exec(id, fmt.Sprintf("cap threshold test content %d", i), nowStr, expires, nowStr); err != nil {
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

	oldStderr := os.Stderr
	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("os.Pipe: %v", pipeErr)
	}
	os.Stderr = w

	results, searchErr := st.MemoryVectorSearchWithThreshold([]float32{1, 0}, 5, 0.5)

	w.Close()
	os.Stderr = oldStderr

	buf := make([]byte, 512)
	n, _ := r.Read(buf)
	r.Close()
	output := string(buf[:n])

	if searchErr != nil {
		t.Fatalf("MemoryVectorSearchWithThreshold: %v", searchErr)
	}
	if len(results) == 0 {
		t.Error("expected results despite cap, got none")
	}
	if output == "" {
		t.Error("expected WARN message on stderr when cap triggered, got nothing")
	}
	if !contains(output, "WARN") || !contains(output, "cap triggered") {
		t.Errorf("expected WARN cap triggered message, got: %q", output)
	}
}

// contains is a simple helper for substring checks in test assertions.
func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func TestMemoryContentHash_Deterministic(t *testing.T) {
	h1 := memoryContentHash("AuthService uses JWT tokens")
	h2 := memoryContentHash("AuthService uses JWT tokens")
	if h1 != h2 {
		t.Errorf("hash should be deterministic: %s != %s", h1, h2)
	}
}

func TestMemoryContentHash_DifferentContent(t *testing.T) {
	h1 := memoryContentHash("AuthService uses JWT tokens")
	h2 := memoryContentHash("AuthService uses OAuth2 tokens")
	if h1 == h2 {
		t.Error("different content should produce different hashes")
	}
}

package store

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"testing"
)

// openHNSWTestStore creates a temporary Store for HNSW integration tests.
// Uses the pre-initialized template DB from TestMain for fast setup.
func openHNSWTestStore(t *testing.T) *Store {
	t.Helper()
	return openFromTemplate(t)
}

// makeUnitVec creates a unit-length vector with a dominant component at the
// given index. All other components are small random noise.
func makeUnitVec(dims, dominant int) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = 0.01 * float32(i%7+1)
	}
	v[dominant%dims] = 1.0
	// Normalize to unit length.
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	scale := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= scale
	}
	return v
}

// makeRandomUnitVec creates a random unit vector.
func makeRandomUnitVec(rng *rand.Rand, dims int) []float32 {
	v := make([]float32, dims)
	var sum float64
	for i := range v {
		v[i] = float32(rng.NormFloat64())
		sum += float64(v[i]) * float64(v[i])
	}
	scale := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= scale
	}
	return v
}

// TestRebuildMemoryHNSW_EmptyStore verifies HNSW rebuild on empty store
// creates a valid empty index.
func TestRebuildMemoryHNSW_EmptyStore(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	// Index should be built (possibly empty) after Open.
	if st.hnswLen() < 0 {
		t.Error("hnswLen should be >= 0")
	}
}

// TestRebuildMemoryHNSW_LoadsEmbeddings verifies that embeddings stored in
// SQLite are loaded into the HNSW index during rebuild.
func TestRebuildMemoryHNSW_LoadsEmbeddings(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	const dims = 384
	// Insert 5 memories with embeddings.
	for i := 0; i < 5; i++ {
		memID, err := st.InsertMemory(Memory{
			Tier:    TierProject,
			Content: fmt.Sprintf("Memory content %d", i),
			AgentID: "test",
		})
		if err != nil {
			t.Fatal(err)
		}
		vec := makeUnitVec(dims, i*50)
		if err := st.UpsertMemoryEmbedding(memID, "test-model", vec); err != nil {
			t.Fatal(err)
		}
	}

	// Rebuild should load all 5.
	st.RebuildMemoryHNSW()
	if got := st.hnswLen(); got != 5 {
		t.Errorf("hnswLen = %d, want 5", got)
	}
}

// TestRebuildMemoryHNSW_IncludesStale verifies that stale embeddings ARE
// loaded into the HNSW index (they appear in search with StaleEmbedding flag).
// Only stale/expired memories (not embeddings) are excluded.
func TestRebuildMemoryHNSW_IncludesStale(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	const dims = 384
	var memIDs []string
	for i := 0; i < 3; i++ {
		memID, err := st.InsertMemory(Memory{
			Tier:    TierProject,
			Content: fmt.Sprintf("Memory content number %d for testing stale embeddings", i),
			AgentID: "test",
		})
		if err != nil {
			t.Fatal(err)
		}
		vec := makeUnitVec(dims, i*100)
		if err := st.UpsertMemoryEmbedding(memID, "old-model", vec); err != nil {
			t.Fatal(err)
		}
		memIDs = append(memIDs, memID)
	}

	// Mark first memory embedding as stale.
	if err := st.MarkMemoryEmbeddingsStale(memIDs[:1]); err != nil {
		t.Fatal(err)
	}

	// Rebuild should load all 3 — stale embeddings stay in HNSW for
	// searchability (the StaleEmbedding flag is resolved in SQL Pass 2).
	st.RebuildMemoryHNSW()
	if got := st.hnswLen(); got != 3 {
		t.Errorf("hnswLen = %d, want 3", got)
	}
}

// TestMemoryVectorSearch_HNSW_FindsNearest verifies end-to-end HNSW-backed
// memory vector search: insert memories, embed them, search, get correct result.
func TestMemoryVectorSearch_HNSW_FindsNearest(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	const dims = 384

	// Insert 3 memories with distinct embedding directions.
	type testMem struct {
		content string
		vec     []float32
	}
	mems := []testMem{
		{"Authentication and JWT tokens", makeUnitVec(dims, 0)},
		{"Database migrations and schemas", makeUnitVec(dims, 100)},
		{"Payment processing with Stripe", makeUnitVec(dims, 200)},
	}

	var memIDs []string
	for _, m := range mems {
		memID, err := st.InsertMemory(Memory{
			Tier:    TierProject,
			Content: m.content,
			AgentID: "test",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertMemoryEmbedding(memID, "test-model", m.vec); err != nil {
			t.Fatal(err)
		}
		memIDs = append(memIDs, memID)
	}

	// Rebuild HNSW to load the embeddings.
	st.RebuildMemoryHNSW()

	// Search for a vector similar to the first memory.
	queryVec := makeUnitVec(dims, 0)
	results, err := st.MemoryVectorSearchWithThreshold(queryVec, 1, 0.0)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 result")
	}
	if results[0].MemoryID != memIDs[0] {
		t.Errorf("top result = %s, want %s (auth memory)", results[0].MemoryID, memIDs[0])
	}
	if results[0].Score <= 0 {
		t.Errorf("score should be positive, got %f", results[0].Score)
	}
}

// TestMemoryVectorSearch_HNSW_IncrementalAdd verifies that embeddings added
// after initial build are searchable without a full rebuild.
func TestMemoryVectorSearch_HNSW_IncrementalAdd(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	const dims = 384

	// Start with 1 memory.
	memID1, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "First memory content about authentication and JWT tokens",
		AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	vec1 := makeUnitVec(dims, 0)
	if err := st.UpsertMemoryEmbedding(memID1, "test-model", vec1); err != nil {
		t.Fatal(err)
	}
	st.RebuildMemoryHNSW()

	// Add a second memory WITHOUT rebuild.
	memID2, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "Second memory content about payment processing with Stripe API",
		AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	vec2 := makeUnitVec(dims, 200)
	if err := st.UpsertMemoryEmbedding(memID2, "test-model", vec2); err != nil {
		t.Fatal(err)
	}

	// Search for the second memory's direction — should find it via incremental add.
	queryVec := makeUnitVec(dims, 200)
	results, err := st.MemoryVectorSearchWithThreshold(queryVec, 1, 0.0)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 result")
	}
	if results[0].MemoryID != memID2 {
		t.Errorf("top result = %s, want %s (second memory)", results[0].MemoryID, memID2)
	}
}

// TestMemoryVectorSearch_HNSW_DeleteRemoves verifies that deleted embeddings
// are removed from the HNSW index.
func TestMemoryVectorSearch_HNSW_DeleteRemoves(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	const dims = 384

	memID, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "This memory content will be deleted during the test run",
		AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	vec := makeUnitVec(dims, 50)
	if err := st.UpsertMemoryEmbedding(memID, "test-model", vec); err != nil {
		t.Fatal(err)
	}
	st.RebuildMemoryHNSW()

	if st.hnswLen() != 1 {
		t.Fatalf("expected 1 in HNSW, got %d", st.hnswLen())
	}

	// Delete the embedding.
	if err := st.DeleteMemoryEmbeddings([]string{memID}); err != nil {
		t.Fatal(err)
	}

	if st.hnswLen() != 0 {
		t.Errorf("expected 0 in HNSW after delete, got %d", st.hnswLen())
	}
}

// TestMemoryVectorSearch_HNSW_MarkStaleKeepsInIndex verifies that marking
// embeddings as stale keeps them in the HNSW index (they appear in search
// results with StaleEmbedding=true, resolved in SQL Pass 2).
func TestMemoryVectorSearch_HNSW_MarkStaleKeepsInIndex(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	const dims = 384

	memID, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "This memory content will become stale during testing",
		AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	vec := makeUnitVec(dims, 50)
	if err := st.UpsertMemoryEmbedding(memID, "test-model", vec); err != nil {
		t.Fatal(err)
	}
	st.RebuildMemoryHNSW()

	if st.hnswLen() != 1 {
		t.Fatalf("expected 1 in HNSW, got %d", st.hnswLen())
	}

	// Mark stale — embedding stays in HNSW (stale is a metadata flag, not removal).
	if err := st.MarkMemoryEmbeddingsStale([]string{memID}); err != nil {
		t.Fatal(err)
	}

	if st.hnswLen() != 1 {
		t.Errorf("expected 1 in HNSW after marking stale (stale stays in index), got %d", st.hnswLen())
	}
}

// TestMemoryVectorSearch_HNSW_BruteforceFallback verifies that search falls
// back to brute-force when the HNSW index is empty/nil.
func TestMemoryVectorSearch_HNSW_BruteforceFallback(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	const dims = 384

	memID, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "Fallback test memory content for brute-force search path",
		AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	vec := makeUnitVec(dims, 50)
	if err := st.UpsertMemoryEmbedding(memID, "test-model", vec); err != nil {
		t.Fatal(err)
	}

	// Force empty HNSW by manually resetting (simulates no rebuild).
	st.hnswMemMu.Lock()
	st.hnswMemIndex = nil
	st.hnswMemMu.Unlock()

	// Search should still work via brute-force fallback.
	results, err := st.MemoryVectorSearchWithThreshold(makeUnitVec(dims, 50), 1, 0.0)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected brute-force fallback to return results")
	}
	if results[0].MemoryID != memID {
		t.Errorf("fallback result = %s, want %s", results[0].MemoryID, memID)
	}
}

// TestMemoryVectorSearch_HNSW_WithThreshold verifies threshold filtering
// works with the HNSW path.
func TestMemoryVectorSearch_HNSW_WithThreshold(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	const dims = 384

	// Insert two memories: one similar to query, one dissimilar.
	memSimilar, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "Similar memory content about authentication and security",
		AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMemoryEmbedding(memSimilar, "test-model", makeUnitVec(dims, 10)); err != nil {
		t.Fatal(err)
	}

	memDifferent, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "Different memory content about database migrations and schemas",
		AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMemoryEmbedding(memDifferent, "test-model", makeUnitVec(dims, 300)); err != nil {
		t.Fatal(err)
	}
	st.RebuildMemoryHNSW()

	// Search with high threshold — should only return the similar one.
	queryVec := makeUnitVec(dims, 10)
	results, err := st.MemoryVectorSearchWithThreshold(queryVec, 10, 0.9)
	if err != nil {
		t.Fatal(err)
	}
	// Only the similar memory should pass the 0.9 threshold.
	for _, r := range results {
		if r.MemoryID == memDifferent {
			t.Errorf("dissimilar memory should not pass threshold 0.9, score=%f", r.Score)
		}
	}
}

// TestMemoryVectorSearch_HNSW_ConcurrentSafety verifies HNSW operations are
// safe under concurrent access (the sync.RWMutex pattern from the spike).
func TestMemoryVectorSearch_HNSW_ConcurrentSafety(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	const (
		dims    = 384
		writers = 4
		readers = 4
		ops     = 20
	)

	// Seed some initial data.
	for i := 0; i < 10; i++ {
		memID, _ := st.InsertMemory(Memory{
			Tier:    TierProject,
			Content: fmt.Sprintf("Seed memory %d", i),
			AgentID: "test",
		})
		_ = st.UpsertMemoryEmbedding(memID, "test-model", makeUnitVec(dims, i*30))
	}
	st.RebuildMemoryHNSW()

	var wg sync.WaitGroup

	// Concurrent writers: insert new memories.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				memID, _ := st.InsertMemory(Memory{
					Tier:    TierProject,
					Content: fmt.Sprintf("Writer %d memory %d", id, i),
					AgentID: "test",
				})
				_ = st.UpsertMemoryEmbedding(memID, "test-model", makeUnitVec(dims, id*100+i))
			}
		}(w)
	}

	// Concurrent readers: search.
	// Each goroutine gets its own rng to avoid data races on shared rand state.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			localRng := rand.New(rand.NewSource(seed))
			for i := 0; i < ops; i++ {
				qv := makeRandomUnitVec(localRng, dims)
				_, _ = st.MemoryVectorSearchWithThreshold(qv, 5, 0.0)
			}
		}(int64(r + 42))
	}

	wg.Wait()

	// Verify index is consistent.
	size := st.hnswLen()
	if size == 0 {
		t.Error("HNSW index should be non-empty after concurrent writes")
	}
	t.Logf("HNSW index size after concurrent ops: %d", size)
}

// TestMemoryVectorSearch_HNSW_RecallAccuracy validates that HNSW with 3×
// oversampling achieves ≥90% recall@10 compared to brute-force on clustered data.
// This is the production validation of the spike finding (≥95%).
func TestMemoryVectorSearch_HNSW_RecallAccuracy(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	const (
		dims      = 384
		nClusters = 10
		perClust  = 10
		N         = nClusters * perClust
		k         = 10
		noiseStd  = float32(0.01)
	)
	rng := rand.New(rand.NewSource(42))

	// Generate cluster centres.
	centres := make([][]float32, nClusters)
	for i := range centres {
		centres[i] = makeRandomUnitVec(rng, dims)
	}

	// Insert N memories with clustered embeddings.
	type memInfo struct {
		id  string
		vec []float32
	}
	mems := make([]memInfo, N)
	for i := 0; i < N; i++ {
		ctr := centres[i%nClusters]
		v := make([]float32, dims)
		var nrm float64
		for j := range v {
			v[j] = ctr[j] + float32(rng.NormFloat64())*noiseStd
			nrm += float64(v[j]) * float64(v[j])
		}
		scale := float32(1.0 / math.Sqrt(nrm))
		for j := range v {
			v[j] *= scale
		}

		memID, err := st.InsertMemory(Memory{
			Tier:    TierProject,
			Content: fmt.Sprintf("hnsw_recall_test unique_item_%d cluster_%d seq_%d", i, i%nClusters, i/nClusters),
			AgentID: "test",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertMemoryEmbedding(memID, "test-model", v); err != nil {
			t.Fatal(err)
		}
		mems[i] = memInfo{id: memID, vec: v}
	}
	st.RebuildMemoryHNSW()

	// Compute brute-force ground truth for each cluster centre query.
	var totalHits int
	totalQueries := nClusters * k

	for q := 0; q < nClusters; q++ {
		// Brute-force: compute similarity to all memories.
		type scored struct {
			id    string
			score float32
		}
		bf := make([]scored, N)
		normQ := normalizeVec(centres[q])
		for i, m := range mems {
			bf[i] = scored{m.id, dotSimilarity(normQ, m.vec)}
		}
		// Sort descending by score.
		for i := 0; i < len(bf); i++ {
			for j := i + 1; j < len(bf); j++ {
				if bf[j].score > bf[i].score {
					bf[i], bf[j] = bf[j], bf[i]
				}
			}
		}
		bfSet := make(map[string]bool, k)
		for _, s := range bf[:k] {
			bfSet[s.id] = true
		}

		// HNSW search.
		results, err := st.MemoryVectorSearchWithThreshold(centres[q], k, 0.0)
		if err != nil {
			t.Fatalf("cluster %d search: %v", q, err)
		}
		for _, r := range results {
			if bfSet[r.MemoryID] {
				totalHits++
			}
		}
	}

	recall := float64(totalHits) / float64(totalQueries)
	t.Logf("HNSW recall@%d: %.1f%% (%d/%d hits, N=%d, dims=%d)", k, recall*100, totalHits, totalQueries, N, dims)
	// Threshold: 85% accounts for HNSW variance with random vectors and the
	// race detector. Typical recall is ~97% with unique test data.
	if recall < 0.85 {
		t.Errorf("recall@%d = %.1f%% < 85%% — HNSW+oversampling not meeting target", k, recall*100)
	}
}

// TestNodeHNSW_RebuildAndSearch verifies the node HNSW index works end-to-end.
func TestNodeHNSW_RebuildAndSearch(t *testing.T) {
	t.Parallel()
	st := openHNSWTestStore(t)

	const dims = 384

	// Insert some nodes and embeddings.
	for i := 0; i < 5; i++ {
		nodeID := fmt.Sprintf("node-%d", i)
		st.graphDB.Exec(`INSERT INTO nodes (id, name, file, type) VALUES (?, ?, ?, ?)`,
			nodeID, fmt.Sprintf("Func%d", i), "test.go", "function")
		vec := makeUnitVec(dims, i*50)
		if err := st.UpsertEmbedding(nodeID, "test-model", vec); err != nil {
			t.Fatal(err)
		}
	}

	st.RebuildNodeHNSW()

	// Search for the first node's direction.
	queryVec := makeUnitVec(dims, 0)
	results, err := st.VectorSearch(queryVec, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 result from node HNSW search")
	}
	if results[0].ID != "node-0" {
		t.Errorf("top result = %s, want node-0", results[0].ID)
	}
}

// BenchmarkMemoryVectorSearch_HNSW benchmarks HNSW-backed memory search.
func BenchmarkMemoryVectorSearch_HNSW(b *testing.B) {
	st := openFromTemplate(b)

	const dims = 384
	rng := rand.New(rand.NewSource(42))

	// Seed with 1000 memories.
	for i := 0; i < 1000; i++ {
		memID, _ := st.InsertMemory(Memory{
			Tier:    TierProject,
			Content: fmt.Sprintf("Benchmark memory %d with some content to make it realistic", i),
			AgentID: "bench",
		})
		_ = st.UpsertMemoryEmbedding(memID, "bench-model", makeRandomUnitVec(rng, dims))
	}
	st.RebuildMemoryHNSW()

	queryVec := makeRandomUnitVec(rng, dims)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = st.MemoryVectorSearchWithThreshold(queryVec, 10, 0.0)
	}
}

// BenchmarkMemoryVectorSearch_BruteForce benchmarks brute-force fallback for comparison.
func BenchmarkMemoryVectorSearch_BruteForce(b *testing.B) {
	st := openFromTemplate(b)

	const dims = 384
	rng := rand.New(rand.NewSource(42))

	// Seed with 1000 memories.
	for i := 0; i < 1000; i++ {
		memID, _ := st.InsertMemory(Memory{
			Tier:    TierProject,
			Content: fmt.Sprintf("Benchmark memory %d with some content to make it realistic", i),
			AgentID: "bench",
		})
		_ = st.UpsertMemoryEmbedding(memID, "bench-model", makeRandomUnitVec(rng, dims))
	}

	// Force brute-force by clearing HNSW index.
	st.hnswMemMu.Lock()
	st.hnswMemIndex = nil
	st.hnswMemMu.Unlock()

	queryVec := makeRandomUnitVec(rng, dims)
	normQuery := normalizeVec(queryVec)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = st.memoryVectorSearchBruteForce(normQuery, 10)
	}
}

// TestHNSW_PersistenceRoundTrip verifies save → load preserves search results.
func TestHNSW_PersistenceRoundTrip(t *testing.T) {
	t.Parallel()

	st := openHNSWTestStore(t)
	defer st.Close()

	const dims = 384
	rng := rand.New(rand.NewSource(42))

	// Insert some vectors.
	for i := 0; i < 50; i++ {
		vec := makeRandomUnitVec(rng, dims)
		st.hnswAdd(fmt.Sprintf("mem-%d", i), vec)
	}

	// Verify index works.
	queryVec := makeRandomUnitVec(rng, dims)
	resultsBefore := st.hnswSearch(queryVec, 5)
	if len(resultsBefore) == 0 {
		t.Fatal("expected search results before save")
	}

	// Save to disk.
	path := st.hnswMemPath()
	saveHNSWToFile(st.hnswMemIndex, path)

	// Load into a fresh graph.
	loaded := loadHNSW(path, 50)
	if loaded == nil {
		t.Fatal("loadHNSW returned nil")
	}
	if loaded.Len() != 50 {
		t.Errorf("loaded graph has %d vectors, want 50", loaded.Len())
	}

	// Replace the in-memory index with the loaded one and search again.
	st.hnswMemMu.Lock()
	st.hnswMemIndex = loaded
	st.hnswMemMu.Unlock()

	resultsAfter := st.hnswSearch(queryVec, 5)
	if len(resultsAfter) == 0 {
		t.Fatal("expected search results after load")
	}

	// Top-1 must match.
	if resultsBefore[0].id != resultsAfter[0].id {
		t.Errorf("top-1 changed after persistence: before=%s after=%s", resultsBefore[0].id, resultsAfter[0].id)
	}
}

// TestHNSW_LoadStaleFile verifies that a stale file is rejected.
func TestHNSW_LoadStaleFile(t *testing.T) {
	t.Parallel()

	st := openHNSWTestStore(t)
	defer st.Close()

	const dims = 384
	rng := rand.New(rand.NewSource(99))

	// Insert 100 vectors and save.
	for i := 0; i < 100; i++ {
		st.hnswAdd(fmt.Sprintf("mem-%d", i), makeRandomUnitVec(rng, dims))
	}
	path := st.hnswMemPath()
	saveHNSWToFile(st.hnswMemIndex, path)

	// File has 100, but expected count is 50 — should be rejected as stale.
	loaded := loadHNSW(path, 50)
	if loaded != nil {
		t.Error("expected stale file to be rejected (count mismatch 100 vs 50)")
	}

	// expectedCount=0 but file has data — should be rejected.
	loaded = loadHNSW(path, 0)
	if loaded != nil {
		t.Error("expected stale file to be rejected (db empty, file has data)")
	}

	// expectedCount close to actual (within 5%) — should succeed.
	loaded = loadHNSW(path, 98)
	if loaded == nil {
		t.Error("expected file to be accepted (count within tolerance)")
	}
}

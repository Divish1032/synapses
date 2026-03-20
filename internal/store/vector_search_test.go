package store

import (
	"container/heap"
	"fmt"
	"math"
	"os"
	"testing"
	"time"
)

// --- topKHeap unit tests ---

func TestTopKHeap_Basic(t *testing.T) {
	h := &topKHeap{k: 3}
	h.tryPush("a", 0.5)
	h.tryPush("b", 0.9)
	h.tryPush("c", 0.7)
	h.tryPush("d", 0.3) // should NOT enter (below min of 0.5)
	h.tryPush("e", 0.95) // should replace "a" (0.5)

	if h.Len() != 3 {
		t.Fatalf("expected heap size 3, got %d", h.Len())
	}

	results := h.drain()
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	// Should be descending: e(0.95), b(0.9), c(0.7)
	if results[0].id != "e" || results[1].id != "b" || results[2].id != "c" {
		t.Errorf("unexpected order: %v", results)
	}
}

func TestTopKHeap_EmptyDrain(t *testing.T) {
	h := &topKHeap{k: 5}
	results := h.drain()
	if results != nil {
		t.Errorf("expected nil from empty heap, got %v", results)
	}
}

func TestTopKHeap_SingleElement(t *testing.T) {
	h := &topKHeap{k: 1}
	h.tryPush("a", 0.5)
	h.tryPush("b", 0.9)
	h.tryPush("c", 0.3)

	results := h.drain()
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].id != "b" || results[0].score != 0.9 {
		t.Errorf("expected b/0.9, got %s/%f", results[0].id, results[0].score)
	}
}

func TestTopKHeap_ExactlyK(t *testing.T) {
	h := &topKHeap{k: 3}
	h.tryPush("a", 0.1)
	h.tryPush("b", 0.2)
	h.tryPush("c", 0.3)

	results := h.drain()
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].id != "c" {
		t.Errorf("expected c first, got %s", results[0].id)
	}
}

func TestTopKHeap_DescendingOrder(t *testing.T) {
	h := &topKHeap{k: 100}
	for i := 0; i < 50; i++ {
		h.tryPush(fmt.Sprintf("item-%d", i), float32(i)*0.01)
	}
	results := h.drain()
	for i := 1; i < len(results); i++ {
		if results[i].score > results[i-1].score {
			t.Errorf("not descending at index %d: %f > %f", i, results[i].score, results[i-1].score)
		}
	}
}

func TestTopKHeap_HeapInterface(t *testing.T) {
	// Verify the heap invariant is maintained after operations.
	h := &topKHeap{k: 5}
	heap.Init(h)
	for i := 0; i < 20; i++ {
		h.tryPush(fmt.Sprintf("n-%d", i), float32(i)*0.05+0.01)
	}
	// The heap should contain the 5 highest scores.
	if h.Len() != 5 {
		t.Fatalf("expected 5, got %d", h.Len())
	}
	// Min of heap (root) should be item at index 15 (score 0.76).
	minScore := h.items[0].score
	for _, item := range h.items {
		if item.score < minScore {
			t.Errorf("heap invariant broken: found %f < root %f", item.score, minScore)
		}
	}
}

func TestTopKHeap_ZeroK_NoPanic(t *testing.T) {
	// k=0 should never accept any items (defensive guard).
	h := &topKHeap{k: 0}
	accepted := h.tryPush("a", 0.9)
	if accepted {
		t.Error("expected rejection with k=0")
	}
	if h.Len() != 0 {
		t.Errorf("expected empty heap with k=0, got %d", h.Len())
	}
	results := h.drain()
	if results != nil {
		t.Errorf("expected nil drain with k=0, got %v", results)
	}
}

func TestTopKHeap_TieBreaking(t *testing.T) {
	// When scores are equal, both should be in the heap (no silent drops).
	h := &topKHeap{k: 3}
	h.tryPush("a", 0.5)
	h.tryPush("b", 0.5)
	h.tryPush("c", 0.5)
	h.tryPush("d", 0.5) // same score as min — NOT accepted (not strictly greater)

	if h.Len() != 3 {
		t.Fatalf("expected 3, got %d", h.Len())
	}
}

// --- Two-pass integration tests ---

func TestMemoryVectorSearch_TwoPass_ContentFetched(t *testing.T) {
	// Verify that the second pass correctly fetches content, tier, and entity_id.
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0, 0, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0, 1, 0, 0})

	results, err := st.MemoryVectorSearch([]float32{1, 0, 0, 0}, 5)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	// Check that content was populated (not empty from a failed second pass).
	if results[0].Content == "" {
		t.Error("content not fetched in second pass")
	}
	if results[0].Tier == "" {
		t.Error("tier not fetched in second pass")
	}
	if results[0].MemoryID != ids[0] {
		t.Errorf("expected %s, got %s", ids[0], results[0].MemoryID)
	}
}

func TestMemoryVectorSearch_TwoPass_DeletedBetweenPasses(t *testing.T) {
	// If a memory is deleted between pass 1 and pass 2, it should be silently
	// filtered out (not cause a panic or return a zero-value entry).
	st, _ := openMemEmbedTestStore(t)
	ids := seedMultipleMemories(t, st)

	_ = st.UpsertMemoryEmbedding(ids[0], "test", []float32{1, 0})
	_ = st.UpsertMemoryEmbedding(ids[1], "test", []float32{0.9, 0.1})

	// Delete memory directly (simulating race between passes).
	_, _ = st.knowledgeDB.Exec(`DELETE FROM memories WHERE id = ?`, ids[0])

	results, err := st.MemoryVectorSearch([]float32{1, 0}, 5)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	// The deleted memory's embedding is still in memory_embeddings,
	// but the JOIN in pass 1 filters it out. So this should still work.
	// If somehow it leaked through, fetchMemorySearchResults filters zero-value entries.
	for _, r := range results {
		if r.MemoryID == ids[0] {
			t.Error("deleted memory should not appear in results")
		}
	}
}

func TestVectorSearch_TwoPass_NodeMetadataFetched(t *testing.T) {
	// Verify that VectorSearch (node embeddings) correctly fetches metadata in pass 2.
	f, err := os.CreateTemp("", "test-vecsearch-*.db")
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

	// Insert nodes directly.
	_, _ = st.graphDB.Exec(`INSERT INTO nodes (id, name, type, file, line, signature, doc)
		VALUES ('n1', 'AuthService', 'struct', 'auth.go', 1, '', 'handles auth')`)
	_, _ = st.graphDB.Exec(`INSERT INTO nodes (id, name, type, file, line, signature, doc)
		VALUES ('n2', 'CacheService', 'struct', 'cache.go', 1, '', 'handles caching')`)

	_ = st.UpsertEmbedding("n1", "test", []float32{1, 0, 0})
	_ = st.UpsertEmbedding("n2", "test", []float32{0, 1, 0})

	results, err := st.VectorSearch([]float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if results[0].Name != "AuthService" {
		t.Errorf("expected AuthService first, got %s", results[0].Name)
	}
	if results[0].Doc != "handles auth" {
		t.Errorf("expected doc from second pass, got %q", results[0].Doc)
	}
}

func TestVectorSearch_TwoPass_LargeDataset(t *testing.T) {
	// Verify VectorSearch works correctly at scale with the two-pass approach.
	f, err := os.CreateTemp("", "test-vecsearch-large-*.db")
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

	const totalNodes = 200
	tx, _ := st.graphDB.Begin()
	nodeStmt, _ := tx.Prepare(`INSERT INTO nodes (id, name, type, file, line, signature, doc)
		VALUES (?, ?, 'function', 'test.go', ?, '', ?)`)
	embStmt, _ := tx.Prepare(`INSERT INTO node_embeddings (node_id, model, embedding, content_hash, indexed_at)
		VALUES (?, 'test', ?, 'hash', strftime('%s','now'))`)

	for i := 0; i < totalNodes; i++ {
		id := fmt.Sprintf("node-%d", i)
		name := fmt.Sprintf("Func%d", i)
		nodeStmt.Exec(id, name, i+1, fmt.Sprintf("doc for func %d", i))
		// The last node has the best embedding match.
		angle := float64(i) * 0.01
		vec := []float32{float32(math.Cos(angle)), float32(math.Sin(angle))}
		embStmt.Exec(id, vecToBlob(vec))
	}
	nodeStmt.Close()
	embStmt.Close()
	tx.Commit()

	results, err := st.VectorSearch([]float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(results) != 10 {
		t.Fatalf("expected 10 results, got %d", len(results))
	}
	// First result should be node-0 (cos(0)=1, sin(0)=0 — exactly {1,0}).
	if results[0].ID != "node-0" {
		t.Errorf("expected node-0 as top result, got %s", results[0].ID)
	}
	// Scores should be descending.
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("scores not descending at index %d: %f > %f", i, results[i].Score, results[i-1].Score)
		}
	}
}

func TestMemoryVectorSearch_ScoreOrdering(t *testing.T) {
	// Verify that the min-heap produces correct descending order.
	st, _ := openMemEmbedTestStore(t)

	now := time.Now().UTC()
	expires := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)
	nowStr := now.Format(time.RFC3339)

	// Insert 20 memories with gradually increasing similarity.
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("order-test-%d", i)
		st.knowledgeDB.Exec(`INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags,
			created_at, expires_at, last_accessed_at, source)
			VALUES (?, 'project', ?, '', 'test', '', '[]', ?, ?, ?, 'manual')`,
			id, fmt.Sprintf("ordering test content %d with unique words for dedup avoidance", i),
			nowStr, expires, nowStr)
		// Gradually rotate from {0,1} toward {1,0}.
		angle := float64(i) * (math.Pi / 2.0) / 19.0
		vec := []float32{float32(math.Sin(angle)), float32(math.Cos(angle))}
		st.knowledgeDB.Exec(`INSERT INTO memory_embeddings (memory_id, model, embedding, content_hash, stale, embedded_at)
			VALUES (?, 'test', ?, 'hash', 0, ?)`, id, vecToBlob(vec), now.Unix())
	}

	results, err := st.MemoryVectorSearch([]float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("MemoryVectorSearch: %v", err)
	}
	if len(results) != 10 {
		t.Fatalf("expected 10 results, got %d", len(results))
	}
	// Verify strict descending order.
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("scores not descending at %d: %f > %f", i, results[i].Score, results[i-1].Score)
		}
	}
}

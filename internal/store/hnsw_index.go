package store

// hnsw_index.go — HNSW approximate nearest-neighbor index for memory embeddings.
//
// Sprint 12 #4: Replaces O(N) brute-force vector scan with O(log N) HNSW graph
// traversal. Removes the 10,000-embedding safety cap that silently degraded recall.
// Sub-5ms queries for 50K+ memories vs 200-400ms brute-force.
//
// Design:
//   - Store holds an in-memory hnsw.Graph[string] keyed by memory_id.
//   - All mutations (Add/Delete) are protected by hnswMemMu.Lock().
//   - All reads (Search) are protected by hnswMemMu.RLock().
//   - Index is rebuilt from SQLite on Open() — no persistent HNSW file needed.
//     SQLite is the authoritative source; HNSW is a derived acceleration structure.
//   - 3× oversampling: request limit*3 from HNSW, then SQL Pass 2 re-ranks and
//     filters stale/expired candidates. This achieves ≥95% recall@10 (spike-proven).
//
// Spike findings (Sprint 12 #3):
//   - coder/hnsw Graph is NOT thread-safe internally → sync.RWMutex required.
//   - Search returns candidates in heap order, NOT distance-sorted → Pass 2 handles ordering.
//   - Delete-then-Add of same key can panic in v0.6.1 → we use Add() which replaces
//     existing keys (safe upsert). For removal, we call Delete() and never re-Add
//     the same key (stale memories get fresh UUIDs when re-embedded).
//   - 3× oversampling required for ≥95% recall at M=16, EfSearch=20.

import (
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/coder/hnsw"
)

const (
	// hnswOversample is the multiplier applied to the requested limit when
	// querying the HNSW index. HNSW graph traversal can terminate before
	// finding all k true nearest neighbours; oversampling compensates.
	// Spike-proven: 3× achieves ≥95% recall@10 at M=16, EfSearch=20.
	hnswOversample = 3

	// hnswM is the max number of bidirectional links per node per layer.
	// Higher M = better recall + more memory. 16 is the standard default
	// for 256-1024 dim embeddings (OpenSearch, Milvus, coder/hnsw defaults).
	hnswM = 16

	// hnswEfSearch is the size of the dynamic candidate list during search.
	// Higher efSearch = better recall + higher latency. 40 balances well
	// for our use case (sub-ms queries up to 100K vectors at 384 dims).
	// Doubled from coder/hnsw default of 20 for better recall on real data.
	hnswEfSearch = 40
)

// initMemoryHNSW creates a new empty HNSW graph configured for memory embeddings.
// Called during Store initialization and can be called to reset the index.
func newMemoryHNSW() *hnsw.Graph[string] {
	g := hnsw.NewGraph[string]()
	g.Distance = hnsw.CosineDistance
	g.M = hnswM
	g.EfSearch = hnswEfSearch
	return g
}

// RebuildMemoryHNSW loads all non-stale memory embeddings from SQLite and
// builds the HNSW index from scratch. Called during Store.Open() and can be
// called to rebuild after bulk operations (e.g., model migration).
//
// This is O(N log N) where N is the number of embeddings. For 10K embeddings
// at 384 dims, this takes ~200-500ms. For 50K, ~2-5s. Acceptable at startup.
//
// Thread-safe: acquires exclusive lock on hnswMemMu for the entire rebuild.
// Callers should not hold hnswMemMu when calling this method.
func (s *Store) RebuildMemoryHNSW() {
	// Include stale embeddings — the search API returns them with a StaleEmbedding
	// flag so callers can surface "possibly outdated" results. Only exclude
	// stale/expired memories (the memory itself, not the embedding).
	rows, err := s.knowledgeDB.Query(`
		SELECT e.memory_id, e.embedding
		FROM memory_embeddings e
		JOIN memories m ON e.memory_id = m.id
		WHERE m.stale = 0
		  AND m.expires_at > datetime('now')`)
	if err != nil {
		logutil.Error("synapses: rebuild HNSW index: %v\n", err)
		return
	}
	defer rows.Close()

	g := newMemoryHNSW()
	var count int
	for rows.Next() {
		var memID string
		var blob []byte
		if err := rows.Scan(&memID, &blob); err != nil {
			logutil.Warn("synapses: rebuild HNSW: skip corrupt row: %v\n", err)
			continue
		}
		vec := blobToVec(blob)
		if len(vec) == 0 {
			continue
		}
		g.Add(hnsw.MakeNode(memID, vec))
		count++
	}
	if err := rows.Err(); err != nil {
		logutil.Error("synapses: rebuild HNSW index scan: %v\n", err)
	}

	s.hnswMemMu.Lock()
	s.hnswMemIndex = g
	s.hnswMemMu.Unlock()

	if count > 0 {
		logutil.Info("synapses: HNSW index built (%d memory embeddings, M=%d, efSearch=%d)\n", count, hnswM, hnswEfSearch)
	}
}

// hnswAdd adds or replaces a memory embedding in the HNSW index.
// Safe for existing keys: coder/hnsw's Add replaces if the key already exists.
// Caller must NOT hold hnswMemMu.
//
// Protected with recover: coder/hnsw v0.6.1 can panic in edge cases during
// graph construction (e.g., "node not added" consistency check failure). A
// failed Add is non-fatal — the vector is still in SQLite and will be loaded
// on the next RebuildMemoryHNSW. Fail-silent to never crash the daemon.
func (s *Store) hnswAdd(memoryID string, vec []float32) {
	s.hnswMemMu.Lock()
	defer s.hnswMemMu.Unlock()
	if s.hnswMemIndex == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logutil.Error("synapses: HNSW add recovered from panic (memory_id=%s): %v — invalidating index\n", memoryID, r)
			// A panic during Add leaves the graph in an inconsistent state
			// (partially inserted node across layers). All subsequent operations
			// would cascade-panic. Nil out the index so searches fall back to
			// brute-force SQLite until the next periodic RebuildMemoryHNSW.
			s.hnswMemIndex = nil
		}
	}()
	// Pre-delete to work around coder/hnsw v0.6.1 bug: Graph.Add() internally
	// deletes existing keys then re-adds, but its invariant check
	// (Len() == preLen+1) doesn't account for the delete, causing
	// panic("node not added"). Pre-deleting ensures Add always sees a fresh key.
	s.hnswMemIndex.Delete(memoryID)
	s.hnswMemIndex.Add(hnsw.MakeNode(memoryID, vec))
}

// hnswDelete removes a memory from the HNSW index.
// Returns false if the key was not found. Caller must NOT hold hnswMemMu.
// Protected with recover for the same reason as hnswAdd.
func (s *Store) hnswDelete(memoryID string) (deleted bool) {
	s.hnswMemMu.Lock()
	defer s.hnswMemMu.Unlock()
	if s.hnswMemIndex == nil {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			logutil.Error("synapses: HNSW delete recovered from panic (memory_id=%s): %v\n", memoryID, r)
			deleted = false
		}
	}()
	return s.hnswMemIndex.Delete(memoryID)
}

// hnswSearch returns the top-k candidate memory IDs from the HNSW index.
// Applies 3× oversampling internally. Returns nil if the index is empty or nil.
// Results are in heap order (NOT distance-sorted) — caller must re-rank.
// Caller must NOT hold hnswMemMu.
func (s *Store) hnswSearch(queryVec []float32, limit int) (results []scoredID) {
	s.hnswMemMu.RLock()
	defer s.hnswMemMu.RUnlock()

	if s.hnswMemIndex == nil || s.hnswMemIndex.Len() == 0 {
		return nil
	}

	// Protected with recover: coder/hnsw panics on dimension mismatch
	// (assertDims). Return nil to trigger brute-force fallback gracefully.
	defer func() {
		if r := recover(); r != nil {
			logutil.Error("synapses: HNSW search recovered from panic: %v\n", r)
			results = nil
		}
	}()

	// Request limit × oversample candidates. HNSW returns candidates in
	// heap order — we convert to scoredID with cosine similarity score
	// (1 - cosine_distance) for compatibility with the existing pipeline.
	candidates := s.hnswMemIndex.Search(queryVec, limit*hnswOversample)
	if len(candidates) == 0 {
		return nil
	}

	results = make([]scoredID, 0, len(candidates))
	for _, c := range candidates {
		// coder/hnsw Search returns Node[K] structs (Key + Value vector).
		// We compute cosine similarity directly via dot product on pre-normalized
		// vectors — this is our canonical scoring, not hnsw.CosineDistance.
		score := dotSimilarity(queryVec, c.Value)
		if score <= 0 {
			continue
		}
		results = append(results, scoredID{id: c.Key, score: score})
	}
	return results
}

// hnswLen returns the number of vectors in the HNSW index.
func (s *Store) hnswLen() int {
	s.hnswMemMu.RLock()
	defer s.hnswMemMu.RUnlock()
	if s.hnswMemIndex == nil {
		return 0
	}
	return s.hnswMemIndex.Len()
}

// hnswDeleteBatch removes multiple memory IDs from the HNSW index.
// Used when deleting or marking embeddings stale in bulk.
// Each delete is wrapped in a recover — a panic on one ID must not
// crash the process or skip remaining deletes.
func (s *Store) hnswDeleteBatch(memoryIDs []string) {
	if len(memoryIDs) == 0 {
		return
	}
	s.hnswMemMu.Lock()
	defer s.hnswMemMu.Unlock()
	if s.hnswMemIndex == nil {
		return
	}
	for _, id := range memoryIDs {
		func(mid string) {
			defer func() {
				if r := recover(); r != nil {
					logutil.Error("synapses: HNSW batch-delete recovered from panic (memory_id=%s): %v\n", mid, r)
				}
			}()
			s.hnswMemIndex.Delete(mid)
		}(id)
	}
}

// memoryHNSWReady returns true if the HNSW index is initialized and non-empty.
// Used to decide whether to use HNSW fast path or brute-force fallback.
func (s *Store) memoryHNSWReady() bool {
	s.hnswMemMu.RLock()
	defer s.hnswMemMu.RUnlock()
	return s.hnswMemIndex != nil && s.hnswMemIndex.Len() > 0
}

// NodeHNSW index for graph node embeddings (used by search tool).
// Same pattern as memory HNSW but keyed on node_id and using graphDB.

// RebuildNodeHNSW loads all node embeddings from graphDB into an HNSW index.
func (s *Store) RebuildNodeHNSW() {
	rows, err := s.graphDB.Query(`SELECT node_id, embedding FROM node_embeddings LIMIT 2000000`)
	if err != nil {
		logutil.Error("synapses: rebuild node HNSW index: %v\n", err)
		return
	}
	defer rows.Close()

	g := newMemoryHNSW() // same config works for node embeddings (same 384 dims)
	var count int
	for rows.Next() {
		var nodeID string
		var blob []byte
		if err := rows.Scan(&nodeID, &blob); err != nil {
			logutil.Warn("synapses: rebuild node HNSW: skip corrupt row: %v\n", err)
			continue
		}
		vec := blobToVec(blob)
		if len(vec) == 0 {
			continue
		}
		g.Add(hnsw.MakeNode(nodeID, vec))
		count++
	}
	if err := rows.Err(); err != nil {
		logutil.Error("synapses: rebuild node HNSW index scan: %v\n", err)
	}

	s.hnswNodeMu.Lock()
	s.hnswNodeIndex = g
	s.hnswNodeMu.Unlock()

	if count > 0 {
		logutil.Info("synapses: node HNSW index built (%d node embeddings)\n", count)
	}
}

// NodeHNSWSearch returns top-k candidate node IDs from the node HNSW index.
// Protected with recover: coder/hnsw panics on dimension mismatch.
func (s *Store) NodeHNSWSearch(queryVec []float32, limit int) (results []scoredID) {
	s.hnswNodeMu.RLock()
	defer s.hnswNodeMu.RUnlock()

	if s.hnswNodeIndex == nil || s.hnswNodeIndex.Len() == 0 {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			logutil.Error("synapses: node HNSW search recovered from panic: %v\n", r)
			results = nil
		}
	}()

	candidates := s.hnswNodeIndex.Search(queryVec, limit*hnswOversample)
	if len(candidates) == 0 {
		return nil
	}

	results = make([]scoredID, 0, len(candidates))
	for _, c := range candidates {
		score := dotSimilarity(queryVec, c.Value)
		if score <= 0 {
			continue
		}
		results = append(results, scoredID{id: c.Key, score: score})
	}
	return results
}

// nodeHNSWAdd adds or replaces a node embedding in the node HNSW index.
// Protected with recover: coder/hnsw v0.6.1 can panic during Add().
func (s *Store) nodeHNSWAdd(nodeID string, vec []float32) {
	s.hnswNodeMu.Lock()
	defer s.hnswNodeMu.Unlock()
	if s.hnswNodeIndex == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logutil.Error("synapses: node HNSW add recovered from panic (node_id=%s): %v — invalidating index\n", nodeID, r)
			s.hnswNodeIndex = nil
		}
	}()
	// Pre-delete: same workaround as hnswAdd (see comment there).
	s.hnswNodeIndex.Delete(nodeID)
	s.hnswNodeIndex.Add(hnsw.MakeNode(nodeID, vec))
}

// nodeHNSWReady returns true if the node HNSW index is initialized and non-empty.
func (s *Store) nodeHNSWReady() bool {
	s.hnswNodeMu.RLock()
	defer s.hnswNodeMu.RUnlock()
	return s.hnswNodeIndex != nil && s.hnswNodeIndex.Len() > 0
}

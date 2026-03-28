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
//   - Index is persisted to disk via coder/hnsw Export/Import. On startup, the
//     persistent file is loaded (sub-100ms) and validated against SQLite row count.
//     If missing, corrupt, or stale, a full SQLite rebuild is performed instead.
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
	"bufio"
	"os"
	"path/filepath"
	"time"

	"github.com/coder/hnsw"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// hnswPendingEntry is a vector queued during an HNSW rebuild.
type hnswPendingEntry struct {
	memoryID string
	vec      []float32
}

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
	// Guarantee hnswRebuilding is cleared even if this function panics.
	// Without this, a panic before the Lock at the bottom leaves
	// hnswRebuilding=true forever, causing all future hnswAdd calls to
	// queue entries that are never replayed.
	defer func() {
		if r := recover(); r != nil {
			logutil.Error("synapses: RebuildMemoryHNSW panicked: %v — clearing rebuild flag\n", r)
			s.hnswMemMu.Lock()
			s.hnswRebuilding = false
			s.hnswPendingAdds = nil
			s.hnswPendingDeletes = nil
			s.hnswMemMu.Unlock()
		}
	}()

	s.hnswMemMu.Lock()
	s.hnswRebuilding = true
	s.hnswMemIndex = nil
	s.hnswMemMu.Unlock()

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
		s.hnswMemMu.Lock()
		s.hnswRebuilding = false
		s.hnswPendingAdds = nil
		s.hnswPendingDeletes = nil
		s.hnswMemMu.Unlock()
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
		func() {
			defer func() {
				if r := recover(); r != nil {
					logutil.Warn("synapses: rebuild HNSW: skip panicking entry %s: %v\n", memID, r)
				}
			}()
			g.Delete(memID)
			g.Add(hnsw.MakeNode(memID, vec))
			count++
		}()
	}
	if err := rows.Err(); err != nil {
		logutil.Error("synapses: rebuild HNSW index scan: %v\n", err)
	}

	s.hnswMemMu.Lock()
	// Replay any additions that were queued during the rebuild.
	// Use the same pre-delete + panic-recovery pattern as hnswAdd.
	// Cap replay to 10K entries to bound memory; remaining entries are
	// already in SQLite and will be picked up on the next rebuild.
	pending := s.hnswPendingAdds
	s.hnswPendingAdds = nil
	pendingDeletes := s.hnswPendingDeletes
	s.hnswPendingDeletes = nil
	cappedPending := len(pending) > 10000
	if cappedPending {
		logutil.Warn("synapses: HNSW rebuild: %d pending entries, capping replay to 10000\n", len(pending))
		pending = pending[len(pending)-10000:] // keep most recent
	}
	for _, p := range pending {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logutil.Warn("synapses: HNSW replay skipped memory_id=%s: %v\n", p.memoryID, r)
				}
			}()
			g.Delete(p.memoryID)
			g.Add(hnsw.MakeNode(p.memoryID, p.vec))
			count++
		}()
	}
	// Replay pending deletes (deletions that arrived during rebuild).
	for _, memID := range pendingDeletes {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logutil.Warn("synapses: HNSW rebuild: skip pending delete %s: %v\n", memID, r)
				}
			}()
			g.Delete(memID)
		}()
	}
	s.hnswMemIndex = g
	s.hnswRebuilding = false
	s.hnswMemMu.Unlock()

	if count > 0 {
		logutil.Info("synapses: HNSW index built (%d memory embeddings, %d replayed, M=%d, efSearch=%d)\n", count, len(pending), hnswM, hnswEfSearch)
	}

	// Persist to disk for fast startup next time. Hold read lock during export
	// to prevent concurrent hnswAdd from mutating the graph mid-serialization.
	s.saveMemoryHNSW()

	// If the pending-add queue was capped during this rebuild, some vectors
	// (the oldest ones beyond 10K) are still in SQLite but not in the index.
	// Schedule one immediate follow-up rebuild to pick them up.  This is
	// safe because hnswRebuilding is now false, so the next call proceeds
	// normally and won't loop more than once per original burst.
	if cappedPending {
		logutil.Info("synapses: HNSW rebuild: scheduling follow-up rebuild to recover %d dropped entries\n", len(pending))
		go s.RebuildMemoryHNSW()
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

	// If a rebuild is in progress, queue the addition for replay after
	// rebuild completes. Without this, vectors added during the 200ms-5s
	// rebuild window would be silently lost from the index.
	// Capped at 10K to prevent unbounded growth during long rebuilds;
	// vectors beyond the cap are still in SQLite and will be indexed on
	// the next rebuild.
	if s.hnswRebuilding {
		if len(s.hnswPendingAdds) < 10000 {
			s.hnswPendingAdds = append(s.hnswPendingAdds, hnswPendingEntry{memoryID: memoryID, vec: vec})
		}
		return
	}

	if s.hnswMemIndex == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logutil.Error("synapses: HNSW add recovered from panic (memory_id=%s): %v — rebuilding index\n", memoryID, r)
			// A panic during Add leaves the graph in an inconsistent state
			// (partially inserted node across layers). Nil out immediately so
			// concurrent searches fall back to brute-force SQLite, then rebuild
			// from the authoritative SQLite source in the background.
			s.hnswMemIndex = nil
			s.hnswRebuilding = true
			go s.RebuildMemoryHNSW()
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
	if s.hnswRebuilding {
		s.hnswPendingDeletes = append(s.hnswPendingDeletes, memoryID)
		return true
	}
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
	if s.hnswRebuilding {
		s.hnswPendingDeletes = append(s.hnswPendingDeletes, memoryIDs...)
		return
	}
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
	// Guarantee hnswNodeRebuilding is cleared even if this function panics.
	defer func() {
		if r := recover(); r != nil {
			logutil.Error("synapses: RebuildNodeHNSW panicked: %v — clearing rebuild flag\n", r)
			s.hnswNodeMu.Lock()
			s.hnswNodeRebuilding = false
			s.hnswNodePendingAdds = nil
			s.hnswNodeMu.Unlock()
		}
	}()

	// Signal that a rebuild is in progress so nodeHNSWAdd queues entries.
	s.hnswNodeMu.Lock()
	s.hnswNodeRebuilding = true
	s.hnswNodeIndex = nil
	s.hnswNodeMu.Unlock()

	rows, err := s.graphDB.Query(`SELECT node_id, embedding FROM node_embeddings LIMIT 2000000`)
	if err != nil {
		logutil.Error("synapses: rebuild node HNSW index: %v\n", err)
		s.hnswNodeMu.Lock()
		s.hnswNodeRebuilding = false
		s.hnswNodePendingAdds = nil
		s.hnswNodeMu.Unlock()
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
		func() {
			defer func() {
				if r := recover(); r != nil {
					logutil.Warn("synapses: rebuild node HNSW: skip panicking entry %s: %v\n", nodeID, r)
				}
			}()
			g.Delete(nodeID)
			g.Add(hnsw.MakeNode(nodeID, vec))
			count++
		}()
	}
	if err := rows.Err(); err != nil {
		logutil.Error("synapses: rebuild node HNSW index scan: %v\n", err)
	}

	s.hnswNodeMu.Lock()
	// Replay any additions that were queued during the rebuild.
	pending := s.hnswNodePendingAdds
	s.hnswNodePendingAdds = nil
	if len(pending) > 10000 {
		logutil.Warn("synapses: node HNSW rebuild: %d pending entries, capping replay to 10000\n", len(pending))
		pending = pending[len(pending)-10000:]
	}
	for _, p := range pending {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logutil.Warn("synapses: node HNSW replay skipped node_id=%s: %v\n", p.memoryID, r)
				}
			}()
			g.Delete(p.memoryID)
			g.Add(hnsw.MakeNode(p.memoryID, p.vec))
			count++
		}()
	}
	s.hnswNodeIndex = g
	s.hnswNodeRebuilding = false
	s.hnswNodeMu.Unlock()

	if count > 0 {
		logutil.Info("synapses: node HNSW index built (%d node embeddings, %d replayed)\n", count, len(pending))
	}

	// Persist to disk for fast startup next time.
	s.saveNodeHNSW()
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

	// If a rebuild is in progress, queue the addition for replay.
	if s.hnswNodeRebuilding {
		if len(s.hnswNodePendingAdds) < 10000 {
			s.hnswNodePendingAdds = append(s.hnswNodePendingAdds, hnswPendingEntry{memoryID: nodeID, vec: vec})
		}
		return
	}

	if s.hnswNodeIndex == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logutil.Error("synapses: node HNSW add recovered from panic (node_id=%s): %v — rebuilding index\n", nodeID, r)
			// A panic during Add leaves the graph inconsistent. Nil out the
			// index and set hnswNodeRebuilding so concurrent adds are queued
			// (not silently dropped) until RebuildNodeHNSW completes.
			s.hnswNodeIndex = nil
			s.hnswNodeRebuilding = true
			go s.RebuildNodeHNSW()
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

// ─── Persistent HNSW ────────────────────────────────────────────────────────
//
// The HNSW graph is serialized to disk after every rebuild using coder/hnsw's
// Export/Import binary format (versioned, forward-compatible). On next startup,
// the file is loaded in ~50ms (vs 2-5s rebuild from SQLite for 50K vectors)
// and validated: if the vector count doesn't match SQLite, the file is stale
// and a full rebuild is triggered instead.
//
// File layout:
//   <dataDir>/hnsw_memory.bin  — memory embedding index
//   <dataDir>/hnsw_node.bin    — graph node embedding index
//
// At enterprise scale (500K nodes), the node HNSW file is ~200-400 MB and
// loads in ~500ms — still 5-10x faster than rebuilding from SQLite.

const (
	hnswMemFile  = "hnsw_memory.bin"
	hnswNodeFile = "hnsw_node.bin"
)

func (s *Store) hnswMemPath() string  { return filepath.Join(s.dataDir, hnswMemFile) }
func (s *Store) hnswNodePath() string { return filepath.Join(s.dataDir, hnswNodeFile) }

// saveMemoryHNSW serializes the memory HNSW index to disk under read lock.
// The read lock is held for the entire Export to prevent concurrent hnswAdd
// from mutating the graph mid-serialization (coder/hnsw is not thread-safe).
func (s *Store) saveMemoryHNSW() {
	s.hnswMemMu.RLock()
	defer s.hnswMemMu.RUnlock()
	saveHNSWToFile(s.hnswMemIndex, s.hnswMemPath())
}

// saveNodeHNSW serializes the node HNSW index to disk under read lock.
func (s *Store) saveNodeHNSW() {
	s.hnswNodeMu.RLock()
	defer s.hnswNodeMu.RUnlock()
	saveHNSWToFile(s.hnswNodeIndex, s.hnswNodePath())
}

// saveHNSWToFile serializes an HNSW graph to disk. Best-effort — errors are
// logged but never fatal (SQLite is the authoritative source).
func saveHNSWToFile(g *hnsw.Graph[string], path string) {
	if g == nil || g.Len() == 0 || path == "" {
		return
	}
	start := time.Now()
	// Write to a temp file and rename for atomic replace.
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		logutil.Warn("synapses: HNSW save: create %s: %v\n", tmp, err)
		return
	}
	bw := bufio.NewWriter(f)
	if err := g.Export(bw); err != nil {
		f.Close()
		os.Remove(tmp)
		logutil.Warn("synapses: HNSW save: export to %s: %v\n", tmp, err)
		return
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		logutil.Warn("synapses: HNSW save: flush %s: %v\n", tmp, err)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		logutil.Warn("synapses: HNSW save: close %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		logutil.Warn("synapses: HNSW save: rename %s → %s: %v\n", tmp, path, err)
		return
	}
	logutil.Info("synapses: HNSW saved %s (%d vectors, %s)\n", filepath.Base(path), g.Len(), time.Since(start).Round(time.Millisecond))
}

// loadHNSW attempts to load an HNSW graph from disk. Returns nil if the file
// doesn't exist, is corrupt, or has a vector count that doesn't match expected.
func loadHNSW(path string, expectedCount int) *hnsw.Graph[string] {
	f, err := os.Open(path)
	if err != nil {
		return nil // file doesn't exist — normal on first run
	}
	defer f.Close()

	g := newMemoryHNSW()
	if err := g.Import(bufio.NewReader(f)); err != nil {
		logutil.Warn("synapses: HNSW load: corrupt %s: %v — will rebuild\n", filepath.Base(path), err)
		return nil
	}

	// Staleness check: if SQLite has significantly more or fewer embeddings
	// than the persisted graph, the file is stale. Allow ±5% tolerance to
	// handle small deltas from concurrent writes during shutdown.
	loaded := g.Len()

	// If DB is empty but file has data, it's stale (e.g. all embeddings deleted).
	if expectedCount == 0 && loaded > 0 {
		logutil.Info("synapses: HNSW load: %s stale (file=%d, db=0) — will rebuild\n", filepath.Base(path), loaded)
		return nil
	}

	if expectedCount > 0 {
		diff := loaded - expectedCount
		if diff < 0 {
			diff = -diff
		}
		tolerance := expectedCount / 20 // 5%
		if tolerance < 10 {
			tolerance = 10
		}
		if diff > tolerance {
			logutil.Info("synapses: HNSW load: %s stale (file=%d, db=%d) — will rebuild\n", filepath.Base(path), loaded, expectedCount)
			return nil
		}
	}

	logutil.Info("synapses: HNSW loaded %s (%d vectors)\n", filepath.Base(path), loaded)
	return g
}

// countMemoryEmbeddings returns the number of valid memory embeddings in SQLite.
func (s *Store) countMemoryEmbeddings() int {
	var count int
	err := s.knowledgeDB.QueryRow(`
		SELECT COUNT(*) FROM memory_embeddings e
		JOIN memories m ON e.memory_id = m.id
		WHERE m.stale = 0 AND m.expires_at > datetime('now')`).Scan(&count)
	if err != nil {
		return -1
	}
	return count
}

// countNodeEmbeddings returns the number of node embeddings in SQLite.
func (s *Store) countNodeEmbeddings() int {
	var count int
	err := s.graphDB.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&count)
	if err != nil {
		return -1
	}
	return count
}

// loadOrRebuildMemoryHNSW tries to load the memory HNSW from disk; rebuilds
// from SQLite if the file is missing, corrupt, or stale.
func (s *Store) loadOrRebuildMemoryHNSW() {
	expected := s.countMemoryEmbeddings()
	if g := loadHNSW(s.hnswMemPath(), expected); g != nil {
		s.hnswMemMu.Lock()
		s.hnswMemIndex = g
		s.hnswMemMu.Unlock()
		return
	}
	s.RebuildMemoryHNSW()
}

// loadOrRebuildNodeHNSW tries to load the node HNSW from disk; rebuilds
// from SQLite if the file is missing, corrupt, or stale.
func (s *Store) loadOrRebuildNodeHNSW() {
	expected := s.countNodeEmbeddings()
	if g := loadHNSW(s.hnswNodePath(), expected); g != nil {
		s.hnswNodeMu.Lock()
		s.hnswNodeIndex = g
		s.hnswNodeMu.Unlock()
		return
	}
	s.RebuildNodeHNSW()
}

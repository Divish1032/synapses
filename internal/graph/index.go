package graph

import (
	"strings"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// GraphIndex — Struct of Arrays (SoA) columnar read view
//
// GraphIndex is a read-optimised columnar representation of the graph built
// alongside the existing map[NodeID]*Node store.  All node properties live in
// parallel slices indexed by a sequential uint32 ("seq ID").  Edges are
// stored as flattened CSR (Compressed Sparse Row) adjacency lists so that
// traversing a node's neighbours is a single contiguous slice read — no
// pointer chasing, maximum CPU cache friendliness.
//
// The index is rebuilt from scratch after each full parse cycle via Build().
// During incremental edits, individual nodes are tombstoned (marked deleted)
// rather than compacted immediately; a background goroutine compacts when idle.
//
// Coexistence with the existing map:
//   - Writes always go to Graph.nodes / outEdges / inEdges (unchanged).
//   - BFS reads (CarveEgoGraph) use GraphIndex when it is ready (index.Ready()==true).
//   - If GraphIndex is not yet ready, BFS falls back to the existing map path.
//
// String interning reuses the existing StringPool / StringID from intern.go.
// ---------------------------------------------------------------------------

// GraphIndex is a read-optimised, cache-friendly columnar view of the graph.
// It is rebuilt atomically after each parse cycle and never mutated in place —
// only tombstoned via MarkTombstone / replaced wholesale via RebuildIndex.
type GraphIndex struct {
	mu sync.RWMutex

	// ready is 1 when the index has been built and is safe for BFS reads.
	// Use atomic load/store to avoid locking on the hot path.
	ready int32 // atomic

	// Pool is the shared string interning pool (from intern.go).
	// All string columns below store StringID values.
	Pool *StringPool

	// Parallel node property slices — all indexed by sequential uint32 seq ID.
	// seq 0 is reserved as the "null / not found" sentinel.
	SeqIDs    []NodeID   // seq → original NodeID string
	Types     []StringID // seq → interned NodeType string
	Names     []StringID // seq → interned Name string
	FileIDs   []StringID // seq → interned File path string
	PkgIDs    []StringID // seq → interned Package string
	Lines     []int32    // seq → line number
	Exported  []bool     // seq → exported flag
	Tombstone []bool     // seq → true means node is deleted (pending compaction)

	// IDToSeq maps NodeID strings → seq for O(1) lookup in the BFS hot path.
	IDToSeq map[NodeID]uint32

	// nameIndex maps lowercase name → list of seq IDs for O(1) FindByName.
	// Keys include both the full lowercase name and, for qualified names like
	// "Store.Close", the unqualified suffix ("close"). This means a lookup for
	// "close" returns nodes named "close" AND nodes named "Store.Close" — matching
	// the original linear-scan semantics. Conversely, looking up "store.close"
	// returns only the exact match, not via suffix (also matching linear-scan).
	nameIndex map[string][]uint32

	// fileIndex maps file path → list of seq IDs for O(1) FindByFile.
	// Only two keys per node: the full file path and the basename (e.g. "graph.go").
	// For intermediate suffix queries like "internal/graph/graph.go", the lookup
	// method falls back to a scan over the ~N_files unique file keys (typically
	// 100–500, far fewer than the ~N_nodes that the old linear scan touched).
	fileIndex map[string][]uint32

	// CSR adjacency lists for outgoing edges.
	// Node with seq i has outgoing edges in OutTargets[OutStart[i]:OutEnd[i]].
	OutStart   []uint32   // len = node count + 2 (1-indexed, sentinel at 0)
	OutEnd     []uint32   // len = node count + 2
	OutTargets []uint32   // flattened target seq IDs
	OutTypes   []StringID // flattened edge type string IDs (parallel to OutTargets)

	// CSR adjacency lists for incoming edges (same layout, reversed direction).
	InStart   []uint32
	InEnd     []uint32
	InTargets []uint32
	InTypes   []StringID

	// TombstoneCount tracks how many nodes are tombstoned.
	// If TombstoneCount/len(SeqIDs) > 0.15, the background compactor triggers.
	TombstoneCount int32 // atomic
}

// newGraphIndex returns an empty, unready GraphIndex with a shared StringPool.
func newGraphIndex(pool *StringPool) *GraphIndex {
	idx := &GraphIndex{
		Pool:      pool,
		IDToSeq:   make(map[NodeID]uint32),
		nameIndex: make(map[string][]uint32),
		fileIndex: make(map[string][]uint32),
	}
	// Append sentinel at position 0 for all slices.
	idx.SeqIDs = append(idx.SeqIDs, "")
	idx.Types = append(idx.Types, 0)
	idx.Names = append(idx.Names, 0)
	idx.FileIDs = append(idx.FileIDs, 0)
	idx.PkgIDs = append(idx.PkgIDs, 0)
	idx.Lines = append(idx.Lines, 0)
	idx.Exported = append(idx.Exported, false)
	idx.Tombstone = append(idx.Tombstone, false)
	// CSR sentinel row at position 0.
	idx.OutStart = append(idx.OutStart, 0)
	idx.OutEnd = append(idx.OutEnd, 0)
	idx.InStart = append(idx.InStart, 0)
	idx.InEnd = append(idx.InEnd, 0)
	return idx
}

// Ready returns true if the index has been built and is safe for BFS reads.
func (idx *GraphIndex) Ready() bool {
	return atomic.LoadInt32(&idx.ready) == 1
}

// Seq returns the sequential uint32 ID for nid, or 0 (sentinel) if not found.
func (idx *GraphIndex) Seq(nid NodeID) uint32 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.IDToSeq[nid]
}

// NodeName returns the interned Name string for seq.
func (idx *GraphIndex) NodeName(seq uint32) string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if int(seq) >= len(idx.Names) {
		return ""
	}
	return idx.Pool.Value(idx.Names[seq])
}

// NodeFile returns the interned File path string for seq.
func (idx *GraphIndex) NodeFile(seq uint32) string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if int(seq) >= len(idx.FileIDs) {
		return ""
	}
	return idx.Pool.Value(idx.FileIDs[seq])
}

// IsTombstoned returns true if the node at seq has been logically deleted.
func (idx *GraphIndex) IsTombstoned(seq uint32) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if int(seq) >= len(idx.Tombstone) {
		return true
	}
	return idx.Tombstone[seq]
}

// OutNeighbours returns the slice of outgoing (target seq, edge type StringID)
// values for node seq. The returned slices are direct subslices of internal
// arrays — callers must not modify them.
func (idx *GraphIndex) OutNeighbours(seq uint32) (targets []uint32, types []StringID) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if int(seq) >= len(idx.OutStart) {
		return nil, nil
	}
	start, end := idx.OutStart[seq], idx.OutEnd[seq]
	return idx.OutTargets[start:end], idx.OutTypes[start:end]
}

// InNeighbours returns the slice of incoming (source seq, edge type StringID) values.
func (idx *GraphIndex) InNeighbours(seq uint32) (sources []uint32, types []StringID) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if int(seq) >= len(idx.InStart) {
		return nil, nil
	}
	start, end := idx.InStart[seq], idx.InEnd[seq]
	return idx.InTargets[start:end], idx.InTypes[start:end]
}

// MarkTombstone logically deletes node seq (e.g. when its source file is edited).
// The node remains in the slice arrays until the next compaction sweep.
func (idx *GraphIndex) MarkTombstone(seq uint32) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if int(seq) < len(idx.Tombstone) && !idx.Tombstone[seq] {
		idx.Tombstone[seq] = true
		atomic.AddInt32(&idx.TombstoneCount, 1)
	}
}

// TombstoneRatio returns the fraction of nodes that are tombstoned.
// Used by the background compactor to decide whether to trigger a rebuild.
func (idx *GraphIndex) TombstoneRatio() float64 {
	total := len(idx.SeqIDs) - 1 // exclude sentinel at 0
	if total <= 0 {
		return 0
	}
	return float64(atomic.LoadInt32(&idx.TombstoneCount)) / float64(total)
}

// nameSeqs returns seq IDs matching name (case-insensitive, including qualified
// suffixes). The caller MUST already hold either g.mu.RLock or idx.mu.RLock —
// this method does no locking itself to avoid redundant nested locks.
func (idx *GraphIndex) nameSeqs(name string) []uint32 {
	return idx.nameIndex[strings.ToLower(name)]
}

// fileSeqs returns seq IDs matching filePath. Tries an exact map hit first
// (covers full-path and basename lookups). On miss, falls back to a suffix scan
// over unique file-path keys — O(unique_files), typically 100–500 entries, far
// cheaper than the O(N_nodes) scan it replaces.
//
// The caller MUST already hold either g.mu.RLock or idx.mu.RLock.
func (idx *GraphIndex) fileSeqs(filePath string) []uint32 {
	// Fast path: exact hit (covers full absolute path and basename).
	if seqs := idx.fileIndex[filePath]; len(seqs) > 0 {
		return seqs
	}
	// Slow path: caller passed a relative suffix like "internal/graph/graph.go".
	// Scan unique file keys for a suffix match. Each key is a unique file path
	// (at most one entry per distinct file), so this is O(unique_files).
	suffix := "/" + filePath
	var merged []uint32
	for key, seqs := range idx.fileIndex {
		if strings.HasSuffix(key, suffix) {
			merged = append(merged, seqs...)
		}
	}
	return merged
}

// ---------------------------------------------------------------------------
// buildIndex — construct a fresh GraphIndex from the current Graph map state.
//
// Runs under g.mu.RLock() for snapshotting, then releases the lock before the
// expensive CSR construction.  Only one rebuild runs at a time (enforced by
// Graph.indexMu in RebuildIndex).
// ---------------------------------------------------------------------------

// buildIndex snapshots g and constructs a new, ready GraphIndex.
func buildIndex(g *Graph, pool *StringPool) *GraphIndex {
	// --- Phase 1: snapshot under RLock ---
	g.mu.RLock()

	type nodeSnap struct {
		id       NodeID
		ntype    NodeType
		name     string
		file     string
		pkg      string
		line     int
		exported bool
	}
	type edgeSnap struct {
		from  NodeID
		to    NodeID
		etype EdgeType
	}

	nodeSnaps := make([]nodeSnap, 0, len(g.nodes))
	for _, n := range g.nodes {
		nodeSnaps = append(nodeSnaps, nodeSnap{
			id:       n.ID,
			ntype:    n.Type,
			name:     n.Name,
			file:     n.File,
			pkg:      n.Package,
			line:     n.Line,
			exported: n.Exported,
		})
	}

	edgeSnaps := make([]edgeSnap, 0)
	for _, edges := range g.outEdges {
		for _, e := range edges {
			edgeSnaps = append(edgeSnaps, edgeSnap{
				from:  e.From,
				to:    e.To,
				etype: e.Type,
			})
		}
	}

	g.mu.RUnlock()

	// --- Phase 2: build index without holding the lock ---
	idx := newGraphIndex(pool)
	n := len(nodeSnaps)

	// Assign sequential IDs (1-based; 0 is the sentinel) and build secondary
	// indexes for FindByName and FindByFile in the same pass.
	for i, ns := range nodeSnaps {
		seq := uint32(i + 1)
		idx.IDToSeq[ns.id] = seq
		idx.SeqIDs = append(idx.SeqIDs, ns.id)
		idx.Types = append(idx.Types, pool.Intern(string(ns.ntype)))
		idx.Names = append(idx.Names, pool.Intern(ns.name))
		idx.FileIDs = append(idx.FileIDs, pool.Intern(ns.file))
		idx.PkgIDs = append(idx.PkgIDs, pool.Intern(ns.pkg))
		idx.Lines = append(idx.Lines, int32(ns.line))
		idx.Exported = append(idx.Exported, ns.exported)
		idx.Tombstone = append(idx.Tombstone, false)

		// --- nameIndex: lowercase full name + unqualified suffix ---
		nameLower := strings.ToLower(ns.name)
		idx.nameIndex[nameLower] = append(idx.nameIndex[nameLower], seq)
		if dotPos := strings.LastIndex(ns.name, "."); dotPos >= 0 {
			suffixLower := strings.ToLower(ns.name[dotPos+1:])
			// Avoid duplicate entry when the suffix equals the full lowercase name
			// (can't happen — dotPos >= 0 means there is a prefix — but guard anyway).
			if suffixLower != nameLower {
				idx.nameIndex[suffixLower] = append(idx.nameIndex[suffixLower], seq)
			}
		}

		// --- fileIndex: full path + basename only (2 entries per node max) ---
		// Intermediate suffix queries ("internal/graph/graph.go") are handled at
		// lookup time by fileSeqs via a scan over unique file keys.
		idx.fileIndex[ns.file] = append(idx.fileIndex[ns.file], seq)
		if slashPos := strings.LastIndex(ns.file, "/"); slashPos >= 0 {
			base := ns.file[slashPos+1:]
			if base != ns.file { // guard: file is not already a bare name
				idx.fileIndex[base] = append(idx.fileIndex[base], seq)
			}
		}
	}

	// --- Phase 3: build CSR adjacency lists ---
	// Count out-degree and in-degree per node (1-indexed arrays).
	outDeg := make([]int, n+1)
	inDeg := make([]int, n+1)
	for _, es := range edgeSnaps {
		srcSeq := idx.IDToSeq[es.from]
		dstSeq := idx.IDToSeq[es.to]
		if srcSeq == 0 || dstSeq == 0 {
			continue // dangling edge
		}
		outDeg[srcSeq]++
		inDeg[dstSeq]++
	}

	// Compute prefix-sum start offsets.
	totalEdges := 0
	for _, d := range outDeg[1:] {
		totalEdges += d
	}

	// Extend CSR arrays (already have sentinel row at 0 from newGraphIndex).
	for i := 1; i <= n; i++ {
		idx.OutStart = append(idx.OutStart, 0) // will be set below
		idx.OutEnd = append(idx.OutEnd, 0)
		idx.InStart = append(idx.InStart, 0)
		idx.InEnd = append(idx.InEnd, 0)
	}
	idx.OutTargets = make([]uint32, totalEdges)
	idx.OutTypes = make([]StringID, totalEdges)
	idx.InTargets = make([]uint32, totalEdges)
	idx.InTypes = make([]StringID, totalEdges)

	outPos := uint32(0)
	inPos := uint32(0)
	for i := 1; i <= n; i++ {
		idx.OutStart[i] = outPos
		idx.OutEnd[i] = outPos
		outPos += uint32(outDeg[i])
		idx.InStart[i] = inPos
		idx.InEnd[i] = inPos
		inPos += uint32(inDeg[i])
	}

	// Fill adjacency arrays.
	for _, es := range edgeSnaps {
		srcSeq := idx.IDToSeq[es.from]
		dstSeq := idx.IDToSeq[es.to]
		if srcSeq == 0 || dstSeq == 0 {
			continue
		}
		etID := pool.Intern(string(es.etype))

		// Out direction: src → dst
		op := idx.OutEnd[srcSeq]
		idx.OutTargets[op] = dstSeq
		idx.OutTypes[op] = etID
		idx.OutEnd[srcSeq]++

		// In direction: dst ← src
		ip := idx.InEnd[dstSeq]
		idx.InTargets[ip] = srcSeq
		idx.InTypes[ip] = etID
		idx.InEnd[dstSeq]++
	}

	atomic.StoreInt32(&idx.ready, 1)
	return idx
}

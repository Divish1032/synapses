package graph

import (
	"fmt"
	"sort"
	"sync"
)

// Pool is the global instance of the StringPool accessed by FlatGraph.
var Pool = NewStringPool()

// NodeIndex is a dense integer ID assigned sequentially to each parsed node.
// This replaces the string-based NodeID ("repo::file::name") in the core engine.
type NodeIndex uint32

// EdgeIndex represents a directed adjacency connection.
type EdgeIndex uint32

// ExtID maps our compact NodeIndex back to the stable string-based NodeID
// required by the MCP protocol communication.
func (fg *FlatGraph) ExtID(idx NodeIndex) NodeID {
	fg.mu.RLock()
	defer fg.mu.RUnlock()
	return fg.extIDLocked(idx)
}

// extIDLocked is the lock-free variant of ExtID for callers that already hold fg.mu.
func (fg *FlatGraph) extIDLocked(idx NodeIndex) NodeID {
	if int(idx) >= len(fg.Names) {
		return ""
	}
	repo := fg.RepoID
	file := Pool.Value(fg.FileIDs[idx])
	name := Pool.Value(fg.Names[idx])

	return NodeID(fmt.Sprintf("%s::%s::%s", repo, file, name))
}

// FlatGraph is the V2 "Deterministic Core" engine.
// It uses a Struct-of-Arrays (SoA) layout instead of pointer-heavy maps.
// This ensures continuous memory allocation, maximizing CPU cache locality
// for BFS traversals and preventing GC pauses during million-node loads.
type FlatGraph struct {
	mu     sync.RWMutex
	RepoID string

	// --- SoA Property Slices ---

	// Names holds compact IDs into the String Interning Pool.
	Names []StringID

	// Types holds the NodeType enum directly.
	Types []NodeType

	// FileIDs maps each node to the source file string ID.
	FileIDs []StringID

	// NamespaceIDs enables multi-monolith cross-linking without changing core logic.
	NamespaceIDs []uint16

	// Tombstones is a bitset-like array. If true, the node was deleted during
	// an incremental file parse. Array compaction happens in the background.
	Tombstones []bool

	// TombstoneCount tracks how many nodes are deleted. When >15%, compaction is triggered.
	TombstoneCount int

	// --- Adjacency Lists (CSR-like format) ---

	// OutEdges stores the destination NodeIndex of all outgoing edges continuously.
	OutEdges []NodeIndex
	// OutWeights stores the Semantic EdgeWeight for each corresponding edge in OutEdges.
	OutWeights []float32
	// OutOffsets denotes the starts and ends of a specific node's edges in the OutEdges slice.
	// Node `i`'s edges are in OutEdges[OutOffsets[i] : OutOffsets[i+1]]
	OutOffsets []uint64

	// InEdges (Incoming edges) built identically to OutEdges for reverse lookups.
	InEdges   []NodeIndex
	InWeights []float32
	InOffsets []uint64

	// stringIDToIndex maps a stable "repoID::file::name" NodeID string back to the continuous NodeIndex.
	// Used primarily when agents request specific nodes by name.
	stringIDToIndex map[NodeID]NodeIndex

	// nodeIDs is a NodeIndex-indexed slice of the original graph NodeIDs (relative-path format).
	// Populated by Graph.EnableFlatGraph for O(1) reverse lookup in flatGraphNeighbors.
	// Must be used instead of ExtID when the canonical NodeID is needed.
	nodeIDs []NodeID
}

// Neighbors returns the NodeIndex values for all undirected (out+in) neighbors
// of the given node index. Used by the PPR BFS fast path.
func (fg *FlatGraph) Neighbors(idx NodeIndex) []NodeIndex {
	fg.mu.RLock()
	defer fg.mu.RUnlock()
	n := NodeIndex(len(fg.Names))
	if idx >= n {
		return nil
	}
	out := fg.OutEdges[fg.OutOffsets[idx]:fg.OutOffsets[idx+1]]
	in := fg.InEdges[fg.InOffsets[idx]:fg.InOffsets[idx+1]]
	result := make([]NodeIndex, 0, len(out)+len(in))
	result = append(result, out...)
	result = append(result, in...)
	return result
}

// LookupIndex returns the NodeIndex for a NodeID, or (0, false) if not found.
func (fg *FlatGraph) LookupIndex(id NodeID) (NodeIndex, bool) {
	fg.mu.RLock()
	defer fg.mu.RUnlock()
	idx, ok := fg.stringIDToIndex[id]
	return idx, ok
}

// NodeIDAt returns the original graph NodeID for a NodeIndex, or "" if out of range.
// Uses the nodeIDs slice populated by Graph.EnableFlatGraph.
func (fg *FlatGraph) NodeIDAt(idx NodeIndex) NodeID {
	fg.mu.RLock()
	defer fg.mu.RUnlock()
	if int(idx) >= len(fg.nodeIDs) {
		return ""
	}
	return fg.nodeIDs[idx]
}

// NewFlatGraph initializes an empty SoA Graph structure.
func NewFlatGraph(repoID string) *FlatGraph {
	fg := &FlatGraph{
		RepoID: repoID,

		Names:        make([]StringID, 0, 1000),
		Types:        make([]NodeType, 0, 1000),
		FileIDs:      make([]StringID, 0, 1000),
		NamespaceIDs: make([]uint16, 0, 1000),
		Tombstones:   make([]bool, 0, 1000),

		OutEdges:   make([]NodeIndex, 0, 2000),
		OutWeights: make([]float32, 0, 2000),
		OutOffsets: []uint64{0}, // Seed the first offset

		InEdges:   make([]NodeIndex, 0, 2000),
		InWeights: make([]float32, 0, 2000),
		InOffsets: []uint64{0}, // Seed the first offset

		stringIDToIndex: make(map[NodeID]NodeIndex),
	}
	return fg
}

// AddNode appends a new node into the SoA structure.
func (fg *FlatGraph) AddNode(name StringID, nodeType NodeType, fileID StringID, nsID uint16) NodeIndex {
	fg.mu.Lock()
	defer fg.mu.Unlock()

	idx := NodeIndex(len(fg.Names))

	fg.Names = append(fg.Names, name)
	fg.Types = append(fg.Types, nodeType)
	fg.FileIDs = append(fg.FileIDs, fileID)
	fg.NamespaceIDs = append(fg.NamespaceIDs, nsID)
	fg.Tombstones = append(fg.Tombstones, false)

	// Expand offset structures
	fg.OutOffsets = append(fg.OutOffsets, fg.OutOffsets[idx])
	fg.InOffsets = append(fg.InOffsets, fg.InOffsets[idx])

	// Store external mapping (use lock-free variant — write lock already held)
	extID := fg.extIDLocked(idx)
	fg.stringIDToIndex[extID] = idx

	return idx
}

// addEdgeSlow is disabled — use BulkAddEdges for batch insertion (O(N+E)).
func (fg *FlatGraph) addEdgeSlow(_, _ NodeIndex, _ float32) {
	panic("addEdgeSlow: use BulkAddEdges for batch insertion")
}

// BulkEdge is a single edge for bulk insertion.
type BulkEdge struct {
	From, To NodeIndex
	Weight   float32
}

// BulkAddEdges rebuilds CSR arrays from scratch given a sorted list of edges.
// This is O(E + N) instead of O(E * N) for AddEdge called E times.
func (fg *FlatGraph) BulkAddEdges(edges []BulkEdge) int {
	fg.mu.Lock()
	defer fg.mu.Unlock()

	N := len(fg.Names)
	if N == 0 {
		return 0
	}
	dropped := 0

	// Sort by From for outgoing CSR, count per node.
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})

	// Build outgoing CSR.
	outEdges := make([]NodeIndex, 0, len(edges))
	outWeights := make([]float32, 0, len(edges))
	outOffsets := make([]uint64, N+1)

	for _, e := range edges {
		if int(e.From) >= N || int(e.To) >= N {
			dropped++
			continue
		}
		outEdges = append(outEdges, e.To)
		outWeights = append(outWeights, e.Weight)
		outOffsets[e.From+1]++
	}
	// Prefix sum to get offsets.
	for i := 1; i <= N; i++ {
		outOffsets[i] += outOffsets[i-1]
	}

	// Sort by To for incoming CSR.
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		return edges[i].From < edges[j].From
	})

	inEdges := make([]NodeIndex, 0, len(edges))
	inWeights := make([]float32, 0, len(edges))
	inOffsets := make([]uint64, N+1)

	for _, e := range edges {
		if int(e.From) >= N || int(e.To) >= N {
			continue // already counted in first pass
		}
		inEdges = append(inEdges, e.From)
		inWeights = append(inWeights, e.Weight)
		inOffsets[e.To+1]++
	}
	for i := 1; i <= N; i++ {
		inOffsets[i] += inOffsets[i-1]
	}

	fg.OutEdges = outEdges
	fg.OutWeights = outWeights
	fg.OutOffsets = outOffsets
	fg.InEdges = inEdges
	fg.InWeights = inWeights
	fg.InOffsets = inOffsets
	return dropped
}

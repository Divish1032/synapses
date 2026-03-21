package graph

import (
	"fmt"
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

	// Store external mapping
	extID := fg.ExtID(idx)
	fg.stringIDToIndex[extID] = idx

	return idx
}

// AddEdge inserts a directed edge.
// Note: In a true immutable CSR graph, edges are added by rebuilding the slice.
// For Synapses FlatGraph, incremental edge addition appends to the slice and shifts offsets.
// Since shifting O(E) elements is expensive, bulk-loading is preferred, or
// edges are accumulated in a buffer and flushed together.
func (fg *FlatGraph) AddEdge(from, to NodeIndex, weight float32) {
	fg.mu.Lock()
	defer fg.mu.Unlock()

	if int(from) >= len(fg.Names) || int(to) >= len(fg.Names) {
		return // Out of bounds safety
	}

	// 1. Insert Outgoing Edge for 'from'
	outInsertIdx := fg.OutOffsets[from+1]

	// Create space
	fg.OutEdges = append(fg.OutEdges, 0)
	copy(fg.OutEdges[outInsertIdx+1:], fg.OutEdges[outInsertIdx:])
	fg.OutEdges[outInsertIdx] = to

	fg.OutWeights = append(fg.OutWeights, 0)
	copy(fg.OutWeights[outInsertIdx+1:], fg.OutWeights[outInsertIdx:])
	fg.OutWeights[outInsertIdx] = weight

	// Shift subsequent offsets
	for i := int(from) + 1; i < len(fg.OutOffsets); i++ {
		fg.OutOffsets[i]++
	}

	// 2. Insert Incoming Edge for 'to'
	inInsertIdx := fg.InOffsets[to+1]

	fg.InEdges = append(fg.InEdges, 0)
	copy(fg.InEdges[inInsertIdx+1:], fg.InEdges[inInsertIdx:])
	fg.InEdges[inInsertIdx] = from

	fg.InWeights = append(fg.InWeights, 0)
	copy(fg.InWeights[inInsertIdx+1:], fg.InWeights[inInsertIdx:])
	fg.InWeights[inInsertIdx] = weight

	for i := int(to) + 1; i < len(fg.InOffsets); i++ {
		fg.InOffsets[i]++
	}
}


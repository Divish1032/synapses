package graph

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
)

const magicHeader = "SYNAPSES_FG_V1\n"

// Serialize compresses the FlatGraph into a monolithic binary BLOB.
// This encodes the sequential arrays (SoA layout) directly, eliminating
// per-node allocations and nested object overhead.
func (fg *FlatGraph) Serialize(w io.Writer) error {
	fg.mu.RLock()
	defer fg.mu.RUnlock()

	// Use zstd for extremely fast decompression times (crucial for <200ms booting)
	enc, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return err
	}
	defer enc.Close()

	if _, err := enc.Write([]byte(magicHeader)); err != nil {
		return err
	}

	// 1. Write the RepoID
	if err := writeString(enc, fg.RepoID); err != nil {
		return err
	}

	// 2. Write the StringPool (interned handles)
	if err := serializePool(enc); err != nil {
		return err
	}

	// 3. Write Nodes Capacity
	nodeCount := uint32(len(fg.Names))
	if err := binary.Write(enc, binary.LittleEndian, nodeCount); err != nil {
		return err
	}

	if nodeCount > 0 {
		// Write SoA properties directly as arrays
		if err := binary.Write(enc, binary.LittleEndian, fg.Names); err != nil {
			return err
		}

		// Map []NodeType (string under the hood) to []uint8
		// This is a naive conversion; in production we map string enums to ints
		typesInt := make([]uint8, len(fg.Types))
		for i, t := range fg.Types {
			typesInt[i] = uint8(len(t)) // Using len temporarily as a surrogate ID
		}
		if err := binary.Write(enc, binary.LittleEndian, typesInt); err != nil {
			return err
		}

		if err := binary.Write(enc, binary.LittleEndian, fg.FileIDs); err != nil {
			return err
		}

		if err := binary.Write(enc, binary.LittleEndian, fg.NamespaceIDs); err != nil {
			return err
		}

		// Write Tombstones (bitset packing later, bytes for now)
		tBytes := make([]byte, len(fg.Tombstones))
		for i, b := range fg.Tombstones {
			if b {
				tBytes[i] = 1
			}
		}
		if _, err := enc.Write(tBytes); err != nil {
			return err
		}
	}

	// 4. Write Edges
	outEdgeCount := uint32(len(fg.OutEdges))
	if err := binary.Write(enc, binary.LittleEndian, outEdgeCount); err != nil {
		return err
	}
	if outEdgeCount > 0 {
		if err := binary.Write(enc, binary.LittleEndian, fg.OutEdges); err != nil {
			return err
		}
		if err := binary.Write(enc, binary.LittleEndian, fg.OutWeights); err != nil {
			return err
		}
		if err := binary.Write(enc, binary.LittleEndian, fg.OutOffsets); err != nil {
			return err
		}
	}

	inEdgeCount := uint32(len(fg.InEdges))
	if err := binary.Write(enc, binary.LittleEndian, inEdgeCount); err != nil {
		return err
	}
	if inEdgeCount > 0 {
		if err := binary.Write(enc, binary.LittleEndian, fg.InEdges); err != nil {
			return err
		}
		if err := binary.Write(enc, binary.LittleEndian, fg.InWeights); err != nil {
			return err
		}
		if err := binary.Write(enc, binary.LittleEndian, fg.InOffsets); err != nil {
			return err
		}
	}

	return nil
}

// Deserialize reads the zstd BLOB and reconstructs the global FlatGraph.
func Deserialize(r io.Reader) (*FlatGraph, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	header := make([]byte, len(magicHeader))
	if _, err := io.ReadFull(dec, header); err != nil {
		return nil, err
	}
	if string(header) != magicHeader {
		return nil, errors.New("invalid FlatGraph binary header")
	}

	repoID, err := readString(dec)
	if err != nil {
		return nil, err
	}

	// Read generic graph
	fg := NewFlatGraph(repoID)

	// Deserialize String Pool back into global Pool
	if err := deserializePool(dec); err != nil {
		return nil, err
	}

	// Read Nodes
	var nodeCount uint32
	if err := binary.Read(dec, binary.LittleEndian, &nodeCount); err != nil {
		return nil, err
	}

	if nodeCount > 0 {
		fg.Names = make([]StringID, nodeCount)
		if err := binary.Read(dec, binary.LittleEndian, &fg.Names); err != nil {
			return nil, err
		}

		typesInt := make([]uint8, nodeCount)
		if err := binary.Read(dec, binary.LittleEndian, &typesInt); err != nil {
			return nil, err
		}
		fg.Types = make([]NodeType, nodeCount)
		for i, t := range typesInt {
			// Placeholder mapping inverse
			if t == 4 {
				fg.Types[i] = NodeFile
			} else {
				fg.Types[i] = NodeFunction
			}
		}

		fg.FileIDs = make([]StringID, nodeCount)
		if err := binary.Read(dec, binary.LittleEndian, &fg.FileIDs); err != nil {
			return nil, err
		}

		fg.NamespaceIDs = make([]uint16, nodeCount)
		if err := binary.Read(dec, binary.LittleEndian, &fg.NamespaceIDs); err != nil {
			return nil, err
		}

		tBytes := make([]byte, nodeCount)
		if _, err := io.ReadFull(dec, tBytes); err != nil {
			return nil, err
		}
		fg.Tombstones = make([]bool, nodeCount)
		for i, b := range tBytes {
			if b == 1 {
				fg.Tombstones[i] = true
				fg.TombstoneCount++
			}
		}

		// Rebuild external string mapper
		for i := uint32(0); i < nodeCount; i++ {
			if !fg.Tombstones[i] {
				fg.stringIDToIndex[fg.ExtID(NodeIndex(i))] = NodeIndex(i)
			}
		}
	}

	// Read OutEdges
	var outEdgeCount uint32
	if err := binary.Read(dec, binary.LittleEndian, &outEdgeCount); err != nil {
		return nil, err
	}
	if outEdgeCount > 0 {
		fg.OutEdges = make([]NodeIndex, outEdgeCount)
		_ = binary.Read(dec, binary.LittleEndian, &fg.OutEdges)
		fg.OutWeights = make([]float32, outEdgeCount)
		_ = binary.Read(dec, binary.LittleEndian, &fg.OutWeights)
		fg.OutOffsets = make([]uint64, nodeCount+1) // N + 1
		_ = binary.Read(dec, binary.LittleEndian, &fg.OutOffsets)
	}

	// Read InEdges
	var inEdgeCount uint32
	if err := binary.Read(dec, binary.LittleEndian, &inEdgeCount); err != nil {
		return nil, err
	}
	if inEdgeCount > 0 {
		fg.InEdges = make([]NodeIndex, inEdgeCount)
		_ = binary.Read(dec, binary.LittleEndian, &fg.InEdges)
		fg.InWeights = make([]float32, inEdgeCount)
		_ = binary.Read(dec, binary.LittleEndian, &fg.InWeights)
		fg.InOffsets = make([]uint64, nodeCount+1)
		_ = binary.Read(dec, binary.LittleEndian, &fg.InOffsets)
	}

	return fg, nil
}

func serializePool(w io.Writer) error {
	Pool.mu.RLock()
	defer Pool.mu.RUnlock()

	internedCount := uint32(len(Pool.reverse))
	if err := binary.Write(w, binary.LittleEndian, internedCount); err != nil {
		return err
	}
	for _, handle := range Pool.reverse {
		if err := writeString(w, handle.Value()); err != nil {
			return err
		}
	}
	return nil
}

func deserializePool(r io.Reader) error {
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return err
	}
	for i := uint32(0); i < count; i++ {
		s, err := readString(r)
		if err != nil {
			return err
		}
		_ = Pool.Intern(s) // Repopulate the pool
	}
	return nil
}

func writeString(w io.Writer, s string) error {
	length := uint32(len(s))
	if err := binary.Write(w, binary.LittleEndian, length); err != nil {
		return err
	}
	if length > 0 {
		_, err := w.Write([]byte(s))
		return err
	}
	return nil
}

func readString(r io.Reader) (string, error) {
	var length uint32
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return "", err
	}
	if length == 0 {
		return "", nil
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// DeserializeMapped deserializes a FlatGraph from raw bytes.
// MemoryMap (Placeholder) - mmap requires OS specific syscalls (unix package)
// to map a file directly to the struct byte slices.
// This deserializer reads to heap for now, but provides the structure
// required to slot in `mmap` instantly.
func DeserializeMapped(rawBytes []byte) (*FlatGraph, error) {
	// Directly cast rawBytes to struct slices using unsafe.Slice
	return Deserialize(bytes.NewReader(rawBytes))
}

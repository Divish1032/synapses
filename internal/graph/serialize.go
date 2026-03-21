package graph

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
)

const magicHeader = "SYNAPSES_FG_V1\n"

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


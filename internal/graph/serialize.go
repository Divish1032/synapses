package graph

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

const magicHeader = "SYNAPSES_FG_V1\n"

// Serialize writes fg to w in zstd-compressed binary format, mirroring
// Deserialize. The format is suitable for fast cold-start graph reloads.
func Serialize(fg *FlatGraph, w io.Writer) error {
	enc, err := zstd.NewWriter(w)
	if err != nil {
		return err
	}
	// Write magic header.
	if _, err := enc.Write([]byte(magicHeader)); err != nil {
		enc.Close()
		return err
	}
	// Write RepoID.
	if err := writeString(enc, fg.RepoID); err != nil {
		enc.Close()
		return err
	}
	// Serialize string pool.
	if err := serializePool(enc); err != nil {
		enc.Close()
		return err
	}

	fg.mu.RLock()
	defer fg.mu.RUnlock()

	nodeCount := uint32(len(fg.Names))
	if err := binary.Write(enc, binary.LittleEndian, nodeCount); err != nil {
		enc.Close()
		return err
	}
	if nodeCount > 0 {
		if err := binary.Write(enc, binary.LittleEndian, fg.Names); err != nil {
			enc.Close()
			return err
		}
		typesInt := make([]uint8, nodeCount)
		for i, t := range fg.Types {
			typesInt[i] = NodeTypeToUint8(t)
		}
		if err := binary.Write(enc, binary.LittleEndian, typesInt); err != nil {
			enc.Close()
			return err
		}
		if err := binary.Write(enc, binary.LittleEndian, fg.FileIDs); err != nil {
			enc.Close()
			return err
		}
		if err := binary.Write(enc, binary.LittleEndian, fg.NamespaceIDs); err != nil {
			enc.Close()
			return err
		}
		tBytes := make([]byte, nodeCount)
		for i, t := range fg.Tombstones {
			if t {
				tBytes[i] = 1
			}
		}
		if _, err := enc.Write(tBytes); err != nil {
			enc.Close()
			return err
		}
	}

	// OutEdges.
	outEdgeCount := uint32(len(fg.OutEdges))
	if err := binary.Write(enc, binary.LittleEndian, outEdgeCount); err != nil {
		enc.Close()
		return err
	}
	if outEdgeCount > 0 {
		if err := binary.Write(enc, binary.LittleEndian, fg.OutEdges); err != nil {
			enc.Close()
			return err
		}
		if err := binary.Write(enc, binary.LittleEndian, fg.OutWeights); err != nil {
			enc.Close()
			return err
		}
		if err := binary.Write(enc, binary.LittleEndian, fg.OutOffsets); err != nil {
			enc.Close()
			return err
		}
	}

	// InEdges.
	inEdgeCount := uint32(len(fg.InEdges))
	if err := binary.Write(enc, binary.LittleEndian, inEdgeCount); err != nil {
		enc.Close()
		return err
	}
	if inEdgeCount > 0 {
		if err := binary.Write(enc, binary.LittleEndian, fg.InEdges); err != nil {
			enc.Close()
			return err
		}
		if err := binary.Write(enc, binary.LittleEndian, fg.InWeights); err != nil {
			enc.Close()
			return err
		}
		if err := binary.Write(enc, binary.LittleEndian, fg.InOffsets); err != nil {
			enc.Close()
			return err
		}
	}

	return enc.Close()
}

// serializePool writes the global Pool to w in the same format deserializePool reads.
func serializePool(w io.Writer) error {
	Pool.mu.RLock()
	defer Pool.mu.RUnlock()
	count := uint32(len(Pool.reverse))
	if err := binary.Write(w, binary.LittleEndian, count); err != nil {
		return err
	}
	for _, h := range Pool.reverse {
		if err := writeString(w, h.Value()); err != nil {
			return err
		}
	}
	return nil
}

// writeString writes a length-prefixed string in the format readString expects.
func writeString(w io.Writer, s string) error {
	length := uint32(len(s))
	if err := binary.Write(w, binary.LittleEndian, length); err != nil {
		return err
	}
	if length > 0 {
		_, err := io.WriteString(w, s)
		return err
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
	const maxNodeCount = 10_000_000
	if nodeCount > maxNodeCount {
		return nil, fmt.Errorf("nodeCount %d exceeds maximum %d", nodeCount, maxNodeCount)
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
			fg.Types[i] = uint8ToNodeType(t)
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
	const maxEdgeCount = 50_000_000
	if outEdgeCount > maxEdgeCount {
		return nil, fmt.Errorf("outEdgeCount %d exceeds maximum %d", outEdgeCount, maxEdgeCount)
	}
	if outEdgeCount > 0 {
		fg.OutEdges = make([]NodeIndex, outEdgeCount)
		if err := binary.Read(dec, binary.LittleEndian, &fg.OutEdges); err != nil {
			return nil, fmt.Errorf("deserialize OutEdges: %w", err)
		}
		fg.OutWeights = make([]float32, outEdgeCount)
		if err := binary.Read(dec, binary.LittleEndian, &fg.OutWeights); err != nil {
			return nil, fmt.Errorf("deserialize OutWeights: %w", err)
		}
		fg.OutOffsets = make([]uint64, nodeCount+1) // N + 1
		if err := binary.Read(dec, binary.LittleEndian, &fg.OutOffsets); err != nil {
			return nil, fmt.Errorf("deserialize OutOffsets: %w", err)
		}
	}

	// Read InEdges
	var inEdgeCount uint32
	if err := binary.Read(dec, binary.LittleEndian, &inEdgeCount); err != nil {
		return nil, err
	}
	if inEdgeCount > maxEdgeCount {
		return nil, fmt.Errorf("inEdgeCount %d exceeds maximum %d", inEdgeCount, maxEdgeCount)
	}
	if inEdgeCount > 0 {
		fg.InEdges = make([]NodeIndex, inEdgeCount)
		if err := binary.Read(dec, binary.LittleEndian, &fg.InEdges); err != nil {
			return nil, fmt.Errorf("deserialize InEdges: %w", err)
		}
		fg.InWeights = make([]float32, inEdgeCount)
		if err := binary.Read(dec, binary.LittleEndian, &fg.InWeights); err != nil {
			return nil, fmt.Errorf("deserialize InWeights: %w", err)
		}
		fg.InOffsets = make([]uint64, nodeCount+1)
		if err := binary.Read(dec, binary.LittleEndian, &fg.InOffsets); err != nil {
			return nil, fmt.Errorf("deserialize InOffsets: %w", err)
		}
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

// uint8ToNodeType maps serialized uint8 back to NodeType. Must stay in sync with
// the serialization side (nodeTypeToUint8 in the graph builder).
func uint8ToNodeType(t uint8) NodeType {
	switch t {
	case 0:
		return NodeFunction
	case 1:
		return NodeMethod
	case 2:
		return NodeStruct
	case 3:
		return NodeInterface
	case 4:
		return NodeFile
	case 5:
		return NodePackage
	case 6:
		return NodeVariable
	case 7:
		return NodeRoute
	case 8:
		return NodeSection
	default:
		// NOTE: If you add a new NodeType, add a case here.
		// Unknown types are mapped to NodeFunction as a safe default;
		// this preserves backward compat with newer serialized data.
		return NodeFunction
	}
}

// maxKnownNodeType is the highest uint8 value with a defined NodeType mapping.
// Used by Deserialize to detect data from a newer serializer version.
const maxKnownNodeType uint8 = 8

// NodeTypeToUint8 maps a NodeType to its serialized uint8 value.
func NodeTypeToUint8(nt NodeType) uint8 {
	switch nt {
	case NodeFunction:
		return 0
	case NodeMethod:
		return 1
	case NodeStruct:
		return 2
	case NodeInterface:
		return 3
	case NodeFile:
		return 4
	case NodePackage:
		return 5
	case NodeVariable:
		return 6
	case NodeRoute:
		return 7
	case NodeSection:
		return 8
	default:
		// NOTE: If you add a new NodeType, add a case here.
		return 0
	}
}

// maxStringLength caps string allocations during deserialization to prevent OOM on malformed BLOBs.
const maxStringLength = 10 * 1024 * 1024 // 10 MB

func readString(r io.Reader) (string, error) {
	var length uint32
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return "", err
	}
	if length == 0 {
		return "", nil
	}
	if length > maxStringLength {
		return "", fmt.Errorf("readString: length %d exceeds maximum %d", length, maxStringLength)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

package graph

// index_snapshot.go — binary serialisation and mmap-load for GraphIndex.
//
// The snapshot encodes the entire SoA slice state as a single zstd-compressed
// binary BLOB.  On a warm boot (SQLite DB + snapshot BLOB both exist and
// file_hashes are unchanged), the watcher calls LoadSnapshot instead of
// re-parsing every file, reducing cold-start time from seconds to <200ms for
// large repos.
//
// Wire-format (little-endian, no external schema):
//   [4]  magic      "SIDX"
//   [4]  version    uint32  (currently 1)
//   [4]  nodeCount  uint32
//   [4]  edgeCount  uint32  (total edges, used to pre-allocate CSR arrays)
//   ... for each node (nodeCount entries):
//         [varies]  NodeID string (uint32 len + bytes)
//         [4]       TypeID  StringID
//         [4]       NameID  StringID
//         [4]       FileID  StringID
//         [4]       PkgID   StringID
//         [4]       Line    int32
//         [1]       Exported bool
//   ... StringPool section:
//         [4]       poolSize uint32
//         for each entry: uint32 len + bytes
//   ... CSR out-edges:
//         for each node: uint32 start, uint32 end
//         [4*edgeCount] OutTargets
//         [4*edgeCount] OutTypes (StringID)
//   ... CSR in-edges (same layout)
//
// The BLOB is zstd-compressed before being passed to the store.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
)

const (
	snapshotMagic   = "SIDX"
	snapshotVersion = uint32(1)
)

var (
	errBadMagic   = errors.New("graph snapshot: invalid magic bytes")
	errBadVersion = errors.New("graph snapshot: unsupported version")
)

// SaveSnapshot serialises idx to a zstd-compressed byte slice.
// The caller is responsible for persisting the bytes (e.g. in the SQLite meta table).
func (idx *GraphIndex) SaveSnapshot() ([]byte, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var buf bytes.Buffer

	// Helper writers
	writeU32 := func(v uint32) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	writeI32 := func(v int32) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	writeSID := func(v StringID) { writeU32(uint32(v)) }
	writeBool := func(v bool) {
		b := byte(0)
		if v {
			b = 1
		}
		buf.WriteByte(b)
	}
	writeStr := func(s string) {
		bs := []byte(s)
		writeU32(uint32(len(bs)))
		buf.Write(bs)
	}

	// Header
	buf.WriteString(snapshotMagic)
	writeU32(snapshotVersion)

	nodeCount := uint32(len(idx.SeqIDs) - 1) // exclude sentinel at 0
	edgeCount := uint32(len(idx.OutTargets))
	writeU32(nodeCount)
	writeU32(edgeCount)

	// Node property arrays (1-indexed, skip sentinel at 0)
	for i := uint32(1); i <= nodeCount; i++ {
		writeStr(string(idx.SeqIDs[i]))
		writeSID(idx.Types[i])
		writeSID(idx.Names[i])
		writeSID(idx.FileIDs[i])
		writeSID(idx.PkgIDs[i])
		writeI32(idx.Lines[i])
		writeBool(idx.Exported[i])
	}

	// StringPool: reconstruct all interned strings in insertion order
	// (position 0 = empty string, already implied)
	idx.Pool.mu.RLock()
	poolStrs := make([]string, len(idx.Pool.reverse))
	for i, h := range idx.Pool.reverse {
		poolStrs[i] = h.Value()
	}
	idx.Pool.mu.RUnlock()

	writeU32(uint32(len(poolStrs)))
	for _, s := range poolStrs {
		writeStr(s)
	}

	// CSR out-edges
	for i := uint32(1); i <= nodeCount; i++ {
		writeU32(idx.OutStart[i])
		writeU32(idx.OutEnd[i])
	}
	for _, t := range idx.OutTargets {
		writeU32(t)
	}
	for _, t := range idx.OutTypes {
		writeSID(t)
	}

	// CSR in-edges
	for i := uint32(1); i <= nodeCount; i++ {
		writeU32(idx.InStart[i])
		writeU32(idx.InEnd[i])
	}
	for _, t := range idx.InTargets {
		writeU32(t)
	}
	for _, t := range idx.InTypes {
		writeSID(t)
	}

	// zstd compress
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("graph snapshot: zstd encoder: %w", err)
	}
	compressed := enc.EncodeAll(buf.Bytes(), nil)
	enc.Close()
	return compressed, nil
}

// LoadSnapshot deserialises a zstd-compressed byte slice produced by SaveSnapshot
// and returns a ready GraphIndex. The provided pool is reused so strings already
// interned during a previous session share the same memory.
func LoadSnapshot(data []byte, pool *StringPool) (*GraphIndex, error) {
	// zstd decompress
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("graph snapshot: zstd decoder: %w", err)
	}
	raw, err := dec.DecodeAll(data, nil)
	dec.Close()
	if err != nil {
		return nil, fmt.Errorf("graph snapshot: decompress: %w", err)
	}

	r := bytes.NewReader(raw)

	readU32 := func() (uint32, error) {
		var v uint32
		return v, binary.Read(r, binary.LittleEndian, &v)
	}
	readI32 := func() (int32, error) {
		var v int32
		return v, binary.Read(r, binary.LittleEndian, &v)
	}
	readSID := func() (StringID, error) {
		v, err := readU32()
		return StringID(v), err
	}
	readBool := func() (bool, error) {
		b, err := r.ReadByte()
		return b == 1, err
	}
	readStr := func() (string, error) {
		l, err := readU32()
		if err != nil {
			return "", err
		}
		if l == 0 {
			return "", nil
		}
		if l > 10*1024*1024 { // 10 MB max string length
			return "", fmt.Errorf("readStr: length %d exceeds maximum", l)
		}
		bs := make([]byte, l)
		if _, err := io.ReadFull(r, bs); err != nil {
			return "", err
		}
		return string(bs), nil
	}

	// Magic + version
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, errBadMagic
	}
	if string(magic) != snapshotMagic {
		return nil, errBadMagic
	}
	ver, err := readU32()
	if err != nil || ver != snapshotVersion {
		return nil, errBadVersion
	}

	nodeCount, err := readU32()
	if err != nil {
		return nil, err
	}
	edgeCount, err := readU32()
	if err != nil {
		return nil, err
	}
	const maxNodeCount uint32 = 10_000_000
	if nodeCount > maxNodeCount {
		return nil, fmt.Errorf("nodeCount %d exceeds maximum %d", nodeCount, maxNodeCount)
	}
	const maxEdgeCount uint32 = 50_000_000
	if edgeCount > maxEdgeCount {
		return nil, fmt.Errorf("edgeCount %d exceeds maximum %d", edgeCount, maxEdgeCount)
	}

	idx := newGraphIndex(pool)

	// Pre-allocate to avoid repeated re-slicing
	idx.SeqIDs = make([]NodeID, nodeCount+1)
	idx.Types = make([]StringID, nodeCount+1)
	idx.Names = make([]StringID, nodeCount+1)
	idx.FileIDs = make([]StringID, nodeCount+1)
	idx.PkgIDs = make([]StringID, nodeCount+1)
	idx.Lines = make([]int32, nodeCount+1)
	idx.Exported = make([]bool, nodeCount+1)
	idx.Tombstone = make([]bool, nodeCount+1)

	// Sentinel at 0 already set by newGraphIndex; fill 1..nodeCount
	for i := uint32(1); i <= nodeCount; i++ {
		nidStr, err := readStr()
		if err != nil {
			return nil, fmt.Errorf("graph snapshot: node %d id: %w", i, err)
		}
		typeID, err := readSID()
		if err != nil {
			return nil, fmt.Errorf("graph snapshot: node %d typeID: %w", i, err)
		}
		nameID, err := readSID()
		if err != nil {
			return nil, fmt.Errorf("graph snapshot: node %d nameID: %w", i, err)
		}
		fileID, err := readSID()
		if err != nil {
			return nil, fmt.Errorf("graph snapshot: node %d fileID: %w", i, err)
		}
		pkgID, err := readSID()
		if err != nil {
			return nil, fmt.Errorf("graph snapshot: node %d pkgID: %w", i, err)
		}
		line, err := readI32()
		if err != nil {
			return nil, fmt.Errorf("graph snapshot: node %d line: %w", i, err)
		}
		exp, err := readBool()
		if err != nil {
			return nil, fmt.Errorf("graph snapshot: node %d exported: %w", i, err)
		}

		nid := NodeID(nidStr)
		idx.SeqIDs[i] = nid
		idx.Types[i] = typeID
		idx.Names[i] = nameID
		idx.FileIDs[i] = fileID
		idx.PkgIDs[i] = pkgID
		idx.Lines[i] = line
		idx.Exported[i] = exp
		idx.IDToSeq[nid] = i
	}

	// StringPool must be deserialized BEFORE rebuilding secondary indexes,
	// because the index rebuild calls pool.Value() to resolve StringIDs.
	poolSize, err := readU32()
	if err != nil {
		return nil, err
	}
	for i := uint32(0); i < poolSize; i++ {
		s, err := readStr()
		if err != nil {
			return nil, fmt.Errorf("graph snapshot: pool entry %d: %w", i, err)
		}
		pool.Intern(s) // re-intern to restore IDs (IDs are positional in the pool)
	}

	// --- Rebuild secondary indexes (nameIndex, fileIndex, receiverIndex) ---
	// These are not serialised; rebuild from the node property arrays just
	// loaded, matching the logic in buildIndex().
	for i := uint32(1); i <= nodeCount; i++ {
		name := pool.Value(idx.Names[i])
		file := pool.Value(idx.FileIDs[i])
		ntype := NodeType(pool.Value(idx.Types[i]))

		// nameIndex: lowercase full name + unqualified suffix
		nameLower := strings.ToLower(name)
		idx.nameIndex[nameLower] = append(idx.nameIndex[nameLower], i)
		if dotPos := strings.LastIndex(name, "."); dotPos >= 0 {
			suffixLower := strings.ToLower(name[dotPos+1:])
			if suffixLower != nameLower {
				idx.nameIndex[suffixLower] = append(idx.nameIndex[suffixLower], i)
			}
			// receiverIndex: map receiver name → method seq IDs
			if ntype == NodeMethod {
				receiverLower := strings.ToLower(name[:dotPos])
				idx.receiverIndex[receiverLower] = append(idx.receiverIndex[receiverLower], i)
			}
		}

		// fileIndex: full path + basename only
		idx.fileIndex[file] = append(idx.fileIndex[file], i)
		if slashPos := strings.LastIndex(file, "/"); slashPos >= 0 {
			base := file[slashPos+1:]
			if base != file {
				idx.fileIndex[base] = append(idx.fileIndex[base], i)
			}
		}
	}

	// CSR out-edges
	idx.OutStart = make([]uint32, nodeCount+2)
	idx.OutEnd = make([]uint32, nodeCount+2)
	for i := uint32(1); i <= nodeCount; i++ {
		if idx.OutStart[i], err = readU32(); err != nil {
			return nil, fmt.Errorf("graph snapshot: out-edge start %d: %w", i, err)
		}
		if idx.OutEnd[i], err = readU32(); err != nil {
			return nil, fmt.Errorf("graph snapshot: out-edge end %d: %w", i, err)
		}
	}
	idx.OutTargets = make([]uint32, edgeCount)
	for i := range idx.OutTargets {
		if idx.OutTargets[i], err = readU32(); err != nil {
			return nil, fmt.Errorf("graph snapshot: out-target %d: %w", i, err)
		}
	}
	idx.OutTypes = make([]StringID, edgeCount)
	for i := range idx.OutTypes {
		if idx.OutTypes[i], err = readSID(); err != nil {
			return nil, fmt.Errorf("graph snapshot: out-type %d: %w", i, err)
		}
	}

	// CSR in-edges
	idx.InStart = make([]uint32, nodeCount+2)
	idx.InEnd = make([]uint32, nodeCount+2)
	for i := uint32(1); i <= nodeCount; i++ {
		if idx.InStart[i], err = readU32(); err != nil {
			return nil, fmt.Errorf("graph snapshot: in-edge start %d: %w", i, err)
		}
		if idx.InEnd[i], err = readU32(); err != nil {
			return nil, fmt.Errorf("graph snapshot: in-edge end %d: %w", i, err)
		}
	}
	idx.InTargets = make([]uint32, edgeCount)
	for i := range idx.InTargets {
		if idx.InTargets[i], err = readU32(); err != nil {
			return nil, fmt.Errorf("graph snapshot: in-target %d: %w", i, err)
		}
	}
	idx.InTypes = make([]StringID, edgeCount)
	for i := range idx.InTypes {
		if idx.InTypes[i], err = readSID(); err != nil {
			return nil, fmt.Errorf("graph snapshot: in-type %d: %w", i, err)
		}
	}

	// Validate CSR offsets to detect corrupt blobs before they cause a panic.
	for i := uint32(1); i <= nodeCount; i++ {
		if idx.OutStart[i] > idx.OutEnd[i] || idx.OutEnd[i] > edgeCount {
			return nil, fmt.Errorf("graph snapshot: corrupt out-edge offsets at node %d (start=%d end=%d edgeCount=%d)",
				i, idx.OutStart[i], idx.OutEnd[i], edgeCount)
		}
		if idx.InStart[i] > idx.InEnd[i] || idx.InEnd[i] > edgeCount {
			return nil, fmt.Errorf("graph snapshot: corrupt in-edge offsets at node %d (start=%d end=%d edgeCount=%d)",
				i, idx.InStart[i], idx.InEnd[i], edgeCount)
		}
	}
	for i, t := range idx.OutTargets {
		if t == 0 || t > nodeCount {
			return nil, fmt.Errorf("graph snapshot: out-target %d (%d) out of range [1, %d]", i, t, nodeCount)
		}
	}
	for i, t := range idx.InTargets {
		if t == 0 || t > nodeCount {
			return nil, fmt.Errorf("graph snapshot: in-target %d (%d) out of range [1, %d]", i, t, nodeCount)
		}
	}

	// Recompute eigenvector centrality from the restored CSR arrays.
	// Not serialised — cheap to recompute (<10 ms) and avoids a version bump.
	idx.computeEigenvectorCentrality()

	atomic.StoreInt32(&idx.ready, 1)
	return idx, nil
}

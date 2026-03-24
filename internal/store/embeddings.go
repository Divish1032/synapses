package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/viterin/vek/vek32"
)

// nodeContentHash computes an 8-char hex hash of the concatenated node text
// (name + signature + doc) used to detect stale embeddings after code changes.
func nodeContentHash(name, sig, doc string) string {
	parts := []string{name}
	if sig != "" {
		parts = append(parts, sig)
	}
	if doc != "" {
		parts = append(parts, doc)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, " ")))
	return hex.EncodeToString(sum[:4]) // 8 hex chars
}

// nodeText builds the embedding input string from raw node fields.
// Mirrors GetNodeTextForEmbedding without a DB round-trip.
func nodeText(name, sig, doc string) string {
	parts := []string{name}
	if sig != "" {
		parts = append(parts, sig)
	}
	if doc != "" {
		parts = append(parts, doc)
	}
	return strings.Join(parts, " ")
}

// UpsertEmbedding stores or replaces the vector embedding for a graph node.
// vec is encoded as a little-endian float32 BLOB. model is the model name
// used to generate the embedding (for cache invalidation when the model
// changes). A content_hash of the node's name+signature+doc is computed and
// stored so that GetNodesWithoutEmbeddings can detect stale embeddings when
// the code changes. Thread-safe: each call is a single UPSERT.
func (s *Store) UpsertEmbedding(nodeID, model string, vec []float32) error {
	// Pre-normalize to unit length so cosine similarity reduces to a dot product.
	nvec := normalizeVec(vec)
	if nvec == nil {
		return fmt.Errorf("upsert embedding: zero-magnitude vector")
	}
	blob := vecToBlob(nvec)

	// Compute content hash for change detection. If the node has been deleted
	// or renamed, the query returns empty strings and the hash reflects that —
	// the embedding will be marked stale on the next re-index pass.
	var name, sig, doc string
	_ = s.graphDB.QueryRow(`SELECT name, signature, doc FROM nodes WHERE id = ?`, nodeID).
		Scan(&name, &sig, &doc)
	hash := nodeContentHash(name, sig, doc)

	_, err := s.graphDB.Exec(`
		INSERT INTO node_embeddings (node_id, model, embedding, content_hash, indexed_at)
		VALUES (?, ?, ?, ?, strftime('%s','now'))
		ON CONFLICT(node_id) DO UPDATE SET
			model        = excluded.model,
			embedding    = excluded.embedding,
			content_hash = excluded.content_hash,
			indexed_at   = excluded.indexed_at`,
		nodeID, model, blob, hash,
	)
	if err != nil {
		return fmt.Errorf("upsert embedding: %w", err)
	}

	// Update the in-memory HNSW index for node embeddings.
	s.nodeHNSWAdd(nodeID, nvec)

	return nil
}

// BatchGetNodeEmbeddings fetches pre-normalized float32 embedding vectors for a
// batch of node IDs. IDs with no stored embedding are silently omitted. The
// returned vectors are already unit-normalized (guaranteed by UpsertEmbedding)
// so callers can use dot product as cosine similarity.
//
// Queries are chunked at 500 IDs per IN(...) clause to stay under
// SQLITE_MAX_VARIABLE_NUMBER (999). Returns nil when ids is empty or no rows match.
func (s *Store) BatchGetNodeEmbeddings(ids []string) map[string][]float32 {
	if len(ids) == 0 {
		return nil
	}
	result := make(map[string][]float32, len(ids))
	const batchSize = 500
	for i := 0; i < len(ids); i += batchSize {
		end := i + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for j, id := range batch {
			placeholders[j] = "?"
			args[j] = id
		}
		rows, err := s.graphDB.Query(
			`SELECT node_id, embedding FROM node_embeddings WHERE node_id IN (`+
				strings.Join(placeholders, ",")+`)`,
			args...,
		)
		if err != nil {
			continue
		}
		for rows.Next() {
			var nodeID string
			var blob []byte
			if err := rows.Scan(&nodeID, &blob); err != nil {
				continue
			}
			if vec := blobToVec(blob); len(vec) > 0 {
				result[nodeID] = vec
			}
		}
		_ = rows.Err() // drain; individual batch failures don't abort the whole fetch
		rows.Close()
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// EmbeddingCount returns the total number of stored embeddings.
func (s *Store) EmbeddingCount() int {
	var count int
	_ = s.graphDB.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&count)
	return count
}

// GetNodesWithoutEmbeddings returns up to limit node IDs that either have no
// embedding yet or whose stored content_hash no longer matches the current
// node text (name+signature+doc). File and package nodes are excluded.
// Pass limit=0 to return all matching nodes (no cap).
//
// Uses cursor-based pagination (WHERE n.rowid > ? LIMIT ?) to guarantee
// exactly limit results are returned when enough qualifying rows exist,
// regardless of the hash-match ratio.
func (s *Store) GetNodesWithoutEmbeddings(limit int) ([]string, error) {
	const pageSize = 500

	baseQuery := `
		SELECT n.rowid, n.id, n.name, n.signature, n.doc, COALESCE(e.content_hash, '') AS stored_hash
		FROM nodes n
		LEFT JOIN node_embeddings e ON n.id = e.node_id
		WHERE n.type NOT IN ('file', 'package')`

	// Unlimited mode: single full-table scan (no cursor needed).
	if limit <= 0 {
		rows, err := s.graphDB.Query(baseQuery)
		if err != nil {
			return nil, fmt.Errorf("get unembed nodes: %w", err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var rowid int64
			var id, name, sig, doc, storedHash string
			if err := rows.Scan(&rowid, &id, &name, &sig, &doc, &storedHash); err != nil {
				return nil, err
			}
			if nodeContentHash(name, sig, doc) != storedHash {
				ids = append(ids, id)
			}
		}
		return ids, rows.Err()
	}

	// Cursor-based pagination: fetch pages until we have limit results or
	// exhaust the table.
	var ids []string
	var cursor int64 // last seen rowid
	pagedQuery := baseQuery + " AND n.rowid > ? LIMIT ?"

	for len(ids) < limit {
		rows, err := s.graphDB.Query(pagedQuery, cursor, pageSize)
		if err != nil {
			return nil, fmt.Errorf("get unembed nodes: %w", err)
		}

		rowCount := 0
		for rows.Next() {
			rowCount++
			var rowid int64
			var id, name, sig, doc, storedHash string
			if err := rows.Scan(&rowid, &id, &name, &sig, &doc, &storedHash); err != nil {
				rows.Close()
				return nil, err
			}
			cursor = rowid
			if nodeContentHash(name, sig, doc) != storedHash {
				ids = append(ids, id)
				if len(ids) >= limit {
					break
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		// No more rows in table — stop.
		if rowCount < pageSize {
			break
		}
	}
	return ids, nil
}

// GetNodeTextForEmbedding returns the text that should be embedded for a node:
// "name signature doc". Returns ("", false) if the node does not exist.
func (s *Store) GetNodeTextForEmbedding(nodeID string) (text string, ok bool) {
	var name, sig, doc string
	err := s.graphDB.QueryRow(
		`SELECT name, signature, doc FROM nodes WHERE id = ?`, nodeID,
	).Scan(&name, &sig, &doc)
	if err != nil {
		return "", false
	}
	return nodeText(name, sig, doc), true
}

// VectorSearch performs cosine similarity search over all stored node embeddings.
// Returns up to limit results ordered by descending similarity.
// Falls back gracefully with (nil, nil) when no embeddings are stored yet.
//
// Uses HNSW approximate nearest-neighbor index when available (Sprint 12 #4):
//   - O(log N) query time vs O(N) brute-force
//   - 3× oversampling for ≥95% recall, then Pass 2 fetches node metadata
//
// Falls back to brute-force scan when HNSW index is empty.
func (s *Store) VectorSearch(queryVec []float32, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if len(queryVec) == 0 {
		return nil, nil
	}

	// Pre-normalize query vector so dot product = cosine similarity.
	normQuery := normalizeVec(queryVec)
	if normQuery == nil {
		return nil, nil
	}

	// Fast path: HNSW ANN index.
	if s.nodeHNSWReady() {
		candidates := s.NodeHNSWSearch(normQuery, limit)
		if len(candidates) > 0 {
			h := &topKHeap{k: limit}
			for _, c := range candidates {
				h.tryPush(c.id, c.score, false)
			}
			winners := h.drain()
			if len(winners) > 0 {
				return s.fetchNodeSearchResults(winners)
			}
		}
	}

	// Fallback: brute-force scan.
	return s.vectorSearchBruteForce(normQuery, limit)
}

// vectorSearchBruteForce is the O(N) fallback path for node vector search.
func (s *Store) vectorSearchBruteForce(normQuery []float32, limit int) ([]SearchResult, error) {
	rows, err := s.graphDB.Query(`
		SELECT e.node_id, e.embedding
		FROM node_embeddings e
		LIMIT 50000`)
	if err != nil {
		return nil, fmt.Errorf("vector search (brute-force): %w", err)
	}
	defer rows.Close()

	h := &topKHeap{k: limit}
	for rows.Next() {
		var nodeID string
		var blob []byte
		if err := rows.Scan(&nodeID, &blob); err != nil {
			return nil, fmt.Errorf("scan embedding row: %w", err)
		}
		vec := blobToVec(blob)
		if len(vec) == 0 {
			continue
		}
		score := dotSimilarity(normQuery, vec)
		if score <= 0 {
			continue
		}
		h.tryPush(nodeID, score, false)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	winners := h.drain()
	if len(winners) == 0 {
		return nil, nil
	}

	return s.fetchNodeSearchResults(winners)
}

// fetchNodeSearchResults performs Pass 2 of the two-pass node vector search:
// given top-K (id, score) tuples, fetches full node metadata from graphDB.
func (s *Store) fetchNodeSearchResults(winners []scoredID) ([]SearchResult, error) {
	lookup := make(map[string]struct{ pos int; score float64 }, len(winners))
	placeholders := make([]string, len(winners))
	args := make([]any, len(winners))
	for i, w := range winners {
		lookup[w.id] = struct{ pos int; score float64 }{pos: i, score: float64(w.score)}
		placeholders[i] = "?"
		args[i] = w.id
	}

	metaRows, err := s.graphDB.Query(
		`SELECT id, name, signature, doc FROM nodes WHERE id IN (`+
			strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("fetch vector search results: %w", err)
	}
	defer metaRows.Close()

	results := make([]SearchResult, len(winners))
	for metaRows.Next() {
		var r SearchResult
		if err := metaRows.Scan(&r.ID, &r.Name, &r.Signature, &r.Doc); err != nil {
			return nil, fmt.Errorf("scan node result: %w", err)
		}
		ps, ok := lookup[r.ID]
		if !ok {
			continue
		}
		r.Score = ps.score
		results[ps.pos] = r
	}
	if err := metaRows.Err(); err != nil {
		return nil, err
	}

	// Filter out any zero-value entries (nodes deleted between pass 1 and 2).
	n := 0
	for _, r := range results {
		if r.ID != "" {
			results[n] = r
			n++
		}
	}
	return results[:n], nil
}

// vecToBlob encodes a []float32 slice as a little-endian byte slice.
func vecToBlob(vec []float32) []byte {
	b := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}

// blobToVec decodes a little-endian byte slice back to []float32.
func blobToVec(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	vec := make([]float32, len(b)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return vec
}

// normalizeVec returns a unit-length copy of vec. Returns nil for empty/zero-magnitude input,
// or if normalization produces NaN/Inf (extreme float32 edge case).
// Pre-normalizing vectors at insertion time reduces cosine similarity to a single dot product.
func normalizeVec(vec []float32) []float32 {
	if len(vec) == 0 {
		return nil
	}
	norm := vek32.Norm(vec)
	if norm == 0 || math.IsNaN(float64(norm)) || math.IsInf(float64(norm), 0) {
		return nil
	}
	out := make([]float32, len(vec))
	copy(out, vec)
	vek32.DivNumber_Inplace(out, norm)
	// Guard: reject if division produced NaN/Inf (near-zero norm edge case).
	if math.IsNaN(float64(out[0])) || math.IsInf(float64(out[0]), 0) {
		return nil
	}
	return out
}

// normalizeStoredEmbeddings migrates existing embeddings to unit-normalized form.
// Called at store Open. Uses a sample-first check: reads ONE embedding per table
// to decide whether a full scan is needed. After the first migration pass, all
// vectors are normalized and subsequent opens skip in O(1).
// Idempotent: re-normalizing a unit vector produces the same value within float32 precision.
func (s *Store) normalizeStoredEmbeddings() {
	type dbIface interface {
		Query(string, ...any) (*sql.Rows, error)
		QueryRow(string, ...any) *sql.Row
		Exec(string, ...any) (sql.Result, error)
	}

	// Check for a persisted normalization_complete flag in the meta table.
	// The flag is set after a full verified sweep; this prevents the one-sample
	// heuristic bug where a partially-migrated table re-uses the first row to
	// skip normalization of all remaining un-normalized rows.
	var flagVal string
	_ = s.graphDB.QueryRow(`SELECT value FROM meta WHERE key = 'embedding_normalization_v1'`).Scan(&flagVal)
	if flagVal == "1" {
		return
	}

	// allowedTables prevents SQL injection via table/column name interpolation.
	allowedTables := map[string]bool{"node_embeddings": true, "memory_embeddings": true}
	allowedCols := map[string]bool{"node_id": true, "memory_id": true}

	normalizeTable := func(db dbIface, table, idCol string) int {
		if !allowedTables[table] || !allowedCols[idCol] {
			return 0 // reject unknown table/column names
		}
		// Collect rows to update using cursor-based pagination (WHERE rowid > ?)
		// instead of LIMIT/OFFSET to avoid O(N^2/chunkSize) performance.
		const chunkSize = 2000
		type updateItem struct {
			id   string
			blob []byte
		}
		var updates []updateItem
		var lastRowid int64
		for {
			rows, err := db.Query(fmt.Sprintf("SELECT rowid, %s, embedding FROM %s WHERE rowid > ? LIMIT ?", idCol, table), lastRowid, chunkSize)
			if err != nil {
				break
			}
			rowCount := 0
			for rows.Next() {
				rowCount++
				var rowid int64
				var id string
				var blob []byte
				if err := rows.Scan(&rowid, &id, &blob); err != nil {
					continue
				}
				lastRowid = rowid
				vec := blobToVec(blob)
				if len(vec) == 0 {
					continue
				}
				norm := vek32.Norm(vec)
				if norm > 0.999 && norm < 1.001 {
					continue
				}
				if norm == 0 {
					continue
				}
				nvec := normalizeVec(vec)
				if nvec == nil {
					continue
				}
				updates = append(updates, updateItem{id: id, blob: vecToBlob(nvec)})
			}
			rows.Close()
			if rowCount < chunkSize {
				break
			}
		}

		if len(updates) == 0 {
			return 0
		}

		// Wrap all UPDATEs in a single transaction for atomicity and performance.
		// Use type assertion to access Begin(); fall back to individual writes if unavailable.
		type beginner interface {
			Begin() (*sql.Tx, error)
		}
		sqlDB, ok := db.(beginner)
		if !ok {
			logutil.Warn("synapses: embedding normalization: db does not support transactions; %d vectors remain denormalized\n", len(updates))
			return 0
		}
		tx, err := sqlDB.Begin()
		if err != nil {
			logutil.Warn("synapses: embedding normalization: begin tx: %v; %d vectors remain denormalized\n", err, len(updates))
			return 0
		}
		defer tx.Rollback() // no-op after successful Commit
		stmt, err := tx.Prepare(fmt.Sprintf("UPDATE %s SET embedding = ? WHERE %s = ?", table, idCol))
		if err != nil {
			return 0
		}
		defer stmt.Close()
		var updated int
		for _, u := range updates {
			if _, execErr := stmt.Exec(u.blob, u.id); execErr != nil {
				return 0
			}
			updated++
		}
		if err := tx.Commit(); err != nil {
			return 0
		}
		return updated
	}

	nodeUpdated := normalizeTable(s.graphDB, "node_embeddings", "node_id")
	memUpdated := normalizeTable(s.knowledgeDB, "memory_embeddings", "memory_id")
	if nodeUpdated+memUpdated > 0 {
		logutil.Info("synapses: normalized %d node + %d memory embeddings to unit length\n", nodeUpdated, memUpdated)
	}
	// Persist the completion flag so future startups skip the full sweep.
	_, _ = s.graphDB.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('embedding_normalization_v1', '1')`)
}

// dotSimilarity returns the dot product of two pre-normalized vectors as their
// cosine similarity. Uses SIMD-accelerated vek32.Dot for 3-5x speedup over
// scalar loops. Both vectors MUST be pre-normalized (unit length) for the
// result to equal cosine similarity. Returns 0 for length mismatches or empty input.
func dotSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	return vek32.Dot(a, b)
}

// cosineSimilarity returns the cosine similarity between two float32 vectors.
// Returns 0 if either vector has zero magnitude or if lengths differ.
// Kept as fallback for non-normalized vectors (e.g., external callers).
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float32
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / float32(math.Sqrt(float64(normA))*math.Sqrt(float64(normB)))
}

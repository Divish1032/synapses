package store

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
)

// UpsertEmbedding stores or replaces the vector embedding for a graph node.
// vec is encoded as a little-endian float32 BLOB. model is the Ollama model
// name used to generate the embedding (needed for cache invalidation when the
// model changes). Thread-safe: each call is a single UPSERT.
func (s *Store) UpsertEmbedding(nodeID, model string, vec []float32) error {
	blob := vecToBlob(vec)
	_, err := s.db.Exec(`
		INSERT INTO node_embeddings (node_id, model, embedding, indexed_at)
		VALUES (?, ?, ?, strftime('%s','now'))
		ON CONFLICT(node_id) DO UPDATE SET
			model      = excluded.model,
			embedding  = excluded.embedding,
			indexed_at = excluded.indexed_at`,
		nodeID, model, blob,
	)
	if err != nil {
		return fmt.Errorf("upsert embedding: %w", err)
	}
	return nil
}

// EmbeddingCount returns the total number of stored embeddings.
func (s *Store) EmbeddingCount() int {
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM node_embeddings`).Scan(&count)
	return count
}

// GetNodesWithoutEmbeddings returns up to limit node IDs that exist in the
// nodes table but have no corresponding row in node_embeddings. File and
// package nodes are excluded — only functions, methods, and structs get embedded.
func (s *Store) GetNodesWithoutEmbeddings(limit int) ([]string, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`
		SELECT n.id
		FROM nodes n
		LEFT JOIN node_embeddings e ON n.id = e.node_id
		WHERE e.node_id IS NULL
		  AND n.type NOT IN ('file', 'package')
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unembed nodes: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetNodeTextForEmbedding returns the text that should be embedded for a node:
// "name signature doc". Returns ("", false) if the node does not exist.
func (s *Store) GetNodeTextForEmbedding(nodeID string) (text string, ok bool) {
	var name, sig, doc string
	err := s.db.QueryRow(
		`SELECT name, signature, doc FROM nodes WHERE id = ?`, nodeID,
	).Scan(&name, &sig, &doc)
	if err != nil {
		return "", false
	}
	// Concatenate non-empty fields with spaces to form a rich embedding input.
	parts := []string{name}
	if sig != "" {
		parts = append(parts, sig)
	}
	if doc != "" {
		parts = append(parts, doc)
	}
	return strings.Join(parts, " "), true
}

// VectorSearch performs brute-force cosine similarity search over all stored
// embeddings. Returns up to limit results ordered by descending similarity.
// Falls back gracefully with (nil, nil) when no embeddings are stored yet.
func (s *Store) VectorSearch(queryVec []float32, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if len(queryVec) == 0 {
		return nil, nil
	}

	rows, err := s.db.Query(`
		SELECT e.node_id, n.name, n.signature, n.doc, e.embedding
		FROM node_embeddings e
		JOIN nodes n ON e.node_id = n.id`)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		result SearchResult
		score  float32
	}
	var candidates []candidate

	for rows.Next() {
		var r SearchResult
		var blob []byte
		if err := rows.Scan(&r.ID, &r.Name, &r.Signature, &r.Doc, &blob); err != nil {
			return nil, fmt.Errorf("scan embedding row: %w", err)
		}
		vec := blobToVec(blob)
		if len(vec) == 0 {
			continue
		}
		score := cosineSimilarity(queryVec, vec)
		candidates = append(candidates, candidate{r, score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}

	results := make([]SearchResult, len(candidates))
	for i, c := range candidates {
		results[i] = c.result
		results[i].Score = float64(c.score)
	}
	return results, nil
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

// cosineSimilarity returns the cosine similarity between two float32 vectors.
// Returns 0 if either vector has zero magnitude or if lengths differ.
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

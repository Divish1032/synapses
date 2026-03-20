package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// maxVectorScanCap is the maximum number of embedding rows loaded into memory
// during a brute-force cosine similarity scan. Prevents unbounded heap
// allocation (≈15 MB per 10K memories). Proper ANN fix ships in Sprint 10.
const maxVectorScanCap = 10_000

// MemorySearchResult represents a memory matched by vector similarity search.
type MemorySearchResult struct {
	MemoryID string  `json:"memory_id"`
	Content  string  `json:"content"`
	Tier     string  `json:"tier"`
	EntityID string  `json:"entity_id,omitempty"`
	Score    float64 `json:"score"` // cosine similarity, higher = more relevant
}

// memoryContentHash computes an 8-char hex hash of the memory content
// used to detect when a memory's embedding is out of date after content changes.
func memoryContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:4]) // 8 hex chars
}

// UpsertMemoryEmbedding stores or replaces the vector embedding for a memory.
// vec is encoded as a little-endian float32 BLOB. model is the model name
// used to generate the embedding (for cache invalidation when the model changes).
// A content_hash of the memory's content is computed and stored so that
// GetMemoriesWithoutEmbeddings can detect stale embeddings when memory content changes.
// Thread-safe: each call is a single UPSERT.
func (s *Store) UpsertMemoryEmbedding(memoryID, model string, vec []float32) error {
	if memoryID == "" {
		return fmt.Errorf("upsert memory embedding: empty memory_id")
	}
	if len(vec) == 0 {
		return fmt.Errorf("upsert memory embedding: empty vector")
	}
	blob := vecToBlob(vec)

	// Compute content hash for change detection.
	var content string
	_ = s.knowledgeDB.QueryRow(`SELECT content FROM memories WHERE id = ?`, memoryID).
		Scan(&content)
	hash := memoryContentHash(content)

	_, err := s.knowledgeDB.Exec(`
		INSERT INTO memory_embeddings (memory_id, model, embedding, content_hash, stale, embedded_at)
		VALUES (?, ?, ?, ?, 0, ?)
		ON CONFLICT(memory_id) DO UPDATE SET
			model        = excluded.model,
			embedding    = excluded.embedding,
			content_hash = excluded.content_hash,
			stale        = 0,
			embedded_at  = excluded.embedded_at`,
		memoryID, model, blob, hash, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("upsert memory embedding: %w", err)
	}
	return nil
}

// MemoryEmbeddingCount returns the total number of stored memory embeddings.
func (s *Store) MemoryEmbeddingCount() int {
	var count int
	_ = s.knowledgeDB.QueryRow(`SELECT COUNT(*) FROM memory_embeddings`).Scan(&count)
	return count
}

// GetMemoriesWithoutEmbeddings returns up to limit memory IDs that either have no
// embedding yet or whose stored content_hash no longer matches the current memory
// content. Only non-expired, non-stale memories are returned.
// Pass limit=0 to return all matching IDs (no cap).
func (s *Store) GetMemoriesWithoutEmbeddings(limit int) ([]string, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	rows, err := s.knowledgeDB.Query(`
		SELECT m.id, m.content, COALESCE(e.content_hash, '') AS stored_hash
		FROM memories m
		LEFT JOIN memory_embeddings e ON m.id = e.memory_id
		WHERE m.expires_at > ?
		  AND m.stale = 0`, now)
	if err != nil {
		return nil, fmt.Errorf("get memories without embeddings: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id, content, storedHash string
		if err := rows.Scan(&id, &content, &storedHash); err != nil {
			return nil, err
		}
		// Include memories with missing embeddings (storedHash=="") or stale ones
		// (current content hash differs from what was stored at embedding time).
		if memoryContentHash(content) != storedHash {
			ids = append(ids, id)
			if limit > 0 && len(ids) >= limit {
				break
			}
		}
	}
	return ids, rows.Err()
}

// GetMemoryTextForEmbedding returns the text content that should be embedded
// for a memory. Returns ("", false) if the memory does not exist or is expired/stale.
func (s *Store) GetMemoryTextForEmbedding(memoryID string) (string, bool) {
	now := time.Now().UTC().Format(time.RFC3339)
	var content string
	err := s.knowledgeDB.QueryRow(
		`SELECT content FROM memories WHERE id = ? AND expires_at > ? AND stale = 0`,
		memoryID, now,
	).Scan(&content)
	if err != nil {
		return "", false
	}
	return content, true
}

// MarkMemoryEmbeddingsStale sets stale=1 on embeddings for the given memory IDs.
// This is the foundation for Sprint 10.7 (graph-anchored embedding invalidation):
// when a file watcher detects an entity change, embeddings of memories anchored to
// that entity are marked stale. On next recall(), stale embeddings are re-embedded
// before scoring. Idempotent. A no-op when memoryIDs is empty.
// Processes in batches of 500 to respect SQLite variable limits.
func (s *Store) MarkMemoryEmbeddingsStale(memoryIDs []string) error {
	if len(memoryIDs) == 0 {
		return nil
	}
	const batchSize = 500
	for i := 0; i < len(memoryIDs); i += batchSize {
		end := i + batchSize
		if end > len(memoryIDs) {
			end = len(memoryIDs)
		}
		batch := memoryIDs[i:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(batch))
		for j, id := range batch {
			args[j] = id
		}
		if _, err := s.knowledgeDB.Exec(
			`UPDATE memory_embeddings SET stale = 1 WHERE memory_id IN (`+placeholders+`)`,
			args...,
		); err != nil {
			return fmt.Errorf("mark memory embeddings stale: %w", err)
		}
	}
	return nil
}

// DeleteMemoryEmbeddings removes embeddings for the given memory IDs.
// Called during memory expiry cleanup. A no-op when memoryIDs is empty.
// Processes in batches of 500 to respect SQLite variable limits.
func (s *Store) DeleteMemoryEmbeddings(memoryIDs []string) error {
	if len(memoryIDs) == 0 {
		return nil
	}
	const batchSize = 500
	for i := 0; i < len(memoryIDs); i += batchSize {
		end := i + batchSize
		if end > len(memoryIDs) {
			end = len(memoryIDs)
		}
		batch := memoryIDs[i:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(batch))
		for j, id := range batch {
			args[j] = id
		}
		if _, err := s.knowledgeDB.Exec(
			`DELETE FROM memory_embeddings WHERE memory_id IN (`+placeholders+`)`,
			args...,
		); err != nil {
			return fmt.Errorf("delete memory embeddings: %w", err)
		}
	}
	return nil
}

// MemoryVectorSearch performs brute-force cosine similarity search over memory
// embeddings. Returns up to limit results ordered by descending similarity.
// Only non-expired, non-stale memories are included. Stale embeddings (stale=1)
// are excluded from results — they need re-embedding first.
// Falls back gracefully with (nil, nil) when no embeddings are stored yet.
func (s *Store) MemoryVectorSearch(queryVec []float32, limit int) ([]MemorySearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if len(queryVec) == 0 {
		return nil, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.knowledgeDB.Query(`
		SELECT e.memory_id, m.content, m.tier, m.entity_id, e.embedding
		FROM memory_embeddings e
		JOIN memories m ON e.memory_id = m.id
		WHERE e.stale = 0
		  AND m.stale = 0
		  AND m.expires_at > ?
		LIMIT 10000`, now)
	if err != nil {
		return nil, fmt.Errorf("memory vector search: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		result MemorySearchResult
		score  float32
	}
	var candidates []candidate

	scanned := 0
	for rows.Next() {
		scanned++
		var r MemorySearchResult
		var blob []byte
		if err := rows.Scan(&r.MemoryID, &r.Content, &r.Tier, &r.EntityID, &blob); err != nil {
			return nil, fmt.Errorf("scan memory embedding row: %w", err)
		}
		vec := blobToVec(blob)
		if len(vec) == 0 {
			continue
		}
		score := cosineSimilarity(queryVec, vec)
		if score <= 0 {
			continue // skip negatively or zero correlated results
		}
		candidates = append(candidates, candidate{r, score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if scanned == maxVectorScanCap {
		s.vectorCapWarnOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "WARN: synapses: vector search cap triggered (%d embeddings scanned); results may be incomplete. Upgrade to sqlite-vec ANN (Sprint 10) for full recall.\n", maxVectorScanCap)
		})
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

	results := make([]MemorySearchResult, len(candidates))
	for i, c := range candidates {
		results[i] = c.result
		results[i].Score = float64(c.score)
	}
	return results, nil
}

// MemoryVectorSearchWithThreshold performs brute-force cosine similarity search
// with a minimum similarity threshold. Results below the threshold are excluded.
// Useful for recall() where low-confidence matches should not pollute results.
// Unlike MemoryVectorSearch, this scans all embeddings before applying the limit
// so threshold filtering does not miss valid matches ranked beyond an arbitrary cutoff.
func (s *Store) MemoryVectorSearchWithThreshold(queryVec []float32, limit int, minScore float64) ([]MemorySearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if len(queryVec) == 0 {
		return nil, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.knowledgeDB.Query(`
		SELECT e.memory_id, m.content, m.tier, m.entity_id, e.embedding
		FROM memory_embeddings e
		JOIN memories m ON e.memory_id = m.id
		WHERE e.stale = 0
		  AND m.stale = 0
		  AND m.expires_at > ?
		LIMIT 10000`, now)
	if err != nil {
		return nil, fmt.Errorf("memory vector search with threshold: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		result MemorySearchResult
		score  float32
	}
	var candidates []candidate

	threshold := float32(minScore)
	scanned := 0
	for rows.Next() {
		scanned++
		var r MemorySearchResult
		var blob []byte
		if err := rows.Scan(&r.MemoryID, &r.Content, &r.Tier, &r.EntityID, &blob); err != nil {
			return nil, fmt.Errorf("scan memory embedding row: %w", err)
		}
		vec := blobToVec(blob)
		if len(vec) == 0 {
			continue
		}
		score := cosineSimilarity(queryVec, vec)
		if score < threshold {
			continue
		}
		candidates = append(candidates, candidate{r, score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if scanned == maxVectorScanCap {
		s.vectorCapWarnOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "WARN: synapses: vector search cap triggered (%d embeddings scanned); results may be incomplete. Upgrade to sqlite-vec ANN (Sprint 10) for full recall.\n", maxVectorScanCap)
		})
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

	results := make([]MemorySearchResult, len(candidates))
	for i, c := range candidates {
		results[i] = c.result
		results[i].Score = float64(c.score)
	}
	return results, nil
}

// memoryEmbeddingDimensions returns the dimensionality of stored embeddings
// by sampling the first row. Returns 0 if no embeddings exist.
// This is used for validation when performing vector search.
func (s *Store) memoryEmbeddingDimensions() int {
	var blob []byte
	err := s.knowledgeDB.QueryRow(`SELECT embedding FROM memory_embeddings LIMIT 1`).Scan(&blob)
	if err != nil {
		return 0
	}
	// Each float32 is 4 bytes.
	return len(blob) / 4
}


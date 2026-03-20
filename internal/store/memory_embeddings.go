package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// MemorySearchResult represents a memory matched by vector similarity search.
type MemorySearchResult struct {
	MemoryID       string  `json:"memory_id"`
	Content        string  `json:"content"`
	Tier           string  `json:"tier"`
	EntityID       string  `json:"entity_id,omitempty"`
	Score          float64 `json:"score"`                      // cosine similarity, higher = more relevant
	StaleEmbedding bool    `json:"stale_embedding,omitempty"`  // true when anchored entity changed since embedding was computed
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

// GetMemoryIDsByAnchorNodes returns the IDs of non-stale, non-expired memories
// that are anchored to ANY of the given node IDs via the memory_anchors table.
// Used by the file watcher to cheaply find which memory embeddings to invalidate
// after a node changes — we only need IDs, not full Memory structs.
// Processes in batches of 500 to respect SQLite variable limits.
// Returns (nil, nil) when nodeIDs is empty.
func (s *Store) GetMemoryIDsByAnchorNodes(nodeIDs []string, limit int) ([]string, error) {
	if len(nodeIDs) == 0 || limit <= 0 {
		return nil, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	seen := make(map[string]struct{})
	var result []string

	const batchSize = 500
	for i := 0; i < len(nodeIDs) && len(result) < limit; i += batchSize {
		end := i + batchSize
		if end > len(nodeIDs) {
			end = len(nodeIDs)
		}
		batch := nodeIDs[i:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, len(batch)+1)
		for _, id := range batch {
			args = append(args, id)
		}
		args = append(args, now)
		rows, err := s.knowledgeDB.Query(`
			SELECT DISTINCT ma.memory_id
			FROM memory_anchors ma
			JOIN memories m ON ma.memory_id = m.id
			WHERE ma.node_id IN (`+placeholders+`)
			  AND m.stale = 0
			  AND m.expires_at > ?`, args...)
		if err != nil {
			return nil, fmt.Errorf("get memory ids by anchor nodes: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan anchor memory id: %w", err)
			}
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				result = append(result, id)
				if len(result) >= limit {
					break
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return result, nil
}

// GetStaleEmbeddingMemoryIDs returns memory IDs whose embeddings are stale
// (stale=1 in memory_embeddings) but whose memory records are still valid
// (non-stale, non-expired). Up to limit IDs are returned.
// Used by the semantic recall channel to drive lazy re-embedding: stale
// embeddings are refreshed just before the vector search runs so they
// participate in scoring instead of being silently excluded.
func (s *Store) GetStaleEmbeddingMemoryIDs(limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.knowledgeDB.Query(`
		SELECT me.memory_id
		FROM memory_embeddings me
		JOIN memories m ON me.memory_id = m.id
		WHERE me.stale = 1
		  AND m.stale = 0
		  AND m.expires_at > ?
		ORDER BY me.embedded_at DESC
		LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("get stale embedding memory ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stale embedding id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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

// MemoryVectorSearch performs cosine similarity search over memory embeddings.
// Returns up to limit results ordered by descending similarity.
// Only non-expired, non-stale memories are included. Stale embeddings (stale=1)
// are excluded from results — they need re-embedding first.
// Falls back gracefully with (nil, nil) when no embeddings are stored yet.
//
// Uses a two-pass approach for memory efficiency:
//
//	Pass 1: Scan only (memory_id, embedding) with a min-heap of size K to
//	         select the top-K candidates. Content is NOT loaded during the scan,
//	         keeping peak memory at O(K) instead of O(N).
//	Pass 2: Fetch full memory data (content, tier, entity_id) only for the
//	         K winning candidates.
func (s *Store) MemoryVectorSearch(queryVec []float32, limit int) ([]MemorySearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if len(queryVec) == 0 {
		return nil, nil
	}

	// Pass 1: Lightweight scan — IDs, embeddings, and stale flag.
	// Stale embeddings (e.stale=1) are INCLUDED in scoring — their vector
	// is still valid (memory text unchanged). StaleEmbedding flag is
	// propagated to results so agents know the anchored entity changed.
	// Dead memories (m.stale=1) are still excluded — different concept.
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.knowledgeDB.Query(`
		SELECT e.memory_id, e.embedding, e.stale
		FROM memory_embeddings e
		JOIN memories m ON e.memory_id = m.id
		WHERE m.stale = 0
		  AND m.expires_at > ?
		ORDER BY e.rowid DESC
		LIMIT 10000`, now)
	if err != nil {
		return nil, fmt.Errorf("memory vector search: %w", err)
	}
	defer rows.Close()

	h := &topKHeap{k: limit}
	for rows.Next() {
		var memID string
		var blob []byte
		var embStale int
		if err := rows.Scan(&memID, &blob, &embStale); err != nil {
			return nil, fmt.Errorf("scan memory embedding row: %w", err)
		}
		vec := blobToVec(blob)
		if len(vec) == 0 {
			continue
		}
		score := cosineSimilarity(queryVec, vec)
		if score <= 0 {
			continue
		}
		h.tryPush(memID, score, embStale == 1)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	winners := h.drain()
	if len(winners) == 0 {
		return nil, nil
	}

	// Pass 2: Fetch content for winners only.
	return s.fetchMemorySearchResults(winners)
}

// MemoryVectorSearchWithThreshold performs cosine similarity search with a
// minimum similarity threshold. Results below the threshold are excluded.
// Useful for recall() where low-confidence matches should not pollute results.
//
// Uses the same two-pass approach as MemoryVectorSearch: lightweight scan with
// min-heap, then content fetch for winners only. The threshold is applied
// during the scan so sub-threshold candidates never enter the heap.
func (s *Store) MemoryVectorSearchWithThreshold(queryVec []float32, limit int, minScore float64) ([]MemorySearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if len(queryVec) == 0 {
		return nil, nil
	}

	// Pass 1: Lightweight scan with threshold filter.
	// Stale embeddings included — see MemoryVectorSearch comment.
	// LIMIT 10000: safety cap so heap allocation stays bounded on large corpora.
	// ORDER BY e.rowid DESC: recent memories scanned first within the cap.
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.knowledgeDB.Query(`
		SELECT e.memory_id, e.embedding, e.stale
		FROM memory_embeddings e
		JOIN memories m ON e.memory_id = m.id
		WHERE m.stale = 0
		  AND m.expires_at > ?
		ORDER BY e.rowid DESC
		LIMIT 10000`, now)
	if err != nil {
		return nil, fmt.Errorf("memory vector search with threshold: %w", err)
	}
	defer rows.Close()

	threshold := float32(minScore)
	h := &topKHeap{k: limit}
	for rows.Next() {
		var memID string
		var blob []byte
		var embStale int
		if err := rows.Scan(&memID, &blob, &embStale); err != nil {
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
		h.tryPush(memID, score, embStale == 1)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	winners := h.drain()
	if len(winners) == 0 {
		return nil, nil
	}

	// Pass 2: Fetch content for winners only.
	return s.fetchMemorySearchResults(winners)
}

// fetchMemorySearchResults performs the second pass of the two-pass vector
// search: given the top-K (id, score, stale) tuples from the scan pass, it
// fetches the full memory content, tier, and entity_id in a single query.
// Results are returned in the same descending-score order as winners.
// The stale flag from Pass 1 is propagated to MemorySearchResult.StaleEmbedding.
//
// Re-applies expired filter to close the consistency gap between passes:
// a memory valid during Pass 1 could expire before Pass 2 executes.
// Memory-level stale (m.stale=1) is also re-checked — dead memories excluded.
func (s *Store) fetchMemorySearchResults(winners []scoredID) ([]MemorySearchResult, error) {
	// Build id → score+position+stale map for reassembly.
	type posScore struct {
		pos   int
		score float64
		stale bool
	}
	lookup := make(map[string]posScore, len(winners))
	placeholders := make([]string, len(winners))
	args := make([]any, len(winners)+1)
	now := time.Now().UTC().Format(time.RFC3339)
	args[0] = now
	for i, w := range winners {
		lookup[w.id] = posScore{pos: i, score: float64(w.score), stale: w.stale}
		placeholders[i] = "?"
		args[i+1] = w.id
	}

	rows, err := s.knowledgeDB.Query(
		`SELECT id, content, tier, entity_id FROM memories
		 WHERE stale = 0 AND expires_at > ?
		   AND id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("fetch memory search results: %w", err)
	}
	defer rows.Close()

	results := make([]MemorySearchResult, len(winners))
	for rows.Next() {
		var r MemorySearchResult
		if err := rows.Scan(&r.MemoryID, &r.Content, &r.Tier, &r.EntityID); err != nil {
			return nil, fmt.Errorf("scan memory result: %w", err)
		}
		ps, ok := lookup[r.MemoryID]
		if !ok {
			continue // should not happen
		}
		r.Score = ps.score
		r.StaleEmbedding = ps.stale
		results[ps.pos] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Filter out any zero-value entries (memories deleted between pass 1 and 2).
	n := 0
	for _, r := range results {
		if r.MemoryID != "" {
			results[n] = r
			n++
		}
	}
	return results[:n], nil
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

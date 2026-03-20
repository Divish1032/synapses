package store

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RecencyDecayScore computes a decay score based on age since creation.
// Returns a value in (0, 1] where 1.0 = just created, 0.5 = halfLifeHours old.
// Exported as a reusable utility for Sprint 10 #2 (knowledge decay scoring).
func RecencyDecayScore(createdAt time.Time, halfLifeHours float64) float64 {
	if halfLifeHours <= 0 {
		halfLifeHours = 168 // 1 week default
	}
	ageHours := time.Since(createdAt).Hours()
	if ageHours < 0 {
		ageHours = 0 // future timestamps treated as "just created"
	}
	return 1.0 / (1.0 + ageHours/halfLifeHours)
}

// RecentMemories returns the N most recent non-expired, non-stale memories
// regardless of text match. This is the data source for the temporal channel
// in quad-channel recall — it finds memories relevant by recency alone.
// sinceDays limits the lookback window (0 = 7 days default).
// When includeStale is true, stale memories are also returned.
func (s *Store) RecentMemories(limit, sinceDays int, includeStale bool) ([]Memory, error) {
	if limit <= 0 {
		limit = 20
	}
	if sinceDays <= 0 {
		sinceDays = 7 // default 1-week window for temporal channel
	}

	now := time.Now().UTC()
	cutoff := now.AddDate(0, 0, -sinceDays).Format(time.RFC3339)
	nowStr := now.Format(time.RFC3339)

	q := `SELECT id, tier, content, entity_id, agent_id, task_id, tags,
	             created_at, expires_at, last_accessed_at, source
	      FROM memories
	      WHERE created_at >= ?
	        AND expires_at > ?`
	args := []interface{}{cutoff, nowStr}

	if !includeStale {
		q += ` AND stale = 0`
	}

	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.knowledgeDB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("recent memories: %w", err)
	}
	defer rows.Close()

	return scanMemories(rows)
}

// GetAnchorNodesByFTSQuery finds distinct anchor node IDs of memories whose
// content matches the given FTS5 query. These node IDs seed the graph channel's
// BFS traversal — structurally-related entities are discovered via graph edges.
//
// Single SQL: memory_anchors JOIN memories JOIN memories_fts.
// Independent of the BM25 channel (different query path, different purpose).
// Returns at most limit node IDs. Returns (nil, nil) on empty/invalid query.
func (s *Store) GetAnchorNodesByFTSQuery(query string, limit int) ([]string, error) {
	if query == "" {
		return nil, nil
	}
	safeQuery := sanitizeFTSQuery(query)
	if safeQuery == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}

	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.knowledgeDB.Query(`
		SELECT DISTINCT ma.node_id
		FROM memory_anchors ma
		JOIN memories m ON ma.memory_id = m.id
		JOIN memories_fts f ON m.rowid = f.rowid
		WHERE memories_fts MATCH ?
		  AND m.stale = 0
		  AND m.expires_at > ?
		LIMIT ?`, safeQuery, now, limit)
	if err != nil {
		return nil, fmt.Errorf("get anchor nodes by FTS query: %w", err)
	}
	defer rows.Close()

	var nodeIDs []string
	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			return nil, fmt.Errorf("scan anchor node: %w", err)
		}
		nodeIDs = append(nodeIDs, nodeID)
	}
	return nodeIDs, rows.Err()
}

// GetMemoriesByAnchorNodes returns memories anchored to ANY of the given node IDs.
// Uses a batched IN-clause query (batches of 500 for SQLite variable limits).
// Only returns non-expired, non-stale memories. Ordered by created_at DESC.
// Deduplicates by memory ID across batches.
// When includeStale is true, stale memories are also returned.
func (s *Store) GetMemoriesByAnchorNodes(nodeIDs []string, limit int, includeStale bool) ([]Memory, error) {
	if len(nodeIDs) == 0 || limit <= 0 {
		return nil, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	seen := make(map[string]bool)
	var result []Memory

	const batchSize = 500
	for i := 0; i < len(nodeIDs); i += batchSize {
		end := i + batchSize
		if end > len(nodeIDs) {
			end = len(nodeIDs)
		}
		batch := nodeIDs[i:end]

		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)+1)
		for j, nid := range batch {
			placeholders[j] = "?"
			args = append(args, nid)
		}
		args = append(args, now) // for expires_at filter

		q := `SELECT DISTINCT m.id, m.tier, m.content, m.entity_id, m.agent_id, m.task_id, m.tags,
		             m.created_at, m.expires_at, m.last_accessed_at, m.source
		      FROM memories m
		      JOIN memory_anchors ma ON m.id = ma.memory_id
		      WHERE ma.node_id IN (` + strings.Join(placeholders, ",") + `)
		        AND m.expires_at > ?`

		if !includeStale {
			q += ` AND m.stale = 0`
		}
		q += ` ORDER BY m.created_at DESC`

		rows, err := s.knowledgeDB.Query(q, args...)
		if err != nil {
			return nil, fmt.Errorf("get memories by anchor nodes: %w", err)
		}

		mems, err := scanMemories(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}

		for _, m := range mems {
			if !seen[m.ID] {
				seen[m.ID] = true
				result = append(result, m)
				if len(result) >= limit {
					return result, nil
				}
			}
		}
	}

	return result, nil
}

// RRFMerge applies Reciprocal Rank Fusion across multiple ranked result lists.
// Each channel provides a list of memory IDs in ranked order (best first).
// score(d) = sum(1 / (k + rank_i(d))) where k=60 (standard RRF constant).
// Returns the top-N memory IDs sorted by fused score, along with the
// per-memory channel attribution (which channels contributed to each result).
//
// k=60 is the standard value from Cormack et al. (2009). It balances the
// contribution between channels — a rank-1 result scores 1/61 ≈ 0.0164,
// rank-10 scores 1/70 ≈ 0.0143. The flat curve ensures multi-channel
// presence matters more than exact rank within any single channel.
func RRFMerge(channels map[string][]string, limit int, k int) ([]string, map[string][]string) {
	if k <= 0 {
		k = 60
	}
	if limit <= 0 {
		limit = 20
	}

	type scored struct {
		id       string
		score    float64
		channels []string
	}

	scoreMap := make(map[string]*scored)

	for channelName, rankedIDs := range channels {
		for rank, id := range rankedIDs {
			s, ok := scoreMap[id]
			if !ok {
				s = &scored{id: id}
				scoreMap[id] = s
			}
			s.score += 1.0 / float64(k+rank+1) // rank is 0-indexed, RRF uses 1-indexed
			s.channels = append(s.channels, channelName)
		}
	}

	// Collect and sort by score descending.
	items := make([]scored, 0, len(scoreMap))
	for _, s := range scoreMap {
		items = append(items, *s)
	}

	// Sort by score DESC, then by ID for deterministic ordering on ties.
	sort.Slice(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].id < items[j].id
	})

	if len(items) > limit {
		items = items[:limit]
	}

	resultIDs := make([]string, len(items))
	attribution := make(map[string][]string, len(items))
	for i, item := range items {
		resultIDs[i] = item.id
		attribution[item.id] = item.channels
	}

	return resultIDs, attribution
}


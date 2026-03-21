package store

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RecencyDecayScore computes a decay score using ACT-R frequency-weighted
// power-law decay. Returns a value in (0, 1] where 1.0 = just created,
// 0.5 = one effective half-life old. The effective half-life grows
// logarithmically with access_count: a memory accessed 20 times decays
// ~4.4x slower than one accessed once.
//
// Formula: score = 1 / (1 + ageHours / effectiveHalfLife)
// where effectiveHalfLife = halfLifeHours × log2(max(accessCount, 1) + 1).
//
// Based on ACT-R base-level activation (Anderson & Lebiere, 1998).
// The log-frequency scaling is the core ACT-R insight: each additional
// access has diminishing returns on memory strength.
func RecencyDecayScore(createdAt time.Time, halfLifeHours float64, accessCount int) float64 {
	if halfLifeHours <= 0 {
		halfLifeHours = 168 // 1 week default
	}
	ageHours := time.Since(createdAt).Hours()
	if ageHours < 0 {
		ageHours = 0 // future timestamps treated as "just created"
	}

	// ACT-R frequency boost: effective half-life scales with log2(n+1).
	// n=0 or n=1 → multiplier = 1.0 (backward compatible with old formula).
	// n=20 → multiplier ≈ 4.39 (memory decays 4.39x slower).
	n := float64(accessCount)
	if n < 1 {
		n = 1
	}
	effectiveHalfLife := halfLifeHours * math.Log2(n+1)

	return 1.0 / (1.0 + ageHours/effectiveHalfLife)
}

// TierHalfLife returns the tier-specific decay half-life in hours.
// Different memory tiers decay at different rates:
//   - session_log: 72h (3 days) — ephemeral session summaries fade quickly
//   - project: 336h (2 weeks) — conventions and decisions persist longer
//   - entity + auto: 168h (1 week) — auto-captured code facts
//   - entity + manual: 504h (3 weeks) — manually annotated code facts persist longest
//
// Based on A-MAC (arXiv:2603.04549) differential decay rates.
func TierHalfLife(tier, source string) float64 {
	switch tier {
	case TierSessionLog:
		return 72
	case TierProject:
		return 336
	case TierEntity:
		if source == SourceManual {
			return 504
		}
		return 168
	default:
		return 168
	}
}

// DecayedImportanceScore combines memory importance weight with recency decay.
//
// Rules:
//   - ImportancePinned ("pinned"): returns 1.0. Pinned memories are exempt from
//     decay and always visible in recall results. Use for security configs,
//     compliance decisions, architectural invariants.
//   - Numeric string (e.g. "0.8"): parsed as the importance weight, then
//     multiplied by RecencyDecayScore(lastAccessedAt, halfLifeHours).
//   - Invalid or empty string: treated as weight 1.0 (pure recency decay).
//
// halfLifeHours controls how fast scores decay. 0 = use tier-specific defaults
// (session_log 72h, project 336h, entity+auto 168h, entity+manual 504h).
// Result is in (0, 1] for pinned=false, exactly 1.0 for pinned.
func DecayedImportanceScore(m Memory, halfLifeHours float64) float64 {
	if m.Importance == ImportancePinned {
		return 1.0
	}

	weight := 1.0
	if m.Importance != "" {
		if w, err := strconv.ParseFloat(m.Importance, 64); err == nil && w >= 0 {
			weight = w
		}
	}

	// Use last_accessed_at for the recency signal — memories that are actively
	// used (recalled, touched) maintain their score longer than memories that
	// were written once and never accessed. This rewards useful knowledge.
	accessedAt, err := time.Parse(time.RFC3339, m.LastAccessedAt)
	if err != nil || accessedAt.IsZero() {
		// Fallback: parse created_at. If both fail, treat as "just created".
		accessedAt, err = time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil || accessedAt.IsZero() {
			accessedAt = time.Now().UTC()
		}
	}

	if halfLifeHours <= 0 {
		halfLifeHours = TierHalfLife(m.Tier, m.Source)
	}
	return weight * RecencyDecayScore(accessedAt, halfLifeHours, m.AccessCount)
}

// DecayVisibilityThreshold is the minimum DecayedImportanceScore for a memory
// to be included in recall() results. Memories scoring below this threshold are
// demoted (excluded from results) but never deleted — they remain in the DB for
// audit queries (include_stale=true) and as_of temporal lookups.
//
// At 0.05, a default-importance memory (weight=1.0, halfLife=168h) decays below
// threshold after approximately 19 weeks (~4.5 months) without being accessed.
// Pinned memories always score 1.0 and are never demoted.
const DecayVisibilityThreshold = 0.05

// RecentMemories returns the N most recent non-expired, non-stale memories
// regardless of text match. This is the data source for the temporal channel
// in quad-channel recall — it finds memories relevant by recency alone.
// sinceDays limits the lookback window (0 = 7 days default).
// until optionally caps the upper bound on created_at (nil = no upper bound).
// When includeStale is true, stale memories are also returned.
func (s *Store) RecentMemories(limit, sinceDays int, until *time.Time, includeStale bool) ([]Memory, error) {
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
	             created_at, expires_at, last_accessed_at, source, importance, access_count
	      FROM memories
	      WHERE created_at >= ?
	        AND expires_at > ?`
	args := []interface{}{cutoff, nowStr}

	// Sprint 10.5: optional upper bound for time-bounded queries (since + until).
	if until != nil {
		q += ` AND created_at <= ?`
		args = append(args, until.UTC().Format(time.RFC3339))
	}

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
		             m.created_at, m.expires_at, m.last_accessed_at, m.source, m.importance, m.access_count
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
// RRFChannelWeights defines per-channel weight multipliers for RRF scoring.
// A weight of 1.0 means the channel contributes at full RRF strength.
// Lower weights reduce the channel's influence, preventing it from
// displacing results from stronger channels.
//
// Default weights (used when nil is passed):
//   - bm25: 1.0 (text relevance is the primary signal)
//   - semantic: 1.0 (conceptual similarity is equally weighted)
//   - graph: 1.0 (structural relationships are equally weighted)
//   - temporal: 0.5 (recency is a supplementary signal, not primary)
//
// The temporal weight of 0.5 ensures that at 1000+ memories, a temporal-only
// result (score ~0.008) never outranks a BM25 rank-2 result (score ~0.016).
// Temporal results still surface when other channels leave gaps.
var DefaultRRFWeights = map[string]float64{
	"bm25":     1.0,
	"semantic": 1.0,
	"graph":    1.0,
	"temporal": 0.5,
}

func RRFMerge(channels map[string][]string, limit int, k int) ([]string, map[string][]string) {
	return RRFMergeWeighted(channels, limit, k, nil)
}

// RRFMergeWeighted applies Reciprocal Rank Fusion with per-channel weights.
// weights maps channel name → weight multiplier (nil = DefaultRRFWeights).
// Channels not in the weights map get weight 1.0.
func RRFMergeWeighted(channels map[string][]string, limit int, k int, weights map[string]float64) ([]string, map[string][]string) {
	if k <= 0 {
		k = 60
	}
	if limit <= 0 {
		limit = 20
	}
	if weights == nil {
		weights = DefaultRRFWeights
	}

	type scored struct {
		id       string
		score    float64
		channels []string
	}

	scoreMap := make(map[string]*scored)

	for channelName, rankedIDs := range channels {
		w := 1.0
		if cw, ok := weights[channelName]; ok {
			w = cw
		}
		for rank, id := range rankedIDs {
			s, ok := scoreMap[id]
			if !ok {
				s = &scored{id: id}
				scoreMap[id] = s
			}
			s.score += w * (1.0 / float64(k+rank+1)) // rank is 0-indexed, RRF uses 1-indexed
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


package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// MemoryTier classifies the scope and lifespan of a memory.
const (
	TierSessionLog = "session_log" // What happened — auto-captured session summaries.
	TierEntity     = "entity"      // Facts about code nodes — travels with the entity.
	TierProject    = "project"     // Conventions, decisions, gotchas — project-wide.
)

// MemorySource indicates how the memory was created.
const (
	SourceManual    = "manual"    // Agent explicitly called remember() or annotate_node().
	SourceAuto      = "auto"      // Auto-captured by end_session structured extraction.
	SourceExtracted = "extracted" // LLM-synthesized from session data by brain sidecar.
)

// Base TTLs per tier. Entity memories live until the node dies in the graph.
const (
	ttlSessionLog = 90 * 24 * time.Hour // 90 days
	ttlProject    = 60 * 24 * time.Hour // 60 days
)

// Memory represents a single memory entry in the unified memories table.
type Memory struct {
	ID             string `json:"id"`
	Tier           string `json:"tier"`
	Content        string `json:"content"`
	EntityID       string `json:"entity_id,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
	Tags           string `json:"tags,omitempty"`           // JSON array string
	CreatedAt      string `json:"created_at"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	LastAccessedAt string `json:"last_accessed_at,omitempty"`
	Source         string `json:"source"`
}

// InvalidatedMemory is a stale memory surfaced once per agent at session start (AM-3).
// Per-agent tracking via memory_surfaced table ensures every agent sees each
// invalidation independently.
type InvalidatedMemory struct {
	ID            string `json:"id"`
	Content       string `json:"content"`
	Tier          string `json:"tier"`
	StaleReason   string `json:"stale_reason"`
	InvalidatedAt string `json:"invalidated_at"` // when the memory was invalidated (staled_at)
}

// InsertMemory writes a new memory, applying tier-based TTL and noise filtering.
// Returns the memory ID. Deduplicates against existing memories with similar content.
func (s *Store) InsertMemory(m Memory) (string, error) {
	m, deduped, err := s.prepareMemory(m)
	if err != nil {
		return "", err
	}
	if deduped != "" {
		_ = s.TouchMemory(deduped)
		return deduped, nil
	}

	_, err = s.knowledgeDB.Exec(`
		INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags,
		                      created_at, expires_at, last_accessed_at, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Tier, m.Content, m.EntityID, m.AgentID, m.TaskID, m.Tags,
		m.CreatedAt, m.ExpiresAt, m.LastAccessedAt, m.Source,
	)
	if err != nil {
		return "", fmt.Errorf("insert memory: %w", err)
	}
	return m.ID, nil
}

// QueryMemories retrieves memories matching the given filters.
// All filter params are optional (empty string = no filter applied for that field).
// NOTE: passing empty entityID does NOT filter by entity — it returns all entities.
// Use QueryMemoriesForEntities for multi-entity batched lookups.
func (s *Store) QueryMemories(tier, entityID, agentID string, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 10
	}

	now := time.Now().UTC().Format(time.RFC3339)
	q := `SELECT id, tier, content, entity_id, agent_id, task_id, tags,
	             created_at, expires_at, last_accessed_at, source
	      FROM memories WHERE expires_at > ? AND stale = 0`
	args := []interface{}{now}

	if tier != "" {
		q += ` AND tier = ?`
		args = append(args, tier)
	}
	if entityID != "" {
		q += ` AND entity_id = ?`
		args = append(args, entityID)
	}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}

	q += ` ORDER BY last_accessed_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.knowledgeDB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query memories: %w", err)
	}
	defer rows.Close()

	return scanMemories(rows)
}

// QueryMemoriesIncludingStale is like QueryMemories but returns both active and stale
// memories. Use for audit scenarios (e.g. recall(include_stale=true)) where the agent
// explicitly wants to see the full history including invalidated entries.
func (s *Store) QueryMemoriesIncludingStale(tier, entityID, agentID string, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 10
	}

	now := time.Now().UTC().Format(time.RFC3339)
	q := `SELECT id, tier, content, entity_id, agent_id, task_id, tags,
	             created_at, expires_at, last_accessed_at, source
	      FROM memories WHERE expires_at > ?`
	args := []interface{}{now}

	if tier != "" {
		q += ` AND tier = ?`
		args = append(args, tier)
	}
	if entityID != "" {
		q += ` AND entity_id = ?`
		args = append(args, entityID)
	}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}

	q += ` ORDER BY last_accessed_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.knowledgeDB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query memories including stale: %w", err)
	}
	defer rows.Close()

	return scanMemories(rows)
}

// QueryRecentSessionMemoriesIncludingStale is like QueryRecentSessionMemories but
// returns both active and stale session-log memories for explicit audit queries.
func (s *Store) QueryRecentSessionMemoriesIncludingStale(agentID string, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 10
	}

	now := time.Now().UTC().Format(time.RFC3339)
	q := `SELECT id, tier, content, entity_id, agent_id, task_id, tags,
	             created_at, expires_at, last_accessed_at, source
	      FROM memories
	      WHERE tier = 'session_log'
	        AND agent_id = ?
	        AND expires_at > ?
	      ORDER BY created_at DESC LIMIT ?`

	rows, err := s.knowledgeDB.Query(q, agentID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("query session memories including stale: %w", err)
	}
	defer rows.Close()

	return scanMemories(rows)
}

// QueryMemoriesForEntities retrieves entity-tier memories for multiple entity IDs.
// Returns a map of entityID → []Memory. Non-expired only.
func (s *Store) QueryMemoriesForEntities(entityIDs []string, limit int) (map[string][]Memory, error) {
	if len(entityIDs) == 0 || limit <= 0 {
		return nil, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	placeholders := make([]string, len(entityIDs))
	args := make([]interface{}, 0, len(entityIDs)+1)
	for i, id := range entityIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, now)

	q := fmt.Sprintf(`
		SELECT id, tier, content, entity_id, agent_id, task_id, tags,
		       created_at, expires_at, last_accessed_at, source
		FROM memories
		WHERE tier = 'entity'
		  AND entity_id IN (%s)
		  AND expires_at > ?
		  AND stale = 0
		ORDER BY last_accessed_at DESC`, strings.Join(placeholders, ","))

	rows, err := s.knowledgeDB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query memories for entities: %w", err)
	}
	defer rows.Close()

	mems, err := scanMemories(rows)
	if err != nil {
		return nil, err
	}

	result := make(map[string][]Memory)
	for _, m := range mems {
		if len(result[m.EntityID]) < limit {
			result[m.EntityID] = append(result[m.EntityID], m)
		}
	}
	return result, nil
}

// QueryRecentSessionMemories retrieves the most recent session-log memories
// for the given agent, ordered newest-first. Returns at most limit rows.
func (s *Store) QueryRecentSessionMemories(agentID string, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 10
	}

	now := time.Now().UTC().Format(time.RFC3339)
	q := `SELECT id, tier, content, entity_id, agent_id, task_id, tags,
	             created_at, expires_at, last_accessed_at, source
	      FROM memories
	      WHERE tier = 'session_log'
	        AND agent_id = ?
	        AND expires_at > ?
	        AND stale = 0
	      ORDER BY created_at DESC LIMIT ?`

	rows, err := s.knowledgeDB.Query(q, agentID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("query session memories: %w", err)
	}
	defer rows.Close()

	return scanMemories(rows)
}

// GetLatestWorkSummary returns the most recent session-log work-summary memory
// for the given agent. Work summaries are stored by handleEndSession with the
// tag "work_summary" and contain a JSON array of PackageWork entries.
// Returns nil, nil when no unexpired work summary exists.
func (s *Store) GetLatestWorkSummary(agentID string) (*Memory, error) {
	if agentID == "" {
		return nil, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	row := s.knowledgeDB.QueryRow(`
		SELECT id, tier, content, entity_id, agent_id, task_id, tags,
		       created_at, expires_at, last_accessed_at, source
		FROM memories
		WHERE tier = 'session_log'
		  AND agent_id = ?
		  AND tags LIKE '%"work_summary"%'
		  AND stale = 0
		  AND expires_at > ?
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, agentID, now)

	var m Memory
	err := row.Scan(
		&m.ID, &m.Tier, &m.Content, &m.EntityID, &m.AgentID, &m.TaskID,
		&m.Tags, &m.CreatedAt, &m.ExpiresAt, &m.LastAccessedAt, &m.Source,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest work summary: %w", err)
	}
	return &m, nil
}

// TouchMemory updates last_accessed_at and extends expires_at by 50% of the
// tier's base TTL (capped at 2x base). This implements access-based decay
// renewal — memories that prove useful stay alive longer.
func (s *Store) TouchMemory(id string) error {
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	// Read current tier to compute extension.
	var tier, expiresAt string
	err := s.knowledgeDB.QueryRow(`SELECT tier, expires_at FROM memories WHERE id = ?`, id).Scan(&tier, &expiresAt)
	if err != nil {
		return fmt.Errorf("touch memory: %w", err)
	}

	// Compute extension.
	var extension time.Duration
	var maxExpiry time.Time
	switch tier {
	case TierSessionLog:
		extension = ttlSessionLog / 2
		maxExpiry = now.Add(2 * ttlSessionLog)
	case TierProject:
		extension = ttlProject / 2
		maxExpiry = now.Add(2 * ttlProject)
	default:
		// Entity memories: no meaningful extension needed, just update access time.
		_, err := s.knowledgeDB.Exec(`UPDATE memories SET last_accessed_at = ? WHERE id = ?`, nowStr, id)
		return err
	}

	// Parse current expires_at and extend.
	current, _ := time.Parse(time.RFC3339, expiresAt)
	if current.IsZero() {
		current = now
	}
	newExpiry := current.Add(extension)
	if newExpiry.After(maxExpiry) {
		newExpiry = maxExpiry
	}

	_, err = s.knowledgeDB.Exec(`UPDATE memories SET last_accessed_at = ?, expires_at = ? WHERE id = ?`,
		nowStr, newExpiry.Format(time.RFC3339), id)
	return err
}

// TouchMemories batch-updates last_accessed_at for multiple memory IDs.
func (s *Store) TouchMemories(ids []string) {
	for _, id := range ids {
		_ = s.TouchMemory(id) // best-effort
	}
}

// ExpireMemories deletes memories past their expires_at. Call periodically.
// Also cleans up orphaned memory_anchors and memory_surfaced rows for deleted memories.
func (s *Store) ExpireMemories() (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	// Delete expired memories and clean up their anchors in one transaction.
	tx, err := s.knowledgeDB.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin expire tx: %w", err)
	}
	defer tx.Rollback()

	// Delete anchors, surfacing records, and embeddings for memories about to expire.
	// Correlated EXISTS is O(n·log n) with the PK index on memories,
	// vs NOT IN which materializes a full result set.
	_, _ = tx.Exec(`DELETE FROM memory_anchors WHERE EXISTS (
		SELECT 1 FROM memories WHERE memories.id = memory_anchors.memory_id AND memories.expires_at <= ?
	)`, now)
	_, _ = tx.Exec(`DELETE FROM memory_surfaced WHERE EXISTS (
		SELECT 1 FROM memories WHERE memories.id = memory_surfaced.memory_id AND memories.expires_at <= ?
	)`, now)
	_, _ = tx.Exec(`DELETE FROM memory_embeddings WHERE EXISTS (
		SELECT 1 FROM memories WHERE memories.id = memory_embeddings.memory_id AND memories.expires_at <= ?
	)`, now)
	result, err := tx.Exec(`DELETE FROM memories WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, fmt.Errorf("expire memories: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit expire tx: %w", err)
	}
	return result.RowsAffected()
}

// MarkEntityMemoriesStale marks all entity-tier memories for the given entity
// as stale (stale=1) and shortens their TTL to 30 days.
// Called when a single node is tombstoned (deleted from graph).
// For bulk node removal use MarkEntityMemoriesStaleForNodes.
func (s *Store) MarkEntityMemoriesStale(entityID, reason string) error {
	now := time.Now().UTC()
	staleExpiry := now.Add(30 * 24 * time.Hour).Format(time.RFC3339)
	staledAt := now.Format(time.RFC3339)
	_, err := s.knowledgeDB.Exec(`
		UPDATE memories SET stale = 1, stale_reason = ?, expires_at = ?, staled_at = ?
		WHERE tier = 'entity' AND entity_id = ?`,
		reason, staleExpiry, staledAt, entityID)
	return err
}

// MarkEntityMemoriesStaleForNodes marks entity-tier memories stale (stale=1) for
// all entity IDs in nodeIDs in a single batch. Covers non-anchored entity memories
// (written with entity_id but no anchor_nodes) that MarkAnchoredMemoriesStale
// does not reach. nodeIDs is processed in batches of ≤500 to respect SQLite's
// SQLITE_MAX_VARIABLE_NUMBER limit. reason is stored in stale_reason.
// A no-op when nodeIDs is empty.
func (s *Store) MarkEntityMemoriesStaleForNodes(nodeIDs []string, reason string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	tx, err := s.knowledgeDB.Begin()
	if err != nil {
		return fmt.Errorf("store.MarkEntityMemoriesStaleForNodes begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	now := time.Now().UTC()
	staleExpiry := now.Add(30 * 24 * time.Hour).Format(time.RFC3339)
	staledAt := now.Format(time.RFC3339)
	const batchSize = 500
	for i := 0; i < len(nodeIDs); i += batchSize {
		end := i + batchSize
		if end > len(nodeIDs) {
			end = len(nodeIDs)
		}
		batch := nodeIDs[i:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, len(batch)+3)
		args = append(args, reason, staleExpiry, staledAt)
		for _, id := range batch {
			args = append(args, id)
		}
		if _, err := tx.Exec(`
			UPDATE memories SET stale = 1, stale_reason = ?, expires_at = ?, staled_at = ?
			WHERE tier = 'entity' AND entity_id IN (`+placeholders+`)`,
			args...); err != nil {
			return fmt.Errorf("store.MarkEntityMemoriesStaleForNodes: %w", err)
		}
	}
	return tx.Commit()
}

// MarkAnchoredMemoriesStale sets stale=1 on all memories that have at least one
// anchor in nodeIDs. Idempotent: calling twice with the same IDs is safe.
// reason is stored in stale_reason for surfacing in session_init (AM-3).
// A no-op when nodeIDs is empty.
//
// nodeIDs is processed in batches of ≤500 to stay under SQLite's default
// SQLITE_MAX_VARIABLE_NUMBER limit of 999 (Gap 5 fix).
func (s *Store) MarkAnchoredMemoriesStale(nodeIDs []string, reason string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	tx, err := s.knowledgeDB.Begin()
	if err != nil {
		return fmt.Errorf("store.MarkAnchoredMemoriesStale begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	staledAt := time.Now().UTC().Format(time.RFC3339)
	const batchSize = 500
	for i := 0; i < len(nodeIDs); i += batchSize {
		end := i + batchSize
		if end > len(nodeIDs) {
			end = len(nodeIDs)
		}
		batch := nodeIDs[i:end]
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, len(batch)+2)
		args = append(args, reason, staledAt)
		for _, id := range batch {
			args = append(args, id)
		}
		if _, err := tx.Exec(`
			UPDATE memories SET stale = 1, stale_reason = ?, staled_at = ?
			WHERE id IN (
				SELECT memory_id FROM memory_anchors
				GROUP BY memory_id
				HAVING COUNT(*) = SUM(CASE WHEN node_id IN (`+placeholders+`) THEN 1 ELSE 0 END)
			)`, args...); err != nil {
			return fmt.Errorf("store.MarkAnchoredMemoriesStale: %w", err)
		}
	}
	return tx.Commit()
}

// QueryInvalidatedMemories returns stale memories that have not yet been
// surfaced to the given agent. Per-agent tracking: each agent has its own
// surfacing record in memory_surfaced, so every agent sees invalidated
// memories independently. Capped at limit rows, ordered by staled_at DESC
// so the most recently invalidated beliefs appear first.
// Used by AM-3: session_init surfaces these once, then MarkMemoriesSurfaced
// records the (memory_id, agent_id) pair so they don't re-appear for that agent.
func (s *Store) QueryInvalidatedMemories(agentID string, limit int) ([]InvalidatedMemory, error) {
	if limit <= 0 {
		limit = 10
	}
	now := time.Now().UTC().Format(time.RFC3339)

	// When agentID is empty (anonymous session), fall back to global surfaced_at
	// on the memories table so invalidations are still surfaced at least once.
	var rows *sql.Rows
	var err error
	if agentID != "" {
		rows, err = s.knowledgeDB.Query(`
			SELECT m.id, m.content, m.tier, m.stale_reason, m.staled_at
			FROM memories m
			LEFT JOIN memory_surfaced ms ON m.id = ms.memory_id AND ms.agent_id = ?
			WHERE m.stale = 1
			  AND ms.memory_id IS NULL
			  AND m.expires_at > ?
			ORDER BY CASE WHEN m.staled_at = '' THEN m.created_at ELSE m.staled_at END DESC
			LIMIT ?`, agentID, now, limit)
	} else {
		rows, err = s.knowledgeDB.Query(`
			SELECT m.id, m.content, m.tier, m.stale_reason, m.staled_at
			FROM memories m
			WHERE m.stale = 1
			  AND m.surfaced_at IS NULL
			  AND m.expires_at > ?
			ORDER BY CASE WHEN m.staled_at = '' THEN m.created_at ELSE m.staled_at END DESC
			LIMIT ?`, now, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query invalidated memories: %w", err)
	}
	defer rows.Close()

	var out []InvalidatedMemory
	for rows.Next() {
		var m InvalidatedMemory
		if err := rows.Scan(&m.ID, &m.Content, &m.Tier, &m.StaleReason, &m.InvalidatedAt); err != nil {
			return nil, fmt.Errorf("scan invalidated memory: %w", err)
		}
		// Fallback: if staled_at was empty (pre-migration data), use empty string
		// rather than a misleading created_at value.
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkMemoriesSurfaced records that the given agent has seen these invalidated
// memories. Two paths:
//   - Named agent (agentID != ""): INSERT into memory_surfaced table. Does NOT
//     touch the legacy surfaced_at column — anonymous sessions must still be able
//     to see these memories via their own fallback path.
//   - Anonymous (agentID == ""): SET surfaced_at on the memories table directly.
//     This is the only surfacing mechanism for anonymous sessions.
//
// Idempotent: INSERT OR IGNORE on the composite PK (named); UPDATE is idempotent (anon).
func (s *Store) MarkMemoriesSurfaced(agentID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.knowledgeDB.Begin()
	if err != nil {
		return fmt.Errorf("mark memories surfaced begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if agentID != "" {
		// Per-agent surfacing record. Does NOT touch legacy surfaced_at.
		for _, id := range ids {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO memory_surfaced (memory_id, agent_id, surfaced_at)
				VALUES (?, ?, ?)`, id, agentID, now); err != nil {
				return fmt.Errorf("mark memories surfaced: %w", err)
			}
		}
	} else {
		// Anonymous fallback: set global surfaced_at on the memories table.
		placeholders := strings.Repeat("?,", len(ids))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, 0, len(ids)+1)
		args = append(args, now)
		for _, id := range ids {
			args = append(args, id)
		}
		if _, err := tx.Exec(`UPDATE memories SET surfaced_at = ? WHERE id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("mark memories surfaced (legacy): %w", err)
		}
	}
	return tx.Commit()
}

// SearchMemories performs FTS5 BM25 full-text search over memory content.
// Returns non-expired memories ordered by relevance (best match first).
// The query uses FTS5 query syntax — each space-separated word is an implicit AND term.
func (s *Store) SearchMemories(query string, limit int) ([]Memory, error) {
	if query == "" {
		return nil, nil
	}
	safeQuery := sanitizeFTSQuery(query)
	if safeQuery == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.knowledgeDB.Query(`
		SELECT m.id, m.tier, m.content, m.entity_id, m.agent_id, m.task_id, m.tags,
		       m.created_at, m.expires_at, m.last_accessed_at, m.source
		FROM memories m
		JOIN memories_fts f ON m.rowid = f.rowid
		WHERE memories_fts MATCH ?
		  AND m.expires_at > ?
		  AND m.stale = 0
		ORDER BY rank
		LIMIT ?`, safeQuery, now, limit)
	if err != nil {
		return nil, fmt.Errorf("search memories: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// SearchMemoriesIncludingStale is like SearchMemories but also returns stale memories.
// Use for audit scenarios where the agent explicitly passes include_stale=true to recall().
func (s *Store) SearchMemoriesIncludingStale(query string, limit int) ([]Memory, error) {
	if query == "" {
		return nil, nil
	}
	safeQuery := sanitizeFTSQuery(query)
	if safeQuery == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.knowledgeDB.Query(`
		SELECT m.id, m.tier, m.content, m.entity_id, m.agent_id, m.task_id, m.tags,
		       m.created_at, m.expires_at, m.last_accessed_at, m.source
		FROM memories m
		JOIN memories_fts f ON m.rowid = f.rowid
		WHERE memories_fts MATCH ?
		  AND m.expires_at > ?
		ORDER BY rank
		LIMIT ?`, safeQuery, now, limit)
	if err != nil {
		return nil, fmt.Errorf("search memories including stale: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// InsertMemoryWithAnchors atomically inserts a memory and its anchor links in a
// single transaction. Both the memory INSERT and all anchor INSERTs run inside
// the same tx — if any step fails, the entire operation rolls back cleanly.
// If the memory deduplicates against an existing one, anchors are still added
// to the existing memory (additive enrichment) outside the tx.
func (s *Store) InsertMemoryWithAnchors(m Memory, anchorNodes []string) (string, error) {
	// No anchors → delegate to non-transactional path (avoids tx overhead).
	if len(anchorNodes) == 0 {
		return s.InsertMemory(m)
	}

	// ── Phase 1: Validate and check dedup OUTSIDE the tx ──────────────────
	// SetMaxOpenConns(1) means a tx holds the only conn. Any s.knowledgeDB.Query inside
	// a tx would deadlock. So we run all reads (dedup, defaults) first.
	m, deduped, err := s.prepareMemory(m)
	if err != nil {
		return "", err
	}
	if deduped != "" {
		// Memory deduped — wrap touch + anchor inserts in one tx so crash
		// between touch and anchors can't leave inconsistent state.
		tx, err := s.knowledgeDB.Begin()
		if err != nil {
			// Best-effort fallback: touch and anchors separately.
			_ = s.TouchMemory(deduped)
			_ = s.InsertMemoryAnchors(deduped, anchorNodes)
			return deduped, nil
		}
		defer tx.Rollback()

		// Inline touch: just update last_accessed_at (skip TTL extension logic
		// for simplicity — the full TouchMemory reads tier+expires_at which
		// would need s.knowledgeDB.QueryRow inside tx, risking the same conn deadlock).
		tx.Exec(`UPDATE memories SET last_accessed_at = ? WHERE id = ?`,
			time.Now().UTC().Format(time.RFC3339), deduped) //nolint:errcheck — best-effort

		now := time.Now().UTC().Format(time.RFC3339)
		for _, nid := range anchorNodes {
			if nid == "" {
				continue
			}
			tx.Exec(`INSERT OR IGNORE INTO memory_anchors (memory_id, node_id, created_at) VALUES (?, ?, ?)`,
				deduped, nid, now) //nolint:errcheck — INSERT OR IGNORE
		}
		_ = tx.Commit()
		return deduped, nil
	}

	// ── Phase 2: Insert memory + anchors in one tx ────────────────────────
	tx, err := s.knowledgeDB.Begin()
	if err != nil {
		return "", fmt.Errorf("begin memory+anchor tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags,
		                      created_at, expires_at, last_accessed_at, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Tier, m.Content, m.EntityID, m.AgentID, m.TaskID, m.Tags,
		m.CreatedAt, m.ExpiresAt, m.LastAccessedAt, m.Source,
	)
	if err != nil {
		return "", fmt.Errorf("insert memory in tx: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for _, nid := range anchorNodes {
		if nid == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO memory_anchors (memory_id, node_id, created_at) VALUES (?, ?, ?)`,
			m.ID, nid, now); err != nil {
			return "", fmt.Errorf("insert anchor in tx: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit memory+anchor tx: %w", err)
	}
	return m.ID, nil
}

// queryFreshMemoriesForDedup returns non-expired, non-stale memories for dedup
// comparison. Excludes stale memories (stale=1) so that TouchMemory can never
// resurrect an AM-2-invalidated memory by deduplicating a new write against it.
func (s *Store) queryFreshMemoriesForDedup(tier, entityID, agentID string) ([]Memory, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	q := `SELECT id, tier, content, entity_id, agent_id, task_id, tags,
	             created_at, expires_at, last_accessed_at, source
	      FROM memories WHERE expires_at > ? AND stale = 0`
	args := []interface{}{now}
	if tier != "" {
		q += ` AND tier = ?`
		args = append(args, tier)
	}
	if entityID != "" {
		q += ` AND entity_id = ?`
		args = append(args, entityID)
	}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	q += ` ORDER BY last_accessed_at DESC LIMIT 5`
	rows, err := s.knowledgeDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

// prepareMemory applies defaults, validates, and checks dedup for a Memory.
// Returns the prepared Memory, the deduped ID if a duplicate was found (empty if new),
// and any validation error. Does NOT insert — caller decides how to write.
func (s *Store) prepareMemory(m Memory) (Memory, string, error) {
	if m.ID == "" {
		m.ID = newID()
	}
	now := time.Now().UTC()
	if m.CreatedAt == "" {
		m.CreatedAt = now.Format(time.RFC3339)
	}
	if m.LastAccessedAt == "" {
		m.LastAccessedAt = m.CreatedAt
	}
	if m.Source == "" {
		m.Source = SourceManual
	}
	if m.Tags == "" {
		m.Tags = "[]"
	}
	if m.ExpiresAt == "" {
		switch m.Tier {
		case TierSessionLog:
			m.ExpiresAt = now.Add(ttlSessionLog).Format(time.RFC3339)
		case TierProject:
			m.ExpiresAt = now.Add(ttlProject).Format(time.RFC3339)
		case TierEntity:
			m.ExpiresAt = now.Add(365 * 10 * 24 * time.Hour).Format(time.RFC3339)
		default:
			m.ExpiresAt = now.Add(ttlProject).Format(time.RFC3339)
		}
	}

	content := strings.TrimSpace(m.Content)
	if len(content) < 10 {
		return m, "", fmt.Errorf("memory content too short (min 10 chars)")
	}
	m.Content = content

	if runes := []rune(m.Content); len(runes) > 2000 {
		m.Content = string(runes[:2000]) + "…[truncated]"
	}

	// Dedup check — only compare against fresh (non-stale) memories.
	// Comparing against stale memories would cause TouchMemory to resurrect
	// invalidated data and extend its TTL indefinitely (Gap 2 fix).
	var dupCandidates []Memory
	if m.EntityID != "" {
		dupCandidates, _ = s.queryFreshMemoriesForDedup(m.Tier, m.EntityID, "")
	} else if m.AgentID != "" {
		dupCandidates, _ = s.queryFreshMemoriesForDedup(m.Tier, "", m.AgentID)
	}
	for _, ex := range dupCandidates {
		if stringSimilarity(ex.Content, m.Content) > 0.85 {
			// Return dedup ID without side effects — caller handles touch.
			// prepareMemory must be pure (no writes) so callers can decide
			// whether to touch inside or outside a transaction.
			return m, ex.ID, nil
		}
	}

	return m, "", nil
}

// InsertMemoryAnchors links a memory to one or more graph node IDs.
// Used by AM-1: agents pass anchor_nodes when calling remember() to bind
// codebase-derived beliefs to the graph. AM-2 cascades invalidation via node_id index.
func (s *Store) InsertMemoryAnchors(memoryID string, nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, nid := range nodeIDs {
		if nid == "" {
			continue
		}
		_, err := s.knowledgeDB.Exec(`INSERT OR IGNORE INTO memory_anchors (memory_id, node_id, created_at) VALUES (?, ?, ?)`,
			memoryID, nid, now)
		if err != nil {
			return fmt.Errorf("insert memory anchor: %w", err)
		}
	}
	return nil
}

// GetMemoryAnchors returns the node IDs anchored to a memory.
func (s *Store) GetMemoryAnchors(memoryID string) ([]string, error) {
	rows, err := s.knowledgeDB.Query(`SELECT node_id FROM memory_anchors WHERE memory_id = ? ORDER BY node_id`, memoryID)
	if err != nil {
		return nil, fmt.Errorf("get memory anchors: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var nid string
		if err := rows.Scan(&nid); err != nil {
			return nil, fmt.Errorf("scan memory anchor: %w", err)
		}
		out = append(out, nid)
	}
	return out, rows.Err()
}

// GetMemoriesByAnchorNode returns memories anchored to the given node ID via the
// memory_anchors junction table. This finds memories linked through anchor_nodes=
// in remember(), which are NOT discoverable via QueryMemories(entityID=...) alone.
// Uses the idx_memory_anchors_node index for O(log N) lookup.
// Only returns non-expired, non-stale memories. Ordered by created_at DESC.
func (s *Store) GetMemoriesByAnchorNode(nodeID string, limit int) ([]Memory, error) {
	if nodeID == "" || limit <= 0 {
		return nil, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.knowledgeDB.Query(`
		SELECT m.id, m.tier, m.content, m.entity_id, m.agent_id, m.task_id, m.tags,
		       m.created_at, m.expires_at, m.last_accessed_at, m.source
		FROM memories m
		JOIN memory_anchors ma ON m.id = ma.memory_id
		WHERE ma.node_id = ?
		  AND m.expires_at > ?
		  AND m.stale = 0
		ORDER BY m.created_at DESC
		LIMIT ?`, nodeID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("get memories by anchor node: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// CountMemories returns total memory count by tier.
func (s *Store) CountMemories() (map[string]int, error) {
	rows, err := s.knowledgeDB.Query(`SELECT tier, COUNT(*) FROM memories GROUP BY tier`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var tier string
		var count int
		if err := rows.Scan(&tier, &count); err != nil {
			return nil, err
		}
		counts[tier] = count
	}
	return counts, rows.Err()
}

// GetMemoriesByIDs returns full Memory structs for the given IDs.
// Missing IDs are silently skipped. Used by recall() to hydrate vector
// search results that only contain partial fields.
func (s *Store) GetMemoriesByIDs(ids []string) ([]Memory, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT id, tier, content, entity_id, agent_id, task_id, tags,
	                 created_at, expires_at, last_accessed_at, source
	          FROM memories WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.knowledgeDB.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get memories by IDs: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// scanMemories reads rows into a Memory slice.
func scanMemories(rows *sql.Rows) ([]Memory, error) {
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(
			&m.ID, &m.Tier, &m.Content, &m.EntityID, &m.AgentID, &m.TaskID, &m.Tags,
			&m.CreatedAt, &m.ExpiresAt, &m.LastAccessedAt, &m.Source,
		); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// stringSimilarity computes Jaccard similarity between two strings based on
// normalized word overlap. Punctuation is stripped from word boundaries so
// "management." and "management" are treated as the same word. Returns 0.0–1.0.
func stringSimilarity(a, b string) float64 {
	setA := tokenSet(a)
	setB := tokenSet(b)
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}

	var intersection int
	for w := range setA {
		if setB[w] {
			intersection++
		}
	}

	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// tokenSet normalizes a string into a set of lowercase words, splitting on any
// non-alphanumeric character. This handles hyphens, parens, and punctuation so
// "auth-module (JWT)" and "auth module JWT" produce identical token sets.
func tokenSet(s string) map[string]bool {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	set := make(map[string]bool, len(words))
	for _, w := range words {
		if w != "" {
			set[w] = true
		}
	}
	return set
}

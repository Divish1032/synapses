package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// MemoryTier classifies the scope and lifespan of a memory.
const (
	TierSessionLog = "session_log" // What happened — auto-captured session summaries.
	TierEntity     = "entity"      // Facts about code nodes — travels with the entity.
	TierProject    = "project"     // Conventions, decisions, gotchas — project-wide.
)

// DefaultMaxMemoryRows is the per-project memory cap. Prevents unbounded disk
// growth from agents calling remember() in a loop. Configurable via Store.MaxMemoryRows.
const DefaultMaxMemoryRows = 10000

// DefaultMaxEpisodeRows is the per-project episode cap.
const DefaultMaxEpisodeRows = 10000

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

// timeFmtMicro is a fixed-width RFC 3339 variant with microsecond precision.
// Used for created_at so that rapid sequential inserts always get distinct,
// lexicographically-sortable timestamps — eliminating reliance on rowid as a
// tiebreaker (which is unreliable with TEXT PRIMARY KEY in some SQLite drivers).
const timeFmtMicro = "2006-01-02T15:04:05.000000Z07:00"

// ImportancePinned is a special importance value that exempts a memory from
// decay scoring. Pinned memories always score 1.0 regardless of age, making
// them permanently visible in recall results. Use for security configs,
// compliance decisions, architectural invariants — facts that must never be
// silently demoted by time-based decay.
const ImportancePinned = "pinned"

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
	Version        int    `json:"version,omitempty"` // Sprint 10.1: current version number (1-indexed)
	// Sprint 10.2: importance weight for decay scoring.
	// "pinned" = never decays. Numeric string (e.g. "0.8") = weight multiplier
	// applied to RecencyDecayScore. Default "1.0" = full recency decay.
	Importance  string `json:"importance,omitempty"`
	AccessCount int    `json:"access_count,omitempty"` // Sprint 11.5: ACT-R frequency counter
}

// MemoryVersion is a historical snapshot preserved when remember() deduplicates.
// The chain: version N → superseded_by → version N+1 (or current memory ID).
type MemoryVersion struct {
	ID           string `json:"id"`
	MemoryID     string `json:"memory_id"`
	Version      int    `json:"version"`
	Content      string `json:"content"`
	SupersededBy string `json:"superseded_by"`
	CreatedAt    string `json:"created_at"`    // when this version was originally written
	SupersededAt string `json:"superseded_at"` // when it was replaced
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
// Sprint 10.1: on dedup, snapshots the old content as a version before touching.
func (s *Store) InsertMemory(m Memory) (string, error) {
	// BUG-014: enforce per-project memory row cap.
	// rowCapMu serializes cap-check + insert to prevent concurrent writers
	// from both passing the check. Held for the duration of InsertMemory.
	// Cost: ~54µs per call (4µs COUNT + 50µs INSERT) — negligible.
	s.rowCapMu.Lock()
	defer s.rowCapMu.Unlock()
	maxRows := s.MaxMemoryRows
	if maxRows <= 0 {
		maxRows = DefaultMaxMemoryRows
	}
	var count int
	if err := s.knowledgeDB.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&count); err == nil && count >= maxRows {
		return "", fmt.Errorf("memory row cap reached (%d/%d) — prune old memories or increase the cap", count, maxRows)
	}

	m, dedup, err := s.prepareMemory(m)
	if err != nil {
		return "", err
	}
	if dedup.dedupedID != "" {
		// Sprint 10.1: snapshot old content before overwriting via touch.
		// activeFrom = the matched memory's created_at (or its last version's superseded_at
		// if versions exist, but for simplicity we use the memory's own last_accessed_at
		// which is updated on each dedup — approximating "when this content became active").
		// For v1 (no prior versions), activeFrom = memory.created_at is exact.
		if dedup.dedupedContent != "" && dedup.dedupedContent != m.Content {
			activeFrom := dedup.dedupedCreatedAt
			// If versions already exist, the activeFrom should be the latest
			// version's superseded_at (when that version was replaced with
			// the content we're now snapshotting). Fall back to created_at.
			var latestSupersededAt sql.NullString
			_ = s.knowledgeDB.QueryRow(
				`SELECT superseded_at FROM memory_versions WHERE memory_id = ? ORDER BY version DESC LIMIT 1`,
				dedup.dedupedID,
			).Scan(&latestSupersededAt)
			if latestSupersededAt.Valid && latestSupersededAt.String != "" {
				activeFrom = latestSupersededAt.String
			}
			if _, verr := s.CreateMemoryVersion(dedup.dedupedID, dedup.dedupedContent, activeFrom); verr != nil {
				logutil.Warn("synapses: store: create memory version on dedup: %v\n", verr)
			}
		}
		// Update memory content to the new (dedup-winning) content.
		if m.Content != dedup.dedupedContent {
			if uerr := s.UpdateMemoryContent(dedup.dedupedID, m.Content); uerr != nil {
				logutil.Warn("synapses: store: update memory content on dedup: %v\n", uerr)
			}
		}
		// Only emit knowledge_updated if the touch succeeds — a failed touch means
		// the memory was deleted between the dedup check and now (concurrent prune).
		// Emitting an event for a non-existent memory would corrupt learning-loop data.
		if touchErr := s.TouchMemory(dedup.dedupedID); touchErr == nil {
			if err := s.AppendEvent("knowledge_updated", m.AgentID,
				fmt.Sprintf(`{"memory_id":%q,"reason":"dedup"}`, dedup.dedupedID)); err != nil {
				logutil.Warn("synapses: store: append knowledge_updated event: %v\n", err)
			}
		}
		return dedup.dedupedID, nil
	}

	_, err = s.knowledgeDB.Exec(`
		INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags,
		                      created_at, expires_at, last_accessed_at, source, importance, access_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Tier, m.Content, m.EntityID, m.AgentID, m.TaskID, m.Tags,
		m.CreatedAt, m.ExpiresAt, m.LastAccessedAt, m.Source, m.Importance, m.AccessCount,
	)
	if err != nil {
		return "", fmt.Errorf("insert memory: %w", err)
	}
	if err := s.AppendEvent("knowledge_created", m.AgentID,
		fmt.Sprintf(`{"memory_id":%q,"tier":%q,"source":%q}`, m.ID, m.Tier, m.Source)); err != nil {
		logutil.Warn("synapses: store: append knowledge_created event: %v\n", err)
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
	             created_at, expires_at, last_accessed_at, source, importance, access_count
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
	             created_at, expires_at, last_accessed_at, source, importance, access_count
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

	// Cap total rows: limit per entity × number of entities.
	totalLimit := limit * len(entityIDs)
	q := fmt.Sprintf(`
		SELECT id, tier, content, entity_id, agent_id, task_id, tags,
		       created_at, expires_at, last_accessed_at, source, importance, access_count
		FROM memories
		WHERE tier = 'entity'
		  AND entity_id IN (%s)
		  AND expires_at > ?
		  AND stale = 0
		ORDER BY last_accessed_at DESC
		LIMIT ?`, strings.Join(placeholders, ","))
	args = append(args, totalLimit)

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
	             created_at, expires_at, last_accessed_at, source, importance, access_count
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
		       created_at, expires_at, last_accessed_at, source, importance, access_count
		FROM memories
		WHERE tier = 'session_log'
		  AND agent_id = ?
		  AND tags LIKE '%"work_summary"%'
		  AND stale = 0
		  AND expires_at > ?
		ORDER BY created_at DESC
		LIMIT 1`, agentID, now)

	var m Memory
	err := row.Scan(
		&m.ID, &m.Tier, &m.Content, &m.EntityID, &m.AgentID, &m.TaskID,
		&m.Tags, &m.CreatedAt, &m.ExpiresAt, &m.LastAccessedAt, &m.Source, &m.Importance,
		&m.AccessCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest work summary: %w", err)
	}
	return &m, nil
}

// GetMemoryContent returns the content of a memory by ID. Returns ("", false) if not found.
func (s *Store) GetMemoryContent(id string) (string, bool) {
	var content string
	err := s.knowledgeDB.QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&content)
	if err != nil {
		return "", false
	}
	return content, true
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
		// Entity memories: no meaningful extension needed, just update access time and count.
		_, err := s.knowledgeDB.Exec(`UPDATE memories SET last_accessed_at = ?, access_count = access_count + 1 WHERE id = ?`, nowStr, id)
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

	_, err = s.knowledgeDB.Exec(`UPDATE memories SET last_accessed_at = ?, expires_at = ?, access_count = access_count + 1 WHERE id = ?`,
		nowStr, newExpiry.Format(time.RFC3339), id)
	return err
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

	// Collect expiring memories inside the transaction to avoid TOCTOU races.
	type expiredEntry struct{ id, agentID string }
	var expiring []expiredEntry
	if erows, qErr := tx.Query(
		`SELECT id, agent_id FROM memories WHERE expires_at <= ?`, now,
	); qErr == nil {
		for erows.Next() {
			var e expiredEntry
			_ = erows.Scan(&e.id, &e.agentID)
			expiring = append(expiring, e)
		}
		_ = erows.Close()
	}

	// Delete anchors, surfacing records, embeddings, and versions for memories about to expire.
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
	// Sprint 10.1: cascade delete historical versions when parent memory expires.
	_, _ = tx.Exec(`DELETE FROM memory_versions WHERE EXISTS (
		SELECT 1 FROM memories WHERE memories.id = memory_versions.memory_id AND memories.expires_at <= ?
	)`, now)
	result, err := tx.Exec(`DELETE FROM memories WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, fmt.Errorf("expire memories: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit expire tx: %w", err)
	}

	// Emit lifecycle events for all deleted memories (Sprint 10.3).
	// Non-fatal: event failure does not roll back the deletion.
	for _, e := range expiring {
		if evErr := s.AppendEvent("knowledge_expired", e.agentID,
			fmt.Sprintf(`{"memory_id":%q}`, e.id)); evErr != nil {
			logutil.Warn("synapses: store: append knowledge_expired event: %v\n", evErr)
		}
	}

	return result.RowsAffected()
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
	return s.SearchMemoriesCtx(context.Background(), query, limit)
}

// SearchMemoriesCtx is the context-aware variant of SearchMemories.
func (s *Store) SearchMemoriesCtx(ctx context.Context, query string, limit int) ([]Memory, error) {
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
	rows, err := s.knowledgeDB.QueryContext(ctx, `
		SELECT m.id, m.tier, m.content, m.entity_id, m.agent_id, m.task_id, m.tags,
		       m.created_at, m.expires_at, m.last_accessed_at, m.source, m.importance, m.access_count
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

// ScoredMemory pairs a Memory with a raw channel score for ConvexMerge fusion.
type ScoredMemory struct {
	Memory Memory
	Score  float64 // raw BM25 score (higher = better match)
}

// SearchMemoriesWithScores returns memories matching an FTS query along with
// their raw BM25 scores. Used by ConvexMerge to do score-magnitude-aware
// fusion instead of rank-only RRF.
func (s *Store) SearchMemoriesWithScores(query string, limit int, includeStale bool) ([]ScoredMemory, error) {
	return s.SearchMemoriesWithScoresCtx(context.Background(), query, limit, includeStale)
}

// SearchMemoriesWithScoresCtx is the context-aware variant of SearchMemoriesWithScores.
func (s *Store) SearchMemoriesWithScoresCtx(ctx context.Context, query string, limit int, includeStale bool) ([]ScoredMemory, error) {
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

	q := `SELECT m.id, m.tier, m.content, m.entity_id, m.agent_id, m.task_id, m.tags,
	             m.created_at, m.expires_at, m.last_accessed_at, m.source, m.importance, m.access_count,
	             -rank AS score
	      FROM memories m
	      JOIN memories_fts f ON m.rowid = f.rowid
	      WHERE memories_fts MATCH ?
	        AND m.expires_at > ?`

	if !includeStale {
		q += ` AND m.stale = 0`
	}
	q += ` ORDER BY rank LIMIT ?`

	rows, err := s.knowledgeDB.QueryContext(ctx, q, safeQuery, now, limit)
	if err != nil {
		return nil, fmt.Errorf("search memories with scores: %w", err)
	}
	defer rows.Close()

	var out []ScoredMemory
	for rows.Next() {
		var sm ScoredMemory
		if err := rows.Scan(
			&sm.Memory.ID, &sm.Memory.Tier, &sm.Memory.Content, &sm.Memory.EntityID,
			&sm.Memory.AgentID, &sm.Memory.TaskID, &sm.Memory.Tags,
			&sm.Memory.CreatedAt, &sm.Memory.ExpiresAt, &sm.Memory.LastAccessedAt,
			&sm.Memory.Source, &sm.Memory.Importance, &sm.Memory.AccessCount,
			&sm.Score,
		); err != nil {
			return nil, fmt.Errorf("scan scored memory: %w", err)
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// SearchMemoriesIncludingStale is like SearchMemories but also returns stale memories.
// Use for audit scenarios where the agent explicitly passes include_stale=true to recall().
func (s *Store) SearchMemoriesIncludingStale(query string, limit int) ([]Memory, error) {
	return s.SearchMemoriesIncludingStaleCtx(context.Background(), query, limit)
}

// SearchMemoriesIncludingStaleCtx is the context-aware variant of SearchMemoriesIncludingStale.
func (s *Store) SearchMemoriesIncludingStaleCtx(ctx context.Context, query string, limit int) ([]Memory, error) {
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
	rows, err := s.knowledgeDB.QueryContext(ctx, `
		SELECT m.id, m.tier, m.content, m.entity_id, m.agent_id, m.task_id, m.tags,
		       m.created_at, m.expires_at, m.last_accessed_at, m.source, m.importance, m.access_count
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
	// Writer pool has MaxOpenConns=1: a tx holds the only writer conn.
	// Reads go through the separate reader pool and won't deadlock, but we
	// still run dedup/defaults first to avoid reading uncommitted writes.
	m, dedup, err := s.prepareMemory(m)
	if err != nil {
		return "", err
	}
	if dedup.dedupedID != "" {
		// Sprint 10.1: snapshot old content before overwriting via touch.
		if dedup.dedupedContent != "" && dedup.dedupedContent != m.Content {
			activeFrom := dedup.dedupedCreatedAt
			var latestSupersededAt sql.NullString
			_ = s.knowledgeDB.QueryRow(
				`SELECT superseded_at FROM memory_versions WHERE memory_id = ? ORDER BY version DESC LIMIT 1`,
				dedup.dedupedID,
			).Scan(&latestSupersededAt)
			if latestSupersededAt.Valid && latestSupersededAt.String != "" {
				activeFrom = latestSupersededAt.String
			}
			if _, verr := s.CreateMemoryVersion(dedup.dedupedID, dedup.dedupedContent, activeFrom); verr != nil {
				logutil.Warn("synapses: store: create memory version on dedup (anchored): %v\n", verr)
			}
		}
		// Update memory content to the new (dedup-winning) content.
		if m.Content != dedup.dedupedContent {
			if uerr := s.UpdateMemoryContent(dedup.dedupedID, m.Content); uerr != nil {
				logutil.Warn("synapses: store: update memory content on dedup (anchored): %v\n", uerr)
			}
		}
		// Memory deduped — wrap touch + anchor inserts in one tx so crash
		// between touch and anchors can't leave inconsistent state.
		tx, err := s.knowledgeDB.Begin()
		if err != nil {
			// Best-effort fallback: touch and anchors separately.
			_ = s.TouchMemory(dedup.dedupedID)
			_ = s.InsertMemoryAnchors(dedup.dedupedID, anchorNodes)
			// Emit outside any tx — connection is free after fallback ops.
			if evErr := s.AppendEvent("knowledge_updated", m.AgentID,
				fmt.Sprintf(`{"memory_id":%q,"reason":"dedup"}`, dedup.dedupedID)); evErr != nil {
				logutil.Warn("synapses: store: append knowledge_updated event: %v\n", evErr)
			}
			return dedup.dedupedID, nil
		}
		defer tx.Rollback()

		// Inline touch: just update last_accessed_at (skip TTL extension logic
		// for simplicity — the full TouchMemory reads tier+expires_at which
		// would need s.knowledgeDB.QueryRow inside tx, risking the same conn deadlock).
		tx.Exec(`UPDATE memories SET last_accessed_at = ? WHERE id = ?`,
			time.Now().UTC().Format(time.RFC3339), dedup.dedupedID) //nolint:errcheck — best-effort

		now := time.Now().UTC().Format(time.RFC3339)
		for _, nid := range anchorNodes {
			if nid == "" {
				continue
			}
			tx.Exec(`INSERT OR IGNORE INTO memory_anchors (memory_id, node_id, created_at) VALUES (?, ?, ?)`,
				dedup.dedupedID, nid, now) //nolint:errcheck — INSERT OR IGNORE
		}
		if commitErr := tx.Commit(); commitErr == nil {
			// Emit after commit — connection is released back to pool at this point.
			if evErr := s.AppendEvent("knowledge_updated", m.AgentID,
				fmt.Sprintf(`{"memory_id":%q,"reason":"dedup","anchors":%d}`, dedup.dedupedID, len(anchorNodes))); evErr != nil {
				logutil.Warn("synapses: store: append knowledge_updated event: %v\n", evErr)
			}
		}
		return dedup.dedupedID, nil
	}

	// ── Phase 2: Insert memory + anchors in one tx ────────────────────────
	tx, err := s.knowledgeDB.Begin()
	if err != nil {
		return "", fmt.Errorf("begin memory+anchor tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags,
		                      created_at, expires_at, last_accessed_at, source, importance, access_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Tier, m.Content, m.EntityID, m.AgentID, m.TaskID, m.Tags,
		m.CreatedAt, m.ExpiresAt, m.LastAccessedAt, m.Source, m.Importance, m.AccessCount,
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
	// Emit after commit — connection is released back to pool at this point.
	if evErr := s.AppendEvent("knowledge_created", m.AgentID,
		fmt.Sprintf(`{"memory_id":%q,"tier":%q,"source":%q,"anchors":%d}`, m.ID, m.Tier, m.Source, len(anchorNodes))); evErr != nil {
		logutil.Warn("synapses: store: append knowledge_created event: %v\n", evErr)
	}
	return m.ID, nil
}

// queryFreshMemoriesForDedup returns non-expired, non-stale memories for dedup
// comparison. Excludes stale memories (stale=1) so that TouchMemory can never
// resurrect an AM-2-invalidated memory by deduplicating a new write against it.
func (s *Store) queryFreshMemoriesForDedup(tier, entityID, agentID string) ([]Memory, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	q := `SELECT id, tier, content, entity_id, agent_id, task_id, tags,
	             created_at, expires_at, last_accessed_at, source, importance, access_count
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

// prepareMemoryResult holds the result of prepareMemory dedup check.
type prepareMemoryResult struct {
	dedupedID        string // non-empty if dedup matched an existing memory
	dedupedContent   string // the old content of the matched memory (for versioning)
	dedupedCreatedAt string // the matched memory's created_at (for version activeFrom)
}

// prepareMemory applies defaults, validates, and checks dedup for a Memory.
// Returns the prepared Memory, dedup result (ID + old content if matched),
// and any validation error. Does NOT insert — caller decides how to write.
func (s *Store) prepareMemory(m Memory) (Memory, prepareMemoryResult, error) {
	if m.ID == "" {
		m.ID = newID()
	}
	now := time.Now().UTC()
	if m.CreatedAt == "" {
		m.CreatedAt = now.Format(timeFmtMicro)
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
	// Validate caller-provided importance (non-empty, non-pinned).
	// When empty, A-MAC admission control computes it after the dedup check.
	if m.Importance != "" && m.Importance != ImportancePinned {
		// Values below the visibility threshold (e.g. "0.0", "0.01") would make the
		// memory permanently invisible on recall immediately after creation — almost
		// certainly not what the caller intended. We clamp rather than reject to stay
		// non-breaking.
		//
		// Clamp floor is 2× DecayVisibilityThreshold so that a freshly-created memory
		// always scores comfortably above the threshold (not right at the boundary,
		// where floating-point rounding of the recency term could dip it below).
		const minImportanceWeight = DecayVisibilityThreshold * 2 // 0.10
		if w, err := strconv.ParseFloat(m.Importance, 64); err != nil || w < 0 {
			m.Importance = "1.0" // invalid/negative — default to normal decay
		} else if w < minImportanceWeight {
			m.Importance = strconv.FormatFloat(minImportanceWeight, 'f', -1, 64)
		}
	}

	content := strings.TrimSpace(m.Content)
	if len(content) < 10 {
		return m, prepareMemoryResult{}, fmt.Errorf("memory content too short (min 10 chars)")
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
	// newVec caches the new memory's embedding for the semantic dedup check.
	// Computed lazily (at most once) only when Jaccard is inconclusive AND
	// semanticDedupFunc is set AND the candidate has a stored embedding.
	var newVec []float32
	var newVecComputed bool
	embedFn := s.getSemanticDedupFunc() // thread-safe snapshot

	// maxJaccard tracks the highest Jaccard similarity seen across all candidates.
	// maxCosine tracks the highest cosine similarity (when embeddings are available).
	// Both are passed to computeAdmissionImportance — no re-fetching of embeddings needed.
	var maxJaccard float64
	var maxCosine float32
	var hasCosine bool // true once at least one cosine similarity was computed

	for _, ex := range dupCandidates {
		sim := stringSimilarity(ex.Content, m.Content)
		if sim > maxJaccard {
			maxJaccard = sim
		}
		if sim > 0.85 {
			// High Jaccard — definite dedup match.
			return m, prepareMemoryResult{
				dedupedID:        ex.ID,
				dedupedContent:   ex.Content,
				dedupedCreatedAt: ex.CreatedAt,
			}, nil
		}
		// Inconclusive Jaccard [0.5, 0.85): check cosine similarity of
		// embeddings to catch paraphrased duplicates. Degrades gracefully:
		// no embedder or no stored embedding → skip (same as before).
		if sim >= 0.5 && embedFn != nil {
			candidateVec := s.GetMemoryEmbedding(ex.ID)
			if len(candidateVec) == 0 {
				continue
			}
			if !newVecComputed {
				newVecComputed = true
				if raw, embedErr := s.safeEmbed(embedFn, m.Content); embedErr == nil {
					newVec = normalizeVec(raw)
				}
			}
			if len(newVec) > 0 {
				cos := dotSimilarity(newVec, candidateVec)
				hasCosine = true
				if cos > maxCosine {
					maxCosine = cos // track for A-MAC — no re-fetch needed
				}
				if cos > 0.9 {
					return m, prepareMemoryResult{
						dedupedID:        ex.ID,
						dedupedContent:   ex.Content,
						dedupedCreatedAt: ex.CreatedAt,
					}, nil
				}
			}
		}
	}

	// A-MAC: auto-compute importance when caller didn't set it explicitly.
	// Score = content_type_prior × novelty_factor (A-MAC, arXiv:2603.04549).
	// maxJaccard and maxCosine were captured during the dedup loop above — no
	// extra embedding or DB calls are needed here.
	if m.Importance == "" {
		m.Importance = computeAdmissionImportance(m.Tags, m.Source, len(dupCandidates) > 0, maxJaccard, maxCosine, hasCosine)
	}

	return m, prepareMemoryResult{}, nil
}

// parseContentTypePrior returns the A-MAC content-type prior for importance scoring.
// Explicit episodic decisions (failure, pattern, rule_proposal) score higher than
// generic decisions or auto-captured session logs, reflecting their higher future utility.
func parseContentTypePrior(tags, source string) float64 {
	if source == SourceAuto {
		return 0.8 // auto-captured session logs have lower signal-to-noise ratio
	}
	// Tags format: ["episode","failure"] — check for episode type substring.
	switch {
	case strings.Contains(tags, `"failure"`):
		return 1.4 // failures must surface to prevent repeat mistakes
	case strings.Contains(tags, `"pattern"`):
		return 1.2 // reusable patterns have high future utility
	case strings.Contains(tags, `"rule_proposal"`):
		return 1.2 // architectural rules are structural knowledge
	default:
		return 1.0 // general decisions and non-episodic content
	}
}

// computeAdmissionImportance implements A-MAC write-time importance scoring.
//
// Score = content_type_prior × novelty_factor (A-MAC, arXiv:2603.04549).
//   - content_type_prior: derived from episode type in tags and source field
//   - novelty_factor: 1 − max_similarity(new, recent_same_tier_memories)
//
// maxJaccard and maxCosine are pre-computed in the dedup loop — this function
// performs no DB calls and is a pure computation. Clamped to [0.10, 2.0].
//
// hasCandidates must be true when any same-tier memories exist (even if their
// embeddings are absent). It distinguishes "no prior memories" (novelty=1.0)
// from "similar memories but no embeddings" (Jaccard fallback).
//
// hasCosine must be true when at least one cosine similarity was computed
// during dedup. This is distinct from maxCosine > 0: perfectly orthogonal
// embeddings produce cosine=0.0, which is a valid "no similarity" result
// that should use the cosine path, not fall through to Jaccard.
func computeAdmissionImportance(tags, source string, hasCandidates bool, maxJaccard float64, maxCosine float32, hasCosine bool) string {
	const (
		minImportance = DecayVisibilityThreshold * 2 // 0.10 — floor matching clamp elsewhere
		maxImportance = 2.0                          // cap: matches edge weight scale in BFS
		noveltyFloor  = 0.2                          // even near-duplicates retain baseline value
	)

	prior := parseContentTypePrior(tags, source)

	var noveltyFactor float64
	switch {
	case !hasCandidates:
		noveltyFactor = 1.0 // no existing memories → fully novel by definition
	case hasCosine:
		// Cosine was computed during dedup — more accurate than Jaccard for
		// semantic similarity. Uses embeddings already fetched, no extra DB calls.
		// Uses hasCosine flag (not maxCosine > 0) because cosine=0.0 (perfectly
		// orthogonal) is a valid result that means "no similarity".
		noveltyFactor = 1.0 - float64(maxCosine)
	default:
		// Jaccard fallback: embedder unavailable or no candidate had stored embeddings.
		noveltyFactor = 1.0 - maxJaccard
	}

	// Clamp novelty: floor ensures even redundant memories retain baseline importance.
	if noveltyFactor < noveltyFloor {
		noveltyFactor = noveltyFloor
	} else if noveltyFactor > 1.0 {
		noveltyFactor = 1.0
	}

	importance := prior * noveltyFactor

	// Clamp to system-safe range.
	if importance < minImportance {
		importance = minImportance
	} else if importance > maxImportance {
		importance = maxImportance
	}

	return strconv.FormatFloat(importance, 'f', -1, 64)
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

// GetMemoryAnchorNodeIDsInSet returns for each memory the first anchor node ID
// that is present in nodeSet (the BFS-discovered nodes). Returns map[memoryID → nodeID].
// Memories with no anchors in nodeSet are absent from the map.
//
// This is the correct method for path reconstruction: a memory may have multiple
// anchors, and only the one that was actually discovered by BFS is useful for tracing
// the path back to the query seed. Using GetMemoryAnchorNodeIDs (first by created_at)
// would silently drop paths when the first anchor is not the BFS-discovered one.
//
// Batches memIDs in groups of 200 (leaves room for nodeSet placeholders within
// SQLite's 999-variable limit even when nodeSet has up to 500 entries).
func (s *Store) GetMemoryAnchorNodeIDsInSet(memIDs []string, nodeSet map[string]bool) (map[string]string, error) {
	if len(memIDs) == 0 || len(nodeSet) == 0 {
		return nil, nil
	}

	// Build sorted node ID slice for stable, deterministic IN clause.
	nodeIDs := make([]string, 0, len(nodeSet))
	for nid := range nodeSet {
		nodeIDs = append(nodeIDs, nid)
	}

	nodePlaceholders := make([]string, len(nodeIDs))
	nodeArgs := make([]interface{}, len(nodeIDs))
	for i, nid := range nodeIDs {
		nodePlaceholders[i] = "?"
		nodeArgs[i] = nid
	}
	nodeInClause := strings.Join(nodePlaceholders, ",")

	result := make(map[string]string, len(memIDs))

	const batchSize = 200
	for i := 0; i < len(memIDs); i += batchSize {
		end := i + batchSize
		if end > len(memIDs) {
			end = len(memIDs)
		}
		batch := memIDs[i:end]

		memPlaceholders := make([]string, len(batch))
		args := make([]interface{}, len(batch))
		for j, id := range batch {
			memPlaceholders[j] = "?"
			args[j] = id
		}
		// Combine args: mem IDs first, then node IDs.
		allArgs := append(args, nodeArgs...)

		rows, err := s.knowledgeDB.Query(
			`SELECT memory_id, node_id FROM memory_anchors
			 WHERE memory_id IN (`+strings.Join(memPlaceholders, ",")+`)
			   AND node_id IN (`+nodeInClause+`)
			 ORDER BY memory_id, created_at`,
			allArgs...,
		)
		if err != nil {
			return nil, fmt.Errorf("get memory anchor node IDs in set: %w", err)
		}
		func() {
			defer rows.Close()
			for rows.Next() {
				var memID, nodeID string
				if scanErr := rows.Scan(&memID, &nodeID); scanErr != nil {
					err = fmt.Errorf("scan memory anchor node ID in set: %w", scanErr)
					return
				}
				if _, exists := result[memID]; !exists {
					result[memID] = nodeID
				}
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				err = fmt.Errorf("memory anchor node IDs in set rows: %w", rowsErr)
			}
		}()
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

// GetAllMemoryAnchorNodeIDsInSet returns ALL anchor node IDs in nodeSet for
// each memory, not just the first. Used by the spreading activation sort step
// to compute maximum activation across all of a memory's anchors.
//
// Returns map[memoryID → []nodeID]. Memories with no anchors in nodeSet are
// absent. Batches like GetMemoryAnchorNodeIDsInSet (200 mem IDs per batch).
func (s *Store) GetAllMemoryAnchorNodeIDsInSet(memIDs []string, nodeSet map[string]bool) (map[string][]string, error) {
	if len(memIDs) == 0 || len(nodeSet) == 0 {
		return nil, nil
	}

	nodeIDs := make([]string, 0, len(nodeSet))
	for nid := range nodeSet {
		nodeIDs = append(nodeIDs, nid)
	}
	nodePlaceholders := make([]string, len(nodeIDs))
	nodeArgs := make([]interface{}, len(nodeIDs))
	for i, nid := range nodeIDs {
		nodePlaceholders[i] = "?"
		nodeArgs[i] = nid
	}
	nodeInClause := strings.Join(nodePlaceholders, ",")

	result := make(map[string][]string, len(memIDs))

	const batchSize = 200
	for i := 0; i < len(memIDs); i += batchSize {
		end := i + batchSize
		if end > len(memIDs) {
			end = len(memIDs)
		}
		batch := memIDs[i:end]

		memPlaceholders := make([]string, len(batch))
		args := make([]interface{}, len(batch))
		for j, id := range batch {
			memPlaceholders[j] = "?"
			args[j] = id
		}
		allArgs := append(args, nodeArgs...)

		rows, err := s.knowledgeDB.Query(
			`SELECT memory_id, node_id FROM memory_anchors
			 WHERE memory_id IN (`+strings.Join(memPlaceholders, ",")+`)
			   AND node_id IN (`+nodeInClause+`)`,
			allArgs...,
		)
		if err != nil {
			return nil, fmt.Errorf("get all memory anchor node IDs in set: %w", err)
		}
		for rows.Next() {
			var memID, nodeID string
			if err := rows.Scan(&memID, &nodeID); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan all memory anchor node IDs in set: %w", err)
			}
			result[memID] = append(result[memID], nodeID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("all memory anchor node IDs in set rows: %w", err)
		}
		rows.Close()
	}
	return result, nil
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
		       m.created_at, m.expires_at, m.last_accessed_at, m.source, m.importance, m.access_count
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

// GetMemoriesByIDs returns full Memory structs for the given IDs.
// Missing IDs are silently skipped. Used by recall() to hydrate vector
// search results that only contain partial fields.
func (s *Store) GetMemoriesByIDs(ids []string) ([]Memory, error) {
	return s.GetMemoriesByIDsCtx(context.Background(), ids)
}

// GetMemoriesByIDsCtx is the context-aware variant of GetMemoriesByIDs.
func (s *Store) GetMemoriesByIDsCtx(ctx context.Context, ids []string) ([]Memory, error) {
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
	                 created_at, expires_at, last_accessed_at, source, importance, access_count
	          FROM memories WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.knowledgeDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get memories by IDs: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// ── Sprint 10.1: Memory Versioning ──────────────────────────────────────────

// maxVersionsPerMemory caps how many historical versions a single memory can have.
// When exceeded, the oldest version is deleted. Prevents unbounded row growth from
// high-frequency dedup cycles.
const maxVersionsPerMemory = 50

// CreateMemoryVersion snapshots the current content of a memory as a historical
// version before the memory is updated (dedup overwrite). Returns the version number.
//
// Temporal semantics:
//   - created_at: when this version's content was originally written (the memory's
//     created_at for v1, or the previous version's superseded_at for v2+).
//   - superseded_at: NOW — when this version was replaced by the new content.
//
// The live memory row in `memories` always holds the *current* content.
// Versions hold *previous* content that was active from created_at to superseded_at.
//
// Caller must provide oldContent (the content being replaced) and memCreatedAt
// (the memory's created_at or last version's superseded_at as the start time).
//
// Concurrency safety: uses INSERT ... SELECT for atomic version numbering.
// Enforces maxVersionsPerMemory cap — oldest version is pruned when exceeded.
func (s *Store) CreateMemoryVersion(memoryID, oldContent, activeFrom string) (int, error) {
	supersededAt := time.Now().UTC().Format(time.RFC3339)
	versionID := newID()

	// Atomic: compute next version and insert in one statement.
	_, err := s.knowledgeDB.Exec(`
		INSERT INTO memory_versions (id, memory_id, version, content, superseded_by, created_at, superseded_at)
		SELECT ?, ?, COALESCE(MAX(version), 0) + 1, ?, ?, ?, ?
		FROM memory_versions WHERE memory_id = ?`,
		versionID, memoryID, oldContent, memoryID, activeFrom, supersededAt, memoryID,
	)
	if err != nil {
		return 0, fmt.Errorf("insert memory version: %w", err)
	}

	// Read back the version number for the return value.
	var ver int
	err = s.knowledgeDB.QueryRow(
		`SELECT version FROM memory_versions WHERE id = ?`, versionID,
	).Scan(&ver)
	if err != nil {
		return 0, fmt.Errorf("read back version: %w", err)
	}

	// Prune oldest versions if cap exceeded.
	if ver > maxVersionsPerMemory {
		_, _ = s.knowledgeDB.Exec(`
			DELETE FROM memory_versions WHERE id IN (
				SELECT id FROM memory_versions WHERE memory_id = ?
				ORDER BY version ASC LIMIT ?
			)`, memoryID, ver-maxVersionsPerMemory)
	}

	return ver, nil
}

// UpdateMemoryContent updates the content of an existing memory in-place.
// Called after versioning to store the new (dedup-winning) content.
func (s *Store) UpdateMemoryContent(memoryID, newContent string) error {
	_, err := s.knowledgeDB.Exec(
		`UPDATE memories SET content = ? WHERE id = ?`, newContent, memoryID,
	)
	if err != nil {
		return fmt.Errorf("update memory content: %w", err)
	}
	return nil
}

// GetMemoryAsOf returns memories with content as it existed at the given point in time.
// For each memory in the input set:
//   - If memory.created_at > asOf → memory didn't exist yet → excluded
//   - If a version existed that was active at asOf (created_at <= asOf < superseded_at)
//     → return that version's content instead of current content
//   - If no version covers asOf but the memory existed → current content is returned
//     (the memory was never overwritten, or asOf is after the latest supersession)
//
// All timestamps are UTC RFC3339 (guaranteed by prepareMemory), so string comparison
// is safe for temporal ordering.
func (s *Store) GetMemoryAsOf(memoryIDs []string, asOf time.Time) ([]Memory, error) {
	if len(memoryIDs) == 0 {
		return nil, nil
	}

	asOfStr := asOf.Format(time.RFC3339)

	// Step 1: Fetch current memories.
	mems, err := s.GetMemoriesByIDs(memoryIDs)
	if err != nil {
		return nil, err
	}

	// Step 2: For each memory, check if it existed at asOf and find the active content.
	var result []Memory
	for _, m := range mems {
		// Memory didn't exist at asOf.
		if m.CreatedAt > asOfStr {
			continue
		}

		// Find the version that was active at asOf.
		// Active at time T means: created_at <= T AND superseded_at > T.
		// We want the highest version number matching this range (most recent
		// version that was still active at asOf).
		var vContent sql.NullString
		var vVersion sql.NullInt64
		err := s.knowledgeDB.QueryRow(`
			SELECT content, version FROM memory_versions
			WHERE memory_id = ? AND created_at <= ? AND superseded_at > ?
			ORDER BY version DESC LIMIT 1`,
			m.ID, asOfStr, asOfStr,
		).Scan(&vContent, &vVersion)

		if err == nil && vContent.Valid {
			// A historical version was active at asOf — use its content.
			m.Content = vContent.String
			m.Version = int(vVersion.Int64)
		}
		// else: no historical version was active at asOf. Either no versions exist
		// (memory was never overwritten) or asOf is after the latest supersession
		// (current content is the right answer). Both cases: keep m.Content as-is.

		result = append(result, m)
	}

	return result, nil
}

// scanMemories reads rows into a Memory slice.
// Expects columns: id, tier, content, entity_id, agent_id, task_id, tags,
// created_at, expires_at, last_accessed_at, source, importance, access_count, access_count.
func scanMemories(rows *sql.Rows) ([]Memory, error) {
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(
			&m.ID, &m.Tier, &m.Content, &m.EntityID, &m.AgentID, &m.TaskID, &m.Tags,
			&m.CreatedAt, &m.ExpiresAt, &m.LastAccessedAt, &m.Source, &m.Importance,
			&m.AccessCount,
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

// safeEmbed calls the embedding function with panic recovery. If the embedder
// panics (e.g., ONNX runtime crash), the error is captured instead of crashing
// the server. This makes semantic dedup a best-effort enhancement that never
// degrades the core memory write path.
func (*Store) safeEmbed(fn func(string) ([]float32, error), text string) (vec []float32, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("semantic dedup embed panic: %v", r)
		}
	}()
	return fn(text)
}

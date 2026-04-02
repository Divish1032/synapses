package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// ExploreEntitySummary aggregates cross-session exploration data for a single
// entity. Returned by GetTopExploredEntities for session_init's previously_explored
// briefing section. All counts span prior sessions only (current session excluded).
type ExploreEntitySummary struct {
	Entity       string `json:"entity"`
	HitCount     int    `json:"hit_count"`     // total exploration events across prior sessions
	SessionCount int    `json:"session_count"` // distinct sessions that explored this entity
	TopFinding   string `json:"top_finding"`   // most recent non-empty finding_summary
}

// ExplorationEntry records what an agent queried and what was found in a single
// tool call. Unlike LedgerEntry (which only captures input signals for cross-session
// overlap detection), ExplorationEntry captures the response-side findings so the
// compaction recovery packet can surface "what was learned" — not just "what was touched."
//
// Populated by the server's ledgerWrapped for get_context, search, get_impact,
// and validate tool calls. No agent action required (Tier 1 server-side capture).
type ExplorationEntry struct {
	SessionID      string `json:"session_id"`
	ProjectID      string `json:"project_id"`
	ToolName       string `json:"tool_name"`
	EntityQueried  string `json:"entity_queried,omitempty"`  // primary entity/symbol/query term
	QueryContext   string `json:"query_context,omitempty"`   // intent, mode, or extra context
	FindingSummary string `json:"finding_summary,omitempty"` // NL summary of what was found (≤300 chars)
	CreatedAt      string `json:"created_at,omitempty"`
}

// AppendExplorationEntry writes a single exploration log entry to the knowledge DB.
// Safe to call from a background goroutine.
func (s *Store) AppendExplorationEntry(e ExplorationEntry) error {
	if s.knowledgeDB == nil {
		return nil
	}
	_, err := s.knowledgeDB.Exec(
		`INSERT INTO exploration_log
		    (session_id, project_id, tool_name, entity_queried, query_context, finding_summary)
		 VALUES (?,?,?,?,?,?)`,
		e.SessionID, e.ProjectID, e.ToolName, e.EntityQueried, e.QueryContext, e.FindingSummary,
	)
	return err
}

// GetSessionExplorationLog returns up to limit exploration entries for the given
// session, ordered by most recent first. Used by buildCompactionRecovery to
// populate the explored_entities section of the recovery packet.
func (s *Store) GetSessionExplorationLog(sessionID string, limit int) ([]ExplorationEntry, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.knowledgeDB.Query(
		`SELECT tool_name, entity_queried, query_context, finding_summary, created_at
		 FROM exploration_log
		 WHERE session_id = ?
		 ORDER BY created_at DESC
		 LIMIT ?`,
		sessionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []ExplorationEntry
	for rows.Next() {
		var e ExplorationEntry
		e.SessionID = sessionID
		if err := rows.Scan(&e.ToolName, &e.EntityQueried, &e.QueryContext, &e.FindingSummary, &e.CreatedAt); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// GetExploredEntitySet returns the set of entity names explored in the given
// session. Used by 24.3 (compaction detection heuristic) to detect re-exploration:
// if the agent calls get_context/search for an entity already in this set,
// it is likely re-exploring after compaction.
func (s *Store) GetExploredEntitySet(sessionID string) (map[string]struct{}, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	rows, err := s.knowledgeDB.Query(
		`SELECT DISTINCT entity_queried
		 FROM exploration_log
		 WHERE session_id = ? AND entity_queried != ''`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]struct{})
	for rows.Next() {
		var entity string
		if err := rows.Scan(&entity); err != nil {
			continue
		}
		result[entity] = struct{}{}
	}
	return result, rows.Err()
}

// GetCrossSessionExplorations returns exploration entries for a specific entity
// from sessions OTHER than excludeSessionID, for the given project. Entries
// with a non-empty finding_summary are returned first (most informative first),
// then by recency. Returns at most limit entries.
//
// Used by get_context to inject a "previously explored" note when the agent is
// about to re-examine an entity already explored in a prior session.
// Sprint 25.4: cross-session exploration dedup.
func (s *Store) GetCrossSessionExplorations(projectID, excludeSessionID, entity string, limit int) ([]ExplorationEntry, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	// Exclude the current session when excludeSessionID is non-empty.
	// The (? = '' OR session_id != ?) pattern handles both cases cleanly:
	//   - excludeSessionID == "" → no session filter (return all prior sessions)
	//   - excludeSessionID != "" → filter out that specific session
	rows, err := s.knowledgeDB.Query(`
		SELECT tool_name, entity_queried, query_context, finding_summary, created_at
		FROM exploration_log
		WHERE project_id    = ?
		  AND entity_queried = ?
		  AND entity_queried != ''
		  AND (? = '' OR session_id != ?)
		ORDER BY
		  CASE WHEN finding_summary != '' THEN 0 ELSE 1 END,
		  created_at DESC
		LIMIT ?`,
		projectID, entity, excludeSessionID, excludeSessionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []ExplorationEntry
	for rows.Next() {
		var e ExplorationEntry
		e.ProjectID = projectID
		if err := rows.Scan(
			&e.ToolName, &e.EntityQueried,
			&e.QueryContext, &e.FindingSummary, &e.CreatedAt,
		); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// GetTopExploredEntities returns the top entities most explored across prior
// sessions for the given project (excluding excludeSessionID), aggregated by
// entity name. Only entities with at least minHits total exploration events are
// returned. Ordered by hit_count DESC, then session_count DESC.
//
// Used by session_init to build the "previously_explored" briefing section so
// agents know which areas were heavily investigated in prior sessions.
// Sprint 25.4: cross-session exploration dedup.
func (s *Store) GetTopExploredEntities(projectID, excludeSessionID string, minHits, limit int) ([]ExploreEntitySummary, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if minHits <= 0 {
		minHits = 1
	}
	// Aggregate by entity_queried to get hit counts and session counts.
	// A correlated subquery fetches the most recent non-empty finding_summary
	// without a separate round-trip. SQLite handles this efficiently at the
	// small scales Synapses operates at (hundreds of exploration_log rows).
	rows, err := s.knowledgeDB.Query(`
		SELECT
		    e1.entity_queried,
		    COUNT(*)                       AS hit_count,
		    COUNT(DISTINCT e1.session_id)  AS session_count,
		    COALESCE((
		        SELECT e2.finding_summary
		        FROM   exploration_log AS e2
		        WHERE  e2.project_id    = e1.project_id
		          AND  e2.entity_queried = e1.entity_queried
		          AND  e2.finding_summary != ''
		          AND  (? = '' OR e2.session_id != ?)
		        ORDER BY e2.created_at DESC
		        LIMIT 1
		    ), '') AS top_finding
		FROM exploration_log AS e1
		WHERE e1.project_id    = ?
		  AND e1.entity_queried != ''
		  AND (? = '' OR e1.session_id != ?)
		GROUP BY e1.entity_queried
		HAVING COUNT(*) >= ?
		ORDER BY hit_count DESC, session_count DESC
		LIMIT ?`,
		// subquery params
		excludeSessionID, excludeSessionID,
		// outer WHERE params
		projectID, excludeSessionID, excludeSessionID,
		// HAVING + LIMIT
		minHits, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ExploreEntitySummary
	for rows.Next() {
		var s ExploreEntitySummary
		var topFinding sql.NullString
		if err := rows.Scan(&s.Entity, &s.HitCount, &s.SessionCount, &topFinding); err != nil {
			continue
		}
		if topFinding.Valid {
			s.TopFinding = topFinding.String
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PruneExplorationLog deletes exploration log entries older than maxAge.
// Called alongside PruneLedger to prevent unbounded growth.
func (s *Store) PruneExplorationLog(maxAge time.Duration) (int64, error) {
	if s.knowledgeDB == nil {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-maxAge).Format("2006-01-02T15:04:05.000Z")
	res, err := s.knowledgeDB.Exec(`DELETE FROM exploration_log WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SessionExplorationLogRaw returns raw JSON of exploration entries for the session.
// Convenience method for tests and diagnostics.
func (s *Store) SessionExplorationLogRaw(sessionID string) (string, error) {
	entries, err := s.GetSessionExplorationLog(sessionID, 50)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

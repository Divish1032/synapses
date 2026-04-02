package store

import (
	"encoding/json"
	"time"
)

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

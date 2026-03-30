package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// LedgerEntry records a single tool call's entity/file signals for cross-session awareness.
type LedgerEntry struct {
	SessionID string   `json:"session_id"`
	ProjectID string   `json:"project_id"`
	ToolName  string   `json:"tool_name"`
	EntityIDs []string `json:"entity_ids,omitempty"`
	FilePaths []string `json:"file_paths,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
}

// SessionWorkSummary aggregates a session's entity/file touchpoints for overlap detection.
type SessionWorkSummary struct {
	SessionID  string   `json:"session_id"`
	AgentID    string   `json:"agent_id"`
	Intent     string   `json:"intent"`
	EntityIDs  []string `json:"entity_ids"`
	FilePaths  []string `json:"file_paths"`
	LastActive string   `json:"last_active"`
}

// AppendLedger writes a single work ledger entry. Safe to call from bgQueue.
func (s *Store) AppendLedger(e LedgerEntry) error {
	if s.knowledgeDB == nil {
		return nil
	}
	entityJSON, _ := json.Marshal(e.EntityIDs)
	fileJSON, _ := json.Marshal(e.FilePaths)
	_, err := s.knowledgeDB.Exec(
		`INSERT INTO work_ledger (session_id, project_id, tool_name, entity_ids, file_paths) VALUES (?,?,?,?,?)`,
		e.SessionID, e.ProjectID, e.ToolName, string(entityJSON), string(fileJSON),
	)
	return err
}

// ActiveSessionWork returns aggregated entity/file sets for all sessions in projectID
// that are NOT excludeSessionID and have been active in the last windowMinutes.
// Returns nil if no overlapping sessions exist.
func (s *Store) ActiveSessionWork(projectID, excludeSessionID string, windowMinutes int) ([]SessionWorkSummary, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	if windowMinutes <= 0 {
		windowMinutes = 15
	}
	rows, err := s.knowledgeDB.Query(`
		SELECT wl.session_id, COALESCE(s.agent_id, '') as agent_id, COALESCE(s.intent, '') as intent,
		       GROUP_CONCAT(wl.entity_ids, '|||') as all_entities,
		       GROUP_CONCAT(wl.file_paths, '|||') as all_files,
		       MAX(wl.created_at) as last_active
		FROM work_ledger wl
		LEFT JOIN sessions s ON s.id = wl.session_id
		WHERE wl.project_id = ? AND wl.session_id != ?
		  AND (s.state IS NULL OR s.state = 'active')
		  AND wl.created_at > datetime('now', ? || ' minutes')
		GROUP BY wl.session_id
		ORDER BY last_active DESC
		LIMIT 10
	`, projectID, excludeSessionID, fmt.Sprintf("-%d", windowMinutes))
	if err != nil {
		return nil, fmt.Errorf("active session work: %w", err)
	}
	defer rows.Close()

	var results []SessionWorkSummary
	for rows.Next() {
		var sw SessionWorkSummary
		var allEntities, allFiles sql.NullString
		if err := rows.Scan(&sw.SessionID, &sw.AgentID, &sw.Intent, &allEntities, &allFiles, &sw.LastActive); err != nil {
			continue
		}
		sw.EntityIDs = deduplicateJSONArrayConcat(allEntities.String, "|||")
		sw.FilePaths = deduplicateJSONArrayConcat(allFiles.String, "|||")
		// Cap to prevent oversized alerts — top 20 entities and files per session.
		if len(sw.EntityIDs) > 20 {
			sw.EntityIDs = sw.EntityIDs[:20]
		}
		if len(sw.FilePaths) > 20 {
			sw.FilePaths = sw.FilePaths[:20]
		}
		if len(sw.EntityIDs) > 0 || len(sw.FilePaths) > 0 {
			results = append(results, sw)
		}
	}
	return results, rows.Err()
}

// SessionLedgerEntities returns the deduplicated set of entity IDs and file paths
// recorded in the work ledger for a given session. Used for session resumption.
func (s *Store) SessionLedgerEntities(sessionID string) (entityIDs, filePaths []string, err error) {
	if s.knowledgeDB == nil {
		return nil, nil, nil
	}
	rows, err := s.knowledgeDB.Query(
		`SELECT entity_ids, file_paths FROM work_ledger WHERE session_id = ?`, sessionID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	entitySet := make(map[string]struct{})
	fileSet := make(map[string]struct{})
	for rows.Next() {
		var eJSON, fJSON string
		if err := rows.Scan(&eJSON, &fJSON); err != nil {
			continue
		}
		var eArr, fArr []string
		_ = json.Unmarshal([]byte(eJSON), &eArr)
		_ = json.Unmarshal([]byte(fJSON), &fArr)
		for _, e := range eArr {
			if e != "" {
				entitySet[e] = struct{}{}
			}
		}
		for _, f := range fArr {
			if f != "" {
				fileSet[f] = struct{}{}
			}
		}
	}
	for e := range entitySet {
		entityIDs = append(entityIDs, e)
	}
	for f := range fileSet {
		filePaths = append(filePaths, f)
	}
	return entityIDs, filePaths, rows.Err()
}

// SessionLedgerEntityCounts returns the frequency count for each entity in the
// session's work ledger. Unlike SessionLedgerEntities which deduplicates, this
// preserves signal strength: an entity appearing in 5 tool calls gets count=5.
// Used for compaction guide entity importance ranking.
func (s *Store) SessionLedgerEntityCounts(sessionID string) (map[string]int, error) {
	counts := make(map[string]int)
	if s.knowledgeDB == nil {
		return counts, nil
	}
	rows, err := s.knowledgeDB.Query(
		`SELECT entity_ids FROM work_ledger WHERE session_id = ?`, sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var eJSON string
		if err := rows.Scan(&eJSON); err != nil {
			continue
		}
		var arr []string
		if json.Unmarshal([]byte(eJSON), &arr) == nil {
			for _, e := range arr {
				if e != "" {
					counts[e]++
				}
			}
		}
	}
	return counts, rows.Err()
}

// PruneLedger deletes work ledger entries older than the given duration.
func (s *Store) PruneLedger(maxAge time.Duration) (int64, error) {
	if s.knowledgeDB == nil {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-maxAge).Format("2006-01-02T15:04:05.000Z")
	res, err := s.knowledgeDB.Exec(`DELETE FROM work_ledger WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// deduplicateJSONArrayConcat parses "['a','b']|||['b','c']" into deduplicated ["a","b","c"].
func deduplicateJSONArrayConcat(raw, sep string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, sep)
	seen := make(map[string]struct{})
	var result []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "[]" {
			continue
		}
		var arr []string
		if err := json.Unmarshal([]byte(part), &arr); err != nil {
			continue
		}
		for _, v := range arr {
			if v == "" {
				continue
			}
			if _, ok := seen[v]; !ok {
				seen[v] = struct{}{}
				result = append(result, v)
			}
		}
	}
	return result
}

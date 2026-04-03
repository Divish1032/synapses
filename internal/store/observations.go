package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ObservationCategory classifies what type of signal a session observation represents.
// Used by the convention extraction engine (Sprint 29.2) to group observations.
const (
	ObsCategoryToolUsage       = "tool_usage"       // Which tools were used and how heavily
	ObsCategoryTestingPattern  = "testing_pattern"  // Testing conventions observed
	ObsCategoryLibraryUsage    = "library_usage"    // Libraries/frameworks detected in touched files
	ObsCategoryApproachOutcome = "approach_outcome" // Session success/failure signal
	ObsCategoryFilePattern     = "file_pattern"     // Architectural layer / directory patterns
	ObsCategoryUserPref        = "user_preference"  // User preference signals from memory saves (Sprint 29.6)
)

// MaxSessionObservationsRows is the per-project row cap for session_observations.
// Old rows are never deleted automatically — the cap only blocks inserts when reached
// so the table never grows unboundedly.
const MaxSessionObservationsRows = 5000

// SessionObservation is a single structured signal extracted from a session
// at end_session time. Multiple observations from the same session form the
// raw material the convention extraction engine (Sprint 29.2) aggregates
// across sessions to identify recurring patterns.
//
// Confidence is a per-observation estimate: 0.0 = noise, 1.0 = certain.
// A single occurrence has confidence 0.3–0.8; 29.2 promotes to conventions
// once a pattern repeats across 3+ sessions with aggregated confidence.
type SessionObservation struct {
	ID         string  `json:"id"`
	SessionID  string  `json:"session_id"`
	ProjectID  string  `json:"project_id"`
	AgentID    string  `json:"agent_id"`
	Category   string  `json:"category"`   // ObsCategory* constant
	Key        string  `json:"key"`        // what was observed (e.g. "uses_testify")
	Value      string  `json:"value"`      // optional detail or count (may be empty)
	Confidence float64 `json:"confidence"` // 0.0–1.0 for this single observation
	CreatedAt  int64   `json:"created_at"` // Unix seconds
}

// InsertSessionObservation persists a new observation. Returns the generated ID.
// Observations are immutable — InsertSessionObservation is the only write path.
// Returns an error (and skips the insert) when the per-project row cap is reached.
func (s *Store) InsertSessionObservation(o SessionObservation) (string, error) {
	if s.knowledgeDB == nil {
		return "", nil
	}
	if o.Category == "" {
		return "", fmt.Errorf("observation category is required")
	}
	if o.Key == "" {
		return "", fmt.Errorf("observation key is required")
	}
	if o.SessionID == "" {
		return "", fmt.Errorf("observation session_id is required")
	}
	if o.ID == "" {
		o.ID = newID()
	}
	if o.CreatedAt == 0 {
		o.CreatedAt = time.Now().UTC().Unix()
	}
	// Clamp confidence to [0, 1].
	if o.Confidence < 0 {
		o.Confidence = 0
	}
	if o.Confidence > 1 {
		o.Confidence = 1
	}

	// Enforce per-project row cap.
	var count int
	if err := s.knowledgeDB.QueryRow(
		`SELECT COUNT(*) FROM session_observations WHERE project_id = ?`, o.ProjectID,
	).Scan(&count); err == nil && count >= MaxSessionObservationsRows {
		return "", fmt.Errorf("session_observations row cap reached (%d/%d)", count, MaxSessionObservationsRows)
	}

	_, err := s.knowledgeDB.Exec(
		`INSERT INTO session_observations
		    (id, session_id, project_id, agent_id, category, key, value, confidence, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		o.ID, o.SessionID, o.ProjectID, o.AgentID,
		o.Category, o.Key, o.Value, o.Confidence, o.CreatedAt,
	)
	if err != nil {
		return "", fmt.Errorf("insert session observation: %w", err)
	}
	return o.ID, nil
}

// GetSessionObservations returns all observations for a specific session,
// ordered newest first. Used by compaction recovery and test assertions.
func (s *Store) GetSessionObservations(sessionID string) ([]SessionObservation, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	rows, err := s.knowledgeDB.Query(
		`SELECT id, session_id, project_id, agent_id, category, key, value, confidence, created_at
		 FROM session_observations
		 WHERE session_id = ?
		 ORDER BY created_at DESC`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("get session observations: %w", err)
	}
	defer rows.Close()
	return scanObservations(rows)
}

// GetObservationsByCategory returns observations for a project and category,
// ordered newest first up to limit. Used by the convention extraction engine
// (Sprint 29.2) to find cross-session patterns.
//
// Pass limit <= 0 for a default of 500.
func (s *Store) GetObservationsByCategory(projectID, category string, limit int) ([]SessionObservation, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.knowledgeDB.Query(
		`SELECT id, session_id, project_id, agent_id, category, key, value, confidence, created_at
		 FROM session_observations
		 WHERE project_id = ? AND category = ?
		 ORDER BY created_at DESC
		 LIMIT ?`,
		projectID, category, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get observations by category: %w", err)
	}
	defer rows.Close()
	return scanObservations(rows)
}

// GetObservationKeyCounts returns a map of key → occurrence count for all
// observations in a project+category. This is the core aggregation primitive
// that 29.2 (convention extraction engine) uses: a key that appears in 3+
// sessions is a convention candidate.
func (s *Store) GetObservationKeyCounts(projectID, category string) (map[string]int, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	rows, err := s.knowledgeDB.Query(
		`SELECT key, COUNT(DISTINCT session_id) AS session_count
		 FROM session_observations
		 WHERE project_id = ? AND category = ?
		 GROUP BY key
		 ORDER BY session_count DESC`,
		projectID, category,
	)
	if err != nil {
		return nil, fmt.Errorf("get observation key counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var key string
		var count int
		if err := rows.Scan(&key, &count); err == nil {
			counts[key] = count
		}
	}
	return counts, rows.Err()
}

// GetObservationKeyMaxValue returns a map of key → maximum integer value stored
// across all observations for that key in a project+category. The Value field
// carries file-count evidence set by the session observation pipeline (e.g.,
// "14" meaning 14 distinct files imported the library in that session).
//
// Returns the per-key maximum across all sessions — a conservative lower bound
// on how widespread the pattern was in the project's most active session.
// Keys with non-numeric or empty Value are excluded from the result.
//
// Returns nil (not an error) when knowledgeDB is unavailable.
func (s *Store) GetObservationKeyMaxValue(projectID, category string) (map[string]int, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	rows, err := s.knowledgeDB.Query(
		`SELECT key, MAX(CAST(value AS INTEGER)) AS max_val
		 FROM session_observations
		 WHERE project_id = ? AND category = ?
		   AND value != '' AND CAST(value AS INTEGER) > 0
		 GROUP BY key`,
		projectID, category,
	)
	if err != nil {
		return nil, fmt.Errorf("get observation key max value: %w", err)
	}
	defer rows.Close()

	maxVals := make(map[string]int)
	for rows.Next() {
		var key string
		var maxVal int
		if err := rows.Scan(&key, &maxVal); err == nil && maxVal > 0 {
			maxVals[key] = maxVal
		}
	}
	return maxVals, rows.Err()
}

// ── Helpers ────────────────────────────────────────────────────────────────────

type observationScanner interface {
	Scan(dest ...any) error
}

func scanObservation(row observationScanner) (*SessionObservation, error) {
	var o SessionObservation
	err := row.Scan(
		&o.ID, &o.SessionID, &o.ProjectID, &o.AgentID,
		&o.Category, &o.Key, &o.Value, &o.Confidence, &o.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &o, err
}

func scanObservations(rows *sql.Rows) ([]SessionObservation, error) {
	var out []SessionObservation
	for rows.Next() {
		o, err := scanObservation(rows)
		if err != nil {
			return out, err
		}
		if o != nil {
			out = append(out, *o)
		}
	}
	return out, rows.Err()
}

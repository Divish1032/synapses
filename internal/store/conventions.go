package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ExtractedConvention is a project-wide convention automatically promoted from
// cross-session observations by the convention extraction engine (Sprint 29.2).
//
// A convention is created when an observation key appears in ≥ MinSessionsForConvention
// distinct sessions. Confidence increases with each additional session that
// confirms the pattern. The convention is delivered to agents at session_init
// time as part of the "conventions" briefing field.
//
// Conventions are upserted (not appended) — the same project/category/key triple
// always maps to a single row whose session_count and confidence grow over time.
type ExtractedConvention struct {
	// ID is the deterministic primary key: projectID + "::" + category + "::" + key.
	ID           string  `json:"id"`
	ProjectID    string  `json:"project_id"`
	Category     string  `json:"category"`
	Key          string  `json:"key"`
	SessionCount int     `json:"session_count"` // distinct sessions that observed this
	Confidence   float64 `json:"confidence"`    // 0.0–1.0; grows with session_count
	Text         string  `json:"text"`          // rendered natural-language convention
	CreatedAt    int64   `json:"created_at"`    // Unix seconds — set on first insert
	UpdatedAt    int64   `json:"updated_at"`    // Unix seconds — set on every upsert
}

// ConventionID returns the deterministic primary key for a convention.
func ConventionID(projectID, category, key string) string {
	return projectID + "::" + category + "::" + key
}

// UpsertConvention inserts a new convention or updates an existing one.
// The ID field is used as the primary key. On conflict, session_count,
// confidence, text, and updated_at are updated; created_at is preserved.
//
// Returns nil immediately when knowledgeDB is unavailable.
func (s *Store) UpsertConvention(c ExtractedConvention) error {
	if s.knowledgeDB == nil {
		return nil
	}
	if c.ID == "" {
		return fmt.Errorf("convention ID is required")
	}
	if c.ProjectID == "" {
		return fmt.Errorf("convention project_id is required")
	}
	if c.Category == "" {
		return fmt.Errorf("convention category is required")
	}
	if c.Key == "" {
		return fmt.Errorf("convention key is required")
	}
	now := time.Now().UTC().Unix()
	if c.CreatedAt == 0 {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	// Clamp confidence to [0, 1].
	if c.Confidence < 0 {
		c.Confidence = 0
	}
	if c.Confidence > 1 {
		c.Confidence = 1
	}

	_, err := s.knowledgeDB.Exec(`
		INSERT INTO extracted_conventions
		    (id, project_id, category, key, session_count, confidence, text, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    session_count = excluded.session_count,
		    confidence    = excluded.confidence,
		    text          = excluded.text,
		    updated_at    = excluded.updated_at`,
		c.ID, c.ProjectID, c.Category, c.Key,
		c.SessionCount, c.Confidence, c.Text, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert convention: %w", err)
	}
	return nil
}

// GetProjectConventions returns all conventions for a project with confidence
// ≥ minConfidence, ordered by confidence DESC then session_count DESC.
//
// Pass minConfidence = 0 to retrieve all conventions regardless of confidence.
// Returns nil (not an error) when knowledgeDB is unavailable.
func (s *Store) GetProjectConventions(projectID string, minConfidence float64) ([]ExtractedConvention, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	if minConfidence < 0 {
		minConfidence = 0
	}
	rows, err := s.knowledgeDB.Query(`
		SELECT id, project_id, category, key, session_count, confidence, text, created_at, updated_at
		FROM extracted_conventions
		WHERE project_id = ? AND confidence >= ?
		ORDER BY confidence DESC, session_count DESC`,
		projectID, minConfidence,
	)
	if err != nil {
		return nil, fmt.Errorf("get project conventions: %w", err)
	}
	defer rows.Close()
	return scanConventions(rows)
}

// ── Helpers ────────────────────────────────────────────────────────────────────

type conventionScanner interface {
	Scan(dest ...any) error
}

func scanConvention(row conventionScanner) (*ExtractedConvention, error) {
	var c ExtractedConvention
	err := row.Scan(
		&c.ID, &c.ProjectID, &c.Category, &c.Key,
		&c.SessionCount, &c.Confidence, &c.Text, &c.CreatedAt, &c.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &c, err
}

func scanConventions(rows *sql.Rows) ([]ExtractedConvention, error) {
	var out []ExtractedConvention
	for rows.Next() {
		c, err := scanConvention(rows)
		if err != nil {
			return out, err
		}
		if c != nil {
			out = append(out, *c)
		}
	}
	return out, rows.Err()
}

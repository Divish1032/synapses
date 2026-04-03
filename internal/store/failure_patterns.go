package store

import (
	"database/sql"
	"fmt"
	"time"
)

// FailurePattern is a cross-session failure signal promoted from project-wide
// rejected_approach records by the failure avoidance engine (Sprint 29.4).
//
// A pattern is created when the same keyword (library name, package name)
// appears in 2+ rejected_approach records for a project. The occurrence_count
// grows as more approaches are abandoned with the same keyword.
//
// Patterns are delivered to agents at session_init time as _briefing.failure_avoidance
// warnings — natural language only, no code snippets.
type FailurePattern struct {
	// ID is the deterministic primary key: projectID + "::" + keyword.
	ID              string  `json:"id"`
	ProjectID       string  `json:"project_id"`
	Keyword         string  `json:"keyword"`          // extracted keyword (e.g., "jwt-go", "fasthttp")
	PatternType     string  `json:"pattern_type"`     // "library" | "package"
	OccurrenceCount int     `json:"occurrence_count"` // number of rejected_approach records matching
	SampleApproach  string  `json:"sample_approach"`  // most recent approach text (for NL generation)
	SampleReason    string  `json:"sample_reason"`    // most recent failure_reason (for NL generation)
	Confidence      float64 `json:"confidence"`       // 0.0–1.0; grows with occurrence_count
	Text            string  `json:"text"`             // rendered natural-language warning
	CreatedAt       int64   `json:"created_at"`       // Unix seconds — set on first insert
	UpdatedAt       int64   `json:"updated_at"`       // Unix seconds — set on every upsert
}

// FailurePatternID returns the deterministic primary key for a failure pattern.
func FailurePatternID(projectID, keyword string) string {
	return projectID + "::" + keyword
}

// UpsertFailurePattern inserts a new failure pattern or updates an existing one.
// On conflict the occurrence_count, confidence, text, and updated_at fields are
// updated; created_at is preserved (first-seen timestamp).
//
// Returns nil immediately when knowledgeDB is unavailable.
func (s *Store) UpsertFailurePattern(fp FailurePattern) error {
	if s.knowledgeDB == nil {
		return nil
	}
	if fp.ID == "" {
		return fmt.Errorf("failure pattern ID is required")
	}
	if fp.ProjectID == "" {
		return fmt.Errorf("failure pattern project_id is required")
	}
	if fp.Keyword == "" {
		return fmt.Errorf("failure pattern keyword is required")
	}
	now := time.Now().UTC().Unix()
	if fp.CreatedAt == 0 {
		fp.CreatedAt = now
	}
	fp.UpdatedAt = now
	// Clamp confidence to [0, 1].
	if fp.Confidence < 0 {
		fp.Confidence = 0
	}
	if fp.Confidence > 1 {
		fp.Confidence = 1
	}

	_, err := s.knowledgeDB.Exec(`
		INSERT INTO extracted_failure_patterns
		    (id, project_id, keyword, pattern_type, occurrence_count,
		     sample_approach, sample_reason, confidence, text, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    occurrence_count = excluded.occurrence_count,
		    sample_approach  = excluded.sample_approach,
		    sample_reason    = excluded.sample_reason,
		    confidence       = excluded.confidence,
		    text             = excluded.text,
		    updated_at       = excluded.updated_at`,
		fp.ID, fp.ProjectID, fp.Keyword, fp.PatternType, fp.OccurrenceCount,
		fp.SampleApproach, fp.SampleReason, fp.Confidence, fp.Text,
		fp.CreatedAt, fp.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert failure pattern: %w", err)
	}
	return nil
}

// GetProjectFailurePatterns returns all failure patterns for a project with
// confidence ≥ minConfidence, ordered by confidence DESC then occurrence_count DESC.
//
// Pass minConfidence = 0 to retrieve all patterns regardless of confidence.
// Returns nil (not an error) when knowledgeDB is unavailable.
func (s *Store) GetProjectFailurePatterns(projectID string, minConfidence float64) ([]FailurePattern, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	if minConfidence < 0 {
		minConfidence = 0
	}
	rows, err := s.knowledgeDB.Query(`
		SELECT id, project_id, keyword, pattern_type, occurrence_count,
		       sample_approach, sample_reason, confidence, text, created_at, updated_at
		FROM extracted_failure_patterns
		WHERE project_id = ? AND confidence >= ?
		ORDER BY confidence DESC, occurrence_count DESC`,
		projectID, minConfidence,
	)
	if err != nil {
		return nil, fmt.Errorf("get project failure patterns: %w", err)
	}
	defer rows.Close()
	return scanFailurePatterns(rows)
}

// ── Helpers ────────────────────────────────────────────────────────────────────

type failurePatternScanner interface {
	Scan(dest ...any) error
}

func scanFailurePattern(row failurePatternScanner) (*FailurePattern, error) {
	var fp FailurePattern
	err := row.Scan(
		&fp.ID, &fp.ProjectID, &fp.Keyword, &fp.PatternType, &fp.OccurrenceCount,
		&fp.SampleApproach, &fp.SampleReason, &fp.Confidence, &fp.Text,
		&fp.CreatedAt, &fp.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &fp, err
}

func scanFailurePatterns(rows *sql.Rows) ([]FailurePattern, error) {
	var out []FailurePattern
	for rows.Next() {
		fp, err := scanFailurePattern(rows)
		if err != nil {
			return out, err
		}
		if fp != nil {
			out = append(out, *fp)
		}
	}
	return out, rows.Err()
}

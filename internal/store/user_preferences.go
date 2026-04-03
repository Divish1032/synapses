package store

import (
	"database/sql"
	"fmt"
	"time"
)

// UserPreference is a cross-session user preference signal promoted from
// project-wide manual memory saves by the user preference engine (Sprint 29.6).
//
// A preference is created when the same normalized preference phrase appears
// in ≥ MinOccurrencesForUserPref memory records for a project. The
// occurrence_count grows as the agent saves more memories mentioning the same
// preference.
//
// Preferences are delivered to agents at session_init as _briefing.preferences
// — natural language only ("User prefers bundled PRs for refactors (seen 3 times)").
type UserPreference struct {
	// ID is the deterministic primary key: projectID + "::" + prefKey.
	ID              string  `json:"id"`
	ProjectID       string  `json:"project_id"`
	PrefKey         string  `json:"pref_key"`         // normalized preference phrase (first 60 chars, lowercase)
	Text            string  `json:"text"`             // rendered natural-language preference
	OccurrenceCount int     `json:"occurrence_count"` // number of memory records matching
	Confidence      float64 `json:"confidence"`       // 0.0–1.0; grows with occurrence_count
	CreatedAt       int64   `json:"created_at"`       // Unix seconds — set on first insert
	UpdatedAt       int64   `json:"updated_at"`       // Unix seconds — set on every upsert
}

// UserPreferenceID returns the deterministic primary key for a user preference.
func UserPreferenceID(projectID, prefKey string) string {
	return projectID + "::" + prefKey
}

// UpsertUserPreference inserts a new user preference or updates an existing one.
// On conflict the occurrence_count, confidence, text, and updated_at fields are
// updated; created_at is preserved (first-seen timestamp).
//
// Returns nil immediately when knowledgeDB is unavailable.
func (s *Store) UpsertUserPreference(up UserPreference) error {
	if s.knowledgeDB == nil {
		return nil
	}
	if up.ID == "" {
		return fmt.Errorf("user preference ID is required")
	}
	if up.ProjectID == "" {
		return fmt.Errorf("user preference project_id is required")
	}
	if up.PrefKey == "" {
		return fmt.Errorf("user preference pref_key is required")
	}
	now := time.Now().UTC().Unix()
	if up.CreatedAt == 0 {
		up.CreatedAt = now
	}
	up.UpdatedAt = now
	// Clamp confidence to [0, 1].
	if up.Confidence < 0 {
		up.Confidence = 0
	}
	if up.Confidence > 1 {
		up.Confidence = 1
	}

	_, err := s.knowledgeDB.Exec(`
		INSERT INTO user_preferences
		    (id, project_id, pref_key, text, occurrence_count, confidence, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    occurrence_count = excluded.occurrence_count,
		    confidence       = excluded.confidence,
		    text             = excluded.text,
		    updated_at       = excluded.updated_at`,
		up.ID, up.ProjectID, up.PrefKey, up.Text, up.OccurrenceCount,
		up.Confidence, up.CreatedAt, up.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert user preference: %w", err)
	}
	return nil
}

// GetProjectUserPreferences returns all user preferences for a project with
// confidence ≥ minConfidence, ordered by confidence DESC then occurrence_count DESC.
//
// Pass minConfidence = 0 to retrieve all preferences regardless of confidence.
// Returns nil (not an error) when knowledgeDB is unavailable.
func (s *Store) GetProjectUserPreferences(projectID string, minConfidence float64) ([]UserPreference, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	if minConfidence < 0 {
		minConfidence = 0
	}
	rows, err := s.knowledgeDB.Query(`
		SELECT id, project_id, pref_key, text, occurrence_count, confidence, created_at, updated_at
		FROM user_preferences
		WHERE project_id = ? AND confidence >= ?
		ORDER BY confidence DESC, occurrence_count DESC`,
		projectID, minConfidence,
	)
	if err != nil {
		return nil, fmt.Errorf("get project user preferences: %w", err)
	}
	defer rows.Close()
	return scanUserPreferences(rows)
}

// ── Helpers ────────────────────────────────────────────────────────────────────

type userPreferenceScanner interface {
	Scan(dest ...any) error
}

func scanUserPreference(row userPreferenceScanner) (*UserPreference, error) {
	var up UserPreference
	err := row.Scan(
		&up.ID, &up.ProjectID, &up.PrefKey, &up.Text, &up.OccurrenceCount,
		&up.Confidence, &up.CreatedAt, &up.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &up, err
}

func scanUserPreferences(rows *sql.Rows) ([]UserPreference, error) {
	var out []UserPreference
	for rows.Next() {
		up, err := scanUserPreference(rows)
		if err != nil {
			return out, err
		}
		if up != nil {
			out = append(out, *up)
		}
	}
	return out, rows.Err()
}

package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Hypothesis represents an agent's working theory about the codebase — a belief
// held during a session that can be confirmed or invalidated as evidence accumulates.
// Unlike episodes (append-only), hypotheses are mutable: state transitions from
// active → confirmed or active → rejected as the agent learns more.
type Hypothesis struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	ProjectID string `json:"project_id,omitempty"`
	Content   string `json:"content"`            // "I think the bug is in X because Y"
	State     string `json:"state"`              // active, confirmed, rejected
	Evidence  string `json:"evidence,omitempty"` // supporting or refuting text
	CreatedAt int64  `json:"created_at"`         // Unix seconds
	UpdatedAt int64  `json:"updated_at"`         // Unix seconds
}

// HypothesisState values.
const (
	HypothesisStateActive    = "active"
	HypothesisStateConfirmed = "confirmed"
	HypothesisStateRejected  = "rejected"
)

// DefaultMaxHypothesesRows is the per-project hypothesis cap.
const DefaultMaxHypothesesRows = 500

// validHypothesisStates is the set of accepted state values.
var validHypothesisStates = map[string]bool{
	HypothesisStateActive:    true,
	HypothesisStateConfirmed: true,
	HypothesisStateRejected:  true,
}

// InsertHypothesis creates a new hypothesis in the active state.
// Returns the generated ID.
func (s *Store) InsertHypothesis(h Hypothesis) (string, error) {
	// Enforce row cap using the same mutex that serializes memory writes.
	s.rowCapMu.Lock()
	defer s.rowCapMu.Unlock()

	var count int
	if err := s.knowledgeDB.QueryRow(`SELECT COUNT(*) FROM hypotheses WHERE project_id = ?`, h.ProjectID).Scan(&count); err == nil && count >= DefaultMaxHypothesesRows {
		return "", fmt.Errorf("hypothesis row cap reached (%d/%d) — reject old hypotheses or increase the cap", count, DefaultMaxHypothesesRows)
	}

	if h.ID == "" {
		h.ID = newID()
	}
	now := time.Now().Unix()
	if h.CreatedAt == 0 {
		h.CreatedAt = now
	}
	if h.UpdatedAt == 0 {
		h.UpdatedAt = now
	}
	if h.State == "" {
		h.State = HypothesisStateActive
	}
	if !validHypothesisStates[h.State] {
		return "", fmt.Errorf("invalid hypothesis state %q: must be active, confirmed, or rejected", h.State)
	}

	if _, err := s.knowledgeDB.Exec(`
		INSERT INTO hypotheses (id, agent_id, project_id, content, state, evidence, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.AgentID, h.ProjectID, h.Content, h.State, h.Evidence, h.CreatedAt, h.UpdatedAt,
	); err != nil {
		return "", fmt.Errorf("insert hypothesis: %w", err)
	}
	return h.ID, nil
}

// UpdateHypothesisState changes the state of an existing hypothesis and appends
// evidence. Returns the updated Hypothesis so callers can read the content for
// invalidation prompts.
func (s *Store) UpdateHypothesisState(id, state, evidence string) (*Hypothesis, error) {
	if !validHypothesisStates[state] {
		return nil, fmt.Errorf("invalid hypothesis state %q: must be active, confirmed, or rejected", state)
	}
	now := time.Now().Unix()
	result, err := s.knowledgeDB.Exec(`
		UPDATE hypotheses
		SET state = ?, evidence = CASE WHEN ? != '' THEN ? ELSE evidence END, updated_at = ?
		WHERE id = ?`,
		state, evidence, evidence, now, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update hypothesis state: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return nil, fmt.Errorf("hypothesis %q not found", id)
	}
	return s.GetHypothesisByID(id)
}

// GetHypothesisByID returns a single hypothesis by ID.
func (s *Store) GetHypothesisByID(id string) (*Hypothesis, error) {
	row := s.knowledgeDB.QueryRow(`
		SELECT id, agent_id, project_id, content, state, evidence, created_at, updated_at
		FROM hypotheses WHERE id = ?`, id)
	h, err := scanHypothesis(row)
	if err != nil {
		return nil, fmt.Errorf("get hypothesis %q: %w", id, err)
	}
	return h, nil
}

// GetActiveHypotheses returns ACTIVE hypotheses for the given agent and project,
// newest first, up to the specified limit. Used by compaction recovery.
func (s *Store) GetActiveHypotheses(agentID, projectID string, limit int) ([]Hypothesis, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.knowledgeDB.Query(`
		SELECT id, agent_id, project_id, content, state, evidence, created_at, updated_at
		FROM hypotheses
		WHERE agent_id = ? AND project_id = ? AND state = 'active'
		ORDER BY created_at DESC
		LIMIT ?`,
		agentID, projectID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get active hypotheses: %w", err)
	}
	defer rows.Close()
	return scanHypotheses(rows)
}

// GetHypotheses returns all hypotheses for the agent and project, optionally
// filtered by state ("" means all states). Newest first, up to limit.
func (s *Store) GetHypotheses(agentID, projectID, stateFilter string, limit int) ([]Hypothesis, error) {
	if limit <= 0 {
		limit = 20
	}
	var (
		rows *sql.Rows
		err  error
	)
	if stateFilter == "" || !validHypothesisStates[stateFilter] {
		rows, err = s.knowledgeDB.Query(`
			SELECT id, agent_id, project_id, content, state, evidence, created_at, updated_at
			FROM hypotheses
			WHERE agent_id = ? AND project_id = ?
			ORDER BY created_at DESC
			LIMIT ?`,
			agentID, projectID, limit,
		)
	} else {
		rows, err = s.knowledgeDB.Query(`
			SELECT id, agent_id, project_id, content, state, evidence, created_at, updated_at
			FROM hypotheses
			WHERE agent_id = ? AND project_id = ? AND state = ?
			ORDER BY created_at DESC
			LIMIT ?`,
			agentID, projectID, stateFilter, limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("get hypotheses: %w", err)
	}
	defer rows.Close()
	return scanHypotheses(rows)
}

// SearchHypotheses performs a case-insensitive keyword search across content
// and evidence fields for the given project. When agentID is non-empty, results
// are scoped to that agent; otherwise all agents in the project are searched.
// When query is empty, returns the most recent hypotheses.
// Used by intent-aware memory retrieval (Sprint 25.3).
func (s *Store) SearchHypotheses(agentID, projectID, query string, limit int) ([]Hypothesis, error) {
	if limit <= 0 {
		limit = 20
	}

	var (
		rows *sql.Rows
		err  error
	)
	query = strings.TrimSpace(query)
	if query == "" {
		if agentID == "" {
			rows, err = s.knowledgeDB.Query(`
				SELECT id, agent_id, project_id, content, state, evidence, created_at, updated_at
				FROM hypotheses
				WHERE project_id = ?
				ORDER BY created_at DESC
				LIMIT ?`,
				projectID, limit,
			)
		} else {
			rows, err = s.knowledgeDB.Query(`
				SELECT id, agent_id, project_id, content, state, evidence, created_at, updated_at
				FROM hypotheses
				WHERE agent_id = ? AND project_id = ?
				ORDER BY created_at DESC
				LIMIT ?`,
				agentID, projectID, limit,
			)
		}
	} else {
		like := "%" + escapeLike(query) + "%"
		if agentID == "" {
			rows, err = s.knowledgeDB.Query(`
				SELECT id, agent_id, project_id, content, state, evidence, created_at, updated_at
				FROM hypotheses
				WHERE project_id = ?
				  AND (content LIKE ? ESCAPE '\'
				    OR evidence LIKE ? ESCAPE '\')
				ORDER BY created_at DESC
				LIMIT ?`,
				projectID, like, like, limit,
			)
		} else {
			rows, err = s.knowledgeDB.Query(`
				SELECT id, agent_id, project_id, content, state, evidence, created_at, updated_at
				FROM hypotheses
				WHERE agent_id = ? AND project_id = ?
				  AND (content LIKE ? ESCAPE '\'
				    OR evidence LIKE ? ESCAPE '\')
				ORDER BY created_at DESC
				LIMIT ?`,
				agentID, projectID, like, like, limit,
			)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("search hypotheses: %w", err)
	}
	defer rows.Close()
	return scanHypotheses(rows)
}

// ── scanning helpers ──────────────────────────────────────────────────────────

type hypothesisScanner interface {
	Scan(dest ...interface{}) error
}

func scanHypothesis(row hypothesisScanner) (*Hypothesis, error) {
	var h Hypothesis
	if err := row.Scan(&h.ID, &h.AgentID, &h.ProjectID, &h.Content, &h.State, &h.Evidence, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	return &h, nil
}

func scanHypotheses(rows *sql.Rows) ([]Hypothesis, error) {
	var out []Hypothesis
	for rows.Next() {
		h, err := scanHypothesis(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Decision captures a structured architectural or implementation decision —
// what was chosen, what alternatives were evaluated, why this choice was made,
// and the context in which it was decided. Unlike hypotheses (mutable, state
// machine), decisions are immutable: once recorded they stand as a permanent
// audit trail. Future sessions retrieve them via memory(action="list_decisions")
// or see them in the compaction recovery packet.
type Decision struct {
	ID           string `json:"id"`
	AgentID      string `json:"agent_id"`
	ProjectID    string `json:"project_id,omitempty"`
	Choice       string `json:"choice"`                // what was decided/chosen
	Alternatives string `json:"alternatives,omitempty"` // what else was considered (free text)
	Reasoning    string `json:"reasoning,omitempty"`    // why this choice
	Context      string `json:"context,omitempty"`      // when/where (e.g. "adding OAuth to /api/auth")
	CreatedAt    int64  `json:"created_at"`             // Unix seconds
}

// DefaultMaxDecisionsRows is the per-project decision cap.
const DefaultMaxDecisionsRows = 500

// InsertDecision persists a new decision record. Returns the generated ID.
// Decisions are immutable — InsertDecision is the only mutation path.
func (s *Store) InsertDecision(d Decision) (string, error) {
	if d.Choice == "" {
		return "", fmt.Errorf("decision choice is required")
	}

	// Enforce row cap using the same mutex that serialises memory writes.
	s.rowCapMu.Lock()
	defer s.rowCapMu.Unlock()

	var count int
	if err := s.knowledgeDB.QueryRow(
		`SELECT COUNT(*) FROM decisions WHERE project_id = ?`, d.ProjectID,
	).Scan(&count); err == nil && count >= DefaultMaxDecisionsRows {
		return "", fmt.Errorf("decision row cap reached (%d/%d) — prune old decisions to continue", count, DefaultMaxDecisionsRows)
	}

	if d.ID == "" {
		d.ID = newID()
	}
	if d.CreatedAt == 0 {
		d.CreatedAt = time.Now().Unix()
	}

	if _, err := s.knowledgeDB.Exec(`
		INSERT INTO decisions (id, agent_id, project_id, choice, alternatives, reasoning, context, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.AgentID, d.ProjectID, d.Choice, d.Alternatives, d.Reasoning, d.Context, d.CreatedAt,
	); err != nil {
		return "", fmt.Errorf("insert decision: %w", err)
	}
	return d.ID, nil
}

// GetDecisionByID returns a single decision by its ID.
func (s *Store) GetDecisionByID(id string) (*Decision, error) {
	row := s.knowledgeDB.QueryRow(`
		SELECT id, agent_id, project_id, choice, alternatives, reasoning, context, created_at
		FROM decisions WHERE id = ?`, id)
	d, err := scanDecision(row)
	if err != nil {
		return nil, fmt.Errorf("get decision %q: %w", id, err)
	}
	return d, nil
}

// GetRecentDecisions returns the most recent decisions for the agent and project,
// newest first, up to limit. Used by compaction recovery and session_init.
func (s *Store) GetRecentDecisions(agentID, projectID string, limit int) ([]Decision, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.knowledgeDB.Query(`
		SELECT id, agent_id, project_id, choice, alternatives, reasoning, context, created_at
		FROM decisions
		WHERE agent_id = ? AND project_id = ?
		ORDER BY created_at DESC
		LIMIT ?`,
		agentID, projectID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get recent decisions: %w", err)
	}
	defer rows.Close()
	return scanDecisions(rows)
}

// SearchDecisions performs a case-insensitive keyword search across choice,
// reasoning, and context fields for the given agent and project.
// When query is empty, returns the most recent decisions (equivalent to GetRecentDecisions).
func (s *Store) SearchDecisions(agentID, projectID, query string, limit int) ([]Decision, error) {
	if limit <= 0 {
		limit = 20
	}

	var (
		rows *sql.Rows
		err  error
	)
	query = strings.TrimSpace(query)
	if query == "" {
		rows, err = s.knowledgeDB.Query(`
			SELECT id, agent_id, project_id, choice, alternatives, reasoning, context, created_at
			FROM decisions
			WHERE agent_id = ? AND project_id = ?
			ORDER BY created_at DESC
			LIMIT ?`,
			agentID, projectID, limit,
		)
	} else {
		like := "%" + query + "%"
		rows, err = s.knowledgeDB.Query(`
			SELECT id, agent_id, project_id, choice, alternatives, reasoning, context, created_at
			FROM decisions
			WHERE agent_id = ? AND project_id = ?
			  AND (choice LIKE ? OR reasoning LIKE ? OR context LIKE ? OR alternatives LIKE ?)
			ORDER BY created_at DESC
			LIMIT ?`,
			agentID, projectID, like, like, like, like, limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("search decisions: %w", err)
	}
	defer rows.Close()
	return scanDecisions(rows)
}

// ── scanning helpers ──────────────────────────────────────────────────────────

type decisionScanner interface {
	Scan(dest ...interface{}) error
}

func scanDecision(row decisionScanner) (*Decision, error) {
	var d Decision
	if err := row.Scan(
		&d.ID, &d.AgentID, &d.ProjectID,
		&d.Choice, &d.Alternatives, &d.Reasoning, &d.Context,
		&d.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &d, nil
}

func scanDecisions(rows *sql.Rows) ([]Decision, error) {
	var out []Decision
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

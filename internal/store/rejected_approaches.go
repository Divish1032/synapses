package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// RejectedApproach captures an explicitly abandoned implementation approach —
// what was tried, why it failed, and what specific error or blocker was hit.
// Unlike hypotheses (mutable state machine), rejected approaches are immutable:
// they represent a permanent record of a path that was explored and abandoned.
//
// Future sessions see these at session_init time: "A previous session tried
// this approach and abandoned it because [failure_reason]."
//
// Agent records via: memory(action="abandon")
// Agent retrieves via: memory(action="list_rejected")
type RejectedApproach struct {
	ID            string `json:"id"`
	AgentID       string `json:"agent_id"`
	ProjectID     string `json:"project_id,omitempty"`
	Approach      string `json:"approach"`                 // what was tried (required)
	FailureReason string `json:"failure_reason"`           // why it failed (required)
	Blocker       string `json:"blocker,omitempty"`        // specific error/exception/blocker text
	Context       string `json:"context,omitempty"`        // when/where (e.g. "adding OAuth flow to /api/auth")
	CreatedAt     int64  `json:"created_at"`               // Unix seconds
}

// DefaultMaxRejectedApproachesRows is the per-project rejected approach row cap.
const DefaultMaxRejectedApproachesRows = 500

// InsertRejectedApproach persists a new rejected approach record. Returns the
// generated ID. Records are immutable — InsertRejectedApproach is the only
// mutation path.
func (s *Store) InsertRejectedApproach(r RejectedApproach) (string, error) {
	if r.Approach == "" {
		return "", fmt.Errorf("approach is required")
	}
	if r.FailureReason == "" {
		return "", fmt.Errorf("failure_reason is required")
	}

	// Enforce row cap under the same mutex that serialises memory writes.
	s.rowCapMu.Lock()
	defer s.rowCapMu.Unlock()

	var count int
	if err := s.knowledgeDB.QueryRow(
		`SELECT COUNT(*) FROM rejected_approaches WHERE project_id = ?`, r.ProjectID,
	).Scan(&count); err == nil && count >= DefaultMaxRejectedApproachesRows {
		return "", fmt.Errorf("rejected_approaches row cap reached (%d/%d) — old entries can be disregarded once no longer relevant", count, DefaultMaxRejectedApproachesRows)
	}

	if r.ID == "" {
		r.ID = newID()
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().Unix()
	}

	if _, err := s.knowledgeDB.Exec(`
		INSERT INTO rejected_approaches (id, agent_id, project_id, approach, failure_reason, blocker, context, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.AgentID, r.ProjectID, r.Approach, r.FailureReason, r.Blocker, r.Context, r.CreatedAt,
	); err != nil {
		return "", fmt.Errorf("insert rejected approach: %w", err)
	}
	return r.ID, nil
}

// GetRecentRejectedApproaches returns the most recent rejected approaches for
// the agent and project, newest first, up to limit. Used by compaction recovery
// and session_init to warn the agent away from repeated failures.
func (s *Store) GetRecentRejectedApproaches(agentID, projectID string, limit int) ([]RejectedApproach, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.knowledgeDB.Query(`
		SELECT id, agent_id, project_id, approach, failure_reason, blocker, context, created_at
		FROM rejected_approaches
		WHERE agent_id = ? AND project_id = ?
		ORDER BY created_at DESC
		LIMIT ?`,
		agentID, projectID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get recent rejected approaches: %w", err)
	}
	defer rows.Close()
	return scanRejectedApproaches(rows)
}

// SearchRejectedApproaches performs a case-insensitive keyword search across
// approach, failure_reason, blocker, and context fields for the given project.
// When agentID is non-empty, results are scoped to that agent; otherwise all
// agents in the project are searched (project-wide intent-aware retrieval).
// When query is empty, returns the most recent entries.
func (s *Store) SearchRejectedApproaches(agentID, projectID, query string, limit int) ([]RejectedApproach, error) {
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
				SELECT id, agent_id, project_id, approach, failure_reason, blocker, context, created_at
				FROM rejected_approaches
				WHERE project_id = ?
				ORDER BY created_at DESC
				LIMIT ?`,
				projectID, limit,
			)
		} else {
			rows, err = s.knowledgeDB.Query(`
				SELECT id, agent_id, project_id, approach, failure_reason, blocker, context, created_at
				FROM rejected_approaches
				WHERE agent_id = ? AND project_id = ?
				ORDER BY created_at DESC
				LIMIT ?`,
				agentID, projectID, limit,
			)
		}
	} else {
		// escapeLike guards against LIKE metacharacters (%, _) in the query.
		like := "%" + escapeLike(query) + "%"
		if agentID == "" {
			rows, err = s.knowledgeDB.Query(`
				SELECT id, agent_id, project_id, approach, failure_reason, blocker, context, created_at
				FROM rejected_approaches
				WHERE project_id = ?
				  AND (approach LIKE ? ESCAPE '\'
				    OR failure_reason LIKE ? ESCAPE '\'
				    OR blocker LIKE ? ESCAPE '\'
				    OR context LIKE ? ESCAPE '\')
				ORDER BY created_at DESC
				LIMIT ?`,
				projectID, like, like, like, like, limit,
			)
		} else {
			rows, err = s.knowledgeDB.Query(`
				SELECT id, agent_id, project_id, approach, failure_reason, blocker, context, created_at
				FROM rejected_approaches
				WHERE agent_id = ? AND project_id = ?
				  AND (approach LIKE ? ESCAPE '\'
				    OR failure_reason LIKE ? ESCAPE '\'
				    OR blocker LIKE ? ESCAPE '\'
				    OR context LIKE ? ESCAPE '\')
				ORDER BY created_at DESC
				LIMIT ?`,
				agentID, projectID, like, like, like, like, limit,
			)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("search rejected approaches: %w", err)
	}
	defer rows.Close()
	return scanRejectedApproaches(rows)
}

// GetRejectedApproachesInRange returns rejected approaches for the given agent
// and project whose created_at falls in [since, until] (Unix seconds, inclusive).
// Use since=0 to skip the lower bound. Used by the deterministic Archivist to
// scope failures to the current session window.
func (s *Store) GetRejectedApproachesInRange(agentID, projectID string, since, until int64, limit int) ([]RejectedApproach, error) {
	if s.knowledgeDB == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	var (
		rows *sql.Rows
		err  error
	)
	if since > 0 {
		rows, err = s.knowledgeDB.Query(`
			SELECT id, agent_id, project_id, approach, failure_reason, blocker, context, created_at
			FROM rejected_approaches
			WHERE agent_id = ? AND project_id = ? AND created_at >= ? AND created_at <= ?
			ORDER BY created_at DESC
			LIMIT ?`,
			agentID, projectID, since, until, limit,
		)
	} else {
		rows, err = s.knowledgeDB.Query(`
			SELECT id, agent_id, project_id, approach, failure_reason, blocker, context, created_at
			FROM rejected_approaches
			WHERE agent_id = ? AND project_id = ? AND created_at <= ?
			ORDER BY created_at DESC
			LIMIT ?`,
			agentID, projectID, until, limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("get rejected approaches in range: %w", err)
	}
	defer rows.Close()
	return scanRejectedApproaches(rows)
}

// ── scanning helpers ──────────────────────────────────────────────────────────

type rejectedApproachScanner interface {
	Scan(dest ...interface{}) error
}

func scanRejectedApproach(row rejectedApproachScanner) (*RejectedApproach, error) {
	var r RejectedApproach
	if err := row.Scan(
		&r.ID, &r.AgentID, &r.ProjectID,
		&r.Approach, &r.FailureReason, &r.Blocker, &r.Context,
		&r.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &r, nil
}

func scanRejectedApproaches(rows *sql.Rows) ([]RejectedApproach, error) {
	var out []RejectedApproach
	for rows.Next() {
		r, err := scanRejectedApproach(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

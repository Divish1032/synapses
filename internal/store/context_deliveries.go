package store

import "time"

// ContextDelivery is a single recorded get_context or prepare_context call.
// Used by the Sprint 11 feedback loop to measure context quality outcomes.
type ContextDelivery struct {
	SessionID   string // Synapses session UUID (from sessions table); may be empty
	AgentID     string // agent_id from request; may be empty
	ToolName    string // "get_context" or "prepare_context"
	Entity      string // entity/target queried
	Refetched   bool   // true when this is a repeat request for the same entity in the same session
	TaskOutcome string // "", "success", "unknown" — populated at end_session via CorrelateSessionOutcome
}

// InsertContextDelivery records a context delivery row in knowledge.db.
// Safe to call from a goroutine — uses the single knowledgeDB connection (WAL mode).
// Errors are silently swallowed: instrumentation must never affect hot-path callers.
// Rows with empty ToolName are skipped to prevent dirty data in quality analysis.
func (s *Store) InsertContextDelivery(cd ContextDelivery) {
	if s == nil || s.knowledgeDB == nil {
		return
	}
	// ToolName is the primary grouping key for Sprint 11 analysis; skip rows without it.
	if cd.ToolName == "" {
		return
	}
	refetchedInt := 0
	if cd.Refetched {
		refetchedInt = 1
	}
	_, _ = s.knowledgeDB.Exec(
		`INSERT INTO context_deliveries (session_id, agent_id, tool_name, entity, refetched, task_outcome, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		cd.SessionID, cd.AgentID, cd.ToolName, cd.Entity,
		refetchedInt, cd.TaskOutcome, time.Now().UTC().Unix(),
	)
}

// CorrelateSessionOutcome updates all context_deliveries rows for the given
// session with the resolved task outcome ("success" or "unknown").
// Called synchronously from handleEndSession — outcome must be persisted before
// the session record is cleared so Sprint 11 queries see consistent state.
// sessionID must be the Synapses session UUID (not the MCP protocol session ID).
// Only rows with task_outcome='' are updated, making this safe to call multiple
// times (idempotent: already-correlated rows are never overwritten).
// Returns the number of rows updated and any database error.
func (s *Store) CorrelateSessionOutcome(sessionID, outcome string) (int64, error) {
	if s == nil || s.knowledgeDB == nil || sessionID == "" {
		return 0, nil
	}
	res, err := s.knowledgeDB.Exec(
		`UPDATE context_deliveries SET task_outcome = ? WHERE session_id = ? AND task_outcome = ''`,
		outcome, sessionID,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}


package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AgentSession tracks the lifecycle of a single agent session.
// Created on session_init(); heartbeat updated on every tool call;
// closed cleanly on end_session() or marked stale after 30 min of inactivity.
type AgentSession struct {
	ID          string
	AgentID     string
	ProjectID   string
	Intent      string
	StartedAt   time.Time
	LastSeenAt  time.Time
	EndedAt     *time.Time // nil = still active
	EndReason   string     // "clean" | "timeout" | "reconciled" | ""
	Outcome     string     // "success" | "failure" | "partial" | "unknown"
	Summary     string
	ToolCalls   int
}

// SessionTaskAction is the relationship between a session and a task.
type SessionTaskAction string

const (
	SessionTaskCreated   SessionTaskAction = "created"
	SessionTaskClaimed   SessionTaskAction = "claimed"
	SessionTaskCompleted SessionTaskAction = "completed"
	SessionTaskAbandoned SessionTaskAction = "abandoned"
)

// OrphanedTask is a task started or created by a stale session that was never
// completed. Surfaced in session_init responses for human-confirmed resolution.
type OrphanedTask struct {
	TaskID       string `json:"task_id"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	Action       string `json:"action"`        // what the stale session did: "created" | "claimed"
	LikelyStatus string `json:"likely_status"` // "likely_done" | "unclear" | "likely_abandoned"
	Evidence     string `json:"evidence,omitempty"`
}

// StaleSession is a session that timed out without a clean end_session.
// Surfaced in session_init so the incoming agent can reconcile orphaned tasks.
type StaleSession struct {
	SessionID     string         `json:"session_id"`
	AgentID       string         `json:"agent_id"`
	StartedAt     string         `json:"started_at"`  // RFC3339 for JSON consumers
	LastSeenAt    string         `json:"last_seen_at"` // RFC3339 for JSON consumers
	Intent        string         `json:"intent,omitempty"`
	ToolCalls     int            `json:"tool_calls"`
	OrphanedTasks []OrphanedTask `json:"orphaned_tasks,omitempty"`
}

// ToolCallCount is a single tool with its invocation count in a session.
type ToolCallCount struct {
	ToolName string `json:"tool_name"`
	Count    int    `json:"count"`
}

// ToolCallSummary is an aggregated view of tool calls for a session.
// Used to build the session retrospective in end_session responses.
type ToolCallSummary struct {
	TotalCalls int             `json:"total_calls"`
	DurationMs int64           `json:"duration_ms"` // cumulative sum of all call durations
	TopTools   []ToolCallCount `json:"top_tools,omitempty"`
	ErrorRate  float64         `json:"error_rate"` // 0.0–1.0
}

// defaultReconnectWindowSec is the fallback reconnect window used when no
// value is provided via config. 5 minutes covers all real reconnect scenarios:
//   - Claude Code / Cursor restart:   < 30 s
//   - MCP transport hiccup:           < 2 min
//   - Slow machine cold start:        < 5 min
const defaultReconnectWindowSec int64 = 5 * 60

// GetOrResumeSession is the single entry point for session creation at
// session_init time. It solves two problems:
//
//  1. Duplicate rows on reconnect — if the same MCP connection (identified by
//     mcpSessionID) reconnects within the reconnect window, the existing session
//     is resumed rather than a new row created. mcpSessionID is the transport-level
//     connection UUID assigned by the MCP framework (unique per physical connection),
//     NOT the agent's self-declared agentID. This means two simultaneous Claude Code
//     windows on the same project are always treated as separate sessions even if
//     they both declare agent_id="claude-code".
//
//  2. Dirty state from prior unclosed sessions — when a fresh session IS created,
//     any previously unclosed sessions for the same (agentID, projectID) are
//     immediately closed with end_reason="superseded". This prevents them from
//     lingering as false stale-session alerts for up to 30 min.
//
// Parameters:
//   - agentID:          self-declared agent name (e.g. "claude-code"). May be "anonymous".
//   - projectID:        stable FNV hash of the project root path.
//   - mcpSessionID:     MCP transport connection ID ("stdio" for stdio mode).
//   - intent:           optional declared goal from session_init (may be empty).
//   - reconnectWindow:  from config.Session.ReconnectWindowSecs; 0 → default (300 s).
func (s *Store) GetOrResumeSession(agentID, projectID, mcpSessionID, intent string, reconnectWindow int) (sessionID string, resumed bool, err error) {
	windowSec := int64(reconnectWindow)
	if windowSec <= 0 {
		windowSec = defaultReconnectWindowSec
	}
	now := time.Now().UTC().Unix()
	cutoff := now - windowSec

	// BEGIN IMMEDIATE serializes concurrent session_init calls on the same connection.
	// Without this, two rapid session_init calls can both pass the SELECT (deferred
	// read lock) before either writes — both would INSERT, producing two live sessions
	// for one mcp_session_id.
	// sql.LevelSerializable maps to BEGIN IMMEDIATE in the modernc SQLite driver,
	// acquiring a reserved write lock upfront so the check-supersede-insert is atomic.
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return "", false, fmt.Errorf("begin session tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// ── Try resume: same MCP connection, same agent, same project, still active ──
	// mcpSessionID is the authoritative discriminator: two concurrent agents on the
	// same project but different connections have different mcpSessionIDs, so they
	// never steal each other's sessions.
	var existing string
	queryErr := tx.QueryRow(`
		SELECT id FROM sessions
		WHERE agent_id      = ?
		  AND project_id    = ?
		  AND mcp_session_id = ?
		  AND ended_at      IS NULL
		  AND last_seen_at  > ?
		ORDER BY last_seen_at DESC
		LIMIT 1`,
		agentID, projectID, mcpSessionID, cutoff).Scan(&existing)

	if queryErr == nil && existing != "" {
		// Resume: refresh the heartbeat without resetting tool_calls or intent.
		_, _ = tx.Exec(
			`UPDATE sessions SET last_seen_at = ? WHERE id = ?`,
			now, existing)
		err = tx.Commit()
		if err != nil {
			return "", false, fmt.Errorf("commit resume: %w", err)
		}
		return existing, true, nil
	}

	// ── No resumable session — supersede prior unclosed sessions for THIS connection ──
	// Scope to mcp_session_id so we only close sessions from THIS physical connection's
	// prior runs. Sessions from other concurrent connections (different mcp_session_id)
	// are live and must not be touched — closing them would be a critical correctness bug
	// for multi-window setups (two Claude Code windows on the same project).
	_, _ = tx.Exec(`
		UPDATE sessions
		SET ended_at = ?, end_reason = 'superseded', outcome = 'unknown'
		WHERE agent_id = ? AND project_id = ? AND mcp_session_id = ? AND ended_at IS NULL`,
		now, agentID, projectID, mcpSessionID)

	// ── Create fresh session ──
	sessionID = newID()
	_, err = tx.Exec(
		`INSERT INTO sessions(id, agent_id, project_id, mcp_session_id, intent, started_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sessionID, agentID, projectID, mcpSessionID, intent, now, now)
	if err != nil {
		return "", false, fmt.Errorf("insert session: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return "", false, fmt.Errorf("commit new session: %w", err)
	}
	return sessionID, false, nil
}

// TouchSession updates last_seen_at and increments the tool_calls counter for
// an active session. Fire-and-forget: all errors are silently discarded so this
// can never block the hot path (< 0.5 ms per call).
func (s *Store) TouchSession(sessionID string) {
	if sessionID == "" {
		return
	}
	now := time.Now().UTC().Unix()
	_, _ = s.db.Exec(
		`UPDATE sessions SET last_seen_at = ?, tool_calls = tool_calls + 1
		 WHERE id = ? AND ended_at IS NULL`,
		now, sessionID)
}

// EndSession marks a session as closed with the given reason, outcome, and summary.
// reason: "clean" (end_session called), "timeout" (manual reconciliation).
// outcome: "success" | "failure" | "partial" | "unknown".
func (s *Store) EndSession(sessionID, reason, outcome, summary string) error {
	now := time.Now().UTC().Unix()
	_, err := s.db.Exec(
		`UPDATE sessions SET ended_at = ?, end_reason = ?, outcome = ?, summary = ?
		 WHERE id = ?`,
		now, reason, outcome, summary, sessionID)
	return err
}

// GetStaleSessions returns sessions for projectID that have not been seen within
// staleThreshold and have not been cleanly closed. currentSessionID is excluded
// so the caller never surfaces its own session. Results are capped at 5.
func (s *Store) GetStaleSessions(projectID, currentSessionID string, staleThreshold time.Duration) ([]StaleSession, error) {
	cutoff := time.Now().UTC().Add(-staleThreshold).Unix()
	rows, err := s.db.Query(`
		SELECT id, agent_id, started_at, last_seen_at, intent, tool_calls
		FROM sessions
		WHERE project_id = ?
		  AND ended_at IS NULL
		  AND last_seen_at < ?
		  AND id != ?
		ORDER BY last_seen_at DESC
		LIMIT 5`,
		projectID, cutoff, currentSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []StaleSession
	for rows.Next() {
		var ss StaleSession
		var startedEpoch, lastSeenEpoch int64
		if err := rows.Scan(&ss.SessionID, &ss.AgentID, &startedEpoch, &lastSeenEpoch, &ss.Intent, &ss.ToolCalls); err != nil {
			return nil, err
		}
		// Convert epoch to RFC3339 for JSON consumers.
		ss.StartedAt = time.Unix(startedEpoch, 0).UTC().Format(time.RFC3339)
		ss.LastSeenAt = time.Unix(lastSeenEpoch, 0).UTC().Format(time.RFC3339)
		result = append(result, ss)
	}
	return result, rows.Err()
}

// GetOrphanedTasks returns tasks that were started or created by the given
// stale session but never completed, and are still in an active state.
// These are candidates for human-confirmed reconciliation.
func (s *Store) GetOrphanedTasks(sessionID string) ([]OrphanedTask, error) {
	// GROUP BY st.task_id deduplicates tasks that have both 'created' and 'claimed'
	// rows. The correlated subquery selects the most recent action for that task in
	// this session ordered by 'at' DESC — so 'claimed' (written after 'created')
	// is correctly returned when both exist, reflecting that the agent was actively
	// working on it. This is intentional, not alphabetical accident.
	rows, err := s.db.Query(`
		SELECT st.task_id,
		       (SELECT st2.action FROM session_tasks st2
		        WHERE st2.task_id = st.task_id
		          AND st2.session_id = st.session_id
		          AND st2.action IN ('created', 'claimed')
		        ORDER BY CASE st2.action WHEN 'claimed' THEN 1 ELSE 0 END DESC,
		                 st2.at DESC
		        LIMIT 1) AS latest_action,
		       t.title, t.status
		FROM session_tasks st
		JOIN tasks t ON st.task_id = t.id
		WHERE st.session_id = ?
		  AND st.action IN ('created', 'claimed')
		  AND t.status IN ('pending', 'in_progress')
		  AND NOT EXISTS (
		      SELECT 1 FROM session_tasks st2
		      WHERE st2.task_id = st.task_id
		        AND st2.action = 'completed'
		  )
		GROUP BY st.task_id`,
		sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []OrphanedTask
	for rows.Next() {
		var ot OrphanedTask
		if err := rows.Scan(&ot.TaskID, &ot.Action, &ot.Title, &ot.Status); err != nil {
			return nil, err
		}
		ot.LikelyStatus = "unclear" // callers enrich with file-change evidence
		result = append(result, ot)
	}
	return result, rows.Err()
}

// LinkSessionTask records the relationship between a session and a task at a
// point in time. action: "created" | "claimed" | "completed" | "abandoned".
// Fire-and-forget: all errors silently discarded — must never block task ops.
func (s *Store) LinkSessionTask(sessionID, taskID string, action SessionTaskAction) {
	if sessionID == "" || taskID == "" {
		return
	}
	now := time.Now().UTC().Unix()
	_, _ = s.db.Exec(
		`INSERT INTO session_tasks(session_id, task_id, action, at) VALUES (?, ?, ?, ?)`,
		sessionID, taskID, string(action), now)
}

// GetToolCallSummary returns aggregated tool call stats for a session.
// DurationMs is the cumulative sum of all individual call durations — not a
// wall-clock span — so it correctly reflects actual work time regardless of
// idle gaps between calls.
// Returns an empty summary (not an error) when no calls exist for the session.
func (s *Store) GetToolCallSummary(sessionID string) (ToolCallSummary, error) {
	var summary ToolCallSummary

	row := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(1.0 - AVG(CAST(success AS REAL)), 0.0),
		       COALESCE(SUM(duration_ms), 0)
		FROM tool_calls
		WHERE session_id = ?`, sessionID)
	if err := row.Scan(&summary.TotalCalls, &summary.ErrorRate, &summary.DurationMs); err != nil {
		return summary, fmt.Errorf("tool call summary: %w", err)
	}
	if summary.TotalCalls == 0 {
		return summary, nil
	}

	rows, err := s.db.Query(`
		SELECT tool_name, COUNT(*) as cnt
		FROM tool_calls
		WHERE session_id = ?
		GROUP BY tool_name
		ORDER BY cnt DESC
		LIMIT 10`, sessionID)
	if err != nil {
		return summary, nil // non-fatal; return what we have
	}
	defer rows.Close()
	for rows.Next() {
		var tc ToolCallCount
		if err := rows.Scan(&tc.ToolName, &tc.Count); err == nil {
			summary.TopTools = append(summary.TopTools, tc)
		}
	}
	return summary, nil
}

// SetSessionBranch records the current git branch for a session.
// Fire-and-forget: used by session_init to persist branch state.
func (s *Store) SetSessionBranch(sessionID, branch string) {
	if sessionID == "" || branch == "" {
		return
	}
	_, _ = s.db.Exec(
		`UPDATE sessions SET last_branch = ? WHERE id = ?`,
		branch, sessionID)
}

// GetLastBranch returns the git branch from the most recent ended session
// for the given agent. Returns "" if no prior session exists or if the
// previous session never recorded a branch (pre-R22 sessions).
func (s *Store) GetLastBranch(agentID string) string {
	if agentID == "" {
		return ""
	}
	var branch string
	err := s.db.QueryRow(`
		SELECT last_branch FROM sessions
		WHERE agent_id = ? AND ended_at IS NOT NULL AND last_branch != ''
		ORDER BY ended_at DESC, started_at DESC
		LIMIT 1`, agentID).Scan(&branch)
	if err != nil {
		return ""
	}
	return branch
}

// PruneToolCallsOlderThan deletes tool_calls rows older than age.
// Returns the number of rows deleted. Safe to call concurrently — a built-in
// 1-hour debounce ensures at most one prune runs per hour regardless of how
// many goroutines invoke it (e.g. multiple parallel session_init calls).
// At 50 calls/session × 10 sessions/day, a 7-day window is ~35 KB.
func (s *Store) PruneToolCallsOlderThan(age time.Duration) (int64, error) {
	s.lastPruneMu.Lock()
	if time.Since(s.lastPruneAt) < time.Hour {
		s.lastPruneMu.Unlock()
		return 0, nil // already pruned recently; skip
	}
	s.lastPruneAt = time.Now()
	s.lastPruneMu.Unlock()

	cutoff := time.Now().UTC().Add(-age).Format(time.RFC3339)
	res, err := s.db.Exec(`DELETE FROM tool_calls WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

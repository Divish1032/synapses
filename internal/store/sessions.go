package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// HibernateResumeContext carries prior session information surfaced when a
// cross-connection resume occurs (agent restarts editor and calls session_init).
// Non-nil only when GetOrResumeSession performs a Phase 2 hibernate resume.
type HibernateResumeContext struct {
	// PriorIntent is the intent declared in the hibernated session (may be empty).
	PriorIntent string
	// PriorSummary is from end_session or an auto-log (may be empty).
	PriorSummary string
	// PriorToolCalls is the total tool calls made in the prior session segment.
	PriorToolCalls int
	// GapSeconds is how long the session was dormant before this resume.
	GapSeconds int64
	// StartedAt is the original session.started_at (Unix epoch), so the MCP
	// handler can display total session age across all resume cycles.
	StartedAt int64
	// ParentID is the sessions.id row that was resumed (now the current session).
	ParentID string
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
	StartedAt     string         `json:"started_at"`   // RFC3339 for JSON consumers
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

// defaultHibernateWindowSec is the fallback hibernate window: 4 hours.
// Covers the "went for lunch / meeting / overnight" pattern without being
// so large that it causes confusion on the next working day.
const defaultHibernateWindowSec int64 = 4 * 60 * 60

// GetOrResumeSession is the single entry point for session creation at
// session_init time. It handles three scenarios in priority order:
//
//  1. Same-connection resume (Phase 1): if the same MCP connection
//     (identified by mcpSessionID) reconnects within the reconnect window,
//     the existing session is resumed rather than a new row created. Handles
//     MCP transport hiccups and rapid reconnects without creating duplicate rows.
//
//  2. Cross-connection hibernate resume (Phase 2): if no same-connection match
//     is found AND hibernateWindow > 0, Synapses looks for a recent session
//     from the same (agentID, projectID) that went dormant for more than the
//     reconnect window (i.e. not currently live on another connection) but less
//     than the hibernate window (i.e. still within the resumable period). This
//     covers the "user took a break / restarted editor" pattern. On a match,
//     the session's mcp_session_id is updated to the new connection and the row
//     is reactivated. A non-nil HibernateResumeContext is returned so the MCP
//     handler can surface prior intent, summary, and gap duration to the agent.
//
//  3. Fresh session (Phase 3+4): supersede any unclosed sessions for THIS
//     physical connection and create a new row. Concurrent windows on the
//     same project with different mcp_session_ids are never touched.
//
// Parameters:
//   - agentID:          self-declared agent name (e.g. "claude-code"). May be "anonymous".
//   - projectID:        stable FNV hash of the project root path.
//   - mcpSessionID:     MCP transport connection ID ("stdio" for stdio mode).
//   - intent:           optional declared goal from session_init (may be empty).
//   - reconnectWindow:  from config.Session.ReconnectWindowSecs; 0 or negative → default (300 s).
//   - hibernateWindow:  from config.Session.HibernateWindowSecs.
//     0 → default (14400 s / 4 h).
//     Positive → use that value as the window.
//     Negative (e.g. -1) → disable cross-connection resume entirely.
func (s *Store) GetOrResumeSession(agentID, projectID, mcpSessionID, intent string, reconnectWindow, hibernateWindow int) (sessionID string, resumed bool, hibernateCtx *HibernateResumeContext, err error) {
	windowSec := int64(reconnectWindow)
	if windowSec <= 0 {
		windowSec = defaultReconnectWindowSec
	}
	hibWindowSec := int64(hibernateWindow)
	switch {
	case hibWindowSec < 0:
		// Negative: cross-connection resume explicitly disabled.
		hibWindowSec = -1
	case hibWindowSec == 0:
		// Zero / unset: apply built-in default.
		hibWindowSec = defaultHibernateWindowSec
	}
	// Guard: hibernate window must be strictly greater than the reconnect window.
	// If not, the Phase 2 query range (last_seen_at > hibCutoff AND < reconnectCutoff)
	// would be empty or inverted, silently matching nothing. Treat as disabled.
	if hibWindowSec > 0 && hibWindowSec <= windowSec {
		hibWindowSec = -1
	}

	now := time.Now().UTC().Unix()
	cutoff := now - windowSec

	// BEGIN IMMEDIATE serializes concurrent session_init calls on the same connection.
	// Without this, two rapid session_init calls can both pass the SELECT (deferred
	// read lock) before either writes — both would INSERT, producing two live sessions
	// for one mcp_session_id.
	// sql.LevelSerializable maps to BEGIN IMMEDIATE in the modernc SQLite driver,
	// acquiring a reserved write lock upfront so the check-supersede-insert is atomic.
	tx, err := s.knowledgeDB.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return "", false, nil, fmt.Errorf("begin session tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// ── Phase 1: Same-connection resume ──────────────────────────────────
	// mcpSessionID is the authoritative discriminator: two concurrent agents on
	// the same project but different connections have different mcpSessionIDs,
	// so they never steal each other's sessions.
	var existing string
	queryErr := tx.QueryRow(`
		SELECT id FROM sessions
		WHERE agent_id       = ?
		  AND project_id     = ?
		  AND mcp_session_id = ?
		  AND ended_at       IS NULL
		  AND state         != 'closed'
		  AND last_seen_at   > ?
		ORDER BY last_seen_at DESC
		LIMIT 1`,
		agentID, projectID, mcpSessionID, cutoff).Scan(&existing)

	if queryErr == nil && existing != "" {
		// Resume: refresh heartbeat and ensure state is active.
		if _, err := tx.Exec(
			`UPDATE sessions SET last_seen_at = ?, state = 'active' WHERE id = ?`,
			now, existing); err != nil {
			_ = tx.Rollback()
			return "", false, nil, fmt.Errorf("resume heartbeat update: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return "", false, nil, fmt.Errorf("commit resume: %w", err)
		}
		return existing, true, nil, nil
	}

	// ── Phase 2: Cross-connection hibernate resume ────────────────────────
	// Look for a prior session from the same (agent_id, project_id) that:
	//   - Is within the hibernate window (not yet expired)
	//   - last_seen_at < reconnect cutoff: older than reconnect window, so we
	//     know it is NOT a currently-live concurrent editor window. A session
	//     seen within the last 5 minutes is still being used by another connection
	//     and must not be stolen.
	//   - Not already cleanly closed (ended_at IS NULL).
	// We pick the most recently active candidate (ORDER BY last_seen_at DESC).
	if hibWindowSec > 0 {
		hibCutoff := now - hibWindowSec

		// Lazily promote idle-active sessions to 'hibernated' within this
		// BEGIN IMMEDIATE transaction before Phase 2 queries on state. Without
		// this, a session left open in a dormant editor window (state='active'
		// but last_seen_at older than the reconnect window) could satisfy the
		// Phase 2 time predicates and be stolen — even though the editor is
		// still open. Marking it 'hibernated' here (atomically, under the write
		// lock) ensures Phase 2 can safely filter on state = 'hibernated'.
		if _, err := tx.Exec(`
			UPDATE sessions SET state = 'hibernated'
			WHERE agent_id   = ?
			  AND project_id = ?
			  AND state      = 'active'
			  AND ended_at   IS NULL
			  AND last_seen_at < ?`,
			agentID, projectID, cutoff); err != nil {
			_ = tx.Rollback()
			return "", false, nil, fmt.Errorf("promote idle sessions to hibernated: %w", err)
		}

		var priorID, priorIntent, priorSummary string
		var priorToolCalls int
		var priorStartedAt, priorLastSeen int64

		hibErr := tx.QueryRow(`
			SELECT id, intent, summary, tool_calls, started_at, last_seen_at
			FROM sessions
			WHERE agent_id   = ?
			  AND project_id = ?
			  AND state      = 'hibernated'
			  AND ended_at   IS NULL
			  AND last_seen_at > ?
			  AND last_seen_at < ?
			ORDER BY last_seen_at DESC
			LIMIT 1`,
			agentID, projectID, hibCutoff, cutoff).Scan(
			&priorID, &priorIntent, &priorSummary, &priorToolCalls,
			&priorStartedAt, &priorLastSeen)

		if hibErr == nil && priorID != "" {
			// Found a hibernated session — reactivate it on the new connection.
			gapSecs := now - priorLastSeen
			// Use the new intent if provided; fall back to the prior intent so
			// the agent's declared goal is preserved across breaks.
			activeIntent := intent
			if activeIntent == "" {
				activeIntent = priorIntent
			}
			_, err = tx.Exec(`
				UPDATE sessions
				SET mcp_session_id = ?,
				    last_seen_at   = ?,
				    state          = 'active',
				    intent         = ?
				WHERE id = ?`,
				mcpSessionID, now, activeIntent, priorID)
			if err != nil {
				_ = tx.Rollback()
				return "", false, nil, fmt.Errorf("hibernate resume update: %w", err)
			}
			if err = tx.Commit(); err != nil {
				return "", false, nil, fmt.Errorf("commit hibernate resume: %w", err)
			}
			return priorID, false, &HibernateResumeContext{
				PriorIntent:    priorIntent,
				PriorSummary:   priorSummary,
				PriorToolCalls: priorToolCalls,
				GapSeconds:     gapSecs,
				StartedAt:      priorStartedAt,
				ParentID:       priorID,
			}, nil
		}
	}

	// ── Phase 3: Supersede prior unclosed sessions for THIS connection ────
	// Scope to mcp_session_id so we only close sessions from THIS physical
	// connection's prior runs. Sessions from other concurrent connections
	// (different mcp_session_id) are live and must not be touched.
	if _, err := tx.Exec(`
		UPDATE sessions
		SET ended_at = ?, end_reason = 'superseded', outcome = 'unknown', state = 'closed'
		WHERE agent_id = ? AND project_id = ? AND mcp_session_id = ? AND ended_at IS NULL`,
		now, agentID, projectID, mcpSessionID); err != nil {
		_ = tx.Rollback()
		return "", false, nil, fmt.Errorf("supersede prior sessions: %w", err)
	}

	// ── Phase 4: Create fresh session ──────────────────────────────────────
	// Find the most recent closed session for this (agent_id, project_id) to
	// set as parent, giving a traceable ancestry chain across restarts.
	var parentID string
	// Best-effort parent lookup — sql.ErrNoRows is expected for brand-new agents.
	if err := tx.QueryRow(`
		SELECT id FROM sessions
		WHERE agent_id = ? AND project_id = ? AND ended_at IS NOT NULL
		ORDER BY ended_at DESC
		LIMIT 1`, agentID, projectID).Scan(&parentID); err != nil && err != sql.ErrNoRows {
		logutil.Warn("synapses: store: parent session lookup: %v\n", err)
	}

	sessionID = newID()
	_, err = tx.Exec(
		`INSERT INTO sessions(id, agent_id, project_id, mcp_session_id, intent, started_at, last_seen_at, state, parent_session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?)`,
		sessionID, agentID, projectID, mcpSessionID, intent, now, now, parentID)
	if err != nil {
		return "", false, nil, fmt.Errorf("insert session: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return "", false, nil, fmt.Errorf("commit new session: %w", err)
	}
	return sessionID, false, nil, nil
}

// TouchSession updates last_seen_at, increments tool_calls, and ensures the
// session state is 'active' (a hibernated session becomes active again on first
// tool call). Fire-and-forget: all errors silently discarded (< 0.5 ms per call).
func (s *Store) TouchSession(sessionID string) {
	if sessionID == "" {
		return
	}
	now := time.Now().UTC().Unix()
	_, _ = s.knowledgeDB.Exec(
		`UPDATE sessions SET last_seen_at = ?, tool_calls = tool_calls + 1, state = 'active'
		 WHERE id = ? AND ended_at IS NULL`,
		now, sessionID)
}

// EndSession marks a session as closed with the given reason, outcome, and summary.
// reason: "clean" (end_session called), "timeout" (manual reconciliation).
// outcome: "success" | "failure" | "partial" | "unknown".
func (s *Store) EndSession(sessionID, reason, outcome, summary string) error {
	now := time.Now().UTC().Unix()
	_, err := s.knowledgeDB.Exec(
		`UPDATE sessions SET ended_at = ?, end_reason = ?, outcome = ?, summary = ?, state = 'closed'
		 WHERE id = ?`,
		now, reason, outcome, summary, sessionID)
	return err
}

// GetStaleSessions returns sessions for projectID that have not been seen within
// staleThreshold and have not been cleanly closed. currentSessionID is excluded
// so the caller never surfaces its own session. Results are capped at 5.
func (s *Store) GetStaleSessions(projectID, currentSessionID string, staleThreshold time.Duration) ([]StaleSession, error) {
	cutoff := time.Now().UTC().Add(-staleThreshold).Unix()

	ctx := context.Background()
	tx, err := s.knowledgeDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("begin stale sessions tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Lazily mark dormant sessions as 'hibernated' so the Tauri app and any
	// observer can see accurate state without a separate reconciliation pass.
	// Scoped to this project; error silently ignored (non-critical side effect).
	_, _ = tx.Exec(`
		UPDATE sessions
		SET state = 'hibernated'
		WHERE project_id = ?
		  AND ended_at IS NULL
		  AND state = 'active'
		  AND last_seen_at < ?`, projectID, cutoff)

	rows, err := tx.Query(`
		SELECT id, agent_id, started_at, last_seen_at, intent, tool_calls
		FROM sessions
		WHERE project_id = ?
		  AND ended_at IS NULL
		  AND state    != 'closed'
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit stale sessions tx: %w", err)
	}
	return result, nil
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
	rows, err := s.knowledgeDB.Query(`
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
	_, _ = s.knowledgeDB.Exec(
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

	row := s.knowledgeDB.QueryRow(`
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

	rows, err := s.knowledgeDB.Query(`
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
	_, _ = s.knowledgeDB.Exec(
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
	err := s.knowledgeDB.QueryRow(`
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
	res, err := s.knowledgeDB.Exec(`DELETE FROM tool_calls WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneOldSessions deletes sessions (and their linked session_tasks rows via
// CASCADE) that have been closed or hibernated for longer than age. This
// prevents unbounded growth in long-running installations.
//
// Safe to call on every startup or from a periodic goroutine — a built-in
// 24-hour debounce ensures the DELETE runs at most once per day regardless of
// how many callers invoke it.
//
// At ~5 sessions/day a 90-day window keeps fewer than 450 rows; the DELETE
// itself is effectively instantaneous at that scale.
func (s *Store) PruneOldSessions(age time.Duration) (int64, error) {
	s.lastPruneMu.Lock()
	if time.Since(s.lastSessionPruneAt) < 24*time.Hour {
		s.lastPruneMu.Unlock()
		return 0, nil
	}
	s.lastSessionPruneAt = time.Now()
	s.lastPruneMu.Unlock()

	cutoff := time.Now().UTC().Add(-age).Unix()
	// Use IMMEDIATE transaction to atomically prune sessions and their tasks.
	// Without this, a concurrent insert into session_tasks between the two
	// DELETEs could orphan freshly-inserted task rows.
	tx, err := s.knowledgeDB.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return 0, fmt.Errorf("begin prune tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Only prune rows that are fully closed or hibernated past the age window.
	// Active sessions are never touched regardless of age.
	res, err := tx.Exec(`
		DELETE FROM sessions
		WHERE state IN ('closed', 'hibernated')
		  AND last_seen_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	// Clean up orphaned session_tasks — no FOREIGN KEY CASCADE exists on this table.
	if _, err := tx.Exec(`DELETE FROM session_tasks WHERE session_id NOT IN (SELECT id FROM sessions)`); err != nil {
		return 0, fmt.Errorf("prune session_tasks: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit prune tx: %w", err)
	}
	return res.RowsAffected()
}

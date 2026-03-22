package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// Plan is a named collection of related tasks created during an LLM session.
// It persists in SQLite so future sessions can resume the agreed work.
type Plan struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	CreatedBy   string `json:"created_by,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	// CompletedAt is set (unix seconds) when all tasks in the plan reach done/cancelled.
	// Zero means still active.
	CompletedAt int64 `json:"completed_at,omitempty"`
}

// Task is a single actionable work item belonging to a plan.
// Status flows: pending → in_progress → done (or cancelled).
type Task struct {
	ID            string   `json:"id"`
	PlanID        string   `json:"plan_id"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Status        string   `json:"status"`       // pending | in_progress | done | cancelled
	Priority      string   `json:"priority"`     // p0 | p1 | p2 | p3
	LinkedNodes   []string `json:"linked_nodes"` // node IDs related to this task
	DependsOn     []string `json:"depends_on"`   // task IDs that must complete first
	Notes         string   `json:"notes"`        // append-only notes from each session
	AssignedTo    string   `json:"assigned_to,omitempty"`
	LastUpdatedBy string   `json:"last_updated_by,omitempty"`
	// R21: Commit tracking — populated when git is available in the project root.
	// StartCommit is the HEAD SHA captured when the task was set to in_progress.
	// CommitsSinceStart is the git log since StartCommit, captured at done time.
	// Both are empty when git is unavailable. Commits are repo-wide (not per-agent).
	StartCommit       string   `json:"start_commit,omitempty"`
	CommitsSinceStart []string `json:"commits_since_start,omitempty"`
	// Computed fields — not stored in DB; set by GetPendingTasks.
	Blocked   bool     `json:"blocked,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

// TaskInput is used when creating a batch of tasks inside CreatePlan.
type TaskInput struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Priority    string   `json:"priority"`
	LinkedNodes []string `json:"linked_nodes"`
	DependsOn   []string `json:"depends_on"` // task IDs that must be done before this task
}

// UnmarshalJSON allows `priority` to be either a string ("p0", "p1") or a number (0, 1, 2).
// LLMs naturally emit integer priorities; this coerces them to the internal string format.
func (t *TaskInput) UnmarshalJSON(data []byte) error {
	// Use an alias to avoid recursion.
	type Alias TaskInput
	aux := &struct {
		Priority json.RawMessage `json:"priority"`
		*Alias
	}{Alias: (*Alias)(t)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if len(aux.Priority) == 0 {
		return nil
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(aux.Priority, &s); err == nil {
		t.Priority = s
		return nil
	}
	// Try number — convert to "p<n>" string.
	var n json.Number
	if err := json.Unmarshal(aux.Priority, &n); err == nil {
		t.Priority = "p" + n.String()
		return nil
	}
	return fmt.Errorf("priority must be a string or number, got %s", string(aux.Priority))
}

// PlanSummary is a plan with task completion counts, used by GetPlans.
type PlanSummary struct {
	Plan
	TotalTasks   int  `json:"total_tasks"`
	PendingTasks int  `json:"pending_tasks"`
	DoneTasks    int  `json:"done_tasks"`
	IsCompleted  bool `json:"is_completed"` // true when all tasks are done/cancelled
}

// CreatePlan persists a plan and its initial tasks atomically.
// agentID is optional — if non-empty it records which agent created the plan.
// Returns the plan ID.
// CreatePlan persists a plan and its initial tasks atomically. agentID is optional —
// if non-empty it records which agent created the plan. Returns the plan ID and
// the IDs of all created tasks (for session-task linkage).
func (s *Store) CreatePlan(title, description, agentID string, tasks []TaskInput) (planID string, taskIDs []string, err error) {
	planID = newID()
	now := time.Now().UTC().Format(time.RFC3339)

	tx, txErr := s.knowledgeDB.Begin()
	if txErr != nil {
		return "", nil, fmt.Errorf("begin tx: %w", txErr)
	}
	defer tx.Rollback() //nolint:errcheck

	_, txErr = tx.Exec(
		`INSERT INTO plans (id, title, description, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		planID, title, description, agentID, now, now,
	)
	if txErr != nil {
		return "", nil, fmt.Errorf("insert plan: %w", txErr)
	}

	for _, t := range tasks {
		taskID := newID()
		taskIDs = append(taskIDs, taskID)
		priority := t.Priority
		if priority == "" {
			priority = "p2"
		}
		linked, _ := json.Marshal(t.LinkedNodes)
		deps, _ := json.Marshal(t.DependsOn)
		_, txErr = tx.Exec(
			`INSERT INTO tasks (id, plan_id, title, description, status, priority, linked_nodes, depends_on, notes, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, '', ?, ?)`,
			taskID, planID, t.Title, t.Description, priority, string(linked), string(deps), now, now,
		)
		if txErr != nil {
			return "", nil, fmt.Errorf("insert task %q: %w", t.Title, txErr)
		}
	}

	if txErr = tx.Commit(); txErr != nil {
		return "", nil, fmt.Errorf("commit: %w", txErr)
	}
	return planID, taskIDs, nil
}

// GetPendingTasks returns all tasks with status 'pending' or 'in_progress',
// ordered by priority (p0 first) then creation time.
// If planID is non-empty, results are filtered to that plan only.
// If agentID is non-empty, results are filtered to tasks assigned to that agent.
// Each task's Blocked/BlockedBy fields are computed from depends_on status.
func (s *Store) GetPendingTasks(planID, agentID string) ([]Task, error) {
	query := `
		SELECT id, plan_id, title, description, status, priority, linked_nodes, depends_on, notes,
		       assigned_to, last_updated_by, created_at, updated_at, start_commit, commits
		FROM tasks
		WHERE status IN ('pending', 'in_progress')
	`
	args := []interface{}{}
	if planID != "" {
		query += ` AND plan_id = ?`
		args = append(args, planID)
	}
	if agentID != "" {
		// Return tasks owned by this agent OR still unassigned — the agent can
		// claim the unassigned ones below.
		query += ` AND (assigned_to = ? OR assigned_to = '' OR assigned_to IS NULL)`
		args = append(args, agentID)
	}
	query += ` ORDER BY
		CASE priority WHEN 'p0' THEN 0 WHEN 'p1' THEN 1 WHEN 'p2' THEN 2 ELSE 3 END,
		created_at ASC
		LIMIT 500`

	rows, err := s.knowledgeDB.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query pending tasks: %w", err)
	}
	defer rows.Close()

	tasks, err := scanTasks(rows)
	if err != nil {
		return nil, err
	}

	// Auto-claim unassigned tasks for the calling agent so ownership is tracked
	// from the moment the agent discovers the task.
	if agentID != "" {
		for _, t := range tasks {
			if t.AssignedTo == "" {
				_, _ = s.knowledgeDB.Exec(
					`UPDATE tasks SET assigned_to = ?, updated_at = ? WHERE id = ? AND (assigned_to = '' OR assigned_to IS NULL)`,
					agentID, time.Now().UTC(), t.ID,
				)
			}
		}
		// Reflect claimed assignments in the returned slice.
		for i := range tasks {
			if tasks[i].AssignedTo == "" {
				tasks[i].AssignedTo = agentID
			}
		}
	}

	// Compute blocked status: collect all unique dependency IDs, fetch their
	// statuses in one query, then annotate each task.
	allDepIDs := map[string]struct{}{}
	for _, t := range tasks {
		for _, dep := range t.DependsOn {
			allDepIDs[dep] = struct{}{}
		}
	}
	if len(allDepIDs) > 0 {
		ids := make([]string, 0, len(allDepIDs))
		for id := range allDepIDs {
			ids = append(ids, id)
		}
		statusMap, err := s.taskStatusByID(ids)
		if err != nil {
			return nil, fmt.Errorf("resolve task dependencies: %w", err)
		}
		for i := range tasks {
			var blockedBy []string
			for _, depID := range tasks[i].DependsOn {
				st, known := statusMap[depID]
				if !known || st != "done" {
					blockedBy = append(blockedBy, depID)
				}
			}
			if len(blockedBy) > 0 {
				tasks[i].Blocked = true
				tasks[i].BlockedBy = blockedBy
			}
		}
	}

	return tasks, nil
}

// FindTasksByNodeID searches tasks where linked_nodes contains the given node ID.
// Returns up to limit results ordered by most recently updated first.
// Wraps the search term in JSON double quotes so "Auth" does NOT false-match
// "AuthService" — the closing quote acts as an exact-entry boundary.
func (s *Store) FindTasksByNodeID(nodeID string, limit int) ([]Task, error) {
	if nodeID == "" || limit <= 0 {
		return nil, nil
	}
	pattern := `%"` + escapeLike(nodeID) + `"%`
	rows, err := s.knowledgeDB.Query(`
		SELECT id, plan_id, title, description, status, priority, linked_nodes, depends_on, notes,
		       assigned_to, last_updated_by, created_at, updated_at, start_commit, commits
		FROM tasks
		WHERE linked_nodes LIKE ? ESCAPE '\'
		ORDER BY updated_at DESC
		LIMIT ?`, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("find tasks by node: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		var linkedJSON, depsJSON, commitsJSON string
		if err := rows.Scan(
			&t.ID, &t.PlanID, &t.Title, &t.Description,
			&t.Status, &t.Priority, &linkedJSON, &depsJSON, &t.Notes,
			&t.AssignedTo, &t.LastUpdatedBy,
			&t.CreatedAt, &t.UpdatedAt,
			&t.StartCommit, &commitsJSON,
		); err != nil {
			return nil, fmt.Errorf("scan task by node: %w", err)
		}
		if err := json.Unmarshal([]byte(linkedJSON), &t.LinkedNodes); err != nil {
			logutil.Debug("synapses: tasks: unmarshal linked_nodes for task %q: %v\n", t.ID, err)
		}
		if t.LinkedNodes == nil {
			t.LinkedNodes = []string{}
		}
		if err := json.Unmarshal([]byte(depsJSON), &t.DependsOn); err != nil {
			logutil.Debug("synapses: tasks: unmarshal depends_on for task %q: %v\n", t.ID, err)
		}
		if t.DependsOn == nil {
			t.DependsOn = []string{}
		}
		if err := json.Unmarshal([]byte(commitsJSON), &t.CommitsSinceStart); err != nil {
			logutil.Debug("synapses: tasks: unmarshal commits for task %q: %v\n", t.ID, err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// UpdateLinkedNodes replaces the linked_nodes for a task with nodeIDs.
// Call with the full desired set (existing + newly detected); deduplication is
// the caller's responsibility. Used by handleLinkTaskNodes and autoLinkNodes.
func (s *Store) UpdateLinkedNodes(taskID string, nodeIDs []string) error {
	if nodeIDs == nil {
		nodeIDs = []string{}
	}
	linked, _ := json.Marshal(nodeIDs)
	_, err := s.knowledgeDB.Exec(
		`UPDATE tasks SET linked_nodes = ?, updated_at = ? WHERE id = ?`,
		string(linked), time.Now().UTC().Format(time.RFC3339), taskID,
	)
	return err
}

// UpdateTask changes the status and optionally appends notes to a task.
// agentID is optional — if non-empty it is recorded as the last_updated_by agent.
// Appended notes are prefixed with a timestamp so they form an audit trail.
// Returns:
//   - unblocked: task IDs that became unblocked (only meaningful when status=="done")
//   - planCompleted: true when this update caused the parent plan to auto-complete
//     (all tasks in the plan are now done/cancelled)
func (s *Store) UpdateTask(id, status, appendNotes, agentID string) (unblocked []string, planCompleted bool, err error) {
	now := time.Now().UTC().Format(time.RFC3339)

	// When transitioning to in_progress, also stamp assigned_to so that
	// get_session_state can look up failure episodes for the right agent (F12).
	assignedTo := ""
	if status == "in_progress" && agentID != "" {
		assignedTo = agentID
	}

	if appendNotes != "" {
		newNote := fmt.Sprintf("[%s] %s", now, appendNotes)
		// Atomic SQL append — avoids the read-modify-write TOCTOU race that
		// previously lost notes under concurrent access.
		notesExpr := `CASE WHEN notes IS NULL OR notes = '' THEN ? ELSE notes || char(10) || ? END`
		if assignedTo != "" {
			if _, execErr := s.knowledgeDB.Exec(
				`UPDATE tasks SET status = ?, notes = `+notesExpr+`, assigned_to = ?, last_updated_by = ?, updated_at = ? WHERE id = ?`,
				status, newNote, newNote, assignedTo, agentID, now, id,
			); execErr != nil {
				return nil, false, execErr
			}
		} else {
			if _, execErr := s.knowledgeDB.Exec(
				`UPDATE tasks SET status = ?, notes = `+notesExpr+`, last_updated_by = ?, updated_at = ? WHERE id = ?`,
				status, newNote, newNote, agentID, now, id,
			); execErr != nil {
				return nil, false, execErr
			}
		}
	} else {
		if assignedTo != "" {
			if _, execErr := s.knowledgeDB.Exec(
				`UPDATE tasks SET status = ?, assigned_to = ?, last_updated_by = ?, updated_at = ? WHERE id = ?`,
				status, assignedTo, agentID, now, id,
			); execErr != nil {
				return nil, false, execErr
			}
		} else {
			if _, execErr := s.knowledgeDB.Exec(
				`UPDATE tasks SET status = ?, last_updated_by = ?, updated_at = ? WHERE id = ?`,
				status, agentID, now, id,
			); execErr != nil {
				return nil, false, execErr
			}
		}
	}

	// For terminal statuses, run both dependency unblocking and plan completion checks.
	if status == "done" || status == "cancelled" {
		// Fetch the plan_id of this task so we can check plan completion.
		var planID string
		_ = s.knowledgeDB.QueryRow(`SELECT plan_id FROM tasks WHERE id = ?`, id).Scan(&planID)

		if status == "done" {
			unblocked, err = s.findNewlyUnblocked(id)
			if err != nil {
				return nil, false, err
			}
		}
		planCompleted, err = checkAndCompletePlan(s.knowledgeDB, planID)
		if err != nil {
			return unblocked, false, err
		}
	}
	return unblocked, planCompleted, nil
}

// checkAndCompletePlan marks a plan as completed if every task in it has
// reached a terminal status (done or cancelled). It is idempotent: a plan
// that is already completed (completed_at != 0) is never double-stamped.
// Returns true when the plan was just transitioned to completed.
func checkAndCompletePlan(db *rwDB, planID string) (bool, error) {
	if planID == "" {
		return false, nil
	}

	tx, err := db.Begin()
	if err != nil {
		return false, fmt.Errorf("check plan completion: begin tx: %w", err)
	}
	defer tx.Rollback() // no-op after successful Commit

	// BEGIN IMMEDIATE to prevent concurrent modifications between read and write.
	if _, err := tx.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		return false, fmt.Errorf("check plan completion: set busy timeout: %w", err)
	}

	// A plan completes when it has at least one task and all tasks are terminal.
	var totalTasks, openTasks int
	row := tx.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status NOT IN ('done','cancelled') THEN 1 ELSE 0 END), 0)
		FROM tasks WHERE plan_id = ?`, planID)
	if err := row.Scan(&totalTasks, &openTasks); err != nil {
		return false, fmt.Errorf("check plan completion: %w", err)
	}
	if totalTasks == 0 || openTasks > 0 {
		return false, nil
	}
	// Stamp completed_at atomically — only rows that are still active (0) are updated.
	res, err := tx.Exec(
		`UPDATE plans SET completed_at = ? WHERE id = ? AND completed_at = 0`,
		time.Now().Unix(), planID,
	)
	if err != nil {
		return false, fmt.Errorf("mark plan complete: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("mark plan complete: commit: %w", err)
	}
	return n > 0, nil
}

// findNewlyUnblocked returns IDs of pending tasks that had `completedID` as
// their only blocking dependency (i.e. all other deps are now done too).
func (s *Store) findNewlyUnblocked(completedID string) ([]string, error) {
	// Find all pending/in_progress tasks that depend on completedID.
	rows, err := s.knowledgeDB.Query(
		`SELECT id, depends_on FROM tasks WHERE status IN ('pending','in_progress') AND depends_on LIKE ? ESCAPE '\'`,
		"%"+escapeLike(completedID)+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type candidate struct {
		id   string
		deps []string
	}
	var candidates []candidate
	for rows.Next() {
		var tid, depsJSON string
		if err := rows.Scan(&tid, &depsJSON); err != nil {
			return nil, err
		}
		var deps []string
		if err := json.Unmarshal([]byte(depsJSON), &deps); err != nil {
			logutil.Debug("synapses: tasks: unmarshal depends_on for task %q: %v\n", tid, err)
		}
		// Filter to only tasks that actually list completedID.
		for _, d := range deps {
			if d == completedID {
				candidates = append(candidates, candidate{tid, deps})
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// For each candidate, check if all its other deps are done.
	var unblocked []string
	for _, c := range candidates {
		otherDeps := make([]string, 0, len(c.deps)-1)
		for _, d := range c.deps {
			if d != completedID {
				otherDeps = append(otherDeps, d)
			}
		}
		if len(otherDeps) == 0 {
			unblocked = append(unblocked, c.id)
			continue
		}
		statusMap, err := s.taskStatusByID(otherDeps)
		if err != nil {
			return nil, err
		}
		allDone := true
		for _, st := range statusMap {
			if st != "done" {
				allDone = false
				break
			}
		}
		if allDone {
			unblocked = append(unblocked, c.id)
		}
	}
	return unblocked, nil
}

// SetTaskStartCommit records the git HEAD SHA captured when a task was set to in_progress.
// It is a no-op (not an error) when sha is empty, ensuring graceful degradation when
// git is unavailable.
func (s *Store) SetTaskStartCommit(taskID, sha string) error {
	if sha == "" {
		return nil
	}
	_, err := s.knowledgeDB.Exec(
		`UPDATE tasks SET start_commit = ?, updated_at = ? WHERE id = ?`,
		sha, time.Now().UTC().Format(time.RFC3339), taskID,
	)
	return err
}

// SetTaskCommits stores the git log lines captured at task completion.
// commits may be nil (no commits made, or git unavailable) — stored as '[]'.
// This is a write-once operation per task: called once at update_task(done).
func (s *Store) SetTaskCommits(taskID string, commits []string) error {
	if commits == nil {
		commits = []string{}
	}
	raw, _ := json.Marshal(commits)
	_, err := s.knowledgeDB.Exec(
		`UPDATE tasks SET commits = ?, updated_at = ? WHERE id = ?`,
		string(raw), time.Now().UTC().Format(time.RFC3339), taskID,
	)
	return err
}

// GetTask retrieves a single task by ID. Returns an error wrapping sql.ErrNoRows if not found.
func (s *Store) GetTask(id string) (*Task, error) {
	row := s.knowledgeDB.QueryRow(`
		SELECT id, plan_id, title, description, status, priority, linked_nodes, depends_on, notes,
		       assigned_to, last_updated_by, created_at, updated_at, start_commit, commits
		FROM tasks WHERE id = ?`, id)
	var t Task
	var linkedJSON, depsJSON, commitsJSON string
	if err := row.Scan(
		&t.ID, &t.PlanID, &t.Title, &t.Description,
		&t.Status, &t.Priority, &linkedJSON, &depsJSON, &t.Notes,
		&t.AssignedTo, &t.LastUpdatedBy,
		&t.CreatedAt, &t.UpdatedAt,
		&t.StartCommit, &commitsJSON,
	); err != nil {
		return nil, fmt.Errorf("get task %q: %w", id, err)
	}
	if err := json.Unmarshal([]byte(linkedJSON), &t.LinkedNodes); err != nil {
		logutil.Debug("synapses: tasks: unmarshal linked_nodes for task %q: %v\n", t.ID, err)
	}
	if t.LinkedNodes == nil {
		t.LinkedNodes = []string{}
	}
	if err := json.Unmarshal([]byte(depsJSON), &t.DependsOn); err != nil {
		logutil.Debug("synapses: tasks: unmarshal depends_on for task %q: %v\n", t.ID, err)
	}
	if t.DependsOn == nil {
		t.DependsOn = []string{}
	}
	if err := json.Unmarshal([]byte(commitsJSON), &t.CommitsSinceStart); err != nil {
		logutil.Debug("synapses: tasks: unmarshal commits_since_start for task %q: %v\n", t.ID, err)
	}
	return &t, nil
}

// taskStatusByID returns a map of id→status for the given task IDs.
// Used internally to compute blocked status without a JOIN.
func (s *Store) taskStatusByID(ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	// Build the query with the right number of placeholders.
	placeholders := make([]byte, 0, len(ids)*2)
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args[i] = id
	}
	rows, err := s.knowledgeDB.Query(
		`SELECT id, status FROM tasks WHERE id IN (`+string(placeholders)+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string, len(ids))
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		m[id] = status
	}
	return m, rows.Err()
}

// GetPlans returns all plans with task completion summaries, ordered by creation time desc.
func (s *Store) GetPlans() ([]PlanSummary, error) {
	rows, err := s.knowledgeDB.Query(`
		SELECT p.id, p.title, p.description, p.created_by, p.created_at, p.updated_at,
		       p.completed_at,
		       COUNT(t.id)                                           AS total,
		       SUM(CASE WHEN t.status IN ('pending','in_progress') THEN 1 ELSE 0 END) AS pending,
		       SUM(CASE WHEN t.status = 'done' THEN 1 ELSE 0 END)  AS done
		FROM plans p
		LEFT JOIN tasks t ON t.plan_id = p.id
		GROUP BY p.id
		ORDER BY p.created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query plans: %w", err)
	}
	defer rows.Close()

	var summaries []PlanSummary
	for rows.Next() {
		var ps PlanSummary
		if err := rows.Scan(
			&ps.ID, &ps.Title, &ps.Description, &ps.CreatedBy, &ps.CreatedAt, &ps.UpdatedAt,
			&ps.CompletedAt,
			&ps.TotalTasks, &ps.PendingTasks, &ps.DoneTasks,
		); err != nil {
			return nil, err
		}
		ps.IsCompleted = ps.CompletedAt != 0
		summaries = append(summaries, ps)
	}
	return summaries, rows.Err()
}

// scanTasks reads task rows into a slice.
func scanTasks(rows *sql.Rows) ([]Task, error) {
	var tasks []Task
	for rows.Next() {
		var t Task
		var linkedJSON, depsJSON, commitsJSON string
		if err := rows.Scan(
			&t.ID, &t.PlanID, &t.Title, &t.Description,
			&t.Status, &t.Priority, &linkedJSON, &depsJSON, &t.Notes,
			&t.AssignedTo, &t.LastUpdatedBy,
			&t.CreatedAt, &t.UpdatedAt,
			&t.StartCommit, &commitsJSON,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(linkedJSON), &t.LinkedNodes); err != nil {
			logutil.Debug("synapses: tasks: unmarshal linked_nodes for task %q: %v\n", t.ID, err)
		}
		if t.LinkedNodes == nil {
			t.LinkedNodes = []string{}
		}
		if err := json.Unmarshal([]byte(depsJSON), &t.DependsOn); err != nil {
			logutil.Debug("synapses: tasks: unmarshal depends_on for task %q: %v\n", t.ID, err)
		}
		if t.DependsOn == nil {
			t.DependsOn = []string{}
		}
		if err := json.Unmarshal([]byte(commitsJSON), &t.CommitsSinceStart); err != nil {
			logutil.Debug("synapses: tasks: unmarshal commits_since_start for task %q: %v\n", t.ID, err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// newID generates a cryptographically random ID for plans and tasks.
// Uses 16 bytes of crypto/rand, hex-encoded (32 chars). Existing stored IDs
// (which used time+counter format) remain valid — the format is opaque.
func newID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Fallback: should never happen on modern OS.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// --- Session State (exact-moment task resumption) ---

// SessionState captures the precise working state of a task so that a future
// LLM session can resume from exactly where the previous session stopped.
// Unlike Task.Notes (append-only audit trail), this is a single mutable snapshot.
type SessionState struct {
	ID              string   `json:"id"`
	TaskID          string   `json:"task_id"`
	AgentID         string   `json:"agent_id,omitempty"`
	Approach        string   `json:"approach,omitempty"`         // current strategy being taken
	FilesModified   []string `json:"files_modified,omitempty"`   // files being edited
	CompletedSteps  []string `json:"completed_steps,omitempty"`  // what's already done
	RemainingSteps  []string `json:"remaining_steps,omitempty"`  // what still needs doing
	Blockers        []string `json:"blockers,omitempty"`         // any known blockers
	Decisions       []string `json:"decisions,omitempty"`        // key decisions made
	ContextSnapshot string   `json:"context_snapshot,omitempty"` // free-form context dump
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
}

// UpsertSessionState saves or replaces the session state for a task.
// Each task has at most one session_state row (keyed by task_id).
// agentID is optional metadata for auditing.
func (s *Store) UpsertSessionState(state SessionState) error {
	filesJSON, _ := json.Marshal(state.FilesModified)
	completedJSON, _ := json.Marshal(state.CompletedSteps)
	remainingJSON, _ := json.Marshal(state.RemainingSteps)
	blockersJSON, _ := json.Marshal(state.Blockers)
	decisionsJSON, _ := json.Marshal(state.Decisions)

	now := time.Now().UTC().Format(time.RFC3339)
	id := newID()
	if state.ID != "" {
		id = state.ID
	}

	_, err := s.knowledgeDB.Exec(`
		INSERT INTO session_state
			(id, task_id, agent_id, approach, files_modified, completed_steps,
			 remaining_steps, blockers, decisions, context_snapshot, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			agent_id         = excluded.agent_id,
			approach         = excluded.approach,
			files_modified   = excluded.files_modified,
			completed_steps  = excluded.completed_steps,
			remaining_steps  = excluded.remaining_steps,
			blockers         = excluded.blockers,
			decisions        = excluded.decisions,
			context_snapshot = excluded.context_snapshot,
			updated_at       = excluded.updated_at`,
		id, state.TaskID, state.AgentID, state.Approach,
		string(filesJSON), string(completedJSON), string(remainingJSON),
		string(blockersJSON), string(decisionsJSON), state.ContextSnapshot,
		now, now,
	)
	return err
}

// GetSessionState returns the session state for a task, or nil if none exists.
func (s *Store) GetSessionState(taskID string) (*SessionState, error) {
	row := s.knowledgeDB.QueryRow(`
		SELECT id, task_id, agent_id, approach,
		       files_modified, completed_steps, remaining_steps,
		       blockers, decisions, context_snapshot, created_at, updated_at
		FROM session_state WHERE task_id = ?`, taskID)

	var st SessionState
	var filesJSON, completedJSON, remainingJSON, blockersJSON, decisionsJSON string
	err := row.Scan(
		&st.ID, &st.TaskID, &st.AgentID, &st.Approach,
		&filesJSON, &completedJSON, &remainingJSON,
		&blockersJSON, &decisionsJSON, &st.ContextSnapshot,
		&st.CreatedAt, &st.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(filesJSON), &st.FilesModified); err != nil {
		logutil.Debug("synapses: tasks: unmarshal files_modified for session state %q: %v\n", st.ID, err)
	}
	if err := json.Unmarshal([]byte(completedJSON), &st.CompletedSteps); err != nil {
		logutil.Debug("synapses: tasks: unmarshal completed_steps for session state %q: %v\n", st.ID, err)
	}
	if err := json.Unmarshal([]byte(remainingJSON), &st.RemainingSteps); err != nil {
		logutil.Debug("synapses: tasks: unmarshal remaining_steps for session state %q: %v\n", st.ID, err)
	}
	if err := json.Unmarshal([]byte(blockersJSON), &st.Blockers); err != nil {
		logutil.Debug("synapses: tasks: unmarshal blockers for session state %q: %v\n", st.ID, err)
	}
	if err := json.Unmarshal([]byte(decisionsJSON), &st.Decisions); err != nil {
		logutil.Debug("synapses: tasks: unmarshal decisions for session state %q: %v\n", st.ID, err)
	}
	return &st, nil
}

// GetSessionStateForTasks returns session states for multiple task IDs,
// keyed by task_id. Used by GetPendingTasks to inline state into task results.
func (s *Store) GetSessionStateForTasks(taskIDs []string) (map[string]*SessionState, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	// Build ? placeholders
	placeholders := make([]string, len(taskIDs))
	args := make([]interface{}, len(taskIDs))
	for i, id := range taskIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.knowledgeDB.Query(
		`SELECT id, task_id, agent_id, approach,
		        files_modified, completed_steps, remaining_steps,
		        blockers, decisions, context_snapshot, created_at, updated_at
		 FROM session_state WHERE task_id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]*SessionState)
	for rows.Next() {
		var st SessionState
		var filesJSON, completedJSON, remainingJSON, blockersJSON, decisionsJSON string
		if err := rows.Scan(
			&st.ID, &st.TaskID, &st.AgentID, &st.Approach,
			&filesJSON, &completedJSON, &remainingJSON,
			&blockersJSON, &decisionsJSON, &st.ContextSnapshot,
			&st.CreatedAt, &st.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(filesJSON), &st.FilesModified); err != nil {
			logutil.Debug("synapses: tasks: unmarshal files_modified for session state %q: %v\n", st.ID, err)
		}
		if err := json.Unmarshal([]byte(completedJSON), &st.CompletedSteps); err != nil {
			logutil.Debug("synapses: tasks: unmarshal completed_steps for session state %q: %v\n", st.ID, err)
		}
		if err := json.Unmarshal([]byte(remainingJSON), &st.RemainingSteps); err != nil {
			logutil.Debug("synapses: tasks: unmarshal remaining_steps for session state %q: %v\n", st.ID, err)
		}
		if err := json.Unmarshal([]byte(blockersJSON), &st.Blockers); err != nil {
			logutil.Debug("synapses: tasks: unmarshal blockers for session state %q: %v\n", st.ID, err)
		}
		if err := json.Unmarshal([]byte(decisionsJSON), &st.Decisions); err != nil {
			logutil.Debug("synapses: tasks: unmarshal decisions for session state %q: %v\n", st.ID, err)
		}
		result[st.TaskID] = &st
	}
	return result, rows.Err()
}

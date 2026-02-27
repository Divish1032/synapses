package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
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

// PlanSummary is a plan with task completion counts, used by GetPlans.
type PlanSummary struct {
	Plan
	TotalTasks   int `json:"total_tasks"`
	PendingTasks int `json:"pending_tasks"`
	DoneTasks    int `json:"done_tasks"`
}

// CreatePlan persists a plan and its initial tasks atomically.
// agentID is optional — if non-empty it records which agent created the plan.
// Returns the plan ID.
func (s *Store) CreatePlan(title, description, agentID string, tasks []TaskInput) (string, error) {
	planID := newID()
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.Exec(
		`INSERT INTO plans (id, title, description, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		planID, title, description, agentID, now, now,
	)
	if err != nil {
		return "", fmt.Errorf("insert plan: %w", err)
	}

	for _, t := range tasks {
		taskID := newID()
		priority := t.Priority
		if priority == "" {
			priority = "p2"
		}
		linked, _ := json.Marshal(t.LinkedNodes)
		deps, _ := json.Marshal(t.DependsOn)
		_, err = tx.Exec(
			`INSERT INTO tasks (id, plan_id, title, description, status, priority, linked_nodes, depends_on, notes, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, '', ?, ?)`,
			taskID, planID, t.Title, t.Description, priority, string(linked), string(deps), now, now,
		)
		if err != nil {
			return "", fmt.Errorf("insert task %q: %w", t.Title, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return planID, nil
}

// GetPendingTasks returns all tasks with status 'pending' or 'in_progress',
// ordered by priority (p0 first) then creation time.
// If planID is non-empty, results are filtered to that plan only.
// If agentID is non-empty, results are filtered to tasks assigned to that agent.
// Each task's Blocked/BlockedBy fields are computed from depends_on status.
func (s *Store) GetPendingTasks(planID, agentID string) ([]Task, error) {
	query := `
		SELECT id, plan_id, title, description, status, priority, linked_nodes, depends_on, notes,
		       assigned_to, last_updated_by, created_at, updated_at
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
		created_at ASC`

	rows, err := s.db.Query(query, args...)
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
				_, _ = s.db.Exec(
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

// UpdateLinkedNodes replaces the linked_nodes for a task with nodeIDs.
// Call with the full desired set (existing + newly detected); deduplication is
// the caller's responsibility. Used by handleLinkTaskNodes and autoLinkNodes.
func (s *Store) UpdateLinkedNodes(taskID string, nodeIDs []string) error {
	if nodeIDs == nil {
		nodeIDs = []string{}
	}
	linked, _ := json.Marshal(nodeIDs)
	_, err := s.db.Exec(
		`UPDATE tasks SET linked_nodes = ?, updated_at = ? WHERE id = ?`,
		string(linked), time.Now().UTC().Format(time.RFC3339), taskID,
	)
	return err
}

// UpdateTask changes the status and optionally appends notes to a task.
// agentID is optional — if non-empty it is recorded as the last_updated_by agent.
// Appended notes are prefixed with a timestamp so they form an audit trail.
// Returns the IDs of tasks that became unblocked as a result of this update
// (only meaningful when status == "done").
func (s *Store) UpdateTask(id, status, appendNotes, agentID string) ([]string, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	if appendNotes != "" {
		var existing string
		row := s.db.QueryRow(`SELECT notes FROM tasks WHERE id = ?`, id)
		if err := row.Scan(&existing); err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("read task notes: %w", err)
		}
		newNote := fmt.Sprintf("[%s] %s", now, appendNotes)
		if existing != "" {
			existing = existing + "\n" + newNote
		} else {
			existing = newNote
		}
		if _, err := s.db.Exec(
			`UPDATE tasks SET status = ?, notes = ?, last_updated_by = ?, updated_at = ? WHERE id = ?`,
			status, existing, agentID, now, id,
		); err != nil {
			return nil, err
		}
	} else {
		if _, err := s.db.Exec(
			`UPDATE tasks SET status = ?, last_updated_by = ?, updated_at = ? WHERE id = ?`,
			status, agentID, now, id,
		); err != nil {
			return nil, err
		}
	}

	// If marking done, find tasks that depended on this task and are now unblocked.
	if status != "done" {
		return nil, nil
	}
	return s.findNewlyUnblocked(id)
}

// findNewlyUnblocked returns IDs of pending tasks that had `completedID` as
// their only blocking dependency (i.e. all other deps are now done too).
func (s *Store) findNewlyUnblocked(completedID string) ([]string, error) {
	// Find all pending/in_progress tasks that depend on completedID.
	rows, err := s.db.Query(
		`SELECT id, depends_on FROM tasks WHERE status IN ('pending','in_progress') AND depends_on LIKE ?`,
		"%"+completedID+"%",
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
		_ = json.Unmarshal([]byte(depsJSON), &deps)
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

// GetTask retrieves a single task by ID. Returns an error wrapping sql.ErrNoRows if not found.
func (s *Store) GetTask(id string) (*Task, error) {
	row := s.db.QueryRow(`
		SELECT id, plan_id, title, description, status, priority, linked_nodes, depends_on, notes,
		       assigned_to, last_updated_by, created_at, updated_at
		FROM tasks WHERE id = ?`, id)
	var t Task
	var linkedJSON, depsJSON string
	if err := row.Scan(
		&t.ID, &t.PlanID, &t.Title, &t.Description,
		&t.Status, &t.Priority, &linkedJSON, &depsJSON, &t.Notes,
		&t.AssignedTo, &t.LastUpdatedBy,
		&t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("get task %q: %w", id, err)
	}
	_ = json.Unmarshal([]byte(linkedJSON), &t.LinkedNodes)
	if t.LinkedNodes == nil {
		t.LinkedNodes = []string{}
	}
	_ = json.Unmarshal([]byte(depsJSON), &t.DependsOn)
	if t.DependsOn == nil {
		t.DependsOn = []string{}
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
	rows, err := s.db.Query(
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
	rows, err := s.db.Query(`
		SELECT p.id, p.title, p.description, p.created_by, p.created_at, p.updated_at,
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
		var s PlanSummary
		if err := rows.Scan(
			&s.ID, &s.Title, &s.Description, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
			&s.TotalTasks, &s.PendingTasks, &s.DoneTasks,
		); err != nil {
			return nil, err
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// scanTasks reads task rows into a slice.
func scanTasks(rows *sql.Rows) ([]Task, error) {
	var tasks []Task
	for rows.Next() {
		var t Task
		var linkedJSON, depsJSON string
		if err := rows.Scan(
			&t.ID, &t.PlanID, &t.Title, &t.Description,
			&t.Status, &t.Priority, &linkedJSON, &depsJSON, &t.Notes,
			&t.AssignedTo, &t.LastUpdatedBy,
			&t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(linkedJSON), &t.LinkedNodes)
		if t.LinkedNodes == nil {
			t.LinkedNodes = []string{}
		}
		_ = json.Unmarshal([]byte(depsJSON), &t.DependsOn)
		if t.DependsOn == nil {
			t.DependsOn = []string{}
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// newID generates a short random ID for plans and tasks.
// Uses time + random suffix — collision probability is negligible for local use.
func newID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

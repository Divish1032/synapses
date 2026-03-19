package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Episode records a decision or failure made by an agent so future sessions can
// recall it. episode_type distinguishes decisions from failures; outcome tracks
// whether the approach worked. promoted_rule links a failure to the dynamic_rule
// it eventually spawned.
type Episode struct {
	ID            string  `json:"id"`
	AgentID       string  `json:"agent_id"`
	ProjectID     string  `json:"project_id,omitempty"`
	CreatedAt     int64   `json:"created_at"` // Unix seconds
	EpisodeType   string  `json:"episode_type"`
	Outcome       string  `json:"outcome"`
	Trigger       string  `json:"trigger,omitempty"`
	Decision      string  `json:"decision"`
	Rationale     string  `json:"rationale,omitempty"`
	AffectedFiles string  `json:"affected_files"` // JSON array string
	AffectedNodes string  `json:"affected_nodes"` // JSON array string
	Tags          string  `json:"tags"`           // JSON array string
	Importance    float64 `json:"importance"`
	PromotedRule  string  `json:"promoted_rule,omitempty"`
}

// RuleCandidate is a failure pattern that has appeared enough times to be
// promoted to a dynamic architectural rule.
type RuleCandidate struct {
	Decision    string `json:"decision"`
	Trigger     string `json:"trigger,omitempty"`
	Occurrences int    `json:"occurrences"`
	EpisodeIDs  string `json:"episode_ids"` // JSON array of ids
}

// RememberEpisode inserts a new episode and keeps the FTS5 index in sync.
func (s *Store) RememberEpisode(e Episode) (string, error) {
	if e.ID == "" {
		e.ID = newID()
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().Unix()
	}
	if e.EpisodeType == "" {
		e.EpisodeType = "decision"
	}
	if e.Outcome == "" {
		e.Outcome = "unknown"
	}
	if e.AffectedFiles == "" {
		e.AffectedFiles = "[]"
	}
	if e.AffectedNodes == "" {
		e.AffectedNodes = "[]"
	}
	if e.Tags == "" {
		e.Tags = "[]"
	}
	if e.Importance == 0 {
		e.Importance = 0.5
	}

	_, err := s.knowledgeDB.Exec(
		`INSERT INTO episodes
		 (id, agent_id, project_id, created_at, episode_type, outcome,
		  trigger, decision, rationale, affected_files, affected_nodes,
		  tags, importance, promoted_rule)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.AgentID, e.ProjectID, e.CreatedAt, e.EpisodeType, e.Outcome,
		e.Trigger, e.Decision, e.Rationale, e.AffectedFiles, e.AffectedNodes,
		e.Tags, e.Importance, e.PromotedRule,
	)
	if err != nil {
		return "", fmt.Errorf("remember episode: %w", err)
	}
	// FTS5 index is kept in sync automatically by the episodes_ai trigger.
	return e.ID, nil
}

// RecallEpisodes performs an FTS5 BM25 search over episodes matching query.
// Optional filters: projectID (empty = all), agentID (empty = all),
// episodeType (empty = all), outcomeFilter (empty = all).
// sinceDays limits results to the last N days (0 = no time filter).
// Returns up to limit results ordered by relevance (best match first).
// v1 uses top-N strategy with no score threshold — caller decides relevance.
func (s *Store) RecallEpisodes(query, projectID, agentID, episodeType, outcomeFilter string, limit, sinceDays int) ([]Episode, error) {
	if limit <= 0 {
		limit = 10
	}

	// Sanitize query before passing to FTS5 MATCH to prevent syntax errors
	// from special characters like ".", "-", "/", etc.
	safeQuery := sanitizeFTSQuery(query)
	if safeQuery == "" {
		return nil, nil
	}

	// FTS5 MATCH returns rows ordered by BM25 rank (most relevant first).
	baseQuery := `
		SELECT e.id, e.agent_id, e.project_id, e.created_at, e.episode_type,
		       e.outcome, e.trigger, e.decision, e.rationale,
		       e.affected_files, e.affected_nodes, e.tags, e.importance, e.promoted_rule
		FROM episodes_fts
		JOIN episodes e ON episodes_fts.rowid = e.rowid
		WHERE episodes_fts MATCH ?`

	args := []interface{}{safeQuery}

	if sinceDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -sinceDays).Unix()
		baseQuery += ` AND e.created_at >= ?`
		args = append(args, cutoff)
	}
	if projectID != "" {
		baseQuery += ` AND e.project_id = ?`
		args = append(args, projectID)
	}
	if agentID != "" {
		baseQuery += ` AND e.agent_id = ?`
		args = append(args, agentID)
	}
	if episodeType != "" {
		baseQuery += ` AND e.episode_type = ?`
		args = append(args, episodeType)
	}
	if outcomeFilter != "" {
		baseQuery += ` AND e.outcome = ?`
		args = append(args, outcomeFilter)
	}

	baseQuery += ` ORDER BY rank LIMIT ?`
	args = append(args, limit)

	rows, err := s.knowledgeDB.Query(baseQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("recall episodes: %w", err)
	}
	defer rows.Close()

	return scanEpisodes(rows)
}

// GetEpisodes lists episodes with optional filters, ordered by created_at DESC.
func (s *Store) GetEpisodes(projectID, agentID, episodeType string, tags []string, limit int, sinceDays int) ([]Episode, error) {
	if limit <= 0 {
		limit = 20
	}

	query := `
		SELECT id, agent_id, project_id, created_at, episode_type, outcome,
		       trigger, decision, rationale, affected_files, affected_nodes,
		       tags, importance, promoted_rule
		FROM episodes WHERE 1=1`

	args := []interface{}{}

	if projectID != "" {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	if agentID != "" {
		query += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	if episodeType != "" {
		query += ` AND episode_type = ?`
		args = append(args, episodeType)
	}
	if sinceDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -sinceDays).Unix()
		query += ` AND created_at >= ?`
		args = append(args, cutoff)
	}
	// tag filter: simple substring match on the JSON array string
	for _, tag := range tags {
		query += ` AND tags LIKE ?`
		args = append(args, "%"+tag+"%")
	}

	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.knowledgeDB.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get episodes: %w", err)
	}
	defer rows.Close()

	return scanEpisodes(rows)
}

// CheckPlanSafety searches failure episodes for the closest match to planDesc.
// Returns the top-1 matching failure episode (caller decides relevance).
// Returns nil, nil when no failure episodes exist yet (cold-start safe).
//
// Uses OR matching (any key term matches) rather than AND matching, because
// plan descriptions use different words than stored episodes — "change auth
// handler" should match "modified auth token validation" via shared key terms.
func (s *Store) CheckPlanSafety(planDesc, projectID string) (*Episode, error) {
	safeQuery := buildORQuery(planDesc)
	if safeQuery == "" {
		return nil, nil
	}

	query := `
		SELECT e.id, e.agent_id, e.project_id, e.created_at, e.episode_type,
		       e.outcome, e.trigger, e.decision, e.rationale,
		       e.affected_files, e.affected_nodes, e.tags, e.importance, e.promoted_rule
		FROM episodes_fts
		JOIN episodes e ON episodes_fts.rowid = e.rowid
		WHERE episodes_fts MATCH ?
		  AND e.episode_type = 'failure'`

	args := []interface{}{safeQuery}
	if projectID != "" {
		query += ` AND e.project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY rank LIMIT 1`

	row := s.knowledgeDB.QueryRow(query, args...)
	episodes, err := scanEpisodeRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check plan safety: %w", err)
	}
	return episodes, nil
}

// FindEpisodesByNodeID searches episodes where affected_nodes contains the
// given node ID. Used by federation to find memories anchored to specific
// entities, which is more precise than text-based FTS search on entity names.
// Returns up to limit results ordered by recency (newest first).
func (s *Store) FindEpisodesByNodeID(nodeID string, limit int) ([]Episode, error) {
	if nodeID == "" || limit <= 0 {
		return nil, nil
	}
	// Use LIKE with the node ID substring. The affected_nodes column stores
	// a JSON array like '["repo::file.go::Name"]'. We search for the node ID
	// within it. escapeLike handles % and _ in node IDs.
	pattern := "%" + escapeLike(nodeID) + "%"
	rows, err := s.knowledgeDB.Query(`
		SELECT id, agent_id, project_id, created_at, episode_type, outcome,
		       trigger, decision, rationale, affected_files, affected_nodes,
		       tags, importance, promoted_rule
		FROM episodes
		WHERE affected_nodes LIKE ? ESCAPE '\'
		ORDER BY created_at DESC
		LIMIT ?`, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("find episodes by node: %w", err)
	}
	defer rows.Close()
	return scanEpisodes(rows)
}

// GetRuleCandidates returns failure episodes that have appeared ≥minOccurrences
// times (matched by decision similarity) and have not yet been promoted to a rule.
// Uses exact decision-text grouping as a v1 approximation; FTS/vector grouping later.
func (s *Store) GetRuleCandidates(minOccurrences int) ([]RuleCandidate, error) {
	if minOccurrences <= 0 {
		minOccurrences = 2
	}

	rows, err := s.knowledgeDB.Query(`
		SELECT decision, trigger, COUNT(*) as occurrences,
		       json_group_array(id) as episode_ids
		FROM episodes
		WHERE episode_type = 'failure'
		  AND (promoted_rule = '' OR promoted_rule IS NULL)
		GROUP BY decision
		HAVING COUNT(*) >= ?
		ORDER BY occurrences DESC`,
		minOccurrences,
	)
	if err != nil {
		return nil, fmt.Errorf("get rule candidates: %w", err)
	}
	defer rows.Close()

	var candidates []RuleCandidate
	for rows.Next() {
		var c RuleCandidate
		if err := rows.Scan(&c.Decision, &c.Trigger, &c.Occurrences, &c.EpisodeIDs); err != nil {
			return nil, fmt.Errorf("scan rule candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

// MarkEpisodePromoted sets promoted_rule on an episode after it has been
// converted to a dynamic_rule via upsert_rule().
func (s *Store) MarkEpisodePromoted(episodeID, ruleID string) error {
	_, err := s.knowledgeDB.Exec(
		`UPDATE episodes SET promoted_rule = ? WHERE id = ?`,
		ruleID, episodeID,
	)
	if err != nil {
		return fmt.Errorf("mark episode promoted: %w", err)
	}
	return nil
}

// scanEpisodes reads all rows into an Episode slice.
func scanEpisodes(rows *sql.Rows) ([]Episode, error) {
	var out []Episode
	for rows.Next() {
		var e Episode
		if err := rows.Scan(
			&e.ID, &e.AgentID, &e.ProjectID, &e.CreatedAt, &e.EpisodeType,
			&e.Outcome, &e.Trigger, &e.Decision, &e.Rationale,
			&e.AffectedFiles, &e.AffectedNodes, &e.Tags, &e.Importance, &e.PromotedRule,
		); err != nil {
			return nil, fmt.Errorf("scan episode: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// scanEpisodeRow reads a single *sql.Row into an Episode.
func scanEpisodeRow(row *sql.Row) (*Episode, error) {
	var e Episode
	err := row.Scan(
		&e.ID, &e.AgentID, &e.ProjectID, &e.CreatedAt, &e.EpisodeType,
		&e.Outcome, &e.Trigger, &e.Decision, &e.Rationale,
		&e.AffectedFiles, &e.AffectedNodes, &e.Tags, &e.Importance, &e.PromotedRule,
	)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// buildORQuery converts a natural-language plan description into an FTS5 OR
// query so CheckPlanSafety matches any key term rather than requiring all terms.
// "change auth token logic" → `"auth"* OR "token"* OR "change"* OR "logic"*`
// Short stop-words (≤3 chars) are filtered to reduce noise.
func buildORQuery(s string) string {
	// Reuse sanitizeFTSQuery which handles special-char stripping and prefix quoting.
	sanitized := sanitizeFTSQuery(s)
	if sanitized == "" {
		return ""
	}
	// sanitizeFTSQuery returns `"word1"* OR "word2"* OR "word3"*`.
	// Split on whitespace to get term tokens interleaved with "OR" separators.
	parts := strings.Fields(sanitized)
	var kept []string
	for _, p := range parts {
		// Skip "OR" separator tokens injected by sanitizeFTSQuery.
		if p == "OR" {
			continue
		}
		// Strip quotes and * to measure raw word length for stop-word filter.
		raw := strings.Trim(p, `"*`)
		if len(raw) > 3 {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		// All words were short — fall back to including them anyway.
		// Collect term tokens only (skip "OR" separators) to avoid
		// producing invalid FTS5 like `"go" OR OR OR "is"`.
		for _, p := range parts {
			if p != "OR" {
				kept = append(kept, p)
			}
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, " OR ")
}

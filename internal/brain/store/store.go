// Package store manages the brain's own SQLite database.
// It stores semantic summaries, cached violation explanations, SDLC state,
// decision history, and learned co-occurrence patterns.
// This is separate from Synapses' SQLite database.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

const schema = `
CREATE TABLE IF NOT EXISTS semantic_summaries (
	project_id TEXT NOT NULL DEFAULT '',
	node_id    TEXT NOT NULL,
	node_name  TEXT NOT NULL,
	summary    TEXT NOT NULL,
	tags       TEXT NOT NULL DEFAULT '[]',
	updated_at TEXT NOT NULL,
	PRIMARY KEY (project_id, node_id)
);
CREATE INDEX IF NOT EXISTS idx_ss_node_id ON semantic_summaries(node_id);

CREATE TABLE IF NOT EXISTS violation_cache (
	rule_id      TEXT NOT NULL,
	source_file  TEXT NOT NULL,
	explanation  TEXT NOT NULL,
	fix          TEXT NOT NULL,
	cached_at    TEXT NOT NULL,
	PRIMARY KEY (rule_id, source_file)
);

CREATE TABLE IF NOT EXISTS sdlc_config (
	id           TEXT PRIMARY KEY DEFAULT 'default',
	phase        TEXT NOT NULL DEFAULT 'development',
	quality_mode TEXT NOT NULL DEFAULT 'standard',
	updated_at   TEXT NOT NULL,
	updated_by   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS context_patterns (
	id          TEXT PRIMARY KEY,
	trigger     TEXT NOT NULL,
	co_change   TEXT NOT NULL,
	reason      TEXT NOT NULL DEFAULT '',
	co_count    INTEGER NOT NULL DEFAULT 1,
	total_count INTEGER NOT NULL DEFAULT 1,
	confidence  REAL NOT NULL DEFAULT 1.0,
	updated_at  TEXT NOT NULL,
	UNIQUE(trigger, co_change)
);
CREATE INDEX IF NOT EXISTS idx_patterns_trigger    ON context_patterns(trigger);
CREATE INDEX IF NOT EXISTS idx_patterns_confidence ON context_patterns(confidence DESC);

-- Caches LLM-generated insight per (node_id, phase). TTL: 6 hours.
-- Avoids repeat LLM calls for unchanged code during the same work session.
CREATE TABLE IF NOT EXISTS insight_cache (
	node_id    TEXT NOT NULL,
	phase      TEXT NOT NULL,
	insight    TEXT NOT NULL,
	concerns   TEXT NOT NULL DEFAULT '[]',
	cached_at  TEXT NOT NULL,
	PRIMARY KEY (node_id, phase)
);
CREATE INDEX IF NOT EXISTS idx_insight_node ON insight_cache(node_id);

CREATE TABLE IF NOT EXISTS decision_log (
	id               TEXT PRIMARY KEY,
	agent_id         TEXT NOT NULL DEFAULT '',
	phase            TEXT NOT NULL DEFAULT '',
	entity_name      TEXT NOT NULL,
	action           TEXT NOT NULL,
	related_entities TEXT NOT NULL DEFAULT '[]',
	outcome          TEXT NOT NULL DEFAULT '',
	notes            TEXT NOT NULL DEFAULT '',
	created_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_dlog_entity  ON decision_log(entity_name);
CREATE INDEX IF NOT EXISTS idx_dlog_created ON decision_log(created_at);

-- Architectural Decision Records: persistent cold memory for key design choices.
-- linked_files is a JSON array of file path glob patterns (e.g. ["internal/store/", "*.go"]).
CREATE TABLE IF NOT EXISTS adrs (
	id           TEXT PRIMARY KEY,
	title        TEXT NOT NULL,
	status       TEXT NOT NULL DEFAULT 'proposed',
	context_text TEXT NOT NULL DEFAULT '',
	decision     TEXT NOT NULL,
	consequences TEXT NOT NULL DEFAULT '',
	linked_files TEXT NOT NULL DEFAULT '[]',
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_adrs_status ON adrs(status);
`

// Store is the brain's persistent SQLite store.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the brain SQLite database at the given path.
// Parent directories are created if they do not exist.
// Old data is pruned at startup (decision_log >30d, stale patterns >14d).
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"+
		"&_pragma=synchronous(NORMAL)&_pragma=cache_size(-65536)"+
		"&_pragma=mmap_size(268435456)&_pragma=temp_store(MEMORY)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	s := &Store{db: db}
	s.pruneOldData()
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// pruneOldData removes stale entries at startup to keep brain.sqlite small.
func (s *Store) pruneOldData() {
	now := time.Now().UTC()
	// Prune decision log entries older than 30 days.
	cutoff30d := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := s.db.Exec(`DELETE FROM decision_log WHERE created_at < ?`, cutoff30d); err != nil {
		logutil.Error("brain store: prune decision_log: %v\n", err)
	}
	// Prune weak, stale patterns (seen < 2 times AND older than 14 days).
	cutoff14d := now.Add(-14 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := s.db.Exec(`DELETE FROM context_patterns WHERE co_count < 2 AND updated_at < ?`, cutoff14d); err != nil {
		logutil.Error("brain store: prune context_patterns: %v\n", err)
	}
	// Prune insight cache entries older than 6 hours (stale insight).
	cutoff6h := now.Add(-6 * time.Hour).Format(time.RFC3339)
	if _, err := s.db.Exec(`DELETE FROM insight_cache WHERE cached_at < ?`, cutoff6h); err != nil {
		logutil.Error("brain store: prune insight_cache: %v\n", err)
	}
	// Prune violation cache entries older than 7 days (rules can change).
	cutoff7d := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := s.db.Exec(`DELETE FROM violation_cache WHERE cached_at < ?`, cutoff7d); err != nil {
		logutil.Error("brain store: prune violation_cache: %v\n", err)
	}
}

// --- Semantic Summaries ---

// UpsertSummary stores or updates the semantic summary and tags for a node.
// If the node already exists (re-ingest), the insight cache is invalidated for
// all phases — the old insight may no longer match the updated code.
// projectID scopes the summary; use "" for legacy/unscoped entries.
func (s *Store) UpsertSummary(projectID, nodeID, nodeName, summary string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	tagsJSON, _ := json.Marshal(tags)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
		INSERT INTO semantic_summaries (project_id, node_id, node_name, summary, tags, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, node_id) DO UPDATE SET
			node_name  = excluded.node_name,
			summary    = excluded.summary,
			tags       = excluded.tags,
			updated_at = excluded.updated_at`,
		projectID, nodeID, nodeName, summary, string(tagsJSON), now,
	)
	if err != nil {
		return err
	}
	// Invalidate cached insight — code has changed, old insight is stale.
	_, _ = s.db.Exec(`DELETE FROM insight_cache WHERE node_id = ?`, nodeID)
	return nil
}

// GetSummary returns the stored summary for a node, or "" if not found.
// Falls back to the empty-projectID (legacy) row if no project-scoped row exists.
func (s *Store) GetSummary(projectID, nodeID string) string {
	var summary string
	err := s.db.QueryRow(
		`SELECT summary FROM semantic_summaries WHERE project_id = ? AND node_id = ?`,
		projectID, nodeID,
	).Scan(&summary)
	if err == nil {
		return summary
	}
	// Fallback: legacy unscoped entry
	s.db.QueryRow(
		`SELECT summary FROM semantic_summaries WHERE project_id = '' AND node_id = ?`, nodeID,
	).Scan(&summary)
	return summary
}

// GetSummaries returns summaries for all given node IDs keyed by node ID.
// Missing nodes are omitted from the result map.
func (s *Store) GetSummaries(projectID string, nodeIDs []string) map[string]string {
	result := make(map[string]string, len(nodeIDs))
	for _, id := range nodeIDs {
		if sm := s.GetSummary(projectID, id); sm != "" {
			result[id] = sm
		}
	}
	return result
}

// GetSummariesByName returns summaries keyed by node_name for the given names.
// This is used by the contextbuilder to look up dep summaries by entity name.
func (s *Store) GetSummariesByName(names []string) map[string]string {
	if len(names) == 0 {
		return map[string]string{}
	}
	placeholders := make([]string, len(names))
	args := make([]interface{}, len(names))
	for i, n := range names {
		placeholders[i] = "?"
		args[i] = n
	}
	query := fmt.Sprintf(
		`SELECT node_name, summary FROM semantic_summaries WHERE node_name IN (%s)`,
		strings.Join(placeholders, ","),
	)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return map[string]string{}
	}
	defer rows.Close()
	result := make(map[string]string, len(names))
	for rows.Next() {
		var name, summary string
		if rows.Scan(&name, &summary) == nil {
			result[name] = summary
		}
	}
	return result
}

// --- Violation Cache ---

// UpsertViolationExplanation caches a plain-English explanation for a rule+file pair.
func (s *Store) UpsertViolationExplanation(ruleID, sourceFile, explanation, fix string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
		INSERT INTO violation_cache (rule_id, source_file, explanation, fix, cached_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(rule_id, source_file) DO UPDATE SET
			explanation = excluded.explanation,
			fix         = excluded.fix,
			cached_at   = excluded.cached_at`,
		ruleID, sourceFile, explanation, fix, now,
	)
	return err
}

// GetViolationExplanation returns the cached explanation for a rule+file pair.
// Returns ("", "", false) if not cached.
func (s *Store) GetViolationExplanation(ruleID, sourceFile string) (explanation, fix string, found bool) {
	err := s.db.QueryRow(
		`SELECT explanation, fix FROM violation_cache WHERE rule_id = ? AND source_file = ?`,
		ruleID, sourceFile,
	).Scan(&explanation, &fix)
	if err != nil {
		return "", "", false
	}
	return explanation, fix, true
}

// --- Insight Cache ---

// InsightCacheEntry is a stored LLM-generated insight for a (node_id, phase) pair.
type InsightCacheEntry struct {
	Insight  string
	Concerns []string
}

// GetInsightCache returns the cached insight for a (nodeID, phase) pair.
// Returns ("", nil, false) if not cached or if the entry was pruned (>6h old).
func (s *Store) GetInsightCache(nodeID, phase string) (entry InsightCacheEntry, found bool) {
	var insight, concernsJSON string
	err := s.db.QueryRow(
		`SELECT insight, concerns FROM insight_cache WHERE node_id = ? AND phase = ?`,
		nodeID, phase,
	).Scan(&insight, &concernsJSON)
	if err != nil {
		return InsightCacheEntry{}, false
	}
	var concerns []string
	if err := json.Unmarshal([]byte(concernsJSON), &concerns); err != nil {
		logutil.Warn("brain store: decode concerns for insight cache %s/%s: %v\n", nodeID, phase, err)
	}
	return InsightCacheEntry{Insight: insight, Concerns: concerns}, true
}

// UpsertInsightCache stores a (nodeID, phase) → insight mapping.
// Existing entries are replaced (insight content may have improved).
func (s *Store) UpsertInsightCache(nodeID, phase, insight string, concerns []string) error {
	if concerns == nil {
		concerns = []string{}
	}
	concernsJSON, _ := json.Marshal(concerns)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
		INSERT INTO insight_cache (node_id, phase, insight, concerns, cached_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(node_id, phase) DO UPDATE SET
			insight   = excluded.insight,
			concerns  = excluded.concerns,
			cached_at = excluded.cached_at`,
		nodeID, phase, insight, string(concernsJSON), now,
	)
	return err
}

// --- SDLC Config ---

// SDLCConfigRow is the stored project SDLC state.
type SDLCConfigRow struct {
	Phase       string
	QualityMode string
	UpdatedAt   string
	UpdatedBy   string
}

// GetSDLCConfig returns the current project SDLC config.
// Returns defaults if no config row exists.
func (s *Store) GetSDLCConfig() SDLCConfigRow {
	var row SDLCConfigRow
	err := s.db.QueryRow(
		`SELECT phase, quality_mode, updated_at, updated_by FROM sdlc_config WHERE id = 'default'`,
	).Scan(&row.Phase, &row.QualityMode, &row.UpdatedAt, &row.UpdatedBy)
	if err != nil {
		return SDLCConfigRow{
			Phase:       "development",
			QualityMode: "standard",
			UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
		}
	}
	return row
}

// UpsertSDLCConfig saves the SDLC config row.
func (s *Store) UpsertSDLCConfig(phase, qualityMode, updatedBy string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
		INSERT INTO sdlc_config (id, phase, quality_mode, updated_at, updated_by)
		VALUES ('default', ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			phase        = excluded.phase,
			quality_mode = excluded.quality_mode,
			updated_at   = excluded.updated_at,
			updated_by   = excluded.updated_by`,
		phase, qualityMode, now, updatedBy,
	)
	return err
}

// --- Context Patterns (co-occurrence learning) ---

// ContextPattern is a learned co-occurrence rule.
type ContextPattern struct {
	Trigger    string
	CoChange   string
	Reason     string
	Confidence float64
	CoCount    int
}

// GetPatternsForTriggers returns the top patterns for any of the given trigger names.
// Results are ordered by confidence descending, capped at limit.
func (s *Store) GetPatternsForTriggers(triggers []string, limit int) []ContextPattern {
	if len(triggers) == 0 || limit <= 0 {
		return nil
	}
	placeholders := make([]string, len(triggers))
	args := make([]interface{}, len(triggers))
	for i, t := range triggers {
		placeholders[i] = "?"
		args[i] = t
	}
	args = append(args, limit)
	query := fmt.Sprintf(
		`SELECT trigger, co_change, reason, confidence, co_count
		 FROM context_patterns
		 WHERE trigger IN (%s)
		 ORDER BY confidence DESC
		 LIMIT ?`,
		strings.Join(placeholders, ","),
	)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ContextPattern
	for rows.Next() {
		var p ContextPattern
		if rows.Scan(&p.Trigger, &p.CoChange, &p.Reason, &p.Confidence, &p.CoCount) == nil {
			out = append(out, p)
		}
	}
	return out
}

// AllPatterns returns all patterns ordered by confidence for CLI display.
func (s *Store) AllPatterns() ([]ContextPattern, error) {
	rows, err := s.db.Query(
		`SELECT trigger, co_change, reason, confidence, co_count
		 FROM context_patterns ORDER BY confidence DESC, co_count DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContextPattern
	for rows.Next() {
		var p ContextPattern
		if rows.Scan(&p.Trigger, &p.CoChange, &p.Reason, &p.Confidence, &p.CoCount) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// UpsertPattern adds or increments a co-occurrence pattern.
// coCount and totalCount are incremented; confidence is recomputed.
func (s *Store) UpsertPattern(trigger, coChange, reason string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	// Truncate reason to 100 chars.
	if len(reason) > 100 {
		reason = reason[:100]
	}
	id := fmt.Sprintf("%s::%s", trigger, coChange)
	_, err := s.db.Exec(`
		INSERT INTO context_patterns (id, trigger, co_change, reason, co_count, total_count, confidence, updated_at)
		VALUES (?, ?, ?, ?, 1, 1, 1.0, ?)
		ON CONFLICT(trigger, co_change) DO UPDATE SET
			co_count    = co_count + 1,
			total_count = total_count + 1,
			confidence  = CAST(co_count + 1 AS REAL) / CAST(total_count + 1 AS REAL),
			reason      = CASE WHEN excluded.reason != '' THEN excluded.reason ELSE reason END,
			updated_at  = excluded.updated_at`,
		id, trigger, coChange, reason, now,
	)
	return err
}

// --- Decision Log ---

// DecisionLogEntry is a row from decision_log.
type DecisionLogEntry struct {
	ID              string
	AgentID         string
	Phase           string
	EntityName      string
	Action          string
	RelatedEntities []string
	Outcome         string
	Notes           string
	CreatedAt       string
}

// LogDecision inserts a new decision log entry.
func (s *Store) LogDecision(agentID, phase, entityName, action string, relatedEntities []string, outcome, notes string) error {
	if relatedEntities == nil {
		relatedEntities = []string{}
	}
	relJSON, _ := json.Marshal(relatedEntities)
	now := time.Now().UTC()
	id := fmt.Sprintf("%d-%s", now.UnixNano(), entityName)
	_, err := s.db.Exec(`
		INSERT INTO decision_log (id, agent_id, phase, entity_name, action, related_entities, outcome, notes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, agentID, phase, entityName, action,
		string(relJSON), outcome, notes,
		now.UTC().Format(time.RFC3339),
	)
	return err
}

// GetDecisionLog returns up to limit decision log entries, optionally filtered
// by entityName (empty string = all), ordered by created_at DESC.
func (s *Store) GetDecisionLog(entityName string, limit int) ([]DecisionLogEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	var (
		rows *sql.Rows
		err  error
	)
	if entityName != "" {
		rows, err = s.db.Query(`
			SELECT id, agent_id, phase, entity_name, action, related_entities, outcome, notes, created_at
			FROM decision_log
			WHERE entity_name = ?
			ORDER BY created_at DESC
			LIMIT ?`, entityName, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT id, agent_id, phase, entity_name, action, related_entities, outcome, notes, created_at
			FROM decision_log
			ORDER BY created_at DESC
			LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []DecisionLogEntry
	for rows.Next() {
		var e DecisionLogEntry
		var relJSON string
		if err := rows.Scan(&e.ID, &e.AgentID, &e.Phase, &e.EntityName, &e.Action,
			&relJSON, &e.Outcome, &e.Notes, &e.CreatedAt); err != nil {
			continue
		}
		if relJSON != "" {
			_ = json.Unmarshal([]byte(relJSON), &e.RelatedEntities)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// --- Reset ---

// Reset deletes all brain data.
func (s *Store) Reset() error {
	_, err := s.db.Exec(`
		DELETE FROM semantic_summaries;
		DELETE FROM violation_cache;
		DELETE FROM insight_cache;
		DELETE FROM sdlc_config;
		DELETE FROM context_patterns;
		DELETE FROM decision_log;
		DELETE FROM adrs;
	`)
	return err
}

// --- Architectural Decision Records (ADRs) ---

// ADR represents an Architectural Decision Record stored in brain.sqlite.
type ADR struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Status       string   `json:"status"` // proposed | accepted | deprecated | superseded
	ContextText  string   `json:"context,omitempty"`
	Decision     string   `json:"decision"`
	Consequences string   `json:"consequences,omitempty"`
	LinkedFiles  []string `json:"linked_files,omitempty"` // file path glob patterns
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

// UpsertADR creates or updates an ADR. ID must be non-empty.
func (s *Store) UpsertADR(adr ADR) error {
	linkedJSON, _ := json.Marshal(adr.LinkedFiles)
	now := time.Now().UTC().Format(time.RFC3339)
	if adr.CreatedAt == "" {
		adr.CreatedAt = now
	}
	_, err := s.db.Exec(`
		INSERT INTO adrs (id, title, status, context_text, decision, consequences, linked_files, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title        = excluded.title,
			status       = excluded.status,
			context_text = excluded.context_text,
			decision     = excluded.decision,
			consequences = excluded.consequences,
			linked_files = excluded.linked_files,
			updated_at   = excluded.updated_at`,
		adr.ID, adr.Title, adr.Status, adr.ContextText, adr.Decision, adr.Consequences,
		string(linkedJSON), adr.CreatedAt, now,
	)
	return err
}

// GetADR returns the ADR with the given ID, or an error if not found.
func (s *Store) GetADR(id string) (ADR, error) {
	row := s.db.QueryRow(`
		SELECT id, title, status, context_text, decision, consequences, linked_files, created_at, updated_at
		FROM adrs WHERE id = ?`, id)
	return scanADR(row)
}

// AllADRs returns all ADRs ordered by updated_at descending.
func (s *Store) AllADRs() ([]ADR, error) {
	rows, err := s.db.Query(`
		SELECT id, title, status, context_text, decision, consequences, linked_files, created_at, updated_at
		FROM adrs ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanADRRows(rows)
}

// GetADRsForFile returns accepted ADRs whose linked_files patterns match the given file path.
// At most `limit` ADRs are returned; pass 0 for no limit.
func (s *Store) GetADRsForFile(filePath string, limit int) ([]ADR, error) {
	rows, err := s.db.Query(`
		SELECT id, title, status, context_text, decision, consequences, linked_files, created_at, updated_at
		FROM adrs WHERE status = 'accepted' ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all, err := scanADRRows(rows)
	if err != nil {
		return nil, err
	}
	var matched []ADR
	for _, adr := range all {
		for _, pattern := range adr.LinkedFiles {
			if strings.Contains(filePath, pattern) {
				matched = append(matched, adr)
				break
			}
		}
		if limit > 0 && len(matched) >= limit {
			break
		}
	}
	return matched, nil
}

// scanADR reads a single ADR from a sql.Row.
func scanADR(row *sql.Row) (ADR, error) {
	var a ADR
	var linkedRaw string
	if err := row.Scan(&a.ID, &a.Title, &a.Status, &a.ContextText, &a.Decision,
		&a.Consequences, &linkedRaw, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return ADR{}, err
	}
	if err := json.Unmarshal([]byte(linkedRaw), &a.LinkedFiles); err != nil {
		logutil.Debug("synapses: brain: unmarshal linked_files for ADR %q: %v\n", a.ID, err)
	}
	return a, nil
}

// scanADRRows reads multiple ADRs from sql.Rows.
func scanADRRows(rows *sql.Rows) ([]ADR, error) {
	var out []ADR
	for rows.Next() {
		var a ADR
		var linkedRaw string
		if err := rows.Scan(&a.ID, &a.Title, &a.Status, &a.ContextText, &a.Decision,
			&a.Consequences, &linkedRaw, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return out, err
		}
		if err := json.Unmarshal([]byte(linkedRaw), &a.LinkedFiles); err != nil {
			logutil.Debug("synapses: brain: unmarshal linked_files for ADR %q: %v\n", a.ID, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

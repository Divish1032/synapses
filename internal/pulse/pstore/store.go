// Package store implements the SQLite persistence layer for synapses-pulse.
// All analytics data is stored locally in a single SQLite file.
package pulsestore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
)

// Store is the pulse analytics database.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// Open creates or opens a pulse SQLite database at the given path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("pulse store: mkdir: %w", err)
	}

	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("pulse store: open: %w", err)
	}

	// Single writer connection — keep pool small.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pulse store: migrate: %w", err)
	}
	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate creates all tables and indexes if they don't exist.
func (s *Store) migrate() error {
	_, err := s.db.Exec(schema)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS context_deliveries (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    tool_name        TEXT    NOT NULL,
    agent_id         TEXT    NOT NULL DEFAULT '',
    project_id       TEXT    NOT NULL DEFAULT '',
    entity           TEXT    NOT NULL DEFAULT '',
    file             TEXT    NOT NULL DEFAULT '',
    response_bytes   INTEGER NOT NULL DEFAULT 0,
    response_tokens  INTEGER NOT NULL DEFAULT 0,
    baseline_tokens  INTEGER NOT NULL DEFAULT 0,
    nodes_delivered  INTEGER NOT NULL DEFAULT 0,
    nodes_pruned     INTEGER NOT NULL DEFAULT 0,
    edges_delivered  INTEGER NOT NULL DEFAULT 0,
    truncated        INTEGER NOT NULL DEFAULT 0,
    duration_ms      INTEGER NOT NULL DEFAULT 0,
    cache_hit        INTEGER NOT NULL DEFAULT 0,
    brain_enriched   INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_cd_tool    ON context_deliveries(tool_name);
CREATE INDEX IF NOT EXISTS idx_cd_agent   ON context_deliveries(agent_id);
CREATE INDEX IF NOT EXISTS idx_cd_created ON context_deliveries(created_at);

CREATE TABLE IF NOT EXISTS tool_calls (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    tool_name        TEXT    NOT NULL,
    agent_id         TEXT    NOT NULL DEFAULT '',
    project_id       TEXT    NOT NULL DEFAULT '',
    entity           TEXT    NOT NULL DEFAULT '',
    duration_ms      INTEGER NOT NULL DEFAULT 0,
    success          INTEGER NOT NULL DEFAULT 1,
    response_bytes   INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_tc_tool    ON tool_calls(tool_name);
CREATE INDEX IF NOT EXISTS idx_tc_ts      ON tool_calls(created_at);
CREATE INDEX IF NOT EXISTS idx_tc_agent   ON tool_calls(agent_id);

CREATE TABLE IF NOT EXISTS brain_usage (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    model             TEXT    NOT NULL,
    tier              TEXT    NOT NULL DEFAULT '',
    endpoint          TEXT    NOT NULL DEFAULT '',
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    duration_ms       INTEGER NOT NULL DEFAULT 0,
    cost_usd          REAL    NOT NULL DEFAULT 0.0,
    agent_id          TEXT    NOT NULL DEFAULT '',
    project_id        TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_bu_model   ON brain_usage(model);
CREATE INDEX IF NOT EXISTS idx_bu_created ON brain_usage(created_at);

CREATE TABLE IF NOT EXISTS sessions (
    id               TEXT    PRIMARY KEY,
    agent_id         TEXT    NOT NULL,
    project_id       TEXT    NOT NULL DEFAULT '',
    started_at       TEXT    NOT NULL,
    ended_at         TEXT,
    tool_calls       INTEGER NOT NULL DEFAULT 0,
    tokens_saved     INTEGER NOT NULL DEFAULT 0,
    cost_saved_usd   REAL    NOT NULL DEFAULT 0.0,
    tasks_completed  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sess_agent ON sessions(agent_id);
CREATE INDEX IF NOT EXISTS idx_sess_start ON sessions(started_at);

CREATE TABLE IF NOT EXISTS daily_rollups (
    day    TEXT NOT NULL,
    metric TEXT NOT NULL,
    value  REAL NOT NULL DEFAULT 0.0,
    PRIMARY KEY (day, metric)
);

CREATE TABLE IF NOT EXISTS pricing (
    model         TEXT PRIMARY KEY,
    input_per_1m  REAL NOT NULL DEFAULT 0.0,
    output_per_1m REAL NOT NULL DEFAULT 0.0,
    source        TEXT NOT NULL DEFAULT 'default',
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- R29: Intent Alignment Metrics — passive outcome signals.
-- Correlates context delivery quality with downstream agent actions.
CREATE TABLE IF NOT EXISTS outcome_signals (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  TEXT    NOT NULL DEFAULT '',
    agent_id    TEXT    NOT NULL DEFAULT '',
    entity      TEXT    NOT NULL DEFAULT '',
    signal_type TEXT    NOT NULL,
    count       INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_os_entity    ON outcome_signals(entity);
CREATE INDEX IF NOT EXISTS idx_os_project   ON outcome_signals(project_id);
CREATE INDEX IF NOT EXISTS idx_os_signal    ON outcome_signals(signal_type);
CREATE INDEX IF NOT EXISTS idx_os_created   ON outcome_signals(created_at);
`

// ---------------------------------------------------------------------------
// Write methods
// ---------------------------------------------------------------------------

// InsertToolCall records a single MCP tool call event.
func (s *Store) InsertToolCall(ev pulsetypes.ToolCallEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	successInt := 0
	if ev.Success {
		successInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO tool_calls (tool_name, agent_id, project_id, entity, duration_ms, success, response_bytes)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.ToolName, ev.AgentID, ev.ProjectID, ev.Entity, ev.DurationMs, successInt, ev.ResponseBytes,
	)
	return err
}

// InsertContextDelivery records a context delivery with token savings.
func (s *Store) InsertContextDelivery(ev pulsetypes.ContextDeliveryEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	truncInt := 0
	if ev.Truncated {
		truncInt = 1
	}
	cacheInt := 0
	if ev.CacheHit {
		cacheInt = 1
	}
	brainInt := 0
	if ev.BrainEnriched {
		brainInt = 1
	}

	_, err := s.db.Exec(
		`INSERT INTO context_deliveries
		 (tool_name, agent_id, project_id, entity, file, response_bytes, response_tokens, baseline_tokens,
		  nodes_delivered, nodes_pruned, edges_delivered, truncated, duration_ms, cache_hit, brain_enriched)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ToolName, ev.AgentID, ev.ProjectID, ev.Entity, ev.File,
		ev.ResponseBytes, ev.ResponseTokens, ev.BaselineTokens,
		ev.NodesDelivered, ev.NodesPruned, ev.EdgesDelivered,
		truncInt, ev.DurationMs, cacheInt, brainInt,
	)
	return err
}

// InsertBrainUsage records a brain sidecar LLM inference event.
func (s *Store) InsertBrainUsage(ev pulsetypes.BrainUsageEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT INTO brain_usage
		 (model, tier, endpoint, prompt_tokens, completion_tokens, duration_ms, cost_usd, agent_id, project_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Model, ev.Tier, ev.Endpoint, ev.PromptTokens, ev.CompletionTokens,
		ev.DurationMs, ev.CostUSD, ev.AgentID, ev.ProjectID,
	)
	return err
}

// UpsertSession creates or updates a session record.
func (s *Store) UpsertSession(id, agentID, projectID string, event string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	switch event {
	case "start":
		_, err := s.db.Exec(
			`INSERT INTO sessions (id, agent_id, project_id, started_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET started_at = excluded.started_at`,
			id, agentID, projectID, now,
		)
		return err
	case "end":
		_, err := s.db.Exec(
			`UPDATE sessions SET ended_at = ? WHERE id = ?`,
			now, id,
		)
		return err
	case "task_done":
		_, err := s.db.Exec(
			`UPDATE sessions SET tasks_completed = tasks_completed + 1 WHERE id = ?`,
			id,
		)
		return err
	}
	return nil
}

// UpdateSessionStats upserts a session row and increments token savings.
// agentID is used only when the session row needs to be created (INSERT path).
// This ensures tokens are attributed even if session_init never fired pulse.
func (s *Store) UpdateSessionStats(sessionID, agentID, projectID string, tokensSaved int, costSaved float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, agent_id, project_id, started_at, tool_calls, tokens_saved, cost_saved_usd)
		 VALUES (?, ?, ?, ?, 1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		 	tool_calls     = tool_calls + 1,
		 	tokens_saved   = tokens_saved + excluded.tokens_saved,
		 	cost_saved_usd = cost_saved_usd + excluded.cost_saved_usd`,
		sessionID, agentID, projectID, now, tokensSaved, costSaved,
	)
	return err
}

// UpsertPricing inserts or updates a model pricing entry.
func (s *Store) UpsertPricing(model string, inputPer1M, outputPer1M float64, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT INTO pricing (model, input_per_1m, output_per_1m, source) VALUES (?, ?, ?, ?)
		 ON CONFLICT(model) DO UPDATE SET input_per_1m = excluded.input_per_1m,
		   output_per_1m = excluded.output_per_1m, source = excluded.source,
		   updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')`,
		model, inputPer1M, outputPer1M, source,
	)
	return err
}

// UpsertDailyRollup inserts or replaces a daily rollup metric.
func (s *Store) UpsertDailyRollup(day, metric string, value float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT INTO daily_rollups (day, metric, value) VALUES (?, ?, ?)
		 ON CONFLICT(day, metric) DO UPDATE SET value = excluded.value`,
		day, metric, value,
	)
	return err
}

// ---------------------------------------------------------------------------
// Read methods
// ---------------------------------------------------------------------------

// Summary holds aggregated metrics for a time period.
type Summary struct {
	TotalToolCalls     int     `json:"total_tool_calls"`
	TokensDelivered    int     `json:"tokens_delivered"`
	BaselineTokens     int     `json:"baseline_tokens"`
	TokensSaved        int     `json:"tokens_saved"`
	SavingsPct         float64 `json:"savings_pct"`
	CompressionRatio   float64 `json:"compression_ratio"`
	CostSavedUSD       float64 `json:"cost_saved_usd"`
	AvgLatencyMs       float64 `json:"avg_latency_ms"`
	CacheHitRate       float64 `json:"cache_hit_rate"`
	BrainEnrichRate    float64 `json:"brain_enrichment_rate"`
	ContextDeliveries  int     `json:"context_deliveries"`
	Sessions           int     `json:"sessions"`
	TasksCompleted     int     `json:"tasks_completed"`
}

// GetSummary returns aggregated analytics for the last N days.
func (s *Store) GetSummary(days int) (*Summary, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	sum := &Summary{}

	// Tool calls
	row := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(AVG(duration_ms), 0)
		 FROM tool_calls WHERE created_at >= ?`, since)
	if err := row.Scan(&sum.TotalToolCalls, &sum.AvgLatencyMs); err != nil {
		return nil, err
	}

	// Context deliveries
	row = s.db.QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(response_tokens), 0),
		        COALESCE(SUM(baseline_tokens), 0),
		        COALESCE(AVG(CASE WHEN cache_hit = 1 THEN 1.0 ELSE 0.0 END), 0),
		        COALESCE(AVG(CASE WHEN brain_enriched = 1 THEN 1.0 ELSE 0.0 END), 0)
		 FROM context_deliveries WHERE created_at >= ?`, since)
	if err := row.Scan(&sum.ContextDeliveries, &sum.TokensDelivered, &sum.BaselineTokens,
		&sum.CacheHitRate, &sum.BrainEnrichRate); err != nil {
		return nil, err
	}

	// Compute savings
	sum.TokensSaved = sum.BaselineTokens - sum.TokensDelivered
	if sum.TokensSaved < 0 {
		sum.TokensSaved = 0
	}
	if sum.BaselineTokens > 0 {
		sum.SavingsPct = float64(sum.TokensSaved) / float64(sum.BaselineTokens) * 100.0
	}
	if sum.TokensDelivered > 0 {
		sum.CompressionRatio = float64(sum.BaselineTokens) / float64(sum.TokensDelivered)
	} else {
		sum.CompressionRatio = 1.0
	}

	// Sessions
	row = s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(tasks_completed), 0)
		 FROM sessions WHERE started_at >= ?`, since)
	if err := row.Scan(&sum.Sessions, &sum.TasksCompleted); err != nil {
		return nil, err
	}

	return sum, nil
}

// TimelinePoint is a single data point in a time series.
type TimelinePoint struct {
	Date         string  `json:"date"`
	TokensSaved  int     `json:"tokens_saved"`
	ToolCalls    int     `json:"tool_calls"`
	CostSavedUSD float64 `json:"cost_saved_usd"`
}

// GetTimeline returns daily aggregated data for the last N days.
func (s *Store) GetTimeline(days int) ([]TimelinePoint, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	rows, err := s.db.Query(
		`SELECT date(created_at) AS day,
		        COALESCE(SUM(baseline_tokens) - SUM(response_tokens), 0),
		        COUNT(*)
		 FROM context_deliveries
		 WHERE date(created_at) >= ?
		 GROUP BY day ORDER BY day`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var points []TimelinePoint
	for rows.Next() {
		var p TimelinePoint
		if err := rows.Scan(&p.Date, &p.TokensSaved, &p.ToolCalls); err != nil {
			return nil, err
		}
		if p.TokensSaved < 0 {
			p.TokensSaved = 0
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

// ToolStats holds per-tool aggregated metrics.
type ToolStats struct {
	Name               string  `json:"name"`
	Calls              int     `json:"calls"`
	AvgMs              float64 `json:"avg_ms"`
	ErrorRate          float64 `json:"error_rate"`
	AvgTokensDelivered int     `json:"avg_tokens_delivered,omitempty"`
	AvgCompression     float64 `json:"avg_compression,omitempty"`
}

// GetToolStats returns per-tool analytics for the last N days.
func (s *Store) GetToolStats(days int) ([]ToolStats, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	rows, err := s.db.Query(
		`SELECT tool_name,
		        COUNT(*),
		        COALESCE(AVG(duration_ms), 0),
		        COALESCE(AVG(CASE WHEN success = 0 THEN 1.0 ELSE 0.0 END), 0)
		 FROM tool_calls
		 WHERE created_at >= ?
		 GROUP BY tool_name
		 ORDER BY COUNT(*) DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []ToolStats
	for rows.Next() {
		var ts ToolStats
		if err := rows.Scan(&ts.Name, &ts.Calls, &ts.AvgMs, &ts.ErrorRate); err != nil {
			return nil, err
		}
		stats = append(stats, ts)
	}
	return stats, rows.Err()
}

// AgentStats holds per-agent aggregated metrics.
type AgentStats struct {
	AgentID        string `json:"agent_id"`
	Sessions       int    `json:"sessions"`
	ToolCalls      int    `json:"tool_calls"`
	TokensSaved    int    `json:"tokens_saved"`
	TasksCompleted int    `json:"tasks_completed"`
}

// GetAgentStats returns per-agent analytics for the last N days.
func (s *Store) GetAgentStats(days int) ([]AgentStats, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	// sessions.tokens_saved is the canonical per-agent token savings counter,
	// updated by UpdateSessionStats on every context delivery that carries an
	// agent_id. This avoids the LEFT JOIN on context_deliveries which produces
	// zeros whenever the delivery lacks an agent_id (common for bare get_context
	// calls that omit the optional agent_id param).
	rows, err := s.db.Query(
		`SELECT agent_id,
		        COUNT(DISTINCT id)    AS sessions,
		        SUM(tool_calls)       AS tool_calls,
		        SUM(tokens_saved)     AS tokens_saved,
		        SUM(tasks_completed)  AS tasks_completed
		 FROM sessions
		 WHERE started_at >= ?
		 GROUP BY agent_id
		 ORDER BY tokens_saved DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []AgentStats
	for rows.Next() {
		var as AgentStats
		if err := rows.Scan(&as.AgentID, &as.Sessions, &as.ToolCalls, &as.TokensSaved, &as.TasksCompleted); err != nil {
			return nil, err
		}
		stats = append(stats, as)
	}
	return stats, rows.Err()
}

// BrainCostTier holds per-tier brain usage stats.
type BrainCostTier struct {
	Tier   string `json:"tier"`
	Model  string `json:"model"`
	Tokens int    `json:"tokens"`
	Calls  int    `json:"calls"`
}

// BrainCosts holds aggregated brain sidecar usage.
type BrainCosts struct {
	TotalTokens int             `json:"total_tokens"`
	ByTier      []BrainCostTier `json:"by_tier"`
}

// GetBrainCosts returns brain sidecar usage for the last N days.
func (s *Store) GetBrainCosts(days int) (*BrainCosts, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	result := &BrainCosts{}

	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0)
		 FROM brain_usage WHERE created_at >= ?`, since)
	if err := row.Scan(&result.TotalTokens); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(
		`SELECT tier, model,
		        SUM(prompt_tokens + completion_tokens) AS tokens,
		        COUNT(*)
		 FROM brain_usage
		 WHERE created_at >= ?
		 GROUP BY tier, model
		 ORDER BY tokens DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var t BrainCostTier
		if err := rows.Scan(&t.Tier, &t.Model, &t.Tokens, &t.Calls); err != nil {
			return nil, err
		}
		result.ByTier = append(result.ByTier, t)
	}
	return result, rows.Err()
}

// EventCount returns the total number of events across all tables.
func (s *Store) EventCount() (int, error) {
	var total int
	for _, table := range []string{"tool_calls", "context_deliveries", "brain_usage"} {
		var count int
		row := s.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table))
		if err := row.Scan(&count); err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

// PruneOldEvents removes events older than the given number of days.
func (s *Store) PruneOldEvents(retentionDays int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	since := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339)
	var totalDeleted int64

	for _, table := range []string{"tool_calls", "context_deliveries", "brain_usage"} {
		result, err := s.db.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE created_at < ?", table), since)
		if err != nil {
			return totalDeleted, err
		}
		n, _ := result.RowsAffected()
		totalDeleted += n
	}

	// Prune sessions older than retention
	result, err := s.db.Exec(`DELETE FROM sessions WHERE started_at < ?`, since)
	if err != nil {
		return totalDeleted, err
	}
	n, _ := result.RowsAffected()
	totalDeleted += n

	return totalDeleted, nil
}

// GetPricing returns the pricing entry for a model, or zero values if not found.
func (s *Store) GetPricing(model string) (inputPer1M, outputPer1M float64, found bool) {
	row := s.db.QueryRow(
		`SELECT input_per_1m, output_per_1m FROM pricing WHERE model = ?`, model)
	if err := row.Scan(&inputPer1M, &outputPer1M); err != nil {
		return 0, 0, false
	}
	return inputPer1M, outputPer1M, true
}

// TopEntities returns the most frequently queried entities in context deliveries.
func (s *Store) TopEntities(days, limit int) ([]string, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT entity, COUNT(*) AS cnt
		 FROM context_deliveries
		 WHERE created_at >= ? AND entity != ''
		 GROUP BY entity ORDER BY cnt DESC LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entities []string
	for rows.Next() {
		var e string
		var cnt int
		if err := rows.Scan(&e, &cnt); err != nil {
			return nil, err
		}
		entities = append(entities, e)
	}
	return entities, rows.Err()
}

// ---------------------------------------------------------------------------
// R29: Intent Alignment Metrics
// ---------------------------------------------------------------------------

// InsertOutcomeSignal records a passive alignment signal.
func (s *Store) InsertOutcomeSignal(ev pulsetypes.OutcomeSignalEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO outcome_signals (project_id, agent_id, entity, signal_type, count)
		 VALUES (?, ?, ?, ?, ?)`,
		ev.ProjectID, ev.AgentID, ev.Entity, ev.SignalType, ev.Count,
	)
	return err
}

// GetEffectiveness returns per-entity effectiveness scores using the shared
// pulsetypes.EntityEffectiveness type so callers don't need a type conversion.
// minSignals filters out entities with too few data points to be meaningful.
// Only entities with at least one negative signal are returned (low performers only).
func (s *Store) GetEffectiveness(projectID string, minSignals int) ([]pulsetypes.EntityEffectiveness, error) {
	if minSignals <= 0 {
		minSignals = 2
	}
	rows, err := s.db.Query(`
		SELECT entity,
		       COUNT(*) AS total,
		       SUM(CASE WHEN signal_type = 'task_done' THEN 1 ELSE 0 END) AS pos,
		       SUM(CASE WHEN signal_type IN ('correction','escalation','replan','task_cancelled') THEN 1 ELSE 0 END) AS neg
		FROM outcome_signals
		WHERE (? = '' OR project_id = ?)
		  AND entity != ''
		GROUP BY entity
		HAVING total >= ?
		ORDER BY neg DESC, total DESC
		LIMIT 20`,
		projectID, projectID, minSignals,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []pulsetypes.EntityEffectiveness
	for rows.Next() {
		var e pulsetypes.EntityEffectiveness
		if err := rows.Scan(&e.Entity, &e.Signals, &e.Positives, &e.Negatives); err != nil {
			return nil, err
		}
		total := e.Positives + e.Negatives
		if total > 0 {
			e.Score = float64(e.Positives) / float64(total)
		} else {
			e.Score = 1.0
		}
		if e.Negatives > 0 {
			switch {
			case e.Score < 0.3:
				e.Suggestion = fmt.Sprintf("Context for %q is frequently insufficient (score %.0f%%). Consider increasing BFS depth or detail_level.", e.Entity, e.Score*100)
			case e.Score < 0.6:
				e.Suggestion = fmt.Sprintf("Context for %q sometimes needs multiple fetches (score %.0f%%). Try get_context with detail_level=full on first call.", e.Entity, e.Score*100)
			}
		}
		results = append(results, e)
	}
	return results, rows.Err()
}

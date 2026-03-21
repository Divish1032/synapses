// Package store implements the SQLite persistence layer for synapses-pulse.
// All analytics data is stored locally in a single SQLite file.
package pulsestore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	db, err := sql.Open("sqlite", path+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"+
		"&_pragma=synchronous(NORMAL)&_pragma=cache_size(-65536)"+
		"&_pragma=mmap_size(268435456)&_pragma=temp_store(MEMORY)")
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

// BeginBatch starts a deferred transaction for batch writes. The returned
// function must be called to commit (ok=true) or rollback (ok=false) the
// transaction. It returns an error if COMMIT/ROLLBACK failed.
// This reduces fsync overhead from N fsyncs to 1 for N events.
func (s *Store) BeginBatch() (commit func(bool) error, err error) {
	s.mu.Lock()
	if _, err := s.db.Exec("BEGIN DEFERRED"); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	return func(ok bool) error {
		var err error
		if ok {
			_, err = s.db.Exec("COMMIT")
		} else {
			_, err = s.db.Exec("ROLLBACK")
		}
		s.mu.Unlock()
		return err
	}, nil
}

// InsertToolCallTx records a tool call event without acquiring the mutex.
// Caller must hold the mutex (via BeginBatch).
func (s *Store) InsertToolCallTx(ev pulsetypes.ToolCallEvent) error {
	successInt := 0
	if ev.Success {
		successInt = 1
	}
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(
		`INSERT INTO tool_calls (tool_name, agent_id, project_id, entity, duration_ms, success, response_bytes, created_date, session_id, error_message)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ToolName, ev.AgentID, ev.ProjectID, ev.Entity, ev.DurationMs, successInt, ev.ResponseBytes,
		today, ev.SessionID, ev.ErrorMessage,
	)
	return err
}

// InsertContextDeliveryTx records a context delivery without acquiring the mutex.
func (s *Store) InsertContextDeliveryTx(ev pulsetypes.ContextDeliveryEvent) error {
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
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(
		`INSERT INTO context_deliveries
		 (tool_name, agent_id, project_id, entity, file, response_bytes, response_tokens, baseline_tokens,
		  nodes_delivered, nodes_pruned, edges_delivered, truncated, duration_ms, cache_hit, brain_enriched,
		  created_date, session_id, intent)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ToolName, ev.AgentID, ev.ProjectID, ev.Entity, ev.File,
		ev.ResponseBytes, ev.ResponseTokens, ev.BaselineTokens,
		ev.NodesDelivered, ev.NodesPruned, ev.EdgesDelivered,
		truncInt, ev.DurationMs, cacheInt, brainInt,
		today, ev.SessionID, ev.Intent,
	)
	return err
}

// InsertBrainUsageTx records a brain usage event without acquiring the mutex.
func (s *Store) InsertBrainUsageTx(ev pulsetypes.BrainUsageEvent) error {
	successInt := 0
	if ev.Success {
		successInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO brain_usage
		 (model, tier, endpoint, prompt_tokens, completion_tokens, duration_ms, cost_usd, agent_id, project_id, target_entity, success)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Model, ev.Tier, ev.Endpoint, ev.PromptTokens, ev.CompletionTokens,
		ev.DurationMs, ev.CostUSD, ev.AgentID, ev.ProjectID,
		ev.TargetEntity, successInt,
	)
	return err
}

// InsertOutcomeSignalTx records an outcome signal without acquiring the mutex.
func (s *Store) InsertOutcomeSignalTx(ev pulsetypes.OutcomeSignalEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO outcome_signals (project_id, agent_id, entity, signal_type, count, session_id)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ev.ProjectID, ev.AgentID, ev.Entity, ev.SignalType, ev.Count, ev.SessionID,
	)
	return err
}

// InsertAgentLLMUsageTx records an agent LLM usage event without acquiring the mutex.
func (s *Store) InsertAgentLLMUsageTx(ev pulsetypes.AgentLLMUsageEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO agent_llm_usage
		 (session_id, agent_id, project_id, model, provider, input_tokens, output_tokens, cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.SessionID, ev.AgentID, ev.ProjectID, ev.Model, ev.Provider,
		ev.InputTokens, ev.OutputTokens, ev.CostUSD,
	)
	return err
}

// UpsertSessionTx creates or updates a session record without acquiring the mutex.
func (s *Store) UpsertSessionTx(id, agentID, projectID, event string) error {
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

// UpdateSessionStatsTx upserts session stats without acquiring the mutex.
func (s *Store) UpdateSessionStatsTx(sessionID, agentID, projectID string, tokensSaved int, costSaved float64) error {
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

// AddSessionTokensSavedTx increments token savings without acquiring the mutex.
func (s *Store) AddSessionTokensSavedTx(sessionID, agentID, projectID string, tokensSaved int, costSaved float64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, agent_id, project_id, started_at, tokens_saved, cost_saved_usd)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		 	tokens_saved   = tokens_saved + excluded.tokens_saved,
		 	cost_saved_usd = cost_saved_usd + excluded.cost_saved_usd`,
		sessionID, agentID, projectID, now, tokensSaved, costSaved,
	)
	return err
}

// UpdateSessionModelTx records model/provider without acquiring the mutex.
func (s *Store) UpdateSessionModelTx(sessionID, agentID, projectID, model, provider string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, agent_id, project_id, started_at, model, provider)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET model = excluded.model, provider = excluded.provider`,
		sessionID, agentID, projectID, now, model, provider,
	)
	return err
}

// InsertParseEventTx records a parse event without acquiring the mutex.
func (s *Store) InsertParseEventTx(ev pulsetypes.ParseEvent) error {
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(
		`INSERT INTO parse_events (file, language, duration_ms, nodes_produced, edges_produced, call_sites_produced, error_type, project_id, created_date)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.File, ev.Language, ev.DurationMs, ev.NodesProduced, ev.EdgesProduced,
		ev.CallSitesProduced, ev.ErrorType, ev.ProjectID, today,
	)
	return err
}

// InsertParseEvent records a parse event, acquiring the mutex.
func (s *Store) InsertParseEvent(ev pulsetypes.ParseEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertParseEventTx(ev)
}

// InsertReparseEventTx records a reparse event without acquiring the mutex.
func (s *Store) InsertReparseEventTx(ev pulsetypes.ReparseEvent) error {
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(
		`INSERT INTO reparse_events (file, language, duration_ms, nodes_before, nodes_after, edges_delta, memories_staled, project_id, created_date)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.File, ev.Language, ev.DurationMs, ev.NodesBefore, ev.NodesAfter,
		ev.EdgesDelta, ev.MemoriesStaled, ev.ProjectID, today,
	)
	return err
}

// InsertReparseEvent records a reparse event, acquiring the mutex.
func (s *Store) InsertReparseEvent(ev pulsetypes.ReparseEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertReparseEventTx(ev)
}

// InsertGraphSnapshotTx records a graph snapshot without acquiring the mutex.
func (s *Store) InsertGraphSnapshotTx(ev pulsetypes.GraphSnapshotEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO graph_snapshots
		 (snapshot_type, nodes_total, edges_total, edges_calls, density, orphan_nodes,
		  cross_file_edge_pct, max_fanin, max_fanout, fan_in_p50, fan_in_p95, fan_out_p50, fan_out_p95,
		  node_type_dist, project_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.SnapshotType, ev.NodesTotal, ev.EdgesTotal, ev.EdgesCalls, ev.Density,
		ev.OrphanNodes, ev.CrossFileEdgePct, ev.MaxFanin, ev.MaxFanout,
		ev.FanInP50, ev.FanInP95, ev.FanOutP50, ev.FanOutP95,
		ev.NodeTypeDistJSON, ev.ProjectID,
	)
	return err
}

// InsertGraphSnapshot records a graph snapshot, acquiring the mutex.
func (s *Store) InsertGraphSnapshot(ev pulsetypes.GraphSnapshotEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertGraphSnapshotTx(ev)
}

// InsertEmbeddingEventTx records an embedding batch event without acquiring the mutex.
func (s *Store) InsertEmbeddingEventTx(ev pulsetypes.EmbeddingEvent) error {
	successInt := 0
	if ev.Success {
		successInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO embedding_events (trigger, count, errors, duration_ms, model, model_status, success, stale_count, project_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Trigger, ev.Count, ev.Errors, ev.DurationMs, ev.Model, ev.ModelStatus,
		successInt, ev.StaleCount, ev.ProjectID,
	)
	return err
}

// InsertEmbeddingEvent records an embedding event, acquiring the mutex.
func (s *Store) InsertEmbeddingEvent(ev pulsetypes.EmbeddingEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertEmbeddingEventTx(ev)
}

// InsertIndexEventTx records a full-index event without acquiring the mutex.
func (s *Store) InsertIndexEventTx(ev pulsetypes.IndexEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO index_events (duration_ms, files_indexed, total_nodes, total_edges, call_sites_resolved, call_sites_unresolved, resolution_rate, language_dist, project_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.DurationMs, ev.FilesIndexed, ev.TotalNodes, ev.TotalEdges,
		ev.CallSitesResolved, ev.CallSitesUnresolved, ev.ResolutionRate,
		ev.LanguageDistJSON, ev.ProjectID,
	)
	return err
}

// InsertIndexEvent records a full-index event, acquiring the mutex.
func (s *Store) InsertIndexEvent(ev pulsetypes.IndexEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertIndexEventTx(ev)
}

// CountStaleEmbeddings returns a best-effort estimate of how many memories
// currently lack up-to-date embeddings. It reads from the pulse store's
// embedding_events table (not the main store's memory_embeddings table, which
// lives in a separate DB). The value is the MAX stale_count reported by any
// EmbeddingEvent in the last 7 days — a point-in-time snapshot, not an exact
// live count. Returns 0 on error (pulse is best-effort).
func (s *Store) CountStaleEmbeddings() int {
	var n int
	row := s.db.QueryRow(`SELECT COALESCE(MAX(stale_count), 0) FROM embedding_events WHERE created_at >= ?`,
		time.Now().UTC().AddDate(0, 0, -7).Format(time.RFC3339))
	_ = row.Scan(&n)
	return n
}

// migrate creates all tables and indexes if they don't exist, then runs
// column-level migrations for databases created before new columns were added.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	return s.migrateColumns()
}

// migrateColumns adds columns introduced after initial schema to existing databases.
// SQLite returns "duplicate column name" when a column already exists; we ignore that.
func (s *Store) migrateColumns() error {
	alterStmts := []string{
		`ALTER TABLE sessions ADD COLUMN model    TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
		// Pulse Phase 1: created_date for index-friendly date filtering.
		`ALTER TABLE tool_calls ADD COLUMN created_date TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE context_deliveries ADD COLUMN created_date TEXT NOT NULL DEFAULT ''`,
		// Pulse Phase 1: session_id correlation.
		`ALTER TABLE tool_calls ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE context_deliveries ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE outcome_signals ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		// Pulse Phase 1: additional dimensions.
		`ALTER TABLE tool_calls ADD COLUMN error_message TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE context_deliveries ADD COLUMN intent TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE brain_usage ADD COLUMN target_entity TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE brain_usage ADD COLUMN success INTEGER NOT NULL DEFAULT 1`,
	}
	for _, stmt := range alterStmts {
		if _, err := s.db.Exec(stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("migrate columns: %w", err)
			}
		}
	}
	// Backfill created_date from created_at for existing rows.
	for _, tbl := range []string{"tool_calls", "context_deliveries"} {
		if _, err := s.db.Exec(fmt.Sprintf(
			`UPDATE %s SET created_date = date(created_at) WHERE created_date = ''`, tbl)); err != nil {
			return fmt.Errorf("backfill created_date on %s: %w", tbl, err)
		}
	}
	// Create indexes that may not exist on older databases.
	indexStmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_tc_cdate ON tool_calls(created_date)`,
		`CREATE INDEX IF NOT EXISTS idx_cd_cdate ON context_deliveries(created_date)`,
		`CREATE INDEX IF NOT EXISTS idx_tc_session ON tool_calls(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_cd_session ON context_deliveries(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_os_session ON outcome_signals(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tc_project ON tool_calls(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_cd_project ON context_deliveries(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_cd_entity_ts ON context_deliveries(entity, created_at)`,
	}
	for _, stmt := range indexStmts {
		if _, err := s.db.Exec(stmt); err != nil {
			// IF NOT EXISTS should prevent duplicates, but tolerate edge cases.
			if !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("migrate indexes: %w", err)
			}
		}
	}
	// Upsert new model pricing entries that may not exist.
	newPricing := []struct{ model string; input, output float64 }{
		{"gpt-4.1", 2.00, 8.00},
		{"gpt-4.1-mini", 0.40, 1.60},
		{"gpt-4.1-nano", 0.10, 0.40},
		{"o3", 10.00, 40.00},
		{"o3-mini", 1.10, 4.40},
		{"o4-mini", 1.10, 4.40},
		{"claude-haiku-4-5", 0.80, 4.00},
		{"gemini-2.5-pro", 1.25, 10.00},
		{"gemini-2.5-flash", 0.15, 0.60},
	}
	for _, p := range newPricing {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO pricing (model, input_per_1m, output_per_1m, source) VALUES (?, ?, ?, 'default')`,
			p.model, p.input, p.output); err != nil {
			return fmt.Errorf("insert pricing for %s: %w", p.model, err)
		}
	}
	return nil
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
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    created_date     TEXT    NOT NULL DEFAULT '',
    session_id       TEXT    NOT NULL DEFAULT '',
    intent           TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_cd_tool    ON context_deliveries(tool_name);
CREATE INDEX IF NOT EXISTS idx_cd_agent   ON context_deliveries(agent_id);
CREATE INDEX IF NOT EXISTS idx_cd_created ON context_deliveries(created_at);
CREATE INDEX IF NOT EXISTS idx_cd_cdate   ON context_deliveries(created_date);
CREATE INDEX IF NOT EXISTS idx_cd_session ON context_deliveries(session_id);
CREATE INDEX IF NOT EXISTS idx_cd_project ON context_deliveries(project_id);
CREATE INDEX IF NOT EXISTS idx_cd_entity_ts ON context_deliveries(entity, created_at);

CREATE TABLE IF NOT EXISTS tool_calls (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    tool_name        TEXT    NOT NULL,
    agent_id         TEXT    NOT NULL DEFAULT '',
    project_id       TEXT    NOT NULL DEFAULT '',
    entity           TEXT    NOT NULL DEFAULT '',
    duration_ms      INTEGER NOT NULL DEFAULT 0,
    success          INTEGER NOT NULL DEFAULT 1,
    response_bytes   INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    created_date     TEXT    NOT NULL DEFAULT '',
    session_id       TEXT    NOT NULL DEFAULT '',
    error_message    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tc_tool    ON tool_calls(tool_name);
CREATE INDEX IF NOT EXISTS idx_tc_ts      ON tool_calls(created_at);
CREATE INDEX IF NOT EXISTS idx_tc_agent   ON tool_calls(agent_id);
CREATE INDEX IF NOT EXISTS idx_tc_cdate   ON tool_calls(created_date);
CREATE INDEX IF NOT EXISTS idx_tc_session ON tool_calls(session_id);
CREATE INDEX IF NOT EXISTS idx_tc_project ON tool_calls(project_id);

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
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    target_entity     TEXT    NOT NULL DEFAULT '',
    success           INTEGER NOT NULL DEFAULT 1
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
    tasks_completed  INTEGER NOT NULL DEFAULT 0,
    model            TEXT    NOT NULL DEFAULT '',
    provider         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sess_agent ON sessions(agent_id);
CREATE INDEX IF NOT EXISTS idx_sess_start ON sessions(started_at);

CREATE TABLE IF NOT EXISTS agent_llm_usage (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT    NOT NULL DEFAULT '',
    agent_id      TEXT    NOT NULL DEFAULT '',
    project_id    TEXT    NOT NULL DEFAULT '',
    model         TEXT    NOT NULL,
    provider      TEXT    NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd      REAL    NOT NULL DEFAULT 0.0,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_alu_model   ON agent_llm_usage(model);
CREATE INDEX IF NOT EXISTS idx_alu_agent   ON agent_llm_usage(agent_id);
CREATE INDEX IF NOT EXISTS idx_alu_created ON agent_llm_usage(created_at);

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

-- Default pricing for common models used as the baseline cost reference.
-- "What would the agent have paid to send the un-compressed baseline tokens?"
-- Prices in USD per 1M tokens (input). Updated via UpsertPricing if stale.
INSERT OR IGNORE INTO pricing (model, input_per_1m, output_per_1m, source) VALUES
    ('gpt-4o',               2.50, 10.00, 'default'),
    ('gpt-4o-mini',          0.15,  0.60, 'default'),
    ('gpt-4.1',              2.00, 8.00,  'default'),
    ('gpt-4.1-mini',         0.40, 1.60,  'default'),
    ('gpt-4.1-nano',         0.10, 0.40,  'default'),
    ('o3',                  10.00, 40.00,  'default'),
    ('o3-mini',              1.10,  4.40,  'default'),
    ('o4-mini',              1.10,  4.40,  'default'),
    ('claude-3-5-sonnet',    3.00, 15.00, 'default'),
    ('claude-3-5-haiku',     0.80,  4.00, 'default'),
    ('claude-sonnet-4-6',    3.00, 15.00, 'default'),
    ('claude-opus-4-6',     15.00, 75.00, 'default'),
    ('claude-haiku-4-5',     0.80,  4.00, 'default'),
    ('gemini-2.5-pro',       1.25,  10.00, 'default'),
    ('gemini-2.5-flash',     0.15,   0.60, 'default');

-- R29: Intent Alignment Metrics — passive outcome signals.
-- Correlates context delivery quality with downstream agent actions.
CREATE TABLE IF NOT EXISTS outcome_signals (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  TEXT    NOT NULL DEFAULT '',
    agent_id    TEXT    NOT NULL DEFAULT '',
    entity      TEXT    NOT NULL DEFAULT '',
    signal_type TEXT    NOT NULL,
    count       INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    session_id  TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_os_entity    ON outcome_signals(entity);
CREATE INDEX IF NOT EXISTS idx_os_project   ON outcome_signals(project_id);
CREATE INDEX IF NOT EXISTS idx_os_signal    ON outcome_signals(signal_type);
CREATE INDEX IF NOT EXISTS idx_os_created   ON outcome_signals(created_at);
CREATE INDEX IF NOT EXISTS idx_os_session   ON outcome_signals(session_id);

-- Phase 2: Pipeline instrumentation tables.

CREATE TABLE IF NOT EXISTS parse_events (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    file                TEXT    NOT NULL DEFAULT '',
    language            TEXT    NOT NULL DEFAULT '',
    duration_ms         INTEGER NOT NULL DEFAULT 0,
    nodes_produced      INTEGER NOT NULL DEFAULT 0,
    edges_produced      INTEGER NOT NULL DEFAULT 0,
    call_sites_produced INTEGER NOT NULL DEFAULT 0,
    error_type          TEXT    NOT NULL DEFAULT '',
    project_id          TEXT    NOT NULL DEFAULT '',
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    created_date        TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_pe_created ON parse_events(created_at);
CREATE INDEX IF NOT EXISTS idx_pe_cdate   ON parse_events(created_date);
CREATE INDEX IF NOT EXISTS idx_pe_project ON parse_events(project_id);

CREATE TABLE IF NOT EXISTS reparse_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    file            TEXT    NOT NULL DEFAULT '',
    language        TEXT    NOT NULL DEFAULT '',
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    nodes_before    INTEGER NOT NULL DEFAULT 0,
    nodes_after     INTEGER NOT NULL DEFAULT 0,
    edges_delta     INTEGER NOT NULL DEFAULT 0,
    memories_staled INTEGER NOT NULL DEFAULT 0,
    project_id      TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    created_date    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_re_created ON reparse_events(created_at);
CREATE INDEX IF NOT EXISTS idx_re_cdate   ON reparse_events(created_date);
CREATE INDEX IF NOT EXISTS idx_re_project ON reparse_events(project_id);

CREATE TABLE IF NOT EXISTS graph_snapshots (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    snapshot_type        TEXT    NOT NULL DEFAULT 'full',
    nodes_total          INTEGER NOT NULL DEFAULT 0,
    edges_total          INTEGER NOT NULL DEFAULT 0,
    edges_calls          INTEGER NOT NULL DEFAULT 0,
    density              REAL    NOT NULL DEFAULT 0.0,
    orphan_nodes         INTEGER NOT NULL DEFAULT 0,
    cross_file_edge_pct  REAL    NOT NULL DEFAULT 0.0,
    max_fanin            INTEGER NOT NULL DEFAULT 0,
    max_fanout           INTEGER NOT NULL DEFAULT 0,
    fan_in_p50           INTEGER NOT NULL DEFAULT 0,
    fan_in_p95           INTEGER NOT NULL DEFAULT 0,
    fan_out_p50          INTEGER NOT NULL DEFAULT 0,
    fan_out_p95          INTEGER NOT NULL DEFAULT 0,
    node_type_dist       TEXT    NOT NULL DEFAULT '{}',
    project_id           TEXT    NOT NULL DEFAULT '',
    created_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_gs_created ON graph_snapshots(created_at);
CREATE INDEX IF NOT EXISTS idx_gs_project ON graph_snapshots(project_id);

CREATE TABLE IF NOT EXISTS embedding_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    trigger      TEXT    NOT NULL DEFAULT '',
    count        INTEGER NOT NULL DEFAULT 0,
    errors       INTEGER NOT NULL DEFAULT 0,
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    model        TEXT    NOT NULL DEFAULT '',
    model_status TEXT    NOT NULL DEFAULT '',
    success      INTEGER NOT NULL DEFAULT 1,
    stale_count  INTEGER NOT NULL DEFAULT 0,
    project_id   TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_ee_created ON embedding_events(created_at);
CREATE INDEX IF NOT EXISTS idx_ee_project ON embedding_events(project_id);

CREATE TABLE IF NOT EXISTS index_events (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    duration_ms          INTEGER NOT NULL DEFAULT 0,
    files_indexed        INTEGER NOT NULL DEFAULT 0,
    total_nodes          INTEGER NOT NULL DEFAULT 0,
    total_edges          INTEGER NOT NULL DEFAULT 0,
    call_sites_resolved  INTEGER NOT NULL DEFAULT 0,
    call_sites_unresolved INTEGER NOT NULL DEFAULT 0,
    resolution_rate      REAL    NOT NULL DEFAULT 0.0,
    language_dist        TEXT    NOT NULL DEFAULT '{}',
    project_id           TEXT    NOT NULL DEFAULT '',
    created_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_ie_created ON index_events(created_at);
CREATE INDEX IF NOT EXISTS idx_ie_project ON index_events(project_id);
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
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(
		`INSERT INTO tool_calls (tool_name, agent_id, project_id, entity, duration_ms, success, response_bytes, created_date, session_id, error_message)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ToolName, ev.AgentID, ev.ProjectID, ev.Entity, ev.DurationMs, successInt, ev.ResponseBytes,
		today, ev.SessionID, ev.ErrorMessage,
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
	today := time.Now().UTC().Format("2006-01-02")

	_, err := s.db.Exec(
		`INSERT INTO context_deliveries
		 (tool_name, agent_id, project_id, entity, file, response_bytes, response_tokens, baseline_tokens,
		  nodes_delivered, nodes_pruned, edges_delivered, truncated, duration_ms, cache_hit, brain_enriched,
		  created_date, session_id, intent)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ToolName, ev.AgentID, ev.ProjectID, ev.Entity, ev.File,
		ev.ResponseBytes, ev.ResponseTokens, ev.BaselineTokens,
		ev.NodesDelivered, ev.NodesPruned, ev.EdgesDelivered,
		truncInt, ev.DurationMs, cacheInt, brainInt,
		today, ev.SessionID, ev.Intent,
	)
	return err
}

// InsertBrainUsage records a brain sidecar LLM inference event.
func (s *Store) InsertBrainUsage(ev pulsetypes.BrainUsageEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	successInt := 0
	if ev.Success {
		successInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO brain_usage
		 (model, tier, endpoint, prompt_tokens, completion_tokens, duration_ms, cost_usd, agent_id, project_id, target_entity, success)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Model, ev.Tier, ev.Endpoint, ev.PromptTokens, ev.CompletionTokens,
		ev.DurationMs, ev.CostUSD, ev.AgentID, ev.ProjectID,
		ev.TargetEntity, successInt,
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

// AddSessionTokensSaved increments only the token savings and cost fields for
// a session row. Unlike UpdateSessionStats it does NOT increment tool_calls —
// this is used by the context-delivery path so tool_calls counts real tool
// invocations only (recorded by UpdateSessionStats in the tool_call path).
func (s *Store) AddSessionTokensSaved(sessionID, agentID, projectID string, tokensSaved int, costSaved float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, agent_id, project_id, started_at, tokens_saved, cost_saved_usd)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
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
	// Summable components — stored in rollups so rates can be recomputed correctly
	// across multi-day queries instead of naively summing daily averages.
	CacheHits          int     `json:"cache_hits"`
	BrainEnrichedCount int     `json:"brain_enriched_count"`
	TotalLatencyMs     float64 `json:"total_latency_ms"`
	// ValueMultiplier shows how many tokens the agent would have consumed without
	// Synapses for every 1 token actually delivered ("Nx multiplier").
	ValueMultiplier    float64 `json:"value_multiplier"`
}

// GetSummary returns aggregated analytics for the last N days.
// Fast path: sums pre-computed daily_rollups for past days, then adds today's
// raw data. Falls back to a full raw-table scan if rollups are not yet available
// (e.g. new installation or rollup gap).
func (s *Store) GetSummary(days int) (*Summary, error) {
	today := time.Now().UTC().Format("2006-01-02")
	rollupSince := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	// ── Fast path: rollups for past days + raw for today ──────────────────
	hist, rollupErr := s.sumRollups(rollupSince, today)
	if rollupErr == nil && hist != nil {
		raw, err := s.GetSummaryForDay(today)
		if err != nil {
			return nil, err
		}
		return mergeSummaries(hist, raw), nil
	}

	// ── Slow path: full raw-table scan (rollups absent or incomplete) ──────
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	sum := &Summary{}

	row := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(duration_ms), 0)
		 FROM tool_calls WHERE created_at >= ?`, since)
	if err := row.Scan(&sum.TotalToolCalls, &sum.TotalLatencyMs); err != nil {
		return nil, err
	}
	if sum.TotalToolCalls > 0 {
		sum.AvgLatencyMs = sum.TotalLatencyMs / float64(sum.TotalToolCalls)
	}

	row = s.db.QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(response_tokens), 0),
		        COALESCE(SUM(baseline_tokens), 0),
		        COALESCE(SUM(CASE WHEN cache_hit = 1 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN brain_enriched = 1 THEN 1 ELSE 0 END), 0)
		 FROM context_deliveries WHERE created_at >= ?`, since)
	if err := row.Scan(&sum.ContextDeliveries, &sum.TokensDelivered, &sum.BaselineTokens,
		&sum.CacheHits, &sum.BrainEnrichedCount); err != nil {
		return nil, err
	}

	if sum.ContextDeliveries > 0 {
		sum.CacheHitRate = float64(sum.CacheHits) / float64(sum.ContextDeliveries)
		sum.BrainEnrichRate = float64(sum.BrainEnrichedCount) / float64(sum.ContextDeliveries)
	}

	// Compute tokens_saved from context_deliveries (ground truth) instead of sessions table
	sum.TokensSaved = sum.BaselineTokens - sum.TokensDelivered
	if sum.TokensSaved < 0 {
		sum.TokensSaved = 0
	}

	if sum.TokensDelivered > 0 {
		sum.CompressionRatio = float64(sum.BaselineTokens) / float64(sum.TokensDelivered)
	} else {
		sum.CompressionRatio = 1.0
	}

	row = s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(tasks_completed), 0), COALESCE(SUM(cost_saved_usd), 0)
		 FROM sessions WHERE started_at >= ?`, since)
	if err := row.Scan(&sum.Sessions, &sum.TasksCompleted, &sum.CostSavedUSD); err != nil {
		return nil, err
	}

	if sum.BaselineTokens > 0 && sum.TokensSaved > 0 {
		sum.SavingsPct = float64(sum.TokensSaved) / float64(sum.BaselineTokens) * 100.0
	}
	computeValueMultiplier(sum)

	return sum, nil
}

// sumRollups aggregates daily_rollups rows in [since, before) into a Summary.
// Returns (nil, nil) when no rollup rows exist for the period (triggers fallback).
// Rate metrics (cache_hit_rate, brain_enrichment_rate, avg_latency_ms) are recomputed
// from summable components (cache_hits, brain_enriched_count, total_latency_ms)
// instead of naively summing daily averages (which produces nonsensical values).
func (s *Store) sumRollups(since, before string) (*Summary, error) {
	rows, err := s.db.Query(
		`SELECT metric, COALESCE(SUM(value), 0)
		 FROM daily_rollups WHERE day >= ? AND day < ?
		 GROUP BY metric`, since, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string]float64)
	for rows.Next() {
		var metric string
		var value float64
		if err := rows.Scan(&metric, &value); err != nil {
			return nil, err
		}
		m[metric] = value
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if len(m) == 0 {
		return nil, nil // no rollup data — caller should use slow path
	}

	sum := &Summary{
		TotalToolCalls:     int(m["tool_calls"]),
		TokensDelivered:    int(m["tokens_delivered"]),
		BaselineTokens:     int(m["baseline_tokens"]),
		TokensSaved:        int(m["tokens_saved"]),
		ContextDeliveries:  int(m["context_deliveries"]),
		CostSavedUSD:       m["cost_saved_usd"],
		Sessions:           int(m["sessions"]),
		TasksCompleted:     int(m["tasks_completed"]),
		CacheHits:          int(m["cache_hits"]),
		BrainEnrichedCount: int(m["brain_enriched_count"]),
		TotalLatencyMs:     m["total_latency_ms"],
	}

	// Recompute rate metrics from summable components.
	if sum.ContextDeliveries > 0 {
		sum.CacheHitRate = float64(sum.CacheHits) / float64(sum.ContextDeliveries)
		sum.BrainEnrichRate = float64(sum.BrainEnrichedCount) / float64(sum.ContextDeliveries)
	}
	if sum.TotalToolCalls > 0 {
		sum.AvgLatencyMs = sum.TotalLatencyMs / float64(sum.TotalToolCalls)
	}
	if sum.TokensDelivered > 0 {
		sum.CompressionRatio = float64(sum.BaselineTokens) / float64(sum.TokensDelivered)
	} else {
		sum.CompressionRatio = 1.0
	}
	if sum.BaselineTokens > 0 && sum.TokensSaved > 0 {
		sum.SavingsPct = float64(sum.TokensSaved) / float64(sum.BaselineTokens) * 100.0
	}

	return sum, nil
}

// mergeSummaries combines historical (rollup-based) and today's (raw) summaries.
// Additive fields are summed; rate fields are recomputed from combined summable
// components (cache_hits, brain_enriched_count, total_latency_ms).
func mergeSummaries(hist, today *Summary) *Summary {
	out := &Summary{
		TotalToolCalls:     hist.TotalToolCalls + today.TotalToolCalls,
		TokensDelivered:    hist.TokensDelivered + today.TokensDelivered,
		BaselineTokens:     hist.BaselineTokens + today.BaselineTokens,
		ContextDeliveries:  hist.ContextDeliveries + today.ContextDeliveries,
		CostSavedUSD:       hist.CostSavedUSD + today.CostSavedUSD,
		Sessions:           hist.Sessions + today.Sessions,
		TasksCompleted:     hist.TasksCompleted + today.TasksCompleted,
		CacheHits:          hist.CacheHits + today.CacheHits,
		BrainEnrichedCount: hist.BrainEnrichedCount + today.BrainEnrichedCount,
		TotalLatencyMs:     hist.TotalLatencyMs + today.TotalLatencyMs,
	}

	// Compute tokens_saved from baseline - delivered (ground truth)
	out.TokensSaved = out.BaselineTokens - out.TokensDelivered
	if out.TokensSaved < 0 {
		out.TokensSaved = 0
	}

	if out.BaselineTokens > 0 && out.TokensSaved > 0 {
		out.SavingsPct = float64(out.TokensSaved) / float64(out.BaselineTokens) * 100.0
	}
	if out.TokensDelivered > 0 {
		out.CompressionRatio = float64(out.BaselineTokens) / float64(out.TokensDelivered)
	} else {
		out.CompressionRatio = 1.0
	}

	// Recompute rate fields from summable components.
	if out.ContextDeliveries > 0 {
		out.CacheHitRate = float64(out.CacheHits) / float64(out.ContextDeliveries)
		out.BrainEnrichRate = float64(out.BrainEnrichedCount) / float64(out.ContextDeliveries)
	}
	if out.TotalToolCalls > 0 {
		out.AvgLatencyMs = out.TotalLatencyMs / float64(out.TotalToolCalls)
	}

	// Value multiplier: how many tokens the agent would consume without Synapses.
	computeValueMultiplier(out)

	return out
}

// computeValueMultiplier sets the ValueMultiplier field on a Summary.
// ValueMultiplier = baseline_tokens / tokens_delivered — the Nx savings factor.
func computeValueMultiplier(s *Summary) {
	if s.TokensDelivered > 0 {
		s.ValueMultiplier = float64(s.BaselineTokens) / float64(s.TokensDelivered)
	} else {
		s.ValueMultiplier = 1.0
	}
}

// GetSummaryForDay returns aggregated metrics for a specific calendar day
// (format "2006-01-02"). Used by the aggregator to pre-compute daily rollups
// with exact calendar-day boundaries instead of a rolling 24-hour window.
// Uses the created_date column (index-friendly) instead of date() function.
func (s *Store) GetSummaryForDay(day string) (*Summary, error) {
	sum := &Summary{}

	row := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(duration_ms), 0)
		 FROM tool_calls WHERE created_date = ?`, day)
	var totalLatency int64
	if err := row.Scan(&sum.TotalToolCalls, &totalLatency); err != nil {
		return nil, err
	}
	sum.TotalLatencyMs = float64(totalLatency)

	row = s.db.QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(response_tokens), 0),
		        COALESCE(SUM(baseline_tokens), 0),
		        COALESCE(SUM(CASE WHEN cache_hit = 1 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN brain_enriched = 1 THEN 1 ELSE 0 END), 0)
		 FROM context_deliveries WHERE created_date = ?`, day)
	if err := row.Scan(&sum.ContextDeliveries, &sum.TokensDelivered, &sum.BaselineTokens,
		&sum.CacheHits, &sum.BrainEnrichedCount); err != nil {
		return nil, err
	}

	// Compute rate fields from summable components.
	if sum.ContextDeliveries > 0 {
		sum.CacheHitRate = float64(sum.CacheHits) / float64(sum.ContextDeliveries)
		sum.BrainEnrichRate = float64(sum.BrainEnrichedCount) / float64(sum.ContextDeliveries)
	}
	if sum.TotalToolCalls > 0 {
		sum.AvgLatencyMs = sum.TotalLatencyMs / float64(sum.TotalToolCalls)
	}

	// Compute tokens_saved from context_deliveries (ground truth) instead of sessions table
	sum.TokensSaved = sum.BaselineTokens - sum.TokensDelivered
	if sum.TokensSaved < 0 {
		sum.TokensSaved = 0
	}

	if sum.TokensDelivered > 0 {
		sum.CompressionRatio = float64(sum.BaselineTokens) / float64(sum.TokensDelivered)
	} else {
		sum.CompressionRatio = 1.0
	}

	row = s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(tasks_completed), 0), COALESCE(SUM(cost_saved_usd), 0)
		 FROM sessions WHERE date(started_at) = ?`, day)
	if err := row.Scan(&sum.Sessions, &sum.TasksCompleted, &sum.CostSavedUSD); err != nil {
		return nil, err
	}

	if sum.BaselineTokens > 0 && sum.TokensSaved > 0 {
		sum.SavingsPct = float64(sum.TokensSaved) / float64(sum.BaselineTokens) * 100.0
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
// ToolCalls counts actual tool invocations; TokensSaved comes from context_deliveries;
// CostSavedUSD comes from sessions. Three subqueries are unioned and grouped by day.
// Uses created_date column (index-friendly) instead of date() function calls.
func (s *Store) GetTimeline(days int) ([]TimelinePoint, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	rows, err := s.db.Query(
		`SELECT day,
		        COALESCE(SUM(tokens_saved), 0),
		        COALESCE(SUM(tool_calls), 0),
		        COALESCE(SUM(cost_saved_usd), 0)
		 FROM (
		   SELECT created_date AS day,
		          COALESCE(SUM(baseline_tokens) - SUM(response_tokens), 0) AS tokens_saved,
		          0 AS tool_calls,
		          0.0 AS cost_saved_usd
		   FROM context_deliveries
		   WHERE created_date >= ?
		   GROUP BY created_date
		   UNION ALL
		   SELECT created_date AS day,
		          0 AS tokens_saved,
		          COUNT(*) AS tool_calls,
		          0.0 AS cost_saved_usd
		   FROM tool_calls
		   WHERE created_date >= ?
		   GROUP BY created_date
		   UNION ALL
		   SELECT date(started_at) AS day,
		          0 AS tokens_saved,
		          0 AS tool_calls,
		          COALESCE(SUM(cost_saved_usd), 0) AS cost_saved_usd
		   FROM sessions
		   WHERE date(started_at) >= ?
		   GROUP BY day
		 )
		 GROUP BY day ORDER BY day`, since, since, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var points []TimelinePoint
	for rows.Next() {
		var p TimelinePoint
		if err := rows.Scan(&p.Date, &p.TokensSaved, &p.ToolCalls, &p.CostSavedUSD); err != nil {
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
// Covers all event tables including outcome_signals and agent_llm_usage.
func (s *Store) PruneOldEvents(retentionDays int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	since := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339)
	var totalDeleted int64

	for _, table := range []string{
		"tool_calls", "context_deliveries", "brain_usage", "outcome_signals", "agent_llm_usage",
		"parse_events", "reparse_events", "graph_snapshots", "embedding_events", "index_events",
	} {
		result, err := s.db.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE created_at < ?", table), since)
		if err != nil {
			return totalDeleted, err
		}
		if n, err := result.RowsAffected(); err == nil {
			totalDeleted += n
		}
	}

	// Prune sessions older than retention
	result, err := s.db.Exec(`DELETE FROM sessions WHERE started_at < ?`, since)
	if err != nil {
		return totalDeleted, err
	}
	if n, err := result.RowsAffected(); err == nil {
		totalDeleted += n
	}

	// Prune old daily_rollups beyond retention
	rollupSince := time.Now().UTC().AddDate(0, 0, -retentionDays).Format("2006-01-02")
	result, err = s.db.Exec(`DELETE FROM daily_rollups WHERE day < ?`, rollupSince)
	if err != nil {
		return totalDeleted, err
	}
	if n, err := result.RowsAffected(); err == nil {
		totalDeleted += n
	}

	return totalDeleted, nil
}

// Vacuum reclaims space freed by DELETE operations. Must NOT be called inside a
// transaction. Safe to call at most once per day — callers are responsible for
// rate-limiting via the lastVacuum timestamp.
func (s *Store) Vacuum() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("VACUUM")
	return err
}

// GetRollupGapDays returns a list of calendar days (YYYY-MM-DD) between
// lastRollupDay (exclusive) and today (exclusive) for which no rollup exists.
// Used by the aggregator to backfill missed days.
func (s *Store) GetRollupGapDays(lastRollupDay, today string) ([]string, error) {
	// Collect days that already have rollup entries.
	rows, err := s.db.Query(
		`SELECT DISTINCT day FROM daily_rollups WHERE day > ? AND day < ? ORDER BY day`,
		lastRollupDay, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	existing := make(map[string]bool)
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		existing[d] = true
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	// Walk from the day after lastRollupDay to the day before today.
	var gaps []string
	cur, err := time.Parse("2006-01-02", lastRollupDay)
	if err != nil {
		return nil, err
	}
	end, err := time.Parse("2006-01-02", today)
	if err != nil {
		return nil, err
	}
	for cur = cur.AddDate(0, 0, 1); cur.Before(end); cur = cur.AddDate(0, 0, 1) {
		d := cur.Format("2006-01-02")
		if !existing[d] {
			gaps = append(gaps, d)
		}
	}
	return gaps, nil
}

// GetLastRollupDay returns the most recent day that has rollup entries,
// or an empty string if no rollup data exists.
func (s *Store) GetLastRollupDay() (string, error) {
	var day string
	row := s.db.QueryRow(`SELECT COALESCE(MAX(day), '') FROM daily_rollups`)
	if err := row.Scan(&day); err != nil {
		return "", err
	}
	return day, nil
}

// CountReparses returns the total count and sum of duration_ms for reparse_events
// on the given calendar day (YYYY-MM-DD).
func (s *Store) CountReparses(day string) (count int, totalDurationMs float64, err error) {
	row := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(duration_ms), 0) FROM reparse_events WHERE created_date = ?`, day)
	err = row.Scan(&count, &totalDurationMs)
	return
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

// EntityCount holds an entity name and the number of times it was queried.
type EntityCount struct {
	Entity string `json:"entity"`
	Count  int    `json:"count"`
}

// TopEntities returns the most frequently queried entities in context deliveries,
// each with a query count so the UI can display relative frequency.
func (s *Store) TopEntities(days, limit int) ([]EntityCount, error) {
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

	var entities []EntityCount
	for rows.Next() {
		var ec EntityCount
		if err := rows.Scan(&ec.Entity, &ec.Count); err != nil {
			return nil, err
		}
		entities = append(entities, ec)
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
		`INSERT INTO outcome_signals (project_id, agent_id, entity, signal_type, count, session_id)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ev.ProjectID, ev.AgentID, ev.Entity, ev.SignalType, ev.Count, ev.SessionID,
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

// ---------------------------------------------------------------------------
// Agent LLM usage tracking (Option A + B: model reported via session_init or report_usage)
// ---------------------------------------------------------------------------

// UpdateSessionModel records which model the agent is using for this session.
// Uses an upsert so it is safe to call before or after UpsertSession("start").
func (s *Store) UpdateSessionModel(sessionID, agentID, projectID, model, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, agent_id, project_id, started_at, model, provider)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET model = excluded.model, provider = excluded.provider`,
		sessionID, agentID, projectID, now, model, provider,
	)
	return err
}

// InsertAgentLLMUsage records one reported LLM call from an AI agent.
func (s *Store) InsertAgentLLMUsage(ev pulsetypes.AgentLLMUsageEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO agent_llm_usage
		 (session_id, agent_id, project_id, model, provider, input_tokens, output_tokens, cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.SessionID, ev.AgentID, ev.ProjectID, ev.Model, ev.Provider,
		ev.InputTokens, ev.OutputTokens, ev.CostUSD,
	)
	return err
}

// AgentLLMStats holds per-model aggregated usage reported by agents.
type AgentLLMStats struct {
	Model        string  `json:"model"`
	Provider     string  `json:"provider"`
	Calls        int     `json:"calls"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// GetAgentLLMStats returns per-model aggregated LLM usage for the last N days.
func (s *Store) GetAgentLLMStats(days int) ([]AgentLLMStats, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT model, provider,
		        COUNT(*),
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cost_usd), 0)
		 FROM agent_llm_usage
		 WHERE created_at >= ?
		 GROUP BY model, provider
		 ORDER BY SUM(cost_usd) DESC, COUNT(*) DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []AgentLLMStats
	for rows.Next() {
		var st AgentLLMStats
		if err := rows.Scan(&st.Model, &st.Provider, &st.Calls,
			&st.InputTokens, &st.OutputTokens, &st.TotalCostUSD); err != nil {
			return nil, err
		}
		stats = append(stats, st)
	}
	return stats, rows.Err()
}

// ---------------------------------------------------------------------------
// Lifetime and value metrics
// ---------------------------------------------------------------------------

// GetLifetimeSummary returns aggregated analytics across all time (no date filter).
// This powers the "hero metrics" in the dashboard.
func (s *Store) GetLifetimeSummary() (*Summary, error) {
	sum := &Summary{}

	row := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(duration_ms), 0)
		 FROM tool_calls`)
	if err := row.Scan(&sum.TotalToolCalls, &sum.TotalLatencyMs); err != nil {
		return nil, err
	}
	if sum.TotalToolCalls > 0 {
		sum.AvgLatencyMs = sum.TotalLatencyMs / float64(sum.TotalToolCalls)
	}

	row = s.db.QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(response_tokens), 0),
		        COALESCE(SUM(baseline_tokens), 0),
		        COALESCE(SUM(CASE WHEN cache_hit = 1 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN brain_enriched = 1 THEN 1 ELSE 0 END), 0)
		 FROM context_deliveries`)
	if err := row.Scan(&sum.ContextDeliveries, &sum.TokensDelivered, &sum.BaselineTokens,
		&sum.CacheHits, &sum.BrainEnrichedCount); err != nil {
		return nil, err
	}

	if sum.ContextDeliveries > 0 {
		sum.CacheHitRate = float64(sum.CacheHits) / float64(sum.ContextDeliveries)
		sum.BrainEnrichRate = float64(sum.BrainEnrichedCount) / float64(sum.ContextDeliveries)
	}

	sum.TokensSaved = sum.BaselineTokens - sum.TokensDelivered
	if sum.TokensSaved < 0 {
		sum.TokensSaved = 0
	}

	if sum.TokensDelivered > 0 {
		sum.CompressionRatio = float64(sum.BaselineTokens) / float64(sum.TokensDelivered)
	} else {
		sum.CompressionRatio = 1.0
	}

	row = s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(tasks_completed), 0), COALESCE(SUM(cost_saved_usd), 0)
		 FROM sessions`)
	if err := row.Scan(&sum.Sessions, &sum.TasksCompleted, &sum.CostSavedUSD); err != nil {
		return nil, err
	}

	if sum.BaselineTokens > 0 && sum.TokensSaved > 0 {
		sum.SavingsPct = float64(sum.TokensSaved) / float64(sum.BaselineTokens) * 100.0
	}
	computeValueMultiplier(sum)

	return sum, nil
}

// GetFirstContextRightRate returns the fraction of (entity, session) pairs
// where context was delivered and NO correction signal followed in the same
// session. This measures whether the first context delivery was sufficient.
// Rate = 1 - (corrected_pairs / total_pairs).
// Returns 1.0 if there are no deliveries (no data = assume perfect).
func (s *Store) GetFirstContextRightRate(days int) (float64, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)

	// Count distinct (entity, session_id) pairs delivered.
	var totalPairs int
	row := s.db.QueryRow(
		`SELECT COUNT(*) FROM (
		   SELECT DISTINCT entity, session_id FROM context_deliveries
		   WHERE created_at >= ? AND entity != '' AND session_id != ''
		 )`, since)
	if err := row.Scan(&totalPairs); err != nil {
		return 1.0, err
	}
	if totalPairs == 0 {
		return 1.0, nil
	}

	// Count (entity, session_id) pairs that received a correction in the same session.
	var correctedPairs int
	row = s.db.QueryRow(
		`SELECT COUNT(*) FROM (
		   SELECT DISTINCT cd.entity, cd.session_id
		   FROM context_deliveries cd
		   INNER JOIN outcome_signals os
		     ON os.entity = cd.entity AND os.session_id = cd.session_id
		   WHERE cd.created_at >= ? AND cd.entity != '' AND cd.session_id != ''
		     AND os.signal_type = 'correction'
		 )`, since)
	if err := row.Scan(&correctedPairs); err != nil {
		return 1.0, err
	}

	rate := 1.0 - float64(correctedPairs)/float64(totalPairs)
	if rate < 0 {
		rate = 0
	}
	return rate, nil
}

// Package store implements the SQLite persistence layer for synapses-pulse.
// All analytics data is stored locally in a single SQLite file.
package pulsestore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
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
		`INSERT INTO tool_calls (tool_name, agent_id, project_id, entity, duration_ms, success, response_bytes, created_date, session_id, error_message, input_bytes, response_type)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ToolName, ev.AgentID, ev.ProjectID, ev.Entity, ev.DurationMs, successInt, ev.ResponseBytes,
		today, ev.SessionID, ev.ErrorMessage, ev.InputBytes, ev.ResponseType,
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
	annotInt := 0
	if ev.AnnotationsIncluded {
		annotInt = 1
	}
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(
		`INSERT INTO context_deliveries
		 (tool_name, agent_id, project_id, entity, file, response_bytes, response_tokens, baseline_tokens,
		  nodes_delivered, nodes_pruned, edges_delivered, truncated, duration_ms, cache_hit, brain_enriched,
		  created_date, session_id, intent, depth_requested, depth_achieved, nodes_visited,
		  annotations_included, output_format, edge_types_dist, traversal_duration_ms, graph_size_at_traversal)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ToolName, ev.AgentID, ev.ProjectID, ev.Entity, ev.File,
		ev.ResponseBytes, ev.ResponseTokens, ev.BaselineTokens,
		ev.NodesDelivered, ev.NodesPruned, ev.EdgesDelivered,
		truncInt, ev.DurationMs, cacheInt, brainInt,
		today, ev.SessionID, ev.Intent,
		ev.DepthRequested, ev.DepthAchieved, ev.NodesVisited,
		annotInt, ev.OutputFormat, ev.EdgeTypesDist, ev.TraversalDurationMs, ev.GraphSizeAtTraversal,
	)
	return err
}

// InsertBrainUsageTx records a brain usage event without acquiring the mutex.
func (s *Store) InsertBrainUsageTx(ev pulsetypes.BrainUsageEvent) error {
	successInt := 0
	if ev.Success {
		successInt = 1
	}
	fallbackInt := 0
	if ev.FallbackUsed {
		fallbackInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO brain_usage
		 (model, tier, endpoint, prompt_tokens, completion_tokens, duration_ms, cost_usd, agent_id, project_id, target_entity, success, quality_score, fallback_used)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Model, ev.Tier, ev.Endpoint, ev.PromptTokens, ev.CompletionTokens,
		ev.DurationMs, ev.CostUSD, ev.AgentID, ev.ProjectID,
		ev.TargetEntity, successInt, ev.QualityScore, fallbackInt,
	)
	return err
}

// InsertOutcomeSignalTx records an outcome signal without acquiring the mutex.
func (s *Store) InsertOutcomeSignalTx(ev pulsetypes.OutcomeSignalEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO outcome_signals (project_id, agent_id, entity, signal_type, count, session_id, tool_calls_between)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.ProjectID, ev.AgentID, ev.Entity, ev.SignalType, ev.Count, ev.SessionID, ev.ToolCallsBetween,
	)
	return err
}

// InsertAgentLLMUsageTx records an agent LLM usage event without acquiring the mutex.
func (s *Store) InsertAgentLLMUsageTx(ev pulsetypes.AgentLLMUsageEvent) error {
	costReportedInt := 0
	if ev.CostReported {
		costReportedInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO agent_llm_usage
		 (session_id, agent_id, project_id, model, provider, input_tokens, output_tokens, cost_usd,
		  cost_reported, cache_creation_input_tokens, cache_read_input_tokens, tool_call_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.SessionID, ev.AgentID, ev.ProjectID, ev.Model, ev.Provider,
		ev.InputTokens, ev.OutputTokens, ev.CostUSD,
		costReportedInt, ev.CacheCreationInputTokens, ev.CacheReadInputTokens, ev.ToolCallID,
	)
	return err
}

// UpsertSessionWithVersionTx creates or updates a session record with agent version support.
// Bug 16 — DQ-C.6: records agent_version when provided on session start.
func (s *Store) UpsertSessionWithVersionTx(id, agentID, projectID, event, agentVersion string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	switch event {
	case "start":
		_, err := s.db.Exec(
			`INSERT INTO sessions (id, agent_id, project_id, started_at, agent_version) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET started_at = excluded.started_at, agent_version = CASE WHEN excluded.agent_version != '' THEN excluded.agent_version ELSE agent_version END`,
			id, agentID, projectID, now, agentVersion,
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
		`INSERT INTO reparse_events (file, language, duration_ms, nodes_before, nodes_after, edges_delta, memories_staled, project_id, created_date, debounce_hits, cross_project_detection_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.File, ev.Language, ev.DurationMs, ev.NodesBefore, ev.NodesAfter,
		ev.EdgesDelta, ev.MemoriesStaled, ev.ProjectID, today, ev.DebounceHits, ev.CrossProjectDetectionMs,
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
	provDist := ev.ProvenanceDist
	if provDist == "" {
		provDist = "{}"
	}
	_, err := s.db.Exec(
		`INSERT INTO graph_snapshots
		 (snapshot_type, nodes_total, edges_total, edges_calls, density, orphan_nodes,
		  cross_file_edge_pct, max_fanin, max_fanout, fan_in_p50, fan_in_p95, fan_out_p50, fan_out_p95,
		  node_type_dist, project_id, tombstone_ratio, provenance_dist)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.SnapshotType, ev.NodesTotal, ev.EdgesTotal, ev.EdgesCalls, ev.Density,
		ev.OrphanNodes, ev.CrossFileEdgePct, ev.MaxFanin, ev.MaxFanout,
		ev.FanInP50, ev.FanInP95, ev.FanOutP50, ev.FanOutP95,
		ev.NodeTypeDistJSON, ev.ProjectID, ev.TombstoneRatio, provDist,
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
		`INSERT INTO embedding_events (trigger, count, errors, duration_ms, model, model_status, success, stale_count, project_id, embed_pool_contention)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Trigger, ev.Count, ev.Errors, ev.DurationMs, ev.Model, ev.ModelStatus,
		successInt, ev.StaleCount, ev.ProjectID, ev.EmbedPoolContention,
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
	workDist := ev.WorkItemTypeDist
	if workDist == "" {
		workDist = "{}"
	}
	_, err := s.db.Exec(
		`INSERT INTO index_events (duration_ms, files_indexed, total_nodes, total_edges, call_sites_resolved, call_sites_unresolved, resolution_rate, language_dist, project_id, work_item_type_dist)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.DurationMs, ev.FilesIndexed, ev.TotalNodes, ev.TotalEdges,
		ev.CallSitesResolved, ev.CallSitesUnresolved, ev.ResolutionRate,
		ev.LanguageDistJSON, ev.ProjectID, workDist,
	)
	return err
}

// InsertIndexEvent records a full-index event, acquiring the mutex.
func (s *Store) InsertIndexEvent(ev pulsetypes.IndexEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertIndexEventTx(ev)
}

// InsertGuardEventTx records a guard (loop/rate-limit) event without acquiring the mutex.
func (s *Store) InsertGuardEventTx(ev pulsetypes.GuardEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO guard_events (guard_type, tool_name, category, agent_id, project_id)
		 VALUES (?, ?, ?, ?, ?)`,
		ev.GuardType, ev.ToolName, ev.Category, ev.AgentID, ev.ProjectID,
	)
	return err
}

// InsertGuardEvent records a guard event, acquiring the mutex.
func (s *Store) InsertGuardEvent(ev pulsetypes.GuardEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertGuardEventTx(ev)
}

// InsertMemoryOpTx records a memory operation event without acquiring the mutex.
func (s *Store) InsertMemoryOpTx(ev pulsetypes.MemoryOperationEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO memory_ops (operation, tier, source, result_count, agent_id, project_id)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ev.Operation, ev.Tier, ev.Source, ev.ResultCount, ev.AgentID, ev.ProjectID,
	)
	return err
}

// InsertMemoryOp records a memory operation event, acquiring the mutex.
func (s *Store) InsertMemoryOp(ev pulsetypes.MemoryOperationEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertMemoryOpTx(ev)
}

// InsertValidationEventTx records a validation event without acquiring the mutex.
func (s *Store) InsertValidationEventTx(ev pulsetypes.ValidationEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO validation_events (tool_name, status, violation_count, safety_status, agent_id, project_id, rule_ids, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ','now'))`,
		ev.ToolName, ev.Status, ev.ViolationCount, ev.SafetyStatus, ev.AgentID, ev.ProjectID, ev.RuleIDs,
	)
	return err
}

// InsertValidationEvent records a validation event, acquiring the mutex.
func (s *Store) InsertValidationEvent(ev pulsetypes.ValidationEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertValidationEventTx(ev)
}

// CountGuardEvents returns the count of guard events of the given type for a calendar day.
func (s *Store) CountGuardEvents(day, guardType string) int {
	var n int
	row := s.db.QueryRow(
		`SELECT COUNT(*) FROM guard_events WHERE guard_type = ? AND date(created_at) = ?`,
		guardType, day)
	_ = row.Scan(&n)
	return n
}

// CountMemoryOps returns the count of memory operations of the given type for a calendar day.
func (s *Store) CountMemoryOps(day, operation string) int {
	var n int
	row := s.db.QueryRow(
		`SELECT COUNT(*) FROM memory_ops WHERE operation = ? AND date(created_at) = ?`,
		operation, day)
	_ = row.Scan(&n)
	return n
}

// CountValidationViolations returns the count of validation events with violations for a calendar day.
func (s *Store) CountValidationViolations(day string) int {
	var n int
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(violation_count), 0) FROM validation_events WHERE date(created_at) = ?`,
		day)
	_ = row.Scan(&n)
	return n
}

// CountToolErrors returns the number of failed tool calls on the given day.
func (s *Store) CountToolErrors(day string) int {
	var n int
	row := s.db.QueryRow(
		`SELECT COUNT(*) FROM tool_calls WHERE success = 0 AND created_date = ?`, day)
	_ = row.Scan(&n)
	return n
}

// SumBrainCostForDay returns total brain LLM cost (USD) for the given day.
func (s *Store) SumBrainCostForDay(day string) float64 {
	var v float64
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(cost_usd), 0.0) FROM brain_usage WHERE date(created_at) = ?`, day)
	_ = row.Scan(&v)
	return v
}

// SumAgentLLMCostForDay returns total agent-reported LLM cost (USD) for the given day.
func (s *Store) SumAgentLLMCostForDay(day string) float64 {
	var v float64
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(cost_usd), 0.0) FROM agent_llm_usage WHERE date(created_at) = ?`, day)
	_ = row.Scan(&v)
	return v
}

// CountTruncatedDeliveries returns (truncated count, total count) for context_deliveries on the given day.
func (s *Store) CountTruncatedDeliveries(day string) (truncated, total int) {
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(CASE WHEN truncated=1 THEN 1 ELSE 0 END),0), COUNT(*)
		 FROM context_deliveries WHERE created_date = ?`, day)
	_ = row.Scan(&truncated, &total)
	return truncated, total
}

// CountBFSCacheHitsForDay returns context deliveries with cache_hit=1 for the given day.
func (s *Store) CountBFSCacheHitsForDay(day string) int {
	var n int
	row := s.db.QueryRow(
		`SELECT COUNT(*) FROM context_deliveries WHERE cache_hit = 1 AND created_date = ?`, day)
	_ = row.Scan(&n)
	return n
}

// CountValidationCalls returns the number of validation_events with the given tool_name for the given day.
func (s *Store) CountValidationCalls(day, toolName string) int {
	var n int
	row := s.db.QueryRow(
		`SELECT COUNT(*) FROM validation_events WHERE tool_name = ? AND date(created_at) = ?`,
		toolName, day)
	_ = row.Scan(&n)
	return n
}

// SumMemoriesStaled returns SUM(memories_staled) from reparse_events for the given day.
func (s *Store) SumMemoriesStaled(day string) int {
	var n int
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(memories_staled), 0) FROM reparse_events WHERE created_date = ?`, day)
	_ = row.Scan(&n)
	return n
}

// AvgSessionDurationMs returns the average session duration in ms for sessions that
// started on the given day and have a non-null ended_at.
func (s *Store) AvgSessionDurationMs(day string) float64 {
	var v float64
	row := s.db.QueryRow(
		`SELECT COALESCE(AVG((julianday(ended_at)-julianday(started_at))*86400000.0), 0.0)
		 FROM sessions WHERE date(started_at) = ? AND ended_at IS NOT NULL`, day)
	_ = row.Scan(&v)
	return v
}

// CountResumedSessions returns the count of sessions on the given day where the same agent_id
// had a previous session that ended within the 24 hours before this session started.
func (s *Store) CountResumedSessions(day string) int {
	var n int
	row := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions s1
		 WHERE date(s1.started_at) = ?
		 AND EXISTS (
		   SELECT 1 FROM sessions s2
		   WHERE s2.agent_id = s1.agent_id AND s2.id != s1.id
		     AND s2.ended_at IS NOT NULL
		     AND s2.ended_at > datetime(s1.started_at, '-24 hours')
		     AND s2.ended_at <= s1.started_at
		 )`, day)
	_ = row.Scan(&n)
	return n
}

// GetWorkflowAdherenceRate returns the fraction of sessions on the given day that called
// session_init + (prepare_context or plan_context) + validate_plan + verify_implementation.
// Returns 0.0 if no sessions match.
func (s *Store) GetWorkflowAdherenceRate(day string) float64 {
	var v float64
	row := s.db.QueryRow(
		`SELECT COALESCE(
		   CAST(SUM(CASE WHEN has_init=1 AND has_context=1 AND has_validate=1 AND has_verify=1 THEN 1 ELSE 0 END) AS REAL)
		   / NULLIF(COUNT(*), 0), 0.0)
		 FROM (
		   SELECT session_id,
		     MAX(CASE WHEN tool_name='session_init' THEN 1 ELSE 0 END) as has_init,
		     MAX(CASE WHEN tool_name IN ('prepare_context','plan_context') THEN 1 ELSE 0 END) as has_context,
		     MAX(CASE WHEN tool_name='validate_plan' THEN 1 ELSE 0 END) as has_validate,
		     MAX(CASE WHEN tool_name='verify_implementation' THEN 1 ELSE 0 END) as has_verify
		   FROM tool_calls
		   WHERE created_date = ? AND session_id != ''
		   GROUP BY session_id
		 ) sub`, day)
	_ = row.Scan(&v)
	return v
}

// GetTasksPerHour returns tasks_completed / total_active_hours for sessions that started
// on the given day. Returns 0 if no sessions.
func (s *Store) GetTasksPerHour(day string) float64 {
	var v float64
	row := s.db.QueryRow(
		`SELECT COALESCE(
		   SUM(tasks_completed) / NULLIF(SUM((julianday(COALESCE(ended_at,datetime('now')))-julianday(started_at))*24), 0),
		   0.0)
		 FROM sessions WHERE date(started_at) = ?`, day)
	_ = row.Scan(&v)
	return v
}

// GetAvgTaskCompletionMs returns the average value of outcome_signals.count
// WHERE signal_type='task_done' AND date(created_at)=day. Only non-zero counts
// are averaged (count stores duration_ms; pre-P3B signals have count=0).
func (s *Store) GetAvgTaskCompletionMs(day string) float64 {
	var v float64
	row := s.db.QueryRow(
		`SELECT COALESCE(AVG(CAST(count AS REAL)), 0.0)
		 FROM outcome_signals
		 WHERE signal_type = 'task_done' AND count > 0 AND date(created_at) = ?`, day)
	_ = row.Scan(&v)
	return v
}

// CountOutcomeSignals returns the count of outcome_signals with the given signal_type
// for the given day.
func (s *Store) CountOutcomeSignals(day, signalType string) int {
	var n int
	row := s.db.QueryRow(
		`SELECT COUNT(*) FROM outcome_signals WHERE signal_type = ? AND date(created_at) = ?`,
		signalType, day)
	_ = row.Scan(&n)
	return n
}

// RuleHitStat holds a single rule's violation count over a query window.
type RuleHitStat struct {
	RuleID string `json:"rule_id"`
	Count  int    `json:"count"`
}

// GetRuleHitDistribution returns the top limit rules by violation count over the last days days.
// JSON decoding is done in Go to avoid SQLite json_each version compatibility issues.
// No mutex needed — read-only query, consistent with other read methods in this store.
func (s *Store) GetRuleHitDistribution(days, limit int) ([]RuleHitStat, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT rule_ids FROM validation_events WHERE violation_count > 0 AND created_at >= ?`,
		cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil || raw == "" {
			continue
		}
		var ids []string
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			continue
		}
		for _, id := range ids {
			if id != "" {
				counts[id]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Build sorted slice.
	stats := make([]RuleHitStat, 0, len(counts))
	for id, cnt := range counts {
		stats = append(stats, RuleHitStat{RuleID: id, Count: cnt})
	}
	// Sort descending by count.
	for i := 0; i < len(stats)-1; i++ {
		for j := i + 1; j < len(stats); j++ {
			if stats[j].Count > stats[i].Count {
				stats[i], stats[j] = stats[j], stats[i]
			}
		}
	}
	if limit > 0 && len(stats) > limit {
		stats = stats[:limit]
	}
	return stats, nil
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
		// Pulse Phase 3: context delivery depth/traversal fields.
		`ALTER TABLE context_deliveries ADD COLUMN depth_requested INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE context_deliveries ADD COLUMN depth_achieved INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE context_deliveries ADD COLUMN nodes_visited INTEGER NOT NULL DEFAULT 0`,
		// Pulse Phase 3B: rule hit distribution.
		`ALTER TABLE validation_events ADD COLUMN rule_ids TEXT NOT NULL DEFAULT ''`,
		// Pulse Phase 4 — Bug 7 (DQ-E.3): explicit cost flag.
		`ALTER TABLE agent_llm_usage ADD COLUMN cost_reported INTEGER NOT NULL DEFAULT 0`,
		// Pulse Phase 4 — Bug 9 (DQ-E.2): Anthropic cache token fields.
		`ALTER TABLE agent_llm_usage ADD COLUMN cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE agent_llm_usage ADD COLUMN cache_read_input_tokens INTEGER NOT NULL DEFAULT 0`,
		// Pulse Phase 4 — Bug 10 (DQ-A.5): tool call input size.
		`ALTER TABLE tool_calls ADD COLUMN input_bytes INTEGER NOT NULL DEFAULT 0`,
		// Pulse Phase 4 — Bug 11 (DQ-A.6): response type categorization.
		`ALTER TABLE tool_calls ADD COLUMN response_type TEXT NOT NULL DEFAULT ''`,
		// Pulse Phase 4 — Bug 12 (DQ-B.6): annotation flag on context delivery.
		`ALTER TABLE context_deliveries ADD COLUMN annotations_included INTEGER NOT NULL DEFAULT 0`,
		// Pulse Phase 4 — Bug 13 (DQ-B.7): output format on context delivery.
		`ALTER TABLE context_deliveries ADD COLUMN output_format TEXT NOT NULL DEFAULT ''`,
		// Pulse Phase 4 — Bug 16 (DQ-C.6): agent version on sessions.
		`ALTER TABLE sessions ADD COLUMN agent_version TEXT NOT NULL DEFAULT ''`,
		// Pulse Phase 4 — Bug 17 (DQ-D.3): tool calls between delivery and signal.
		`ALTER TABLE outcome_signals ADD COLUMN tool_calls_between INTEGER NOT NULL DEFAULT 0`,
		// Pulse Phase 4 — Bug 18 (DQ-E.1): tool call ID on LLM usage.
		`ALTER TABLE agent_llm_usage ADD COLUMN tool_call_id TEXT NOT NULL DEFAULT ''`,
		// Pulse Phase 4 — Bug 19 (DQ-F.3): quality score on brain usage.
		`ALTER TABLE brain_usage ADD COLUMN quality_score REAL NOT NULL DEFAULT 0.0`,
		// Pulse Phase 4 — Bug 20 (DQ-F.4): fallback flag on brain usage.
		`ALTER TABLE brain_usage ADD COLUMN fallback_used INTEGER NOT NULL DEFAULT 0`,
		// Pulse Phase 4 — Bug 58 (PIPE-B6): tombstone ratio on graph snapshots.
		`ALTER TABLE graph_snapshots ADD COLUMN tombstone_ratio REAL NOT NULL DEFAULT 0.0`,
		// Pulse Phase 4 — Bug 59 (PIPE-B8): provenance distribution on graph snapshots.
		`ALTER TABLE graph_snapshots ADD COLUMN provenance_dist TEXT NOT NULL DEFAULT '{}'`,
		// Pulse Phase 4 — Bug 60 (PIPE-C5): debounce hits on reparse events.
		`ALTER TABLE reparse_events ADD COLUMN debounce_hits INTEGER NOT NULL DEFAULT 0`,
		// Pulse Phase 4 — Bug 61 (PIPE-C8): cross-project detection time on reparse events.
		`ALTER TABLE reparse_events ADD COLUMN cross_project_detection_ms REAL NOT NULL DEFAULT 0.0`,
		// Pulse Phase 4 — Bug 62 (PIPE-D8): pool contention on embedding events.
		`ALTER TABLE embedding_events ADD COLUMN embed_pool_contention INTEGER NOT NULL DEFAULT 0`,
		// Pulse Phase 4 — Bug 63 (PIPE-E5): work item type distribution on index events.
		`ALTER TABLE index_events ADD COLUMN work_item_type_dist TEXT NOT NULL DEFAULT '{}'`,
		// Pulse Phase 4 — Bug 65 (PIPE-F3): edge type distribution on context deliveries.
		`ALTER TABLE context_deliveries ADD COLUMN edge_types_dist TEXT NOT NULL DEFAULT '{}'`,
		// Pulse Phase 4 — Bug 66 (PIPE-F4): traversal duration on context deliveries.
		`ALTER TABLE context_deliveries ADD COLUMN traversal_duration_ms REAL NOT NULL DEFAULT 0.0`,
		// Pulse Phase 4 — Bug 67 (PIPE-F6): graph size at traversal time.
		`ALTER TABLE context_deliveries ADD COLUMN graph_size_at_traversal INTEGER NOT NULL DEFAULT 0`,
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
		// Bug 6 — STO-A.3.2: composite indexes for common date+dimension queries.
		`CREATE INDEX IF NOT EXISTS idx_tc_date_tool ON tool_calls(created_date, tool_name)`,
		`CREATE INDEX IF NOT EXISTS idx_cd_date_entity ON context_deliveries(created_date, entity)`,
		`CREATE INDEX IF NOT EXISTS idx_tc_proj_date ON tool_calls(project_id, created_date)`,
		`CREATE INDEX IF NOT EXISTS idx_cd_proj_date ON context_deliveries(project_id, created_date)`,
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
	// Bug 2 — DQ-H.2: use INSERT OR REPLACE so stale prices are updated on upgrade.
	newPricing := []struct {
		model  string
		input  float64
		output float64
	}{
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
		if err := s.upsertPricingWithHistory(p.model, p.input, p.output); err != nil {
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
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    created_date      TEXT    NOT NULL DEFAULT '',
    session_id        TEXT    NOT NULL DEFAULT '',
    intent            TEXT    NOT NULL DEFAULT '',
    depth_requested   INTEGER NOT NULL DEFAULT 0,
    depth_achieved    INTEGER NOT NULL DEFAULT 0,
    nodes_visited     INTEGER NOT NULL DEFAULT 0
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
-- Bug 2 — DQ-H.2: INSERT OR REPLACE so prices are updated when schema runs on upgrade.
INSERT OR REPLACE INTO pricing (model, input_per_1m, output_per_1m, source, updated_at) VALUES
    ('gpt-4o',               2.50, 10.00, 'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('gpt-4o-mini',          0.15,  0.60, 'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('gpt-4.1',              2.00, 8.00,  'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('gpt-4.1-mini',         0.40, 1.60,  'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('gpt-4.1-nano',         0.10, 0.40,  'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('o3',                  10.00, 40.00,  'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('o3-mini',              1.10,  4.40,  'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('o4-mini',              1.10,  4.40,  'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('claude-3-5-sonnet',    3.00, 15.00, 'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('claude-3-5-haiku',     0.80,  4.00, 'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('claude-sonnet-4-6',    3.00, 15.00, 'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('claude-opus-4-6',     15.00, 75.00, 'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('claude-haiku-4-5',     0.80,  4.00, 'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('gemini-2.5-pro',       1.25,  10.00, 'default', strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    ('gemini-2.5-flash',     0.15,   0.60, 'default', strftime('%Y-%m-%dT%H:%M:%SZ','now'));

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

-- Phase 3: Agent behavior & value metric tables.

CREATE TABLE IF NOT EXISTS guard_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    guard_type  TEXT    NOT NULL,
    tool_name   TEXT    NOT NULL,
    category    TEXT    NOT NULL DEFAULT '',
    agent_id    TEXT    NOT NULL DEFAULT '',
    project_id  TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_ge_created ON guard_events(created_at);

CREATE TABLE IF NOT EXISTS memory_ops (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    operation    TEXT    NOT NULL,
    tier         TEXT    NOT NULL DEFAULT '',
    source       TEXT    NOT NULL DEFAULT '',
    result_count INTEGER NOT NULL DEFAULT 0,
    agent_id     TEXT    NOT NULL DEFAULT '',
    project_id   TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_mo_created ON memory_ops(created_at);

CREATE TABLE IF NOT EXISTS validation_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    tool_name       TEXT    NOT NULL,
    status          TEXT    NOT NULL,
    violation_count INTEGER NOT NULL DEFAULT 0,
    safety_status   TEXT    NOT NULL DEFAULT '',
    agent_id        TEXT    NOT NULL DEFAULT '',
    project_id      TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_ve_created ON validation_events(created_at);

-- Task P4-8 / Bug 57: search event tracking.
CREATE TABLE IF NOT EXISTS search_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id     TEXT    NOT NULL DEFAULT '',
    project_id   TEXT    NOT NULL DEFAULT '',
    query        TEXT    NOT NULL DEFAULT '',
    mode         TEXT    NOT NULL DEFAULT '',
    result_count INTEGER NOT NULL DEFAULT 0,
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    cache_hit    INTEGER NOT NULL DEFAULT 0,
    session_id   TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    created_date TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_se_created ON search_events(created_at);
CREATE INDEX IF NOT EXISTS idx_se_project ON search_events(project_id);

-- Bug 64 — PIPE-E6: lifecycle events (shutdown drain, etc.).
CREATE TABLE IF NOT EXISTS lifecycle_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type TEXT    NOT NULL,
    value_ms   REAL    NOT NULL DEFAULT 0.0,
    project_id TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- Bug 68 — COV-9: config hot-reload tracking.
CREATE TABLE IF NOT EXISTS config_reload_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    success        INTEGER NOT NULL DEFAULT 1,
    changed_fields TEXT    NOT NULL DEFAULT '[]',
    project_id     TEXT    NOT NULL DEFAULT '',
    created_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    created_date   TEXT    NOT NULL DEFAULT ''
);

-- Bug 69 — COV-12: store persistence tracking.
CREATE TABLE IF NOT EXISTS persistence_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    duration_ms   REAL    NOT NULL DEFAULT 0.0,
    bytes_written INTEGER NOT NULL DEFAULT 0,
    project_id    TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    created_date  TEXT    NOT NULL DEFAULT ''
);

-- Bug 70 — COV-Subsys (metrics/): enrichment event tracking.
CREATE TABLE IF NOT EXISTS enrichment_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    enrichment_type TEXT    NOT NULL DEFAULT '',
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    success         INTEGER NOT NULL DEFAULT 1,
    project_id      TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    created_date    TEXT    NOT NULL DEFAULT ''
);

-- Bug 71 — COV-Subsys (config/): rule evaluation tracking.
CREATE TABLE IF NOT EXISTS rule_eval_events (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    rules_evaluated  INTEGER NOT NULL DEFAULT 0,
    violations_found INTEGER NOT NULL DEFAULT 0,
    project_id       TEXT    NOT NULL DEFAULT '',
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    created_date     TEXT    NOT NULL DEFAULT ''
);

-- Bug 73 — STO-A.1.3: pricing history table.
CREATE TABLE IF NOT EXISTS pricing_history (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    model      TEXT NOT NULL,
    old_price  REAL NOT NULL DEFAULT 0.0,
    new_price  REAL NOT NULL DEFAULT 0.0,
    changed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- Bug 74 — STO-F.2.1: pruning audit log.
CREATE TABLE IF NOT EXISTS pruning_log (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    day          TEXT    NOT NULL DEFAULT '',
    table_name   TEXT    NOT NULL,
    rows_deleted INTEGER NOT NULL DEFAULT 0,
    archived_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
`

// ---------------------------------------------------------------------------
// Write methods
// ---------------------------------------------------------------------------

// InsertToolCall records a single MCP tool call event.
func (s *Store) InsertToolCall(ev pulsetypes.ToolCallEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertToolCallTx(ev)
}

// InsertContextDelivery records a context delivery with token savings.
func (s *Store) InsertContextDelivery(ev pulsetypes.ContextDeliveryEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertContextDeliveryTx(ev)
}

// InsertBrainUsage records a brain sidecar LLM inference event.
func (s *Store) InsertBrainUsage(ev pulsetypes.BrainUsageEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertBrainUsageTx(ev)
}

// UpsertSessionWithVersion creates or updates a session record with agent version (Bug 16 — DQ-C.6).
func (s *Store) UpsertSessionWithVersion(id, agentID, projectID, event, agentVersion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.UpsertSessionWithVersionTx(id, agentID, projectID, event, agentVersion)
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

// GetSummaryForProject returns aggregated analytics for the last N days filtered to a specific project.
// Uses a raw-table scan since daily_rollups are not project-scoped.
func (s *Store) GetSummaryForProject(days int, projectID string) (*Summary, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	sum := &Summary{}

	row := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(duration_ms), 0)
		 FROM tool_calls WHERE created_at >= ? AND project_id = ?`, since, projectID)
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
		 FROM context_deliveries WHERE created_at >= ? AND project_id = ?`, since, projectID)
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
		 FROM sessions WHERE started_at >= ? AND project_id = ?`, since, projectID)
	if err := row.Scan(&sum.Sessions, &sum.TasksCompleted, &sum.CostSavedUSD); err != nil {
		return nil, err
	}

	if sum.BaselineTokens > 0 && sum.TokensSaved > 0 {
		sum.SavingsPct = float64(sum.TokensSaved) / float64(sum.BaselineTokens) * 100.0
	}
	computeValueMultiplier(sum)

	return sum, nil
}

// WoWComparison holds week-over-week metric deltas computed from daily_rollups.
type WoWComparison struct {
	ThisWeek map[string]float64 `json:"this_week"`
	LastWeek map[string]float64 `json:"last_week"`
	Delta    map[string]float64 `json:"delta"`     // absolute change
	DeltaPct map[string]float64 `json:"delta_pct"` // percentage change (0 if last_week was 0)
}

// wowSummableMetrics is the set of metrics that can be meaningfully summed over a week.
// Rate/average metrics are excluded because summing them produces nonsensical values.
var wowSummableMetrics = map[string]bool{
	"tokens_saved": true, "tokens_delivered": true, "baseline_tokens": true,
	"tool_calls": true, "context_deliveries": true, "cost_saved_usd": true,
	"sessions": true, "tasks_completed": true, "cache_hits": true,
	"brain_enriched_count": true, "total_latency_ms": true,
	"total_reparses": true, "total_reparse_duration_ms": true,
	"file_changes_count": true,
	"guard_circuit_breaks": true, "rate_limit_rejections": true,
	"recall_hits": true, "recall_misses": true, "validation_violations": true,
	// Phase 3B summable metrics (rate/average metrics intentionally excluded).
	"error_count": true, "brain_cost_usd": true, "agent_llm_cost_usd": true,
	"truncated_deliveries": true, "bfs_cache_hits": true, "validate_plan_count": true,
	"memory_writes": true, "safety_check_hits": true, "safety_check_misses": true,
	"memory_anchor_invalidations": true, "resumed_sessions": true, "replan_count": true,
}

// GetWeekOverWeek computes this-week vs last-week for key summable metrics from daily_rollups.
// Returns nil on error (best-effort; caller should handle nil).
func (s *Store) GetWeekOverWeek() (*WoWComparison, error) {
	today := time.Now().UTC().Format("2006-01-02")
	weekAgo := time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02")
	twoWeeksAgo := time.Now().UTC().AddDate(0, 0, -14).Format("2006-01-02")

	sumPeriod := func(from, before string) (map[string]float64, error) {
		rows, err := s.db.Query(
			`SELECT metric, SUM(value) FROM daily_rollups WHERE day >= ? AND day < ? GROUP BY metric`,
			from, before)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		result := make(map[string]float64)
		for rows.Next() {
			var metric string
			var value float64
			if err := rows.Scan(&metric, &value); err != nil {
				return nil, err
			}
			if wowSummableMetrics[metric] {
				result[metric] = value
			}
		}
		return result, rows.Err()
	}

	thisWeek, err := sumPeriod(weekAgo, today)
	if err != nil {
		return nil, err
	}
	lastWeek, err := sumPeriod(twoWeeksAgo, weekAgo)
	if err != nil {
		return nil, err
	}

	delta := make(map[string]float64)
	deltaPct := make(map[string]float64)
	// Union of both weeks' metrics.
	keys := make(map[string]bool)
	for k := range thisWeek {
		keys[k] = true
	}
	for k := range lastWeek {
		keys[k] = true
	}
	for k := range keys {
		tw := thisWeek[k]
		lw := lastWeek[k]
		delta[k] = tw - lw
		if lw != 0 {
			deltaPct[k] = (tw - lw) / lw * 100.0
		} else {
			deltaPct[k] = 0
		}
	}

	return &WoWComparison{
		ThisWeek: thisWeek,
		LastWeek: lastWeek,
		Delta:    delta,
		DeltaPct: deltaPct,
	}, nil
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
	Date             string  `json:"date"`
	TokensSaved      int     `json:"tokens_saved"`
	ToolCalls        int     `json:"tool_calls"`
	CostSavedUSD     float64 `json:"cost_saved_usd"`
	// Bug 30 — ROI-A5: compression ratio (baseline / delivered) for this day.
	CompressionRatio float64 `json:"compression_ratio,omitempty"`
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
		        COALESCE(SUM(cost_saved_usd), 0),
		        COALESCE(SUM(baseline_sum), 0),
		        COALESCE(SUM(response_sum), 0)
		 FROM (
		   SELECT created_date AS day,
		          COALESCE(SUM(baseline_tokens) - SUM(response_tokens), 0) AS tokens_saved,
		          0 AS tool_calls,
		          0.0 AS cost_saved_usd,
		          COALESCE(SUM(baseline_tokens), 0) AS baseline_sum,
		          COALESCE(SUM(response_tokens), 0) AS response_sum
		   FROM context_deliveries
		   WHERE created_date >= ?
		   GROUP BY created_date
		   UNION ALL
		   SELECT created_date AS day,
		          0 AS tokens_saved,
		          COUNT(*) AS tool_calls,
		          0.0 AS cost_saved_usd,
		          0 AS baseline_sum,
		          0 AS response_sum
		   FROM tool_calls
		   WHERE created_date >= ?
		   GROUP BY created_date
		   UNION ALL
		   SELECT date(started_at) AS day,
		          0 AS tokens_saved,
		          0 AS tool_calls,
		          COALESCE(SUM(cost_saved_usd), 0) AS cost_saved_usd,
		          0 AS baseline_sum,
		          0 AS response_sum
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
		var baselineSum, responseSum int64
		if err := rows.Scan(&p.Date, &p.TokensSaved, &p.ToolCalls, &p.CostSavedUSD, &baselineSum, &responseSum); err != nil {
			return nil, err
		}
		if p.TokensSaved < 0 {
			p.TokensSaved = 0
		}
		if responseSum > 0 {
			p.CompressionRatio = float64(baselineSum) / float64(responseSum)
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
	AgentID              string `json:"agent_id"`
	Sessions             int    `json:"sessions"`
	ToolCalls            int    `json:"tool_calls"`
	TokensSaved          int    `json:"tokens_saved"`
	TasksCompleted       int    `json:"tasks_completed"`
	// Bug 14 — DQ-C.3: number of distinct entities queried by this agent.
	UniqueEntitiesQueried int `json:"unique_entities_queried,omitempty"`
	// Bug 15 — DQ-C.4: number of distinct tools used by this agent.
	UniqueToolsUsed int `json:"unique_tools_used,omitempty"`
}

// GetAgentStats returns per-agent analytics for the last N days.
func (s *Store) GetAgentStats(days int) ([]AgentStats, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	cutoffDate := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	// sessions.tokens_saved is the canonical per-agent token savings counter,
	// updated by UpdateSessionStats on every context delivery that carries an
	// agent_id. This avoids the LEFT JOIN on context_deliveries which produces
	// zeros whenever the delivery lacks an agent_id (common for bare get_context
	// calls that omit the optional agent_id param).
	rows, err := s.db.Query(
		`SELECT a.agent_id,
		        COUNT(DISTINCT a.id)    AS sessions,
		        SUM(a.tool_calls)       AS tool_calls,
		        SUM(a.tokens_saved)     AS tokens_saved,
		        SUM(a.tasks_completed)  AS tasks_completed,
		        (SELECT COUNT(DISTINCT entity) FROM tool_calls WHERE agent_id=a.agent_id AND created_date >= ?) as unique_entities,
		        (SELECT COUNT(DISTINCT tool_name) FROM tool_calls WHERE agent_id=a.agent_id AND created_date >= ?) as unique_tools
		 FROM sessions a
		 WHERE a.started_at >= ?
		 GROUP BY a.agent_id
		 ORDER BY tokens_saved DESC`, cutoffDate, cutoffDate, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []AgentStats
	for rows.Next() {
		var as AgentStats
		if err := rows.Scan(&as.AgentID, &as.Sessions, &as.ToolCalls, &as.TokensSaved, &as.TasksCompleted,
			&as.UniqueEntitiesQueried, &as.UniqueToolsUsed); err != nil {
			return nil, err
		}
		stats = append(stats, as)
	}
	return stats, rows.Err()
}

// EventCount returns the total number of events across all main tables.
// Bug 4 — STO-A.3.3: use a single query to avoid 3 round-trips.
func (s *Store) EventCount() (int, int, int) {
	row := s.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM tool_calls),
			(SELECT COUNT(*) FROM context_deliveries),
			(SELECT COUNT(*) FROM brain_usage)`)
	var tc, cd, bu int
	_ = row.Scan(&tc, &cd, &bu)
	return tc, cd, bu
}

// PruneOldEvents removes events older than the given number of days.
// Covers all event tables including outcome_signals and agent_llm_usage.
// Bug 72 — STO-A.1.2: also cleans up orphaned agent_llm_usage rows.
// Bug 74 — STO-F.2.1: records deleted row counts to pruning_log.
func (s *Store) PruneOldEvents(retentionDays int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	since := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339)
	today := time.Now().UTC().Format("2006-01-02")
	var totalDeleted int64

	for _, table := range []string{
		"tool_calls", "context_deliveries", "brain_usage", "outcome_signals", "agent_llm_usage",
		"parse_events", "reparse_events", "graph_snapshots", "embedding_events", "index_events",
		"guard_events", "memory_ops", "validation_events",
		"search_events", "config_reload_events", "persistence_events",
		"enrichment_events", "rule_eval_events", "lifecycle_events",
	} {
		// Bug 74: count rows before deleting so we can log them.
		var count int
		s.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE created_at < ?", table), since).Scan(&count)
		result, err := s.db.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE created_at < ?", table), since)
		if err != nil {
			return totalDeleted, err
		}
		if n, err := result.RowsAffected(); err == nil {
			totalDeleted += n
			if n > 0 {
				s.db.Exec(`INSERT INTO pruning_log (day, table_name, rows_deleted) VALUES (?, ?, ?)`,
					today, table, n)
			}
		}
	}

	// Prune sessions older than retention
	var sessCount int
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE started_at < ?`, since).Scan(&sessCount)
	result, err := s.db.Exec(`DELETE FROM sessions WHERE started_at < ?`, since)
	if err != nil {
		return totalDeleted, err
	}
	if n, err := result.RowsAffected(); err == nil {
		totalDeleted += n
		if n > 0 {
			s.db.Exec(`INSERT INTO pruning_log (day, table_name, rows_deleted) VALUES (?, 'sessions', ?)`,
				today, n)
		}
	}

	// Bug 72 — STO-A.1.2: soft FK cleanup — orphaned agent_llm_usage rows.
	s.db.Exec(`DELETE FROM agent_llm_usage WHERE session_id != '' AND session_id NOT IN (SELECT id FROM sessions) AND created_at < ?`,
		time.Now().UTC().AddDate(0, 0, -retentionDays).Format(time.RFC3339))

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
	return s.InsertOutcomeSignalTx(ev)
}

// GetEffectiveness returns per-entity effectiveness scores using the shared
// pulsetypes.EntityEffectiveness type so callers don't need a type conversion.
// minSignals filters out entities with too few data points to be meaningful.
// Only entities with at least one negative signal are returned (low performers only).
// Bug 5 — DQ-Effectiveness: applies exponential recency weighting (decay = exp(-days/30)).
func (s *Store) GetEffectiveness(projectID string, minSignals int) ([]pulsetypes.EntityEffectiveness, error) {
	if minSignals <= 0 {
		minSignals = 2
	}
	rows, err := s.db.Query(`
		SELECT entity, signal_type, created_at
		FROM outcome_signals
		WHERE (? = '' OR project_id = ?)
		  AND entity != ''
		ORDER BY created_at DESC`,
		projectID, projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type entityData struct {
		weightedPos float64
		weightedNeg float64
		signals     int
	}
	entityMap := make(map[string]*entityData)
	now := time.Now().UTC()
	for rows.Next() {
		var entity, signalType, createdAt string
		if err := rows.Scan(&entity, &signalType, &createdAt); err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, createdAt)
		if err != nil {
			// try date-only format
			t, err = time.Parse("2006-01-02", createdAt[:10])
			if err != nil {
				continue
			}
		}
		daysSince := now.Sub(t).Hours() / 24.0
		weight := math.Exp(-daysSince / 30.0)

		if _, ok := entityMap[entity]; !ok {
			entityMap[entity] = &entityData{}
		}
		ed := entityMap[entity]
		ed.signals++
		switch signalType {
		case "task_done":
			ed.weightedPos += weight
		case "correction", "escalation", "replan", "task_cancelled":
			ed.weightedNeg += weight
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var results []pulsetypes.EntityEffectiveness
	for entity, ed := range entityMap {
		if ed.signals < minSignals {
			continue
		}
		total := ed.weightedPos + ed.weightedNeg
		var score float64
		if total > 0 {
			score = ed.weightedPos / total
		} else {
			score = 1.0
		}
		// Approximate integer counts for backward-compat fields.
		pos := int(math.Round(ed.weightedPos))
		neg := int(math.Round(ed.weightedNeg))
		if neg == 0 {
			continue // only return low performers
		}
		e := pulsetypes.EntityEffectiveness{
			Entity:    entity,
			Score:     score,
			Signals:   ed.signals,
			Positives: pos,
			Negatives: neg,
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
	// Sort by negatives desc, signals desc to match old order.
	for i := 0; i < len(results)-1; i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Negatives > results[i].Negatives ||
				(results[j].Negatives == results[i].Negatives && results[j].Signals > results[i].Signals) {
				results[i], results[j] = results[j], results[i]
			}
		}
	}
	if len(results) > 20 {
		results = results[:20]
	}
	return results, nil
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
	return s.InsertAgentLLMUsageTx(ev)
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

// ---------------------------------------------------------------------------
// Bug 1 — DQ-H.3: session model lookup for cost-saved computation
// ---------------------------------------------------------------------------

// GetSessionModel returns the model recorded for the given session, or "" if not found.
func (s *Store) GetSessionModel(sessionID string) string {
	var model string
	row := s.db.QueryRow(`SELECT COALESCE(model, '') FROM sessions WHERE id = ?`, sessionID)
	_ = row.Scan(&model)
	return model
}

// ---------------------------------------------------------------------------
// Bug 8 — DQ-G.5: abandonment rate
// ---------------------------------------------------------------------------

// GetAbandonmentRate returns the fraction of sessions that started on the given day,
// were at least 30 minutes old, and never received an end signal.
//
// Both numerator and denominator are filtered to sessions older than 30 minutes so
// that young sessions (started < 30 min ago) are not counted in either side —
// they have not yet had the opportunity to be abandoned, so including them in the
// denominator would deflate the rate incorrectly (especially during morning rollups).
func (s *Store) GetAbandonmentRate(day string) float64 {
	var v float64
	row := s.db.QueryRow(`
		SELECT COALESCE(
			CAST(SUM(CASE WHEN ended_at IS NULL THEN 1 ELSE 0 END) AS REAL)
			/ NULLIF(COUNT(*), 0), 0.0)
		FROM sessions
		WHERE date(started_at) = ?
		  AND started_at < datetime('now', '-30 minutes')`, day)
	_ = row.Scan(&v)
	return v
}

// ---------------------------------------------------------------------------
// Bug 21 — DQ-G.2: per-project rollup helpers
// ---------------------------------------------------------------------------

// GetProjectsForDay returns all distinct project_ids that had activity on the given day.
func (s *Store) GetProjectsForDay(day string) []string {
	rows, err := s.db.Query(
		`SELECT DISTINCT project_id FROM tool_calls WHERE created_date = ? AND project_id != ''`, day)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			result = append(result, p)
		}
	}
	return result
}

// GetSummaryForDayProject returns aggregated metrics for a specific calendar day and project.
func (s *Store) GetSummaryForDayProject(day, projectID string) (*Summary, error) {
	sum := &Summary{}

	row := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(duration_ms), 0)
		 FROM tool_calls WHERE created_date = ? AND project_id = ?`, day, projectID)
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
		 FROM context_deliveries WHERE created_date = ? AND project_id = ?`, day, projectID)
	if err := row.Scan(&sum.ContextDeliveries, &sum.TokensDelivered, &sum.BaselineTokens,
		&sum.CacheHits, &sum.BrainEnrichedCount); err != nil {
		return nil, err
	}

	if sum.ContextDeliveries > 0 {
		sum.CacheHitRate = float64(sum.CacheHits) / float64(sum.ContextDeliveries)
		sum.BrainEnrichRate = float64(sum.BrainEnrichedCount) / float64(sum.ContextDeliveries)
	}
	if sum.TotalToolCalls > 0 {
		sum.AvgLatencyMs = sum.TotalLatencyMs / float64(sum.TotalToolCalls)
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
		 FROM sessions WHERE date(started_at) = ? AND project_id = ?`, day, projectID)
	if err := row.Scan(&sum.Sessions, &sum.TasksCompleted, &sum.CostSavedUSD); err != nil {
		return nil, err
	}
	if sum.BaselineTokens > 0 && sum.TokensSaved > 0 {
		sum.SavingsPct = float64(sum.TokensSaved) / float64(sum.BaselineTokens) * 100.0
	}
	return sum, nil
}

// ---------------------------------------------------------------------------
// Bug 22 — DQ-G.3: per-tool rollup
// ---------------------------------------------------------------------------

// GetTopToolsForDay returns top-5 tool call counts for the given day.
func (s *Store) GetTopToolsForDay(day string) map[string]int {
	rows, err := s.db.Query(
		`SELECT tool_name, COUNT(*) FROM tool_calls WHERE created_date = ? GROUP BY tool_name ORDER BY COUNT(*) DESC LIMIT 5`, day)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make(map[string]int)
	for rows.Next() {
		var name string
		var cnt int
		if rows.Scan(&name, &cnt) == nil {
			result[name] = cnt
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Bug 23 — ROI-C5: cache token savings
// ---------------------------------------------------------------------------

// SumCacheTokenSavings returns tokens saved specifically from cache hits.
func (s *Store) SumCacheTokenSavings(day string) int {
	var n int
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(baseline_tokens - response_tokens), 0)
		 FROM context_deliveries WHERE cache_hit = 1 AND created_date = ?`, day)
	_ = row.Scan(&n)
	return n
}

// ---------------------------------------------------------------------------
// Bug 24 — ROI-C7: context freshness score
// ---------------------------------------------------------------------------

// GetContextFreshnessScore returns fraction of context deliveries where the entity
// was reparsed within 24h prior to delivery.
func (s *Store) GetContextFreshnessScore(day string) float64 {
	var v float64
	row := s.db.QueryRow(`
		SELECT COALESCE(
			CAST(SUM(CASE WHEN EXISTS(
				SELECT 1 FROM reparse_events r
				WHERE r.file = cd.entity
				  AND r.created_at >= datetime(cd.created_at, '-24 hours')
				  AND r.created_at <= cd.created_at
			) THEN 1 ELSE 0 END) AS REAL) / NULLIF(COUNT(*), 0), 0.0)
		FROM context_deliveries cd WHERE cd.created_date = ?`, day)
	_ = row.Scan(&v)
	return v
}

// ---------------------------------------------------------------------------
// Bug 25 — ROI-D5: entity memory coverage
// ---------------------------------------------------------------------------

// GetEntityMemoryCoverage returns fraction of unique queried entities that have
// at least one positive outcome signal (proxy for memory coverage).
func (s *Store) GetEntityMemoryCoverage(day string) float64 {
	var v float64
	row := s.db.QueryRow(`
		SELECT COALESCE(
			CAST(COUNT(DISTINCT os.entity) AS REAL) / NULLIF(COUNT(DISTINCT cd.entity), 0), 0.0)
		FROM context_deliveries cd
		LEFT JOIN outcome_signals os ON os.entity = cd.entity
		WHERE cd.created_date = ?`, day)
	_ = row.Scan(&v)
	return v
}

// ---------------------------------------------------------------------------
// Bug 26 — ROI-E6: embedding coverage
// ---------------------------------------------------------------------------

// CountTotalMemories returns an approximation of total memory entries from
// memory_ops write events (since the main store is a separate DB).
func (s *Store) CountTotalMemories() int {
	var n int
	row := s.db.QueryRow(`SELECT COUNT(*) FROM memory_ops WHERE operation = 'write'`)
	_ = row.Scan(&n)
	return n
}

// GetEmbeddingCoveragePct returns 1 - (stale_embeddings / total_memories).
func (s *Store) GetEmbeddingCoveragePct() float64 {
	stale := s.CountStaleEmbeddings()
	total := s.CountTotalMemories()
	if total == 0 {
		return 1.0
	}
	covered := total - stale
	if covered < 0 {
		covered = 0
	}
	return float64(covered) / float64(total)
}

// ---------------------------------------------------------------------------
// Bug 27 — ROI-E7: DB size
// ---------------------------------------------------------------------------

// DBSizeBytes returns the size of the SQLite database file in bytes.
// Returns 0 if the size cannot be determined.
func (s *Store) DBSizeBytes(dbPath string) int64 {
	fi, err := os.Stat(dbPath)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// ---------------------------------------------------------------------------
// Bug 29 — ROI-A3: top entities by savings
// ---------------------------------------------------------------------------

// EntitySavings holds an entity and its token savings.
type EntitySavings struct {
	Entity  string `json:"entity"`
	Savings int    `json:"savings"`
}

// TopEntitiesBySavings returns entities ranked by token savings over the last N days.
func (s *Store) TopEntitiesBySavings(days, limit int) ([]EntitySavings, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(`
		SELECT entity, COALESCE(SUM(baseline_tokens - response_tokens), 0) as savings
		FROM context_deliveries WHERE created_date >= ? AND entity != ''
		GROUP BY entity ORDER BY savings DESC LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []EntitySavings
	for rows.Next() {
		var es EntitySavings
		if rows.Scan(&es.Entity, &es.Savings) == nil {
			result = append(result, es)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 31 — ROI-A7: cost savings by model
// ---------------------------------------------------------------------------

// ModelCostStat holds per-model cost savings.
type ModelCostStat struct {
	Model     string  `json:"model"`
	CostSaved float64 `json:"cost_saved_usd"`
	Sessions  int     `json:"sessions"`
}

// GetCostSavingsByModel returns cost savings grouped by model over the last N days.
func (s *Store) GetCostSavingsByModel(days int) ([]ModelCostStat, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.db.Query(`
		SELECT model, COALESCE(SUM(cost_saved_usd), 0), COUNT(*)
		FROM sessions WHERE started_at >= ? AND model != ''
		GROUP BY model ORDER BY SUM(cost_saved_usd) DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ModelCostStat
	for rows.Next() {
		var st ModelCostStat
		if rows.Scan(&st.Model, &st.CostSaved, &st.Sessions) == nil {
			result = append(result, st)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 32 — ROI-B3: avg task duration
// ---------------------------------------------------------------------------

// GetAvgTaskDuration returns average task duration approximated from outcome_signals.
func (s *Store) GetAvgTaskDuration(days int) float64 {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	var v float64
	row := s.db.QueryRow(`
		SELECT COALESCE(AVG(CAST(count AS REAL)), 0.0)
		FROM outcome_signals WHERE signal_type = 'task_done' AND count > 0 AND created_at >= ?`, cutoff)
	_ = row.Scan(&v)
	return v
}

// ---------------------------------------------------------------------------
// Bug 33 — ROI-B4: agent token efficiency
// ---------------------------------------------------------------------------

// AgentEfficiencyStat holds per-agent token efficiency.
type AgentEfficiencyStat struct {
	AgentID            string  `json:"agent_id"`
	TokensSavedPerCall float64 `json:"tokens_saved_per_call"`
}

// GetAgentTokenEfficiency returns tokens saved per context delivery call per agent.
func (s *Store) GetAgentTokenEfficiency(days int) ([]AgentEfficiencyStat, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(`
		SELECT agent_id,
		       COALESCE(CAST(SUM(baseline_tokens - response_tokens) AS REAL) / NULLIF(COUNT(*), 0), 0.0)
		FROM context_deliveries WHERE created_date >= ? AND agent_id != ''
		GROUP BY agent_id ORDER BY 2 DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AgentEfficiencyStat
	for rows.Next() {
		var ae AgentEfficiencyStat
		if rows.Scan(&ae.AgentID, &ae.TokensSavedPerCall) == nil {
			result = append(result, ae)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 34 — ROI-B6: context reuse rate
// ---------------------------------------------------------------------------

// GetContextReuseRate returns fraction of context deliveries that reuse the same entity+session.
func (s *Store) GetContextReuseRate(days int) float64 {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var v float64
	row := s.db.QueryRow(`
		SELECT COALESCE(
			CAST(COUNT(*) AS REAL) / NULLIF((SELECT COUNT(*) FROM context_deliveries WHERE created_date >= ?), 0), 0.0)
		FROM (
			SELECT entity, session_id FROM context_deliveries WHERE created_date >= ?
			GROUP BY entity, session_id HAVING COUNT(*) > 1
		)`, cutoff, cutoff)
	_ = row.Scan(&v)
	return v
}

// ---------------------------------------------------------------------------
// Bug 35 — ROI-B7: engagement score
// ---------------------------------------------------------------------------

// GetEngagementScore returns a composite engagement score per session.
// Formula: (tasks_completed×10 + cache_hits×2 + positive_signals×5) / sessions
//
// Uses a single query so all four counters come from one consistent DB snapshot —
// avoiding the skew that four separate QueryRow calls would introduce when new
// rows are inserted between calls.
func (s *Store) GetEngagementScore(days int) float64 {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var tasksCompleted, cacheHits, positiveSignals, sessions int
	row := s.db.QueryRow(`
		SELECT
			(SELECT COALESCE(SUM(tasks_completed), 0) FROM sessions         WHERE date(started_at)  >= ?),
			(SELECT COUNT(*)                           FROM context_deliveries WHERE cache_hit = 1 AND created_date >= ?),
			(SELECT COUNT(*)                           FROM outcome_signals    WHERE signal_type = 'task_done' AND date(created_at) >= ?),
			(SELECT COUNT(*)                           FROM sessions           WHERE date(started_at)  >= ?)`,
		cutoff, cutoff, cutoff, cutoff)
	if err := row.Scan(&tasksCompleted, &cacheHits, &positiveSignals, &sessions); err != nil || sessions == 0 {
		return 0
	}
	return float64(tasksCompleted*10+cacheHits*2+positiveSignals*5) / float64(sessions)
}

// ---------------------------------------------------------------------------
// Bug 36 — SA-A6: onboarding latency
// ---------------------------------------------------------------------------

// GetOnboardingLatencyMs returns average ms from session_init to first context tool call.
func (s *Store) GetOnboardingLatencyMs(days int) float64 {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(`
		SELECT session_id,
		       MIN(CASE WHEN tool_name='session_init' THEN created_at END) as init_at,
		       MIN(CASE WHEN tool_name IN ('get_context','prepare_context','plan_context') THEN created_at END) as ctx_at
		FROM tool_calls WHERE created_date >= ? AND session_id != ''
		GROUP BY session_id
		HAVING init_at IS NOT NULL AND ctx_at IS NOT NULL`, cutoff)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var total float64
	var count int
	for rows.Next() {
		var sid, initAt, ctxAt string
		if rows.Scan(&sid, &initAt, &ctxAt) != nil {
			continue
		}
		t1, e1 := time.Parse(time.RFC3339, initAt)
		t2, e2 := time.Parse(time.RFC3339, ctxAt)
		if e1 != nil || e2 != nil {
			continue
		}
		delta := t2.Sub(t1).Seconds() * 1000
		if delta >= 0 {
			total += delta
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return total / float64(count)
}

// ---------------------------------------------------------------------------
// Bug 37 — SA-A8: multi-session campaigns
// ---------------------------------------------------------------------------

// GetMultiSessionCampaigns returns count of agents with more than 1 session in the period.
func (s *Store) GetMultiSessionCampaigns(days int) int {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	var n int
	row := s.db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT agent_id FROM sessions WHERE started_at >= ? AND agent_id != ''
			GROUP BY agent_id HAVING COUNT(*) > 1
		)`, cutoff)
	_ = row.Scan(&n)
	return n
}

// ---------------------------------------------------------------------------
// Bug 38 — SA-B1: agent tool preferences
// ---------------------------------------------------------------------------

// AgentToolPref holds a single agent+tool call count.
type AgentToolPref struct {
	AgentID  string `json:"agent_id"`
	ToolName string `json:"tool_name"`
	Count    int    `json:"count"`
}

// GetAgentToolPreferences returns per-agent, per-tool call counts.
func (s *Store) GetAgentToolPreferences(days int) ([]AgentToolPref, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(`
		SELECT agent_id, tool_name, COUNT(*) as cnt
		FROM tool_calls WHERE created_date >= ? AND agent_id != ''
		GROUP BY agent_id, tool_name ORDER BY agent_id, cnt DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AgentToolPref
	for rows.Next() {
		var p AgentToolPref
		if rows.Scan(&p.AgentID, &p.ToolName, &p.Count) == nil {
			result = append(result, p)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 39 — SA-B3: agent efficiency scores
// ---------------------------------------------------------------------------

// AgentEfficiency holds per-agent efficiency metrics.
type AgentEfficiency struct {
	AgentID              string  `json:"agent_id"`
	TasksPerSession      float64 `json:"tasks_per_session"`
	TokensSavedPerTask   float64 `json:"tokens_saved_per_task"`
}

// GetAgentEfficiencyScores returns per-agent efficiency scores.
func (s *Store) GetAgentEfficiencyScores(days int) ([]AgentEfficiency, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.db.Query(`
		SELECT agent_id,
		       COALESCE(CAST(SUM(tasks_completed) AS REAL)/NULLIF(COUNT(*),0), 0.0) as tps,
		       COALESCE(CAST(SUM(tokens_saved) AS REAL)/NULLIF(NULLIF(SUM(tasks_completed),0),0), 0.0) as tspt
		FROM sessions WHERE started_at >= ? AND agent_id != ''
		GROUP BY agent_id`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AgentEfficiency
	for rows.Next() {
		var ae AgentEfficiency
		if rows.Scan(&ae.AgentID, &ae.TasksPerSession, &ae.TokensSavedPerTask) == nil {
			result = append(result, ae)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 40 — SA-B4: model comparison
// ---------------------------------------------------------------------------

// ModelComparisonStat holds per-model session stats.
type ModelComparisonStat struct {
	Model          string  `json:"model"`
	SessionCount   int     `json:"session_count"`
	AvgToolCalls   float64 `json:"avg_tool_calls"`
	AvgTokensSaved float64 `json:"avg_tokens_saved"`
	AvgCostSaved   float64 `json:"avg_cost_saved_usd"`
}

// GetModelComparison returns per-model session statistics.
func (s *Store) GetModelComparison(days int) ([]ModelComparisonStat, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.db.Query(`
		SELECT model, COUNT(*), AVG(tool_calls), AVG(tokens_saved), AVG(cost_saved_usd)
		FROM sessions WHERE started_at >= ? AND model != ''
		GROUP BY model ORDER BY COUNT(*) DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ModelComparisonStat
	for rows.Next() {
		var m ModelComparisonStat
		if rows.Scan(&m.Model, &m.SessionCount, &m.AvgToolCalls, &m.AvgTokensSaved, &m.AvgCostSaved) == nil {
			result = append(result, m)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 41 — SA-B7: error recovery patterns
// ---------------------------------------------------------------------------

// ErrorRecovery holds a tool failure → recovery pattern.
type ErrorRecovery struct {
	FailedTool   string  `json:"failed_tool"`
	RecoveryTool string  `json:"recovery_tool"`
	Count        int     `json:"count"`
	RecoveryRate float64 `json:"recovery_rate"`
}

// GetErrorRecoveryPatterns returns common tool failure→recovery sequences.
// Uses ROW_NUMBER() window function to find consecutive calls within each session,
// avoiding the t2.id = t1.id+1 trap (global IDs are not contiguous per session).
func (s *Store) GetErrorRecoveryPatterns(days int) ([]ErrorRecovery, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(`
		WITH ranked AS (
			SELECT id, session_id, tool_name, success,
			       ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY id) AS rn
			FROM tool_calls WHERE created_date >= ? AND session_id != ''
		)
		SELECT t1.tool_name as failed, t2.tool_name as recovery,
		       COUNT(*) as cnt,
		       CAST(SUM(CASE WHEN t2.success=1 THEN 1 ELSE 0 END) AS REAL)/NULLIF(COUNT(*),0) as rate
		FROM ranked t1
		JOIN ranked t2 ON t2.session_id = t1.session_id AND t2.rn = t1.rn + 1
		WHERE t1.success = 0
		GROUP BY t1.tool_name, t2.tool_name
		ORDER BY cnt DESC LIMIT 20`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ErrorRecovery
	for rows.Next() {
		var er ErrorRecovery
		if rows.Scan(&er.FailedTool, &er.RecoveryTool, &er.Count, &er.RecoveryRate) == nil {
			result = append(result, er)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 42 — SA-C2: tool pair correlation
// ---------------------------------------------------------------------------

// ToolPairStat holds a tool→tool co-occurrence.
type ToolPairStat struct {
	Tool1 string `json:"tool1"`
	Tool2 string `json:"tool2"`
	Count int    `json:"count"`
}

// GetToolPairCorrelation returns the most common consecutive tool pairs.
// Uses ROW_NUMBER() window function for correct per-session ordering.
func (s *Store) GetToolPairCorrelation(days int) ([]ToolPairStat, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(`
		WITH ranked AS (
			SELECT session_id, tool_name,
			       ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY id) AS rn
			FROM tool_calls WHERE created_date >= ? AND session_id != ''
		)
		SELECT t1.tool_name, t2.tool_name, COUNT(*) as cnt
		FROM ranked t1
		JOIN ranked t2 ON t2.session_id = t1.session_id AND t2.rn = t1.rn + 1
		GROUP BY t1.tool_name, t2.tool_name
		ORDER BY cnt DESC LIMIT 20`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ToolPairStat
	for rows.Next() {
		var tp ToolPairStat
		if rows.Scan(&tp.Tool1, &tp.Tool2, &tp.Count) == nil {
			result = append(result, tp)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 43 — SA-C3: discovery tool effectiveness
// ---------------------------------------------------------------------------

// GetDiscoveryToolEffectiveness returns how often search/find_entity leads to a context delivery.
func (s *Store) GetDiscoveryToolEffectiveness(days int) float64 {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var v float64
	row := s.db.QueryRow(`
		SELECT COALESCE(
			CAST(COUNT(DISTINCT t1.id) AS REAL) / NULLIF(
				(SELECT COUNT(*) FROM tool_calls WHERE tool_name IN ('search','find_entity') AND created_date >= ?), 0
			), 0.0)
		FROM tool_calls t1
		JOIN tool_calls t2 ON t2.session_id = t1.session_id AND t2.id > t1.id
		WHERE t1.tool_name IN ('search','find_entity')
		  AND t2.tool_name IN ('get_context','prepare_context','plan_context')
		  AND t1.created_date >= ?`, cutoff, cutoff)
	_ = row.Scan(&v)
	return v
}

// ---------------------------------------------------------------------------
// Bug 44 — SA-C6: skill execution stats
// ---------------------------------------------------------------------------

// SkillStat holds aggregated statistics for a specific skill.
type SkillStat struct {
	SkillName     string  `json:"skill_name"`
	CallCount     int     `json:"call_count"`
	SuccessRate   float64 `json:"success_rate"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

// GetSkillExecutionStats returns aggregated stats for execute_skill calls.
func (s *Store) GetSkillExecutionStats(days int) ([]SkillStat, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(`
		SELECT entity, COUNT(*),
		       COALESCE(CAST(SUM(success) AS REAL)/NULLIF(COUNT(*),0), 0.0),
		       COALESCE(AVG(duration_ms), 0.0)
		FROM tool_calls
		WHERE tool_name = 'execute_skill' AND created_date >= ? AND entity != ''
		GROUP BY entity ORDER BY COUNT(*) DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SkillStat
	for rows.Next() {
		var ss SkillStat
		if rows.Scan(&ss.SkillName, &ss.CallCount, &ss.SuccessRate, &ss.AvgDurationMs) == nil {
			result = append(result, ss)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 45 — SA-D5: memory type distribution
// ---------------------------------------------------------------------------

// GetMemoryTypeDistribution returns memory operation counts grouped by operation type.
func (s *Store) GetMemoryTypeDistribution(days int) (map[string]int, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.db.Query(`SELECT operation, COUNT(*) FROM memory_ops WHERE created_at >= ? GROUP BY operation`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]int)
	for rows.Next() {
		var op string
		var cnt int
		if rows.Scan(&op, &cnt) == nil {
			result[op] = cnt
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 46 — SA-E3: cancellation reasons
// ---------------------------------------------------------------------------

// CancellationStat holds a cancellation reason entity and count.
type CancellationStat struct {
	Entity string `json:"entity"`
	Count  int    `json:"count"`
}

// GetCancellationReasons returns entities most associated with task cancellations.
func (s *Store) GetCancellationReasons(days int) ([]CancellationStat, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.db.Query(`
		SELECT entity, COUNT(*) FROM outcome_signals
		WHERE signal_type = 'task_cancelled' AND created_at >= ? AND entity != ''
		GROUP BY entity ORDER BY COUNT(*) DESC LIMIT 20`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CancellationStat
	for rows.Next() {
		var cs CancellationStat
		if rows.Scan(&cs.Entity, &cs.Count) == nil {
			result = append(result, cs)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 47 — SA-E4: plan complexity vs outcome
// ---------------------------------------------------------------------------

// PlanComplexityStat holds replan count and outcome averages.
type PlanComplexityStat struct {
	ReplanCount       int     `json:"replan_count"`
	AvgTasksCompleted float64 `json:"avg_tasks_completed"`
	SessionCount      int     `json:"session_count"`
}

// GetPlanComplexityVsOutcome groups sessions by replan count and computes avg tasks completed.
func (s *Store) GetPlanComplexityVsOutcome(days int) ([]PlanComplexityStat, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.db.Query(`
		SELECT replans, COALESCE(AVG(tasks_completed), 0.0), COUNT(*) FROM (
			SELECT s.id, s.tasks_completed,
				(SELECT COUNT(*) FROM outcome_signals os WHERE os.session_id = s.id AND os.signal_type = 'replan') as replans
			FROM sessions s WHERE s.started_at >= ?
		) GROUP BY replans ORDER BY replans`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PlanComplexityStat
	for rows.Next() {
		var ps PlanComplexityStat
		if rows.Scan(&ps.ReplanCount, &ps.AvgTasksCompleted, &ps.SessionCount) == nil {
			result = append(result, ps)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 48 — SA-E5: blocked task count
// ---------------------------------------------------------------------------

// GetBlockedTaskCount returns sessions with more than 1 replan signal (blocked/stuck sessions).
func (s *Store) GetBlockedTaskCount(days int) int {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	var n int
	row := s.db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT session_id FROM outcome_signals
			WHERE signal_type = 'replan' AND created_at >= ?
			GROUP BY session_id HAVING COUNT(*) > 1
		)`, cutoff)
	_ = row.Scan(&n)
	return n
}

// ---------------------------------------------------------------------------
// Bug 49 — SA-F1: message volume stats
// ---------------------------------------------------------------------------

// MessageVolumeStat holds message volume metrics.
type MessageVolumeStat struct {
	TotalToolCalls     int     `json:"total_tool_calls"`
	TotalSessions      int     `json:"total_sessions"`
	AvgCallsPerSession float64 `json:"avg_calls_per_session"`
}

// GetMessageVolumeStats returns message volume metrics for the last N days.
func (s *Store) GetMessageVolumeStats(days int) (*MessageVolumeStat, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var stat MessageVolumeStat
	s.db.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE created_date >= ?`, cutoff).Scan(&stat.TotalToolCalls)
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE date(started_at) >= ?`, cutoff).Scan(&stat.TotalSessions)
	if stat.TotalSessions > 0 {
		stat.AvgCallsPerSession = float64(stat.TotalToolCalls) / float64(stat.TotalSessions)
	}
	return &stat, nil
}

// ---------------------------------------------------------------------------
// Bug 50 — SA-F3: cross-project query volume
// ---------------------------------------------------------------------------

// GetCrossProjectQueryVolume returns count of cross-project federation events.
func (s *Store) GetCrossProjectQueryVolume(days int) int {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	var n int
	row := s.db.QueryRow(`
		SELECT COUNT(*) FROM outcome_signals
		WHERE signal_type IN ('federation', 'cross_project') AND created_at >= ?`, cutoff)
	_ = row.Scan(&n)
	return n
}

// ---------------------------------------------------------------------------
// Bug 51 — SA-F4: approval gate usage
// ---------------------------------------------------------------------------

// GetApprovalGateUsage returns count of check_plan_safety tool calls for the last N days.
func (s *Store) GetApprovalGateUsage(days int) int {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var n int
	row := s.db.QueryRow(`
		SELECT COUNT(*) FROM tool_calls WHERE tool_name = 'check_plan_safety' AND created_date >= ?`, cutoff)
	_ = row.Scan(&n)
	return n
}

// ---------------------------------------------------------------------------
// Bug 52 — ROI-F2: model efficiency comparison
// ---------------------------------------------------------------------------

// ModelEfficiency holds per-model efficiency metrics.
type ModelEfficiency struct {
	Model                string  `json:"model"`
	TokensSavedPerDollar float64 `json:"tokens_saved_per_dollar"`
	TotalCostUSD         float64 `json:"total_cost_usd"`
}

// GetModelEfficiencyComparison returns tokens saved per dollar spent per model.
func (s *Store) GetModelEfficiencyComparison(days int) ([]ModelEfficiency, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(`
		SELECT s.model,
		       COALESCE(CAST(SUM(s.tokens_saved) AS REAL) / NULLIF(SUM(alu.cost_usd), 0), 0.0),
		       COALESCE(SUM(alu.cost_usd), 0.0)
		FROM sessions s
		LEFT JOIN agent_llm_usage alu ON alu.session_id = s.id
		WHERE s.model != '' AND date(s.started_at) >= ?
		GROUP BY s.model ORDER BY 2 DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ModelEfficiency
	for rows.Next() {
		var me ModelEfficiency
		if rows.Scan(&me.Model, &me.TokensSavedPerDollar, &me.TotalCostUSD) == nil {
			result = append(result, me)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 53 — ROI-F3: project efficiency comparison
// ---------------------------------------------------------------------------

// ProjectEfficiency holds per-project efficiency metrics.
type ProjectEfficiency struct {
	ProjectID      string  `json:"project_id"`
	TokensSaved    int     `json:"tokens_saved"`
	CostSavedUSD   float64 `json:"cost_saved_usd"`
	TasksCompleted int     `json:"tasks_completed"`
	Sessions       int     `json:"sessions"`
}

// GetProjectEfficiencyComparison returns efficiency metrics grouped by project.
func (s *Store) GetProjectEfficiencyComparison(days int) ([]ProjectEfficiency, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.db.Query(`
		SELECT project_id, COALESCE(SUM(tokens_saved),0), COALESCE(SUM(cost_saved_usd),0.0),
		       COALESCE(SUM(tasks_completed),0), COUNT(*)
		FROM sessions WHERE started_at >= ? AND project_id != ''
		GROUP BY project_id ORDER BY SUM(tokens_saved) DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ProjectEfficiency
	for rows.Next() {
		var pe ProjectEfficiency
		if rows.Scan(&pe.ProjectID, &pe.TokensSaved, &pe.CostSavedUSD, &pe.TasksCompleted, &pe.Sessions) == nil {
			result = append(result, pe)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 54 — STO-D.3.2: per-tool timeline
// ---------------------------------------------------------------------------

// ToolTimelinePoint is a single data point for a specific tool.
type ToolTimelinePoint struct {
	Day       string  `json:"day"`
	Calls     int     `json:"calls"`
	ErrorRate float64 `json:"error_rate"`
}

// GetToolTimeline returns daily call counts and error rates for a specific tool.
func (s *Store) GetToolTimeline(toolName string, days int) ([]ToolTimelinePoint, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(`
		SELECT created_date, COUNT(*),
		       COALESCE(CAST(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END) AS REAL)/NULLIF(COUNT(*),0), 0.0)
		FROM tool_calls WHERE tool_name = ? AND created_date >= ?
		GROUP BY created_date ORDER BY created_date`, toolName, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ToolTimelinePoint
	for rows.Next() {
		var tp ToolTimelinePoint
		if rows.Scan(&tp.Day, &tp.Calls, &tp.ErrorRate) == nil {
			result = append(result, tp)
		}
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Bug 55 — STO-D.3.3: session detail
// ---------------------------------------------------------------------------

// SessionDetail holds full detail for a single session.
type SessionDetail struct {
	SessionID  string                             `json:"session_id"`
	AgentID    string                             `json:"agent_id"`
	StartedAt  string                             `json:"started_at"`
	EndedAt    string                             `json:"ended_at,omitempty"`
	Model      string                             `json:"model,omitempty"`
	ToolCalls  []pulsetypes.ToolCallEvent         `json:"tool_calls"`
	Deliveries []pulsetypes.ContextDeliveryEvent  `json:"context_deliveries"`
	Signals    []pulsetypes.OutcomeSignalEvent    `json:"outcome_signals"`
}

// GetSessionDetail returns full event detail for a single session.
func (s *Store) GetSessionDetail(sessionID string) (*SessionDetail, error) {
	var detail SessionDetail
	detail.SessionID = sessionID
	row := s.db.QueryRow(`SELECT agent_id, started_at, COALESCE(ended_at,''), model FROM sessions WHERE id = ?`, sessionID)
	if err := row.Scan(&detail.AgentID, &detail.StartedAt, &detail.EndedAt, &detail.Model); err != nil {
		if err == sql.ErrNoRows {
			return &SessionDetail{SessionID: sessionID}, nil
		}
		return nil, err
	}
	// Each query block closes its rows before opening the next one to avoid
	// exhausting the connection pool (MaxOpenConns=2) with 3 concurrent iterators.
	rows, err := s.db.Query(`SELECT tool_name, agent_id, project_id, entity, duration_ms, success, response_bytes, session_id, error_message FROM tool_calls WHERE session_id = ? ORDER BY id`, sessionID)
	if err == nil {
		for rows.Next() {
			var tc pulsetypes.ToolCallEvent
			var successInt int
			if rows.Scan(&tc.ToolName, &tc.AgentID, &tc.ProjectID, &tc.Entity, &tc.DurationMs, &successInt, &tc.ResponseBytes, &tc.SessionID, &tc.ErrorMessage) == nil {
				tc.Success = successInt == 1
				detail.ToolCalls = append(detail.ToolCalls, tc)
			}
		}
		rows.Close()
	}
	rows2, err := s.db.Query(`SELECT tool_name, agent_id, project_id, entity, response_tokens, baseline_tokens, duration_ms, cache_hit, session_id FROM context_deliveries WHERE session_id = ? ORDER BY id`, sessionID)
	if err == nil {
		for rows2.Next() {
			var cd pulsetypes.ContextDeliveryEvent
			var cacheInt int
			if rows2.Scan(&cd.ToolName, &cd.AgentID, &cd.ProjectID, &cd.Entity, &cd.ResponseTokens, &cd.BaselineTokens, &cd.DurationMs, &cacheInt, &cd.SessionID) == nil {
				cd.CacheHit = cacheInt == 1
				detail.Deliveries = append(detail.Deliveries, cd)
			}
		}
		rows2.Close()
	}
	rows3, err := s.db.Query(`SELECT project_id, agent_id, entity, signal_type, count, session_id FROM outcome_signals WHERE session_id = ?`, sessionID)
	if err == nil {
		for rows3.Next() {
			var sig pulsetypes.OutcomeSignalEvent
			if rows3.Scan(&sig.ProjectID, &sig.AgentID, &sig.Entity, &sig.SignalType, &sig.Count, &sig.SessionID) == nil {
				detail.Signals = append(detail.Signals, sig)
			}
		}
		rows3.Close()
	}
	return &detail, nil
}

// ---------------------------------------------------------------------------
// Bug 56 — STO-D.3.5: export raw data
// ---------------------------------------------------------------------------

// ExportData holds exported raw event data.
type ExportData struct {
	ToolCalls         []map[string]interface{} `json:"tool_calls"`
	ContextDeliveries []map[string]interface{} `json:"context_deliveries"`
	Sessions          []map[string]interface{} `json:"sessions"`
	OutcomeSignals    []map[string]interface{} `json:"outcome_signals"`
	DayRange          int                      `json:"day_range"`
}

// ExportRawData returns raw event data for the last N days suitable for export.
// Each query block closes its rows before opening the next to avoid exhausting
// the connection pool (MaxOpenConns=2) with 4 concurrent iterators.
func (s *Store) ExportRawData(days int) (*ExportData, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	export := &ExportData{DayRange: days}

	if rows, err := s.db.Query(`SELECT tool_name, agent_id, project_id, entity, duration_ms, success, created_date, session_id FROM tool_calls WHERE created_date >= ?`, cutoff); err == nil {
		for rows.Next() {
			var name, agent, proj, entity, date, sess string
			var dur int64
			var succ int
			if rows.Scan(&name, &agent, &proj, &entity, &dur, &succ, &date, &sess) == nil {
				export.ToolCalls = append(export.ToolCalls, map[string]interface{}{
					"tool_name": name, "agent_id": agent, "project_id": proj,
					"entity": entity, "duration_ms": dur, "success": succ == 1,
					"created_date": date, "session_id": sess,
				})
			}
		}
		rows.Close()
	}

	if rows, err := s.db.Query(`SELECT tool_name, agent_id, entity, response_tokens, baseline_tokens, created_date, session_id FROM context_deliveries WHERE created_date >= ?`, cutoff); err == nil {
		for rows.Next() {
			var name, agent, entity, date, sess string
			var rt, bt int
			if rows.Scan(&name, &agent, &entity, &rt, &bt, &date, &sess) == nil {
				export.ContextDeliveries = append(export.ContextDeliveries, map[string]interface{}{
					"tool_name": name, "agent_id": agent, "entity": entity,
					"response_tokens": rt, "baseline_tokens": bt, "created_date": date, "session_id": sess,
				})
			}
		}
		rows.Close()
	}

	if rows, err := s.db.Query(`SELECT id, agent_id, project_id, started_at, COALESCE(ended_at,''), model FROM sessions WHERE date(started_at) >= ?`, cutoff); err == nil {
		for rows.Next() {
			var id, agent, proj, startAt, endAt, model string
			if rows.Scan(&id, &agent, &proj, &startAt, &endAt, &model) == nil {
				export.Sessions = append(export.Sessions, map[string]interface{}{
					"id": id, "agent_id": agent, "project_id": proj,
					"started_at": startAt, "ended_at": endAt, "model": model,
				})
			}
		}
		rows.Close()
	}

	if rows, err := s.db.Query(`SELECT project_id, agent_id, entity, signal_type, session_id FROM outcome_signals WHERE date(created_at) >= ?`, cutoff); err == nil {
		for rows.Next() {
			var proj, agent, entity, sig, sess string
			if rows.Scan(&proj, &agent, &entity, &sig, &sess) == nil {
				export.OutcomeSignals = append(export.OutcomeSignals, map[string]interface{}{
					"project_id": proj, "agent_id": agent, "entity": entity,
					"signal_type": sig, "session_id": sess,
				})
			}
		}
		rows.Close()
	}
	return export, nil
}

// ---------------------------------------------------------------------------
// Task P4-4: hypothetical vs actual cost
// ---------------------------------------------------------------------------

// GetHypotheticalCostUSD returns what the agent would have paid using baseline_tokens at the default model price.
func (s *Store) GetHypotheticalCostUSD(days int) float64 {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var totalBaseline int64
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(baseline_tokens), 0) FROM context_deliveries WHERE created_date >= ?`, cutoff)
	_ = row.Scan(&totalBaseline)
	inputPer1M, _, found := s.GetPricing("claude-sonnet-4-6")
	if !found || inputPer1M <= 0 {
		return 0
	}
	return float64(totalBaseline) / 1_000_000.0 * inputPer1M
}

// GetWithSynapsesCostUSD returns what the agent actually paid using response_tokens at the default model price.
func (s *Store) GetWithSynapsesCostUSD(days int) float64 {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var totalResponse int64
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(response_tokens), 0) FROM context_deliveries WHERE created_date >= ?`, cutoff)
	_ = row.Scan(&totalResponse)
	inputPer1M, _, found := s.GetPricing("claude-sonnet-4-6")
	if !found || inputPer1M <= 0 {
		return 0
	}
	return float64(totalResponse) / 1_000_000.0 * inputPer1M
}

// ---------------------------------------------------------------------------
// Task P4-3: latency percentiles
// ---------------------------------------------------------------------------

// GetLatencyPercentiles returns p50, p95, p99 latency in ms for context deliveries.
func (s *Store) GetLatencyPercentiles(days int) (p50, p95, p99 float64) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(
		`SELECT duration_ms FROM context_deliveries WHERE created_date >= ? ORDER BY duration_ms`, cutoff)
	if err != nil {
		return 0, 0, 0
	}
	defer rows.Close()
	var durations []float64
	for rows.Next() {
		var d float64
		if rows.Scan(&d) == nil {
			durations = append(durations, d)
		}
	}
	n := len(durations)
	if n == 0 {
		return 0, 0, 0
	}
	p50 = percentile(durations, 50)
	p95 = percentile(durations, 95)
	p99 = percentile(durations, 99)
	return
}

// percentile returns the value at the given percentile of a sorted slice.
func percentile(sorted []float64, pct float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * pct / 100.0)
	return sorted[idx]
}

// ---------------------------------------------------------------------------
// Task P4-6: context precision/recall/F1
// ---------------------------------------------------------------------------

// GetContextPrecision returns the fraction of individual context deliveries that were
// followed by a task_done outcome signal in the same session.
//
// Measured at delivery granularity (not session granularity) so that a session
// with 20 redundant deliveries and 1 task_done does not score the same as a
// focused session with 1 delivery and 1 task_done — both would be 100% at
// session level, but 5% vs 100% at delivery level, which is far more informative.
func (s *Store) GetContextPrecision(days int) float64 {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var v float64
	row := s.db.QueryRow(`
		SELECT COALESCE(
			CAST(SUM(CASE WHEN EXISTS(
				SELECT 1 FROM outcome_signals os
				WHERE os.session_id = cd.session_id
				  AND os.signal_type = 'task_done'
			) THEN 1 ELSE 0 END) AS REAL) / NULLIF(COUNT(*), 0),
		0.0)
		FROM context_deliveries cd
		WHERE cd.created_date >= ?`, cutoff)
	_ = row.Scan(&v)
	return v
}

// GetContextRecall returns fraction of task_done sessions that had a context delivery.
func (s *Store) GetContextRecall(days int) float64 {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var v float64
	row := s.db.QueryRow(`
		SELECT COALESCE(
			CAST(COUNT(DISTINCT os.session_id) AS REAL) / NULLIF((SELECT COUNT(DISTINCT session_id) FROM outcome_signals WHERE signal_type='task_done' AND date(created_at) >= ?), 0), 0.0)
		FROM outcome_signals os
		WHERE os.signal_type = 'task_done' AND date(os.created_at) >= ?
		AND EXISTS (
			SELECT 1 FROM context_deliveries cd WHERE cd.session_id = os.session_id
		)`, cutoff, cutoff)
	_ = row.Scan(&v)
	return v
}

// GetContextF1 returns the harmonic mean of precision and recall.
func (s *Store) GetContextF1(days int) float64 {
	p := s.GetContextPrecision(days)
	r := s.GetContextRecall(days)
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}

// ---------------------------------------------------------------------------
// Task P4-7: graph snapshot endpoint
// ---------------------------------------------------------------------------

// GraphSnapshotRow is a graph snapshot record for HTTP responses.
type GraphSnapshotRow struct {
	SnapshotType string  `json:"snapshot_type"`
	NodesTotal   int     `json:"nodes_total"`
	EdgesTotal   int     `json:"edges_total"`
	Density      float64 `json:"density"`
	OrphanNodes  int     `json:"orphan_nodes"`
	NodeTypeDist string  `json:"node_type_distribution"`
	ProjectID    string  `json:"project_id"`
	CreatedAt    string  `json:"created_at"`
}

// GetLatestGraphSnapshot returns the most recent graph_snapshots row.
func (s *Store) GetLatestGraphSnapshot() (*GraphSnapshotRow, error) {
	row := s.db.QueryRow(`
		SELECT snapshot_type, nodes_total, edges_total, density, orphan_nodes, node_type_dist, project_id, created_at
		FROM graph_snapshots ORDER BY created_at DESC LIMIT 1`)
	var gs GraphSnapshotRow
	err := row.Scan(&gs.SnapshotType, &gs.NodesTotal, &gs.EdgesTotal, &gs.Density, &gs.OrphanNodes, &gs.NodeTypeDist, &gs.ProjectID, &gs.CreatedAt)
	if err == sql.ErrNoRows {
		return &GraphSnapshotRow{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &gs, nil
}

// ---------------------------------------------------------------------------
// Task P4-8 / Bug 57: search event tracking
// ---------------------------------------------------------------------------

// InsertSearchEventTx records a search event without acquiring the mutex.
func (s *Store) InsertSearchEventTx(ev pulsetypes.SearchEvent) error {
	cacheInt := 0
	if ev.CacheHit {
		cacheInt = 1
	}
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(
		`INSERT INTO search_events (agent_id, project_id, query, mode, result_count, duration_ms, cache_hit, session_id, created_date)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.AgentID, ev.ProjectID, ev.Query, ev.Mode, ev.ResultCount, ev.DurationMs, cacheInt, ev.SessionID, today)
	return err
}

// InsertSearchEvent records a search event, acquiring the mutex.
func (s *Store) InsertSearchEvent(ev pulsetypes.SearchEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertSearchEventTx(ev)
}

// SearchStats holds aggregated search event statistics.
type SearchStats struct {
	TotalSearches  int            `json:"total_searches"`
	AvgResultCount float64        `json:"avg_result_count"`
	AvgDurationMs  float64        `json:"avg_duration_ms"`
	CacheHitRate   float64        `json:"cache_hit_rate"`
	TopModes       map[string]int `json:"top_modes"`
}

// GetSearchStats returns aggregated search statistics for the last N days.
func (s *Store) GetSearchStats(days int) (*SearchStats, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	row := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(AVG(result_count),0), COALESCE(AVG(duration_ms),0),
		       COALESCE(CAST(SUM(cache_hit) AS REAL)/NULLIF(COUNT(*),0), 0.0)
		FROM search_events WHERE created_date >= ?`, cutoff)
	var stats SearchStats
	if err := row.Scan(&stats.TotalSearches, &stats.AvgResultCount, &stats.AvgDurationMs, &stats.CacheHitRate); err != nil {
		return &SearchStats{}, nil
	}
	modeRows, err := s.db.Query(
		`SELECT mode, COUNT(*) FROM search_events WHERE created_date >= ? GROUP BY mode ORDER BY COUNT(*) DESC LIMIT 5`, cutoff)
	if err == nil {
		defer modeRows.Close()
		stats.TopModes = make(map[string]int)
		for modeRows.Next() {
			var m string
			var cnt int
			if modeRows.Scan(&m, &cnt) == nil {
				stats.TopModes[m] = cnt
			}
		}
	}
	return &stats, nil
}

// ---------------------------------------------------------------------------
// Bug 64 — PIPE-E6: lifecycle events
// ---------------------------------------------------------------------------

// RecordLifecycleEvent records a lifecycle event (e.g., shutdown_drain).
func (s *Store) RecordLifecycleEvent(eventType string, valueMs float64, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO lifecycle_events (event_type, value_ms, project_id) VALUES (?, ?, ?)`,
		eventType, valueMs, projectID)
	return err
}

// ---------------------------------------------------------------------------
// Bug 68 — COV-9: config reload tracking
// ---------------------------------------------------------------------------

// InsertConfigReloadEventTx records a config reload event without acquiring the mutex.
func (s *Store) InsertConfigReloadEventTx(ev pulsetypes.ConfigReloadEvent) error {
	successInt := 1
	if !ev.Success {
		successInt = 0
	}
	fieldsJSON := "[]"
	if len(ev.ChangedFields) > 0 {
		if b, err := json.Marshal(ev.ChangedFields); err == nil {
			fieldsJSON = string(b)
		}
	}
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(
		`INSERT INTO config_reload_events (success, changed_fields, project_id, created_date) VALUES (?, ?, ?, ?)`,
		successInt, fieldsJSON, ev.ProjectID, today)
	return err
}

// InsertConfigReloadEvent records a config reload event, acquiring the mutex.
func (s *Store) InsertConfigReloadEvent(ev pulsetypes.ConfigReloadEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertConfigReloadEventTx(ev)
}

// CountConfigReloads returns the number of config reload events for the given day.
func (s *Store) CountConfigReloads(day string) int {
	var n int
	row := s.db.QueryRow(`SELECT COUNT(*) FROM config_reload_events WHERE created_date = ?`, day)
	_ = row.Scan(&n)
	return n
}

// ---------------------------------------------------------------------------
// Bug 69 — COV-12: persistence event tracking
// ---------------------------------------------------------------------------

// InsertPersistenceEventTx records a persistence event without acquiring the mutex.
func (s *Store) InsertPersistenceEventTx(ev pulsetypes.PersistenceEvent) error {
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(
		`INSERT INTO persistence_events (duration_ms, bytes_written, project_id, created_date) VALUES (?, ?, ?, ?)`,
		ev.DurationMs, ev.BytesWritten, ev.ProjectID, today)
	return err
}

// InsertPersistenceEvent records a persistence event, acquiring the mutex.
func (s *Store) InsertPersistenceEvent(ev pulsetypes.PersistenceEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertPersistenceEventTx(ev)
}

// AvgPersistMs returns the average persistence duration for the given day.
func (s *Store) AvgPersistMs(day string) float64 {
	var v float64
	row := s.db.QueryRow(`SELECT COALESCE(AVG(duration_ms), 0.0) FROM persistence_events WHERE created_date = ?`, day)
	_ = row.Scan(&v)
	return v
}

// CountPersistOps returns the number of persistence events for the given day.
func (s *Store) CountPersistOps(day string) int {
	var n int
	row := s.db.QueryRow(`SELECT COUNT(*) FROM persistence_events WHERE created_date = ?`, day)
	_ = row.Scan(&n)
	return n
}

// ---------------------------------------------------------------------------
// Bug 70 — COV-Subsys (metrics/): enrichment event tracking
// ---------------------------------------------------------------------------

// InsertEnrichmentEventTx records an enrichment event without acquiring the mutex.
func (s *Store) InsertEnrichmentEventTx(ev pulsetypes.EnrichmentEvent) error {
	successInt := 1
	if !ev.Success {
		successInt = 0
	}
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(
		`INSERT INTO enrichment_events (enrichment_type, duration_ms, success, project_id, created_date) VALUES (?, ?, ?, ?, ?)`,
		ev.EnrichmentType, ev.DurationMs, successInt, ev.ProjectID, today)
	return err
}

// InsertEnrichmentEvent records an enrichment event, acquiring the mutex.
func (s *Store) InsertEnrichmentEvent(ev pulsetypes.EnrichmentEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertEnrichmentEventTx(ev)
}

// ---------------------------------------------------------------------------
// Bug 71 — COV-Subsys (config/): rule evaluation tracking
// ---------------------------------------------------------------------------

// InsertRuleEvalEventTx records a rule evaluation event without acquiring the mutex.
func (s *Store) InsertRuleEvalEventTx(ev pulsetypes.RuleEvalEvent) error {
	today := time.Now().UTC().Format("2006-01-02")
	_, err := s.db.Exec(
		`INSERT INTO rule_eval_events (rules_evaluated, violations_found, project_id, created_date) VALUES (?, ?, ?, ?)`,
		ev.RulesEvaluated, ev.ViolationsFound, ev.ProjectID, today)
	return err
}

// InsertRuleEvalEvent records a rule evaluation event, acquiring the mutex.
func (s *Store) InsertRuleEvalEvent(ev pulsetypes.RuleEvalEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InsertRuleEvalEventTx(ev)
}

// ---------------------------------------------------------------------------
// Bug 73 — STO-A.1.3: pricing history helper
// ---------------------------------------------------------------------------

// upsertPricingWithHistory inserts/replaces a pricing entry and records history if the price changed.
func (s *Store) upsertPricingWithHistory(model string, inputPer1M, outputPer1M float64) error {
	var oldInput float64
	row := s.db.QueryRow(`SELECT input_per_1m FROM pricing WHERE model = ?`, model)
	_ = row.Scan(&oldInput)

	if _, err := s.db.Exec(`INSERT OR REPLACE INTO pricing (model, input_per_1m, output_per_1m, source, updated_at) VALUES (?, ?, ?, 'default', strftime('%Y-%m-%dT%H:%M:%SZ','now'))`,
		model, inputPer1M, outputPer1M); err != nil {
		return err
	}

	if oldInput > 0 && oldInput != inputPer1M {
		s.db.Exec(`INSERT INTO pricing_history (model, old_price, new_price) VALUES (?, ?, ?)`,
			model, oldInput, inputPer1M)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Task P4-1: brain cost stats (new format)
// ---------------------------------------------------------------------------

// BrainCostStat holds per-model/tier brain cost statistics.
type BrainCostStat struct {
	Model    string  `json:"model"`
	Tier     string  `json:"tier"`
	CostUSD  float64 `json:"cost_usd"`
	Calls    int     `json:"calls"`
	DayRange int     `json:"day_range"`
}

// GetBrainCostStats returns brain LLM costs grouped by model and tier.
func (s *Store) GetBrainCostStats(days int) ([]BrainCostStat, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	rows, err := s.db.Query(`
		SELECT model, tier, SUM(cost_usd), COUNT(*)
		FROM brain_usage WHERE created_at >= ?
		GROUP BY model, tier ORDER BY SUM(cost_usd) DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []BrainCostStat
	for rows.Next() {
		var stat BrainCostStat
		stat.DayRange = days
		if err := rows.Scan(&stat.Model, &stat.Tier, &stat.CostUSD, &stat.Calls); err != nil {
			continue
		}
		result = append(result, stat)
	}
	return result, rows.Err()
}

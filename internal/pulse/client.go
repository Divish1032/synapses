// Package pulse provides in-process analytics for the Synapses MCP server.
// Previously a separate HTTP sidecar (synapses-pulse), the collector and store
// are now embedded directly so no external process or port is required.
//
// All public methods are fire-and-forget: errors are silently discarded so
// that pulse never degrades the MCP hot path.
package pulse

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/SynapsesOS/synapses/internal/pulse/aggregator"
	"github.com/SynapsesOS/synapses/internal/pulse/collector"
	pulsestore "github.com/SynapsesOS/synapses/internal/pulse/pstore"
	"github.com/SynapsesOS/synapses/internal/pulse/types"
)

// Re-export event types from pulse/types so callers use pulse.ToolCallEvent etc.
// without needing to import the types sub-package.
type ToolCallEvent = types.ToolCallEvent
type ContextDeliveryEvent = types.ContextDeliveryEvent
type SessionEvent = types.SessionEvent
type OutcomeSignalEvent = types.OutcomeSignalEvent
type EntityEffectiveness = types.EntityEffectiveness
type BrainUsageEvent = types.BrainUsageEvent
type AgentLLMUsageEvent = types.AgentLLMUsageEvent

// Phase 2 pipeline event types.
type ParseEvent = types.ParseEvent
type ReparseEvent = types.ReparseEvent
type GraphSnapshotEvent = types.GraphSnapshotEvent
type EmbeddingEvent = types.EmbeddingEvent
type IndexEvent = types.IndexEvent

// Phase 3 agent behavior event types.
type GuardEvent = types.GuardEvent
type MemoryOperationEvent = types.MemoryOperationEvent
type ValidationEvent = types.ValidationEvent

// Phase 4 event types.
type SearchEvent = types.SearchEvent
type ConfigReloadEvent = types.ConfigReloadEvent
type PersistenceEvent = types.PersistenceEvent
type EnrichmentEvent = types.EnrichmentEvent
type RuleEvalEvent = types.RuleEvalEvent

// Phase 5 event types.
type FederationDetectEvent = types.FederationDetectEvent
type SkillExecutionEvent = types.SkillExecutionEvent
type ToolSequenceEntry = types.ToolSequenceEntry
type EntityQuality = types.EntityQuality
type DeliveryOutcome = types.DeliveryOutcome
type SessionEffectiveness = types.SessionEffectiveness
type DailyEffectiveness = types.DailyEffectiveness
type WeeklyEfficiency = types.WeeklyEfficiency
type MonthlyROI = types.MonthlyROI
type DecayStats = types.DecayStats
type SessionPercentiles = types.SessionPercentiles
type SkillStat = types.SkillStat
type DurationBuckets = types.DurationBuckets

// Client is the in-process analytics collector. It replaces the HTTP sidecar.
// Create with New; call Close when the daemon shuts down.
type Client struct {
	store  *pulsestore.Store
	coll   *collector.Collector
	agg    *aggregator.Aggregator
	dbPath string // retained for lifecycle event recording (Bug 64 — PIPE-E6)
	// P2-5: background worker counters — incremented by server.goBackground.
	bgEnqueued atomic.Int64
	bgDropped  atomic.Int64
	bgPanics   atomic.Int64
}

// DefaultDBPath returns the canonical path for the pulse SQLite database.
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("pulse: locate home dir: %w", err)
	}
	return filepath.Join(home, ".synapses", "pulse.sqlite"), nil
}

// New creates and starts an in-process pulse collector backed by a SQLite store
// at dbPath. Returns an error if the database cannot be opened.
func New(dbPath string) (*Client, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("pulse: mkdir: %w", err)
	}
	st, err := pulsestore.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("pulse: open store: %w", err)
	}
	coll := collector.New(st, 1000, 500)
	coll.Start()

	// Bug 27 — ROI-E7 / Bug 28 — ROI-E8: pass dbPath and drop-rate fn to aggregator.
	agg := aggregator.NewWithOptions(st, 3600, dbPath, coll.DropRate)
	agg.Start()

	return &Client{store: st, coll: coll, agg: agg, dbPath: dbPath}, nil
}

// NewClient is a backwards-compatible constructor for code that still calls
// NewClient(url, timeout). It ignores both arguments and calls New with the
// default DB path; errors are silently swallowed (pulse is optional).
//
// Deprecated: prefer New(dbPath) for explicit control.
func NewClient(_ string, _ int) *Client {
	dbPath, err := DefaultDBPath()
	if err != nil {
		return nil
	}
	c, _ := New(dbPath)
	return c
}

// Close stops the collector (flushing remaining events) and closes the store.
// Bug 64 — PIPE-E6: measures drain time and records a lifecycle event.
func (c *Client) Close() {
	if c == nil {
		return
	}
	start := time.Now()
	c.agg.Stop()
	c.coll.Stop()
	drainMs := float64(time.Since(start).Milliseconds())
	// Record drain latency as a lifecycle event before closing the store.
	_ = c.store.RecordLifecycleEvent("collector_drain", drainMs, "")
	_ = c.store.Close()
}

// RecordToolCall enqueues a tool call event. Fire-and-forget.
func (c *Client) RecordToolCall(ev ToolCallEvent) {
	if c == nil {
		return
	}
	c.coll.RecordToolCall(ev)
}

// RecordContextDelivery enqueues a context delivery event. Fire-and-forget.
func (c *Client) RecordContextDelivery(ev ContextDeliveryEvent) {
	if c == nil {
		return
	}
	c.coll.RecordContextDelivery(ev)
}

// RecordSessionEvent enqueues a session lifecycle event. Fire-and-forget.
// If sessionID is provided, it is used directly (preferred — avoids session ID
// collision). If empty, falls back to the legacy synthetic ID format for
// backward compatibility.
func (c *Client) RecordSessionEvent(agentID, projectID, eventType string) {
	if c == nil {
		return
	}
	sessionID := agentID + ":" + projectID + ":" + time.Now().UTC().Format("2006-01-02")
	c.coll.RecordSessionEvent(sessionID, agentID, projectID, eventType)
}

// RecordSessionEventWithID enqueues a session lifecycle event with an explicit
// session UUID from the main store. If sessionID is empty, the event is
// silently dropped — callers must resolve the session ID before calling.
func (c *Client) RecordSessionEventWithID(sessionID, agentID, projectID, eventType string) {
	if c == nil || sessionID == "" {
		return
	}
	c.coll.RecordSessionEvent(sessionID, agentID, projectID, eventType)
}

// RecordSessionEventFull enqueues a session lifecycle event with agent version
// (Bug 16 — DQ-C.6). If sessionID is empty, the event is silently dropped.
func (c *Client) RecordSessionEventFull(sessionID, agentID, projectID, eventType, agentVersion string) {
	if c == nil || sessionID == "" {
		return
	}
	c.coll.RecordSessionEventFull(sessionID, agentID, projectID, eventType, agentVersion)
}

// RecordOutcomeSignal enqueues an intent alignment outcome signal. Fire-and-forget.
func (c *Client) RecordOutcomeSignal(ev OutcomeSignalEvent) {
	if c == nil {
		return
	}
	c.coll.RecordOutcomeSignal(ev)
}

// RecordSessionModel records which model the agent declared in session_init (Option A).
// Fire-and-forget; errors are silently discarded.
func (c *Client) RecordSessionModel(agentID, projectID, model, provider string) {
	if c == nil || model == "" {
		return
	}
	sessionID := agentID + ":" + projectID + ":" + time.Now().UTC().Format("2006-01-02")
	c.coll.RecordSessionModel(sessionID, agentID, projectID, model, provider)
}

// RecordSessionModelWithID records the model with an explicit session UUID.
// If sessionID is empty, the event is silently dropped.
func (c *Client) RecordSessionModelWithID(sessionID, agentID, projectID, model, provider string) {
	if c == nil || model == "" || sessionID == "" {
		return
	}
	c.coll.RecordSessionModel(sessionID, agentID, projectID, model, provider)
}

// RecordBrainUsage enqueues a brain LLM inference event. Fire-and-forget.
// Used to track deterministic vs. Ollama call ratios in the brain enricher.
func (c *Client) RecordBrainUsage(ev BrainUsageEvent) {
	if c == nil {
		return
	}
	c.coll.RecordBrainUsage(ev)
}

// RecordAgentLLMUsage enqueues an agent-reported LLM usage event (Option B).
// Fire-and-forget.
func (c *Client) RecordAgentLLMUsage(ev AgentLLMUsageEvent) {
	if c == nil {
		return
	}
	c.coll.RecordAgentLLMUsage(ev)
}

// RecordParseEvent enqueues a per-file parse event. Fire-and-forget (P2-2).
func (c *Client) RecordParseEvent(ev ParseEvent) {
	if c == nil {
		return
	}
	c.coll.RecordParseEvent(ev)
}

// RecordReparseEvent enqueues an incremental reparse event. Fire-and-forget (P2-3).
func (c *Client) RecordReparseEvent(ev ReparseEvent) {
	if c == nil {
		return
	}
	c.coll.RecordReparseEvent(ev)
}

// RecordGraphSnapshot enqueues a graph topology snapshot. Fire-and-forget (P2-7).
func (c *Client) RecordGraphSnapshot(ev GraphSnapshotEvent) {
	if c == nil {
		return
	}
	c.coll.RecordGraphSnapshot(ev)
}

// RecordEmbeddingEvent enqueues an embedding batch event. Fire-and-forget (P2-6).
func (c *Client) RecordEmbeddingEvent(ev EmbeddingEvent) {
	if c == nil {
		return
	}
	c.coll.RecordEmbeddingEvent(ev)
}

// RecordIndexEvent enqueues a full-index completion event. Fire-and-forget (P2-8).
func (c *Client) RecordIndexEvent(ev IndexEvent) {
	if c == nil {
		return
	}
	c.coll.RecordIndexEvent(ev)
}

// RecordGuardEvent enqueues a loop-guard or rate-limiter block event. Fire-and-forget (P3-2/P3-3).
func (c *Client) RecordGuardEvent(ev GuardEvent) {
	if c == nil {
		return
	}
	c.coll.RecordGuardEvent(ev)
}

// RecordMemoryOp enqueues a recall hit/miss or memory write event. Fire-and-forget (P3-4).
func (c *Client) RecordMemoryOp(ev MemoryOperationEvent) {
	if c == nil {
		return
	}
	c.coll.RecordMemoryOp(ev)
}

// RecordValidationEvent enqueues a validate_plan or verify_implementation outcome event. Fire-and-forget (P3-5).
func (c *Client) RecordValidationEvent(ev ValidationEvent) {
	if c == nil {
		return
	}
	c.coll.RecordValidationEvent(ev)
}

// RecordSearchEvent enqueues a search or find_entity analytics event. Fire-and-forget (P4-8).
func (c *Client) RecordSearchEvent(ev SearchEvent) {
	if c == nil {
		return
	}
	c.coll.RecordSearchEvent(ev)
}

// RecordConfigReload enqueues a configuration hot-reload event. Fire-and-forget (Bug 68 — COV-9).
func (c *Client) RecordConfigReload(ev ConfigReloadEvent) {
	if c == nil {
		return
	}
	c.coll.RecordConfigReload(ev)
}

// RecordPersistenceEvent enqueues a store write duration/size event. Fire-and-forget (Bug 69 — COV-12).
func (c *Client) RecordPersistenceEvent(ev PersistenceEvent) {
	if c == nil {
		return
	}
	c.coll.RecordPersistenceEvent(ev)
}

// RecordEnrichmentEvent enqueues a code enrichment pass outcome event. Fire-and-forget (Bug 70 — COV-Subsys).
func (c *Client) RecordEnrichmentEvent(ev EnrichmentEvent) {
	if c == nil {
		return
	}
	c.coll.RecordEnrichmentEvent(ev)
}

// RecordRuleEvalEvent enqueues an architecture rule evaluation event. Fire-and-forget (Bug 71 — COV-Subsys).
func (c *Client) RecordRuleEvalEvent(ev RuleEvalEvent) {
	if c == nil {
		return
	}
	c.coll.RecordRuleEvalEvent(ev)
}

// RecordFederationEvent enqueues a federation detection event. Fire-and-forget (P5 — COV-8).
func (c *Client) RecordFederationEvent(ev FederationDetectEvent) {
	if c == nil {
		return
	}
	c.coll.RecordFederationEvent(ev)
}

// RecordSkillExecution enqueues a skill execution event. Fire-and-forget (P5 — COV-15).
func (c *Client) RecordSkillExecution(ev SkillExecutionEvent) {
	if c == nil {
		return
	}
	c.coll.RecordSkillExecution(ev)
}

// RecordToolSequenceEntry enqueues a tool call sequence entry. Fire-and-forget (P5 — SA-C1).
func (c *Client) RecordToolSequenceEntry(sessionID, toolName string, position int, success bool) {
	if c == nil || sessionID == "" {
		return
	}
	c.coll.RecordToolSequenceEntry(sessionID, toolName, position, success)
}

// RecordHeartbeat enqueues a system uptime heartbeat tick. Fire-and-forget (P5 — ROI-E1).
func (c *Client) RecordHeartbeat() {
	if c == nil {
		return
	}
	c.coll.RecordHeartbeat()
}

// InsertSessionEffectiveness records a computed session effectiveness row. Fire-and-forget (P5 — Item 13).
func (c *Client) InsertSessionEffectiveness(ev SessionEffectiveness) {
	if c == nil {
		return
	}
	_ = c.store.InsertSessionEffectiveness(ev)
}

// InsertDeliveryOutcome records a delivery-to-outcome linkage. Fire-and-forget (P5 — Item 11).
func (c *Client) InsertDeliveryOutcome(deliveryID int, sessionID, entity, signalType string, toolsBetween int, success bool) {
	if c == nil {
		return
	}
	_ = c.store.InsertDeliveryOutcome(deliveryID, sessionID, entity, signalType, toolsBetween, success)
}

// SetSessionTermination records termination reason and duration. Fire-and-forget (P5 — Item 32).
func (c *Client) SetSessionTermination(sessionID, reason string) {
	if c == nil || sessionID == "" {
		return
	}
	_ = c.store.SetSessionTermination(sessionID, reason)
}

// UpdateEntityQualityScore recomputes the running quality score for an entity. Fire-and-forget (P5 — Item 10).
func (c *Client) UpdateEntityQualityScore(entity, projectID string) {
	if c == nil {
		return
	}
	c.store.UpdateEntityQualityScore(entity, projectID)
}

// UpdateRecallChannelStats recomputes recall channel attribution weights. Fire-and-forget (P5 — Item 12).
func (c *Client) UpdateRecallChannelStats(projectID string) {
	if c == nil {
		return
	}
	c.store.UpdateRecallChannelStats(projectID)
}

// GetToolSequences returns tool call sequences for a session (P5 — SA-C1).
func (c *Client) GetToolSequences(sessionID string) []ToolSequenceEntry {
	if c == nil || sessionID == "" {
		return nil
	}
	return c.store.GetToolSequences(sessionID)
}

// GetEntityQualityScores returns entity context quality scores (P5 — Item 10).
func (c *Client) GetEntityQualityScores(projectID string, limit int) []EntityQuality {
	if c == nil {
		return nil
	}
	return c.store.GetEntityQualityScores(projectID, limit)
}

// GetDeliveryOutcomes returns delivery-to-outcome linkages (P5 — Item 11).
func (c *Client) GetDeliveryOutcomes(days int) []DeliveryOutcome {
	if c == nil {
		return nil
	}
	if days <= 0 {
		days = 7
	}
	return c.store.GetDeliveryOutcomes(days)
}

// GetRecallChannelWeights returns recall channel attribution weights (P5 — Item 12).
func (c *Client) GetRecallChannelWeights(projectID string) map[string]float64 {
	if c == nil {
		return nil
	}
	return c.store.GetRecallChannelWeights(projectID)
}

// GetSessionEffectiveness returns a specific session's effectiveness record (P5 — Item 13).
func (c *Client) GetSessionEffectiveness(sessionID string) *SessionEffectiveness {
	if c == nil || sessionID == "" {
		return nil
	}
	return c.store.GetSessionEffectivenessP5(sessionID)
}

// GetRecentEffectivenessTrend returns daily effectiveness averages (P5 — Item 13).
func (c *Client) GetRecentEffectivenessTrend(days int, agentID string) []DailyEffectiveness {
	if c == nil {
		return nil
	}
	if days <= 0 {
		days = 14
	}
	return c.store.GetRecentEffectivenessTrend(days, agentID)
}

// GetAgentLearningCurve returns weekly efficiency trend for an agent (P5 — Item 14).
func (c *Client) GetAgentLearningCurve(agentID string, weeks int) []WeeklyEfficiency {
	if c == nil || agentID == "" {
		return nil
	}
	if weeks <= 0 {
		weeks = 12
	}
	return c.store.GetAgentLearningCurve(agentID, weeks)
}

// GetImplementationQualityGap returns pre/post implementation quality gap (P5 — Item 15).
func (c *Client) GetImplementationQualityGap(days int) float64 {
	if c == nil {
		return 0
	}
	if days <= 0 {
		days = 30
	}
	return c.store.GetImplementationQualityGap(days)
}

// GetBrainEnrichmentUplift returns the enrichment quality uplift ratio (P5 — Item 16).
func (c *Client) GetBrainEnrichmentUplift(days int) float64 {
	if c == nil {
		return 0
	}
	if days <= 0 {
		days = 30
	}
	return c.store.GetBrainEnrichmentUplift(days)
}

// GetMemoryFailurePreventionRate returns the memory failure prevention rate (P5 — Item 17).
func (c *Client) GetMemoryFailurePreventionRate(days int) float64 {
	if c == nil {
		return 0
	}
	if days <= 0 {
		days = 30
	}
	return c.store.GetMemoryFailurePreventionRate(days)
}

// GetDecayEffectiveness returns knowledge decay hit rate buckets (P5 — Item 18).
func (c *Client) GetDecayEffectiveness(days int) DecayStats {
	if c == nil {
		return DecayStats{}
	}
	if days <= 0 {
		days = 90
	}
	return c.store.GetDecayEffectiveness(days)
}

// GetMonthlyROIReport returns the monthly ROI report (P5 — Item 20).
func (c *Client) GetMonthlyROIReport(year, month int) *MonthlyROI {
	if c == nil {
		return nil
	}
	return c.store.GetMonthlyROIReport(year, month)
}

// GetGraphFreshnessScore returns the graph freshness score for today (P5 — Item 21).
func (c *Client) GetGraphFreshnessScore() float64 {
	if c == nil {
		return 0
	}
	today := time.Now().UTC().Format("2006-01-02")
	return c.store.GetGraphFreshnessScoreP5(today)
}

// GetTokenSavingsByIntent returns per-intent token savings (P5 — Item 22).
func (c *Client) GetTokenSavingsByIntent(days int) map[string]int64 {
	if c == nil {
		return nil
	}
	if days <= 0 {
		days = 7
	}
	return c.store.GetTokenSavingsByIntent(days)
}

// GetToolsPerSessionPercentiles returns tools-per-session percentile distribution (P5 — Item 45).
func (c *Client) GetToolsPerSessionPercentiles(days int) SessionPercentiles {
	if c == nil {
		return SessionPercentiles{}
	}
	if days <= 0 {
		days = 30
	}
	sp := SessionPercentiles{}
	sp.ToolsP50, sp.ToolsP95, sp.ToolsP99 = c.store.GetToolsPerSessionPercentiles(days)
	sp.CallsP50, sp.CallsP95, sp.CallsP99 = c.store.GetCallsPerSessionPercentiles(days)
	return sp
}

// GetSkillExecutionStatsP5 returns skill execution stats for the last N days (P5 — COV-15).
func (c *Client) GetSkillExecutionStatsP5(days int) []SkillStat {
	if c == nil {
		return nil
	}
	if days <= 0 {
		days = 30
	}
	return c.store.GetSkillExecutionStatsP5(days)
}

// GetMostRecentDeliveryID returns the most recent context delivery ID for an entity (P5 — Item 11).
func (c *Client) GetMostRecentDeliveryID(entity string) int {
	if c == nil || entity == "" {
		return 0
	}
	return c.store.GetMostRecentDeliveryIDByEntity(entity)
}

// WriteErrors returns the total collector write errors (P5 — DQ-Integrity.1 / Item 35).
func (c *Client) WriteErrors() int64 {
	if c == nil {
		return 0
	}
	return c.coll.WriteErrors()
}

// GetHealthSnapshot returns a lightweight health check payload (P5 — Item 23).
func (c *Client) GetHealthSnapshot() map[string]interface{} {
	if c == nil {
		return map[string]interface{}{"status": "unavailable"}
	}
	today := time.Now().UTC().Format("2006-01-02")
	return map[string]interface{}{
		"status":               "ok",
		"events_today":         c.store.CountEventsToday(),
		"last_rollup":          c.store.GetLastRollupTime(),
		"collector_errors":     c.coll.WriteErrors(),
		"collector_drop_rate":  c.coll.DropRate(),
		"collector_hwm":        c.coll.HighWaterMark(),
		"collector_buf_len":    c.coll.Len(),
		"db_size_bytes":        c.store.DBSizeBytesP5(c.dbPath),
		"errors_today":         c.store.GetErrorsToday(),
		"graph_freshness":      c.store.GetGraphFreshnessScoreP5(today),
		"bg_enqueued":          c.bgEnqueued.Load(),
		"bg_dropped":           c.bgDropped.Load(),
		"bg_panics":            c.bgPanics.Load(),
	}
}

// GetSummaryForProject returns aggregated analytics for the last N days filtered to projectID.
// Returns a PulseSummary with only Summary populated (no per-tool/agent breakdowns).
func (c *Client) GetSummaryForProject(days int, projectID string) *PulseSummary {
	if c == nil {
		return &PulseSummary{Days: days}
	}
	if days <= 0 {
		days = 7
	}
	sum, err := c.store.GetSummaryForProject(days, projectID)
	if err != nil {
		sum = &pulsestore.Summary{}
	}
	return &PulseSummary{Days: days, Summary: sum}
}

// GetWeekOverWeek returns week-over-week metric comparisons from daily_rollups.
// Returns nil on error (best-effort).
func (c *Client) GetWeekOverWeek() *pulsestore.WoWComparison {
	if c == nil {
		return nil
	}
	wow, err := c.store.GetWeekOverWeek()
	if err != nil {
		return nil
	}
	return wow
}

// RecordBackgroundWorkerEnqueue increments the bgEnqueued counter (P2-5).
func (c *Client) RecordBackgroundWorkerEnqueue() {
	if c == nil {
		return
	}
	c.bgEnqueued.Add(1)
}

// RecordBackgroundWorkerDrop increments the bgDropped counter (P2-5).
func (c *Client) RecordBackgroundWorkerDrop() {
	if c == nil {
		return
	}
	c.bgDropped.Add(1)
}

// RecordBackgroundWorkerPanic increments the bgPanics counter (P2-5).
func (c *Client) RecordBackgroundWorkerPanic() {
	if c == nil {
		return
	}
	c.bgPanics.Add(1)
}

// FetchEffectiveness returns per-entity effectiveness scores from the local store.
// Returns nil if no data is available or the store returns an error.
func (c *Client) FetchEffectiveness(projectID string, minSignals int) []EntityEffectiveness {
	if c == nil {
		return nil
	}
	results, err := c.store.GetEffectiveness(projectID, minSignals)
	if err != nil || len(results) == 0 {
		return nil
	}
	return results
}

// PulseSummary is the response shape for GET /api/admin/pulse/summary.
type PulseSummary struct {
	Days        int                          `json:"days"`
	Summary     *pulsestore.Summary          `json:"summary"`
	Tools       []pulsestore.ToolStats       `json:"tools"`
	Agents      []pulsestore.AgentStats      `json:"agents"`
	Timeline    []pulsestore.TimelinePoint   `json:"timeline"`
	TopEntities []pulsestore.EntityCount     `json:"top_entities"`
	Insights    []EntityEffectiveness        `json:"insights"`
	LLMStats    []pulsestore.AgentLLMStats   `json:"llm_stats"`
	RuleHits    []pulsestore.RuleHitStat     `json:"rule_hits"`
	// Phase 4 analytics extensions.
	TopEntitiesBySavings    []pulsestore.EntitySavings        `json:"top_entities_by_savings,omitempty"`
	CostSavingsByModel      []pulsestore.ModelCostStat        `json:"cost_savings_by_model,omitempty"`
	AgentTokenEfficiency    []pulsestore.AgentEfficiencyStat  `json:"agent_token_efficiency,omitempty"`
	ContextReuseRate        float64                           `json:"context_reuse_rate,omitempty"`
	EngagementScore         float64                           `json:"engagement_score,omitempty"`
	OnboardingLatencyMs     float64                           `json:"onboarding_latency_ms,omitempty"`
	MultiSessionCampaigns   int                               `json:"multi_session_campaigns,omitempty"`
	AgentToolPreferences    []pulsestore.AgentToolPref        `json:"agent_tool_preferences,omitempty"`
	AgentEfficiencyScores   []pulsestore.AgentEfficiency      `json:"agent_efficiency_scores,omitempty"`
	ModelComparison         []pulsestore.ModelComparisonStat  `json:"model_comparison,omitempty"`
	ErrorRecoveryPatterns   []pulsestore.ErrorRecovery        `json:"error_recovery_patterns,omitempty"`
	ToolPairCorrelation     []pulsestore.ToolPairStat         `json:"tool_pair_correlation,omitempty"`
	DiscoveryToolEffective  float64                           `json:"discovery_tool_effectiveness,omitempty"`
	SkillExecutionStats     []pulsestore.SkillStat            `json:"skill_execution_stats,omitempty"`
	MemoryTypeDistribution  map[string]int                    `json:"memory_type_distribution,omitempty"`
	CancellationReasons     []pulsestore.CancellationStat     `json:"cancellation_reasons,omitempty"`
	PlanComplexityVsOutcome []pulsestore.PlanComplexityStat   `json:"plan_complexity_vs_outcome,omitempty"`
	BlockedTaskCount        int                               `json:"blocked_task_count,omitempty"`
	MessageVolumeStats      *pulsestore.MessageVolumeStat     `json:"message_volume_stats,omitempty"`
	CrossProjectQueryVolume int                               `json:"cross_project_query_volume,omitempty"`
	ApprovalGateUsage       int                               `json:"approval_gate_usage,omitempty"`
	ModelEfficiencyComparison []pulsestore.ModelEfficiency    `json:"model_efficiency_comparison,omitempty"`
	ProjectEfficiencyComparison []pulsestore.ProjectEfficiency `json:"project_efficiency_comparison,omitempty"`
	HypotheticalCostUSD     float64                           `json:"hypothetical_cost_usd,omitempty"`
	WithSynapsesCostUSD     float64                           `json:"with_synapses_cost_usd,omitempty"`
	LatencyP50Ms            float64                           `json:"latency_p50_ms,omitempty"`
	LatencyP95Ms            float64                           `json:"latency_p95_ms,omitempty"`
	LatencyP99Ms            float64                           `json:"latency_p99_ms,omitempty"`
	ContextPrecision        float64                           `json:"context_precision,omitempty"`
	ContextRecall           float64                           `json:"context_recall,omitempty"`
	ContextF1               float64                           `json:"context_f1,omitempty"`
	BrainCostStats          []pulsestore.BrainCostStat        `json:"brain_cost_stats,omitempty"`
	SearchStats             *pulsestore.SearchStats           `json:"search_stats,omitempty"`
	GraphSnapshot           *pulsestore.GraphSnapshotRow      `json:"graph_snapshot,omitempty"`
	AvgTaskDurationMs       float64                           `json:"avg_task_duration_ms,omitempty"`
	// Phase 5: self-refining loop, coverage & observability.
	EntityQualityScores     []types.EntityQuality             `json:"entity_quality_scores,omitempty"`
	RecallChannelWeights    map[string]float64                `json:"recall_channel_weights,omitempty"`
	EffectivenessTrend      []types.DailyEffectiveness        `json:"effectiveness_trend,omitempty"`
	ImplementationQualityGap float64                          `json:"implementation_quality_gap,omitempty"`
	BrainEnrichmentUplift   float64                           `json:"brain_enrichment_uplift,omitempty"`
	MemoryFailurePrevRate   float64                           `json:"memory_failure_prevention_rate,omitempty"`
	DecayEffectiveness      types.DecayStats                  `json:"decay_effectiveness,omitempty"`
	GraphFreshnessScoreP5   float64                           `json:"graph_freshness_score_p5,omitempty"`
	TokenSavingsByIntent    map[string]int64                  `json:"token_savings_by_intent,omitempty"`
	SessionPercentiles      types.SessionPercentiles          `json:"session_percentiles,omitempty"`
	SkillStatsP5            []types.SkillStat                 `json:"skill_stats_p5,omitempty"`
	CollectorWriteErrors    int64                             `json:"collector_write_errors,omitempty"`
	CrossSessionReuseRate   float64                           `json:"cross_session_reuse_rate,omitempty"`
	ConcurrentAgentsMax     int                               `json:"concurrent_agents_max,omitempty"`
}

// GetSummary returns aggregated analytics for the last N days including per-tool,
// per-agent, 14-day timeline, and top queried entities.
// Returns nil summary and empty slices if pulse is unavailable.
func (c *Client) GetSummary(days int) *PulseSummary {
	if c == nil {
		return &PulseSummary{Days: days}
	}
	if days <= 0 {
		days = 7
	}
	sum, err := c.store.GetSummary(days)
	if err != nil {
		sum = &pulsestore.Summary{}
	}
	tools, err := c.store.GetToolStats(days)
	if err != nil {
		tools = nil
	}
	agents, err := c.store.GetAgentStats(days)
	if err != nil {
		agents = nil
	}
	// Bug 3 — STO-D.1.3: use the requested window directly instead of a forced minimum.
	timeline, err := c.store.GetTimeline(days)
	if err != nil {
		timeline = nil
	}
	topEntities, err := c.store.TopEntities(days, 12)
	if err != nil {
		topEntities = nil
	}
	insights := c.FetchEffectiveness("", 2)
	llmStats, err := c.store.GetAgentLLMStats(days)
	if err != nil {
		llmStats = nil
	}
	ruleHits, err := c.store.GetRuleHitDistribution(days, 20)
	if err != nil {
		ruleHits = nil
	}

	// Phase 4 analytics.
	topEntitiesBySavings, _ := c.store.TopEntitiesBySavings(days, 10)
	costByModel, _ := c.store.GetCostSavingsByModel(days)
	agentTokenEff, _ := c.store.GetAgentTokenEfficiency(days)
	agentToolPrefs, _ := c.store.GetAgentToolPreferences(days)
	agentEffScores, _ := c.store.GetAgentEfficiencyScores(days)
	modelCmp, _ := c.store.GetModelComparison(days)
	errRecovery, _ := c.store.GetErrorRecoveryPatterns(days)
	toolPairs, _ := c.store.GetToolPairCorrelation(days)
	skillStats, _ := c.store.GetSkillExecutionStats(days)
	memTypeDist, _ := c.store.GetMemoryTypeDistribution(days)
	cancReasons, _ := c.store.GetCancellationReasons(days)
	planCmplx, _ := c.store.GetPlanComplexityVsOutcome(days)
	msgVol, _ := c.store.GetMessageVolumeStats(days)
	modelEff, _ := c.store.GetModelEfficiencyComparison(days)
	projectEff, _ := c.store.GetProjectEfficiencyComparison(days)
	p50, p95, p99 := c.store.GetLatencyPercentiles(days)
	brainCosts, _ := c.store.GetBrainCostStats(days)
	searchStats, _ := c.store.GetSearchStats(days)
	graphSnap, _ := c.store.GetLatestGraphSnapshot()

	// Phase 5: self-refining & observability queries.
	today := time.Now().UTC().Format("2006-01-02")
	entityQuality := c.store.GetEntityQualityScores("", 20)
	recallWeights := c.store.GetRecallChannelWeights("")
	effTrend := c.store.GetRecentEffectivenessTrend(14, "")
	skillStatsP5 := c.store.GetSkillExecutionStatsP5(days)
	tokensByIntent := c.store.GetTokenSavingsByIntent(days)
	var sessPct types.SessionPercentiles
	sessPct.ToolsP50, sessPct.ToolsP95, sessPct.ToolsP99 = c.store.GetToolsPerSessionPercentiles(days)
	sessPct.CallsP50, sessPct.CallsP95, sessPct.CallsP99 = c.store.GetCallsPerSessionPercentiles(days)

	return &PulseSummary{
		Days:        days,
		Summary:     sum,
		Tools:       tools,
		Agents:      agents,
		Timeline:    timeline,
		TopEntities: topEntities,
		Insights:    insights,
		LLMStats:    llmStats,
		RuleHits:    ruleHits,
		// Phase 4.
		TopEntitiesBySavings:        toEntitySavings(topEntitiesBySavings),
		CostSavingsByModel:          costByModel,
		AgentTokenEfficiency:        agentTokenEff,
		ContextReuseRate:            c.store.GetContextReuseRate(days),
		EngagementScore:             c.store.GetEngagementScore(days),
		OnboardingLatencyMs:         c.store.GetOnboardingLatencyMs(days),
		MultiSessionCampaigns:       c.store.GetMultiSessionCampaigns(days),
		AgentToolPreferences:        agentToolPrefs,
		AgentEfficiencyScores:       agentEffScores,
		ModelComparison:             modelCmp,
		ErrorRecoveryPatterns:       errRecovery,
		ToolPairCorrelation:         toolPairs,
		DiscoveryToolEffective:      c.store.GetDiscoveryToolEffectiveness(days),
		SkillExecutionStats:         skillStats,
		MemoryTypeDistribution:      memTypeDist,
		CancellationReasons:         cancReasons,
		PlanComplexityVsOutcome:     planCmplx,
		BlockedTaskCount:            c.store.GetBlockedTaskCount(days),
		MessageVolumeStats:          msgVol,
		CrossProjectQueryVolume:     c.store.GetCrossProjectQueryVolume(days),
		ApprovalGateUsage:           c.store.GetApprovalGateUsage(days),
		ModelEfficiencyComparison:   modelEff,
		ProjectEfficiencyComparison: projectEff,
		HypotheticalCostUSD:         c.store.GetHypotheticalCostUSD(days),
		WithSynapsesCostUSD:         c.store.GetWithSynapsesCostUSD(days),
		LatencyP50Ms:                p50,
		LatencyP95Ms:                p95,
		LatencyP99Ms:                p99,
		ContextPrecision:            c.store.GetContextPrecision(days),
		ContextRecall:               c.store.GetContextRecall(days),
		ContextF1:                   c.store.GetContextF1(days),
		BrainCostStats:              brainCosts,
		SearchStats:                 searchStats,
		GraphSnapshot:               graphSnap,
		AvgTaskDurationMs:           c.store.GetAvgTaskDuration(days),
		// Phase 5.
		EntityQualityScores:      entityQuality,
		RecallChannelWeights:     recallWeights,
		EffectivenessTrend:       effTrend,
		ImplementationQualityGap: c.store.GetImplementationQualityGap(days),
		BrainEnrichmentUplift:    c.store.GetBrainEnrichmentUplift(days),
		MemoryFailurePrevRate:    c.store.GetMemoryFailurePreventionRate(days),
		DecayEffectiveness:       c.store.GetDecayEffectiveness(90),
		GraphFreshnessScoreP5:    c.store.GetGraphFreshnessScoreP5(today),
		TokenSavingsByIntent:     tokensByIntent,
		SessionPercentiles:       sessPct,
		SkillStatsP5:             skillStatsP5,
		CollectorWriteErrors:     c.coll.WriteErrors(),
		CrossSessionReuseRate:    c.store.GetCrossSessionReuseRate(days),
		ConcurrentAgentsMax:      c.store.GetConcurrentAgentsMax(today),
	}
}

// toEntitySavings is a nil-safe pass-through (the types are identical; this
// ensures the nil slice becomes nil rather than an empty slice).
func toEntitySavings(in []pulsestore.EntitySavings) []pulsestore.EntitySavings {
	return in
}

// GetLifetimeSummary returns aggregated analytics across all time.
// Returns nil if pulse is unavailable.
func (c *Client) GetLifetimeSummary() *PulseSummary {
	if c == nil {
		return &PulseSummary{Days: 0}
	}
	sum, err := c.store.GetLifetimeSummary()
	if err != nil {
		sum = &pulsestore.Summary{}
	}
	return &PulseSummary{
		Days:    0, // 0 means "all time"
		Summary: sum,
	}
}

// GetFirstContextRightRate returns the fraction of context deliveries that
// did not require correction. Returns 1.0 if no data is available.
func (c *Client) GetFirstContextRightRate(days int) float64 {
	if c == nil {
		return 1.0
	}
	if days <= 0 {
		days = 7
	}
	rate, err := c.store.GetFirstContextRightRate(days)
	if err != nil {
		return 1.0
	}
	return rate
}

// GetToolTimeline returns per-tool call counts across daily data points (Bug 54).
func (c *Client) GetToolTimeline(toolName string, days int) []pulsestore.ToolTimelinePoint {
	if c == nil {
		return nil
	}
	if days <= 0 {
		days = 7
	}
	pts, err := c.store.GetToolTimeline(toolName, days)
	if err != nil {
		return nil
	}
	return pts
}

// GetSessionDetail returns full analytics detail for a specific session (Bug 55).
func (c *Client) GetSessionDetail(sessionID string) *pulsestore.SessionDetail {
	if c == nil || sessionID == "" {
		return nil
	}
	detail, err := c.store.GetSessionDetail(sessionID)
	if err != nil {
		return nil
	}
	return detail
}

// ExportRawData returns a raw data export for the last N days (Bug 56).
func (c *Client) ExportRawData(days int) *pulsestore.ExportData {
	if c == nil {
		return nil
	}
	if days <= 0 {
		days = 7
	}
	data, err := c.store.ExportRawData(days)
	if err != nil {
		return nil
	}
	return data
}

// GetLatestGraphSnapshot returns the most recent graph topology snapshot (P4-7).
func (c *Client) GetLatestGraphSnapshot() *pulsestore.GraphSnapshotRow {
	if c == nil {
		return nil
	}
	snap, err := c.store.GetLatestGraphSnapshot()
	if err != nil {
		return nil
	}
	return snap
}

// GetSearchStats returns aggregated search analytics for the last N days (P4-8).
func (c *Client) GetSearchStats(days int) *pulsestore.SearchStats {
	if c == nil {
		return nil
	}
	if days <= 0 {
		days = 7
	}
	stats, err := c.store.GetSearchStats(days)
	if err != nil {
		return nil
	}
	return stats
}

// GetBrainCostStats returns per-model brain LLM cost breakdown (P4-1).
func (c *Client) GetBrainCostStats(days int) []pulsestore.BrainCostStat {
	if c == nil {
		return nil
	}
	if days <= 0 {
		days = 7
	}
	stats, err := c.store.GetBrainCostStats(days)
	if err != nil {
		return nil
	}
	return stats
}

// GetToolStatsRaw returns raw per-tool stats for the last N days (P4-1).
func (c *Client) GetToolStatsRaw(days int) []pulsestore.ToolStats {
	if c == nil {
		return nil
	}
	if days <= 0 {
		days = 7
	}
	stats, err := c.store.GetToolStats(days)
	if err != nil {
		return nil
	}
	return stats
}

// GetTimelineRaw returns the daily timeline for the last N days (P4-1).
func (c *Client) GetTimelineRaw(days int) []pulsestore.TimelinePoint {
	if c == nil {
		return nil
	}
	if days <= 0 {
		days = 7
	}
	pts, err := c.store.GetTimeline(days)
	if err != nil {
		return nil
	}
	return pts
}

// RecordLifecycleEvent records a daemon lifecycle event (startup, shutdown, drain).
// Fire-and-forget; errors are silently discarded.
func (c *Client) RecordLifecycleEvent(eventType string, valueMs float64, projectID string) {
	if c == nil {
		return
	}
	_ = c.store.RecordLifecycleEvent(eventType, valueMs, projectID)
}

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

// Client is the in-process analytics collector. It replaces the HTTP sidecar.
// Create with New; call Close when the daemon shuts down.
type Client struct {
	store *pulsestore.Store
	coll  *collector.Collector
	agg   *aggregator.Aggregator
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

	agg := aggregator.New(st, 3600)
	agg.Start()

	return &Client{store: st, coll: coll, agg: agg}, nil
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
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.agg.Stop()
	c.coll.Stop()
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

// GetBackgroundWorkerStats returns enqueued, dropped, and panic counts (P2-5).
func (c *Client) GetBackgroundWorkerStats() (enqueued, dropped, panics int64) {
	if c == nil {
		return 0, 0, 0
	}
	return c.bgEnqueued.Load(), c.bgDropped.Load(), c.bgPanics.Load()
}

// GetCollectorStats returns the collector's event-drop count and high-water mark (P2-17/P2-19).
// Returns zeros if pulse is unavailable.
func (c *Client) GetCollectorStats() (dropped, hwm int64) {
	if c == nil {
		return 0, 0
	}
	return c.coll.Dropped(), c.coll.HighWaterMark()
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
	// Always use 14 days for the timeline chart regardless of the summary period.
	timelineDays := 14
	if days > 14 {
		timelineDays = days
	}
	timeline, err := c.store.GetTimeline(timelineDays)
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
	return &PulseSummary{
		Days:        days,
		Summary:     sum,
		Tools:       tools,
		Agents:      agents,
		Timeline:    timeline,
		TopEntities: topEntities,
		Insights:    insights,
		LLMStats:    llmStats,
	}
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

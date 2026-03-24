// Package brain provides in-process access to the Thinking Brain.
// Previously a separate HTTP sidecar (synapses-intelligence), the brain is now
// embedded directly so no external process or port is required.
//
// All public methods are fail-silent: errors are silently discarded so that
// brain failures never degrade the MCP hot path. The graph-only path always works.
package brain

import (
	"context"
	"io"

	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
)

// Client wraps the in-process Brain implementation. It exposes the same method
// signatures as the former HTTP client so all callers compile without changes.
// Create with NewInProcess; always non-nil (uses NullBrain on failure).
//
// Background scheduling: when the brain is enabled, Client creates a SystemPulse
// and a Scheduler. Low-priority background tasks (Ingest) are submitted to the
// Scheduler as P2 tasks and executed by the drain goroutine only when system health
// is Green. High-priority P0 tasks (BuildContextPacket, ExplainViolation) check
// ShouldDegrade() before invoking the LLM to fast-fail under resource pressure.
type Client struct {
	brain     Brain
	scheduler *Scheduler
	pulse     *SystemPulse // owned by Client; nil when brain is disabled
}

// NewInProcess creates a Client backed by an in-process Brain. If cfg is nil or
// cfg.Enabled is false, returns a Client wrapping NullBrain (all methods return
// zero values). Never returns nil.
//
// When enabled, NewInProcess starts a SystemPulse (health monitor) and a Scheduler
// (priority task queue). Both are stopped when Close() is called.
func NewInProcess(cfg *brainconfig.BrainConfig) *Client {
	if cfg == nil || !cfg.Enabled {
		// NullBrain path: scheduler with nil pulse runs tasks immediately (no-op).
		return &Client{
			brain:     &NullBrain{},
			scheduler: NewScheduler(nil),
		}
	}

	// Start system health monitoring so the scheduler can make health-aware
	// decisions about when to run P1/P2 tasks.
	pulse := NewSystemPulse()
	pulse.Start()

	sched := NewScheduler(pulse)
	sched.Start()

	return &Client{
		brain:     New(*cfg),
		scheduler: sched,
		pulse:     pulse,
	}
}

// NewClient is a backward-compatible constructor kept for callers that still use
// NewClient(url, timeout). It now ignores both arguments and returns a NullBrain
// client. Callers should migrate to NewInProcess(cfg).
//
// Deprecated: use NewInProcess.
func NewClient(_ string, _ int) *Client {
	return &Client{
		brain:     &NullBrain{},
		scheduler: NewScheduler(nil),
	}
}

// HealthCheck returns ("ok", nil) when the brain is available, or an error when not.
func (c *Client) HealthCheck(_ context.Context) (string, error) {
	if c.brain.Available() {
		return c.brain.ModelName(), nil
	}
	return "", nil // NullBrain — brain disabled, not an error
}

// BuildContextPacket builds and returns an enriched context packet.
//
// Returns nil when:
//   - The brain is unavailable or returns an error.
//   - System health is Red, or Yellow with no model loaded (ShouldDegrade).
//     Callers fall back to raw Synapses context unchanged.
func (c *Client) BuildContextPacket(ctx context.Context, req ContextPacketRequest) *ContextPacket {
	// P0 degradation check: skip the LLM call if system is under memory pressure
	// and no model is already loaded. Returning nil is the documented fallback.
	if c.scheduler.ShouldDegrade() {
		return nil
	}
	pkt, err := c.brain.BuildContextPacket(ctx, req)
	if err != nil {
		return nil
	}
	return pkt
}

// Ingest submits a code node for semantic summarization.
//
// The request is enqueued as a P2 (IDLE priority) task via the Scheduler and
// executed by the background drain goroutine when system health is Green.
// Under Yellow or Red health, the task is deferred up to 15 minutes.
//
// The caller's ctx is intentionally not forwarded to the queued fn — the context
// may expire before the task is eligible to run. The queued fn uses a fresh
// background context so the LLM call succeeds when the drain goroutine fires.
func (c *Client) Ingest(_ context.Context, req IngestRequest) {
	// Build a stable dedup key: projectID + nodeID + task type.
	key := req.ProjectID + ":" + req.NodeID + ":ingest"
	c.scheduler.Submit(key, PriorityP2, func() {
		_, _ = c.brain.Ingest(context.Background(), req)
	})
}

// ExplainViolation returns (explanation, fix) for an architecture violation.
//
// Returns ("", "") when:
//   - The brain is unavailable.
//   - System health warrants degradation (ShouldDegrade returns true).
func (c *Client) ExplainViolation(ctx context.Context, req ViolationRequest) (string, string) {
	// P0 degradation check: the caller (validate_plan handler) has a fallback
	// rule-template message when explanation is empty.
	if c.scheduler.ShouldDegrade() {
		return "", ""
	}
	resp, err := c.brain.ExplainViolation(ctx, req)
	if err != nil {
		return "", ""
	}
	return resp.Explanation, resp.Fix
}

// GetSummary returns the cached summary for nodeID, or "" if not yet summarized.
func (c *Client) GetSummary(_ context.Context, nodeID string) string {
	return c.brain.Summary("", nodeID)
}

// Summary returns the brain-generated summary for a node, scoped by projectID.
// Implements the federation.BrainSummaryProvider interface.
func (c *Client) Summary(projectID, nodeID string) string {
	return c.brain.Summary(projectID, nodeID)
}

// Available reports whether the brain LLM backend is accessible.
// Implements the federation.BrainSummaryProvider interface.
func (c *Client) Available() bool {
	return c.brain.Available()
}

// Generate sends a prompt to the brain's LLM and returns the raw response.
// Returns ("", error) if brain is unavailable. Used for brain-enhanced
// drift summaries in the federation resolver.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	return c.brain.Generate(ctx, prompt)
}

// LogDecision records a reasoning decision. Fire-and-forget.
func (c *Client) LogDecision(ctx context.Context, req DecisionRequest) {
	_ = c.brain.LogDecision(ctx, req)
}

// SetPhase updates the active SDLC phase. Returns the updated SDLCConfig.
func (c *Client) SetPhase(_ context.Context, req SetPhaseRequest) (*SDLCConfig, error) {
	if err := c.brain.SetSDLCPhase(SDLCPhase(req.Phase), ""); err != nil {
		return nil, err
	}
	cfg := c.brain.GetSDLCConfig()
	return &cfg, nil
}

// UpsertADR creates or updates an ADR. Returns the stored ADR.
func (c *Client) UpsertADR(_ context.Context, req ADRRequest) (*ADR, error) {
	if err := c.brain.UpsertADR(req); err != nil {
		return nil, err
	}
	adr, err := c.brain.GetADR(req.ID)
	if err != nil {
		return nil, err
	}
	return &adr, nil
}

// GetADR retrieves an ADR by ID.
func (c *Client) GetADR(_ context.Context, id string) (*ADR, error) {
	adr, err := c.brain.GetADR(id)
	if err != nil {
		return nil, err
	}
	return &adr, nil
}

// GetADRs returns all ADRs, optionally filtered by file path.
func (c *Client) GetADRs(_ context.Context, fileFilter string) ([]ADR, error) {
	if fileFilter != "" {
		return c.brain.GetADRsForFile(fileFilter, 50)
	}
	return c.brain.AllADRs()
}

// BrainHealth returns structured per-tier health data for session_init.
// Returns nil if the underlying Brain does not implement BrainStatsProvider
// (e.g. NullBrain when brain is disabled).
func (c *Client) BrainHealth() map[string]interface{} {
	sp, ok := c.brain.(BrainStatsProvider)
	if !ok {
		return nil
	}
	stats := sp.BrainStats()

	// Gather circuit breaker state (optional — NullBrain won't have it).
	var tierStatus map[string]TierState
	if tp, ok := c.brain.(TierStatusProvider); ok {
		tierStatus = tp.TierStatus()
	}

	tiers := []string{"ingest", "enrich", "guardian", "orchestrate", "archivist", "context_builder"}
	tierMap := make(map[string]interface{}, len(tiers))

	for _, tier := range tiers {
		callsKey := tier + "_calls"
		successKey := tier + "_success"
		avgKey := tier + "_avg_ms"

		calls, _ := stats[callsKey].(int64)
		success, _ := stats[successKey].(int64)
		avgMS, _ := stats[avgKey].(int64)

		var successRate float64
		if calls > 0 {
			successRate = float64(success) / float64(calls)
		}

		circuit := "closed"
		if ts, ok := tierStatus[tier]; ok && ts.Open {
			circuit = "open"
		}

		tierMap[tier] = map[string]interface{}{
			"calls":        calls,
			"success_rate": successRate,
			"avg_ms":       avgMS,
			"circuit":      circuit,
		}
	}

	return map[string]interface{}{
		"model": c.brain.ModelName(),
		"tiers": tierMap,
	}
}

// Close shuts down the in-process brain, scheduler, and system pulse,
// releasing all associated resources.
func (c *Client) Close() {
	// Stop the scheduler first so no new tasks are dispatched after brain close.
	if c.scheduler != nil {
		c.scheduler.Stop()
	}
	// Stop the system pulse sampler.
	if c.pulse != nil {
		c.pulse.Stop()
	}
	// Close the brain (releases LLM client, SQLite store).
	if closer, ok := c.brain.(io.Closer); ok {
		_ = closer.Close()
	}
}

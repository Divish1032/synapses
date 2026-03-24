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
	"github.com/SynapsesOS/synapses/internal/brain/archivist"
)

// Client wraps the in-process Brain implementation. It exposes the same method
// signatures as the former HTTP client so all callers compile without changes.
// Create with NewInProcess; always non-nil (uses NullBrain on failure).
type Client struct {
	brain Brain
}

// NewInProcess creates a Client backed by an in-process Brain. If cfg is nil or
// cfg.Enabled is false, returns a Client wrapping NullBrain (all methods return
// zero values). Never returns nil.
func NewInProcess(cfg *brainconfig.BrainConfig) *Client {
	if cfg == nil || !cfg.Enabled {
		return &Client{brain: &NullBrain{}}
	}
	return &Client{brain: New(*cfg)}
}

// NewClient is a backward-compatible constructor kept for callers that still use
// NewClient(url, timeout). It now ignores both arguments and returns a NullBrain
// client. Callers should migrate to NewInProcess(cfg).
//
// Deprecated: use NewInProcess.
func NewClient(_ string, _ int) *Client {
	return &Client{brain: &NullBrain{}}
}

// HealthCheck returns ("ok", nil) when the brain is available, or an error when not.
func (c *Client) HealthCheck(_ context.Context) (string, error) {
	if c.brain.Available() {
		return c.brain.ModelName(), nil
	}
	return "", nil // NullBrain — brain disabled, not an error
}

// BuildContextPacket builds and returns an enriched context packet. Returns nil
// if the brain is unavailable or returns an error.
func (c *Client) BuildContextPacket(ctx context.Context, req ContextPacketRequest) *ContextPacket {
	pkt, err := c.brain.BuildContextPacket(ctx, req)
	if err != nil {
		return nil
	}
	return pkt
}

// Ingest submits a code node for summarization. Fire-and-forget.
func (c *Client) Ingest(ctx context.Context, req IngestRequest) {
	_, _ = c.brain.Ingest(ctx, req)
}

// ExplainViolation returns (explanation, fix) for an architecture violation.
// Returns ("", "") if the brain is unavailable.
func (c *Client) ExplainViolation(ctx context.Context, req ViolationRequest) (string, string) {
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

// Prune uses the Tier 0 LLM to extract core technical content from raw text
// (e.g. web pages, over-budget context packets), discarding boilerplate.
// Returns the original content unchanged if brain unavailable.
func (c *Client) Prune(ctx context.Context, content string) (string, error) {
	return c.brain.Prune(ctx, content)
}

// Memorize synthesizes a session transcript into persistent memory entries.
// Returns empty response (no error) when the Archivist LLM is unavailable.
func (c *Client) Memorize(ctx context.Context, req archivist.MemorizeRequest) (archivist.MemorizeResponse, error) {
	return c.brain.Memorize(ctx, req)
}

// SetQualityMode updates the active quality mode. Returns the updated SDLCConfig.
func (c *Client) SetQualityMode(_ context.Context, mode QualityMode) (*SDLCConfig, error) {
	if err := c.brain.SetQualityMode(mode, ""); err != nil {
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

// Close shuts down the in-process brain, releasing resources.
func (c *Client) Close() {
	if closer, ok := c.brain.(io.Closer); ok {
		_ = closer.Close()
	}
}

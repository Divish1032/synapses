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

// Client is the in-process analytics collector. It replaces the HTTP sidecar.
// Create with New; call Close when the daemon shuts down.
type Client struct {
	store *pulsestore.Store
	coll  *collector.Collector
	agg   *aggregator.Aggregator
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
func (c *Client) RecordSessionEvent(agentID, projectID, eventType string) {
	if c == nil {
		return
	}
	sessionID := agentID + ":" + time.Now().UTC().Format("2006-01-02")
	c.coll.RecordSessionEvent(sessionID, agentID, projectID, eventType)
}

// RecordOutcomeSignal enqueues an intent alignment outcome signal. Fire-and-forget.
func (c *Client) RecordOutcomeSignal(ev OutcomeSignalEvent) {
	if c == nil {
		return
	}
	c.coll.RecordOutcomeSignal(ev)
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

// Package pulse is a thin, fail-silent HTTP client for the synapses-pulse
// analytics sidecar. Every method is fire-and-forget: errors are silently
// discarded so that the absence of pulse never degrades the MCP hot path.
package pulse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ToolCallEvent is sent for every MCP tool invocation.
type ToolCallEvent struct {
	ToolName      string `json:"tool_name"`
	AgentID       string `json:"agent_id,omitempty"`
	ProjectID     string `json:"project_id,omitempty"`
	Entity        string `json:"entity,omitempty"`
	DurationMs    int64  `json:"duration_ms"`
	Success       bool   `json:"success"`
	ResponseBytes int    `json:"response_bytes"`
}

// ContextDeliveryEvent is sent for context-delivery tools (get_context,
// get_file_context, prepare_context) and carries the token savings data.
type ContextDeliveryEvent struct {
	ToolName       string `json:"tool_name"`
	AgentID        string `json:"agent_id,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
	Entity         string `json:"entity,omitempty"`
	File           string `json:"file,omitempty"`
	ResponseBytes  int    `json:"response_bytes"`
	ResponseTokens int    `json:"response_tokens"`
	BaselineTokens int    `json:"baseline_tokens"`
	NodesDelivered int    `json:"nodes_delivered"`
	NodesPruned    int    `json:"nodes_pruned"`
	EdgesDelivered int    `json:"edges_delivered"`
	Truncated      bool   `json:"truncated"`
	DurationMs     int64  `json:"duration_ms"`
	CacheHit       bool   `json:"cache_hit"`
	BrainEnriched  bool   `json:"brain_enriched"`
}

// SessionEvent is sent when an agent session starts or ends.
type SessionEvent struct {
	AgentID   string `json:"agent_id"`
	ProjectID string `json:"project_id,omitempty"`
	Event     string `json:"event"` // "start" | "end" | "task_done"
}

// OutcomeSignalEvent is sent for passive outcome signals (R29 — Intent Alignment Metrics).
// These signals correlate context delivery quality with downstream outcomes.
//
// SignalType values:
//   - "correction":  same entity queried twice in one session w/o file change  — first context was insufficient
//   - "escalation":  same entity queried 3+ times in one session — context wasn't sufficient upfront
//   - "replan":      create_plan called mid-session — context led to wrong direction
//   - "task_done":   task status set to "done" — positive outcome
//   - "task_cancelled": task status set to "cancelled" — negative outcome
type OutcomeSignalEvent struct {
	ProjectID  string `json:"project_id,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`
	Entity     string `json:"entity,omitempty"` // entity name that was queried (correction/escalation)
	SignalType string `json:"signal_type"`      // see above
	Count      int    `json:"count,omitempty"`  // repeat count for correction/escalation
}

// EntityEffectiveness is returned by GET /v1/effectiveness for a single entity.
type EntityEffectiveness struct {
	Entity     string  `json:"entity"`
	Score      float64 `json:"score"`       // 0.0–1.0; higher = context was helpful
	Signals    int     `json:"signals"`     // total signal count
	Positives  int     `json:"positives"`   // task_done signals
	Negatives  int     `json:"negatives"`   // correction + escalation + replan signals
	Suggestion string  `json:"suggestion"`  // human-readable improvement hint
}

// Client is a fail-silent HTTP client for the synapses-pulse sidecar.
type Client struct {
	baseURL string
	cli     *http.Client
}

// NewClient creates a Client targeting the given base URL. timeoutSec is the
// per-request HTTP timeout; pass 0 to use the default of 2 seconds.
func NewClient(baseURL string, timeoutSec int) *Client {
	if timeoutSec <= 0 {
		timeoutSec = 2
	}
	return &Client{
		baseURL: baseURL,
		cli:     &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
	}
}

// RecordToolCall sends a ToolCallEvent to pulse. All errors are silently discarded.
func (c *Client) RecordToolCall(ev ToolCallEvent) {
	c.post("/v1/ingest/tool-call", ev)
}

// RecordContextDelivery sends a ContextDeliveryEvent to pulse. All errors are silently discarded.
func (c *Client) RecordContextDelivery(ev ContextDeliveryEvent) {
	c.post("/v1/ingest/context-delivery", ev)
}

// RecordSessionEvent sends a SessionEvent to pulse. All errors are silently discarded.
func (c *Client) RecordSessionEvent(agentID, projectID, eventType string) {
	c.post("/v1/ingest/session-event", SessionEvent{AgentID: agentID, ProjectID: projectID, Event: eventType})
}

// RecordOutcomeSignal sends an OutcomeSignalEvent to pulse. All errors are silently discarded.
func (c *Client) RecordOutcomeSignal(ev OutcomeSignalEvent) {
	c.post("/v1/ingest/outcome-signal", ev)
}

// FetchEffectiveness fetches per-entity effectiveness scores from pulse.
// Returns nil if pulse is unavailable or returns no data.
func (c *Client) FetchEffectiveness(projectID string, minSignals int) []EntityEffectiveness {
	url := c.baseURL + "/v1/effectiveness?project_id=" + projectID
	if minSignals > 0 {
		url += fmt.Sprintf("&min_signals=%d", minSignals)
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.cli.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := c.cli.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out []EntityEffectiveness
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return out
}

// post marshals body as JSON and POSTs to the endpoint. All errors are silently discarded.
func (c *Client) post(path string, body interface{}) {
	data, err := json.Marshal(body)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.cli.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.cli.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// Package types defines the shared event types for the pulse analytics subsystem.
// It has no dependencies on other pulse sub-packages so that collector, pstore,
// and the top-level pulse package can all import it without cycles.
package types

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
// get_file_context, prepare_context) and carries token savings data.
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

// OutcomeSignalEvent is sent for passive outcome signals (R29).
// SignalType: "correction", "escalation", "replan", "task_done", "task_cancelled".
type OutcomeSignalEvent struct {
	ProjectID  string `json:"project_id,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`
	Entity     string `json:"entity,omitempty"`
	SignalType string `json:"signal_type"`
	Count      int    `json:"count,omitempty"`
}

// EntityEffectiveness is returned by GetEffectiveness for a single entity.
type EntityEffectiveness struct {
	Entity     string  `json:"entity"`
	Score      float64 `json:"score"`     // 0.0–1.0; higher = context was helpful
	Signals    int     `json:"signals"`
	Positives  int     `json:"positives"`
	Negatives  int     `json:"negatives"`
	Suggestion string  `json:"suggestion"`
}

// BrainUsageEvent represents a single LLM inference call from synapses-intelligence.
type BrainUsageEvent struct {
	Model            string  `json:"model"`
	Tier             string  `json:"tier,omitempty"`     // "ingest", "guardian", "enrich", "orchestrate"
	Endpoint         string  `json:"endpoint,omitempty"` // "/v1/ingest", "/v1/context-packet", etc.
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	DurationMs       int64   `json:"duration_ms"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	AgentID          string  `json:"agent_id,omitempty"`
	ProjectID        string  `json:"project_id,omitempty"`
}

// TotalTokens returns prompt + completion tokens.
func (e *BrainUsageEvent) TotalTokens() int {
	return e.PromptTokens + e.CompletionTokens
}

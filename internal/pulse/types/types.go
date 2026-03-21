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
	SessionID     string `json:"session_id,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
}

// ContextDeliveryEvent is sent for context-delivery tools (get_context,
// get_file_context, prepare_context) and carries token savings data.
type ContextDeliveryEvent struct {
	ToolName        string `json:"tool_name"`
	AgentID         string `json:"agent_id,omitempty"`
	ProjectID       string `json:"project_id,omitempty"`
	Entity          string `json:"entity,omitempty"`
	File            string `json:"file,omitempty"`
	ResponseBytes   int    `json:"response_bytes"`
	ResponseTokens  int    `json:"response_tokens"`
	BaselineTokens  int    `json:"baseline_tokens"`
	NodesDelivered  int    `json:"nodes_delivered"`
	NodesPruned     int    `json:"nodes_pruned"`
	EdgesDelivered  int    `json:"edges_delivered"`
	Truncated       bool   `json:"truncated"`
	DurationMs      int64  `json:"duration_ms"`
	CacheHit        bool   `json:"cache_hit"`
	BrainEnriched   bool   `json:"brain_enriched"`
	SessionID       string `json:"session_id,omitempty"`
	Intent          string `json:"intent,omitempty"`
	DepthRequested  int    `json:"depth_requested,omitempty"`
	DepthAchieved   int    `json:"depth_achieved,omitempty"`
	NodesVisited    int    `json:"nodes_visited,omitempty"`
}

// SessionEvent is sent when an agent session starts or ends.
type SessionEvent struct {
	AgentID   string `json:"agent_id"`
	ProjectID string `json:"project_id,omitempty"`
	Event     string `json:"event"` // "start" | "end" | "task_done"
	SessionID string `json:"session_id,omitempty"`
}

// OutcomeSignalEvent is sent for passive outcome signals (R29).
// SignalType: "correction", "escalation", "replan", "task_done", "task_cancelled".
type OutcomeSignalEvent struct {
	ProjectID  string `json:"project_id,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`
	Entity     string `json:"entity,omitempty"`
	SignalType string `json:"signal_type"`
	Count      int    `json:"count,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
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
	TargetEntity     string  `json:"target_entity,omitempty"`
	Success          bool    `json:"success"`
}

// TotalTokens returns prompt + completion tokens.
func (e *BrainUsageEvent) TotalTokens() int {
	return e.PromptTokens + e.CompletionTokens
}

// AgentLLMUsageEvent is reported by the AI agent itself (via report_usage tool)
// to record the model it used and how many tokens/cost the response incurred.
// This is Option B: agent-self-reported usage for accurate cost tracking.
type AgentLLMUsageEvent struct {
	SessionID    string  `json:"session_id"`
	AgentID      string  `json:"agent_id,omitempty"`
	ProjectID    string  `json:"project_id,omitempty"`
	Model        string  `json:"model"`
	Provider     string  `json:"provider,omitempty"` // "anthropic", "openai", etc.
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// Phase 2: Pipeline instrumentation event types.

// ParseEvent records the outcome of parsing a single source file during WalkDir.
type ParseEvent struct {
	File              string `json:"file"`
	Language          string `json:"language"`
	DurationMs        int64  `json:"duration_ms"`
	NodesProduced     int    `json:"nodes_produced"`
	EdgesProduced     int    `json:"edges_produced"`
	CallSitesProduced int    `json:"call_sites_produced"`
	ErrorType         string `json:"error_type,omitempty"`
	ProjectID         string `json:"project_id"`
}

// ReparseEvent records the outcome of incrementally re-parsing a changed file.
type ReparseEvent struct {
	File           string `json:"file"`
	Language       string `json:"language"`
	DurationMs     int64  `json:"duration_ms"`
	NodesBefore    int    `json:"nodes_before"`
	NodesAfter     int    `json:"nodes_after"`
	EdgesDelta     int    `json:"edges_delta"`
	MemoriesStaled int    `json:"memories_staled"`
	ProjectID      string `json:"project_id"`
}

// GraphSnapshotEvent captures a point-in-time snapshot of graph topology metrics.
type GraphSnapshotEvent struct {
	SnapshotType       string  `json:"snapshot_type"` // "full" | "delta"
	NodesTotal         int     `json:"nodes_total"`
	EdgesTotal         int     `json:"edges_total"`
	EdgesCalls         int     `json:"edges_calls"`
	Density            float64 `json:"density"`
	OrphanNodes        int     `json:"orphan_nodes"`
	CrossFileEdgePct   float64 `json:"cross_file_edge_pct"`
	MaxFanin           int     `json:"max_fanin"`
	MaxFanout          int     `json:"max_fanout"`
	FanInP50           int     `json:"fan_in_p50"`
	FanInP95           int     `json:"fan_in_p95"`
	FanOutP50          int     `json:"fan_out_p50"`
	FanOutP95          int     `json:"fan_out_p95"`
	NodeTypeDistJSON   string  `json:"node_type_distribution"` // JSON map
	ProjectID          string  `json:"project_id"`
}

// EmbeddingEvent records the outcome of an EmbedAllMemories batch operation.
type EmbeddingEvent struct {
	Trigger     string `json:"trigger"`      // "startup" | "reparse" | "manual"
	Count       int    `json:"count"`
	Errors      int    `json:"errors"`
	DurationMs  int64  `json:"duration_ms"`
	Model       string `json:"model"`
	ModelStatus string `json:"model_status"` // "loaded" | "downloading" | "failed" | "none"
	Success     bool   `json:"success"`
	StaleCount  int    `json:"stale_count"`
	ProjectID   string `json:"project_id"`
}

// GuardEvent is emitted when loop-guard or rate-limiter blocks a tool call.
type GuardEvent struct {
	GuardType string `json:"guard_type"` // "loop_warning" | "loop_circuit_break" | "rate_limit"
	ToolName  string `json:"tool_name"`
	Category  string `json:"category,omitempty"` // for rate_limit: "write_ops"|"expensive_reads"|"cross_project"
	AgentID   string `json:"agent_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

// MemoryOperationEvent tracks recall hits/misses and memory writes.
type MemoryOperationEvent struct {
	Operation   string `json:"operation"`    // "recall_hit" | "recall_miss" | "write" | "anchor_invalidated"
	Tier        string `json:"tier"`         // "episodic" | "entity" | "project"
	Source      string `json:"source"`       // "manual" | "auto"
	ResultCount int    `json:"result_count"`
	AgentID     string `json:"agent_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
}

// ValidationEvent tracks outcomes of validate_plan and verify_implementation.
type ValidationEvent struct {
	ToolName       string `json:"tool_name"`              // "validate_plan" | "verify_implementation"
	Status         string `json:"status"`                 // "ok" | "violations_found" | "pass"
	ViolationCount int    `json:"violation_count"`
	SafetyStatus   string `json:"safety_status,omitempty"` // "clear" | "warning"
	AgentID        string `json:"agent_id,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
}

// IndexEvent records the outcome of a full or incremental index operation.
type IndexEvent struct {
	DurationMs          int64   `json:"duration_ms"`
	FilesIndexed        int     `json:"files_indexed"`
	TotalNodes          int     `json:"total_nodes"`
	TotalEdges          int     `json:"total_edges"`
	CallSitesResolved   int     `json:"call_sites_resolved"`
	CallSitesUnresolved int     `json:"call_sites_unresolved"`
	ResolutionRate      float64 `json:"resolution_rate"`
	LanguageDistJSON    string  `json:"language_distribution"` // JSON map
	ProjectID           string  `json:"project_id"`
}

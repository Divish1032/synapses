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
	// Bug 10 — DQ-A.5: size of tool input payload in bytes.
	InputBytes int `json:"input_bytes,omitempty"`
	// Bug 11 — DQ-A.6: categorizes response shape for downstream analytics.
	ResponseType string `json:"response_type,omitempty"` // "graph_slice", "validation", "memory_list", "error", "text"
	// P5 — DQ-A.3: JSON-serialized request parameters (truncated at 512 chars).
	RequestParams string `json:"request_params,omitempty"`
	// P5 — DQ-A.4: retry count (same tool+entity within 60s).
	RetryCount int `json:"retry_count,omitempty"`
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
	// Bug 12 — DQ-B.6: whether annotation data was included in delivery.
	AnnotationsIncluded bool `json:"annotations_included,omitempty"`
	// Bug 13 — DQ-B.7: serialization format used for this delivery.
	OutputFormat string `json:"output_format,omitempty"` // "compact", "json", "default"
	// Bug 65 — PIPE-F3: distribution of edge types in the delivered slice.
	EdgeTypesDist string `json:"edge_types_dist,omitempty"` // JSON map of edge type -> count
	// Bug 66 — PIPE-F4: time spent on graph traversal for this delivery.
	TraversalDurationMs float64 `json:"traversal_duration_ms,omitempty"`
	// Bug 67 — PIPE-F6: number of nodes in the graph when traversal was performed.
	GraphSizeAtTraversal int `json:"graph_size_at_traversal,omitempty"`
	// P5 — DQ-B.4: detail level ("full", "compact", "").
	DetailLevel string `json:"detail_level,omitempty"`
	// P5 — DQ-B.5: architectural rules matched and violations found.
	RulesMatched   int `json:"rules_matched,omitempty"`
	ViolationsFound int `json:"violations_found,omitempty"`
	// P5 — DQ-B.8: whether entity was found or not.
	EntityFound bool `json:"entity_found,omitempty"`
	// P5 — PIPE-F7: nodes excluded by min_relevance threshold.
	MinRelevanceHits int `json:"min_relevance_hits,omitempty"`
	// P5 — PIPE-F7: whether token budget was the binding constraint.
	TokenBudgetHit bool `json:"token_budget_hit,omitempty"`
	// P7-17: whether this delivery is a refetch of a previously delivered entity.
	Refetched bool `json:"refetched,omitempty"`
	// P9-8: BFS subgraph cache size at delivery time.
	CacheSize int `json:"cache_size,omitempty"`
}

// SessionEvent is sent when an agent session starts or ends.
type SessionEvent struct {
	AgentID      string `json:"agent_id"`
	ProjectID    string `json:"project_id,omitempty"`
	Event        string `json:"event"` // "start" | "end" | "task_done"
	SessionID    string `json:"session_id,omitempty"`
	// Bug 16 — DQ-C.6: record which version of the agent started the session.
	AgentVersion string `json:"agent_version,omitempty"`
}

// OutcomeSignalEvent is sent for passive outcome signals (R29).
// SignalType: "correction", "escalation", "replan", "task_done", "task_cancelled", "task_abandoned".
type OutcomeSignalEvent struct {
	ProjectID       string `json:"project_id,omitempty"`
	AgentID         string `json:"agent_id,omitempty"`
	Entity          string `json:"entity,omitempty"`
	SignalType      string `json:"signal_type"`
	Count           int    `json:"count,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	// Bug 17 — DQ-D.3: how many tool calls occurred between context delivery and this signal.
	ToolCallsBetween int `json:"tool_calls_between,omitempty"`
	// P5 — DQ-D.2: time from most recent context delivery to this signal.
	TimeToOutcomeMs int64 `json:"time_to_outcome_ms,omitempty"`
	// P8-8: task priority for priority-completion correlation.
	Priority string `json:"priority,omitempty"`
	// Sprint 15 #1: signal quality weight for per-entity quality scoring.
	// Positive = context was helpful; negative = context was insufficient or session failed.
	// Callers must set this using the Signal* constants below.
	SignalWeight float64 `json:"signal_weight,omitempty"`
}

// Signal quality weights (Sprint 15 #1 — signal quality discipline).
// These constants encode the ROADMAP's quality tiers into numeric weights so that
// per-entity quality scores (Sprint 15 #2) can use COALESCE(SUM(signal_weight),0).
const (
	// SignalWeightTaskDone is a weak positive: task completed but context may have been
	// over-broad. Positive because completion is the goal; "weak" because correlation
	// is indirect — the task might have completed despite unhelpful context.
	SignalWeightTaskDone = 0.3

	// SignalWeightTaskCancelled is a moderate negative: agent explicitly abandoned the task.
	SignalWeightTaskCancelled = -0.5

	// SignalWeightTaskAbandoned is a strong negative: session ended without any task
	// completion signal — context likely did not enable progress.
	SignalWeightTaskAbandoned = -0.8

	// SignalWeightCorrectionImmediate is a moderate negative: same-entity re-fetch
	// within 5 minutes signals the initial context slice was insufficient.
	SignalWeightCorrectionImmediate = -0.5

	// SignalWeightCorrectionDelayed is a mild negative: same-entity re-fetch between
	// 5 and 30 minutes. May be a new subtask angle rather than a correction.
	SignalWeightCorrectionDelayed = -0.2

	// SignalWeightEscalation is a strong negative: three or more fetches of the same
	// entity — context was structurally insufficient (too shallow, wrong entity, etc).
	SignalWeightEscalation = -0.7

	// RefetchImmediateThreshold is the boundary between "correction" and "delayed" signals.
	RefetchImmediateThreshold = 5 * 60 // seconds
)

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
	// Bug 19 — DQ-F.3: subjective quality score for this inference (0.0–1.0).
	QualityScore float64 `json:"quality_score,omitempty"`
	// Bug 20 — DQ-F.4: whether a fallback model/path was used.
	FallbackUsed bool `json:"fallback_used,omitempty"`
	// P11-5: session ID for correlating brain inference to agent sessions.
	SessionID string `json:"session_id,omitempty"`
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
	// Bug 7 — DQ-E.3: disambiguate explicit zero cost from unreported cost.
	CostReported bool `json:"cost_reported,omitempty"` // true when cost_usd was explicitly provided
	// Bug 9 — DQ-E.2: Anthropic cache token fields.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	// Bug 18 — DQ-E.1: correlate with the tool call that triggered this usage.
	ToolCallID string `json:"tool_call_id,omitempty"`
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
	// P5 — PIPE-A6: count of tree-sitter ERROR nodes.
	TsErrorNodes int `json:"ts_error_nodes,omitempty"`
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
	// Bug 60 — PIPE-C5: number of times the debouncer suppressed a redundant reparse.
	DebounceHits int `json:"debounce_hits,omitempty"`
	// Bug 61 — PIPE-C8: time to detect whether this file belongs to a cross-project ref.
	CrossProjectDetectionMs float64 `json:"cross_project_detection_ms,omitempty"`
	// P5 — PIPE-C3: rows affected by delta save.
	DeltaRows int `json:"delta_rows,omitempty"`
	// P5 — PIPE-C3: bytes written by delta save.
	DeltaBytes int64 `json:"delta_bytes,omitempty"`
	// P9-7: error action taken by watcher ("clean", "skip", "proceed").
	ErrorAction string `json:"error_action,omitempty"`
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
	// Bug 58 — PIPE-B6: ratio of tombstoned (deleted) nodes to total nodes.
	TombstoneRatio float64 `json:"tombstone_ratio,omitempty"`
	// Bug 59 — PIPE-B8: JSON map of node provenance (parser, inferred, manual).
	ProvenanceDist string `json:"provenance_dist,omitempty"` // JSON map
	// P5 — COV-7: wall-clock rebuild duration in milliseconds.
	RebuildDurationMs float64 `json:"rebuild_duration_ms,omitempty"`
	// P5 — COV-7: what triggered the rebuild ("startup", "file_change", "manual").
	RebuildTrigger string `json:"rebuild_trigger,omitempty"`
	// P9-2: edge type distribution JSON map {"CALLS": 1200, "CONTAINS": 800, ...}.
	EdgeTypeDist string `json:"edge_type_dist,omitempty"`
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
	// Bug 62 — PIPE-D8: times a goroutine had to wait for the embedding pool.
	EmbedPoolContention int `json:"embed_pool_contention,omitempty"`
	// P5 — PIPE-D6: event type ("batch", "download_start", "download_complete", "load", "error").
	EventType string `json:"event_type,omitempty"`
	// P8-10: percentage of memories with up-to-date embeddings (0.0–1.0).
	CoveragePct float64 `json:"coverage_pct,omitempty"`
}

// GuardEvent is emitted when loop-guard or rate-limiter blocks a tool call.
type GuardEvent struct {
	GuardType string `json:"guard_type"` // "loop_warning" | "loop_circuit_break" | "rate_limit"
	ToolName  string `json:"tool_name"`
	Category  string `json:"category,omitempty"` // for rate_limit: "write_ops"|"expensive_reads"|"cross_project"
	AgentID   string `json:"agent_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	// P8-2: session ID for guard-event-to-session-outcome joins.
	SessionID string `json:"session_id,omitempty"`
}

// MemoryOperationEvent tracks recall hits/misses and memory writes.
type MemoryOperationEvent struct {
	Operation   string `json:"operation"`    // "recall_hit" | "recall_miss" | "write" | "anchor_invalidated" | "cross_session_hit" | "invalidated_cascade"
	Tier        string `json:"tier"`         // "episodic" | "entity" | "project"
	Source      string `json:"source"`       // "manual" | "auto"
	ResultCount int    `json:"result_count"`
	AgentID     string `json:"agent_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	// P5 — COV-10: number of memories invalidated in cascade.
	Count int `json:"count,omitempty"`
	// P5 — SA-D6: session ID for cross-session tracking.
	SessionID string `json:"session_id,omitempty"`
	// P5 — Item 12: which recall channel contributed the top-ranked result.
	TopChannel string `json:"top_channel,omitempty"` // "graph", "bm25", "semantic", "temporal", "combined"
	// P5 — Item 12: score of the top-ranked channel result.
	TopChannelScore float64 `json:"top_channel_score,omitempty"`
	// P5 — PIPE-D7: vector search latency in milliseconds.
	VectorSearchMs float64 `json:"vector_search_ms,omitempty"`
}

// ValidationEvent tracks outcomes of validate_plan and verify_implementation.
type ValidationEvent struct {
	ToolName       string `json:"tool_name"`              // "validate_plan" | "verify_implementation"
	Status         string `json:"status"`                 // "ok" | "violations_found" | "pass"
	ViolationCount int    `json:"violation_count"`
	SafetyStatus   string `json:"safety_status,omitempty"` // "clear" | "warning"
	RuleIDs        string `json:"rule_ids,omitempty"`      // JSON array of config.Violation.RuleID strings
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
	// Bug 63 — PIPE-E5: JSON map of work item types processed during this index run.
	WorkItemTypeDist string `json:"work_item_type_dist,omitempty"` // JSON map
	// P5 — COV-Subsys (resolver/): call-site resolution duration.
	ResolverDurationMs float64 `json:"resolver_duration_ms,omitempty"`
	// P9-1: files skipped by mtime check during incremental reindex.
	FilesSkipped int `json:"files_skipped,omitempty"`
	// P9-3: per-language call-site resolution rate JSON map {"Go": 0.95, "TypeScript": 0.82}.
	ResolutionByLangJSON string `json:"resolution_by_lang_json,omitempty"`
	// P9-5: per-language parser coverage JSON map {"Go": {"files": 10, "entities": 42}, ...}.
	CoverageJSON string `json:"coverage_json,omitempty"`
	// P9-6: heritage (INHERITS) edges created during this index run.
	HeritageEdgesCreated int `json:"heritage_edges_created,omitempty"`
	// P9-6: implements edges created during this index run.
	ImplementsEdgesCreated int `json:"implements_edges_created,omitempty"`
}

// SearchEvent records a search or find_entity tool call for analytics (Bug 57 / Task P4-8).
type SearchEvent struct {
	AgentID     string `json:"agent_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	Query       string `json:"query,omitempty"`
	Mode        string `json:"mode,omitempty"`   // "semantic", "exact", "hybrid", "discover", "call_chain", "impact", "doc_lookup"
	ResultCount int    `json:"result_count"`
	DurationMs  int64  `json:"duration_ms"`
	CacheHit    bool   `json:"cache_hit"`
	SessionID   string `json:"session_id,omitempty"`
	// P8-3: discover_tools funnel tracking — tools and workflows matched.
	MatchedTools     int `json:"matched_tools,omitempty"`
	MatchedWorkflows int `json:"matched_workflows,omitempty"`
}

// ConfigReloadEvent tracks configuration hot-reload outcomes (Bug 68 — COV-9).
type ConfigReloadEvent struct {
	Success       bool     `json:"success"`
	ChangedFields []string `json:"changed_fields,omitempty"`
	ProjectID     string   `json:"project_id,omitempty"`
}

// PersistenceEvent tracks store write durations and sizes (Bug 69 — COV-12).
type PersistenceEvent struct {
	DurationMs   float64 `json:"duration_ms"`
	BytesWritten int64   `json:"bytes_written"`
	ProjectID    string  `json:"project_id,omitempty"`
}

// EnrichmentEvent records the outcome of a code enrichment pass (Bug 70 — COV-Subsys).
type EnrichmentEvent struct {
	EnrichmentType string `json:"enrichment_type"` // "metrics", "brain", "types"
	DurationMs     int64  `json:"duration_ms"`
	Success        bool   `json:"success"`
	ProjectID      string `json:"project_id,omitempty"`
}

// RuleEvalEvent records architecture rule evaluation outcomes (Bug 71 — COV-Subsys).
type RuleEvalEvent struct {
	RulesEvaluated  int    `json:"rules_evaluated"`
	ViolationsFound int    `json:"violations_found"`
	ProjectID       string `json:"project_id,omitempty"`
}

// Phase 5 event types.

// FederationDetectEvent records cross-project dependency detection (COV-8/COV-14).
type FederationDetectEvent struct {
	AgentID        string  `json:"agent_id,omitempty"`
	ProjectID      string  `json:"project_id,omitempty"`
	SiblingProject string  `json:"sibling_project,omitempty"`
	Tier           int     `json:"tier"`
	DepsFound      int     `json:"deps_found"`
	DurationMs     float64 `json:"duration_ms"`
	EventType      string  `json:"event_type"` // "detection" | "impact_notification"
}

// ToolSequenceEntry records a single tool call in session order (SA-C1).
type ToolSequenceEntry struct {
	SessionID string `json:"session_id"`
	Position  int    `json:"position"`
	ToolName  string `json:"tool_name"`
	Success   bool   `json:"success"`
}

// EntityQuality holds per-entity context quality scores (Phase 5 Item 10).
type EntityQuality struct {
	Entity          string  `json:"entity"`
	ProjectID       string  `json:"project_id"`
	QualityScore    float64 `json:"quality_score"`
	PositiveSignals int     `json:"positive_signals"`
	NegativeSignals int     `json:"negative_signals"`
	LastUpdated     string  `json:"last_updated"`
}

// DeliveryOutcome links a context delivery to its subsequent task outcome (Phase 5 Item 11).
type DeliveryOutcome struct {
	DeliveryID        int    `json:"delivery_id"`
	SessionID         string `json:"session_id"`
	Entity            string `json:"entity"`
	OutcomeSignalType string `json:"outcome_signal_type"`
	OutcomeAt         string `json:"outcome_at"`
	ToolsBetween      int    `json:"tools_between"`
	Success           int    `json:"success"`
}

// SessionEffectiveness holds session-level effectiveness metrics (Phase 5 Item 13).
type SessionEffectiveness struct {
	SessionID          string  `json:"session_id"`
	AgentID            string  `json:"agent_id"`
	ProjectID          string  `json:"project_id"`
	ContextHitRate     float64 `json:"context_hit_rate"`
	TaskCompletionRate float64 `json:"task_completion_rate"`
	TokensSaved        int     `json:"tokens_saved"`
	ToolCalls          int     `json:"tool_calls"`
	DurationMs         int64   `json:"duration_ms"`
	KnowledgeGrowth    int     `json:"knowledge_growth"`
	CreatedAt          string  `json:"created_at"`
}

// DailyEffectiveness holds daily aggregated effectiveness for trend charts (Phase 5 Item 13).
type DailyEffectiveness struct {
	Day                  string  `json:"day"`
	AvgContextHitRate    float64 `json:"avg_context_hit_rate"`
	AvgTaskCompletion    float64 `json:"avg_task_completion"`
	TotalTokensSaved     int     `json:"total_tokens_saved"`
	Sessions             int     `json:"sessions"`
	TotalKnowledgeGrowth int     `json:"total_knowledge_growth"`
}

// WeeklyEfficiency holds per-week agent efficiency metrics (SA-B2).
type WeeklyEfficiency struct {
	WeekStart       string  `json:"week_start"`
	TasksPerSession float64 `json:"tasks_per_session"`
	ToolCallsPerTask float64 `json:"tool_calls_per_task"`
}

// SessionPerformance holds per-session efficiency metrics for learning velocity (P8-6).
type SessionPerformance struct {
	SessionNum       int     `json:"session_num"`
	ToolCalls        int     `json:"tool_calls"`
	TasksCompleted   int     `json:"tasks_completed"`
	ToolCallsPerTask float64 `json:"tool_calls_per_task"`
	TokensSaved      int     `json:"tokens_saved"`
}

// MonthlyROI holds monthly ROI report data (ROI-F5).
type MonthlyROI struct {
	TotalTokensSaved   int64   `json:"total_tokens_saved"`
	TotalCostSavedUSD  float64 `json:"total_cost_saved_usd"`
	TotalSessions      int     `json:"total_sessions"`
	TotalTasksCompleted int    `json:"total_tasks_completed"`
	AvgDailyAgents     float64 `json:"avg_daily_agents"`
	AvgContextHitRate  float64 `json:"avg_context_hit_rate"`
	BestWeek           string  `json:"best_week"`
	WorstWeek          string  `json:"worst_week"`
}

// DecayStats holds recall hit rates bucketed by memory age (ROI-D4).
type DecayStats struct {
	HitRateUnder7d  float64 `json:"hit_rate_under_7d"`
	HitRate7to30d   float64 `json:"hit_rate_7_to_30d"`
	HitRateOver30d  float64 `json:"hit_rate_over_30d"`
}

// SessionPercentiles holds tools/calls-per-session distribution (SA-A3).
type SessionPercentiles struct {
	ToolsP50  float64 `json:"tools_p50"`
	ToolsP95  float64 `json:"tools_p95"`
	ToolsP99  float64 `json:"tools_p99"`
	CallsP50  float64 `json:"calls_p50"`
	CallsP95  float64 `json:"calls_p95"`
	CallsP99  float64 `json:"calls_p99"`
}

// DurationBuckets holds session duration percentiles (DQ-C.2).
type DurationBuckets struct {
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
}

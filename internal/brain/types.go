// Package brain provides a fail-silent HTTP client for the synapses-intelligence
// sidecar service. All methods return zero values (nil, "", etc.) on failure so
// callers never need to handle brain errors — the graph-only path always works.
package brain

// ContextPacketRequest is the payload for POST /v1/context-packet.
type ContextPacketRequest struct {
	AgentID     string        `json:"agent_id"`
	Snapshot    SnapshotInput `json:"snapshot"`
	Phase       string        `json:"phase,omitempty"`        // "" = use stored
	QualityMode string        `json:"quality_mode,omitempty"` // "" = use stored
	EnableLLM   bool          `json:"enable_llm"`
}

// SnapshotInput is the structural snapshot sent with a context-packet request.
type SnapshotInput struct {
	RootNodeID      string       `json:"root_node_id"`
	RootName        string       `json:"root_name"`
	RootType        string       `json:"root_type"`
	RootFile        string       `json:"root_file"`
	CalleeNames     []string     `json:"callee_names"`
	CallerNames     []string     `json:"caller_names"`
	ApplicableRules []RuleInput  `json:"applicable_rules"`
	ActiveClaims    []ClaimInput `json:"active_claims"`
	TaskContext     string       `json:"task_context,omitempty"`
	TaskID          string       `json:"task_id,omitempty"`
	HasTests        bool         `json:"has_tests"` // whether *_test.go exists for root file
	FanIn           int          `json:"fan_in"`    // total caller count from graph
}

// RuleInput is a slim rule descriptor for the intelligence service.
type RuleInput struct {
	RuleID      string `json:"rule_id"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
}

// ClaimInput is a slim work-claim descriptor.
type ClaimInput struct {
	AgentID   string `json:"agent_id"`
	Scope     string `json:"scope"`
	ScopeType string `json:"scope_type"`
	ExpiresAt string `json:"expires_at"`
}

// ContextPacket is the response from POST /v1/context-packet.
type ContextPacket struct {
	AgentID             string            `json:"agent_id"`
	EntityName          string            `json:"entity_name"`
	EntityType          string            `json:"entity_type"`
	Phase               string            `json:"phase"`
	QualityMode         string            `json:"quality_mode"`
	RootSummary         string            `json:"root_summary,omitempty"`
	DependencySummaries map[string]string `json:"dependency_summaries,omitempty"`
	Insight             string            `json:"insight,omitempty"`
	Concerns            []string          `json:"concerns,omitempty"`
	ActiveConstraints   []ConstraintItem  `json:"active_constraints,omitempty"`
	TeamStatus          []AgentStatus     `json:"team_status,omitempty"`
	QualityGate         *QualityGate      `json:"quality_gate,omitempty"`
	PatternHints        []PatternHint     `json:"pattern_hints,omitempty"`
	PhaseGuidance       string            `json:"phase_guidance,omitempty"`
	LLMUsed             bool              `json:"llm_used"`
	PacketQuality       float64           `json:"packet_quality"`
	GraphWarnings       []string          `json:"graph_warnings,omitempty"`
}

// ConstraintItem is an active architectural constraint in a context packet.
type ConstraintItem struct {
	RuleID      string `json:"rule_id"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
}

// AgentStatus is a team-member status entry in a context packet.
type AgentStatus struct {
	AgentID   string `json:"agent_id"`
	Scope     string `json:"scope"`
	ScopeType string `json:"scope_type"`
	ExpiresAt string `json:"expires_at"`
}

// QualityGate holds pass/fail checklist items from the intelligence service.
type QualityGate struct {
	Passed   bool     `json:"passed"`
	Checks   []string `json:"checks"`
	Failures []string `json:"failures"`
}

// PatternHint is a code-pattern recommendation from the intelligence service.
type PatternHint struct {
	Pattern     string `json:"pattern"`
	Description string `json:"description"`
}

// IngestRequest is the payload for POST /v1/ingest.
type IngestRequest struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	NodeType string `json:"node_type"`
	Package  string `json:"package"`
	Code     string `json:"code"`
}

// ViolationRequest is the payload for POST /v1/explain-violation.
type ViolationRequest struct {
	RuleID       string `json:"rule_id"`
	RuleSeverity string `json:"rule_severity"`
	Description  string `json:"description"`
	SourceFile   string `json:"source_file"`
	TargetName   string `json:"target_name"`
}

// ViolationExplanation is the response from POST /v1/explain-violation.
type ViolationExplanation struct {
	Explanation string `json:"explanation"`
	Fix         string `json:"fix"`
}

// CoordinateRequest is the payload for POST /v1/coordinate.
type CoordinateRequest struct {
	NewAgentID        string       `json:"new_agent_id"`
	NewScope          string       `json:"new_scope"`
	ConflictingClaims []ClaimInput `json:"conflicting_claims"`
}

// CoordinateResponse is the response from POST /v1/coordinate.
type CoordinateResponse struct {
	Suggestion string `json:"suggestion"`
}

// DecisionRequest is the payload for POST /v1/decision.
type DecisionRequest struct {
	AgentID         string   `json:"agent_id"`
	Phase           string   `json:"phase"`
	EntityName      string   `json:"entity_name"`
	Action          string   `json:"action"`
	RelatedEntities []string `json:"related_entities"`
	Outcome         string   `json:"outcome"`
	Notes           string   `json:"notes"`
}

// SDLCConfig is the response from GET /v1/sdlc.
type SDLCConfig struct {
	Phase       string `json:"phase"`
	QualityMode string `json:"quality_mode"`
}

// SetPhaseRequest is the payload for PUT /v1/sdlc/phase.
type SetPhaseRequest struct {
	Phase   string `json:"phase"`
	AgentID string `json:"agent_id,omitempty"`
}

// Package brain provides the public API types for synapses-intelligence.
// Synapses imports this package to integrate the Thinking Brain.
package brain

// --- Ingest (Semantic Ingestor) ---

// IngestRequest carries a code snippet for semantic summarization.
// Called on file-save events for changed functions/methods/structs.
type IngestRequest struct {
	// ProjectID scopes summaries to a specific project, preventing collisions
	// when multiple projects have identically-named entities (e.g., "auth.Login").
	ProjectID string `json:"project_id,omitempty"`
	// NodeID is the stable graph node identifier (e.g., "pkg:func:AuthService.Validate").
	NodeID string `json:"node_id"`
	// NodeName is the short name of the entity (e.g., "Validate").
	NodeName string `json:"node_name"`
	// NodeType is one of: "function", "method", "struct", "interface", "variable".
	NodeType string `json:"node_type"`
	// Package is the Go/language package name.
	Package string `json:"package"`
	// Code is the source snippet — capped at 500 chars to keep prompts small.
	Code string `json:"code"`
}

// IngestResponse is returned after summarization.
type IngestResponse struct {
	NodeID  string   `json:"node_id"`
	Summary string   `json:"summary"`        // 1-sentence intent summary
	Tags    []string `json:"tags,omitempty"` // 1-3 domain labels, e.g. ["auth","http"]
}

// --- Enrich (Context Enricher) ---

// EnrichRequest carries the carved subgraph data from a get_context call.
type EnrichRequest struct {
	// ProjectID scopes summary lookups to a specific project.
	ProjectID string `json:"project_id,omitempty"`
	// RootID is the graph node ID of the queried entity.
	RootID string `json:"root_id"`
	// RootName is the entity name (e.g., "AuthService").
	RootName string `json:"root_name"`
	// RootType is the node type (e.g., "struct").
	RootType string `json:"root_type"`
	// AllNodeIDs contains every node ID in the carved subgraph, for summary lookup.
	AllNodeIDs []string `json:"all_node_ids"`
	// CalleeNames are names of entities the root calls directly.
	CalleeNames []string `json:"callee_names"`
	// CallerNames are names of entities that call the root.
	CallerNames []string `json:"caller_names"`
	// RelatedNames are other nodes in the subgraph.
	RelatedNames []string `json:"related_names"`
	// TaskContext is optional context from a linked task (from task_id).
	TaskContext string `json:"task_context,omitempty"`
}

// EnrichResponse is added to the get_context response.
type EnrichResponse struct {
	// Insight is a 2-sentence analysis of the entity's architectural role.
	Insight string `json:"insight"`
	// Concerns are specific observations (e.g., "handles auth tokens", "rate limit boundary").
	Concerns []string `json:"concerns"`
	// Summaries maps nodeID → 1-sentence summary for nodes that have been ingested.
	// These are loaded from brain.sqlite (no LLM call needed — fast lookup).
	Summaries map[string]string `json:"summaries"`
	// LLMUsed is true when the LLM was called to generate the Insight field.
	// False means Insight is empty (LLM unavailable, feature disabled, or timed out).
	LLMUsed bool `json:"llm_used"`
	// Degraded is true when the primary enrich tier's circuit tripped and a
	// lower-tier fallback model was used. Summaries are still present; Insight
	// may be weaker. Callers can use this to decide whether to re-request later.
	Degraded bool `json:"degraded,omitempty"`
}

// --- ExplainViolation (Rule Guardian) ---

// ViolationRequest carries a single architectural rule violation.
type ViolationRequest struct {
	// RuleID is the unique rule identifier from synapses.json.
	RuleID string `json:"rule_id"`
	// RuleSeverity is "error" or "warning".
	RuleSeverity string `json:"rule_severity"`
	// Description is the rule's human-readable description.
	Description string `json:"description"`
	// SourceFile is the file that triggered the violation.
	SourceFile string `json:"source_file"`
	// TargetName is the entity name being imported/called in violation of the rule.
	TargetName string `json:"target_name"`
}

// ViolationResponse is a plain-English explanation with an actionable fix.
type ViolationResponse struct {
	// Explanation describes the violation in plain language.
	Explanation string `json:"explanation"`
	// Fix is a concrete, actionable suggestion to resolve the violation.
	Fix string `json:"fix"`
	// Degraded is true when the guardian tier's circuit tripped and a
	// lower-tier fallback model was used for the explanation.
	Degraded bool `json:"degraded,omitempty"`
}

// --- Coordinate (Task Orchestrator) ---

// WorkClaim represents an existing agent's work claim.
type WorkClaim struct {
	AgentID   string `json:"agent_id"`
	Scope     string `json:"scope"`
	ScopeType string `json:"scope_type"`
}

// CoordinateRequest describes an agent registration that conflicts with existing claims.
type CoordinateRequest struct {
	// NewAgentID is the agent trying to claim work.
	NewAgentID string `json:"new_agent_id"`
	// NewScope is the scope the new agent wants to claim.
	NewScope string `json:"new_scope"`
	// ConflictingClaims are existing claims that overlap with NewScope.
	ConflictingClaims []WorkClaim `json:"conflicting_claims"`
}

// CoordinateResponse suggests how to distribute work to avoid the conflict.
type CoordinateResponse struct {
	// Suggestion is a plain-English recommendation for the new agent.
	Suggestion string `json:"suggestion"`
	// AlternativeScope is a concrete non-conflicting scope the new agent could claim instead.
	AlternativeScope string `json:"alternative_scope"`
	// Degraded is true when the orchestrate tier's circuit tripped and a
	// lower-tier fallback model was used for the coordination suggestion.
	Degraded bool `json:"degraded,omitempty"`
}

// =============================================================================
// v0.2.0: Context Packet, SDLC phases, quality modes, learning loop
// =============================================================================

// SDLCPhase identifies the current stage in the software development lifecycle.
type SDLCPhase string

// SDLC phase constants.
const (
	PhaseUnknown     SDLCPhase = ""
	PhasePlanning    SDLCPhase = "planning"
	PhaseDevelopment SDLCPhase = "development"
	PhaseTesting     SDLCPhase = "testing"
	PhaseReview      SDLCPhase = "review"
	PhaseDeployment  SDLCPhase = "deployment"
)

// QualityMode controls how strict the quality gate is for an agent's work.
type QualityMode string

// Quality mode constants.
const (
	QualityQuick      QualityMode = "quick"      // prototype: just make it work
	QualityStandard   QualityMode = "standard"   // default: unit tests required
	QualityEnterprise QualityMode = "enterprise" // full: tests + docs + PR checklist
)

// ContextPacket is the central output of the Brain's context builder.
// It is a purpose-built, structured document assembled for a specific agent and phase —
// replacing raw graph nodes with semantic summaries and actionable guidance.
//
// A nil ContextPacket means the Brain is unavailable; callers use raw context as fallback.
type ContextPacket struct {
	// Header
	AgentID     string      `json:"agent_id,omitempty"`
	EntityName  string      `json:"entity_name"`
	EntityType  string      `json:"entity_type,omitempty"`
	GeneratedAt string      `json:"generated_at"`
	Phase       SDLCPhase   `json:"phase"`
	QualityMode QualityMode `json:"quality_mode"`

	// Section 1: Semantic Focus (fast path — SQLite only, no LLM)
	// 1-sentence intent summary for the root entity.
	RootSummary string `json:"root_summary,omitempty"`
	// Summaries for key dependencies (entityName → summary). Replaces raw code.
	DependencySummaries map[string]string `json:"dependency_summaries,omitempty"`

	// Section 2: Architectural Insight (LLM path — optional, 2-3s)
	Insight  string   `json:"insight,omitempty"`
	Concerns []string `json:"concerns,omitempty"`

	// Section 3: Architectural Constraints
	ActiveConstraints []ConstraintItem `json:"active_constraints,omitempty"`

	// Section 4: Team Coordination
	TeamStatus []AgentStatus `json:"team_status,omitempty"`

	// Section 5: Quality Gate — concrete checklist for current phase+mode
	QualityGate QualityGate `json:"quality_gate"`

	// Section 6: Learned Patterns ("when editing X, also check Y")
	PatternHints []PatternHint `json:"pattern_hints,omitempty"`

	// Section 7: Phase Guidance — what the agent should do next
	PhaseGuidance string `json:"phase_guidance,omitempty"`

	// LLMUsed indicates whether a live LLM call was made during packet assembly.
	// False means all data came from brain.sqlite (sub-millisecond path).
	LLMUsed bool `json:"llm_used,omitempty"`

	// PacketQuality is a 0.0–1.0 heuristic reflecting how complete this packet is.
	// 0.0 = no summaries ingested yet; 0.5 = summaries present, no insight; 1.0 = full.
	// Agents can use this to decide whether to request a follow-up LLM enrichment pass.
	PacketQuality float64 `json:"packet_quality"`

	// GraphWarnings are actionable warnings derived from graph topology.
	// Examples: "High blast radius (12 callers)", "No tests found for this file".
	// These supplement the LLM Insight with deterministic, always-available guidance.
	GraphWarnings []string `json:"graph_warnings,omitempty"`

	// ComplexityScore is a dimensionless topology-derived risk indicator:
	// (fanIn + fanOut) * (1 + fanOut/10.0). Always populated when the enricher ran,
	// regardless of Ollama availability. 0.0 = isolated/leaf node.
	ComplexityScore float64 `json:"complexity_score,omitempty"`

	// DeterministicPath is true when Phase and ComplexityScore were derived from
	// graph topology without an LLM call. False only when the enricher was not invoked.
	DeterministicPath bool `json:"deterministic_path,omitempty"`
}

// ConstraintItem is a single architectural rule the agent must respect.
type ConstraintItem struct {
	RuleID      string `json:"rule_id"`
	Severity    string `json:"severity"` // "error" | "warning"
	Description string `json:"description"`
	// Hint is a cached fix suggestion (from violation_cache or derived from description).
	Hint string `json:"hint,omitempty"`
}

// AgentStatus is a compact view of another agent's current work claim.
type AgentStatus struct {
	AgentID   string `json:"agent_id"`
	Scope     string `json:"scope"`
	ScopeType string `json:"scope_type"`
	// ExpiresIn is the seconds until this claim expires (0 = unknown).
	ExpiresIn int `json:"expires_in_seconds,omitempty"`
}

// QualityGate lists concrete requirements for the current phase+mode combination.
// The agent treats Checklist as an ordered to-do list before declaring work done.
type QualityGate struct {
	RequireTests   bool     `json:"require_tests"`
	RequireDocs    bool     `json:"require_docs"`
	RequirePRCheck bool     `json:"require_pr_check"` // enterprise only
	Checklist      []string `json:"checklist"`
}

// PatternHint is a learned co-occurrence: "when editing X, also check Y".
type PatternHint struct {
	Trigger    string  `json:"trigger"`
	CoChange   string  `json:"co_change"`
	Reason     string  `json:"reason,omitempty"`
	Confidence float64 `json:"confidence"`
}

// --- Context Packet request/input types ---

// ContextPacketRequest is the input to Brain.BuildContextPacket().
// All fields are optional — empty Phase/QualityMode fall back to the stored project config.
type ContextPacketRequest struct {
	AgentID     string                `json:"agent_id,omitempty"`
	ProjectID   string                `json:"project_id,omitempty"`
	Snapshot    SynapsesSnapshotInput `json:"snapshot"`
	Phase       SDLCPhase             `json:"phase,omitempty"`        // "" = use stored project phase
	QualityMode QualityMode           `json:"quality_mode,omitempty"` // "" = use stored project mode
	EnableLLM   bool                  `json:"enable_llm"`             // true = allow LLM insight (~2s)
}

// SynapsesSnapshotInput carries the raw structural data from a Synapses get_context call.
// Synapses (or the HTTP caller) populates this; the Brain uses it to build the packet.
type SynapsesSnapshotInput struct {
	RootNodeID      string       `json:"root_node_id,omitempty"`
	RootName        string       `json:"root_name"`
	RootType        string       `json:"root_type,omitempty"`
	RootFile        string       `json:"root_file,omitempty"`        // used for constraint hint lookups
	CalleeNames     []string     `json:"callee_names,omitempty"`     // what root calls directly
	CallerNames     []string     `json:"caller_names,omitempty"`     // what calls root directly
	RelatedNames    []string     `json:"related_names,omitempty"`    // transitive neighbours
	ApplicableRules []RuleInput  `json:"applicable_rules,omitempty"` // rules whose pattern matches RootFile
	ActiveClaims    []ClaimInput `json:"active_claims,omitempty"`    // work claims from other agents
	TaskContext     string       `json:"task_context,omitempty"`
	TaskID          string       `json:"task_id,omitempty"`
	HasTests        bool         `json:"has_tests"`          // whether *_test.go exists for root file
	FanIn           int          `json:"fan_in"`             // total caller count (may exceed len(CallerNames)
	RootDoc         string       `json:"root_doc,omitempty"` // AST doc comment; fallback summary when brain.sqlite has no summary)
}

// RuleInput is a single architectural rule reference.
type RuleInput struct {
	RuleID      string `json:"rule_id"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
}

// ClaimInput is a single work claim from another agent.
type ClaimInput struct {
	AgentID   string `json:"agent_id"`
	Scope     string `json:"scope"`
	ScopeType string `json:"scope_type"`
	ExpiresAt string `json:"expires_at,omitempty"` // RFC3339
}

// --- Decision log (learning loop) ---

// DecisionRequest feeds the Brain's co-occurrence learning loop.
// Agents call LogDecision after completing work on an entity.
type DecisionRequest struct {
	AgentID         string   `json:"agent_id"`
	Phase           string   `json:"phase"`
	EntityName      string   `json:"entity_name"`
	Action          string   `json:"action"` // "edit"|"test"|"review"|"fix_violation"
	RelatedEntities []string `json:"related_entities"`
	Outcome         string   `json:"outcome"` // "success"|"violation"|"reverted"|""
	Notes           string   `json:"notes"`
}

// --- SDLC state ---

// SDLCConfig is the current project SDLC state (returned by SetSDLCPhase / SetQualityMode).
type SDLCConfig struct {
	Phase       SDLCPhase   `json:"phase"`
	QualityMode QualityMode `json:"quality_mode"`
	UpdatedAt   string      `json:"updated_at"`
	UpdatedBy   string      `json:"updated_by,omitempty"`
}

// --- Architectural Decision Records (ADRs) ---

// ADR is an Architectural Decision Record — a persistent cold-memory entry
// for a significant design choice. ADRs are injected into get_context output
// when their linked_files patterns match the queried entity's file.
type ADR struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Status       string   `json:"status"` // proposed | accepted | deprecated | superseded
	ContextText  string   `json:"context,omitempty"`
	Decision     string   `json:"decision"`
	Consequences string   `json:"consequences,omitempty"`
	LinkedFiles  []string `json:"linked_files,omitempty"` // file path glob patterns
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

// ADRRequest is the input to UpsertADR.
type ADRRequest struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Status       string   `json:"status"` // proposed | accepted | deprecated | superseded
	ContextText  string   `json:"context,omitempty"`
	Decision     string   `json:"decision"`
	Consequences string   `json:"consequences,omitempty"`
	LinkedFiles  []string `json:"linked_files,omitempty"`
}

// SnapshotInput is a backward-compat alias for SynapsesSnapshotInput.
type SnapshotInput = SynapsesSnapshotInput

// ViolationExplanation is a backward-compat alias for ViolationResponse.
type ViolationExplanation = ViolationResponse

// SetPhaseRequest is the payload for setting the SDLC phase.
type SetPhaseRequest struct {
	Phase string `json:"phase"`
}

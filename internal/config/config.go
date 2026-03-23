// Package config loads and validates the synapses.json project configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/brain/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/logutil"
)

const configFileName = "synapses.json"

// Config is the root structure of synapses.json.
type Config struct {
	// Version is the config schema version. Currently "1".
	Version string `json:"version"`
	// Mode controls the project's operational mode.
	// "full" (default): full code intelligence with graph, parsing, file watching.
	// "knowledge": memory, events, tasks, and messages only — no code graph.
	// Knowledge mode is useful for non-code domains (marketing, ops, QA).
	Mode string `json:"mode,omitempty"`
	// Rules defines the architectural constraints the rule engine enforces.
	Rules []Rule `json:"rules"`
	// EdgeWeights overrides the default relevance weights for context carving.
	EdgeWeights map[graph.EdgeType]float64 `json:"edge_weights,omitempty"`
	// ContextCarve holds global carving defaults.
	ContextCarve ContextCarveConfig `json:"context_carve,omitempty"`
	// Linked is a list of paths to other indexed projects whose graphs are merged
	// into this project at startup. Paths may be absolute or relative to the
	// directory containing synapses.json. Useful for monorepos where multiple
	// sub-projects share exported types and functions.
	Linked []string `json:"linked,omitempty"`
	// EmbeddingEndpoint is an optional OpenAI-compatible HTTP endpoint for
	// generating vector embeddings. When set, semantic_search will request an
	// embedding for the query and rank results by cosine similarity in addition
	// to FTS5 BM25. Leave empty to use FTS5-only search (no external deps).
	// Compatible with Ollama (/api/embeddings) and OpenAI (/v1/embeddings).
	// Example: "http://localhost:11434/api/embeddings"
	EmbeddingEndpoint string `json:"embedding_endpoint,omitempty"`

	// Embeddings controls memory embedding generation mode.
	// "builtin" (default): pure-Go sentence-transformer inference using
	// nomic-embed-text-v1.5 ONNX model (~137MB). Zero external dependencies.
	// "ollama": delegates to a local Ollama instance (requires Ollama running).
	// Uses EmbeddingEndpoint or defaults to http://localhost:11434/api/embeddings.
	// "off": disabled, FTS5-only recall.
	// Embeddings are computed locally and never sent anywhere (Privacy value).
	Embeddings string `json:"embeddings,omitempty"`

	// ApiEntries defines custom patterns for identifying API entry points via
	// get_api_contract. These supplement built-in convention detection (net/http,
	// gin, echo, fiber, gRPC, proto RPC). All non-empty fields in a pattern are
	// ANDed together.
	ApiEntries []ApiEntryPattern `json:"api_entries,omitempty"`

	// UseGoTypes enables type-checked CALLS resolution for Go files using
	// golang.org/x/tools/go/packages. When true, a second resolver pass runs
	// after tree-sitter parsing, using go list + type inference to detect
	// cross-package, interface-dispatch, and closure calls that the structural
	// resolver misses. Requires the project to be a valid Go module (go.mod
	// present, dependencies available). Falls back gracefully on error.
	// Default: false (opt-in to avoid adding latency for non-Go projects).
	UseGoTypes bool `json:"use_go_types,omitempty"`

	// UseTSTypes enables type-checked CALLS resolution for TypeScript (.ts/.tsx)
	// files using the TypeScript compiler API. When true, a Node.js subprocess
	// is spawned to resolve cross-file calls, interface implementations, and
	// re-exported symbols that tree-sitter cannot see. Requires Node.js on PATH
	// and the "typescript" npm package in the project's node_modules (or
	// globally installed). Falls back gracefully on error.
	// Default: false (opt-in to avoid latency for non-TypeScript projects).
	UseTSTypes bool `json:"use_ts_types,omitempty"`

	// MetricsDays is the git history window (in days) used when computing
	// per-file churn counts. A value of 0 defaults to 90 days. Churn is
	// computed at index time and stored as node metadata["churn"].
	MetricsDays int `json:"metrics_days,omitempty"`

	// CoverageProfile is a path to a go test -coverprofile output file.
	// When set, function and method nodes are annotated with metadata["coverage"]
	// (0.00–1.00, fraction of statements covered). The path may be absolute or
	// relative to the directory containing synapses.json.
	// Generate with: go test ./... -coverprofile=cover.out
	CoverageProfile string `json:"coverage_profile,omitempty"`

	// PprofProfile is a path to a Go pprof CPU profile file. When set, function
	// and method nodes are annotated with metadata["cpu_pct"] (flat CPU
	// percentage). The path may be absolute or relative to synapses.json.
	// Requires go tool pprof on PATH (standard with any Go installation).
	// Generate with: go test -cpuprofile=cpu.prof  OR  use runtime/pprof in app.
	PprofProfile string `json:"pprof_profile,omitempty"`

	// DataFlowSources defines additional custom source patterns (data entry points).
	// Built-in detection covers *http.Request, gin/echo contexts, Parse*/Decode*,
	// io.Reader, and env-var accessors. Use this to add project-specific sources.
	DataFlowSources []DataFlowPattern `json:"data_flow_sources,omitempty"`

	// DataFlowSinks defines additional custom sink patterns (dangerous data exits).
	// Built-in detection covers SQL exec/query, exec.Command, os.File writes, io.Writer.
	// Use this to add project-specific sinks (e.g. an internal audit log function).
	DataFlowSinks []DataFlowPattern `json:"data_flow_sinks,omitempty"`

	// DataFlowMaxHops is the maximum BFS depth when tracing source-to-sink paths.
	// A value of 0 defaults to 4. Higher values find more indirect paths but are slower.
	DataFlowMaxHops int `json:"data_flow_max_hops,omitempty"`

	// Plugins have been removed for security reasons (Workspace RCE).
	// They must now be configured globally via the CLI or user-level config.
	Plugins []PluginConfig `json:"-"`

	// Federation is a list of local sibling projects whose SQLite stores are
	// queried read-only for cross-project dependency tracking and drift detection.
	// Unlike Peers (HTTP-based), federation uses direct filesystem access — no
	// running daemon required. Paths may be absolute or relative to the directory
	// containing synapses.json.
	Federation []FederationEntry `json:"federation,omitempty"`

	// FederationACL controls which daemon-registered projects this project is
	// allowed to query via the projects= parameter on cross-project tools
	// (recall, get_events, get_messages, get_agents, etc.).
	// Default (nil or empty AllowReadFrom): deny-all — no cross-project reads.
	// Set AllowReadFrom to ["*"] to allow reading from all registered projects.
	// Set to specific project names (directory basenames) to allow only those.
	FederationACL *FederationACLConfig `json:"federation_acl,omitempty"`

	// Constitution defines project-wide principles that are injected into every
	// agent session and get_context response. Use this to codify architectural
	// laws, coding standards, and constraints that every AI agent must respect.
	Constitution ConstitutionConfig `json:"constitution,omitempty"`

	// Brain configures the optional synapses-intelligence integration.
	// When set, get_context returns LLM-enriched Context Packets, violations
	// include plain-English explanations, and file changes are auto-ingested.
	Brain BrainConfig `json:"brain,omitempty"`

	// Pulse configures the optional synapses-pulse analytics sidecar.
	// When set, every tool call is reported to the pulse service for token
	// savings and cost attribution telemetry. All errors are silently discarded.
	Pulse PulseConfig `json:"pulse,omitempty"`

	// Session configures agent session memory behavior.
	Session SessionConfig `json:"session,omitempty"`

	// RateLimits configures per-session token-bucket rate limiting for write
	// operations and expensive read operations on the MCP stdio transport.
	// All limits are per-session (per MCP connection) and measured per minute.
	// Set any limit to -1 to disable that category. Defaults apply when omitted.
	RateLimits RateLimitConfig `json:"rate_limits,omitempty"`

	// ContentSafety configures the prompt injection scanner that runs on all
	// externally-sourced content before storage. Covers remember(), send_message(),
	// annotate_node(), and web_annotate() inputs.
	ContentSafety ContentSafetyConfig `json:"content_safety,omitempty"`

	// Recall configures the quad-channel retrieval pipeline.
	Recall RecallConfig `json:"recall,omitempty"`
}

// RecallConfig controls the quad-channel recall pipeline behavior.
type RecallConfig struct {
	// FusionMode selects the multi-channel merging strategy.
	// "rrf" (default): Reciprocal Rank Fusion — uses rank positions only.
	// "convex": Score-aware convex combination — uses score magnitudes.
	//   Research shows 3.86% NDCG@10 improvement when channels have
	//   heterogeneous score distributions (Benham & Culpepper, 2017).
	FusionMode string `json:"fusion_mode,omitempty"`

	// ConvexAlpha controls the BM25 vs semantic balance in convex fusion.
	// score = α × norm_bm25 + (1-α) × norm_cosine + graph_bonus + temporal_bonus.
	// Range [0.0, 1.0]. Default 0.5 (equal weight). Higher = more BM25 influence.
	// Only used when FusionMode is "convex".
	ConvexAlpha *float64 `json:"convex_alpha,omitempty"`

	// ConvexGraphBonus is the additive weight for graph channel in convex fusion.
	// Range [0.0, 1.0]. Default 0.3.
	ConvexGraphBonus *float64 `json:"convex_graph_bonus,omitempty"`

	// ConvexTemporalBonus is the additive weight for temporal channel in convex fusion.
	// Range [0.0, 1.0]. Default 0.2.
	ConvexTemporalBonus *float64 `json:"convex_temporal_bonus,omitempty"`
}

// ContentSafetyConfig controls the prompt injection scanner.
type ContentSafetyConfig struct {
	// Enabled controls whether the scanner is active. Default: true.
	Enabled *bool `json:"enabled,omitempty"`
	// Mode controls the response when injection patterns are detected.
	// "warn" (default): log + annotate response, allow storage.
	// "truncate": strip matched content before storage.
	// "reject": return error, refuse to store.
	Mode string `json:"mode,omitempty"`
}

// ContentSafetyEnabled returns whether the content safety scanner is enabled.
// Defaults to true when Enabled is nil (not explicitly set in config).
func (c ContentSafetyConfig) ContentSafetyEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ContentSafetyMode returns the configured scanner mode, defaulting to "reject".
// Only "warn", "truncate", and "reject" are valid. Invalid values fall back to "reject".
// Default changed from "warn" to "reject" (BUG-008): warn-mode detects injection
// but stores content unchanged, allowing poisoned memories to persist and infect
// future readers via recall().
func (c ContentSafetyConfig) ContentSafetyMode() string {
	switch c.Mode {
	case "warn", "truncate", "reject":
		return c.Mode
	default:
		return "reject"
	}
}

// ConstitutionConfig holds project-wide principles that are injected into agent
// responses so every LLM session is aware of the architectural laws it must follow.
type ConstitutionConfig struct {
	// Principles is a list of terse statements, e.g. "No CGo", "All handlers fail-silent".
	Principles []string `json:"principles,omitempty"`
	// InjectInContext controls whether principles are appended to get_context compact output.
	// Defaults to true when Principles is non-empty.
	InjectInContext bool `json:"inject_in_context,omitempty"`
	// InjectInSessionInit controls whether principles are included in session_init output.
	// Defaults to true when Principles is non-empty.
	InjectInSessionInit bool `json:"inject_in_session_init,omitempty"`
}

// BrainConfig configures the in-process Thinking Brain (formerly synapses-intelligence sidecar).
// Set Enabled:true to activate LLM-enriched context packets, violation explanations,
// and auto-ingestion. Requires Ollama running locally.
type BrainConfig struct {
	// Enabled controls whether the brain is active. Default: false.
	Enabled bool `json:"enabled"`
	// OllamaURL is the base URL of the Ollama server. Default: "http://localhost:11434".
	OllamaURL string `json:"ollama_url,omitempty"`
	// Model is the primary model tag. Default: "qwen3.5:2b".
	Model string `json:"model,omitempty"`
	// FastModel is the model for bulk ingestion. Default: "qwen3.5:2b".
	FastModel string `json:"fast_model,omitempty"`
	// ModelIngest overrides the model for the ingest tier.
	ModelIngest string `json:"model_ingest,omitempty"`
	// ModelEnrich overrides the model for the enrich tier.
	ModelEnrich string `json:"model_enrich,omitempty"`
	// ModelOrchestrate overrides the model for the orchestrate tier.
	ModelOrchestrate string `json:"model_orchestrate,omitempty"`
	// DBPath overrides the default SQLite path (~/.synapses/brain.sqlite).
	DBPath string `json:"db_path,omitempty"`
	// Ingest enables automatic code summarization on file save. Default: false.
	Ingest bool `json:"ingest"`
	// Enrich enables LLM enrichment of get_context responses. Default: false.
	Enrich bool `json:"enrich"`
	// ContextBuilder enables LLM-assembled context packets. Default: false.
	ContextBuilder bool `json:"context_builder"`
}

// ToBrainConfig converts to the internal brain configuration type used by NewInProcess.
func (b *BrainConfig) ToBrainConfig() *config.BrainConfig {
	return &config.BrainConfig{
		Enabled:          b.Enabled,
		OllamaURL:        b.OllamaURL,
		Model:            b.Model,
		FastModel:        b.FastModel,
		ModelIngest:      b.ModelIngest,
		ModelEnrich:      b.ModelEnrich,
		ModelOrchestrate: b.ModelOrchestrate,
		DBPath:           b.DBPath,
		Ingest:           b.Ingest,
		Enrich:           b.Enrich,
		ContextBuilder:   b.ContextBuilder,
	}
}

// PulseConfig describes the connection to a synapses-pulse analytics sidecar.
type PulseConfig struct {
	// URL is the base URL of the pulse service, e.g. "http://localhost:11437".
	// Leave empty to disable pulse integration.
	URL string `json:"url,omitempty"`
	// TimeoutSec is the per-request HTTP timeout. Defaults to 2 if URL is set.
	// Pulse is fire-and-forget so a short timeout is appropriate.
	TimeoutSec int `json:"timeout_sec,omitempty"`
}

// SessionConfig configures agent session memory behavior.
type SessionConfig struct {
	// AutoEndThresholdCalls is the number of tool calls before the daemon automatically
	// extracts and persists session memory without waiting for an explicit end_session call.
	// When exceeded, a session log is written with source="auto" and the "auto_session_log"
	// tag so it can be filtered if needed. If the agent later calls end_session explicitly,
	// the existing auto-log is reused (touched) rather than creating a duplicate.
	//
	// Default: 0 (disabled). Set to a positive integer to enable. Recommended: 80
	// (roughly 40 minutes of session time at ~2 calls/minute).
	// Example: auto_end_threshold_calls: 80
	AutoEndThresholdCalls int `json:"auto_end_threshold_calls,omitempty"`

	// ReconnectWindowSecs is the number of seconds within which a new session_init
	// from the same agent on the same MCP connection is treated as a reconnect
	// (resume) rather than a new session. This prevents duplicate session rows when
	// an agent restarts or the MCP transport reconnects briefly.
	//
	// Default: 300 (5 minutes). Must be > 0 to enable resume behaviour; set to 0
	// to always create a new session on every session_init call.
	// Example: reconnect_window_secs: 300
	ReconnectWindowSecs int `json:"reconnect_window_secs,omitempty"`

	// StaleThresholdMins is the number of minutes of inactivity after which a
	// session without a clean end_session is surfaced as stale at the next
	// session_init. Stale sessions are advisory — never auto-closed.
	//
	// Default: 30 minutes. Tune down for high-churn teams, up for long-running
	// background agents.
	// Example: stale_threshold_mins: 30
	StaleThresholdMins int `json:"stale_threshold_mins,omitempty"`

	// HibernateWindowSecs is the number of seconds a session can be dormant
	// before it is no longer resumable across a new MCP connection. When an
	// agent calls session_init on a new physical connection (e.g. after
	// restarting the editor or taking a break), Synapses looks for a prior
	// session from the same agent+project within this window and resumes it
	// transparently — carrying over intent, tool call count, and summary.
	//
	// Only sessions older than ReconnectWindowSecs (i.e. not currently live
	// on another connection) are candidates. This prevents two concurrent
	// editor windows from stealing each other's sessions.
	//
	// Default: 0 (uses built-in default of 14400 s / 4 hours). Set to a
	// positive value to override the default window. Set to a negative value
	// (e.g. -1) to disable cross-connection resume entirely.
	// Example: hibernate_window_secs: 7200   # 2-hour window
	// Example: hibernate_window_secs: -1      # disable
	HibernateWindowSecs int `json:"hibernate_window_secs,omitempty"`
}

// RateLimitConfig configures per-session token-bucket rate limiting for
// write operations, expensive reads, and cross-project queries on the MCP
// stdio transport. Limits are applied independently per category.
//
// All values are calls-per-minute. Omitting a field uses the built-in default.
// Set to -1 to disable a category entirely.
//
// Example:
//
//	"rate_limits": {
//	  "write_ops_per_minute": 20,
//	  "expensive_reads_per_minute": 10,
//	  "cross_project_per_minute": 60
//	}
type RateLimitConfig struct {
	// WriteOpsPerMinute is the maximum number of write-category tool calls
	// (remember, send_message, annotate_node, upsert_rule, create_plan) per
	// session per minute. Default: 30. Set to -1 to disable.
	WriteOpsPerMinute int `json:"write_ops_per_minute,omitempty"`

	// ExpensiveReadsPerMinute is the maximum number of recall calls per session
	// per minute. Default: 20. Set to -1 to disable.
	ExpensiveReadsPerMinute int `json:"expensive_reads_per_minute,omitempty"`

	// CrossProjectPerMinute is the maximum number of cross-project queries
	// (any tool called with projects="*" or a specific project name) per
	// session per minute. Default: 60. Set to -1 to disable.
	CrossProjectPerMinute int `json:"cross_project_per_minute,omitempty"`
}

// FederationEntry describes a local sibling project to query across.
// Unlike Peers (HTTP-based remote instances), federation entries use
// direct SQLite read-only access — no running daemon required.
type FederationEntry struct {
	// Path is the filesystem path to the sibling project root.
	// May be absolute or relative to the directory containing synapses.json.
	Path string `json:"path"`
	// Alias is a short name used in MCP tool params and response labels.
	// Must not contain whitespace. Must be unique across all entries.
	Alias string `json:"alias"`
}

// FederationACLConfig controls which daemon-registered projects this project
// can read from via cross-project queries (projects= parameter).
type FederationACLConfig struct {
	// AllowReadFrom is the list of project names (directory basenames) that
	// this project is allowed to query. An empty or nil list means deny-all
	// (no cross-project reads). Use ["*"] to allow all registered projects.
	AllowReadFrom []string `json:"allow_read_from,omitempty"`
}

// IsAllowed returns true if the given project name is permitted by this ACL.
// A nil receiver or empty AllowReadFrom means deny-all.
func (acl *FederationACLConfig) IsAllowed(projectName string) bool {
	if acl == nil || len(acl.AllowReadFrom) == 0 {
		return false
	}
	for _, allowed := range acl.AllowReadFrom {
		if allowed == "*" || allowed == projectName {
			return true
		}
	}
	return false
}

// PluginConfig describes a single external parser plugin.
type PluginConfig struct {
	// Extensions is the list of file extensions this plugin handles (e.g. [".prisma", ".graphql"]).
	// The leading dot is required. If two plugins claim the same extension the last one wins.
	Extensions []string `json:"extensions"`
	// Command is the shell command to execute, e.g. "./parsers/prisma-parser" or
	// "node parsers/graphql.js". Words are split on whitespace; use a wrapper
	// script for paths containing spaces.
	Command string `json:"command"`
}

// ApiEntryPattern specifies a custom pattern for identifying API entry points.
// Used in the api_entries config block as a supplement to convention detection.
// All non-empty fields are ANDed together (every specified field must match).
type ApiEntryPattern struct {
	// NamePattern is a case-insensitive substring matched against the entity name.
	NamePattern string `json:"name_pattern,omitempty"`
	// FilePattern is a glob matched against the file base name, or a substring
	// matched against the full path for directory-style patterns (e.g. */handlers/*).
	FilePattern string `json:"file_pattern,omitempty"`
	// NodeType restricts to a specific entity type: function, method, etc.
	NodeType graph.NodeType `json:"node_type,omitempty"`
}

// DataFlowPattern specifies a custom source or sink node for data-flow analysis.
// Role must be "source" or "sink". All non-empty fields are ANDed together.
type DataFlowPattern struct {
	// NamePattern is a case-insensitive substring matched against the entity name.
	NamePattern string `json:"name_pattern,omitempty"`
	// SigPattern is a substring matched against the node's signature metadata.
	SigPattern string `json:"sig_pattern,omitempty"`
	// FilePattern is a glob matched against the file base name.
	FilePattern string `json:"file_pattern,omitempty"`
	// NodeType restricts to a specific entity type: function or method.
	NodeType graph.NodeType `json:"node_type,omitempty"`
	// Role is "source" (data entry) or "sink" (dangerous data exit).
	Role string `json:"role"`
	// Label is a human-readable description, e.g. "http_input" or "sql_sink".
	Label string `json:"label,omitempty"`
}

// Rule describes a single architectural constraint.
type Rule struct {
	// ID is a unique, human-readable identifier for this rule.
	ID string `json:"id"`
	// Description explains what the rule prevents and why.
	Description string `json:"description"`
	// ForbiddenEdge describes the edge pattern that must never exist.
	// Empty for agent-type rules (no code-graph check).
	ForbiddenEdge ForbiddenEdge `json:"forbidden_edge"`
	// Severity is one of "error" or "warning".
	Severity string `json:"severity"`
	// RuleType distinguishes structural rules (code-graph enforcement) from
	// agent rules (behavioral constraints surfaced in session_init).
	// Values: "structural" (default) or "agent".
	RuleType string `json:"rule_type,omitempty"`
}

// IsAgentRule reports whether this rule is a behavioral agent constraint
// (no code-graph check, surfaced in session_init instead of violations).
func (r Rule) IsAgentRule() bool {
	return r.RuleType == "agent"
}

// ForbiddenEdge specifies a pattern for edges that must not exist in the graph.
// All non-empty fields are ANDed together (every specified field must match).
type ForbiddenEdge struct {
	// FromFilePattern matches the source node's file path (glob).
	FromFilePattern string `json:"from_file_pattern,omitempty"`
	// ToFilePattern matches the target node's file path (glob).
	ToFilePattern string `json:"to_file_pattern,omitempty"`
	// FromType restricts the source node type.
	FromType graph.NodeType `json:"from_type,omitempty"`
	// ToType restricts the target node type.
	ToType graph.NodeType `json:"to_type,omitempty"`
	// EdgeType restricts the relationship type.
	EdgeType graph.EdgeType `json:"edge_type,omitempty"`
	// ToNamePattern is a substring that must appear in the target node's name.
	ToNamePattern string `json:"to_name_pattern,omitempty"`
}

// ContextCarveConfig holds project-level defaults for context carving.
type ContextCarveConfig struct {
	// DefaultDepth is the BFS hop limit when depth is not specified by the caller.
	DefaultDepth int `json:"default_depth,omitempty"`
	// DecayFactor controls relevance falloff per hop (0 < factor ≤ 1).
	DecayFactor float64 `json:"decay_factor,omitempty"`
	// TokenBudget caps the output size in approximate tokens.
	TokenBudget int `json:"token_budget,omitempty"`
	// MinRelevance is the minimum score for a node to appear in the output.
	// Nodes below this threshold are pruned before the token budget is applied.
	MinRelevance float64 `json:"min_relevance,omitempty"`
	// ExcludeTestFiles omits _test.go nodes from get_context output.
	// Defaults to true; set to false to include test nodes in context.
	ExcludeTestFiles *bool `json:"exclude_test_files,omitempty"`
	// DirectionBoost is a relevance multiplier (e.g. 0.2 = +20%) applied to
	// nodes reached via outgoing CALLS edges (callees). Higher values make the
	// token-budget pruner prefer forward call dependencies over callers.
	// Default: 0.2. Set to 0 to disable.
	DirectionBoost float64 `json:"direction_boost,omitempty"`
	// HybridLambda controls the semantic blend weight for hybrid scoring:
	//   finalScore = (1-λ)×structural + λ×cosineSim(embed(root), embed(n))
	// Range [0, 1]. nil = not configured (server applies default of 0.3).
	// Set to 0.0 to disable hybrid scoring entirely (pure structural, fastest).
	// Set to any value in (0, 1] to use that blend ratio.
	// Only applied when node embeddings are available; otherwise ignored.
	// Uses *float64 (pointer) so that explicit 0 is distinguishable from unset.
	HybridLambda *float64 `json:"hybrid_lambda,omitempty"`
	// UsePPR enables Personalized PageRank traversal in get_context instead of
	// the default BFS heuristic. PPR captures multi-path importance — a node
	// reached by N independent call paths scores proportionally higher than a
	// single-path node at the same structural distance. Validated by Sprint 13
	// spike: 4.69× diamond boost, 5.68× wide-fan boost over BFS max-score.
	//
	// Default: true. Set to false to revert to BFS (e.g. for debugging or on
	// very small repos where PPR overhead is unnecessary). Uses *bool so that
	// explicit false is distinguishable from unset (which defaults to true).
	UsePPR *bool `json:"use_ppr,omitempty"`
	// PPRAlpha is the PPR teleport probability — the chance the random walk
	// restarts from the root at each step. Range (0, 1).
	//   Lower alpha → broader reach, more global importance captured.
	//   Higher alpha → tighter focus on root, shorter effective reach.
	// Default: 0.15 (standard PageRank restart rate, validated in spike tests).
	// Values outside (0, 1) are clamped to 0.15. Only used when use_ppr=true.
	PPRAlpha float64 `json:"ppr_alpha,omitempty"`
}

// Violation records a detected rule breach.
type Violation struct {
	RuleID      string         `json:"rule_id"`
	Severity    string         `json:"severity"`
	Description string         `json:"description"`
	FromNode    graph.NodeID   `json:"from_node"`
	ToNode      graph.NodeID   `json:"to_node"`
	EdgeType    graph.EdgeType `json:"edge_type"`
	// SuggestedFix is a concise, actionable refactoring hint generated from the
	// rule pattern and the names of the two nodes involved in the violation.
	SuggestedFix string `json:"suggested_fix,omitempty"`
	// Explanation is a plain-English LLM-generated explanation of why this
	// violation matters, populated when synapses-intelligence is configured.
	Explanation string `json:"explanation,omitempty"`
}

// FindConfigDir walks upward from start looking for a directory that contains
// synapses.json, stopping at the filesystem root. It mirrors the way git locates
// .git so that "synapses start --path backend/handlers" can still load a
// synapses.json that lives at the repository root.
//
// Returns (dir, true) where dir is the first ancestor (inclusive) that contains
// synapses.json. Returns (start, false) when no such directory is found.
func FindConfigDir(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return start, false
	}
	for {
		candidate := filepath.Join(dir, configFileName)
		if _, err := os.Stat(candidate); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding the file.
			return start, false
		}
		dir = parent
	}
}

// Load reads synapses.json from the given directory. If the file does not
// exist, a default (empty rules) config is returned without error.
func Load(dir string) (*Config, error) {
	path := filepath.Join(dir, configFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg := defaultConfig()
		// No synapses.json — user has not explicitly set anything.
		// Auto-enable use_go_types for Go projects.
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			cfg.UseGoTypes = true
		}
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// BUG-035: detect unknown top-level keys so typos like "contex_carve"
	// are flagged instead of silently ignored.
	warnUnknownKeys(data)

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	cfg.applyDefaults()

	// FIX-RESOLVER-1: auto-enable use_go_types for Go projects unless the user
	// explicitly set it in synapses.json. A plain bool can't distinguish
	// "not set" from "set to false", so we re-parse the raw JSON with a *bool
	// to detect the explicit-false case and respect the user's intent.
	if !cfg.UseGoTypes {
		var rawGoTypes struct {
			UseGoTypes *bool `json:"use_go_types"`
		}
		if json.Unmarshal(data, &rawGoTypes) == nil && rawGoTypes.UseGoTypes == nil {
			// Field absent from JSON — apply go.mod auto-default.
			if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
				cfg.UseGoTypes = true
			}
		}
	}

	// Resolve relative Linked paths against the directory that holds the config.
	for i, p := range cfg.Linked {
		if !filepath.IsAbs(p) {
			cfg.Linked[i] = filepath.Join(dir, p)
		}
	}

	// Resolve relative Federation paths against the directory that holds the config.
	for i, f := range cfg.Federation {
		if !filepath.IsAbs(f.Path) {
			cfg.Federation[i].Path = filepath.Join(dir, f.Path)
		}
	}
	return &cfg, nil
}

// knownTopLevelKeys lists all valid top-level JSON keys in synapses.json.
// Used by warnUnknownKeys to detect typos (BUG-035).
var knownTopLevelKeys = map[string]bool{
	"version": true, "mode": true, "rules": true, "edge_weights": true,
	"context_carve": true, "linked": true, "embedding_endpoint": true,
	"embeddings": true, "api_entries": true, "use_go_types": true,
	"use_ts_types": true, "metrics_days": true, "coverage_profile": true,
	"pprof_profile": true, "data_flow_sources": true, "data_flow_sinks": true,
	"data_flow_max_hops": true, "federation": true, "federation_acl": true,
	"constitution": true, "brain": true, "pulse": true, "session": true,
	"rate_limits": true, "content_safety": true, "recall": true,
}

// warnUnknownKeys parses raw JSON to detect top-level keys not recognized
// by the Config struct. Emits a warning for each, helping users catch typos
// like "contex_carve" that would otherwise be silently ignored (BUG-035).
func warnUnknownKeys(data []byte) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return // can't parse — validation will catch it
	}
	for key := range raw {
		if !knownTopLevelKeys[key] {
			logutil.Warn("synapses: config: unknown key %q in synapses.json (typo?)\n", key)
		}
	}
}

// CarveConfig converts the project-level carving settings into a
// graph.CarveConfig that can be passed directly to graph.Graph.CarveEgoGraph.
func (c *Config) CarveConfig() graph.CarveConfig {
	cfg := graph.DefaultCarveConfig()
	if c.ContextCarve.DefaultDepth > 0 {
		cfg.MaxDepth = c.ContextCarve.DefaultDepth
	}
	if c.ContextCarve.DecayFactor > 0 {
		cfg.DecayFactor = c.ContextCarve.DecayFactor
	}
	if c.ContextCarve.TokenBudget > 0 {
		cfg.TokenBudget = c.ContextCarve.TokenBudget
	}
	if c.ContextCarve.MinRelevance > 0 {
		cfg.MinRelevance = c.ContextCarve.MinRelevance
	}
	if c.ContextCarve.ExcludeTestFiles != nil {
		cfg.ExcludeTestFiles = *c.ContextCarve.ExcludeTestFiles
	}
	if c.ContextCarve.DirectionBoost != 0 {
		cfg.DirectionBoost = c.ContextCarve.DirectionBoost
	} else {
		cfg.DirectionBoost = 0.2 // default: 20% callee preference
	}
	// HybridLambda: nil means "not configured" — handled in the MCP layer where
	// the store is available and a default of 0.3 is applied. Explicit *float64
	// values (including 0.0 to disable) are passed through via CarveConfig.HybridLambda.
	// handlers_context.go reads s.config.ContextCarve.HybridLambda directly to
	// preserve the nil / explicit-zero distinction.
	if c.ContextCarve.HybridLambda != nil {
		cfg.HybridLambda = *c.ContextCarve.HybridLambda
	}
	if c.ContextCarve.UsePPR != nil {
		cfg.UsePPR = *c.ContextCarve.UsePPR
	}
	if c.ContextCarve.PPRAlpha > 0 {
		cfg.Alpha = c.ContextCarve.PPRAlpha
	}
	if len(c.EdgeWeights) > 0 {
		cfg.EdgeWeights = make(map[graph.EdgeType]float64, len(c.EdgeWeights))
		for k, v := range c.EdgeWeights {
			cfg.EdgeWeights[k] = v
		}
	}
	return cfg
}

// CheckViolations scans all edges in g and returns any that match a forbidden
// edge pattern defined in the config rules.
func (c *Config) CheckViolations(g *graph.Graph) []Violation {
	if len(c.Rules) == 0 {
		return nil
	}

	edges := g.AllEdges()
	var violations []Violation

	for _, e := range edges {
		fromNode := g.GetNode(e.From)
		toNode := g.GetNode(e.To)
		if fromNode == nil || toNode == nil {
			continue
		}
		for _, rule := range c.Rules {
			if matchesForbidden(rule.ForbiddenEdge, e, fromNode, toNode) {
				violations = append(violations, Violation{
					RuleID:       rule.ID,
					Severity:     rule.Severity,
					Description:  rule.Description,
					FromNode:     e.From,
					ToNode:       e.To,
					EdgeType:     e.Type,
					SuggestedFix: suggestFix(rule, e.Type, fromNode, toNode),
				})
			}
		}
	}
	return violations
}

// CheckViolationsForFile is a scoped variant of CheckViolations that only
// inspects edges where at least one endpoint belongs to the given file path.
// Used by the watcher to detect new violations after an incremental re-parse
// without scanning the entire graph.
func (c *Config) CheckViolationsForFile(g *graph.Graph, file string) []Violation {
	if len(c.Rules) == 0 {
		return nil
	}

	edges := g.AllEdges()
	var violations []Violation

	for _, e := range edges {
		fromNode := g.GetNode(e.From)
		toNode := g.GetNode(e.To)
		if fromNode == nil || toNode == nil {
			continue
		}
		// Only examine edges where at least one endpoint is in the changed file.
		if fromNode.File != file && toNode.File != file {
			continue
		}
		for _, rule := range c.Rules {
			if matchesForbidden(rule.ForbiddenEdge, e, fromNode, toNode) {
				violations = append(violations, Violation{
					RuleID:       rule.ID,
					Severity:     rule.Severity,
					Description:  rule.Description,
					FromNode:     e.From,
					ToNode:       e.To,
					EdgeType:     e.Type,
					SuggestedFix: suggestFix(rule, e.Type, fromNode, toNode),
				})
			}
		}
	}
	return violations
}

// CheckViolationsForEdges checks a specific set of edges (typically from a
// carved subgraph) against all rules. getNode resolves NodeIDs to *Node for
// pattern matching. This avoids the AllEdges() allocation of CheckViolations
// when only a subset of edges needs checking.
func (c *Config) CheckViolationsForEdges(edges []*graph.Edge, getNode func(graph.NodeID) *graph.Node) []Violation {
	if len(c.Rules) == 0 || len(edges) == 0 {
		return nil
	}
	var violations []Violation
	for _, e := range edges {
		fromNode := getNode(e.From)
		toNode := getNode(e.To)
		if fromNode == nil || toNode == nil {
			continue
		}
		for _, rule := range c.Rules {
			if matchesForbidden(rule.ForbiddenEdge, e, fromNode, toNode) {
				violations = append(violations, Violation{
					RuleID:       rule.ID,
					Severity:     rule.Severity,
					Description:  rule.Description,
					FromNode:     e.From,
					ToNode:       e.To,
					EdgeType:     e.Type,
					SuggestedFix: suggestFix(rule, e.Type, fromNode, toNode),
				})
			}
		}
	}
	return violations
}

// matchFilePath returns true if filePath matches pattern.
// It tries progressively shorter path suffixes so that path-component patterns
// like "*/mcp/*" correctly match "synapses/internal/mcp/tools.go", while simple
// basename patterns like "*.tsx" still work via the first iteration.
func matchFilePath(pattern, filePath string) bool {
	p := filepath.ToSlash(filePath)
	for {
		if matched, _ := filepath.Match(pattern, p); matched {
			return true
		}
		idx := strings.IndexByte(p, '/')
		if idx < 0 {
			break
		}
		p = p[idx+1:]
	}
	return false
}

// matchesForbidden returns true if the given edge matches the forbidden pattern.
// All non-empty pattern fields must match for the rule to fire.
func matchesForbidden(p ForbiddenEdge, e *graph.Edge, from, to *graph.Node) bool {
	// An all-empty ForbiddenEdge means no code-graph check (agent/behavioral rule).
	// Without this guard, every field check would be skipped and the function
	// would return true for every edge, generating spurious violations.
	if p.EdgeType == "" && p.FromType == "" && p.ToType == "" &&
		p.FromFilePattern == "" && p.ToFilePattern == "" && p.ToNamePattern == "" {
		return false
	}
	if p.EdgeType != "" && e.Type != p.EdgeType {
		return false
	}
	if p.FromType != "" && from.Type != p.FromType {
		return false
	}
	if p.ToType != "" && to.Type != p.ToType {
		return false
	}
	if p.FromFilePattern != "" {
		if !matchFilePath(p.FromFilePattern, from.File) {
			return false
		}
	}
	if p.ToFilePattern != "" {
		if !matchFilePath(p.ToFilePattern, to.File) {
			return false
		}
	}
	if p.ToNamePattern != "" {
		if to.Name != p.ToNamePattern && !globContains(to.Name, p.ToNamePattern) {
			return false
		}
	}
	return true
}

func (c *Config) validate() error {
	for i, r := range c.Rules {
		if r.ID == "" {
			return fmt.Errorf("rule[%d]: id is required", i)
		}
		if r.Severity != "error" && r.Severity != "warning" {
			return fmt.Errorf("rule %q: severity must be 'error' or 'warning'", r.ID)
		}
	}
	// Validate federation entries: alias must be non-empty and unique.
	seen := make(map[string]bool, len(c.Federation))
	for i, f := range c.Federation {
		if f.Alias == "" {
			return fmt.Errorf("federation[%d]: alias is required", i)
		}
		if f.Path == "" {
			return fmt.Errorf("federation[%d] %q: path is required", i, f.Alias)
		}
		if strings.ContainsAny(f.Alias, " \t\n\r") {
			return fmt.Errorf("federation[%d] %q: alias must not contain whitespace", i, f.Alias)
		}
		if seen[f.Alias] {
			return fmt.Errorf("federation[%d] %q: duplicate alias", i, f.Alias)
		}
		seen[f.Alias] = true
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.Version == "" {
		c.Version = "1"
	}
	if c.ContextCarve.DefaultDepth == 0 {
		c.ContextCarve.DefaultDepth = 2
	}
	if c.ContextCarve.DecayFactor == 0 {
		c.ContextCarve.DecayFactor = 0.5
	}
	if c.ContextCarve.TokenBudget == 0 {
		c.ContextCarve.TokenBudget = 4000
	}
	// Constitution defaults: if principles are set but inject flags are false, default both to true.
	if len(c.Constitution.Principles) > 0 {
		if !c.Constitution.InjectInContext {
			c.Constitution.InjectInContext = true
		}
		if !c.Constitution.InjectInSessionInit {
			c.Constitution.InjectInSessionInit = true
		}
	}
	// Brain defaults: apply sensible defaults when brain is enabled.
	if c.Brain.Enabled {
		if c.Brain.OllamaURL == "" {
			c.Brain.OllamaURL = "http://localhost:11434"
		}
		if c.Brain.Model == "" {
			c.Brain.Model = "qwen3.5:2b"
		}
	}
}

func defaultConfig() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

// globContains is a permissive match: checks if pattern is a substring of name.
// For the MVP this is sufficient; full glob matching can be added later.
func globContains(name, pattern string) bool {
	return len(pattern) > 0 && len(name) >= len(pattern) &&
		containsStr(name, pattern)
}

func containsStr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// suggestFix produces a concise, actionable refactoring hint for a rule
// violation based on the edge type, the rule pattern, and the two nodes
// involved. The hint is intentionally short — one sentence — so it fits
// comfortably in get_violations output and IDE tooltips.
func suggestFix(rule Rule, edgeType graph.EdgeType, from, to *graph.Node) string {
	fromFile := filepath.Base(from.File)
	toFile := filepath.Base(to.File)
	fromName := from.Name
	toName := to.Name

	p := rule.ForbiddenEdge

	switch edgeType {
	case graph.EdgeCalls:
		// Handler → DB / service direct call pattern.
		if containsStr(fromFile, "handler") || containsStr(fromFile, "controller") ||
			containsStr(fromFile, "route") || containsStr(fromFile, "view") {
			if containsStr(toFile, "db") || containsStr(toFile, "repo") ||
				containsStr(toFile, "store") || containsStr(toFile, "query") ||
				containsStr(toName, "Query") || containsStr(toName, "Exec") ||
				containsStr(toName, "Select") || containsStr(toName, "Insert") {
				return "Introduce a service or repository layer: move the call to " +
					toName + " out of " + fromFile + " into a dedicated service, then call the service from the handler."
			}
			return "Extract the call to " + toName + " into a service layer rather than calling it directly from " + fromFile + "."
		}
		// Generic cross-layer CALLS violation.
		if p.ToNamePattern != "" {
			return "Avoid calling " + p.ToNamePattern + " directly from " + fromFile +
				". Introduce an abstraction (interface or service) between " + fromFile + " and " + toFile + "."
		}
		return "Move the dependency on " + toName + " out of " + fromName +
			" by introducing an intermediate layer (service, repository, or interface)."

	case graph.EdgeImports:
		// Direct import between layers that should be decoupled.
		if containsStr(fromFile, "handler") || containsStr(fromFile, "controller") {
			return "Replace the direct import of " + toFile + " in " + fromFile +
				" with an interface or service abstraction so the handler layer stays decoupled from implementation details."
		}
		if p.ToFilePattern != "" && p.FromFilePattern != "" {
			return "Decouple " + fromFile + " from " + toFile +
				": introduce an interface in a shared package that both sides depend on, then invert the dependency."
		}
		return "Replace the direct import of " + toFile + " in " + fromFile +
			" with an interface or dependency injection to reduce coupling."

	default:
		// Fallback for EMBEDS, DEPENDS_ON, etc.
		return "Remove the direct " + string(edgeType) + " relationship from " +
			fromName + " to " + toName + " and replace it with a looser coupling (interface, event, or message)."
	}
}

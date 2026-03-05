// Package config loads and validates the synapses.json project configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

const configFileName = "synapses.json"

// Config is the root structure of synapses.json.
type Config struct {
	// Version is the config schema version. Currently "1".
	Version string `json:"version"`
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

	// Plugins registers external parser plugins for file extensions not handled
	// by built-in parsers. Each plugin is a process that reads a file path on
	// the first line of stdin followed by the raw file content, and writes a
	// single JSON object {"nodes":[...],"edges":[...]} to stdout.
	Plugins []PluginConfig `json:"plugins,omitempty"`

	// PeerAPIPort is the port this synapses instance listens on for incoming
	// peer queries. Set to 0 (default) to disable the peer API server.
	PeerAPIPort int `json:"peer_api_port,omitempty"`

	// PeerAPIToken is the bearer token that remote peers must supply in the
	// Authorization header when calling this instance's peer API. May also be
	// set via the SYNAPSES_PEER_API_TOKEN environment variable (env wins).
	PeerAPIToken string `json:"peer_api_token,omitempty"`

	// Peers is the list of remote synapses instances this project connects to.
	Peers []PeerConfig `json:"peers,omitempty"`

	// Constitution defines project-wide principles that are injected into every
	// agent session and get_context response. Use this to codify architectural
	// laws, coding standards, and constraints that every AI agent must respect.
	Constitution ConstitutionConfig `json:"constitution,omitempty"`

	// Brain configures the optional synapses-intelligence integration.
	// When set, get_context returns LLM-enriched Context Packets, violations
	// include plain-English explanations, and file changes are auto-ingested.
	Brain BrainConfig `json:"brain,omitempty"`

	// Scout configures the optional synapses-scout integration.
	// When set, web_search, web_fetch, and web_deep_search MCP tools become
	// available, giving AI agents real-time web data through the scout sidecar.
	Scout ScoutConfig `json:"scout,omitempty"`
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

// BrainConfig describes the connection to a synapses-intelligence sidecar.
type BrainConfig struct {
	// URL is the base URL of the intelligence service, e.g. "http://localhost:11435".
	// Leave empty to disable brain integration.
	URL string `json:"url,omitempty"`
	// TimeoutSec is the per-request HTTP timeout. Defaults to 5 if URL is set.
	TimeoutSec int `json:"timeout_sec,omitempty"`
	// EnableLLM controls whether the context-packet endpoint is allowed to call
	// the local LLM. Defaults to true when URL is set.
	EnableLLM bool `json:"enable_llm,omitempty"`
}

// ScoutConfig describes the connection to a synapses-scout sidecar.
type ScoutConfig struct {
	// URL is the base URL of the scout service, e.g. "http://localhost:11436".
	// Leave empty to disable scout integration.
	URL string `json:"url,omitempty"`
	// TimeoutSec is the per-request HTTP timeout. Defaults to 30 if URL is set.
	// Scout fetches real web pages so a generous timeout is required.
	TimeoutSec int `json:"timeout_sec,omitempty"`
}

// PeerConfig describes a remote synapses peer instance to connect to.
type PeerConfig struct {
	// Name is a short, unique identifier used in MCP tool params and env var
	// lookup. Example: "backend". Must not contain whitespace.
	Name string `json:"name"`
	// URL is the base URL of the peer's peer API. Example: "http://localhost:8767".
	URL string `json:"url"`
	// Token is the bearer token to include in requests to this peer. May be
	// left empty and supplied via the SYNAPSES_PEER_TOKEN_<NAME> environment
	// variable instead (env always wins over the JSON value).
	Token string `json:"token,omitempty"`
	// TrustLevel controls which peer API endpoints are accessible.
	// "full" allows queries + claims; "read_only" allows queries only.
	// Defaults to "read_only" if empty.
	TrustLevel string `json:"trust_level,omitempty"`
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
	ForbiddenEdge ForbiddenEdge `json:"forbidden_edge"`
	// Severity is one of "error" or "warning".
	Severity string `json:"severity"`
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
		return defaultConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	cfg.applyDefaults()

	// Resolve env var tokens for peers. SYNAPSES_PEER_TOKEN_<NAME> always wins
	// over the JSON value, making it safe for CI/CD environments.
	replacer := strings.NewReplacer("-", "_", ".", "_", " ", "_")
	for i, p := range cfg.Peers {
		envKey := "SYNAPSES_PEER_TOKEN_" + strings.ToUpper(replacer.Replace(p.Name))
		if tok := os.Getenv(envKey); tok != "" {
			cfg.Peers[i].Token = tok
		}
	}
	// Resolve incoming token: env var beats JSON.
	if tok := os.Getenv("SYNAPSES_PEER_API_TOKEN"); tok != "" {
		cfg.PeerAPIToken = tok
	}

	// Resolve relative Linked paths against the directory that holds the config.
	for i, p := range cfg.Linked {
		if !filepath.IsAbs(p) {
			cfg.Linked[i] = filepath.Join(dir, p)
		}
	}
	return &cfg, nil
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

// matchesForbidden returns true if the given edge matches the forbidden pattern.
// All non-empty pattern fields must match for the rule to fire.
func matchesForbidden(p ForbiddenEdge, e *graph.Edge, from, to *graph.Node) bool {
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
		matched, _ := filepath.Match(p.FromFilePattern, filepath.Base(from.File))
		if !matched {
			return false
		}
	}
	if p.ToFilePattern != "" {
		matched, _ := filepath.Match(p.ToFilePattern, filepath.Base(to.File))
		if !matched {
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
	for i := range c.Peers {
		if c.Peers[i].TrustLevel == "" {
			c.Peers[i].TrustLevel = "read_only"
		}
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
	// Brain defaults: if URL is set but other fields are zero, apply sensible defaults.
	if c.Brain.URL != "" {
		if c.Brain.TimeoutSec <= 0 {
			c.Brain.TimeoutSec = 5
		}
		if !c.Brain.EnableLLM {
			c.Brain.EnableLLM = true // default true when URL is provided
		}
	}
	// Scout defaults: if URL is set but timeout is zero, apply sensible default.
	if c.Scout.URL != "" {
		if c.Scout.TimeoutSec <= 0 {
			c.Scout.TimeoutSec = 30
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

// Package graph implements the core in-memory graph engine for Synapses.
// It stores code entities (nodes) and their relationships (edges), and provides
// BFS-based context carving with edge-type-weighted relevance decay.
package graph

// NodeType classifies what kind of code entity a node represents.
type NodeType string

// Node type constants: classify what kind of code entity a node represents.
const (
	NodeFile      NodeType = "file"
	NodePackage   NodeType = "package"
	NodeFunction  NodeType = "function"
	NodeMethod    NodeType = "method"
	NodeStruct    NodeType = "struct"
	NodeInterface NodeType = "interface"
	NodeVariable  NodeType = "variable"
	// NodeRoute is a virtual node injected by the heuristic pass (R1).
	// It represents an HTTP/RPC route registration (e.g. "GET /api/users").
	// Not present in the AST — synthesised from framework registration patterns.
	NodeRoute NodeType = "route"
)

// EdgeType classifies the relationship between two nodes.
type EdgeType string

// Edge type constants: classify the relationship between two graph nodes.
const (
	EdgeImports    EdgeType = "IMPORTS"
	EdgeCalls      EdgeType = "CALLS"
	EdgeImplements EdgeType = "IMPLEMENTS"
	EdgeDefines    EdgeType = "DEFINES"
	EdgeEmbeds     EdgeType = "EMBEDS"
	EdgeDependsOn  EdgeType = "DEPENDS_ON"
	EdgeExports    EdgeType = "EXPORTS"
	EdgeDataFlows  EdgeType = "DATA_FLOWS"
	// EdgeHandles is a synthetic edge injected by the heuristic pass (R1).
	// Direction: routeNode --HANDLES--> handlerFunction.
	// Represents framework routing registration: "this route dispatches to this handler."
	// Confidence is stored in the route node's metadata (key "confidence").
	EdgeHandles EdgeType = "HANDLES"
)

// DefaultEdgeWeights defines the semantic significance of each edge type.
// Higher weight = more relevant when carving context. Configurable via synapses.json.
//
// EdgeDefines is intentionally low (0.15) because file→entity DEFINES edges would
// otherwise turn every file node into a high-relevance hub, equalising all siblings
// in a file with equal — and misleading — relevance scores.
var DefaultEdgeWeights = map[EdgeType]float64{
	EdgeCalls:      1.0,
	EdgeDataFlows:  0.95,
	EdgeImplements: 0.9,
	EdgeEmbeds:     0.85,
	EdgeDependsOn:  0.8,
	EdgeImports:    0.7,
	EdgeExports:    0.5,
	EdgeDefines:    0.15,
	// EdgeHandles: inferred framework routing edges. High weight so that
	// route→handler paths surface prominently in context carving.
	EdgeHandles: 0.9,
}

// NodeID is a composite identifier with the format: "repoID::file::name".
// Using a named type (not a plain string) enforces intent at compile time.
type NodeID string

// DomainType classifies which knowledge domain a graph node belongs to.
// Code is the default domain; future parsers and connectors set other domains
// so that infrastructure, API, doc, and issue nodes can coexist in the same graph.
type DomainType string

const (
	// DomainCode is the default: source-code entities (functions, structs, etc.).
	DomainCode DomainType = "code"
	// DomainInfra represents infrastructure resources (Terraform, k8s, Docker).
	DomainInfra DomainType = "infra"
	// DomainAPI represents API schema entities (OpenAPI endpoints, gRPC services).
	DomainAPI DomainType = "api"
	// DomainDocs represents documentation sections (Markdown, wikis).
	DomainDocs DomainType = "docs"
	// DomainIssues represents external tickets and issues (GitHub, Linear, Jira).
	DomainIssues DomainType = "issues"
	// DomainCustom is a catch-all for user-defined domain parsers and connectors.
	DomainCustom DomainType = "custom"
)

// ProvenanceType classifies the trust tier of a graph node.
// Derived at index time from file path patterns and content headers — no LLM needed.
type ProvenanceType string

const (
	// ProvenanceUserAuthored is the default: files written by the user/team.
	ProvenanceUserAuthored ProvenanceType = "user-authored"
	// ProvenanceGenerated marks auto-generated files (protobuf, codegen, mocks).
	ProvenanceGenerated ProvenanceType = "generated"
	// ProvenanceVendored marks third-party dependency files (vendor/, node_modules/).
	ProvenanceVendored ProvenanceType = "vendored"
	// ProvenanceExternal marks content ingested from the web via scout sidecar.
	//
	// ARCHITECTURAL NOTE: This constant is defined and wired into the BFS weight
	// system (weight 0.2 — lowest tier) and the digest display layer, but it is
	// never set by any current code path. web_annotate() attaches web findings as
	// annotations on existing graph nodes — it does not create new NodeWebContent
	// nodes. A future implementation would create dedicated web-content nodes
	// tagged ProvenanceExternal when ingesting scout results. Until then this
	// constant is intentionally unused — do not remove it.
	ProvenanceExternal ProvenanceType = "external"
)

// Node represents a single code entity in the graph.
type Node struct {
	ID       NodeID            `json:"id"`
	Type     NodeType          `json:"type"`
	Name     string            `json:"name"`
	Package  string            `json:"package"`
	File     string            `json:"file"`
	Line     int               `json:"line"`
	Exported bool              `json:"exported"`
	Metadata map[string]string `json:"metadata,omitempty"`
	// StableID is a UUID v4 assigned on first creation and preserved across
	// file renames and incremental re-parses. It provides a stable cross-project
	// reference that does not change when a file is moved. Generated by
	// Graph.AddNode if empty; migrated by Watcher.reparseFile via MigrateStableID.
	StableID string `json:"stable_id,omitempty"`
	// Provenance classifies the trust tier of this node's source file.
	// Derived at index time; defaults to ProvenanceUserAuthored ("").
	// Used by BFS ranking (user-authored nodes surface first) and as a
	// Semantic Firewall gate on high-privilege operations.
	Provenance ProvenanceType `json:"provenance,omitempty"`
	// Domain classifies which knowledge domain this node belongs to.
	// Defaults to DomainCode ("code") for all source-code entities.
	// Future domain parsers (infra, api, docs, issues) set this at index time
	// so that non-code nodes coexist in the same graph without ambiguity.
	// An empty string is treated as DomainCode everywhere in the codebase.
	Domain DomainType `json:"domain,omitempty"`
}

// Edge represents a directed relationship between two nodes.
type Edge struct {
	From NodeID   `json:"from"`
	To   NodeID   `json:"to"`
	Type EdgeType `json:"type"`
}

// CallSite records an unresolved function call encountered during parsing.
// The resolver drains these after all files are parsed and creates CALLS edges.
type CallSite struct {
	CallerID   NodeID // node ID of the calling function/method
	CallerFile string // absolute path of the file containing the caller
	PkgAlias   string // "" for direct calls; "pkg" for pkg.Func() qualified calls
	FuncName   string // name of the function/method being called
}

// CarveConfig controls how an ego-subgraph is extracted for a query node.
type CarveConfig struct {
	// MaxDepth is the maximum number of hops from the root node.
	MaxDepth int
	// TokenBudget caps the approximate output size in tokens (1 token ≈ 4 chars).
	TokenBudget int
	// EdgeWeights overrides DefaultEdgeWeights. Nil means use defaults.
	EdgeWeights map[EdgeType]float64
	// DecayFactor is multiplied per hop: relevance = weight × (decay ^ hop).
	DecayFactor float64
	// MinRelevance drops any node whose relevance score falls below this threshold
	// before the token-budget cut is applied. Prevents low-signal siblings and
	// package-import nodes from crowding out actual dependencies.
	MinRelevance float64
	// ExcludeTypes lists node types to omit from the response. These nodes are
	// still traversed during BFS (so edges through them are discovered) but are
	// never emitted to the caller. Defaults to {NodePackage, NodeFile} so that
	// stdlib imports and file hub-nodes do not waste the token budget.
	ExcludeTypes map[NodeType]bool
	// ExcludeTestFiles omits nodes whose source file ends in _test.go from the
	// output. The nodes are still BFS-traversed (so their edges are discovered)
	// but they are never emitted to the caller. Defaults to true so that test
	// functions do not crowd the related bucket for well-tested codebases.
	ExcludeTestFiles bool
	// DirectionBoost applies a relevance multiplier along the CALLS direction.
	// Positive: boosts outgoing (callee) edges — token-budget pruner prefers
	// what this node calls. Negative: boosts incoming (caller) edges — pruner
	// prefers what calls this node. 0 disables directional preference. Default: 0.2.
	DirectionBoost float64
	// IntentID is an optional cache-key discriminator for intent-specific configs.
	// When EdgeWeights are overridden per-intent, set this to the intent string
	// (e.g. "modify", "debug") so that intent-specific subgraphs are cached
	// separately and do not collide with the default or other intents.
	IntentID string
}

// intentModifyWeights boosts outgoing CALLS (callees) for the "modify" intent.
// Agents modifying code need to see what they will break (dependencies).
// IMPLEMENTS is reduced — the focus is behavioral, not contractual.
var intentModifyWeights = map[EdgeType]float64{
	EdgeCalls:      1.0,
	EdgeDataFlows:  0.95,
	EdgeImplements: 0.6,
	EdgeEmbeds:     0.75,
	EdgeDependsOn:  0.8,
	EdgeImports:    0.6,
	EdgeExports:    0.4,
	EdgeDefines:    0.15,
	EdgeHandles:    0.9,
}

// intentDebugWeights boosts DATA_FLOWS and DEPENDS_ON for the "debug" intent.
// Combined with negative DirectionBoost, callers (upstream triggers) are preferred.
var intentDebugWeights = map[EdgeType]float64{
	EdgeCalls:      1.0,
	EdgeDataFlows:  1.1,
	EdgeImplements: 0.7,
	EdgeEmbeds:     0.65,
	EdgeDependsOn:  0.95,
	EdgeImports:    0.5,
	EdgeExports:    0.4,
	EdgeDefines:    0.15,
	EdgeHandles:    0.9,
}

// intentReviewWeights boosts IMPLEMENTS and EMBEDS for the "review" intent.
// Code review is about contract surface, interface compliance, and test coverage.
var intentReviewWeights = map[EdgeType]float64{
	EdgeCalls:      0.8,
	EdgeDataFlows:  1.0,
	EdgeImplements: 1.2,
	EdgeEmbeds:     1.0,
	EdgeDependsOn:  0.7,
	EdgeImports:    0.5,
	EdgeExports:    0.6,
	EdgeDefines:    0.15,
	EdgeHandles:    0.9,
}

// intentUnderstandWeights is the same as DefaultEdgeWeights — balanced for exploration.
var intentUnderstandWeights = map[EdgeType]float64{
	EdgeCalls:      1.0,
	EdgeDataFlows:  0.95,
	EdgeImplements: 0.9,
	EdgeEmbeds:     0.85,
	EdgeDependsOn:  0.8,
	EdgeImports:    0.7,
	EdgeExports:    0.5,
	EdgeDefines:    0.15,
	EdgeHandles:    0.9,
}

// intentAddWeights boosts IMPORTS and IMPLEMENTS for the "add" intent.
// Agents adding new code need to follow existing import patterns and interfaces.
var intentAddWeights = map[EdgeType]float64{
	EdgeCalls:      0.7,
	EdgeDataFlows:  0.8,
	EdgeImplements: 1.0,
	EdgeEmbeds:     0.9,
	EdgeDependsOn:  0.9,
	EdgeImports:    0.85,
	EdgeExports:    0.65,
	EdgeDefines:    0.15,
	EdgeHandles:    0.9,
}

// intentPlanWeights boosts IMPLEMENTS and DEPENDS_ON for the "plan" intent.
// Planning requires understanding interface contracts and dependency scope.
var intentPlanWeights = map[EdgeType]float64{
	EdgeCalls:      1.0,
	EdgeDataFlows:  0.9,
	EdgeImplements: 1.1,
	EdgeEmbeds:     0.85,
	EdgeDependsOn:  0.95,
	EdgeImports:    0.7,
	EdgeExports:    0.6,
	EdgeDefines:    0.15,
	EdgeHandles:    0.9,
}

// IntentCarveWeights returns the pre-allocated edge weight map for the given
// intent. These maps are package-level vars — zero allocation at call time.
// Falls back to DefaultEdgeWeights for unknown intents.
func IntentCarveWeights(intent string) map[EdgeType]float64 {
	switch intent {
	case "modify":
		return intentModifyWeights
	case "debug":
		return intentDebugWeights
	case "review":
		return intentReviewWeights
	case "understand":
		return intentUnderstandWeights
	case "add":
		return intentAddWeights
	case "plan":
		return intentPlanWeights
	default:
		return DefaultEdgeWeights
	}
}

// IntentDirectionBoost returns the DirectionBoost value for the given intent.
// Positive = prefer callees, negative = prefer callers, 0 = balanced.
func IntentDirectionBoost(intent string) float64 {
	switch intent {
	case "modify":
		return 0.3 // prefer callees — see what this will break
	case "debug":
		return -0.3 // prefer callers — find what triggers this
	case "review":
		return 0.0 // balanced — review the full contract surface
	default:
		return 0.2 // default callee preference
	}
}

// DefaultCarveConfig returns sensible defaults for context carving.
func DefaultCarveConfig() CarveConfig {
	return CarveConfig{
		MaxDepth:         2,
		TokenBudget:      4000,
		EdgeWeights:      DefaultEdgeWeights,
		DecayFactor:      0.5,
		MinRelevance:     0.25,
		ExcludeTestFiles: true,
		ExcludeTypes: map[NodeType]bool{
			NodePackage: true,
			NodeFile:    true,
		},
	}
}

// CarvedNode is a node annotated with its relevance score and hop distance
// from the query root, as computed during a carving traversal.
type CarvedNode struct {
	Node      *Node   `json:"node"`
	Relevance float64 `json:"relevance"`
	Hop       int     `json:"hop"`
}

// SubGraph is the result of a context carve: a relevance-ranked slice of the graph.
type SubGraph struct {
	Root           NodeID       `json:"root"`
	Nodes          []CarvedNode `json:"nodes"`
	Edges          []*Edge      `json:"edges"`
	Truncated      bool         `json:"truncated,omitempty"`       // true when token budget cut BFS results
	TruncatedCount int          `json:"truncated_count,omitempty"` // number of nodes dropped by budget
}

// SuggestedRule is a detected high-density structural coupling pattern.
// Returned in get_project_identity to surface architectural conventions that
// the team may want to formalise as explicit forbidden-edge rules.
type SuggestedRule struct {
	// ID is a stable slug derived from the directory pair.
	ID string `json:"id"`
	// Description is a human-readable summary including sample counts.
	Description string `json:"description"`
	// Confidence is the fraction of from-dir nodes that call into to-dir (0–1).
	Confidence float64 `json:"confidence"`
	// SampleCount is the number of distinct from-dir nodes that exhibit the pattern.
	SampleCount int `json:"sample_count"`
	// FromDirPattern is a glob suitable for use as from_file_pattern in a rule.
	FromDirPattern string `json:"from_dir_pattern"`
	// ToDirPattern is a glob suitable for use as to_file_pattern in a rule.
	ToDirPattern string `json:"to_dir_pattern"`
	// EdgeType is the type of coupling detected (always EdgeCalls for now).
	EdgeType EdgeType `json:"edge_type"`
}

// Scale classifies a project's size based on semantic node count
// (functions + methods + structs + interfaces). Used to give agents
// scale-aware guidance on when to prefer Synapses tools vs direct file access.
type Scale string

const (
	// ScaleMicro represents projects <100 semantic nodes — Read/Grep often faster.
	ScaleMicro Scale = "micro"
	// ScaleSmall represents projects with 100–499 nodes — prefer Synapses for exploration.
	ScaleSmall Scale = "small"
	// ScaleMedium represents projects with 500–1999 nodes — strongly prefer Synapses tools.
	ScaleMedium Scale = "medium"
	// ScaleLarge represents projects with 2000+ nodes — always use Synapses tools.
	ScaleLarge Scale = "large"
)

// ProjectIdentity is the compact architectural summary returned by get_project_identity.
type ProjectIdentity struct {
	RepoID         string          `json:"repo_id"`
	Summary        GraphSummary    `json:"summary"`
	EntryPoints    []EntityRef     `json:"entry_points"`
	KeyEntities    []EntityInfo    `json:"key_entities"`
	SuggestedRules []SuggestedRule `json:"suggested_rules,omitempty"`
	// Scale is the repo size tier, computed from semantic node count.
	Scale Scale `json:"scale"`
	// ToolGuidance is a scale-aware recommendation for agents on which tools to prefer.
	ToolGuidance string `json:"tool_guidance"`
}

// GraphSummary contains aggregate counts across the whole graph.
type GraphSummary struct {
	Files      int `json:"files"`
	Packages   int `json:"packages"`
	Functions  int `json:"functions"`
	Methods    int `json:"methods"`
	Structs    int `json:"structs"`
	Interfaces int `json:"interfaces"`
	Edges      int `json:"edges"`
}

// EntityRef is a minimal reference to a node, used for lists like entry points.
type EntityRef struct {
	ID   NodeID   `json:"id"`
	Name string   `json:"name"`
	Type NodeType `json:"type"`
	File string   `json:"file"`
	Line int      `json:"line"`
}

// EntityInfo extends EntityRef with connectivity metrics.
type EntityInfo struct {
	EntityRef
	Fanin  int `json:"fanin"`
	Fanout int `json:"fanout"`
}

// ImpactTier groups nodes at the same blast-radius hop distance.
type ImpactTier struct {
	Depth      int         `json:"depth"`
	Label      string      `json:"label"`      // "direct" | "indirect" | "peripheral"
	Confidence float64     `json:"confidence"` // 1.0 / 0.6 / 0.3
	Nodes      []EntityRef `json:"nodes"`
	Truncated  bool        `json:"truncated,omitempty"`   // true when nodes were capped
	TotalNodes int         `json:"total_nodes,omitempty"` // actual count before cap
}

// ImpactResult is returned by ImpactAnalysis.
type ImpactResult struct {
	Root          EntityRef    `json:"root"`
	Tiers         []ImpactTier `json:"tiers"`
	TotalAffected int          `json:"total_affected"`
	AffectedFiles []string     `json:"affected_files"`
	// Truncated is true when any tier was capped at maxImpactNodesPerTier.
	// Check per-tier Truncated + TotalNodes for exact counts.
	Truncated bool `json:"truncated,omitempty"`
	// TestCoverage lists test files that exercise the root entity (R2).
	// Populated by FindTestsFor via reverse-BFS over CALLS edges filtered to test files.
	TestCoverage []string `json:"test_coverage,omitempty"`
}

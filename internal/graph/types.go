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
	// NodeSection is a documentation section extracted from a markdown file (R31).
	// Each ATX heading (# through ######) becomes a Section node with metadata:
	// title, depth (1-6), body_preview (first 200 chars), body (up to 2000 chars).
	NodeSection NodeType = "section"
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
	// R31: Documentation graph edges.
	// EdgeContains links a document file to its section nodes (doc→section)
	// and parent sections to child subsections (section→subsection).
	EdgeContains EdgeType = "CONTAINS"
	// EdgeExplains links a documentation section to a code entity it describes.
	// Direction: Section → code entity. Created by ResolveDocEdges post-parse.
	EdgeExplains EdgeType = "EXPLAINS"
	// EdgeDocumentedBy is the reverse of EXPLAINS: code entity → Section.
	// Enables get_context to surface documentation for any queried code entity.
	EdgeDocumentedBy EdgeType = "DOCUMENTED_BY"
	// EdgeLinksTo connects document nodes via markdown [text](path.md) links.
	// Direction: source document/section → target document node.
	EdgeLinksTo EdgeType = "LINKS_TO"
	// EdgeManual is a user-defined relationship created via link_entities.
	// Used when the relation string doesn't match a known catalog type.
	// BFS weight 0.5 — traversed but lower priority than structural code edges.
	EdgeManual EdgeType = "MANUAL"

	// Sprint 16: Cross-domain edge types.
	// These connect entities across knowledge domains (code ↔ infra ↔ api ↔ docs ↔ config).

	// EdgeDeploys links a code entity to the infrastructure resource that deploys it.
	// Direction: code entity → Terraform/k8s resource.
	EdgeDeploys EdgeType = "DEPLOYS"
	// EdgeConsumes links a code entity to the API endpoint or service it calls.
	// Direction: code entity → OpenAPI endpoint / gRPC service node.
	EdgeConsumes EdgeType = "CONSUMES"
	// EdgeConfiguredBy links a code entity to the config resource that controls it.
	// Direction: code entity → config resource (Terraform variable, k8s ConfigMap, etc.).
	EdgeConfiguredBy EdgeType = "CONFIGURED_BY"
	// EdgeDocuments links a documentation section to the code entity it describes.
	// Direction: docs section → code entity. Broader than EXPLAINS — used for
	// cross-domain docs (e.g. a README section about a Terraform module).
	EdgeDocuments EdgeType = "DOCUMENTS"
	// EdgeMentions is a synthetic cross-domain name-match edge.
	// Direction: any entity → any entity (cross-domain). Created by the name-matching
	// background pass (Sprint 16 #2) when two entities share the same name across domains.
	// Confidence 0.0–1.0 stored in edge metadata; only edges with confidence ≥ 0.6 are
	// auto-created. BFS weight is lower than structural edges to reflect uncertainty.
	EdgeMentions EdgeType = "MENTIONS"
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
	// R31: Documentation edge weights.
	// CONTAINS is structural (doc→section), low weight like DEFINES.
	EdgeContains: 0.15,
	// EXPLAINS has moderate weight: doc sections explaining code are valuable context.
	EdgeExplains: 0.7,
	// DOCUMENTED_BY is the reverse: code entity → doc section. Slightly lower
	// so code-to-code edges are preferred when the token budget is tight.
	EdgeDocumentedBy: 0.6,
	// LINKS_TO is cross-doc navigation, lowest semantic weight.
	EdgeLinksTo: 0.3,
	// MANUAL is a user-defined cross-domain edge (created via link_entities).
	// Medium weight — traversed by BFS but lower priority than structural code edges.
	EdgeManual: 0.5,
	// Sprint 16: Cross-domain edge weights.
	// DEPLOYS and CONSUMES are strong dependency relationships across domain boundaries.
	EdgeDeploys:  0.75,
	EdgeConsumes: 0.75,
	// CONFIGURED_BY and DOCUMENTS are moderate-weight cross-domain relationships.
	EdgeConfiguredBy: 0.65,
	EdgeDocuments:    0.65,
	// MENTIONS is synthetic (name-match heuristic) — lower weight reflects uncertainty.
	EdgeMentions: 0.55,
}

// EdgeTypeDescriptor captures the semantic metadata for a single edge type.
// The catalog is the authoritative source for BFS weights, domain tags, and
// human-readable descriptions — avoiding the need to scatter this information
// across multiple maps and comments throughout the codebase.
type EdgeTypeDescriptor struct {
	// Name is the EdgeType constant value (e.g. "CALLS").
	Name EdgeType `json:"name"`
	// Description is a human-readable explanation of what this edge means.
	Description string `json:"description"`
	// SemanticWeight is the default BFS traversal weight (matches DefaultEdgeWeights).
	// Higher weight = edge traversed first and contributes more relevance to reachable nodes.
	SemanticWeight float64 `json:"semantic_weight"`
	// Direction is always "directed" for the current graph model.
	// Reserved for future bidirectional edge types (e.g. cross-domain MENTIONS).
	Direction string `json:"direction"`
	// Domain classifies which knowledge domain this edge belongs to.
	// Uses the same values as DomainType constants: DomainCode, DomainDocs,
	// DomainInfra, DomainAPI, DomainKnowledge, DomainIssues, DomainCustom.
	// Sprint 16 added infra, api, and knowledge domain edges (DEPLOYS, CONSUMES,
	// CONFIGURED_BY, DOCUMENTS, MENTIONS).
	Domain DomainType `json:"domain"`
	// Synthetic marks edges injected by heuristic passes rather than derived from the AST.
	// Synthetic edges carry an inherent confidence < 1.0 (stored in node metadata).
	Synthetic bool `json:"synthetic,omitempty"`
}

// EdgeTypeCatalog is the authoritative registry of all edge types in the graph.
// Every entry in DefaultEdgeWeights must have a corresponding descriptor here —
// the TestEdgeTypeCatalogCompleteness test enforces this invariant at test time.
//
// Sprint 16 adds: DEPLOYS, CONSUMES, CONFIGURED_BY (code-to-infra/api),
// DOCUMENTS (docs-to-code), MENTIONS (cross-domain name match).
// When new edge types are added, append a descriptor here AND add to DefaultEdgeWeights.
var EdgeTypeCatalog = []EdgeTypeDescriptor{
	{
		Name:           EdgeCalls,
		Description:    "Function or method invocation. Direction: caller → callee. Highest BFS weight — runtime behaviour flows along CALLS edges.",
		SemanticWeight: 1.0,
		Direction:      "directed",
		Domain:         DomainCode,
	},
	{
		Name:           EdgeDataFlows,
		Description:    "Data dependency between entities: a value produced by one entity is consumed by another. Near-highest weight — data-flow edges are critical for debugging and impact analysis.",
		SemanticWeight: 0.95,
		Direction:      "directed",
		Domain:         DomainCode,
	},
	{
		Name:           EdgeImplements,
		Description:    "Struct or type implements an interface. Direction: concrete type → interface. High weight — interface compliance is central to code review and contract analysis.",
		SemanticWeight: 0.9,
		Direction:      "directed",
		Domain:         DomainCode,
	},
	{
		Name:           EdgeHandles,
		Description:    "HTTP/RPC route dispatches to a handler function. Direction: route node → handler. Injected by the R1 heuristic pass (not AST-derived). Confidence stored in route node metadata.",
		SemanticWeight: 0.9,
		Direction:      "directed",
		Domain:         DomainCode,
		Synthetic:      true,
	},
	{
		Name:           EdgeEmbeds,
		Description:    "Struct embeds another struct (Go embedding / composition). Direction: outer struct → embedded struct. High weight — embedding propagates the full method set.",
		SemanticWeight: 0.85,
		Direction:      "directed",
		Domain:         DomainCode,
	},
	{
		Name:           EdgeDependsOn,
		Description:    "Explicit dependency relationship between entities or modules. Broader than CALLS — captures package-level or declarative dependencies not visible as direct call sites.",
		SemanticWeight: 0.8,
		Direction:      "directed",
		Domain:         DomainCode,
	},
	{
		Name:           EdgeDeploys,
		Description:    "Code entity deploys an infrastructure resource. Direction: code entity → Terraform/k8s resource node. Strong cross-domain dependency — code changes may break deployed infrastructure.",
		SemanticWeight: 0.75,
		Direction:      "directed",
		Domain:         DomainInfra,
		Synthetic:      true,
	},
	{
		Name:           EdgeConsumes,
		Description:    "Code entity calls or depends on an API endpoint or service. Direction: code entity → OpenAPI endpoint / gRPC service node. Strong cross-domain dependency — API changes break consuming code.",
		SemanticWeight: 0.75,
		Direction:      "directed",
		Domain:         DomainAPI,
		Synthetic:      true,
	},
	{
		Name:           EdgeImports,
		Description:    "Source file or package imports another package. Direction: importer → imported package node. Lower weight than CALLS — import edges are structurally noisy (every file that uses a stdlib type gets an edge).",
		SemanticWeight: 0.7,
		Direction:      "directed",
		Domain:         DomainCode,
	},
	{
		Name:           EdgeExplains,
		Description:    "Documentation section describes a code entity (R31). Direction: Section node → code entity. Moderate weight — doc context is valuable but secondary to structural code edges.",
		SemanticWeight: 0.7,
		Direction:      "directed",
		Domain:         DomainDocs,
		Synthetic:      true,
	},
	{
		Name:           EdgeConfiguredBy,
		Description:    "Code entity is controlled by a configuration resource. Direction: code entity → Terraform variable / k8s ConfigMap / config file node. Cross-domain — config changes can silently break code behaviour.",
		SemanticWeight: 0.65,
		Direction:      "directed",
		Domain:         DomainInfra,
		Synthetic:      true,
	},
	{
		Name:           EdgeDocuments,
		Description:    "Documentation section describes a cross-domain entity (broader than EXPLAINS). Direction: docs section → any entity (code, infra, API). Used for README sections that describe Terraform modules or API specs.",
		SemanticWeight: 0.65,
		Direction:      "directed",
		Domain:         DomainDocs,
		Synthetic:      true,
	},
	{
		Name:           EdgeDocumentedBy,
		Description:    "Reverse of EXPLAINS: code entity references its documentation section (R31). Direction: code entity → Section node. Slightly lower than EXPLAINS so code-to-code edges are preferred under token budget pressure.",
		SemanticWeight: 0.6,
		Direction:      "directed",
		Domain:         DomainDocs,
		Synthetic:      true,
	},
	{
		Name:           EdgeMentions,
		Description:    "Synthetic cross-domain name-match edge. Direction: any entity → any entity across domain boundary. Created by the name-matching background pass when two entities share the same identifier across domains. Confidence (0.0–1.0) stored in edge metadata; only edges with confidence ≥ 0.6 are auto-created.",
		SemanticWeight: 0.55,
		Direction:      "directed",
		Domain:         DomainKnowledge,
		Synthetic:      true,
	},
	{
		Name:           EdgeExports,
		Description:    "Module or file exports an identifier. Direction: file/module → exported symbol. Medium-low weight — captures public API surface without dominating BFS traversal.",
		SemanticWeight: 0.5,
		Direction:      "directed",
		Domain:         DomainCode,
	},
	{
		Name:           EdgeManual,
		Description:    "User-defined cross-domain relationship created via link_entities. Used when no standard edge type applies. Medium BFS weight (0.5) — traversed but lower priority than structural code edges.",
		SemanticWeight: 0.5,
		Direction:      "directed",
		Domain:         DomainCustom,
		Synthetic:      true,
	},
	{
		Name:           EdgeLinksTo,
		Description:    "Markdown cross-document link (R31). Direction: source document/section → target document node. Lowest semantic weight among doc edges — navigation structure, not content relationship.",
		SemanticWeight: 0.3,
		Direction:      "directed",
		Domain:         DomainDocs,
		Synthetic:      true,
	},
	{
		Name:           EdgeContains,
		Description:    "Document file contains a section, or parent section contains a subsections (R31). Direction: doc file/section → child section. Structural edge — same intentionally low weight as DEFINES to avoid hub inflation.",
		SemanticWeight: 0.15,
		Direction:      "directed",
		Domain:         DomainDocs,
		Synthetic:      true,
	},
	{
		Name:           EdgeDefines,
		Description:    "Source file defines a code entity. Direction: file node → entity node. Lowest weight — every entity has exactly one DEFINES edge, so including it at higher weight would uniformly equalise all siblings in a file.",
		SemanticWeight: 0.15,
		Direction:      "directed",
		Domain:         DomainCode,
	},
}

// GetEdgeTypes returns the full EdgeTypeCatalog slice.
// The returned slice is the package-level variable — callers must not mutate it.
func GetEdgeTypes() []EdgeTypeDescriptor {
	return EdgeTypeCatalog
}

// IsCrossDomainEdge returns true for edge types that connect entities across
// knowledge domain boundaries (code ↔ infra ↔ api ↔ docs ↔ knowledge ↔ custom).
// Used by collectCrossDomainImpact for one-hop impact detection.
//
// Note: BFS/PPR cross-domain decay is applied based on node.Domain comparison
// (currNode.Domain != neighNode.Domain), not on edge type. This function is not
// called in the BFS/PPR hot path — it classifies edge types for impact analysis.
func IsCrossDomainEdge(et EdgeType) bool {
	switch et {
	case EdgeDeploys, EdgeConsumes, EdgeConfiguredBy, EdgeDocuments, EdgeMentions, EdgeManual:
		return true
	default:
		return false
	}
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
	// DomainKnowledge represents cross-domain or meta-level relationships.
	// Used by synthetic edges (e.g. MENTIONS) that bridge two existing-domain entities
	// rather than belonging to any single domain. Sprint 16.
	DomainKnowledge DomainType = "knowledge"
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

// TerraformRef records an unresolved Terraform resource reference encountered
// during .tf file parsing. The resolver drains these after all files are parsed
// and creates DEPENDS_ON edges between resource nodes. This enables cross-file
// dependency resolution: a resource in vpc.tf can depend on one in compute.tf.
type TerraformRef struct {
	FromID   NodeID // node ID of the resource containing the reference
	FromFile string // absolute path of the .tf file containing the reference
	RefName  string // target resource name: "type.name" or "data.type.name" or "module.name"
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
	// See DefaultCarveConfig() for tuning guidance (BFS vs PPR interaction).
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
	// UsePPR switches the traversal engine from BFS to Personalized PageRank.
	// PPR captures multi-path importance: a node reached via N independent call
	// chains scores N× higher than a structurally equivalent single-path node.
	// BFS max-score heuristic cannot represent this. Default: false (BFS).
	// Validated by Sprint 13 #1 spike (diamond 4.69×, wide-fan 5.68× PPR boost).
	UsePPR bool
	// Alpha is the PPR teleport probability — the chance the random walk jumps
	// back to the root (personalized restart) at each step. Higher alpha means
	// tighter focus on root with shorter effective reach. Default: 0.15
	// (standard PageRank restart rate). Only used when UsePPR=true.
	// Values outside (0,1) are clamped to 0.15.
	Alpha float64
	// EmbeddingLookup batch-fetches pre-normalized float32 embedding vectors for
	// a set of node IDs. Called once after BFS/PPR with all scored node IDs.
	// IDs with no stored embedding are omitted from the result map. Nil disables
	// semantic hybrid scoring (pure structural — backward-compatible default).
	EmbeddingLookup func(ids []NodeID) map[NodeID][]float32
	// HybridLambda controls the semantic blend weight applied after BFS/PPR:
	//   finalScore = (1-λ)×structural + λ×cosineSim(embed(root), embed(n))
	// Range [0, 1]. 0 = pure structural (default). Ignored when EmbeddingLookup
	// is nil or the root node has no stored embedding.
	// Recommended production value: 0.3 (70% structural, 30% semantic).
	HybridLambda float64
	// QualityScoreLookup returns per-entity context quality scores keyed by
	// node ID. Scores are the signed sum of signal_weight values from
	// outcome_signals (Sprint 15 #1/2). Positive = context was consistently
	// helpful; negative = context was repeatedly insufficient or abandoned.
	// Called once after BFS/PPR scoring with all surviving nodes.
	// Each QualityNode carries the ID, Name, and File so closures can convert
	// to entityWithPath format without re-acquiring the graph read lock (which
	// would deadlock — CarveEgoGraph already holds g.mu.RLock when calling this).
	// Nil disables quality-based re-ranking (backward-compatible default).
	QualityScoreLookup func(nodes []QualityNode) map[NodeID]float64
	// CrossDomainDecay is a multiplier applied to relevance when BFS/PPR crosses
	// a domain boundary (e.g., code→infra, code→api). Range (0, 1].
	// A value of 0.5 (default) means cross-domain neighbors score at half the
	// relevance of same-domain neighbors at the same structural distance.
	// This keeps same-domain code nodes higher in the ranking while still
	// surfacing cross-domain context at meaningfully lower relevance.
	// 0 disables the domain-boundary penalty (treats all edges equally).
	// Values ≥ 1 are clamped to 1.0 (no penalty — backward compatible).
	CrossDomainDecay float64
	// LearnedEdgeWeights contains per-specific-edge weight multipliers derived
	// from historical task outcomes (Sprint 15 #3). When traversing edge
	// (From→To, Type), the base edgeWeight is multiplied by this value.
	// A multiplier of 1.0 is neutral; >1.0 boosts the edge; <1.0 penalises it.
	// Cap: 2.0x boost, floor: 0.3x penalty. Nil disables learned-weight
	// adjustments (backward-compatible default).
	LearnedEdgeWeights map[EdgeWeightKey]float64
	// LearnedEdgeWeightsVersion is the store's monotonic write counter at the
	// time LearnedEdgeWeights was loaded. It is included in the subgraph cache
	// key so that cached subgraphs are automatically invalidated after any write
	// to the edge_learned_weights table — regardless of whether the map has the
	// same number of entries (len-based discrimination is not sufficient).
	LearnedEdgeWeightsVersion int64
}

// EdgeWeightKey uniquely identifies a specific directed edge in the graph.
// Used as a map key for per-edge learned weight multipliers (Sprint 15 #3).
type EdgeWeightKey struct {
	From NodeID
	To   NodeID
	Type EdgeType
}

// QualityNode carries the graph identity and file context for a single node
// passed to CarveConfig.QualityScoreLookup. Name and File allow closures to
// convert to entityWithPath format without calling Graph.GetNode — which would
// attempt to re-acquire g.mu.RLock and potentially deadlock because
// CarveEgoGraph already holds the lock when it invokes QualityScoreLookup.
type QualityNode struct {
	ID   NodeID
	Name string
	File string
}

// intentModifyWeights boosts outgoing CALLS (callees) for the "modify" intent.
// Agents modifying code need to see what they will break (dependencies).
// IMPLEMENTS is reduced — the focus is behavioral, not contractual.
var intentModifyWeights = map[EdgeType]float64{
	EdgeCalls:        1.0,
	EdgeDataFlows:    0.95,
	EdgeImplements:   0.6,
	EdgeEmbeds:       0.75,
	EdgeDependsOn:    0.8,
	EdgeImports:      0.6,
	EdgeExports:      0.4,
	EdgeDefines:      0.15,
	EdgeHandles:      0.9,
	EdgeContains:     0.15,
	EdgeExplains:     0.5,
	EdgeDocumentedBy: 0.4,
	EdgeLinksTo:      0.2,
	// Sprint 16: cross-domain edges — deploy/consume targets are critical for modify.
	EdgeDeploys:      0.75,
	EdgeConsumes:     0.75,
	EdgeConfiguredBy: 0.65,
	EdgeDocuments:    0.4,
	EdgeMentions:     0.55,
	EdgeManual:       0.5,
}

// intentDebugWeights boosts DATA_FLOWS and DEPENDS_ON for the "debug" intent.
// Combined with negative DirectionBoost, callers (upstream triggers) are preferred.
var intentDebugWeights = map[EdgeType]float64{
	EdgeCalls:        1.0,
	EdgeDataFlows:    1.1,
	EdgeImplements:   0.7,
	EdgeEmbeds:       0.65,
	EdgeDependsOn:    0.95,
	EdgeImports:      0.5,
	EdgeExports:      0.4,
	EdgeDefines:      0.15,
	EdgeHandles:      0.9,
	EdgeContains:     0.15,
	EdgeExplains:     0.5,
	EdgeDocumentedBy: 0.4,
	EdgeLinksTo:      0.2,
	// Sprint 16: cross-domain edges — config is extra important for debugging.
	EdgeDeploys:      0.65,
	EdgeConsumes:     0.75,
	EdgeConfiguredBy: 0.75,
	EdgeDocuments:    0.4,
	EdgeMentions:     0.55,
	EdgeManual:       0.5,
}

// intentReviewWeights boosts IMPLEMENTS and EMBEDS for the "review" intent.
// Code review is about contract surface, interface compliance, and test coverage.
var intentReviewWeights = map[EdgeType]float64{
	EdgeCalls:        0.8,
	EdgeDataFlows:    1.0,
	EdgeImplements:   1.2,
	EdgeEmbeds:       1.0,
	EdgeDependsOn:    0.7,
	EdgeImports:      0.5,
	EdgeExports:      0.6,
	EdgeDefines:      0.15,
	EdgeHandles:      0.9,
	EdgeContains:     0.15,
	EdgeExplains:     0.7,
	EdgeDocumentedBy: 0.6,
	EdgeLinksTo:      0.3,
	// Sprint 16: cross-domain edges — all relevant for review.
	EdgeDeploys:      0.75,
	EdgeConsumes:     0.75,
	EdgeConfiguredBy: 0.65,
	EdgeDocuments:    0.65,
	EdgeMentions:     0.55,
	EdgeManual:       0.5,
}

// intentAddWeights boosts IMPORTS and IMPLEMENTS for the "add" intent.
// Agents adding new code need to follow existing import patterns and interfaces.
var intentAddWeights = map[EdgeType]float64{
	EdgeCalls:        0.7,
	EdgeDataFlows:    0.8,
	EdgeImplements:   1.0,
	EdgeEmbeds:       0.9,
	EdgeDependsOn:    0.9,
	EdgeImports:      0.85,
	EdgeExports:      0.65,
	EdgeDefines:      0.15,
	EdgeHandles:      0.9,
	EdgeContains:     0.15,
	EdgeExplains:     0.7,
	EdgeDocumentedBy: 0.6,
	EdgeLinksTo:      0.3,
	// Sprint 16: cross-domain edges — API/infra context useful when adding new code.
	EdgeDeploys:      0.65,
	EdgeConsumes:     0.75,
	EdgeConfiguredBy: 0.65,
	EdgeDocuments:    0.55,
	EdgeMentions:     0.55,
	EdgeManual:       0.5,
}

// intentPlanWeights boosts IMPLEMENTS and DEPENDS_ON for the "plan" intent.
// Planning requires understanding interface contracts and dependency scope.
var intentPlanWeights = map[EdgeType]float64{
	EdgeCalls:        1.0,
	EdgeDataFlows:    0.9,
	EdgeImplements:   1.1,
	EdgeEmbeds:       0.85,
	EdgeDependsOn:    0.95,
	EdgeImports:      0.7,
	EdgeExports:      0.6,
	EdgeDefines:      0.15,
	EdgeHandles:      0.9,
	EdgeContains:     0.15,
	EdgeExplains:     0.8,
	EdgeDocumentedBy: 0.7,
	EdgeLinksTo:      0.3,
	// Sprint 16: cross-domain edges — plan intent needs full cross-domain picture.
	EdgeDeploys:      0.75,
	EdgeConsumes:     0.75,
	EdgeConfiguredBy: 0.65,
	EdgeDocuments:    0.65,
	EdgeMentions:     0.55,
	EdgeManual:       0.5,
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
		return DefaultEdgeWeights
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
//
// MinRelevance / PPR interaction:
//   - BFS path: MinRelevance=0.01 prunes nodes whose relevance has decayed below
//     1% of root. With decay=0.5 and a 16K-edge graph this allows ~6 hops for
//     narrow chains and ~3 hops for hub nodes (degree-normalized adaptive decay).
//     Raising MinRelevance tightens the subgraph; lowering it risks hub explosion.
//   - PPR path (UsePPR=true): power iteration assigns near-zero scores to distant
//     nodes naturally — MinRelevance=0.01 trims the long tail without aggressive
//     pruning. The spike benchmark (ppr_spike_test.go) validated this threshold
//     against diamond and wide-fan graph topologies. Lowering below 0.001 has
//     negligible recall gain with O(N) cost. Raising above 0.05 risks losing
//     semantically adjacent nodes in sparse subgraphs.
//
// Recommended tuning guide:
//   - Default (0.01) — correct for most codebases up to ~50K nodes.
//   - Dense monorepos (>100K edges): raise to 0.03–0.05 to keep carves fast.
//   - Sparse/small repos (<1K nodes): lower to 0.005 to improve recall depth.
func DefaultCarveConfig() CarveConfig {
	return CarveConfig{
		MaxDepth:         2,
		TokenBudget:      4000,
		EdgeWeights:      DefaultEdgeWeights,
		DecayFactor:      0.5,
		MinRelevance:     0.01,
		ExcludeTestFiles: true,
		ExcludeTypes: map[NodeType]bool{
			NodePackage: true,
			NodeFile:    true,
		},
		// Sprint 13 end-state: PPR is the default traversal algorithm.
		// Validated by spike tests (diamond 4.69×, wide-fan 5.68× over BFS).
		// Set use_ppr=false in synapses.json to revert to BFS for debugging.
		UsePPR: true,
		// Sprint 16 #4: cross-domain boundary penalty. Default 0.5 means
		// infra/api/docs nodes score at half relevance relative to same-domain
		// neighbors at equal structural distance. Keeps code context primary
		// while still surfacing cross-domain context in the cross_domain bucket.
		CrossDomainDecay: 0.5,
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

// CrossDomainRef is a single entity reached via a cross-domain edge during
// impact analysis. Category groups the finding by relationship type so
// agents can answer "what Terraform resources does this deploy to?" etc.
type CrossDomainRef struct {
	EntityRef
	// EdgeType is the cross-domain edge type that led to this entity
	// (e.g. "DEPLOYS", "CONSUMES", "CONFIGURED_BY", "DOCUMENTS", "MENTIONS", "MANUAL").
	EdgeType EdgeType `json:"edge_type"`
	// Category is a human-readable grouping derived from EdgeType:
	// "infra" (DEPLOYS), "api" (CONSUMES), "config" (CONFIGURED_BY),
	// "docs" (DOCUMENTS), "related" (MENTIONS/MANUAL).
	Category string `json:"category"`
}

// CrossDomainContext groups cross-domain CarvedNodes from a BFS/PPR subgraph
// by their relationship to the root entity. Used by directionalContext in
// get_context responses. Each sub-slice preserves BFS Relevance scores for
// ranking within the sub-bucket.
//
// Nodes connected via a direct edge from/to root are categorized by edge type.
// Multi-hop cross-domain nodes with no direct root edge go into Related.
type CrossDomainContext struct {
	Deploys      []CarvedNode `json:"deploys,omitempty"`
	Consumes     []CarvedNode `json:"consumes,omitempty"`
	ConfiguredBy []CarvedNode `json:"configured_by,omitempty"`
	DocumentedIn []CarvedNode `json:"documented_in,omitempty"`
	Mentions     []CarvedNode `json:"mentions,omitempty"`
	Manual       []CarvedNode `json:"manual,omitempty"`
	Related      []CarvedNode `json:"related,omitempty"` // multi-hop or no direct edge from root
}

// IsEmpty returns true when all sub-buckets are empty.
func (c *CrossDomainContext) IsEmpty() bool {
	if c == nil {
		return true
	}
	return len(c.Deploys) == 0 && len(c.Consumes) == 0 && len(c.ConfiguredBy) == 0 &&
		len(c.DocumentedIn) == 0 && len(c.Mentions) == 0 && len(c.Manual) == 0 && len(c.Related) == 0
}

// CrossDomainCategory returns the human-readable category for a cross-domain edge type.
func CrossDomainCategory(et EdgeType) string {
	switch et {
	case EdgeDeploys:
		return "infra"
	case EdgeConsumes:
		return "api"
	case EdgeConfiguredBy:
		return "config"
	case EdgeDocuments:
		return "docs"
	default:
		return "related"
	}
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
	// CrossDomainImpact lists entities in other knowledge domains that are
	// directly connected to the root via cross-domain edges (DEPLOYS, CONSUMES,
	// CONFIGURED_BY, DOCUMENTS, MENTIONS, MANUAL). Only edges with confidence ≥ 0.6
	// or confirmed are included — this is enforced at edge-injection time so all
	// edges present in the in-memory graph already satisfy the threshold.
	// Sprint 16 #5: the killer feature — "what infra/API/docs does this touch?"
	CrossDomainImpact []CrossDomainRef `json:"cross_domain_impact,omitempty"`
	// CrossDomainAffected is the count of cross-domain entities in CrossDomainImpact.
	// Kept separate from TotalAffected (which counts code-caller tier nodes) so
	// callers can distinguish code blast-radius from cross-domain blast-radius.
	CrossDomainAffected int `json:"cross_domain_affected,omitempty"`
	// CrossDomainTruncated is true when CrossDomainImpact was capped at
	// maxCrossDomainImpactNodes (100). The full count is not available.
	CrossDomainTruncated bool `json:"cross_domain_truncated,omitempty"`
}

// Package mcp implements the Model Context Protocol server for Synapses.
// Agents (Claude Code, Cursor, etc.) connect to this server over stdio and
// call the registered tools to query the code graph.
package mcp

import (
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/synapses/synapses/internal/config"
	"github.com/synapses/synapses/internal/graph"
	"github.com/synapses/synapses/internal/store"
	"github.com/synapses/synapses/internal/watcher"
)

// ChangeSource is implemented by types that maintain a recent file-change log.
// Typically this is *watcher.Watcher, wired in cmdStart via SetChangeSource.
type ChangeSource interface {
	RecentChanges(windowMinutes int) []watcher.ChangeEvent
}

const (
	serverName    = "synapses"
	serverVersion = "0.2.0"
)

// Server holds the MCP server and the dependencies that tool handlers need.
type Server struct {
	mcp          *server.MCPServer
	graph        *graph.Graph
	config       *config.Config
	store        *store.Store  // nil if started without a persistent store
	changeSource ChangeSource  // nil if started without a file watcher
	rulesMu      sync.RWMutex // protects s.config.Rules for concurrent dynamic upserts
}

// New creates a Server wired to the given graph, config, and optional store.
// The store is required for Agent Task Memory tools (create_plan, get_pending_tasks, etc.).
// Pass nil for st if running in a context without persistence (e.g. tests).
// All tools are registered during construction.
func New(g *graph.Graph, cfg *config.Config, st *store.Store) *Server {
	s := &Server{
		graph:  g,
		config: cfg,
		store:  st,
	}

	// Restore dynamic rules persisted from previous sessions. This runs before
	// registerTools() so that validate_plan / get_violations are immediately aware
	// of all previously upserted rules without requiring a daemon restart.
	if st != nil {
		if dynamicRules, err := st.LoadDynamicRules(); err == nil && len(dynamicRules) > 0 {
			cfg.Rules = append(cfg.Rules, dynamicRules...)
		}
	}

	s.mcp = server.NewMCPServer(serverName, serverVersion,
		server.WithToolCapabilities(true),
	)
	s.registerTools()
	return s
}

// SetChangeSource wires a change event source (typically the file watcher) so
// get_working_state can report recent file activity to agents.
func (s *Server) SetChangeSource(cs ChangeSource) {
	s.changeSource = cs
}

// ServeStdio starts the MCP server on stdin/stdout. This call blocks until
// the client disconnects or the process receives a signal.
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.mcp)
}

// registerTools wires all Synapses tool definitions to their handlers.
func (s *Server) registerTools() {
	// ── Code Graph Tools ────────────────────────────────────────────────────

	// get_project_identity
	s.mcp.AddTool(
		mcp.NewTool(
			"get_project_identity",
			mcp.WithDescription(
				"Returns a compact architectural summary of the indexed project: "+
					"node counts, entry points, highest-connectivity entities, and active rules. "+
					"Call this at the start of every session to orient yourself before querying deeper.",
			),
		),
		s.handleGetProjectIdentity,
	)

	// get_context
	s.mcp.AddTool(
		mcp.NewTool(
			"get_context",
			mcp.WithDescription(
				"Returns a relevance-ranked subgraph centred on the named entity. "+
					"Uses BFS with edge-type-weighted decay so the closest, most semantically "+
					"significant relationships appear first. This replaces grep: ask for what you "+
					"need structurally, not textually.",
			),
			mcp.WithString("entity",
				mcp.Required(),
				mcp.Description("The name of the code entity to carve context around (e.g. 'AuthService')."),
			),
			mcp.WithNumber("depth",
				mcp.Description("BFS hop limit. Defaults to the project config value (usually 2)."),
			),
			mcp.WithNumber("token_budget",
				mcp.Description("Maximum approximate tokens in the response. Defaults to 4000."),
			),
			mcp.WithString("task_id",
				mcp.Description("Optional task ID from get_pending_tasks. Nodes linked to this task get a relevance boost."),
			),
			mcp.WithString("mode",
				mcp.Description("'explore' (default): ego-subgraph BFS. 'impact': reverse-BFS showing what depends on this entity (same as get_impact)."),
			),
		),
		s.handleGetContext,
	)

	// find_entity
	s.mcp.AddTool(
		mcp.NewTool(
			"find_entity",
			mcp.WithDescription(
				"Locates nodes in the graph by name or substring. "+
					"Returns matching node references (ID, type, file, line) without full context. "+
					"Use this to discover the exact entity name before calling get_context.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Name or substring to search for (case-insensitive)."),
			),
		),
		s.handleFindEntity,
	)

	// validate_plan
	s.mcp.AddTool(
		mcp.NewTool(
			"validate_plan",
			mcp.WithDescription(
				"Checks a list of proposed code changes against the project's architectural rules. "+
					"Returns any violations before a single line of code is written. "+
					"Call this before implementing a plan that touches multiple files.",
			),
			mcp.WithString("changes",
				mcp.Required(),
				mcp.Description(
					"JSON array of proposed changes. Each item: "+
						`{"file": "path/to/file.go", "adds_call_to": "SomeFunction", "removes_call_to": "OtherFunction"}.`,
				),
			),
		),
		s.handleValidatePlan,
	)

	// get_violations
	s.mcp.AddTool(
		mcp.NewTool(
			"get_violations",
			mcp.WithDescription(
				"Lists all current architectural rule violations found in the graph. "+
					"Returns rule ID, severity, affected nodes, and a human-readable description.",
			),
		),
		s.handleGetViolations,
	)

	// get_file_context
	s.mcp.AddTool(
		mcp.NewTool(
			"get_file_context",
			mcp.WithDescription(
				"Returns all entities (functions, methods, structs, interfaces) defined in a file, "+
					"ordered by line number. Accepts a partial path suffix (e.g. 'store/tasks.go'). "+
					"Use this when working on a specific file to get an instant overview.",
			),
			mcp.WithString("file",
				mcp.Required(),
				mcp.Description("File path or suffix, e.g. 'internal/store/tasks.go' or 'tasks.go'."),
			),
		),
		s.handleGetFileContext,
	)

	// get_api_contract
	s.mcp.AddTool(
		mcp.NewTool(
			"get_api_contract",
			mcp.WithDescription(
				"Detects HTTP and gRPC API endpoints in the codebase using framework conventions "+
					"(net/http, Gin, Echo, Fiber, gRPC, Protocol Buffers) and optional custom patterns "+
					"from synapses.json api_entries. For each endpoint returns its signature, "+
					"the detected framework, direct callers (route registration), and direct callees "+
					"(service/repository dependencies). Answers: 'what is the public API surface?'",
			),
			mcp.WithString("package",
				mcp.Description("Optional. Filter to a specific package name (substring match)."),
			),
			mcp.WithString("file",
				mcp.Description("Optional. Filter to a specific file path suffix, e.g. 'handlers/auth.go'."),
			),
		),
		s.handleGetApiContract,
	)

	// search
	s.mcp.AddTool(
		mcp.NewTool(
			"search",
			mcp.WithDescription(
				"Keyword search across entity names and doc comments. "+
					"Results are ranked: exact name match > name prefix > name substring > doc comment match. "+
					"Returns up to 25 results. Use this to find auth-related code, error handlers, etc.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Search term (case-insensitive)."),
			),
		),
		s.handleSearch,
	)

	// semantic_search
	s.mcp.AddTool(
		mcp.NewTool(
			"semantic_search",
			mcp.WithDescription(
				"Full-text BM25 search over node names, signatures, and doc comments. "+
					"Finds code by concept, not just by exact name. "+
					"Examples: 'rate limiting', 'JWT validation', 'database connection pool'. "+
					"CamelCase names are auto-split: searching 'carve' finds 'CarveEgoGraph'. "+
					"Results ranked by relevance; falls back to LIKE search if FTS finds nothing. "+
					"Prefer this over search() for exploratory queries where you don't know the exact name. "+
					"Configure embedding_endpoint in synapses.json for future vector re-ranking.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Natural-language or keyword query, e.g. 'handles authentication', 'parse call sites'."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum results to return (default 20, max 50)."),
			),
		),
		s.handleSemanticSearch,
	)

	// get_call_chain
	s.mcp.AddTool(
		mcp.NewTool(
			"get_call_chain",
			mcp.WithDescription(
				"Finds the shortest call path between two entities by following CALLS edges. "+
					"Answers 'how does A reach B?' Use this to understand the execution path "+
					"between entry points and deep implementation details.",
			),
			mcp.WithString("from",
				mcp.Required(),
				mcp.Description("Name of the starting entity (caller side)."),
			),
			mcp.WithString("to",
				mcp.Required(),
				mcp.Description("Name of the target entity (callee side)."),
			),
		),
		s.handleGetCallChain,
	)

	// get_events
	s.mcp.AddTool(
		mcp.NewTool(
			"get_events",
			mcp.WithDescription(
				"Returns recent events from the pull-based event log since a cursor sequence number. "+
					"Event types: file_change, task_update, annotation_added, agent_activity. "+
					"Pass the returned latest_seq as since_seq on the next call to receive only new events. "+
					"Use this for multi-agent coordination — poll to discover what other agents did.",
			),
			mcp.WithNumber("since_seq",
				mcp.Description("Return only events with seq > since_seq. Pass 0 (or omit) for all recent events."),
			),
			mcp.WithString("types",
				mcp.Description("Optional comma-separated list of event types to filter: file_change,task_update,annotation_added,agent_activity"),
			),
			mcp.WithNumber("limit",
				mcp.Description("Max events to return (default 100)."),
			),
		),
		s.handleGetEvents,
	)

	// annotate_node
	s.mcp.AddTool(
		mcp.NewTool(
			"annotate_node",
			mcp.WithDescription(
				"Attaches a note to a graph node, visible to all agents via get_context. "+
					"Use this as a shared whiteboard: Agent A can annotate a function with "+
					"'known race condition here' and Agent B will see it in context queries. "+
					"Annotations persist across sessions.",
			),
			mcp.WithString("node_id",
				mcp.Required(),
				mcp.Description("The node ID to annotate (from find_entity or get_context)."),
			),
			mcp.WithString("note",
				mcp.Required(),
				mcp.Description("The annotation text."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Self-declared agent identifier for attribution."),
			),
		),
		s.handleAnnotateNode,
	)

	// get_impact
	s.mcp.AddTool(
		mcp.NewTool(
			"get_impact",
			mcp.WithDescription(
				"Performs blast-radius analysis: reverse-BFS from a named entity "+
					"following incoming CALLS and IMPLEMENTS edges to find everything that "+
					"could break if the entity changes. "+
					"Results grouped by depth: direct (depth 1, confidence 1.0), "+
					"indirect (depth 2, confidence 0.6), peripheral (depth 3+, confidence 0.3). "+
					"Answers: 'what breaks if I change X?'",
			),
			mcp.WithString("symbol",
				mcp.Required(),
				mcp.Description("Name of the entity to analyse (e.g. 'CarveEgoGraph')."),
			),
			mcp.WithNumber("depth",
				mcp.Description("Max hop depth. Default 3, max 10."),
			),
		),
		s.handleGetImpact,
	)

	// detect_changes
	s.mcp.AddTool(
		mcp.NewTool(
			"detect_changes",
			mcp.WithDescription(
				"Maps a git diff to affected graph symbols. "+
					"Runs `git diff --unified=0 [ref]` against the repo, parses changed "+
					"file paths and line ranges, and returns all graph nodes whose source "+
					"location falls within the changed ranges. "+
					"Answers: 'which symbols were touched by recent changes?'",
			),
			mcp.WithString("ref",
				mcp.Description(
					"Optional git ref to diff against (branch name, SHA, '--cached'). "+
						"Defaults to HEAD (unstaged changes). Example: 'main', 'HEAD~1'.",
				),
			),
		),
		s.handleDetectChanges,
	)

	// ── Agent Task Memory Tools ──────────────────────────────────────────────
	// These tools give Synapses session continuity: plans and tasks agreed in
	// one LLM conversation are stored in SQLite and surfaced to future sessions.

	// create_plan
	s.mcp.AddTool(
		mcp.NewTool(
			"create_plan",
			mcp.WithDescription(
				"Saves a named plan with actionable tasks to persistent storage. "+
					"Call this when the user approves an implementation plan so that future "+
					"LLM sessions can resume the work via get_pending_tasks. "+
					"Each task has a title, description, priority (p0–p3), and optional linked node IDs.",
			),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Short name for the plan, e.g. 'v1.0.1 context quality improvements'."),
			),
			mcp.WithString("description",
				mcp.Description("Optional longer description of what the plan achieves."),
			),
			mcp.WithString("tasks",
				mcp.Required(),
				mcp.Description(
					`JSON array of task objects. Each: {"title":"...", "description":"...", "priority":"p0|p1|p2|p3", "linked_nodes":["nodeID",...]}.`,
				),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Self-declared agent identifier, e.g. 'claude-code-session-1'. Recorded as plan creator."),
			),
		),
		s.handleCreatePlan,
	)

	// get_pending_tasks
	s.mcp.AddTool(
		mcp.NewTool(
			"get_pending_tasks",
			mcp.WithDescription(
				"Returns all pending and in-progress tasks, ordered by priority (p0 first). "+
					"Call this at the start of every session to discover what work was agreed "+
					"in previous sessions and resume from exactly where the last session stopped.",
			),
			mcp.WithString("plan_id",
				mcp.Description("Optional. Filter to tasks belonging to a specific plan."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Filter to tasks assigned to a specific agent."),
			),
		),
		s.handleGetPendingTasks,
	)

	// update_task
	s.mcp.AddTool(
		mcp.NewTool(
			"update_task",
			mcp.WithDescription(
				"Updates the status of a task and optionally appends timestamped notes. "+
					"Use this to mark tasks done as you complete them, "+
					"or to leave context notes for the next LLM session.",
			),
			mcp.WithString("id",
				mcp.Required(),
				mcp.Description("The task ID returned by get_pending_tasks."),
			),
			mcp.WithString("status",
				mcp.Required(),
				mcp.Description("New status: pending | in_progress | done | cancelled."),
			),
			mcp.WithString("notes",
				mcp.Description("Optional notes to append (timestamped). Use to leave context for the next session."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Self-declared agent identifier. Recorded as last_updated_by."),
			),
		),
		s.handleUpdateTask,
	)

	// get_plans
	s.mcp.AddTool(
		mcp.NewTool(
			"get_plans",
			mcp.WithDescription(
				"Lists all plans with task completion counts. "+
					"Use this to get an overview of all ongoing and completed plans.",
			),
		),
		s.handleGetPlans,
	)

	// get_my_tasks
	s.mcp.AddTool(
		mcp.NewTool(
			"get_my_tasks",
			mcp.WithDescription(
				"Returns the calling agent's unblocked pending tasks, plus a suggested next task. "+
					"Filters out tasks blocked by unfinished dependencies. "+
					"Convenience wrapper around get_pending_tasks for focused agent workflows.",
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The calling agent's self-declared identifier."),
			),
			mcp.WithString("plan_id",
				mcp.Description("Optional. Filter to a specific plan."),
			),
		),
		s.handleGetMyTasks,
	)

	// get_agents
	s.mcp.AddTool(
		mcp.NewTool(
			"get_agents",
			mcp.WithDescription(
				"Returns all agents that have interacted with Synapses, ordered by last-seen timestamp. "+
					"Agents self-declare their identity via the agent_id parameter on create_plan, "+
					"update_task, and get_pending_tasks calls. "+
					"Use this for multi-agent coordination to discover which agents are active.",
			),
		),
		s.handleGetAgents,
	)

	// link_task_nodes
	s.mcp.AddTool(
		mcp.NewTool(
			"link_task_nodes",
			mcp.WithDescription(
				"Explicitly links a task to code graph nodes, bridging 'work to be done' with 'code to change'. "+
					"If query is provided, finds nodes whose name matches the query and links them. "+
					"If query is omitted, scans the task's own title+description+notes for node name mentions. "+
					"Result is MERGED with existing linked_nodes — safe to call multiple times. "+
					"Note: create_plan already auto-links on creation; use this for corrections or additions.",
			),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("ID of the task to link.")),
			mcp.WithString("query", mcp.Description("Optional node name substring to search for. Omit to auto-scan the task text.")),
		),
		s.handleLinkTaskNodes,
	)

	// ── Diagnostic Tools ────────────────────────────────────────────────────

	// find_orphans
	s.mcp.AddTool(
		mcp.NewTool(
			"find_orphans",
			mcp.WithDescription(
				"Returns unexported functions and methods with no callers (fanin=0). "+
					"Useful for dead code detection. Exported symbols, Go runtime entry points (main, init), "+
					"interface implementors, and framework entry points (http.Handler, Cobra, gRPC) are excluded. "+
					"Each result includes a confidence score: 1.0=fully isolated, 0.7=has outgoing calls, 0.5=only file-defines edge.",
			),
			mcp.WithBoolean("include_tests",
				mcp.Description("Include helpers defined in _test.go files (default false)."),
			),
			mcp.WithNumber("min_confidence",
				mcp.Description("Only return orphans with confidence >= this value (default 0.5, range 0.0–1.0)."),
			),
		),
		s.handleFindOrphans,
	)

	// get_change_coupling
	s.mcp.AddTool(
		mcp.NewTool(
			"get_change_coupling",
			mcp.WithDescription(
				"Analyses git history to find files that frequently change together, "+
					"surfacing implicit dependencies that static analysis misses. "+
					"confidence = co_changes / max(total_commits_A, total_commits_B). "+
					"Returns pairs sorted by confidence descending. "+
					"Use this to discover hidden coupling before making changes.",
			),
			mcp.WithNumber("commit_limit",
				mcp.Description("Number of recent commits to analyse (default 500)."),
			),
			mcp.WithNumber("min_confidence",
				mcp.Description("Minimum coupling confidence to include (default 0.3, range 0–1)."),
			),
		),
		s.handleGetChangeCoupling,
	)

	// get_federation_status
	s.mcp.AddTool(
		mcp.NewTool(
			"get_federation_status",
			mcp.WithDescription(
				"Shows which linked projects are currently merged into the graph, "+
					"node counts per project, and the number of cross-project CALLS edges. "+
					"Returns is_federated=false when running on a single project.",
			),
		),
		s.handleGetFederationStatus,
	)

	// ── Rule Management Tools ────────────────────────────────────────────────

	// upsert_rule
	s.mcp.AddTool(
		mcp.NewTool(
			"upsert_rule",
			mcp.WithDescription(
				"Create or update a dynamic architectural rule. "+
					"Persisted to SQLite and active immediately — no daemon restart required. "+
					"Subsequent validate_plan and get_violations calls enforce it. "+
					"Use this when you detect a pattern that should be formalised as a constraint.",
			),
			mcp.WithString("rule_id",
				mcp.Required(),
				mcp.Description("Unique identifier for the rule, e.g. 'no-db-in-handler'. Used to update an existing rule."),
			),
			mcp.WithString("description",
				mcp.Required(),
				mcp.Description("Human-readable explanation of what this rule prevents and why."),
			),
			mcp.WithString("severity",
				mcp.Required(),
				mcp.Description("'error' or 'warning'."),
			),
			mcp.WithString("edge_type",
				mcp.Description("Edge type to forbid: CALLS, IMPORTS, IMPLEMENTS, etc. Empty = any edge type."),
			),
			mcp.WithString("from_file_pattern",
				mcp.Description("Glob matched against the base name of the source file, e.g. '*/handlers/*'."),
			),
			mcp.WithString("to_file_pattern",
				mcp.Description("Glob matched against the base name of the target file, e.g. '*/db/*'."),
			),
			mcp.WithString("to_name_pattern",
				mcp.Description("Substring that must appear in the target entity name."),
			),
		),
		s.handleUpsertRule,
	)

	// get_violation_log
	s.mcp.AddTool(
		mcp.NewTool(
			"get_violation_log",
			mcp.WithDescription(
				"Returns the audit trail of architectural rule violations detected by get_violations. "+
					"Each entry shows which rule fired, which nodes were involved, when it was "+
					"first and last seen, and how many times it occurred. "+
					"Use rule_id to filter to a specific rule.",
			),
			mcp.WithString("rule_id",
				mcp.Description("Optional. Filter to violations for a specific rule ID."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Max entries to return (default 50)."),
			),
		),
		s.handleGetViolationLog,
	)

	// ── Session Awareness Tools ──────────────────────────────────────────────

	// get_usage_guide
	s.mcp.AddTool(
		mcp.NewTool(
			"get_usage_guide",
			mcp.WithDescription(
				"Returns a project-specific guide for using Synapses: quick-start sequence, "+
					"tool selection guide, entry points, and key entities. "+
					"Call this when you are unsure which tool to use or how to start exploring.",
			),
		),
		s.handleGetUsageGuide,
	)

	// get_working_state
	s.mcp.AddTool(
		mcp.NewTool(
			"get_working_state",
			mcp.WithDescription(
				"Returns recent file changes detected by the file watcher, answering "+
					"'what was the developer just working on?' "+
					"Also includes a git diff stat for the current working tree. "+
					"Call this at session start to orient yourself to recent activity.",
			),
			mcp.WithNumber("window_minutes",
				mcp.Description("Look-back window in minutes. Defaults to 15."),
			),
		),
		s.handleGetWorkingState,
	)

	// ── Multi-Agent Coordination ─────────────────────────────────────────────

	// claim_work
	s.mcp.AddTool(
		mcp.NewTool(
			"claim_work",
			mcp.WithDescription(
				"Registers an agent's active work on a scope (file, package, directory, or entity). "+
					"Prevents two agents from unknowingly editing the same code at the same time. "+
					"Returns any conflicting claims by other agents immediately, so the caller can "+
					"decide whether to proceed or coordinate first. Claims expire automatically after ttl_minutes. "+
					"Call release_claims (or let the TTL expire) when work is done.",
			),
			mcp.WithString("agent_id", mcp.Required(), mcp.Description("Identifier of the calling agent.")),
			mcp.WithString("scope", mcp.Required(), mcp.Description("What is being claimed: file path, package name, directory path, or entity name.")),
			mcp.WithString("scope_type", mcp.Description("How to interpret scope: 'file' (default), 'package', 'directory', or 'entity'.")),
			mcp.WithNumber("ttl_minutes", mcp.Description("How long the claim is valid. Default: 30 minutes.")),
		),
		s.handleClaimWork,
	)

	// get_conflicts
	s.mcp.AddTool(
		mcp.NewTool(
			"get_conflicts",
			mcp.WithDescription(
				"Returns all work claims by other agents that overlap with any scope the given agent "+
					"currently holds. Also returns the agent's own active claims. "+
					"Use this to detect coordination problems before starting work on a shared codebase. "+
					"Expired claims are pruned automatically before checking.",
			),
			mcp.WithString("agent_id", mcp.Required(), mcp.Description("Identifier of the calling agent.")),
		),
		s.handleGetConflicts,
	)

	// release_claims
	s.mcp.AddTool(
		mcp.NewTool(
			"release_claims",
			mcp.WithDescription(
				"Releases all active work claims held by the given agent. "+
					"Call this when you are done editing a file/package/entity to free the scope for other agents. "+
					"Claims also expire automatically after their TTL.",
			),
			mcp.WithString("agent_id", mcp.Required(), mcp.Description("Identifier of the calling agent.")),
		),
		s.handleReleaseClaims,
	)

	// ── Community Detection ──────────────────────────────────────────────────

	// get_communities
	s.mcp.AddTool(
		mcp.NewTool(
			"get_communities",
			mcp.WithDescription(
				"Detects emergent community clusters in the codebase using Label Propagation (LPA). "+
					"Nodes that call each other frequently end up in the same community regardless of directory. "+
					"Pure-package communities indicate clean boundaries; mixed-package communities reveal "+
					"hidden coupling across module lines. Returns communities sorted by size with a "+
					"modularity score (0–1, higher = stronger separation).",
			),
			mcp.WithNumber("max_iterations",
				mcp.Description("Max LPA rounds before stopping (default 10). Higher = more stable but slower."),
			),
			mcp.WithNumber("min_community_size",
				mcp.Description("Minimum nodes per community to include in results (default 2, hides singletons)."),
			),
			mcp.WithBoolean("include_nodes",
				mcp.Description("Include member node lists per community (default false). Omit for a compact summary."),
			),
		),
		s.handleGetCommunities,
	)

	// ── Visualization Export ─────────────────────────────────────────────────

	// export_graph
	s.mcp.AddTool(
		mcp.NewTool(
			"export_graph",
			mcp.WithDescription(
				"Exports a subgraph (around an entity) or the full graph as DOT (Graphviz), Mermaid, or GraphML. "+
					"Use for documentation, PR diagrams, and architectural analysis. "+
					"For ego-subgraph: provide entity name and depth. Omit entity for full-graph export.",
			),
			mcp.WithString("entity",
				mcp.Description("Optional root node name for ego-subgraph export. Omit for full-graph export."),
			),
			mcp.WithString("format",
				mcp.Description("Output format: 'dot' (Graphviz), 'mermaid', or 'graphml'. Default: 'dot'."),
			),
			mcp.WithNumber("depth",
				mcp.Description("Max hop depth for ego-subgraph. Default: 2."),
			),
			mcp.WithBoolean("include_metadata",
				mcp.Description("Include doc/signature in node labels. Default: false."),
			),
		),
		s.handleExportGraph,
	)

	// ── Data Flow Analysis ────────────────────────────────────────────────────

	// find_data_paths
	s.mcp.AddTool(
		mcp.NewTool(
			"find_data_paths",
			mcp.WithDescription(
				"Finds paths from source nodes (HTTP inputs, parsers, env vars) to sink nodes "+
					"(SQL exec, file writes, exec.Command). Answers: 'can user input reach this database call?' "+
					"Sources and sinks are detected automatically from function signatures; "+
					"add custom patterns via data_flow_sources/data_flow_sinks in synapses.json. "+
					"Requires a fresh index (synapses index --reindex) to populate DATA_FLOWS edges.",
			),
			mcp.WithString("source",
				mcp.Description("Optional: entity name to trace forward from (show reachable sinks)."),
			),
			mcp.WithString("sink",
				mcp.Description("Optional: entity name to trace backward to (show sources that reach it)."),
			),
		),
		s.handleFindDataPaths,
	)

	// ── Agent Consensus Tools ────────────────────────────────────────────────

	// propose_change
	s.mcp.AddTool(
		mcp.NewTool(
			"propose_change",
			mcp.WithDescription(
				"Proposes an architectural change that other agents can vote on. "+
					"When multiple agents may have conflicting opinions (e.g. merge vs split a package), "+
					"use this to surface the conflict and reach a documented consensus before implementing. "+
					"A proposal is resolved when approve or reject votes reach vote_threshold.",
			),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Short description of the proposed change, e.g. 'Merge handlers and services packages'."),
			),
			mcp.WithString("description",
				mcp.Description("Detailed rationale and affected code. Supports multi-line text."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Self-declared identifier of the proposing agent."),
			),
			mcp.WithString("affected_nodes",
				mcp.Description("Optional JSON array of node IDs that this change touches. Auto-detected from title+description if omitted."),
			),
			mcp.WithNumber("vote_threshold",
				mcp.Description("Number of approve (or reject) votes required to resolve the proposal. Default: 2."),
			),
		),
		s.handleProposeChange,
	)

	// vote_on_proposal
	s.mcp.AddTool(
		mcp.NewTool(
			"vote_on_proposal",
			mcp.WithDescription(
				"Casts a vote on an open architectural change proposal. "+
					"Each agent gets one vote per proposal (updates if you vote again). "+
					"When approve or reject votes reach the threshold the proposal is resolved automatically. "+
					"Returns the current tally and resolution status.",
			),
			mcp.WithString("proposal_id",
				mcp.Required(),
				mcp.Description("ID of the proposal to vote on (from propose_change or get_proposals)."),
			),
			mcp.WithString("vote",
				mcp.Required(),
				mcp.Description("Your vote: 'approve', 'reject', or 'abstain'."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Self-declared identifier of the voting agent."),
			),
			mcp.WithString("rationale",
				mcp.Description("Optional explanation of your vote, stored for audit purposes."),
			),
		),
		s.handleVoteOnProposal,
	)

	// withdraw_proposal
	s.mcp.AddTool(
		mcp.NewTool(
			"withdraw_proposal",
			mcp.WithDescription(
				"Withdraws an open proposal. Only the agent that created it can withdraw it. "+
					"Use this if circumstances changed and the proposal is no longer relevant.",
			),
			mcp.WithString("proposal_id",
				mcp.Required(),
				mcp.Description("ID of the proposal to withdraw."),
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("Self-declared identifier of the proposing agent (must match the creator)."),
			),
		),
		s.handleWithdrawProposal,
	)

	// get_proposals
	s.mcp.AddTool(
		mcp.NewTool(
			"get_proposals",
			mcp.WithDescription(
				"Lists architectural change proposals, optionally filtered by status. "+
					"Call with no arguments (or status='open') to see proposals waiting for votes. "+
					"Use at session start to check for pending decisions before making changes.",
			),
			mcp.WithString("status",
				mcp.Description("Filter by status: 'open', 'accepted', 'rejected', or 'withdrawn'. Omit for all."),
			),
		),
		s.handleGetProposals,
	)

}

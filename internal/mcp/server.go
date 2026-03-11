// Package mcp implements the Model Context Protocol server for Synapses.
// Agents (Claude Code, Cursor, etc.) connect to this server over stdio and
// call the registered tools to query the code graph.
package mcp

import (
	"context"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
)

// ChangeSource is implemented by types that maintain a recent file-change log.
// Typically this is *watcher.Watcher, wired in cmdStart via SetChangeSource.
type ChangeSource interface {
	RecentChanges(windowMinutes int) []watcher.ChangeEvent
}

const serverName = "synapses"

// Version is the server version advertised to MCP clients.
// Set from main.go via ldflags-injected version before creating the server.
var Version = "dev"

// packetCacheEntry holds a cached context packet with an expiry time.
type packetCacheEntry struct {
	pkt       interface{} // *brain.ContextPacket (typed as interface{} to avoid import cycle)
	expiresAt time.Time
}

// Server holds the MCP server and the dependencies that tool handlers need.
type Server struct {
	mcp          *server.MCPServer
	graph        *graph.Graph
	config       *config.Config
	store        *store.Store  // nil if started without a persistent store
	changeSource ChangeSource  // nil if started without a file watcher
	peerManager  interface{}   // *peer.PeerManager — set via SetPeerManager; nil if no peers configured
	brainClient  interface{}   // *brain.Client — set via SetBrainClient; nil if brain not configured
	scoutClient  interface{}   // *scout.Client — set via SetScoutClient; nil if scout not configured
	pulseClient  interface{}   // *pulse.Client — set via SetPulseClient; nil if pulse not configured
	embedClient  *embed.Client // nil if embedding_endpoint not configured
	techStack    interface{}   // []scout.TechStackEntry — set via SetTechStack after autosubscribe
	rulesMu      sync.RWMutex  // protects s.config.Rules for concurrent dynamic upserts

	// Context-packet cache: 20 slots max, 30s TTL. Keyed by "entityName:depth".
	packetCacheMu sync.Mutex
	packetCache   map[string]*packetCacheEntry

	// Brain cache warming debounce: prevents hammering the brain on rapid saves.
	warmMu   sync.Mutex
	lastWarm time.Time
}

// New creates a Server wired to the given graph, config, and optional store.
// The store is required for Agent Task Memory tools (create_plan, get_pending_tasks, etc.).
// Pass nil for st if running in a context without persistence (e.g. tests).
// All tools are registered during construction.
func New(g *graph.Graph, cfg *config.Config, st *store.Store) *Server {
	s := &Server{
		graph:       g,
		config:      cfg,
		store:       st,
		packetCache: make(map[string]*packetCacheEntry, 20),
	}

	// Restore dynamic rules persisted from previous sessions. This runs before
	// registerTools() so that validate_plan / get_violations are immediately aware
	// of all previously upserted rules without requiring a daemon restart.
	if st != nil {
		if dynamicRules, err := st.LoadDynamicRules(); err == nil && len(dynamicRules) > 0 {
			cfg.Rules = append(cfg.Rules, dynamicRules...)
		}
	}

	// Usage observability: wire before/after hooks to record every tool call
	// timing and success status into the tool_calls SQLite table.
	hooks := &server.Hooks{}
	startTimes := &callStartTimes{}
	hooks.AddBeforeCallTool(func(_ context.Context, _ any, req *mcp.CallToolRequest) {
		startTimes.set(req.Params.Name, time.Now())
	})
	hooks.AddAfterCallTool(func(_ context.Context, _ any, req *mcp.CallToolRequest, result *mcp.CallToolResult) {
		elapsed := time.Since(startTimes.pop(req.Params.Name))
		success := result == nil || !result.IsError
		agentID, _ := req.GetArguments()["agent_id"].(string)
		entity, _ := req.GetArguments()["entity"].(string)
		if entity == "" {
			entity, _ = req.GetArguments()["query"].(string)
		}
		if s.store != nil {
			s.store.RecordToolCall(req.Params.Name, agentID, entity, elapsed.Milliseconds(), success)
		}
		// Fire-and-forget telemetry to synapses-pulse (if configured).
		if pc := s.getPulseClient(); pc != nil {
			var responseBytes int
			if result != nil && !result.IsError && len(result.Content) > 0 {
				if tc, ok := result.Content[0].(mcp.TextContent); ok {
					responseBytes = len(tc.Text)
				}
			}
			go pc.RecordToolCall(pulse.ToolCallEvent{
				ToolName:      req.Params.Name,
				AgentID:       agentID,
				Entity:        entity,
				DurationMs:    elapsed.Milliseconds(),
				Success:       success,
				ResponseBytes: responseBytes,
			})
		}
	})

	s.mcp = server.NewMCPServer(serverName, Version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true), // subscribe + listChanged
		server.WithHooks(hooks),
	)
	s.registerTools()
	s.registerResources()
	return s
}

// callStartTimes is a simple concurrent map for per-tool-call start timestamps.
// It uses the tool name as key; concurrent calls to the same tool will race,
// but that is acceptable — timing accuracy is best-effort for observability.
type callStartTimes struct {
	mu   sync.Mutex
	data map[string]time.Time
}

func (c *callStartTimes) set(name string, t time.Time) {
	c.mu.Lock()
	if c.data == nil {
		c.data = make(map[string]time.Time)
	}
	c.data[name] = t
	c.mu.Unlock()
}

func (c *callStartTimes) pop(name string) time.Time {
	c.mu.Lock()
	t := c.data[name]
	delete(c.data, name)
	c.mu.Unlock()
	return t
}

const (
	packetCacheTTL = 30 * time.Second
	packetCacheMax = 20
)

// getPacketFromCache returns a cached context packet for the given key, or nil
// if the entry is absent or expired.
func (s *Server) getPacketFromCache(key string) interface{} {
	s.packetCacheMu.Lock()
	defer s.packetCacheMu.Unlock()
	e, ok := s.packetCache[key]
	if !ok || time.Now().After(e.expiresAt) {
		delete(s.packetCache, key)
		return nil
	}
	return e.pkt
}

// setPacketCache stores a context packet under key with a 30s TTL.
// When the cache exceeds packetCacheMax entries, the entry with the oldest
// expiresAt timestamp is evicted (LRU-style). With ≤20 slots, the linear
// scan is trivial.
func (s *Server) setPacketCache(key string, pkt interface{}) {
	s.packetCacheMu.Lock()
	defer s.packetCacheMu.Unlock()
	if len(s.packetCache) >= packetCacheMax {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range s.packetCache {
			if oldestKey == "" || v.expiresAt.Before(oldestTime) {
				oldestKey, oldestTime = k, v.expiresAt
			}
		}
		delete(s.packetCache, oldestKey)
	}
	s.packetCache[key] = &packetCacheEntry{pkt: pkt, expiresAt: time.Now().Add(packetCacheTTL)}
}

// InvalidatePacketCache clears the entire context-packet cache. Satisfies the
// watcher.PacketCacheInvalidator interface; delegates to InvalidatePacketCacheForFile
// with an empty path so resource notifications and brain warming are also triggered.
func (s *Server) InvalidatePacketCache() {
	s.InvalidatePacketCacheForFile("")
}

// SetChangeSource wires a change event source (typically the file watcher) so
// get_working_state can report recent file activity to agents.
func (s *Server) SetChangeSource(cs ChangeSource) {
	s.changeSource = cs
}

// SetPeerManager wires a *peer.PeerManager into the server so that the
// list_peers, get_peer_context, and get_dependency_graph tools are functional.
// Using interface{} avoids an import cycle (peer imports graph/store but not mcp).
func (s *Server) SetPeerManager(pm interface{}) {
	s.peerManager = pm
}

// SetBrainClient wires a *brain.Client into the server so that get_context
// returns enriched Context Packets and violations include LLM explanations.
// Using interface{} avoids an import cycle (brain imports only stdlib).
func (s *Server) SetBrainClient(bc interface{}) {
	s.brainClient = bc
}

// SetScoutClient wires a *scout.Client into the server so that web_search,
// web_fetch, and web_deep_search tools are functional.
// Using interface{} avoids an import cycle (scout imports only stdlib).
func (s *Server) SetScoutClient(sc interface{}) {
	s.scoutClient = sc
}

// SetPulseClient wires a *pulse.Client into the server so that every tool
// call emits telemetry to the synapses-pulse analytics sidecar.
func (s *Server) SetPulseClient(pc *pulse.Client) {
	s.pulseClient = pc
}

// getPulseClient type-asserts the stored pulseClient to *pulse.Client.
// Returns nil if no pulse client is configured.
func (s *Server) getPulseClient() *pulse.Client {
	if s.pulseClient == nil {
		return nil
	}
	pc, _ := s.pulseClient.(*pulse.Client)
	return pc
}

// SetTechStack stores the detected tech stack entries ([]scout.TechStackEntry)
// so that get_project_identity can surface them as tech_stack.
// Called from cmdStart after autosubscribe detection completes.
func (s *Server) SetTechStack(ts interface{}) {
	s.techStack = ts
}

// SetEmbedClient wires an embedding client so handleSemanticSearch can
// perform vector similarity search in addition to FTS5 BM25 ranking.
// Pass nil to disable vector search (falls back to FTS5-only).
func (s *Server) SetEmbedClient(ec *embed.Client) {
	s.embedClient = ec
}

// ServeStdio starts the MCP server on stdin/stdout. This call blocks until
// the client disconnects or the process receives a signal.
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.mcp)
}

// registerTools wires all Synapses tool definitions to their handlers.
func (s *Server) registerTools() {
	// ── Session Bootstrap ────────────────────────────────────────────────────

	// session_init: single round-trip startup replacing the 3-call ritual.
	// Supports incremental mode: when agent_id is provided and the agent has
	// called session_init before, unchanged sections (e.g. project_identity)
	// are omitted to save tokens. The agent's context profile is updated
	// after each call so subsequent calls are incremental.
	s.mcp.AddTool(
		mcp.NewTool(
			"session_init",
			mcp.WithDescription(
				"Single-call session bootstrap. Returns pending_tasks, project_identity, "+
					"working_state, and recent_events in one round-trip — replacing the "+
					"three-step startup ritual. Includes scale_guidance so agents self-tune "+
					"their tool usage to repo size. Call this INSTEAD of the three individual tools. "+
					"Incremental mode: when agent_id is provided and the agent has called "+
					"session_init before, unchanged sections are skipped to save tokens "+
					"(e.g. project_identity is omitted if the graph hasn't changed).",
			),
			mcp.WithString("agent_id",
				mcp.Description("Self-declared agent identifier. Enables incremental delivery: "+
					"subsequent calls skip unchanged project_identity and filter events to only "+
					"those since the last session. Always provide for token savings."),
			),
		),
		s.handleSessionInit,
	)

	// discover_tools: lightweight tool finder
	s.mcp.AddTool(
		mcp.NewTool(
			"discover_tools",
			mcp.WithDescription(
				"Finds the right Synapses tool for a task. Describe what you need in natural language "+
					"and get back the top matching tools with usage examples. "+
					"Use this instead of scanning all tool definitions. ~300 tokens vs ~4200.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Natural language description of what you need, e.g. 'check what calls this function' or 'save my progress'."),
			),
		),
		s.handleDiscoverTools,
	)

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
			mcp.WithString("file",
				mcp.Description("Optional file path suffix to pin the lookup to a specific file (e.g. 'cmd/synapses/main.go'). Use when entity names are ambiguous across multiple files."),
			),
			mcp.WithString("format",
				mcp.Description("Output format: 'json' (default, full JSON blob ~2000-3800 tokens) or 'compact' (natural-language briefing ~400-600 tokens). Use 'compact' to reduce token usage when brain summaries are available."),
			),
			mcp.WithString("detail_level",
				mcp.Description("Only used with format='compact'. Controls verbosity: 'summary' (~50 tokens, root entity header + warnings only), 'neighbors' (~200 tokens, adds Calls/Called-by name lists), 'full' (default, ~400-600 tokens, adds callee detail blocks and insight)."),
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

	// get_violations (absorbs get_violation_log via rule_id + limit params)
	s.mcp.AddTool(
		mcp.NewTool(
			"get_violations",
			mcp.WithDescription(
				"Lists all current architectural rule violations found in the graph. "+
					"Returns rule ID, severity, affected nodes, and a human-readable description. "+
					"Pass rule_id to filter to a specific rule. Pass include_log=true to also return the historical audit log.",
			),
			mcp.WithString("rule_id",
				mcp.Description("Optional. Filter violations to a specific rule ID."),
			),
			mcp.WithBoolean("include_log",
				mcp.Description("When true, also returns the historical violation log entries. Default false."),
			),
			mcp.WithNumber("log_limit",
				mcp.Description("Max historical log entries to return when include_log=true. Default 50."),
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

	// search (absorbs semantic_search via mode param)
	s.mcp.AddTool(
		mcp.NewTool(
			"search",
			mcp.WithDescription(
				"Keyword search across entity names and doc comments. "+
					"Results are ranked: exact name match > name prefix > name substring > doc comment match. "+
					"Returns up to 25 results. Use this to find auth-related code, error handlers, etc. "+
					"Set mode='fulltext' for FTS5 BM25 search by concept ('rate limiting', 'JWT validation'). 'semantic' is accepted as alias. "+
					"CamelCase names are auto-split: searching 'carve' finds 'CarveEgoGraph'.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Search term (case-insensitive)."),
			),
			mcp.WithString("mode",
				mcp.Description("Search mode: 'keyword' (default, exact/prefix/substring) or 'semantic' (FTS BM25 by concept)."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum results to return (default 20, max 50). Only used for mode=semantic."),
			),
		),
		s.handleSearch,
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

	// save_session_state
	s.mcp.AddTool(
		mcp.NewTool(
			"save_session_state",
			mcp.WithDescription(
				"Saves the exact working state for an in-progress task so the next LLM session "+
					"can resume from precisely where this session stopped. Call this at the end of "+
					"a session or whenever significant progress is made. "+
					"The state is included automatically in get_pending_tasks() for in_progress tasks.",
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("The task ID from get_pending_tasks."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Self-declared agent identifier."),
			),
			mcp.WithString("approach",
				mcp.Description("Current strategy or approach being taken for this task."),
			),
			mcp.WithString("files_modified",
				mcp.Description("JSON array of file paths modified so far, e.g. [\"internal/store/tasks.go\"]."),
			),
			mcp.WithString("completed_steps",
				mcp.Description("JSON array of step descriptions already completed."),
			),
			mcp.WithString("remaining_steps",
				mcp.Description("JSON array of step descriptions still to do."),
			),
			mcp.WithString("blockers",
				mcp.Description("JSON array of blocker descriptions (empty if none)."),
			),
			mcp.WithString("decisions",
				mcp.Description("JSON array of key decisions made during this session."),
			),
			mcp.WithString("context_snapshot",
				mcp.Description("Free-form text snapshot of the current LLM context (key facts, state, etc.)."),
			),
		),
		s.handleSaveSessionState,
	)

	// get_session_state
	s.mcp.AddTool(
		mcp.NewTool(
			"get_session_state",
			mcp.WithDescription(
				"Returns the saved session state for a task, enabling exact-moment resumption "+
					"of work started in a previous LLM session. "+
					"Note: get_pending_tasks() already includes session state for in_progress tasks inline; "+
					"use this tool only when you need to fetch state for a specific task explicitly.",
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("The task ID to retrieve session state for."),
			),
		),
		s.handleGetSessionState,
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

	// handoff_task: transfer task ownership between agents with session state.
	s.mcp.AddTool(
		mcp.NewTool(
			"handoff_task",
			mcp.WithDescription(
				"Transfers a task to another agent with context. The departing agent's session "+
					"state is preserved so the receiving agent can resume seamlessly. Use this when "+
					"handing off work between agents or LLM sessions.",
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("The task ID to hand off."),
			),
			mcp.WithString("from_agent",
				mcp.Required(),
				mcp.Description("The agent currently owning the task."),
			),
			mcp.WithString("to_agent",
				mcp.Required(),
				mcp.Description("The agent receiving the task."),
			),
			mcp.WithString("notes",
				mcp.Description("Optional handoff notes explaining current state, blockers, or next steps."),
			),
		),
		s.handleHandoffTask,
	)

	// ── Coordination & Multi-Agent Tools ────────────────────────────────────

	// get_plans
	s.mcp.AddTool(
		mcp.NewTool(
			"get_plans",
			mcp.WithDescription(
				"List all saved plans with task completion counts. "+
					"Use this to get an overview of all active and completed work across sessions.",
			),
		),
		s.handleGetPlans,
	)

	// get_my_tasks
	s.mcp.AddTool(
		mcp.NewTool(
			"get_my_tasks",
			mcp.WithDescription(
				"Returns unblocked pending tasks assigned to or created by a specific agent, "+
					"with a suggested next task. Scoped to agent_id so each agent sees only its own work.",
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The agent's self-declared identifier."),
			),
			mcp.WithString("plan_id",
				mcp.Description("Optional. Filter to tasks belonging to a specific plan."),
			),
		),
		s.handleGetMyTasks,
	)

	// link_task_nodes
	s.mcp.AddTool(
		mcp.NewTool(
			"link_task_nodes",
			mcp.WithDescription(
				"Explicitly links a task to graph node IDs. "+
					"Linked nodes get a relevance boost when get_context is called with task_id=. "+
					"Replaces any existing links for the task.",
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("The task ID to link nodes to."),
			),
			mcp.WithString("node_ids",
				mcp.Required(),
				mcp.Description("JSON array of node ID strings, e.g. [\"repo::pkg/auth.go::AuthService\"]."),
			),
		),
		s.handleLinkTaskNodes,
	)

	// get_agents
	s.mcp.AddTool(
		mcp.NewTool(
			"get_agents",
			mcp.WithDescription(
				"Returns all agents that have interacted with Synapses, ordered by last-seen timestamp. "+
					"Useful for understanding who else is working in this repository.",
			),
		),
		s.handleGetAgents,
	)

	// claim_work
	s.mcp.AddTool(
		mcp.NewTool(
			"claim_work",
			mcp.WithDescription(
				"Registers active work on a file, package, directory, or entity. "+
					"Returns any conflicting claims by other agents immediately. "+
					"Call before editing to coordinate with other agents. "+
					"Claims expire automatically after ttl_minutes (default 30).",
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The agent's self-declared identifier."),
			),
			mcp.WithString("scope",
				mcp.Required(),
				mcp.Description("Path or entity being claimed, e.g. 'internal/auth' or 'cmd/server/main.go'."),
			),
			mcp.WithString("scope_type",
				mcp.Description("Type of scope: 'file', 'package', 'directory', 'entity'. Defaults to 'path'."),
			),
			mcp.WithNumber("ttl_minutes",
				mcp.Description("How long to hold the claim in minutes. Defaults to 30."),
			),
		),
		s.handleClaimWork,
	)

	// release_claims
	s.mcp.AddTool(
		mcp.NewTool(
			"release_claims",
			mcp.WithDescription(
				"Releases all active work claims for the given agent. "+
					"Call this when done editing to immediately free scopes for other agents.",
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The agent's self-declared identifier."),
			),
		),
		s.handleReleaseClaims,
	)

	// get_conflicts
	s.mcp.AddTool(
		mcp.NewTool(
			"get_conflicts",
			mcp.WithDescription(
				"Returns all work claims by other agents that overlap with the calling agent's current claims. "+
					"Check this before starting large edits if you are working in a multi-agent environment.",
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The agent's self-declared identifier."),
			),
		),
		s.handleGetConflicts,
	)

	// get_events
	s.mcp.AddTool(
		mcp.NewTool(
			"get_events",
			mcp.WithDescription(
				"Returns recent events from the pull-based event log: file_change, task_update, "+
					"annotation_added, agent_activity. Poll with since_seq cursor (from session_init "+
					"or the previous call's latest_seq) to get only new events.",
			),
			mcp.WithNumber("since_seq",
				mcp.Description("Return events with seq greater than this value. Use 0 for all recent events."),
			),
			mcp.WithString("types",
				mcp.Description("Comma-separated list of event types to filter by, e.g. 'file_change,task_update'. Omit for all types."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum events to return. Defaults to 50."),
			),
		),
		s.handleGetEvents,
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

	// ── Session Awareness Tools ──────────────────────────────────────────────

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

	// ── Web Tools (requires synapses-scout sidecar) ──────────────────────────

	// web_search
	s.mcp.AddTool(
		mcp.NewTool(
			"web_search",
			mcp.WithDescription(
				"Searches the web via the synapses-scout sidecar and returns structured results. "+
					"Use this to find framework docs, API references, error solutions, or any "+
					"information that requires real-time data. "+
					"Requires scout.url in synapses.json (default: http://localhost:11436).",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("The search query."),
			),
			mcp.WithNumber("max_results",
				mcp.Description("Maximum results to return. Default 5."),
			),
			mcp.WithString("region",
				mcp.Description("Optional region code for localised results, e.g. 'us-en'."),
			),
			mcp.WithString("timelimit",
				mcp.Description("Optional time limit, e.g. 'd' (day), 'w' (week), 'm' (month)."),
			),
		),
		s.handleWebSearch,
	)

	// web_fetch
	s.mcp.AddTool(
		mcp.NewTool(
			"web_fetch",
			mcp.WithDescription(
				"Fetches and extracts a URL (or performs a search if given a query) via "+
					"synapses-scout, returning the content as Markdown. "+
					"Use this to read documentation pages, GitHub READMEs, or any web content "+
					"that an agent needs to reason about. "+
					"Requires scout.url in synapses.json (default: http://localhost:11436).",
			),
			mcp.WithString("input",
				mcp.Required(),
				mcp.Description("A URL to fetch or a plain-text search query (scout auto-routes)."),
			),
			mcp.WithBoolean("force_refresh",
				mcp.Description("When true, bypasses the scout cache and re-fetches. Default false."),
			),
		),
		s.handleWebFetch,
	)

	// web_annotate
	s.mcp.AddTool(
		mcp.NewTool(
			"web_annotate",
			mcp.WithDescription(
				"Persists web findings as a graph node annotation so they survive across sessions "+
					"and appear in get_context for that node. "+
					"This is the 'context sharing' pattern: web research becomes a first-class "+
					"data object attached to a code entity, visible to all future agent sessions. "+
					"Pass hits (JSON array from web_search) or a plain note string, or both.",
			),
			mcp.WithString("node_id",
				mcp.Required(),
				mcp.Description("The node ID to annotate (from find_entity or get_context)."),
			),
			mcp.WithString("note",
				mcp.Description("Plain-text note summarising what was found. Used as-is or prepended to hits."),
			),
			mcp.WithString("hits",
				mcp.Description("JSON array of search hits from web_search, e.g. [{\"title\":\"...\",\"url\":\"...\",\"snippet\":\"...\"}]."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Optional. Self-declared agent identifier for attribution."),
			),
		),
		s.handleWebAnnotate,
	)

	// web_deep_search
	s.mcp.AddTool(
		mcp.NewTool(
			"web_deep_search",
			mcp.WithDescription(
				"Performs an orchestrated multi-query search via synapses-scout: expands the "+
					"original query into sub-queries, fans out searches, deduplicates, and returns "+
					"a richer result set than web_search. Use this for research tasks where "+
					"a single query is unlikely to capture all relevant results. "+
					"Requires scout.url in synapses.json (default: http://localhost:11436).",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("The research query to expand and search."),
			),
			mcp.WithNumber("max_results",
				mcp.Description("Maximum deduplicated results to return. Default 10."),
			),
			mcp.WithString("region",
				mcp.Description("Optional region code for localised results, e.g. 'us-en'."),
			),
			mcp.WithString("timelimit",
				mcp.Description("Optional time limit, e.g. 'd' (day), 'w' (week), 'm' (month)."),
			),
		),
		s.handleWebDeepSearch,
	)

	// lookup_docs
	s.mcp.AddTool(
		mcp.NewTool(
			"lookup_docs",
			mcp.WithDescription(
				"One-shot documentation lookup: searches the web and fetches the top result in a "+
					"single call. Use this BEFORE writing code that uses external packages or APIs "+
					"to verify you have the current API — training data may be outdated. "+
					"Faster than calling web_search then web_fetch separately. "+
					"Requires scout.url in synapses.json (default: http://localhost:11436).",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description(
					"What to look up. Include the package name, topic, and language for best results. "+
						"Examples: 'openai python chat completions API', 'react hooks useState', "+
						"'docker compose v2 CLI flags', 'langchain LCEL chain syntax'.",
				),
			),
			mcp.WithNumber("max_chars",
				mcp.Description("Maximum characters of page content to return. Default 6000 (~1500 tokens)."),
			),
		),
		s.handleLookupDocs,
	)

	// upsert_adr
	s.mcp.AddTool(
		mcp.NewTool(
			"upsert_adr",
			mcp.WithDescription(
				"Creates or updates an Architectural Decision Record (ADR) in the brain. "+
					"ADRs are persistent cold-memory entries for significant design choices — "+
					"they appear in get_context compact output when linked_files match the entity's file. "+
					"Requires brain.url to be configured in synapses.json.",
			),
			mcp.WithString("id",
				mcp.Required(),
				mcp.Description("Unique identifier for the ADR, e.g. 'adr-001-no-cgo'. Use kebab-case."),
			),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Short, declarative title of the decision, e.g. 'No CGo — use modernc/sqlite'."),
			),
			mcp.WithString("decision",
				mcp.Required(),
				mcp.Description("The decision made, in 1-3 sentences."),
			),
			mcp.WithString("status",
				mcp.Description("One of: proposed, accepted, deprecated, superseded. Defaults to 'proposed'."),
			),
			mcp.WithString("context",
				mcp.Description("Problem context and forces that led to this decision."),
			),
			mcp.WithString("consequences",
				mcp.Description("Consequences and trade-offs of this decision."),
			),
			mcp.WithArray("linked_files",
				mcp.Description("File path patterns to associate with this ADR (e.g. [\"internal/auth/\", \"cmd/server.go\"]). Used by get_adrs(file=) to surface relevant ADRs for a given file."),
				mcp.Items(map[string]any{"type": "string"}),
			),
		),
		s.handleUpsertADR,
	)

	// prepare_context — intent-based context assembly (replaces multi-tool chains).
	s.mcp.AddTool(
		mcp.NewTool(
			"prepare_context",
			mcp.WithDescription(
				"Intent-based context assembly. Declare WHAT you need and a target; "+
					"Synapses composes the right context in one round-trip. "+
					"Replaces chains like get_context→get_impact→get_violations.\n"+
					"Intents: 'modify' (safe-edit briefing), 'understand' (structure), "+
					"'review' (quality/risk), 'debug' (call-path trace), "+
					"'add' (conventions for new code), 'plan' (dry-run scope assessment).",
			),
			mcp.WithString("intent",
				mcp.Required(),
				mcp.Description("'modify' | 'understand' | 'review' | 'debug' | 'add' | 'plan'"),
			),
			mcp.WithString("target",
				mcp.Required(),
				mcp.Description("Entity name, file path suffix, or search query."),
			),
			mcp.WithString("file",
				mcp.Description("Optional file path suffix to disambiguate entity names."),
			),
			mcp.WithString("task_id",
				mcp.Description("Optional task ID for relevance boosting."),
			),
			mcp.WithNumber("token_budget",
				mcp.Description("Max tokens for the response. Defaults vary by intent (1500–3500)."),
			),
		),
		s.handlePrepareContext,
	)

	// ── Agent Message Bus ────────────────────────────────────────────────────
	// Phase 1: direct + broadcast messaging between ephemeral agent sessions.
	// Transport: SQLite (not HTTP) — agents are ephemeral, need a broker.
	// Semantics: A2A-lite (topic-based, task-linked payloads, lifecycle: unread→read).

	// send_message
	s.mcp.AddTool(
		mcp.NewTool(
			"send_message",
			mcp.WithDescription(
				"Sends a message from one agent to another, or broadcasts it to all agents. "+
					"Use this to notify peers about API changes, task blockers, or plan updates. "+
					"to_agent omitted = broadcast. payload must be valid JSON.",
			),
			mcp.WithString("from_agent",
				mcp.Required(),
				mcp.Description("Self-declared sender identity (e.g. 'backend-claude')."),
			),
			mcp.WithString("topic",
				mcp.Required(),
				mcp.Description("Message topic, e.g. 'api_changed', 'task_blocked', 'plan_updated'."),
			),
			mcp.WithString("payload",
				mcp.Description("JSON payload with message details. Defaults to '{}'. Example: '{\"endpoint\":\"/api/users\",\"change\":\"added role field\"}'."),
			),
			mcp.WithString("to_agent",
				mcp.Description("Target agent ID. Omit (or leave empty) to broadcast to all agents."),
			),
			mcp.WithString("project_id",
				mcp.Description("Optional repo context identifier (e.g. 'my-backend'). Used for cross-project coordination."),
			),
		),
		s.handleSendMessage,
	)

	// get_messages
	s.mcp.AddTool(
		mcp.NewTool(
			"get_messages",
			mcp.WithDescription(
				"Retrieves messages from the agent message bus visible to the calling agent. "+
					"Includes direct messages and broadcasts (to_agent omitted). "+
					"Use since_seq cursor for efficient polling. unread_only=true by default.",
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The calling agent's ID. Only messages addressed to this agent or broadcast are returned."),
			),
			mcp.WithNumber("since_seq",
				mcp.Description("Return messages with seq greater than this value. Use 0 or omit for all. Use latest_seq from previous call as cursor."),
			),
			mcp.WithString("topic_filter",
				mcp.Description("Optional. Only return messages with this exact topic (e.g. 'api_changed')."),
			),
			mcp.WithString("unread_only",
				mcp.Description("If 'true' (default), only return unread messages. Pass 'false' to retrieve all messages including already-read ones."),
			),
			mcp.WithNumber("limit",
				mcp.Description("Maximum messages to return. Defaults to 50."),
			),
		),
		s.handleGetMessages,
	)

	// mark_read
	s.mcp.AddTool(
		mcp.NewTool(
			"mark_read",
			mcp.WithDescription(
				"Marks a message as read by the calling agent. "+
					"Idempotent — calling it on an already-read message is safe. "+
					"Call this after processing a message so it no longer appears in get_messages(unread_only=true).",
			),
			mcp.WithString("message_id",
				mcp.Required(),
				mcp.Description("The message ID to mark as read (from get_messages response)."),
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The agent marking the message as read."),
			),
		),
		s.handleMarkRead,
	)

	// remember
	s.mcp.AddTool(
		mcp.NewTool(
			"remember",
			mcp.WithDescription(
				"Records a decision or failure as an episode in persistent memory. "+
					"Use episode_type='failure' + outcome='failure' to populate the Hall of Shame "+
					"so check_plan_safety() can warn future agents. "+
					"Use episode_type='decision' for general architectural choices.",
			),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("Self-declared agent identifier (e.g. 'claude-code-session-1')."),
			),
			mcp.WithString("decision",
				mcp.Required(),
				mcp.Description("The decision made or failure observed. Concise, 1-2 sentences."),
			),
			mcp.WithString("episode_type",
				mcp.Description("One of: decision (default), failure, pattern, rule_proposal."),
			),
			mcp.WithString("outcome",
				mcp.Description("One of: success, failure, partial, unknown (default)."),
			),
			mcp.WithString("rationale",
				mcp.Description("Why this decision was made or why it failed (1-3 sentences)."),
			),
			mcp.WithString("trigger",
				mcp.Description("What prompted this episode, e.g. 'modifying auth_handler.go'."),
			),
			mcp.WithString("affected_files",
				mcp.Description("JSON array of file paths involved, e.g. '[\"cmd/server/main.go\"]'."),
			),
			mcp.WithString("affected_nodes",
				mcp.Description("JSON array of graph node IDs involved (from find_entity or get_context)."),
			),
			mcp.WithString("tags",
				mcp.Description("JSON array of tags for filtering, e.g. '[\"auth\",\"breaking\"]'."),
			),
			mcp.WithString("project_id",
				mcp.Description("Repo context (leave empty to use current project)."),
			),
		),
		s.handleRemember,
	)

	// recall
	s.mcp.AddTool(
		mcp.NewTool(
			"recall",
			mcp.WithDescription(
				"Searches episodic memory using FTS5 BM25 for episodes matching the query. "+
					"Returns the top-N most relevant episodes (best match first). "+
					"Also surfaces any dynamic_rules that were derived from similar past failures. "+
					"Use this before starting work on a component you haven't touched recently.",
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Natural language search query, e.g. 'auth handler redirect loop'."),
			),
			mcp.WithString("project_id",
				mcp.Description("Filter to episodes for this project (empty = all projects)."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Filter to episodes recorded by this agent (empty = all agents)."),
			),
			mcp.WithString("episode_type",
				mcp.Description("Filter by type: decision, failure, pattern, rule_proposal (empty = all)."),
			),
			mcp.WithString("outcome_filter",
				mcp.Description("Filter by outcome: success, failure, partial, unknown (empty = all)."),
			),
		),
		s.handleRecall,
	)

	// get_episodes
	s.mcp.AddTool(
		mcp.NewTool(
			"get_episodes",
			mcp.WithDescription(
				"Lists recent episodes without search, ordered by creation time (newest first). "+
					"Use recall() for semantic search. Use this to browse recent decisions or failures.",
			),
			mcp.WithString("project_id",
				mcp.Description("Filter to this project (empty = all)."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Filter to this agent (empty = all)."),
			),
			mcp.WithString("episode_type",
				mcp.Description("Filter by type: decision, failure, pattern, rule_proposal (empty = all)."),
			),
			mcp.WithString("tags",
				mcp.Description("Comma-separated tags to filter by, e.g. 'auth,breaking'."),
			),
		),
		s.handleGetEpisodes,
	)

	// check_plan_safety
	s.mcp.AddTool(
		mcp.NewTool(
			"check_plan_safety",
			mcp.WithDescription(
				"Searches failure episodes for the closest match to the proposed plan (Reactive Interjection). "+
					"Returns a Recovery Packet if a similar past failure is found — the agent decides relevance. "+
					"Non-blocking: returns 'clear' if no failures recorded yet or on timeout. "+
					"Call this BEFORE validate_plan() when touching sensitive components.",
			),
			mcp.WithString("plan_description",
				mcp.Required(),
				mcp.Description("Natural language description of what you plan to do, e.g. 'modify auth_provider.dart login flow without fallback'."),
			),
			mcp.WithString("agent_id",
				mcp.Description("Self-declared agent identifier (used to record the interjection event)."),
			),
			mcp.WithString("project_id",
				mcp.Description("Repo context for scoping the failure search (empty = all projects)."),
			),
		),
		s.handleCheckPlanSafety,
	)

	// get_rule_candidates
	s.mcp.AddTool(
		mcp.NewTool(
			"get_rule_candidates",
			mcp.WithDescription(
				"Returns failure episodes that have appeared ≥N times and have not yet been promoted to a dynamic_rule. "+
					"Use this to close the feedback loop: review candidates, call upsert_rule() to enforce the pattern, "+
					"then call mark_episode_promoted() to mark the episode as promoted.",
			),
		),
		s.handleGetRuleCandidates,
	)

	// get_adrs
	s.mcp.AddTool(
		mcp.NewTool(
			"get_adrs",
			mcp.WithDescription(
				"Returns Architectural Decision Records (ADRs) from the brain. "+
					"ADRs are persistent cold-memory entries for significant design choices. "+
					"When a file param is provided, returns only accepted ADRs whose linked_files patterns match. "+
					"Requires brain.url to be configured in synapses.json.",
			),
			mcp.WithString("file",
				mcp.Description("Optional file path suffix to filter ADRs by linked_files patterns. Returns only accepted ADRs that match."),
			),
		),
		s.handleGetADRs,
	)

}

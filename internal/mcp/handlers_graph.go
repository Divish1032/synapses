package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/store"
)

// handleGetProjectIdentity returns the compact architectural summary,
// enriched with federation status and workflow guidance.
func (s *Server) handleGetProjectIdentity(
	_ context.Context,
	_ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	identity := s.graph.ProjectIdentity()

	// Enrich with federation status (absorbed from get_federation_status).
	// CrossRepoCalls iterates the internal graph edge map directly, avoiding
	// the ~500 KB slice allocation that AllEdges() would produce on a large graph.
	primaryRepoID := s.graph.RepoID()
	crossCallCount, linkedRepos := s.graph.CrossRepoCalls(primaryRepoID)

	// Build the enriched result as a map so we can add fields.
	out := map[string]interface{}{
		"identity": identity,
		"federation": map[string]interface{}{
			"is_federated":        len(linkedRepos) > 0,
			"linked_repos":        linkedRepos,
			"cross_project_edges": crossCallCount,
		},
		"workflow_hints": []string{
			"1. session_init → single call to get pending tasks, project identity, and working state",
			"2. validate_plan → check proposed changes against architectural rules",
			"3. get_context → explore entity structure (callees, callers, annotations)",
			"4. annotate_node → leave findings for other agents",
			"5. update_task → mark work done as you go",
		},
	}
	// Autosubscribe: surface detected tech stack (populated by cmdStart after indexing).
	if s.techStack != nil {
		out["tech_stack"] = s.techStack
	}
	return jsonResult(out)
}

// handleFindEntity returns all nodes whose name matches the query string.
// Default format is "compact" (one line per match: "Name · type · file:line").
// Pass format="json" for the full structured response.
func (s *Server) handleFindEntity(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	start := time.Now()

	query, ok := req.GetArguments()["query"].(string)
	if !ok || query == "" {
		return mcp.NewToolResultError("query is required (e.g., 'AuthService', 'handleLogin')"), nil
	}
	format, _ := req.GetArguments()["format"].(string)
	if format == "" {
		format = "compact"
	}
	// Optional: search sibling projects when projects= is specified.
	projectsRaw, _ := req.GetArguments()["projects"].(string)

	// Exact match first, then substring.
	nodes := s.graph.FindByName(query)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPatternLimit(query, 50)
	}
	// Dotted method name fallback: "Store.Close" → search "Close", filter by "Store".
	// Go method nodes are stored by their short name (e.g. "Close") without the
	// receiver type prefix, so "Store.Close" matches nothing via substring.
	if len(nodes) == 0 && strings.Contains(query, ".") {
		parts := strings.SplitN(query, ".", 2)
		prefix, method := strings.ToLower(parts[0]), parts[1]
		candidates := s.graph.FindByName(method)
		if len(candidates) == 0 {
			candidates = s.graph.FindByPatternLimit(method, 50)
		}
		for _, n := range candidates {
			if strings.Contains(strings.ToLower(string(n.ID)), prefix) ||
				strings.Contains(strings.ToLower(n.File), prefix) {
				nodes = append(nodes, n)
			}
		}
	}

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	type entityMatch struct {
		ID        graph.NodeID   `json:"id"`
		Name      string         `json:"name"`
		Type      graph.NodeType `json:"type"`
		File      string         `json:"file"`
		Line      int            `json:"line"`
		Doc       string         `json:"doc,omitempty"`
		Signature string         `json:"signature,omitempty"`
		Callers   int            `json:"callers,omitempty"`
		Callees   int            `json:"callees,omitempty"`
	}
	results := make([]entityMatch, 0, len(nodes))
	for _, n := range nodes {
		file := n.File
		if prefix != "" {
			file = strings.TrimPrefix(file, prefix)
		}
		m := entityMatch{
			ID:   n.ID,
			Name: n.Name,
			Type: n.Type,
			File: file,
			Line: n.Line,
		}
		if n.Metadata != nil {
			m.Doc = n.Metadata["doc"]
			m.Signature = n.Metadata["signature"]
		}
		m.Callers = s.graph.Fanin(n.ID)
		m.Callees = s.graph.Fanout(n.ID)
		results = append(results, m)
	}

	// Sort results: implementation files before test files, then by path depth
	// (shorter = closer to root). This ensures the authoritative definition
	// appears first when both a_test.go and a.go define the same function name.
	sort.Slice(results, func(i, j int) bool {
		ti := isTestFile(results[i].File)
		tj := isTestFile(results[j].File)
		if ti != tj {
			return !ti // non-test wins
		}
		// Same test-ness: prefer shorter file path (closer to project root).
		if len(results[i].File) != len(results[j].File) {
			return len(results[i].File) < len(results[j].File)
		}
		return results[i].File < results[j].File
	})

	// Federation search: when projects= is specified and federation is configured,
	// search sibling stores for matching entities. Results are appended with
	// an [alias] prefix to distinguish them from local matches.
	var fedResults []federation.FederatedSearchResult
	if projectsRaw != "" && s.federationResolver != nil {
		var aliases []string
		if projectsRaw != "*" {
			for _, a := range strings.Split(projectsRaw, ",") {
				if a = strings.TrimSpace(a); a != "" {
					aliases = append(aliases, a)
				}
			}
		}
		fedCtx, fedCancel := context.WithTimeout(ctx, 2*time.Second)
		fedResults = s.federationResolver.FindEntities(fedCtx, query, aliases, 20)
		fedCancel()
	}

	if pc := s.getPulseClient(); pc != nil {
		agentID, _ := req.GetArguments()["agent_id"].(string)
		if agentID == "" {
			agentID = s.getLastAgent()
		}
		pulseSessID := s.getSynapseSessionID(SessionIDFromContext(ctx))
		count := len(results)
		durationMs := time.Since(start).Milliseconds()
		s.goBackground(func() {
			pc.RecordSearchEvent(pulse.SearchEvent{
				AgentID:     agentID,
				ProjectID:   s.projectID,
				Query:       query,
				Mode:        "exact",
				ResultCount: count,
				DurationMs:  durationMs,
				SessionID:   pulseSessID,
			})
		})
	}

	if format == "compact" {
		var sb strings.Builder
		if len(results) == 0 && len(fedResults) == 0 {
			sb.WriteString(fmt.Sprintf("No matches for %q.\nHint: try search(query=%q, mode=\"semantic\") for concept-based lookup, or get_file_context(file=\"...\") for a specific file.", query, query))
			return mcp.NewToolResultText(sb.String()), nil
		}
		localCount := len(results)
		fedCount := 0
		for _, fr := range fedResults {
			fedCount += len(fr.Results)
		}
		fmt.Fprintf(&sb, "%d match(es) for %q:\n", localCount+fedCount, query)
		for _, r := range results {
			testMark := ""
			if isTestFile(r.File) {
				testMark = " (test)"
			}
			if r.Callers > 0 || r.Callees > 0 {
				fmt.Fprintf(&sb, "  [%s] %s · %s:%d%s · %d callers, %d callees\n", r.Name, r.Type, r.File, r.Line, testMark, r.Callers, r.Callees)
			} else {
				fmt.Fprintf(&sb, "  [%s] %s · %s:%d%s\n", r.Name, r.Type, r.File, r.Line, testMark)
			}
		}
		// Federation results (compact format).
		for _, fr := range fedResults {
			for _, r := range fr.Results {
				fmt.Fprintf(&sb, "  [%s] [%s] %s\n", r.Name, fr.Alias, r.ID)
			}
		}
		// Context-aware footer: single match → exact call; multiple → disambiguation hint.
		totalResults := localCount + fedCount
		if totalResults == 1 && localCount == 1 {
			fmt.Fprintf(&sb, "Call get_context(entity=%q) to explore.", results[0].Name)
		} else {
			sb.WriteString("Call get_context(entity=\"Name\", file=\"path/suffix\") to pin to a specific result.")
		}
		return mcp.NewToolResultText(sb.String()), nil
	}

	result := map[string]interface{}{
		"query":   query,
		"count":   len(results),
		"matches": results,
	}
	if len(fedResults) > 0 {
		result["federated"] = fedResults
	}
	if len(results) == 0 && len(fedResults) == 0 {
		result["hint"] = "No exact or substring match. Try search(mode=semantic) for concept-based lookup, or check get_file_context for a specific file."
	}
	return jsonResult(result)
}

type toolCatalogEntry struct {
	Name        string
	Category    string
	Description string
	Keywords    []string
	Example     string
	// Pre-tokenized at init time to avoid repeated string ops per discover_tools call.
	descWords []string // lowercase alphabetic tokens from Description
	nameWords []string // Name split on underscores
}

// toolCatalog is the static catalog of all major Synapses tools, grouped by
// category and annotated with keywords for lightweight keyword matching.
// See handleDiscoverTools for usage. Indexed as NodeVariable (FIX-PARSER-2).
var toolCatalog = []toolCatalogEntry{
	// Session
	{Name: "session_init", Category: "session", Description: "Single-call session bootstrap", Keywords: []string{"start", "begin", "init", "session", "bootstrap"}, Example: `session_init(agent_id="my-agent")`},

	// Code exploration
	{Name: "explain_codebase", Category: "exploration", Description: "First-5-minutes orientation: entry points, key types, patterns, packages, tech stack", Keywords: []string{"explain", "codebase", "orientation", "overview", "introduce", "what is", "architecture", "summary", "new", "unfamiliar", "onboard"}, Example: `explain_codebase()`},
	{Name: "get_repo_map", Category: "exploration", Description: "Navigable package+entity map grouped by architectural layer", Keywords: []string{"repo", "map", "packages", "layout", "navigate", "overview", "structure", "where", "layers"}, Example: `get_repo_map(detail="compact")`},
	{Name: "get_context", Category: "exploration", Description: "Relevance-ranked subgraph around an entity", Keywords: []string{"context", "understand", "entity", "function", "struct", "interface", "subgraph", "explore", "code", "definition"}, Example: `get_context(entity="AuthService")`},
	{Name: "find_entity", Category: "exploration", Description: "Locate nodes by name or substring", Keywords: []string{"find", "search", "locate", "entity", "name", "symbol", "discover", "where", "defined", "definition", "which", "contains", "method", "function", "class", "type", "has"}, Example: `find_entity(query="Auth")`},
	{Name: "get_file_context", Category: "exploration", Description: "All entities in a file", Keywords: []string{"file", "entities", "overview", "list", "defined"}, Example: `get_file_context(file="internal/store/tasks.go")`},
	{Name: "search", Category: "exploration", Description: "Keyword/fulltext search across entities", Keywords: []string{"search", "keyword", "concept", "fulltext", "semantic", "grep"}, Example: `search(query="rate limiting", mode="fulltext")`},
	{Name: "get_call_chain", Category: "exploration", Description: "Shortest call path between two entities", Keywords: []string{"call", "chain", "path", "trace", "reach", "how"}, Example: `get_call_chain(from="Handler", to="Repository")`},
	{Name: "get_impact", Category: "exploration", Description: "Blast-radius analysis of what breaks if entity changes", Keywords: []string{"impact", "blast", "radius", "breaks", "change", "depends", "affected", "callers", "usage", "uses", "downstream", "refactor", "safe", "remove", "delete", "who", "using", "touching"}, Example: `get_impact(symbol="CarveEgoGraph")`},

	// Graph metadata
	{Name: "get_edge_types", Category: "graph", Description: "Semantic catalog of all graph edge types: BFS weight, domain tag (code/docs/infra/api/knowledge), meaning", Keywords: []string{"edge", "types", "catalog", "bfs", "weights", "traversal", "domain", "semantic", "graph", "edges", "relationship", "relationships", "explained"}, Example: `get_edge_types(format="compact")`},

	// Architecture
	{Name: "validate_plan", Category: "architecture", Description: "Check changes against architectural rules", Keywords: []string{"validate", "plan", "check", "rules", "architecture", "violations", "before"}, Example: `validate_plan(changes=[{"file":"auth.go","adds_call_to":"DB"}])`},
	{Name: "verify_implementation", Category: "architecture", Description: "Post-write check: verify written files against rules and task expectations", Keywords: []string{"verify", "implementation", "after", "written", "check", "post", "validate", "confirm"}, Example: `verify_implementation(files_written=["internal/auth/service.go"])`},
	{Name: "get_violations", Category: "architecture", Description: "List current architectural violations", Keywords: []string{"violations", "rules", "broken", "forbidden", "architecture"}, Example: `get_violations()`},
	{Name: "upsert_rule", Category: "architecture", Description: "Create or update an architectural constraint", Keywords: []string{"rule", "create", "constraint", "forbid", "enforce", "pattern", "add", "ban", "prevent", "restrict", "policy", "architectural", "ensure", "access", "never", "allow", "disallow"}, Example: `upsert_rule(rule_id="no-db-in-handler", description="...", severity="error")`},

	// Task management
	{Name: "create_plan", Category: "tasks", Description: "Save a plan with tasks for future sessions", Keywords: []string{"plan", "create", "tasks", "save", "work", "implement"}, Example: `create_plan(title="v1.1 improvements", tasks=[...])`},
	{Name: "get_pending_tasks", Category: "tasks", Description: "List pending/in-progress tasks (suggest_next=true for recommendation)", Keywords: []string{"pending", "tasks", "todo", "remaining", "work", "resume", "my", "assigned"}, Example: `get_pending_tasks(suggest_next=true)`},
	{Name: "update_task", Category: "tasks", Description: "Mark task done or add notes", Keywords: []string{"update", "task", "done", "complete", "status", "notes"}, Example: `update_task(id="...", status="done")`},
	{Name: "save_session_state", Category: "tasks", Description: "Save progress for session resumption", Keywords: []string{"save", "session", "state", "progress", "resume", "checkpoint"}, Example: `save_session_state(task_id="...", completed_steps=[...])`},
	{Name: "get_session_state", Category: "tasks", Description: "Resume from saved session state", Keywords: []string{"get", "session", "state", "resume", "restore"}, Example: `get_session_state(task_id="...")`},

	// Coordination
	{Name: "get_agents", Category: "coordination", Description: "List all agents in this repository", Keywords: []string{"agents", "who", "list", "working", "active"}, Example: `get_agents()`},

	// Memory
	{Name: "remember", Category: "memory", Description: "Record a decision or failure as an episode. Use anchor_nodes to bind codebase-derived beliefs to graph nodes for auto-invalidation.", Keywords: []string{"remember", "record", "episode", "decision", "failure", "learn", "anchor", "bind", "memory", "belief"}, Example: `remember(agent_id="...", decision="...", episode_type="failure", anchor_nodes='["repo::file.go::Func"]')`},
	{Name: "recall", Category: "memory", Description: "Search or browse episodic memory (empty query=chronological, query=FTS5 search)", Keywords: []string{"recall", "remember", "past", "history", "episode", "memory", "similar", "browse"}, Example: `recall(query="auth handler redirect loop")`},
	{Name: "check_plan_safety", Category: "memory", Description: "Check if similar plans failed before", Keywords: []string{"safety", "check", "failed", "before", "similar", "risk", "interjection"}, Example: `check_plan_safety(plan_description="modify auth login flow")`},

	// Messaging
	{Name: "send_message", Category: "messaging", Description: "Send message to another agent", Keywords: []string{"send", "message", "notify", "tell", "broadcast", "communicate"}, Example: `send_message(from_agent="...", topic="api_changed", payload="{...}")`},
	{Name: "get_messages", Category: "messaging", Description: "Retrieve messages + batch-ack via mark_read_ids", Keywords: []string{"messages", "inbox", "unread", "received", "poll", "mark", "read"}, Example: `get_messages(agent_id="...", mark_read_ids=["id1"])`},

	// Web / Doc Cache
	{Name: "web_annotate", Category: "web", Description: "Persist web findings to a code entity (survives across sessions)", Keywords: []string{"annotate", "web", "save", "persist", "findings", "research"}, Example: `web_annotate(node_id="...", note="...", hits=[...])`},
	{Name: "lookup_docs", Category: "web", Description: "Look up version-pinned package docs or cache a URL (cross-session)", Keywords: []string{"docs", "documentation", "api", "reference", "lookup", "package", "cache"}, Example: `lookup_docs(package="github.com/mark3labs/mcp-go")`},

	// Intent-based
	{Name: "prepare_context", Category: "meta", Description: "Intent-based context assembly (replaces multi-tool chains)", Keywords: []string{"prepare", "intent", "modify", "understand", "review", "debug", "add", "plan", "context"}, Example: `prepare_context(intent="modify", target="AuthService")`},
	{Name: "plan_context", Category: "meta", Description: "Compound pre-implementation check: safety + validation + scope in one call", Keywords: []string{"plan", "implement", "check", "safety", "validate", "before", "compound"}, Example: `plan_context(target="AuthService", changes=[{"file":"auth.go","adds_call_to":"DB"}])`},

	// Events
	{Name: "get_events", Category: "coordination", Description: "Recent events (file changes, task updates, annotations)", Keywords: []string{"events", "recent", "changes", "updates", "activity", "poll"}, Example: `get_events(since_seq=0)`},

	// ADRs
	{Name: "upsert_adr", Category: "architecture", Description: "Create/update an Architectural Decision Record", Keywords: []string{"adr", "architecture", "decision", "record", "create"}, Example: `upsert_adr(id="adr-001", title="No CGo", decision="...")`},
	{Name: "get_adrs", Category: "architecture", Description: "List Architectural Decision Records", Keywords: []string{"adr", "adrs", "architecture", "decisions", "records", "list"}, Example: `get_adrs()`},
}

// ── Workflow Recipes for discover_tools (GAP-5: Navigator short-term) ──────

// workflowStep describes one step in a multi-tool workflow recipe.
type workflowStep struct {
	Tool       string `json:"tool"`
	ArgsHint   string `json:"args"`        // template with {placeholders}
	Expects    string `json:"expects"`     // what this step returns
	UsesOutput string `json:"uses_output"` // what from the previous step feeds into this
}

// workflowRecipe is a pre-built tool sequence for a common intent.
// Returned by discover_tools when the query matches the intent keywords,
// so agents don't have to reason about tool chaining from scratch.
type workflowRecipe struct {
	ID          string         `json:"id"`
	Intent      string         `json:"intent"`
	Keywords    []string       `json:"-"`
	Steps       []workflowStep `json:"steps"`
	intentWords []string       // lowercase alphabetic tokens from Intent, pre-computed at init
}

var workflowRecipes = []workflowRecipe{
	{
		ID:       "understand_entity",
		Intent:   "Understand how a function, struct, or module works",
		Keywords: []string{"understand", "explore", "what", "how", "entity", "function", "struct", "works", "does"},
		Steps: []workflowStep{
			{Tool: "find_entity", ArgsHint: `query="{name}"`, Expects: "List of matching nodes with name, type, file, line. Pick the best match.", UsesOutput: ""},
			{Tool: "prepare_context", ArgsHint: `intent="understand", target="{name}", file="{file}"`, Expects: "Subgraph: root entity, callers, callees, annotations, recent_changes (git commits), brain enrichment.", UsesOutput: "Use exact name and file from find_entity results"},
			{Tool: "get_call_chain", ArgsHint: `from="{entity}", to="{callee}"`, Expects: "Step-by-step call path between two entities.", UsesOutput: "Optional: pick an interesting callee from prepare_context callees list to trace deeper"},
		},
	},
	{
		ID:       "implement_change",
		Intent:   "Safely implement a code change across files",
		Keywords: []string{"implement", "modify", "change", "edit", "write", "code", "add", "feature", "refactor"},
		Steps: []workflowStep{
			{Tool: "prepare_context", ArgsHint: `intent="modify", target="{target}"`, Expects: "Safe-edit briefing: current structure, callers/callees, annotations, applicable rules, and blast radius.", UsesOutput: ""},
			{Tool: "plan_context", ArgsHint: `target="{target}", changes=[{"file":"...","adds_call_to":"..."}]`, Expects: "verdict: clear|warnings|violations|blocked + safety check + scope assessment.", UsesOutput: "Use the entity's file and planned dependencies from prepare_context"},
			{Tool: "verify_implementation", ArgsHint: `files_written=["file1.go","file2.go"]`, Expects: "pass|violations_found|pending_indexing + per-file entity counts and violations.", UsesOutput: "After writing code: verify the implementation matches expectations"},
			{Tool: "update_task", ArgsHint: `id="...", status="done"`, Expects: "Task marked complete.", UsesOutput: "After verify passes: mark the task done"},
		},
	},
	{
		ID:       "debug_issue",
		Intent:   "Debug a bug — find the source, trace the call path, assess blast radius",
		Keywords: []string{"debug", "bug", "fix", "broken", "error", "issue", "trace", "wrong", "fails"},
		Steps: []workflowStep{
			{Tool: "search", ArgsHint: `query="{symptom}", mode="semantic"`, Expects: "Matching entities ranked by relevance.", UsesOutput: ""},
			{Tool: "prepare_context", ArgsHint: `intent="debug", target="{suspect}"`, Expects: "Call-path trace around the suspected entity + recent_changes (who modified it last).", UsesOutput: "Pick the most relevant entity from search results"},
			{Tool: "get_call_chain", ArgsHint: `from="{entrypoint}", to="{suspect}"`, Expects: "How the entry point reaches the buggy code.", UsesOutput: "Use the caller that triggers the bug as 'from'"},
			{Tool: "get_impact", ArgsHint: `symbol="{suspect}"`, Expects: "Reverse-BFS: everything that depends on this entity.", UsesOutput: "Assess blast radius before fixing"},
		},
	},
	{
		ID:       "resume_work",
		Intent:   "Resume a previous session's work where you left off",
		Keywords: []string{"resume", "continue", "pick", "left", "session", "previous", "start", "pending"},
		Steps: []workflowStep{
			{Tool: "session_init", ArgsHint: `agent_id="..."`, Expects: "pending_tasks (tasks key absent when empty — check summary), project_identity, working_state, scale_guidance.", UsesOutput: ""},
			{Tool: "get_pending_tasks", ArgsHint: `suggest_next=true`, Expects: "Tasks list + suggested_next (first unblocked task).", UsesOutput: "Use suggested_next to decide what to work on"},
			{Tool: "get_session_state", ArgsHint: `task_id="{suggested_task_id}"`, Expects: "Saved progress: completed_steps, current_step, context_snapshot.", UsesOutput: "Use task ID from suggested_next"},
			{Tool: "prepare_context", ArgsHint: `intent="understand", target="{from_session_state}", task_id="{task_id}"`, Expects: "Fresh context with task-boosted relevance.", UsesOutput: "Use entity from session state's context_snapshot"},
		},
	},
	{
		ID:       "check_impact",
		Intent:   "Assess what breaks if an entity is changed or removed",
		Keywords: []string{"impact", "blast", "radius", "breaks", "change", "refactor", "remove", "depends", "safe"},
		Steps: []workflowStep{
			{Tool: "get_impact", ArgsHint: `symbol="{entity}"`, Expects: "Reverse-BFS: all dependents and their distance from the entity.", UsesOutput: ""},
			{Tool: "validate_plan", ArgsHint: `changes=[{"file":"{entity_file}","adds_call_to":"..."}], check_safety=true`, Expects: "violations + safety_check from episodic memory.", UsesOutput: "Use affected files from get_impact to build your changes array"},
		},
	},
	{
		ID:       "review_architecture",
		Intent:   "Review and enforce architectural rules and constraints",
		Keywords: []string{"architecture", "rules", "violations", "enforce", "review", "constraints", "quality"},
		Steps: []workflowStep{
			{Tool: "get_violations", ArgsHint: ``, Expects: "List of all current rule violations with severity, suggested_fix.", UsesOutput: ""},
			{Tool: "prepare_context", ArgsHint: `intent="review", target="{violating_entity}"`, Expects: "Quality/risk context around the violating entity to understand why the violation exists.", UsesOutput: "Use from_node or to_node from violations list"},
			{Tool: "upsert_rule", ArgsHint: `rule_id="...", description="...", severity="error"`, Expects: "Rule created/updated.", UsesOutput: "Only if you need to add new constraints"},
		},
	},
	{
		ID:       "search_concept",
		Intent:   "Find code related to a concept or feature area",
		Keywords: []string{"find", "search", "concept", "feature", "where", "which", "related", "handles", "about"},
		Steps: []workflowStep{
			{Tool: "search", ArgsHint: `query="{concept}", mode="semantic"`, Expects: "Entities ranked by relevance. search_mode shows if vector or FTS5 was used.", UsesOutput: ""},
			{Tool: "prepare_context", ArgsHint: `intent="understand", target="{top_result}"`, Expects: "Full context around the best match: structure, callers, callees, annotations.", UsesOutput: "Use the top result's name and file"},
		},
	},
}

// splitAlpha tokenizes s into lowercase alphabetic words (same split rule as
// in handleDiscoverTools description matching). Used by init() to pre-compute
// descWords and intentWords so discover_tools avoids repeated string ops.
func splitAlpha(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r < 'a' || r > 'z' })
}

func init() {
	// Pre-tokenize tool catalog entries so handleDiscoverTools doesn't repeat
	// strings.FieldsFunc + strings.ToLower on every query.
	for i := range toolCatalog {
		toolCatalog[i].descWords = splitAlpha(toolCatalog[i].Description)
		toolCatalog[i].nameWords = strings.Split(toolCatalog[i].Name, "_")
	}
	// Pre-tokenize workflow recipe intents for the same reason.
	for i := range workflowRecipes {
		workflowRecipes[i].intentWords = splitAlpha(workflowRecipes[i].Intent)
	}
}

// dotProduct returns the dot product of two pre-normalized float32 vectors.
// For unit-length vectors this equals cosine similarity.
// Returns 0 for empty or mismatched-length inputs.
func dotProduct(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// handleDiscoverTools ranks tools for a query. When the memory embedder is
// configured and tool embeddings are ready, ranking uses cosine similarity
// (semantic path). Otherwise it falls back to keyword overlap scoring.
func (s *Server) handleDiscoverTools(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := strings.ToLower(stringArg(req, "query"))
	debug, _ := req.GetArguments()["debug"].(bool)

	// Empty query: return categorized overview of all tools.
	if query == "" {
		categories := make(map[string][]map[string]string)
		for _, t := range toolCatalog {
			// In knowledge mode, only show knowledge-mode tools.
			if s.knowledgeMode && !knowledgeTools[t.Name] {
				continue
			}
			entry := map[string]string{
				"name":        t.Name,
				"description": t.Description,
			}
			if hiddenTools[t.Name] {
				entry["status"] = "hidden — not in tools/list, still callable"
			}
			categories[t.Category] = append(categories[t.Category], entry)
		}
		resp := map[string]interface{}{
			"hint":       "Pass a query to get targeted results, e.g. discover_tools(query=\"check what calls this function\")",
			"categories": categories,
		}
		if s.knowledgeMode {
			resp["mode"] = "knowledge"
			resp["note"] = "Running in knowledge mode — only memory, task, and messaging tools are available."
		}
		return jsonResult(resp)
	}

	// Tokenize query, removing stop words.
	// Short common words (≤2 chars) and frequent English filler ("the", "and",
	// "for", etc.) match as substrings inside longer keywords — e.g. "is" inside
	// "episodes", "if" inside "verify" — causing severe score inflation on wrong
	// tools. Filtering them before scoring ensures only intent-bearing terms count.
	stopWords := map[string]bool{
		// 1-char
		"a": true, "i": true,
		// 2-char
		"an": true, "as": true, "at": true, "be": true, "by": true,
		"do": true, "if": true, "in": true, "is": true, "it": true,
		"no": true, "of": true, "on": true, "or": true, "to": true,
		// 3-char (common filler — kept short to preserve intent words like "ban", "add", "api")
		"and": true, "are": true, "but": true, "can": true, "did": true,
		"for": true, "has": true, "its": true, "not": true, "the": true,
		"was": true,
	}
	var queryWords []string
	for _, w := range strings.Fields(query) {
		if !stopWords[w] {
			queryWords = append(queryWords, w)
		}
	}

	// kwMatch returns true when kw and qw are the same word or share a stem prefix.
	// Whole-word matching prevents short query fragments ("add", "ban") from matching
	// unrelated longer words as substrings — the core IMP-EVAL-5 fix.
	// Rules:
	//   - exact: kw == qw
	//   - qw is prefix of kw (len(qw)≥4): "call" → "callers", "arch" → "architectural"
	//   - kw is prefix of qw (len(kw)≥4): "impact" → "impacts", "rule" → "rules"
	// The 4-char minimum prevents 3-char tokens like "add", "ban", "api" from
	// matching "added", "banning", "application" via prefix.
	kwMatch := func(kw, qw string) bool {
		if kw == qw {
			return true
		}
		if len(qw) >= 4 && strings.HasPrefix(kw, qw) {
			return true
		}
		if len(kw) >= 4 && strings.HasPrefix(qw, kw) {
			return true
		}
		return false
	}

	// Score each tool by keyword overlap.
	type breakdown struct {
		KeywordsMatched []string `json:"keywords_matched"`
		NameHits        []string `json:"name_hits"`
		DescHits        []string `json:"desc_hits"`
	}
	type scored struct {
		entry     toolCatalogEntry
		score     int
		breakdown breakdown
	}
	var results []scored
	for _, tool := range toolCatalog {
		// In knowledge mode, only score knowledge-mode tools.
		if s.knowledgeMode && !knowledgeTools[tool.Name] {
			continue
		}
		score := 0
		var bd breakdown
		for _, qw := range queryWords {
			for _, kw := range tool.Keywords {
				if kwMatch(kw, qw) {
					score++
					if debug {
						bd.KeywordsMatched = append(bd.KeywordsMatched, kw)
					}
				}
			}
			// Also check tool name (tokenized on underscores) and description.
			// descWords and nameWords are pre-computed at init time.
			for _, nw := range tool.nameWords {
				if nw == qw {
					score += 2
					if debug {
						bd.NameHits = append(bd.NameHits, nw)
					}
					break
				}
			}
			for _, dw := range tool.descWords {
				if kwMatch(dw, qw) {
					score++
					if debug {
						bd.DescHits = append(bd.DescHits, dw)
					}
					break
				}
			}
		}
		if score > 0 || debug {
			results = append(results, scored{tool, score, bd})
		}
	}

	// Sort by score descending.
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })

	// In debug mode return all tools; otherwise cap at top 3.
	if !debug {
		limit := 3
		if len(results) < limit {
			limit = len(results)
		}
		results = results[:limit]
	}

	// Format output with status indicator (Phase 6: tool discoverability).
	type toolMatch struct {
		Name             string     `json:"name"`
		Category         string     `json:"category"`
		Description      string     `json:"description"`
		Example          string     `json:"example"`
		Score            int        `json:"score,omitempty"`
		SimilarityScore  float32    `json:"similarity_score,omitempty"`
		Status           string     `json:"status"`
		Breakdown        *breakdown `json:"breakdown,omitempty"`
	}

	// ── Semantic path ──────────────────────────────────────────────────────────
	// When tool embeddings are ready and the embedder is available, rank by
	// cosine similarity instead of keyword overlap. Falls back to keyword path
	// on any error (embed failure, nil embedder, embeddings not yet computed).
	s.toolEmbedsMu.RLock()
	toolEmbeds := s.toolEmbeds
	toolEmbedModel := s.toolEmbedModel
	embeddingsReady := len(toolEmbeds) == len(toolCatalog)
	s.toolEmbedsMu.RUnlock()

	// Model consistency check: query embeddings must come from the same model as
	// tool embeddings. A mismatch (possible during embedder hot-swap or model
	// upgrade) means vectors live in different spaces — cosine similarity would
	// be meaningless. Trigger background re-embedding so the next call works.
	if embeddingsReady && s.memoryEmbedder != nil && toolEmbedModel != s.memoryEmbedder.Model() {
		embeddingsReady = false
		// Re-embed tool catalog with the new model in background.
		s.wg.Add(1)
		embedder := s.memoryEmbedder
		go func() {
			defer s.wg.Done()
			bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			s.EmbedToolCatalog(bgCtx, embedder)
		}()
	}

	if embeddingsReady && s.memoryEmbedder != nil {
		embedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		queryVec, embedErr := s.memoryEmbedder.Embed(embedCtx, query)
		cancel()
		if embedErr == nil && len(queryVec) > 0 {
			queryVec = normalizeVec(queryVec)
			type semScored struct {
				idx   int
				score float32
			}
			semResults := make([]semScored, 0, len(toolCatalog))
			for i, tool := range toolCatalog {
				if s.knowledgeMode && !knowledgeTools[tool.Name] {
					continue
				}
				sim := dotProduct(queryVec, toolEmbeds[i])
				semResults = append(semResults, semScored{idx: i, score: sim})
			}
			sort.Slice(semResults, func(i, j int) bool { return semResults[i].score > semResults[j].score })

			// Drop tools with non-positive similarity — they are semantically
			// unrelated to the query (cosine ≤ 0 means orthogonal or opposite).
			// If nothing passes the threshold, fall through to the keyword path
			// rather than returning irrelevant results tagged as "semantic".
			filtered := semResults[:0]
			for _, r := range semResults {
				if r.score > 0 {
					filtered = append(filtered, r)
				}
			}
			if len(filtered) == 0 {
				// No positive-similarity tools — keyword path will do better.
				goto keywordPath
			}
			semResults = filtered

			limit := 3
			if debug || len(semResults) < limit {
				limit = len(semResults)
			}
			semResults = semResults[:limit]

			matches := make([]toolMatch, len(semResults))
			for i, r := range semResults {
				tool := toolCatalog[r.idx]
				status := "available — ready to call"
				if hiddenTools[tool.Name] {
					status = "hidden — not in tools/list, still callable"
				} else if coreTierTools[tool.Name] {
					status = "core — always available"
				} else if standardTierTools[tool.Name] {
					status = "standard — always available"
				}
				matches[i] = toolMatch{
					Name:            tool.Name,
					Category:        tool.Category,
					Description:     tool.Description,
					Example:         tool.Example,
					SimilarityScore: r.score,
					Status:          status,
				}
			}

			resp := map[string]interface{}{
				"query":       query,
				"matches":     matches,
				"search_mode": "semantic",
			}
			if len(matches) == 0 {
				resp["hint"] = "No matches. Try broader terms like 'explore', 'task', 'web', 'architecture'."
			}
			// Workflow recipe matching stays keyword-based — it uses rich intent
			// descriptions that benefit from literal keyword alignment.
			var bestWorkflow *workflowRecipe
			bestWfScore := 0
			for i := range workflowRecipes {
				wf := &workflowRecipes[i]
				score := 0
				for _, qw := range queryWords {
					for _, kw := range wf.Keywords {
						if kwMatch(kw, qw) {
							score++
						}
					}
					for _, dw := range wf.intentWords {
						if kwMatch(dw, qw) {
							score++
							break
						}
					}
				}
				if score > bestWfScore {
					bestWfScore = score
					bestWorkflow = wf
				}
			}
			if bestWorkflow != nil && bestWfScore > 0 {
				resp["recommended_workflow"] = bestWorkflow
			}
			if s.projectRegistry != nil {
				allowed := s.allowedProjectNames()
				if len(allowed) > 0 {
					resp["cross_project_hint"] = fmt.Sprintf(
						"Cross-project queries available for: %s. Add projects=\"*\" to recall, get_events, get_messages, or get_agents to query across them.",
						strings.Join(allowed, ", "),
					)
				}
			}
			return jsonResult(resp)
		}
		// Embed failed or no positive-similarity results — fall through to keyword path.
	}

keywordPath:
	// ── Keyword path (fallback) ────────────────────────────────────────────────

	matches := make([]toolMatch, len(results))
	for i, r := range results {
		status := "available — ready to call"
		if hiddenTools[r.entry.Name] {
			status = "hidden — not in tools/list, still callable"
		} else if coreTierTools[r.entry.Name] {
			status = "core — always available"
		} else if standardTierTools[r.entry.Name] {
			status = "standard — always available"
		}
		m := toolMatch{
			Name:        r.entry.Name,
			Category:    r.entry.Category,
			Description: r.entry.Description,
			Example:     r.entry.Example,
			Score:       r.score,
			Status:      status,
		}
		if debug {
			m.Breakdown = &r.breakdown
		}
		matches[i] = m
	}

	resp := map[string]interface{}{
		"query":       query,
		"matches":     matches,
		"search_mode": "keyword",
	}
	if len(matches) == 0 {
		resp["hint"] = "No matches. Try broader terms like 'explore', 'task', 'web', 'architecture'."
	}

	// GAP-5: Navigator short-term — match against workflow recipes and return
	// the best one as recommended_workflow so agents get a full tool sequence.
	var bestWorkflow *workflowRecipe
	bestWfScore := 0
	for i := range workflowRecipes {
		wf := &workflowRecipes[i]
		score := 0
		for _, qw := range queryWords {
			for _, kw := range wf.Keywords {
				if kwMatch(kw, qw) {
					score++
				}
			}
			for _, dw := range wf.intentWords {
				if kwMatch(dw, qw) {
					score++
					break
				}
			}
		}
		if score > bestWfScore {
			bestWfScore = score
			bestWorkflow = wf
		}
	}
	if bestWorkflow != nil && bestWfScore > 0 {
		resp["recommended_workflow"] = bestWorkflow
	}

	// When multiple projects are registered and ACL allows reading from them,
	// hint about cross-project queries. Only show ACL-allowed project names.
	if s.projectRegistry != nil {
		allowed := s.allowedProjectNames()
		if len(allowed) > 0 {
			resp["cross_project_hint"] = fmt.Sprintf(
				"Cross-project queries available for: %s. Add projects=\"*\" to recall, get_events, get_messages, or get_agents to query across them.",
				strings.Join(allowed, ", "),
			)
		}
	}

	// P8-3: emit discover_tools funnel event.
	if pc := s.getPulseClient(); pc != nil {
		wfCount := 0
		if bestWorkflow != nil && bestWfScore > 0 {
			wfCount = 1
		}
		pc.RecordSearchEvent(pulse.SearchEvent{
			Mode:             "discover",
			Query:            query,
			ResultCount:      len(matches),
			MatchedTools:     len(matches),
			MatchedWorkflows: wfCount,
			ProjectID:        s.projectID,
		})
	}

	return jsonResult(resp)
}

func (s *Server) handleGetFileContext(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	handlerStart := time.Now()
	filePath, ok := req.GetArguments()["file"].(string)
	if !ok || filePath == "" {
		return mcp.NewToolResultError("file is required (e.g., 'internal/auth/service.go')"), nil
	}

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	// Use indexed FindByFile for O(1) lookup instead of scanning all nodes.
	candidates := s.graph.FindByFile(filePath)
	var matches []*graph.Node
	for _, n := range candidates {
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage {
			continue
		}
		matches = append(matches, n)
	}

	if len(matches) == 0 {
		return jsonResult(map[string]interface{}{
			"error": fmt.Sprintf("no entities found for file: %q", filePath),
			"hint":  "Use a path suffix (e.g. 'store/tasks.go' not full absolute path). The file may not be indexed yet — try reindex.",
		})
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].File != matches[j].File {
			return matches[i].File < matches[j].File
		}
		return matches[i].Line < matches[j].Line
	})

	type fileEntity struct {
		Type     graph.NodeType    `json:"type"`
		Name     string            `json:"name"`
		Line     int               `json:"line"`
		Exported bool              `json:"exported"`
		Metadata map[string]string `json:"metadata,omitempty"`
	}

	// Check how many distinct files were matched.
	fileSet := make(map[string]struct{})
	for _, n := range matches {
		fileSet[n.File] = struct{}{}
	}

	fileTokenBudget := 4000
	// Sprint 11: apply model-based budget multiplier to the default budget.
	if mult := s.getSessionBudgetMultiplier(ctx); mult != 1.0 {
		fileTokenBudget = int(float64(fileTokenBudget) * mult)
	}
	if tb, ok := req.GetArguments()["token_budget"].(float64); ok && tb > 0 {
		fileTokenBudget = int(tb)
	}

	agentIDFC, _ := req.GetArguments()["agent_id"].(string)
	if agentIDFC == "" {
		agentIDFC = s.getLastAgent()
	}
	// R29: track repeated file-context fetches as a confusion signal, the same
	// way get_context tracks repeated entity fetches.
	if agentIDFC != "" && s.store != nil {
		s.trackContextCall(agentIDFC, "file:"+filePath)
	}

	if len(fileSet) == 1 {
		// Single file — keep existing flat format.
		out := make([]fileEntity, len(matches))
		for i, n := range matches {
			out[i] = fileEntity{Type: n.Type, Name: n.Name, Line: n.Line, Exported: n.Exported, Metadata: n.Metadata}
		}
		totalEntities := len(out)
		// IMP-EVAL-10: truncate to token budget by dropping highest-line entities first.
		// Proportional truncation: marshal once, compute keep ratio in O(n).
		truncated := false
		if fileTokenBudget > 0 {
			if raw, err := json.Marshal(out); err == nil && len(raw) > fileTokenBudget*4 {
				keep := len(out) * (fileTokenBudget * 4) / len(raw)
				if keep < 1 {
					keep = 1
				}
				out = out[:keep]
				truncated = true
			}
		}
		payload := map[string]interface{}{
			"file":     strings.TrimPrefix(matches[0].File, prefix),
			"package":  matches[0].Package,
			"count":    len(out),
			"entities": out,
		}
		if truncated {
			payload["truncated"] = true
			payload["total_entities"] = totalEntities
		}
		pulseSessID := s.getSynapseSessionID(SessionIDFromContext(ctx))
		s.emitFileContextDelivery(agentIDFC, filePath, matches, payload, time.Since(handlerStart).Milliseconds(), pulseSessID, truncated, totalEntities-len(out))
		return jsonResult(payload)
	}

	// Multiple files matched — group by file with attribution.
	byFile := make(map[string][]fileEntity)
	fileOrder := make([]string, 0, len(fileSet))
	for _, n := range matches {
		rel := strings.TrimPrefix(n.File, prefix)
		if _, seen := byFile[rel]; !seen {
			fileOrder = append(fileOrder, rel)
		}
		byFile[rel] = append(byFile[rel], fileEntity{Type: n.Type, Name: n.Name, Line: n.Line, Exported: n.Exported, Metadata: n.Metadata})
	}
	sort.Strings(fileOrder)
	multiPayload := map[string]interface{}{
		"files_matched":    len(fileSet),
		"total_count":      len(matches),
		"entities_by_file": byFile,
		"hint":             fmt.Sprintf("%d files named %q found. Use file= param with a longer path suffix to pin to one file.", len(fileSet), filePath),
	}
	pulseSessID := s.getSynapseSessionID(SessionIDFromContext(ctx))
	s.emitFileContextDelivery(agentIDFC, filePath, matches, multiPayload, time.Since(handlerStart).Milliseconds(), pulseSessID, false, 0)
	return jsonResult(multiPayload)
}

// handleSearch performs a keyword search across entity names and doc comments.
// Results are ranked: exact name > name prefix > name substring > doc match.
func (s *Server) handleSearch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	query, ok := req.GetArguments()["query"].(string)
	if !ok || query == "" {
		return mcp.NewToolResultError("query is required (e.g., 'auth caching', 'UserService login flow')"), nil
	}

	// R29: track repeated searches for the same query as a confusion signal.
	if agentIDSrch, _ := req.GetArguments()["agent_id"].(string); agentIDSrch != "" && s.store != nil {
		s.trackContextCall(agentIDSrch, "search:"+query)
	}

	// mode=fulltext (or legacy alias "semantic") delegates to FTS5 BM25 search.
	if mode := stringArg(req, "mode"); mode == "semantic" || mode == "fulltext" {
		return s.handleSemanticSearch(ctx, req)
	}

	start := time.Now()

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	lower := strings.ToLower(query)

	type hit struct {
		node  *graph.Node
		score int
	}
	var hits []hit
	const maxResults = 25
	highScoreCount := 0 // tracks hits with score >= 20 (exact/prefix)

	// O(N) scan with early termination: once we have maxResults exact/prefix
	// matches (score ≥ 20), lower-scored hits can never displace them, so we
	// stop scanning. On typical queries this terminates after a small fraction
	// of the graph is scanned.
	for _, n := range s.graph.AllNodes() {
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage {
			continue
		}
		nameLow := strings.ToLower(n.Name)
		score := 0
		switch {
		case nameLow == lower:
			score = 30
		case strings.HasPrefix(nameLow, lower):
			score = 20
		case strings.Contains(nameLow, lower):
			score = 10
		default:
			// Skip expensive file-path, doc, and multi-word checks if we
			// already have enough high-quality hits to fill the results.
			if highScoreCount >= maxResults {
				continue
			}
			// Score 8: file path suffix match — lets agents search by package name
			// (e.g. "watcher" matches all nodes in internal/watcher/*.go).
			fileLow := strings.ToLower(n.File)
			if strings.HasSuffix(fileLow, "/"+lower+".go") ||
				strings.Contains(fileLow, "/"+lower+"/") {
				score = 8
			} else if doc, ok := n.Metadata["doc"]; ok && strings.Contains(strings.ToLower(doc), lower) {
				score = 5
			}
		}
		// Multi-word AND query: each query word must appear in the name components
		// or doc comment. Handles stemmed/derived forms like "BFS carver" matching
		// "CarveEgoGraph" (query "carver" prefix-matches name component "carve").
		if score == 0 && highScoreCount < maxResults {
			words := strings.Fields(lower)
			if len(words) > 1 {
				nameWords := camelWords(n.Name)
				docLow := strings.ToLower(n.Metadata["doc"])
				matchCount := 0
				inNameCount := 0
				for _, qw := range words {
					// Check name components: exact match, or qw starts with component
					// (handles "carver"→"carve"), or component starts with qw.
					inName := false
					for _, nw := range nameWords {
						if nw == qw || strings.HasPrefix(qw, nw) || strings.HasPrefix(nw, qw) {
							inName = true
							break
						}
					}
					if inName {
						matchCount++
						inNameCount++
					} else if strings.Contains(docLow, qw) {
						matchCount++
					}
				}
				if matchCount == len(words) {
					if inNameCount > 0 {
						score = 6 // partial name + doc match
					} else {
						score = 3 // all words only in doc
					}
				}
			}
		}

		if score > 0 {
			hits = append(hits, hit{n, score})
			if score >= 20 {
				highScoreCount++
				// Early termination: enough exact/prefix matches found —
				// no lower-scored hit can displace these after sorting.
				if highScoreCount >= maxResults {
					// Continue scanning only for name-based matches (score ≥ 10)
					// which are cheap to compute. The switch-default branch above
					// skips expensive file/doc checks when highScoreCount >= maxResults.
				}
			}
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].node.Name < hits[j].node.Name
	})

	// Cap at 25 results to stay within token budget.
	if len(hits) > 25 {
		hits = hits[:25]
	}

	type result struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		File      string `json:"file"`
		Line      int    `json:"line"`
		Doc       string `json:"doc,omitempty"`
		Signature string `json:"signature,omitempty"`
	}
	results := make([]result, len(hits))
	for i, h := range hits {
		results[i] = result{
			Type:      string(h.node.Type),
			Name:      h.node.Name,
			File:      strings.TrimPrefix(h.node.File, prefix),
			Line:      h.node.Line,
			Doc:       h.node.Metadata["doc"],
			Signature: h.node.Metadata["signature"],
		}
	}

	if pc := s.getPulseClient(); pc != nil {
		agentID, _ := req.GetArguments()["agent_id"].(string)
		if agentID == "" {
			agentID = s.getLastAgent()
		}
		pulseSessID := s.getSynapseSessionID(SessionIDFromContext(ctx))
		count := len(results)
		durationMs := time.Since(start).Milliseconds()
		s.goBackground(func() {
			pc.RecordSearchEvent(pulse.SearchEvent{
				AgentID:     agentID,
				ProjectID:   s.projectID,
				Query:       query,
				Mode:        "exact",
				ResultCount: count,
				DurationMs:  durationMs,
				SessionID:   pulseSessID,
			})
		})
	}

	return jsonResult(map[string]interface{}{
		"query":   query,
		"count":   len(results),
		"results": results,
	})
}

// handleGetCallChain finds the shortest call path between two entities using
// BFS over CALLS edges. Answers "how does A reach B?"
func (s *Server) handleGetCallChain(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	fromName, ok1 := req.GetArguments()["from"].(string)
	toName, ok2 := req.GetArguments()["to"].(string)
	if !ok1 || !ok2 || fromName == "" || toName == "" {
		return mcp.NewToolResultError("from and to are required"), nil
	}

	resolve := func(name string) *graph.Node {
		nodes := s.graph.FindByName(name)
		if len(nodes) == 0 {
			nodes = s.graph.FindByPatternLimit(name, 50)
		}
		if len(nodes) == 0 {
			return nil
		}
		return pickBestNode(nodes, s.graph)
	}

	fromNode := resolve(fromName)
	if fromNode == nil {
		return jsonResult(map[string]interface{}{
			"error": fmt.Sprintf("entity not found: %q", fromName),
			"hint":  "Use find_entity or semantic_search to discover the correct entity name.",
		})
	}
	toNode := resolve(toName)
	if toNode == nil {
		return jsonResult(map[string]interface{}{
			"error": fmt.Sprintf("entity not found: %q", toName),
			"hint":  "Use find_entity or semantic_search to discover the correct entity name.",
		})
	}

	if fromNode.ID == toNode.ID {
		return jsonResult(map[string]interface{}{
			"found": true,
			"chain": []string{fromNode.Name},
		})
	}

	// BFS following CALLS + HANDLES edges (forward) and IMPLEMENTS edges (both
	// directions). HANDLES edges allow traversal through framework routing
	// registrations (R1): setupFn --CALLS--> routeNode --HANDLES--> handlerFn.
	// IMPLEMENTS edges are traversed bidirectionally so the search can cross
	// interface boundaries: struct → interface (forward) and interface → struct
	// (backward), enabling chains like: Caller → ConcreteType → Interface or
	// Caller → Interface → ConcreteImplementation.
	prev := map[graph.NodeID]graph.NodeID{fromNode.ID: ""}
	// viaImpl tracks which steps in the chain crossed an IMPLEMENTS boundary.
	viaImpl := make(map[graph.NodeID]bool)
	// viaHandles tracks which steps crossed a synthetic HANDLES boundary (R1).
	viaHandles := make(map[graph.NodeID]bool)
	// Single queue carries both the node ID and hop distance — no parallel slice needed.
	type bfsEntry struct {
		id  graph.NodeID
		hop int
	}
	const maxBFSHops = 30 // prevent full-graph traversal on dense graphs
	queue := []bfsEntry{{fromNode.ID, 0}}
	found := false
	// closestReachable tracks the deepest node reached (by hop count from root).
	// Used in the not-found response to show agents where the static graph ends.
	var closestReachableID graph.NodeID
	maxHop := 0

	for len(queue) > 0 && !found {
		curr := queue[0]
		queue = queue[1:]

		if curr.hop >= maxBFSHops {
			continue
		}

		if curr.hop > maxHop {
			maxHop = curr.hop
			closestReachableID = curr.id
		}

		// Forward edges: CALLS, HANDLES, and IMPLEMENTS (concrete → interface).
		for _, e := range s.graph.OutEdges(curr.id) {
			if e.Type != graph.EdgeCalls && e.Type != graph.EdgeImplements && e.Type != graph.EdgeHandles {
				continue
			}
			if _, visited := prev[e.To]; visited {
				continue
			}
			prev[e.To] = curr.id
			if e.Type == graph.EdgeImplements {
				viaImpl[e.To] = true
			}
			if e.Type == graph.EdgeHandles {
				viaHandles[e.To] = true
			}
			if e.To == toNode.ID {
				found = true
				break
			}
			queue = append(queue, bfsEntry{e.To, curr.hop + 1})
		}
		if found {
			break
		}

		// Backward IMPLEMENTS edges (interface → concrete struct).
		for _, e := range s.graph.InEdges(curr.id) {
			if e.Type != graph.EdgeImplements {
				continue
			}
			if _, visited := prev[e.From]; visited {
				continue
			}
			prev[e.From] = curr.id
			viaImpl[e.From] = true
			if e.From == toNode.ID {
				found = true
				break
			}
			queue = append(queue, bfsEntry{e.From, curr.hop + 1})
		}
	}

	if !found {
		// P7-9: emit search event for call chain BFS (not found).
		if pc := s.getPulseClient(); pc != nil {
			pc.RecordSearchEvent(pulse.SearchEvent{
				Mode: "call_chain", Query: fromName + " -> " + toName,
				ResultCount: 0, ProjectID: s.projectID,
			})
		}
		// Build a helpful explanation for why no path was found.
		fromPkg := topLevelPackage(fromNode.File)
		toPkg := topLevelPackage(toNode.File)
		var reason, hint string
		if fromPkg != toPkg && fromPkg != "" && toPkg != "" {
			// Different top-level packages — likely a cross-binary boundary.
			reason = fmt.Sprintf(
				"No direct CALLS path found. %q (%s) and %q (%s) are in different packages (%s vs %s). "+
					"Cross-binary calls (e.g. HTTP, gRPC, queue) are not captured as CALLS edges.",
				fromName, fromNode.File, toName, toNode.File, fromPkg, toPkg,
			)
			hint = "If these communicate via HTTP or another protocol, use get_context on each entity to understand their APIs, then trace the integration manually."
		} else {
			reason = fmt.Sprintf(
				"No direct CALLS path found between %q and %q. "+
					"They may be unrelated, or connected only at runtime (e.g. via interface dispatch, reflection, or dynamic config).",
				fromName, toName,
			)
			hint = "Use get_context on each entity to see their callers/callees, or get_impact to find what depends on them."
		}
		notFound := map[string]interface{}{
			"found":  false,
			"from":   map[string]interface{}{"name": fromName, "file": fromNode.File, "type": string(fromNode.Type)},
			"to":     map[string]interface{}{"name": toName, "file": toNode.File, "type": string(toNode.Type)},
			"reason": reason,
			"hint":   hint,
		}
		// R2: surface the deepest reachable node so agents know where the static
		// graph ends — especially useful for dynamic-dispatch gaps.
		if closestReachableID != "" && closestReachableID != fromNode.ID {
			if n := s.graph.GetNode(closestReachableID); n != nil {
				notFound["closest_reachable"] = map[string]interface{}{
					"name": n.Name,
					"file": strings.TrimPrefix(n.File, s.graph.Root()+"/"),
					"type": string(n.Type),
					"hops": maxHop,
				}
			}
		}
		return jsonResult(notFound)
	}

	// Reconstruct path.
	var chainIDs []graph.NodeID
	curr := toNode.ID
	for curr != "" {
		chainIDs = append([]graph.NodeID{curr}, chainIDs...)
		curr = prev[curr]
	}

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	type chainStep struct {
		Name string `json:"name"`
		Type string `json:"type"`
		File string `json:"file"`
		Line int    `json:"line"`
		Via  string `json:"via,omitempty"` // "implements" | "handles" when crossing a dispatch boundary
	}
	usedInterface := false
	usedHandles := false
	chain := make([]chainStep, 0, len(chainIDs))
	for _, id := range chainIDs {
		n := s.graph.GetNode(id)
		if n == nil {
			continue
		}
		step := chainStep{
			Name: n.Name,
			Type: string(n.Type),
			File: strings.TrimPrefix(n.File, prefix),
			Line: n.Line,
		}
		if viaImpl[id] {
			step.Via = "implements"
			usedInterface = true
		}
		if viaHandles[id] {
			step.Via = "handles" // R1: inferred framework routing edge
			usedHandles = true
		}
		chain = append(chain, step)
	}

	// P7-9: emit search event for call chain BFS (found).
	if pc := s.getPulseClient(); pc != nil {
		pc.RecordSearchEvent(pulse.SearchEvent{
			Mode: "call_chain", Query: fromName + " -> " + toName,
			ResultCount: len(chain) - 1, ProjectID: s.projectID,
		})
	}

	return jsonResult(map[string]interface{}{
		"found":         true,
		"hops":          len(chain) - 1,
		"via_interface": usedInterface,
		"via_handles":   usedHandles, // R1: true when path crossed a synthetic routing edge
		"chain":         chain,
	})
}

// isTestFile returns true for test files (_test.go, test_*.py, *_test.ts, etc.)
// so find_entity can rank implementation files above test files.
func (s *Server) handleSemanticSearch(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("semantic_search requires a persistent store (run 'synapses start' or 'synapses index' first)"), nil
	}

	query := stringArg(req, "query")
	if query == "" {
		return mcp.NewToolResultError("query is required (e.g., 'auth decisions', 'why we switched to OAuth')"), nil
	}

	limitRaw, _ := req.GetArguments()["limit"].(float64)
	limit := int(limitRaw)
	if limit <= 0 {
		limit = 20
	}

	// --- Vector path (when embedding_endpoint is configured) ---
	// Embed the query with a 2s timeout so a slow Ollama never blocks the agent.
	// On any error, silently fall through to FTS5-only results.
	var vectorResults []store.SearchResult
	searchMode := "fts5_bm25"
	if s.embedClient != nil {
		embedCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		queryVec, embedErr := s.embedClient.Embed(embedCtx, query)
		cancel()
		if embedErr == nil && len(queryVec) > 0 {
			vr, verr := s.store.VectorSearch(queryVec, limit)
			if verr == nil && len(vr) > 0 {
				vectorResults = vr
				searchMode = "vector_cosine"
			}
		}
	}

	// --- FTS5 path (always runs as fallback / supplement) ---
	ftsResults, err := s.store.SemanticSearch(query, limit)
	if err != nil {
		return toolError("semantic search", err)
	}

	// --- Merge: vector results first, then FTS results not already returned ---
	var results []store.SearchResult
	if len(vectorResults) > 0 {
		results = vectorResults
		seen := make(map[string]bool, len(vectorResults))
		for _, r := range vectorResults {
			seen[r.ID] = true
		}
		for _, r := range ftsResults {
			if !seen[r.ID] {
				results = append(results, r)
			}
		}
		if len(results) > limit {
			results = results[:limit]
		}
		if len(vectorResults) > 0 {
			searchMode = "hybrid_vector+fts5"
		}
	} else {
		results = ftsResults
	}

	resp := map[string]interface{}{
		"query":       query,
		"count":       len(results),
		"results":     results,
		"search_mode": searchMode,
	}

	embeddingCount := s.store.EmbeddingCount()
	if s.embedClient != nil && searchMode == "fts5_bm25" {
		if embeddingCount == 0 {
			resp["note"] = fmt.Sprintf("Vector embeddings not yet built. Run 'synapses index' or wait for the background embedding pass to complete (model: %s).", s.embedClient.Model())
		} else {
			resp["note"] = fmt.Sprintf("Vector index partial (%d nodes embedded). Results blended from cosine+FTS5 as more embeddings complete.", embeddingCount)
		}
	}
	if len(results) == 0 {
		resp["hint"] = "No matches found. Try broader terms, partial names, or use search() for exact substring matching."
	}

	return jsonResult(resp)
}

// inlineFindEntity runs the find_entity lookup inline. Used by handleGetContext
// to surface candidates when the requested entity cannot be found by exact name,
// saving agents a separate find_entity round-trip.
func (s *Server) inlineFindEntity(query string) []map[string]interface{} {
	nodes := s.graph.FindByName(query)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPatternLimit(query, 50)
	}
	// Dotted method name fallback: "Store.Close" → search "Close", filter by "Store".
	if len(nodes) == 0 && strings.Contains(query, ".") {
		parts := strings.SplitN(query, ".", 2)
		typePrefix, method := strings.ToLower(parts[0]), parts[1]
		candidates := s.graph.FindByName(method)
		if len(candidates) == 0 {
			candidates = s.graph.FindByPatternLimit(method, 50)
		}
		for _, n := range candidates {
			if strings.Contains(strings.ToLower(string(n.ID)), typePrefix) ||
				strings.Contains(strings.ToLower(n.File), typePrefix) {
				nodes = append(nodes, n)
			}
		}
	}

	pathPrefix := s.graph.Root()
	if pathPrefix != "" && !strings.HasSuffix(pathPrefix, "/") {
		pathPrefix += "/"
	}

	results := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		file := n.File
		if pathPrefix != "" {
			file = strings.TrimPrefix(file, pathPrefix)
		}
		results = append(results, map[string]interface{}{
			"name": n.Name,
			"type": string(n.Type),
			"file": file,
			"line": n.Line,
		})
	}
	return results
}

// truncateAtWord shortens s to at most maxChars Unicode code points, breaking
// at the last space before the limit and appending "…" when truncation occurs.
// Safe for multi-byte UTF-8 (operates on runes, not bytes).
func truncateAtWord(s string, maxChars int) string {
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	// Walk backward from maxChars-1 to find the last space.
	cut := maxChars - 1 // leave room for ellipsis rune
	for cut > 0 && runes[cut] != ' ' {
		cut--
	}
	if cut == 0 {
		// No space found — hard cut at maxChars-1 to fit the ellipsis.
		cut = maxChars - 1
	}
	return string(runes[:cut]) + "…"
}

// handleGetEdgeTypes returns the full EdgeTypeCatalog: every edge type registered
// in the graph with its semantic weight, BFS direction, domain tag, and description.
// The catalog is the foundation for multi-domain BFS (Sprint 12) — agents can
// query it to understand traversal semantics or to select domain-specific edge filters.
//
// Response format: {"edge_types": [EdgeTypeDescriptor...], "total": N}
// The array is sorted by descending semantic_weight (highest-impact edges first).
func (s *Server) handleGetEdgeTypes(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	format, _ := req.GetArguments()["format"].(string)

	catalog := graph.GetEdgeTypes()

	if format == "compact" {
		// Compact: one line per edge type, sorted by weight descending.
		// Useful for quick orientation without token budget pressure.
		var sb strings.Builder
		sb.WriteString("# Edge Type Catalog\n\n")
		sb.WriteString("Sorted by BFS semantic weight (descending). Higher weight = traversed first.\n\n")
		sb.WriteString(fmt.Sprintf("%-20s %-8s %-10s %s\n", "TYPE", "WEIGHT", "DOMAIN", "DESCRIPTION"))
		sb.WriteString(strings.Repeat("-", 80) + "\n")
		for _, d := range catalog {
			synMark := ""
			if d.Synthetic {
				synMark = "*"
			}
			sb.WriteString(fmt.Sprintf("%-20s %-8.2f %-10s %s%s\n",
				string(d.Name), d.SemanticWeight, d.Domain, truncateAtWord(d.Description, 60), synMark))
		}
		sb.WriteString("\n* = synthetic edge (heuristic-injected, not AST-derived)\n")
		sb.WriteString("\nUse format=\"json\" for full descriptions and machine-readable output.\n")
		return mcp.NewToolResultText(sb.String()), nil
	}

	return jsonResult(map[string]interface{}{
		"edge_types": catalog,
		"total":      len(catalog),
		"note":       "Sorted by semantic_weight descending. Synthetic edges are heuristic-injected (not AST-derived). Sprint 9 adds infra/api domain edges; Sprint 12 adds cross-domain edges.",
	})
}

// adaptiveCarveConfig adjusts cfg in-place based on stored feedback episodes
// for the given entity+agent pair. Returns true when the detail level should
// be forced to "full" (caller handles compact-format override).
//
// Two signals are read from the episode store (last 30 days):
//  1. context_quality + failure (explicit helpful=false) within the last 7 days
//     → depth += 1, force full detail
//  2. repeated_context (≥2 cross-session repeat episodes within 30 days)
//     → expand to depth=3 (if not already deeper), force full detail
//
// Older feedback (7–30 days) is ignored so decay is natural over time.

// handleLinkEntities creates a user-defined cross-domain edge between two entities.
// Edges are persisted in the manual_edges store table so they survive restarts and
// reindexes. The in-memory graph is updated immediately — get_context and get_impact
// can traverse the edge in the same session.
func (s *Server) handleLinkEntities(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("link_entities requires a store — daemon may not be running"), nil
	}

	const maxRelationLen = 256

	args := req.GetArguments()
	a, ok := args["a"].(string)
	if !ok || strings.TrimSpace(a) == "" {
		return mcp.NewToolResultError("a is required — entity name or node ID for the source"), nil
	}
	b, ok := args["b"].(string)
	if !ok || strings.TrimSpace(b) == "" {
		return mcp.NewToolResultError("b is required — entity name or node ID for the target"), nil
	}
	relation, ok := args["relation"].(string)
	relation = strings.TrimSpace(relation)
	if !ok || relation == "" {
		return mcp.NewToolResultError("relation is required (e.g. 'CALLS', 'DEPLOYS', 'DEPENDS_ON', or any label)"), nil
	}
	if len(relation) > maxRelationLen {
		return mcp.NewToolResultError(fmt.Sprintf("relation exceeds max length (%d chars)", maxRelationLen)), nil
	}
	domain, _ := args["domain"].(string)
	if len(domain) > maxRelationLen {
		domain = domain[:maxRelationLen]
	}
	agentID, _ := args["agent_id"].(string)

	// Resolve a → node ID, capturing ambiguity count for caller warning.
	fromNode, fromAmbiguous := s.resolveEntityRefWithCount(a)
	if fromNode == nil {
		return mcp.NewToolResultError(fmt.Sprintf("entity not found: %q — use find_entity to discover the correct name or ID", a)), nil
	}
	// Resolve b → node ID.
	toNode, toAmbiguous := s.resolveEntityRefWithCount(b)
	if toNode == nil {
		return mcp.NewToolResultError(fmt.Sprintf("entity not found: %q — use find_entity to discover the correct name or ID", b)), nil
	}

	fromID := fromNode.ID
	toID := toNode.ID

	// Persist the edge so it survives restarts and reindexes.
	// clearSuppressed=true: human explicitly (re-)creating this edge overrides any
	// prior confirm_edge rejection.
	saved, err := s.store.SaveManualEdge(fromID, toID, relation, domain, agentID, 1.0, true)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("persist edge: %v", err)), nil
	}

	// Inject into the live in-memory graph. AddEdge is idempotent — safe to call
	// even if the edge already exists. Unknown relation strings get BFS weight 0.5
	// (the edgeWeight fallback) — they ARE traversed, just at medium priority.
	s.graph.AddEdge(&graph.Edge{
		From: fromID,
		To:   toID,
		Type: graph.EdgeType(relation),
	})

	// Check whether the relation is a known catalog type to advise the caller.
	knownType := false
	for _, d := range graph.GetEdgeTypes() {
		if d.Name == graph.EdgeType(relation) {
			knownType = true
			break
		}
	}

	result := map[string]interface{}{
		"linked":   true,
		"from":     map[string]string{"id": string(fromID), "name": fromNode.Name, "file": fromNode.File},
		"to":       map[string]string{"id": string(toID), "name": toNode.Name, "file": toNode.File},
		"relation": saved.Relation,
		"domain":   saved.Domain,
		"hint":     "Edge is live in this session and will persist across restarts. Use get_context or get_impact to traverse it.",
	}
	if !knownType {
		// Unknown types get weight 0.5 from edgeWeight() fallback — they ARE traversed.
		// Only CALLS/IMPLEMENTS/etc. are in the catalog with higher dedicated weights.
		result["weight_note"] = fmt.Sprintf("Relation %q is not in the edge catalog (BFS weight=0.5 fallback). Edge will be traversed but at lower priority than catalog types. Use get_edge_types to see all catalog types.", relation)
	}
	// Warn when name resolution was ambiguous — agent may have linked the wrong entity.
	var warnings []string
	if fromAmbiguous > 1 {
		warnings = append(warnings, fmt.Sprintf("source %q matched %d entities — linked first match (id=%s). Use full node ID to be precise.", a, fromAmbiguous, fromID))
	}
	if toAmbiguous > 1 {
		warnings = append(warnings, fmt.Sprintf("target %q matched %d entities — linked first match (id=%s). Use full node ID to be precise.", b, toAmbiguous, toID))
	}
	if len(warnings) > 0 {
		result["ambiguity_warnings"] = warnings
	}
	return jsonResult(result)
}

// resolveEntityRefWithCount resolves an entity name or full node ID to a graph Node
// and also returns the total number of matches found (for ambiguity detection).
// Tries exact node-ID lookup first (unambiguous by definition), then FindByName,
// then substring match. Returns (nil, 0) if no match found.
//
// When multiple candidates share the same name, selects the one with the highest
// fan-in (incoming CALLS edge count) — the most-called entity is almost always
// the one the caller means. This beats insertion-order for common names like
// "New", "Close", "Init" that exist in many packages.
func (s *Server) resolveEntityRefWithCount(nameOrID string) (*graph.Node, int) {
	// Full node ID (repoID::file::name) — unambiguous by definition.
	if strings.Contains(nameOrID, "::") {
		if n := s.graph.GetNode(graph.NodeID(nameOrID)); n != nil {
			return n, 1
		}
	}
	// Exact name match — may return multiple nodes with the same name.
	nodes := s.graph.FindByName(nameOrID)
	if len(nodes) >= 1 {
		return bestByFanIn(s.graph, nodes), len(nodes)
	}
	// Substring / pattern match — bounded to avoid large scans.
	nodes = s.graph.FindByPatternLimit(nameOrID, 5)
	if len(nodes) >= 1 {
		return bestByFanIn(s.graph, nodes), len(nodes)
	}
	return nil, 0
}

// bestByFanIn returns the node with the highest incoming-edge count from candidates.
// Prefers exported nodes over unexported as a secondary tiebreaker, and
// non-test files over test files as a tertiary tiebreaker.
// This heuristic makes link_entities prefer production-code hubs over test stubs.
func bestByFanIn(g *graph.Graph, candidates []*graph.Node) *graph.Node {
	if len(candidates) == 1 {
		return candidates[0]
	}
	best := candidates[0]
	bestScore := nodeScore(g, best)
	for _, n := range candidates[1:] {
		if s := nodeScore(g, n); s > bestScore {
			bestScore = s
			best = n
		}
	}
	return best
}

// nodeScore computes a composite priority for ambiguous name resolution.
// Fan-in is the primary signal; exported and non-test are tiebreakers.
func nodeScore(g *graph.Graph, n *graph.Node) int {
	fanIn := len(g.InEdges(n.ID))
	score := fanIn * 4 // fan-in carries 4× weight
	if n.Exported {
		score += 2
	}
	if !strings.Contains(n.File, "_test.go") {
		score += 1
	}
	return score
}

// handleUnlinkEntities removes a previously-created manual edge immediately
// from both the in-memory graph and the persistent store.
func (s *Server) handleUnlinkEntities(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("unlink_entities requires a store — daemon may not be running"), nil
	}

	args := req.GetArguments()
	a, ok := args["a"].(string)
	if !ok || strings.TrimSpace(a) == "" {
		return mcp.NewToolResultError("a is required — entity name or node ID for the source"), nil
	}
	b, ok := args["b"].(string)
	if !ok || strings.TrimSpace(b) == "" {
		return mcp.NewToolResultError("b is required — entity name or node ID for the target"), nil
	}
	relation, ok := args["relation"].(string)
	relation = strings.TrimSpace(relation)
	if !ok || relation == "" {
		return mcp.NewToolResultError("relation is required — must match the relation used in link_entities"), nil
	}

	// Resolve endpoints. Use same heuristic as link_entities.
	fromNode, _ := s.resolveEntityRefWithCount(a)
	if fromNode == nil {
		return mcp.NewToolResultError(fmt.Sprintf("entity not found: %q — use find_entity to discover the correct node ID", a)), nil
	}
	toNode, _ := s.resolveEntityRefWithCount(b)
	if toNode == nil {
		return mcp.NewToolResultError(fmt.Sprintf("entity not found: %q — use find_entity to discover the correct node ID", b)), nil
	}

	fromID := fromNode.ID
	toID := toNode.ID

	// Verify the manual edge exists in the store before removing.
	// Uses primary-key lookup — O(log N), no full table scan.
	found, err := s.store.ManualEdgeExists(fromID, toID, relation)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("check edge: %v", err)), nil
	}
	if !found {
		return mcp.NewToolResultError(fmt.Sprintf(
			"no manual edge found: %s -[%s]-> %s — was it created with link_entities?",
			fromID, relation, toID,
		)), nil
	}

	// Remove from persistent store first — if this fails, leave the graph unchanged.
	if err := s.store.DeleteManualEdge(fromID, toID, relation); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delete edge: %v", err)), nil
	}

	// Remove from in-memory graph immediately. Effect is instant — no restart needed.
	s.graph.RemoveEdge(fromID, toID, graph.EdgeType(relation))

	return jsonResult(map[string]interface{}{
		"unlinked": true,
		"from":     map[string]string{"id": string(fromID), "name": fromNode.Name},
		"to":       map[string]string{"id": string(toID), "name": toNode.Name},
		"relation": relation,
		"hint":     "Edge removed from graph and store. Effect is immediate — get_context will no longer traverse this edge.",
	})
}

// handleConfirmEdge allows a human to approve or reject a cross-domain edge produced
// by the name-matcher. Confirmed edges get confidence=1.0 and are never re-scored.
// Rejected edges are suppressed permanently — they will not appear in get_context
// results and the name-matcher will not re-inject them between restarts.
func (s *Server) handleConfirmEdge(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("confirm_edge requires a store — daemon may not be running"), nil
	}

	args := req.GetArguments()
	a, ok := args["a"].(string)
	if !ok || strings.TrimSpace(a) == "" {
		return mcp.NewToolResultError("a is required — entity name or node ID for the source"), nil
	}
	b, ok := args["b"].(string)
	if !ok || strings.TrimSpace(b) == "" {
		return mcp.NewToolResultError("b is required — entity name or node ID for the target"), nil
	}
	relation, ok := args["relation"].(string)
	relation = strings.TrimSpace(relation)
	if !ok || relation == "" {
		return mcp.NewToolResultError("relation is required — the edge type label (e.g. MENTIONS, DEPLOYS)"), nil
	}
	confirmedRaw, ok := args["confirmed"]
	if !ok {
		return mcp.NewToolResultError("confirmed is required — true to approve the edge, false to reject it permanently"), nil
	}
	confirmed, ok := confirmedRaw.(bool)
	if !ok {
		return mcp.NewToolResultError("confirmed must be a boolean — true to approve, false to reject"), nil
	}

	fromNode, _ := s.resolveEntityRefWithCount(a)
	if fromNode == nil {
		return mcp.NewToolResultError(fmt.Sprintf("entity not found: %q — use find_entity to discover the correct node ID", a)), nil
	}
	toNode, _ := s.resolveEntityRefWithCount(b)
	if toNode == nil {
		return mcp.NewToolResultError(fmt.Sprintf("entity not found: %q — use find_entity to discover the correct node ID", b)), nil
	}

	if err := s.store.ConfirmEdge(fromNode.ID, toNode.ID, relation, confirmed); err != nil {
		// Check reverse direction — the name-matcher uses orderEdge (heavier domain
		// first) so the stored direction may be the opposite of what the caller passed.
		if revErr := s.store.ConfirmEdge(toNode.ID, fromNode.ID, relation, confirmed); revErr == nil {
			// Reverse worked: swap for the success path below.
			fromNode, toNode = toNode, fromNode
		} else {
			return mcp.NewToolResultError(fmt.Sprintf(
				"%v — also tried reverse direction and found no edge. Use get_context on either entity to see its cross-domain edges.",
				err,
			)), nil
		}
	}

	if confirmed {
		// Re-inject in case the edge was previously suppressed and removed from the graph.
		s.graph.AddEdge(&graph.Edge{From: fromNode.ID, To: toNode.ID, Type: graph.EdgeType(relation)})
	} else {
		// For rejections: remove from the live graph immediately so get_context stops
		// traversing the edge without waiting for the next restart.
		s.graph.RemoveEdge(fromNode.ID, toNode.ID, graph.EdgeType(relation))
	}

	status := "confirmed"
	hint := "Edge confidence set to 1.0. The name-matcher will not re-score this edge. It will continue to appear in get_context results."
	if !confirmed {
		status = "suppressed"
		hint = "Edge suppressed permanently. Removed from the live graph. The name-matcher will not re-inject it. Use link_entities if you change your mind."
	}

	return jsonResult(map[string]interface{}{
		"status":   status,
		"from":     map[string]string{"id": string(fromNode.ID), "name": fromNode.Name},
		"to":       map[string]string{"id": string(toNode.ID), "name": toNode.Name},
		"relation": relation,
		"hint":     hint,
	})
}

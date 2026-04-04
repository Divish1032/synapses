package mcp

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// registerResources wires MCP Resources to their handlers.
// Three resources are exposed:
//
//	synapses://active-context  — compact project briefing, updated on file changes
//	synapses://file/{path}     — entity map for a specific file
//	synapses://violations      — current architectural violations
func (s *Server) registerResources() {
	// Knowledge mode: skip graph-dependent resources.
	if s.knowledgeMode {
		return
	}

	// synapses://active-context
	s.mcp.AddResource(
		mcp.NewResource(
			"synapses://active-context",
			"Active Project Context",
			mcp.WithResourceDescription(
				"Compact project briefing: scale, recently changed files, pending tasks, "+
					"and active violations. Re-read whenever you need a project orientation "+
					"without calling session_init.",
			),
			mcp.WithMIMEType("text/plain"),
		),
		s.handleActiveContextResource,
	)

	// synapses://file/{path}
	s.mcp.AddResourceTemplate(
		mcp.NewResourceTemplate(
			"synapses://file/{path}",
			"File Entity Map",
			mcp.WithTemplateDescription(
				"All entities defined in the given file (functions, methods, structs, "+
					"interfaces) with line numbers and exported status. "+
					"Use a relative path suffix, e.g. 'internal/graph/traverse.go'.",
			),
			mcp.WithTemplateMIMEType("text/plain"),
		),
		s.handleFileResource,
	)

	// synapses://violations
	s.mcp.AddResource(
		mcp.NewResource(
			"synapses://violations",
			"Current Violations",
			mcp.WithResourceDescription(
				"Active architectural rule violations detected in the indexed codebase. "+
					"Empty string when the codebase is clean.",
			),
			mcp.WithMIMEType("text/plain"),
		),
		s.handleViolationsResource,
	)

	// ── Sprint 24: Tools → Resources migration ──────────────────────────────

	// synapses://repo-map (was get_repo_map tool)
	s.mcp.AddResource(
		mcp.NewResource(
			"synapses://repo-map",
			"Repository Map",
			mcp.WithResourceDescription(
				"Navigable package+entity map grouped by architectural layer "+
					"(entry points, API surface, core logic, persistence, config). "+
					"Top 3 entities per package by fanin. Cached until structural change.",
			),
			mcp.WithMIMEType("text/plain"),
		),
		s.handleRepoMapResource,
	)

	// synapses://edge-types (was get_edge_types tool)
	s.mcp.AddResource(
		mcp.NewResource(
			"synapses://edge-types",
			"Edge Type Catalog",
			mcp.WithResourceDescription(
				"Semantic catalog of all graph edge types with BFS weights, "+
					"direction, domain tags, and descriptions. Compact text table format.",
			),
			mcp.WithMIMEType("text/plain"),
		),
		s.handleEdgeTypesResource,
	)

	// synapses://analytics (was get_my_analytics tool)
	s.mcp.AddResource(
		mcp.NewResource(
			"synapses://analytics",
			"Agent Analytics",
			mcp.WithResourceDescription(
				"Personal analytics summary: tool call counts, context deliveries, "+
					"tokens saved, cost savings, and cache hit rate. Defaults to last 7 days.",
			),
			mcp.WithMIMEType("text/plain"),
		),
		s.handleAnalyticsResource,
	)

	// synapses://decision-log (was get_decision_log tool)
	if s.getBrainClient() != nil {
		s.mcp.AddResource(
			mcp.NewResource(
				"synapses://decision-log",
				"Decision Log",
				mcp.WithResourceDescription(
					"Agent decision audit trail: action, agent, SDLC phase, entity, "+
						"and outcome. Last 20 entries. Requires brain.url configured.",
				),
				mcp.WithMIMEType("text/plain"),
			),
			s.handleDecisionLogResource,
		)
	}

	// synapses://query/{q} (was query_graph tool)
	s.mcp.AddResourceTemplate(
		mcp.NewResourceTemplate(
			"synapses://query/{q}",
			"Graph Query",
			mcp.WithTemplateDescription(
				"Constrained DSL for direct graph node filtering. "+
					"Syntax: NODES WHERE package=\"auth\" AND fanin > 5. "+
					"Fields: package, type, domain, file, name, exported, fanin, fanout.",
			),
			mcp.WithTemplateMIMEType("text/plain"),
		),
		s.handleQueryResource,
	)

	// synapses://tool-params/{tool} — detailed parameter docs for each tool.
	// Moved out of tool descriptions (Sprint 30.1) to keep tool descriptions ≤150 tokens.
	// Agents call this when they need full parameter details for a specific tool.
	s.mcp.AddResourceTemplate(
		mcp.NewResourceTemplate(
			"synapses://tool-params/{tool}",
			"Tool Parameter Reference",
			mcp.WithTemplateDescription(
				"Full parameter documentation for a Synapses tool. "+
					"Use when you need parameter details beyond the brief tool description. "+
					"Supported tools: session_init, get_context, validate, search, get_impact, memory, tasks, end_session.",
			),
			mcp.WithTemplateMIMEType("text/plain"),
		),
		s.handleToolParamsResource,
	)
}

// notifyResourceChanged sends a notifications/resources/updated delta to all
// connected MCP clients. The client receives only the URI that changed — not
// the full content — and decides whether to re-read based on its current task.
// This prevents stdio pipe overload while still enabling proactive updates.
func (s *Server) notifyResourceChanged(uri string) {
	s.mcp.SendNotificationToAllClients(
		mcp.MethodNotificationResourceUpdated,
		map[string]any{"uri": uri},
	)
}

// ---------------------------------------------------------------------------
// Resource handlers
// ---------------------------------------------------------------------------

// handleActiveContextResource returns a compact Markdown dashboard briefing.
// Target: ≤400 tokens so agents can "glance" at it for orientation without
// spending significant context budget.
func (s *Server) handleActiveContextResource(
	_ context.Context,
	_ mcp.ReadResourceRequest,
) ([]mcp.ResourceContents, error) {
	var b strings.Builder

	// Project identity.
	repoID := s.graph.RepoID()
	nodeCount := s.graph.NodeCount()
	edgeCount := s.graph.EdgeCount()
	identity := s.graph.ProjectIdentity()

	fmt.Fprintf(&b, "# Synapses Active Context\n")
	fmt.Fprintf(&b, "**Project:** %s\n", repoID)
	fmt.Fprintf(&b, "**Scale:** %d Nodes | %d Edges | %s\n", nodeCount, edgeCount, identity.Scale)

	// Most recently changed file (single line — keeps it under budget).
	if s.changeSource != nil {
		changes := s.changeSource.RecentChanges(15)
		if len(changes) > 0 {
			root := s.graph.Root()
			prefix := root
			if prefix != "" && !strings.HasSuffix(prefix, "/") {
				prefix += "/"
			}
			c := changes[0]
			relFile := strings.TrimPrefix(c.File, prefix)
			age := formatChangeAge(c.Timestamp)
			fmt.Fprintf(&b, "**Last Change:** %s (%s ago)\n", relFile, age)
		}
	}

	// Violations — single line, critical number up front.
	violations := s.checkViolations()
	if len(violations) > 0 {
		fmt.Fprintf(&b, "**Critical Violations:** %d", len(violations))
		// Show the first error-severity violation inline if present.
		for _, v := range violations {
			if v.Severity == "error" {
				fmt.Fprintf(&b, " — %s", v.Description)
				break
			}
		}
		b.WriteString("\n")
	} else {
		b.WriteString("**Critical Violations:** 0\n")
	}

	// Active task — top in-progress or highest-priority pending.
	if s.store != nil {
		tasks, err := s.store.GetPendingTasks("", "")
		if err == nil && len(tasks) > 0 {
			// Prefer in_progress tasks.
			active := tasks[0]
			for _, t := range tasks {
				if t.Status == "in_progress" {
					active = t
					break
				}
			}
			fmt.Fprintf(&b, "**Active Task:** \"%s\" [%s/%s]\n", active.Title, active.Priority, active.Status)
			if len(tasks) > 1 {
				fmt.Fprintf(&b, "**Pending Tasks:** %d total\n", len(tasks))
			}
		}
	}

	// Constitution principles (brief — only if ≤3, else count only).
	if s.config != nil && len(s.config.Constitution.Principles) > 0 {
		ps := s.config.Constitution.Principles
		if len(ps) <= 3 {
			b.WriteString("**Laws:** ")
			b.WriteString(strings.Join(ps, " · "))
			b.WriteString("\n")
		} else {
			fmt.Fprintf(&b, "**Laws:** %d project principles — use get_project_identity() for full list\n", len(ps))
		}
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      "synapses://active-context",
			MIMEType: "text/plain",
			Text:     strings.TrimSpace(b.String()),
		},
	}, nil
}

// formatChangeAge returns a human-readable age string for a file change timestamp.
func formatChangeAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// handleFileResource returns an entity map for the file identified by {path}.
func (s *Server) handleFileResource(
	_ context.Context,
	req mcp.ReadResourceRequest,
) ([]mcp.ResourceContents, error) {
	uri := req.Params.URI
	filePath := strings.TrimPrefix(uri, "synapses://file/")
	if filePath == "" {
		return nil, fmt.Errorf("file path required in URI, e.g. synapses://file/internal/graph/traverse.go")
	}
	// Decode percent-encoded characters before traversal check to prevent
	// bypasses like %2e%2e/%2e%2e/etc/passwd.
	if decoded, err := url.PathUnescape(filePath); err == nil {
		filePath = decoded
	}
	// Reject absolute paths and null bytes.
	if strings.HasPrefix(filePath, "/") || strings.Contains(filePath, "\x00") {
		return nil, fmt.Errorf("invalid file URI path")
	}
	// Reject path traversal attempts — ".." components could escape the repo root.
	for _, seg := range strings.Split(filepath.ToSlash(filePath), "/") {
		if seg == ".." {
			return nil, fmt.Errorf("path traversal not allowed in file URI")
		}
	}

	nodes := s.graph.FindByFile(filePath)
	if len(nodes) == 0 {
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      uri,
				MIMEType: "text/plain",
				Text:     fmt.Sprintf("No entities found for file: %q\nThe file may not be indexed yet — try reindex.", filePath),
			},
		}, nil
	}

	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Line < nodes[j].Line
	})

	root := s.graph.Root()
	prefix := root
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	var b strings.Builder
	relFile := strings.TrimPrefix(nodes[0].File, prefix)
	pkg := nodes[0].Package
	fmt.Fprintf(&b, "# %s", relFile)
	if pkg != "" {
		fmt.Fprintf(&b, " (package %s)", pkg)
	}
	fmt.Fprintf(&b, " — %d entities\n\n", len(nodes))

	for _, n := range nodes {
		expMark := ""
		if n.Exported {
			expMark = " ✓"
		}
		line := ""
		if n.Line > 0 {
			line = fmt.Sprintf(":%d", n.Line)
		}
		fmt.Fprintf(&b, "[%s] %s%s · %s%s\n", n.Name, n.Type, expMark, filepath.Base(n.File), line)
		if n.Metadata != nil {
			if doc := n.Metadata["doc"]; doc != "" {
				if len(doc) > 100 {
					doc = doc[:97] + "…"
				}
				fmt.Fprintf(&b, "  %s\n", doc)
			}
		}
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      uri,
			MIMEType: "text/plain",
			Text:     strings.TrimSpace(b.String()),
		},
	}, nil
}

// handleViolationsResource returns current architectural violations as plain text.
func (s *Server) handleViolationsResource(
	_ context.Context,
	_ mcp.ReadResourceRequest,
) ([]mcp.ResourceContents, error) {
	violations := s.checkViolations()

	var b strings.Builder
	if len(violations) == 0 {
		b.WriteString("No architectural violations detected.")
	} else {
		fmt.Fprintf(&b, "%d violation(s):\n\n", len(violations))
		for _, v := range violations {
			from := string(v.FromNode)
			to := string(v.ToNode)
			if n := s.graph.GetNode(v.FromNode); n != nil {
				from = n.Name
			}
			if n := s.graph.GetNode(v.ToNode); n != nil {
				to = n.Name
			}
			fmt.Fprintf(&b, "⚠ [%s] %s\n  Rule: %s · From: %s → To: %s\n\n",
				v.Severity, v.Description, v.RuleID, from, to)
		}
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      "synapses://violations",
			MIMEType: "text/plain",
			Text:     strings.TrimSpace(b.String()),
		},
	}, nil
}

// ---------------------------------------------------------------------------
// Violation helper (delegates to config.CheckViolations)
// ---------------------------------------------------------------------------

// checkViolations runs the rule engine and returns all current violations.
// It wraps config.CheckViolations under the rules mutex to avoid data races.
func (s *Server) checkViolations() []violationRef {
	if s.config == nil {
		return nil
	}
	s.rulesMu.RLock()
	vs := s.config.CheckViolations(s.graph)
	s.rulesMu.RUnlock()

	out := make([]violationRef, len(vs))
	for i, v := range vs {
		out[i] = violationRef{
			RuleID:      v.RuleID,
			Severity:    v.Severity,
			Description: v.Description,
			FromNode:    v.FromNode,
			ToNode:      v.ToNode,
		}
	}
	return out
}

// violationRef is a slim struct used by resource handlers to avoid importing
// config.Violation directly in display logic.
type violationRef struct {
	RuleID      string
	Severity    string
	Description string
	FromNode    graph.NodeID
	ToNode      graph.NodeID
}

// ---------------------------------------------------------------------------
// Watcher integration
// ---------------------------------------------------------------------------

// InvalidatePacketCacheForFile clears the in-memory packet cache and sends MCP
// resource-updated notifications for the changed file.
func (s *Server) InvalidatePacketCacheForFile(changedFile string) {
	s.packetCacheMu.Lock()
	s.packetCache = make(map[string]*packetCacheEntry, packetCacheMax)
	s.packetCacheMu.Unlock()

	// Invalidate the orientation cache (explain_codebase, get_repo_map).
	// These are stored separately from the packet cache to survive LRU eviction,
	// but they must be cleared on any structural graph change.
	s.orientMu.Lock()
	s.orientExplain = nil
	s.orientRepoCompact = nil
	s.orientRepoFull = nil
	s.orientMu.Unlock()

	// Loop guard no longer resets on file change. File saves are NOT evidence
	// of agent progress — agents save files as part of their loop. The loop
	// guard now auto-resets when the agent's NEXT call has a DIFFERENT
	// fingerprint (proving they moved on), or after 60s of inactivity.

	s.notifyResourceChanged("synapses://active-context")
	s.notifyResourceChanged("synapses://violations")
	if changedFile != "" {
		root := s.graph.Root()
		prefix := root
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		relFile := strings.TrimPrefix(changedFile, prefix)
		s.notifyResourceChanged("synapses://file/" + relFile)
	}
}

// ---------------------------------------------------------------------------
// Sprint 24: Tool → Resource handlers
// ---------------------------------------------------------------------------
// These resource handlers reuse the existing tool handler logic by calling
// the handler with a synthetic request and extracting the text from the result.

// toolResultToResource calls a tool handler and wraps its text output as a Resource.
func toolResultToResource(uri string, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) ([]mcp.ResourceContents, error) {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	result, err := handler(context.Background(), req)
	if err != nil {
		return nil, err
	}
	text := ""
	if result != nil {
		if result.IsError {
			return nil, fmt.Errorf("resource handler error: %v", result.Content)
		}
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(mcp.TextContent); ok {
				text = tc.Text
			}
		}
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      uri,
			MIMEType: "text/plain",
			Text:     text,
		},
	}, nil
}

func (s *Server) handleRepoMapResource(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	return toolResultToResource("synapses://repo-map", s.handleGetRepoMap, map[string]any{"detail": "compact"})
}

func (s *Server) handleEdgeTypesResource(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	return toolResultToResource("synapses://edge-types", s.handleGetEdgeTypes, map[string]any{"format": "compact"})
}

func (s *Server) handleAnalyticsResource(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	return toolResultToResource("synapses://analytics", s.handleGetMyAnalytics, map[string]any{})
}

func (s *Server) handleDecisionLogResource(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	return toolResultToResource("synapses://decision-log", s.handleGetDecisionLog, map[string]any{})
}

// handleToolParamsResource returns full parameter documentation for a named tool.
// URI: synapses://tool-params/{tool}
func (s *Server) handleToolParamsResource(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	uri := req.Params.URI
	toolName := ""
	if idx := strings.Index(uri, "synapses://tool-params/"); idx >= 0 {
		toolName = uri[len("synapses://tool-params/"):]
	}

	docs := toolParamDocs(toolName)
	if docs == "" {
		docs = fmt.Sprintf("Unknown tool: %q. Supported: session_init, get_context, validate, search, get_impact, memory, tasks, end_session.", toolName)
	}
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      uri,
			MIMEType: "text/plain",
			Text:     docs,
		},
	}, nil
}

// toolParamDocs returns the full parameter reference for a named tool.
// Content moved from tool descriptions to keep descriptions ≤150 tokens (Sprint 30.1).
func toolParamDocs(tool string) string {
	switch tool {
	case "session_init":
		return `# session_init — Full Parameter Reference

Parameters:
  agent_id (string)  — Self-declared identifier. Enables incremental delivery: subsequent
                       calls skip unchanged sections. Always provide for token savings.
  intent   (string)  — Short declaration of current work (visible to peer agents).
                       Pass "" to clear. E.g. "implementing auth middleware".
  scope    (string)  — Controls response verbosity:
                       "standard" (default): tasks + working_state + scale_guidance (~500t)
                       "full": all sections including project_identity, memories, health stats
                       "quick": alias for standard (legacy)
                       "resume": task continuity after reconnect
                       "compaction": recovery briefing after context compaction
  format   (string)  — "json" (default, structured) | "kv" (labeled key-value, ~5x smaller)
  detail_level (str) — "signal" (~30t, warnings only) | "summary" (~150t, default) | "full" (~500t)
  token_budget (num) — Max response tokens. Default 500. 0 = no limit.
  model    (string)  — Model name for analytics (e.g. "claude-sonnet-4-6").
  provider (string)  — Model provider: "anthropic", "openai", etc.
  peer_window_hours (num) — Hours back to look for peer activity. Default 24.

KV format example (format=kv, detail_level=summary):
  # SESSION (dev/0.9.5 | 2026-04-04)
  Status: 2 pending tasks | 0 violations
  Task: implement-auth — Implement OAuth2 middleware [in_progress]
  Convention: table-driven tests (7 sessions)
  Warning: jwt-go v3 incompatible — failed 3x; use github.com/golang-jwt/jwt`

	case "get_context":
		return `# get_context — Full Parameter Reference

Parameters:
  entity       (str) — Entity name for mode=context. Required unless mode=path/investigate.
  mode         (str) — "context" (default): BFS graph traversal
                       "intent": one-call assembly for a declared goal
                       "path": shortest call chain between two entities
                       "investigate": rank suspicious locations by relevance to a problem
  intent       (str) — For mode=intent: "understand"|"modify"|"add"|"debug"|"review"|"plan"
  depth        (num) — BFS hop limit (mode=context). Defaults to project config.
  token_budget (num) — Max tokens. Default 4000 (context) or intent-specific.
  file         (str) — File path suffix to disambiguate (e.g. "internal/auth/service.go").
  format       (str) — "compact" (default) | "json" | "kv" (alias for compact)
  detail_level (str) — "summary" (~50t) | "neighbors" (~200t, default) | "full" (~600t)
  task_id      (str) — Relevance-boost by linked task.
  agent_id     (str) — For peer tracking.
  projects     (str) — Comma-separated federation aliases.
  from, to     (str) — Source and target entity for mode=path.
  problem      (str) — Problem description for mode=investigate.`

	case "validate":
		return `# validate — Full Parameter Reference

Phases:
  pre (default) — Check proposed changes against architectural rules before writing.
                  Params: changes (JSON array), check_safety (bool), plan_description (str)
  pre_write     — Describe what you are ABOUT TO WRITE; checked against security patterns.
                  Params: description (str, required), files (JSON array)
  post          — Audit written files for new violations.
                  Params: files_written (JSON array, required), task_id (str)
  list          — List active violations.
                  Params: rule_id (str filter), include_log (bool), log_limit (num)
  full          — Compound gate: scope + safety + rules.
                  Params: target (str), file (str), changes (JSON), plan_description (str)
  safety        — Check failure history for similar past mistakes.
                  Params: plan_description (str), agent_id (str), project_id (str)
  upsert_rule   — Add/update architectural rule.
                  Params: description, severity, edge_type, from_file_pattern, to_file_pattern,
                          to_name_pattern, path_pattern, context_source
  delete_rule   — Remove rule. Params: rule_id (str, required)
  candidates    — Show rule candidates from observed violations.
  upsert_adr    — Add/update Architecture Decision Record.
                  Params: id, title, decision, adr_status, context, consequences, linked_files
  list_adrs     — List ADRs. Params: query (str filter)

Common params:
  format        (str) — "json" (default) | "kv" (one line per finding)
  detail_level  (str) — "signal" (CRITICAL only) | "summary" (all findings, default) | "full"
  token_budget  (num) — Max response tokens. Default 300.
  override      (bool) — Pass true to proceed past CRITICAL findings. Logged as episode.
  agent_id      (str) — Attribution.

KV format example (format=kv):
  # VALIDATE | post | handlers/users.go
  [CRITICAL] missing-auth: POST /api/users lacks auth middleware (8/8 routes have it)
  [MEDIUM] coupling-increase: handlers → store direct call (skip service layer)
  Action: BLOCK — fix CRITICAL before proceeding | override=true to bypass`

	case "search":
		return `# search — Full Parameter Reference

Parameters:
  query        (str) — Search term. Required.
  mode         (str) — "keyword" (default): substring match across entity names
                       "fulltext": BM25 ranked full-text search
                       "semantic": vector search (describe concept, not exact name)
                       "exact": precise name-to-node-ID lookup (fastest)
  limit        (num) — Max results. Default 8.
  format       (str) — "json" (default) | "kv" (compact result list)
  detail_level (str) — "signal" (names only) | "summary" (names + file:line, default) | "full"
  token_budget (num) — Max response tokens. Default 300.
  agent_id     (str) — Logged for analytics.
  projects     (str) — Comma-separated federation aliases for cross-project search.

KV format example (format=kv):
  # SEARCH: "handleLogin" | 3 results
  handleLogin (auth/handlers.go:145) — callers:4, callees:3
  loginHandler (api/router.go:89) — alias
  handleLoginAttempt (auth/attempts.go:67) — related`

	case "get_impact":
		return `# get_impact — Full Parameter Reference

Parameters:
  symbol       (str) — Entity name to analyse. Required unless files= provided.
  files        (str) — Comma-separated file paths for PR-level blast radius.
  depth        (num) — Max hop depth. Default 3, max 10.
  token_budget (num) — Max response tokens. Default 2000. Peripheral nodes dropped first.
  scope        (str) — "review": adds blast_radius summary, test_gaps, risk_flags, failure_history
  projects     (str) — Federation aliases for cross-project callers.
  format       (str) — "json" (default) | "kv" (compact blast-radius summary)
  detail_level (str) — "signal" (count only) | "summary" (top callers, default) | "full"`

	case "memory":
		return `# memory — Full Parameter Reference

Actions and their params:
  save          — Record episode. Params: agent_id (req), decision (req), episode_type,
                  outcome, rationale, trigger, affected_files, affected_nodes, tags,
                  anchor_nodes, memory_importance, project_id
  search        — Keyword/semantic recall. Params: query, outcome_filter, limit, include_stale,
                  projects, as_of, since, until, depth
  list          — Chronological browse. Params: limit, since_days, tags
  annotate      — Attach note to graph entity. Params: node_id (req), note (req)
  annotate_web  — Attach note + web hits to entity. Params: node_id (req), note, hits
  add_gap       — Track quality gap. Params: node_id (req), gap_id (req), gap_description (req),
                  gap_severity, gap_status, fix_notes
  list_gaps     — List quality gaps. Params: gap_severity, gap_status, file
  history       — Entity change timeline. Params: entity (req), file
  hypothesize   — Record working theory. Params: content (new) or hypothesis_id + state (update)
  list_hypotheses — Retrieve by state. Params: state_filter
  decide        — Record structured decision. Params: choice (req), alternatives, reasoning, context
  list_decisions — Search decisions. Params: query, limit
  abandon       — Record rejected approach. Params: approach (req), failure_reason (req),
                  blocker, context, agent_id
  list_rejected — Search rejected approaches. Params: query, limit

Common params:
  format        (str) — "json" (default) | "kv" (compact labeled list)
  detail_level  (str) — "signal" (count) | "summary" (titles + outcomes, default) | "full"
  token_budget  (num) — Max response tokens. Default 300.`

	case "tasks":
		return `# tasks — Full Parameter Reference

Actions and their params:
  create_plan  — Create tracking plan. Params: title (req), description, tasks (JSON array),
                 agent_id. Task objects: {title, description, spec_items:[{id,description}]}
  list_plans   — All plans overview. No extra params.
  pending      — Pending/in-progress tasks. Params: plan_id, agent_id, suggest_next
  update       — Mark task done. Params: id (req), status (req), notes, intent, agent_id
  update_spec_item — Mark spec item done. Params: task_id (req), item_id (req), done (bool)
  save_state   — Checkpoint session state. Params: task_id (req), approach, files_modified,
                 completed_steps, remaining_steps, blockers, decisions, context_snapshot, agent_id
  get_state    — Restore session state. Params: task_id (req)
  link_nodes   — Connect task to graph entities. Params: task_id (req), node_ids (JSON array)

Common params:
  format        (str) — "json" (default) | "kv" (compact task list)
  detail_level  (str) — "signal" (counts only) | "summary" (titles + status, default) | "full"
  token_budget  (num) — Max response tokens. Default 300.`

	case "end_session":
		return `# end_session — Full Parameter Reference

Parameters:
  agent_id     (str) — Required. Self-declared identifier.
  summary      (str) — High-level summary of work. Saved as project-tier memory.
  task_id      (str) — Link session memories to a task.
  model        (str) — Model name for usage reporting (e.g. "claude-sonnet-4-6").
  provider     (str) — Model provider: "anthropic", "openai", etc.
  input_tokens (num) — Total input tokens consumed.
  output_tokens (num) — Total output tokens generated.
  cost_usd     (num) — Total USD cost if known.
  format       (str) — "json" (default) | "kv" (compact effectiveness report)
  detail_level (str) — "signal" (summary line only) | "summary" (metrics, default) | "full"

Returns:
  effectiveness_report: context_hit_rate, first_fetch_right, tokens_saved, 7-day trend.
  memory_extracted: count of conventions/failures auto-extracted from session.`

	default:
		return ""
	}
}

func (s *Server) handleQueryResource(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	// Extract query from URI: synapses://query/{q}
	uri := req.Params.URI
	q := ""
	if idx := strings.Index(uri, "synapses://query/"); idx >= 0 {
		q = uri[len("synapses://query/"):]
	}
	if q == "" {
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      uri,
				MIMEType: "text/plain",
				Text:     "Error: query parameter required. Example: synapses://query/NODES WHERE package=\"auth\"",
			},
		}, nil
	}
	// URL-decode the query
	decoded, err := url.QueryUnescape(q)
	if err == nil {
		q = decoded
	}
	return toolResultToResource(uri, s.handleQueryGraph, map[string]any{"query": q})
}

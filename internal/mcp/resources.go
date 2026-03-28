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

	"github.com/SynapsesOS/synapses/internal/brain"
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

// warmBrainCacheDebounce is the minimum interval between consecutive warm cycles.
const warmBrainCacheDebounce = 2 * time.Second

// warmBrainCache proactively pre-computes brain packets for entities in the
// changed file and stores them in the brain's SQLite insight cache (6h TTL).
// Runs in a background goroutine so the file-watcher callback is non-blocking.
// Capped at 5 entities per call and debounced to 2s to avoid hammering the
// brain on rapid successive saves.
func (s *Server) warmBrainCache(changedFile string) {
	bc := s.getBrainClient()
	if bc == nil {
		return
	}

	// Debounce: skip if we warmed the cache less than 2s ago.
	s.warmMu.Lock()
	if time.Since(s.lastWarm) < warmBrainCacheDebounce {
		s.warmMu.Unlock()
		return
	}
	s.lastWarm = time.Now()
	s.warmMu.Unlock()

	entities := s.graph.FindByFile(changedFile)
	if len(entities) == 0 {
		return
	}
	if len(entities) > 5 {
		entities = entities[:5]
	}

	ents := make([]*graph.Node, len(entities))
	copy(ents, entities)
	projID := s.projectID
	enableLLM := s.config.Brain.ContextBuilderEnabled()
	s.goBackground(func() {
		for _, entity := range ents {
			ctx30, cancel30 := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel30()
			_ = bc.BuildContextPacket(ctx30, brain.ContextPacketRequest{
				ProjectID: projID,
				Snapshot: brain.SnapshotInput{
					RootNodeID: string(entity.ID),
					RootName:   entity.Name,
					RootType:   string(entity.Type),
					RootFile:   entity.File,
					RootDoc:    entity.Metadata["doc"],
					HasTests:   fileHasTests(entity.File),
					FanIn:      s.graph.Fanin(entity.ID),
				},
				EnableLLM: enableLLM,
			})
		}
	})
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

// InvalidatePacketCacheForFile clears the in-memory packet cache, sends MCP
// resource-updated notifications (currently a no-op, see notifyResourceChanged),
// and proactively warms the brain cache for entities in the changed file.
//
// Call this from the file watcher instead of (or in addition to) the plain
// InvalidatePacketCache to activate brain pre-warming.
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
		s.warmBrainCache(changedFile)
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

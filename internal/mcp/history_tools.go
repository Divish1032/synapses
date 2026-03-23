package mcp

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// timelineEvent is a single entry in the entity history timeline.
// All data sources are normalised into this format before merging.
type timelineEvent struct {
	Type      string `json:"type"`      // memory, episode, annotation, task, git_change
	Timestamp int64  `json:"timestamp"` // Unix seconds — used for sorting
	Date      string `json:"date"`      // Human-readable RFC3339
	Summary   string `json:"summary"`
	Detail    string `json:"detail,omitempty"`
}

// handleGetEntityHistory returns a chronological timeline compositing memories,
// episodes, annotations, task references, and git changes for a named entity.
// One tool call replaces five. Serves Speed.
//
// Memory lookup uses BOTH paths:
//   - entity_id column (QueryMemories): direct 1:1 entity-tier memories
//   - memory_anchors table (GetMemoriesByAnchorNode): memories linked via anchor_nodes=
//
// Results are deduplicated by memory ID to prevent doubles when a memory is linked
// through both entity_id and anchor_nodes.
func (s *Server) handleGetEntityHistory(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	entityName, _ := req.GetArguments()["entity"].(string)
	entityName = strings.TrimSpace(entityName)
	if entityName == "" {
		return mcp.NewToolResultError(
			"entity is required (e.g., 'AuthService', 'handleLogin'). " +
				"Pass the name of a code entity to see its full history timeline.",
		), nil
	}

	if s.graph == nil {
		return mcp.NewToolResultError(
			"get_entity_history requires a code graph. " +
				"This tool is not available in knowledge-only mode.",
		), nil
	}

	fileHint, _ := req.GetArguments()["file"].(string)

	limitF, _ := req.GetArguments()["limit"].(float64)
	limit := int(limitF)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	// ── Resolve entity name to node(s) ──────────────────────────────────────
	node, ambiguousMsg := s.resolveEntityNode(entityName, fileHint)
	if node == nil {
		return mcp.NewToolResultText(ambiguousMsg), nil
	}

	nodeID := string(node.ID)

	// ── Collect from all sources in parallel ─────────────────────────────────
	// 6 goroutines: entity memories, anchored memories, episodes, annotations,
	// tasks, git changes. Each writes to its own local slice — no shared state
	// until appendEvents grabs the mutex.
	var (
		mu       sync.Mutex
		events   []timelineEvent
		warnings []string
		wg       sync.WaitGroup
	)

	appendEvents := func(evts []timelineEvent) {
		if len(evts) == 0 {
			return
		}
		mu.Lock()
		events = append(events, evts...)
		mu.Unlock()
	}

	appendWarning := func(msg string) {
		mu.Lock()
		warnings = append(warnings, msg)
		mu.Unlock()
	}

	// Track seen memory IDs to deduplicate across entity_id and anchor paths.
	var seenMu sync.Mutex
	seenMemIDs := make(map[string]bool)

	memoryToEvent := func(id, content, createdAt, tier, source, agentID string) (timelineEvent, bool) {
		seenMu.Lock()
		if seenMemIDs[id] {
			seenMu.Unlock()
			return timelineEvent{}, false
		}
		seenMemIDs[id] = true
		seenMu.Unlock()

		ts := parseTimestamp(createdAt)
		return timelineEvent{
			Type:      "memory",
			Timestamp: ts,
			Date:      createdAt,
			Summary:   truncate(content, 200),
			Detail:    compactDetail("tier", tier, "source", source, "agent", agentID),
		}, true
	}

	// 1a. Entity-tier memories (via entity_id column)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if s.store == nil || ctx.Err() != nil {
			return
		}
		mems, err := s.store.QueryMemories("entity", nodeID, "", limit)
		if err != nil {
			appendWarning(fmt.Sprintf("entity memories query failed: %v", err))
			return
		}
		var evts []timelineEvent
		for _, m := range mems {
			if evt, ok := memoryToEvent(m.ID, m.Content, m.CreatedAt, m.Tier, m.Source, m.AgentID); ok {
				evts = append(evts, evt)
			}
		}
		appendEvents(evts)
	}()

	// 1b. Anchored memories (via memory_anchors junction table)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if s.store == nil || ctx.Err() != nil {
			return
		}
		mems, err := s.store.GetMemoriesByAnchorNode(nodeID, limit)
		if err != nil {
			appendWarning(fmt.Sprintf("anchored memories query failed: %v", err))
			return
		}
		var evts []timelineEvent
		for _, m := range mems {
			if evt, ok := memoryToEvent(m.ID, m.Content, m.CreatedAt, m.Tier, m.Source, m.AgentID); ok {
				evts = append(evts, evt)
			}
		}
		appendEvents(evts)
	}()

	// 2. Episodes
	wg.Add(1)
	go func() {
		defer wg.Done()
		if s.store == nil || ctx.Err() != nil {
			return
		}
		eps, err := s.store.FindEpisodesByNodeID(nodeID, limit)
		if err != nil {
			appendWarning(fmt.Sprintf("episodes query failed: %v", err))
			return
		}
		var evts []timelineEvent
		for _, e := range eps {
			evts = append(evts, timelineEvent{
				Type:      "episode",
				Timestamp: e.CreatedAt,
				Date:      time.Unix(e.CreatedAt, 0).UTC().Format(time.RFC3339),
				Summary:   truncate(e.Decision, 200),
				Detail:    compactDetail("type", e.EpisodeType, "outcome", e.Outcome, "trigger", truncate(e.Trigger, 80)),
			})
		}
		appendEvents(evts)
	}()

	// 3. Annotations
	wg.Add(1)
	go func() {
		defer wg.Done()
		if s.store == nil || ctx.Err() != nil {
			return
		}
		annMap, err := s.store.GetAnnotationsForNodes([]string{nodeID})
		if err != nil {
			appendWarning(fmt.Sprintf("annotations query failed: %v", err))
			return
		}
		var evts []timelineEvent
		for _, anns := range annMap {
			for _, a := range anns {
				ts := parseTimestamp(a.CreatedAt)
				src := a.Source
				if a.Stale {
					src += " (stale)"
				}
				evts = append(evts, timelineEvent{
					Type:      "annotation",
					Timestamp: ts,
					Date:      a.CreatedAt,
					Summary:   truncate(a.Note, 200),
					Detail:    compactDetail("source", src, "agent", a.AgentID),
				})
			}
		}
		appendEvents(evts)
	}()

	// 4. Task references
	wg.Add(1)
	go func() {
		defer wg.Done()
		if s.store == nil || ctx.Err() != nil {
			return
		}
		tasks, err := s.store.FindTasksByNodeID(nodeID, limit)
		if err != nil {
			appendWarning(fmt.Sprintf("tasks query failed: %v", err))
			return
		}
		var evts []timelineEvent
		for _, t := range tasks {
			ts := parseTimestamp(t.UpdatedAt)
			evts = append(evts, timelineEvent{
				Type:      "task",
				Timestamp: ts,
				Date:      t.UpdatedAt,
				Summary:   fmt.Sprintf("[%s] %s", t.Status, t.Title),
				Detail:    compactDetail("priority", t.Priority, "assigned", t.AssignedTo, "plan", t.PlanID),
			})
		}
		appendEvents(evts)
	}()

	// 5. Git changes (for the entity's file)
	wg.Add(1)
	go func() {
		defer wg.Done()
		repoRoot := s.graph.Root()
		if repoRoot == "" || node.File == "" {
			return
		}
		evts := s.gitFileHistory(ctx, repoRoot, node.File, 20)
		appendEvents(evts)
	}()

	wg.Wait()

	// ── Merge and sort by timestamp descending (stable for determinism) ─────
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Timestamp > events[j].Timestamp
	})
	if len(events) > limit {
		events = events[:limit]
	}

	// ── Format as compact natural-language timeline ─────────────────────────
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Entity History: %s\n", node.Name)
	fmt.Fprintf(&sb, "**File:** %s:%d  **Type:** %s\n\n", node.File, node.Line, node.Type)

	if len(warnings) > 0 {
		sb.WriteString("**Warnings** (some data may be incomplete):\n")
		for _, w := range warnings {
			fmt.Fprintf(&sb, "- %s\n", w)
		}
		sb.WriteString("\n")
	}

	if len(events) == 0 {
		sb.WriteString("No history found for this entity. It has no memories, episodes, annotations, task references, or git changes.\n")
		return mcp.NewToolResultText(sb.String()), nil
	}

	fmt.Fprintf(&sb, "%d events (newest first):\n\n", len(events))

	for _, e := range events {
		// Use short date for compactness: "2026-03-20 12:00"
		shortDate := e.Date
		if t, err := time.Parse(time.RFC3339, e.Date); err == nil {
			shortDate = t.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(&sb, "- **[%s]** %s — %s", e.Type, shortDate, e.Summary)
		if e.Detail != "" {
			fmt.Fprintf(&sb, " (%s)", e.Detail)
		}
		sb.WriteByte('\n')
	}

	return mcp.NewToolResultText(sb.String()), nil
}

// resolveEntityNode resolves an entity name (with optional file hint) to a
// single graph node. Returns (nil, message) when resolution fails; the message
// is a user-facing error or disambiguation prompt.
func (s *Server) resolveEntityNode(entityName, fileHint string) (*graph.Node, string) {
	nodes := s.graph.FindByName(entityName)
	if len(nodes) == 0 {
		nodes = s.graph.FindByPattern(entityName)
	}
	// Dotted-name resolution: "Graph.New" → find "New" filtered by prefix.
	if len(nodes) == 0 && strings.Contains(entityName, ".") {
		parts := strings.SplitN(entityName, ".", 2)
		prefix, method := strings.ToLower(parts[0]), parts[1]
		for _, n := range s.graph.FindByName(method) {
			if strings.Contains(strings.ToLower(string(n.ID)), prefix) ||
				strings.Contains(strings.ToLower(n.File), prefix) {
				nodes = append(nodes, n)
			}
		}
	}
	if len(nodes) == 0 {
		candidates := s.inlineFindEntity(entityName)
		if len(candidates) == 0 {
			return nil, fmt.Sprintf(
				"entity not found: %q\nHint: no substring match. Try search(query=\"...\", mode=\"semantic\") for concept-based lookup.",
				entityName,
			)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "entity not found: %q\nDid you mean one of these?\n", entityName)
		for _, c := range candidates {
			name, _ := c["name"].(string)
			file, _ := c["file"].(string)
			typ, _ := c["type"].(string)
			fmt.Fprintf(&sb, "  • %s (%s) in %s\n", name, typ, file)
		}
		sb.WriteString("Re-call get_entity_history with entity= set to one of the exact names above. Add file= to pin if multiple files match.")
		return nil, sb.String()
	}

	// File hint filtering.
	if fileHint != "" {
		var filtered []*graph.Node
		for _, n := range nodes {
			if strings.HasSuffix(n.File, fileHint) || strings.Contains(n.File, fileHint) {
				filtered = append(filtered, n)
			}
		}
		if len(filtered) > 0 {
			nodes = filtered
		}
	}

	// Ambiguity: multiple nodes remain — use pickBestNode for automatic disambiguation.
	if len(nodes) > 1 {
		best := pickBestNode(nodes, s.graph)
		return best, ""
	}

	return nodes[0], ""
}

// gitFileHistory returns git log entries for a specific file as timeline events.
// Limited to maxEntries commits. Uses a 5-second timeout.
func (s *Server) gitFileHistory(parentCtx context.Context, repoRoot, relFile string, maxEntries int) []timelineEvent {
	ctx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx,
		"git", "-C", repoRoot,
		"log", fmt.Sprintf("-%d", maxEntries),
		"--format=%H\x1f%an\x1f%ad\x1f%s",
		"--date=iso-strict",
		"--follow",
		"--", relFile,
	).Output()
	if err != nil || len(out) == 0 {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var evts []timelineEvent
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x1f", 4)
		if len(parts) < 4 {
			continue
		}
		hash, author, dateStr, subject := parts[0], parts[1], parts[2], parts[3]
		ts := parseTimestamp(dateStr)
		shortHash := hash
		if len(shortHash) > 7 {
			shortHash = shortHash[:7]
		}
		evts = append(evts, timelineEvent{
			Type:      "git_change",
			Timestamp: ts,
			Date:      dateStr,
			Summary:   fmt.Sprintf("%s %s", shortHash, subject),
			Detail:    fmt.Sprintf("author=%s", author),
		})
	}
	return evts
}

// parseTimestamp attempts to parse an RFC3339, ISO-8601, or Unix timestamp string
// into Unix seconds. Returns 0 on failure (sorts to the end).
func parseTimestamp(s string) int64 {
	// Try RFC3339 first (most common in our store).
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	// Try ISO-8601 with timezone offset (git --date=iso-strict).
	if t, err := time.Parse("2006-01-02T15:04:05-07:00", s); err == nil {
		return t.Unix()
	}
	// Try date-only.
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Unix()
	}
	return 0
}

// truncate returns at most maxLen runes of s, appending "…" if truncated.
// Safe for multi-byte UTF-8 (CJK, emoji, etc.). Returns "" for maxLen <= 0.
func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "…"
}

// compactDetail builds a key=value detail string, omitting pairs where the value is empty.
// Example: compactDetail("tier", "entity", "agent", "") → "tier=entity"
func compactDetail(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		k, v := pairs[i], pairs[i+1]
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

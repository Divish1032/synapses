package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// Note: extractText and callTool are defined in handlers_federation_test.go
// and testhelpers_test.go respectively — same package.

// ── handleGetEntityHistory ──────────────────────────────────────────────────

func TestGetEntityHistory_MissingEntity(t *testing.T) {
	srv := newTestServer(t)
	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	if !strings.Contains(text, "entity is required") {
		t.Errorf("expected 'entity is required', got: %s", text)
	}
}

func TestGetEntityHistory_EntityNotFound(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)
	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "NonExistentThing",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	if !strings.Contains(text, "not found") {
		t.Errorf("expected 'not found' message, got: %s", text)
	}
}

func TestGetEntityHistory_EmptyTimeline(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)
	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "AuthLogin",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	if !strings.Contains(text, "Entity History: AuthLogin") {
		t.Errorf("expected header, got: %s", text)
	}
	// With no store data or git, timeline should note no history.
	if !strings.Contains(text, "No history found") {
		t.Errorf("expected 'No history found', got: %s", text)
	}
}

func TestGetEntityHistory_WithAnnotation(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	_, _ = srv.store.AddAnnotation(string(loginID), "test-agent", "Added rate limiting to this handler")

	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "AuthLogin",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	if !strings.Contains(text, "[annotation]") {
		t.Errorf("expected annotation event, got: %s", text)
	}
	if !strings.Contains(text, "rate limiting") {
		t.Errorf("expected annotation content, got: %s", text)
	}
}

func TestGetEntityHistory_WithEpisode(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	_, _ = srv.store.RememberEpisode(store.Episode{
		AgentID:       "test-agent",
		EpisodeType:   "decision",
		Outcome:       "success",
		Decision:      "Switched AuthLogin to use JWT tokens",
		AffectedNodes: `["` + string(loginID) + `"]`,
	})

	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "AuthLogin",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	if !strings.Contains(text, "[episode]") {
		t.Errorf("expected episode event, got: %s", text)
	}
	if !strings.Contains(text, "JWT tokens") {
		t.Errorf("expected episode content, got: %s", text)
	}
}

func TestGetEntityHistory_WithMemory(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:     "entity",
		Content:  "AuthLogin handles OAuth2 refresh flow",
		EntityID: string(loginID),
		AgentID:  "test-agent",
		Source:   "manual",
	})

	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "AuthLogin",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	if !strings.Contains(text, "[memory]") {
		t.Errorf("expected memory event, got: %s", text)
	}
	if !strings.Contains(text, "OAuth2") {
		t.Errorf("expected memory content, got: %s", text)
	}
}

func TestGetEntityHistory_WithTask(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	_, _, _ = srv.store.CreatePlan("test plan", "", "", []store.TaskInput{
		{Title: "Fix auth login bug", Priority: "p0", LinkedNodes: []string{string(loginID)}},
	})

	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "AuthLogin",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	if !strings.Contains(text, "[task]") {
		t.Errorf("expected task event, got: %s", text)
	}
	if !strings.Contains(text, "Fix auth login bug") {
		t.Errorf("expected task content, got: %s", text)
	}
}

func TestGetEntityHistory_FileHintDisambiguates(t *testing.T) {
	srv := newTestServer(t)

	// Create two entities with the same name in different files.
	addFunc := func(file, name, pkg string) graph.NodeID {
		id := srv.graph.MakeNodeID(file, name)
		srv.graph.AddNode(&graph.Node{
			ID:      id,
			Type:    graph.NodeFunction,
			Name:    name,
			File:    file,
			Line:    1,
			Package: pkg,
		})
		return id
	}

	id1 := addFunc("pkg/auth/handler.go", "New", "auth")
	_ = addFunc("pkg/db/handler.go", "New", "db")

	// Add annotation only to the auth one.
	_, _ = srv.store.AddAnnotation(string(id1), "agent", "auth handler note")

	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "New",
		"file":   "auth/handler.go",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	if !strings.Contains(text, "auth handler note") {
		t.Errorf("expected auth annotation with file hint, got: %s", text)
	}
}

func TestGetEntityHistory_LimitCapsResults(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	// Insert 10 annotations.
	for i := 0; i < 10; i++ {
		_, _ = srv.store.AddAnnotation(string(loginID), "agent", "note "+string(rune('A'+i)))
	}

	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "AuthLogin",
		"limit":  float64(3),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	if !strings.Contains(text, "3 events") {
		t.Errorf("expected '3 events', got: %s", text)
	}
}

func TestGetEntityHistory_NoGraph(t *testing.T) {
	srv := newTestServer(t)
	srv.graph = nil

	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "AuthLogin",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	if !strings.Contains(text, "not available in knowledge-only mode") {
		t.Errorf("expected knowledge-mode error, got: %s", text)
	}
}

func TestGetEntityHistory_NilStore(t *testing.T) {
	srv := newTestServer(t)

	// Add a node to the graph so entity resolution succeeds.
	id := srv.graph.MakeNodeID("auth.go", "AuthLogin")
	srv.graph.AddNode(&graph.Node{
		ID:   id,
		Type: graph.NodeFunction,
		Name: "AuthLogin",
		File: "auth.go",
		Line: 1,
	})
	srv.store = nil

	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "AuthLogin",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	// Should still return a response (git changes may exist), not panic.
	if !strings.Contains(text, "Entity History: AuthLogin") {
		t.Errorf("expected header even with nil store, got: %s", text)
	}
}

// ── resolveEntityNode ───────────────────────────────────────────────────────

func TestResolveEntityNode_DottedName(t *testing.T) {
	srv := newTestServer(t)

	id := srv.graph.MakeNodeID("pkg/auth/service.go", "Login")
	srv.graph.AddNode(&graph.Node{
		ID:      id,
		Type:    graph.NodeFunction,
		Name:    "Login",
		File:    "pkg/auth/service.go",
		Line:    10,
		Package: "auth",
	})

	node, msg := srv.resolveEntityNode("auth.Login", "")
	if node == nil {
		t.Fatalf("expected node, got message: %s", msg)
	}
	if node.Name != "Login" {
		t.Errorf("expected Login, got %s", node.Name)
	}
}

func TestGetEntityHistory_WithAnchoredMemory(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	// Insert a memory anchored via memory_anchors (not entity_id).
	// This is the path used by remember(anchor_nodes=[...]).
	memID, _ := srv.store.InsertMemory(store.Memory{
		Tier:    "project",
		Content: "Auth redesign: switched to OAuth2 flow",
		AgentID: "test-agent",
		Source:  "manual",
		// EntityID intentionally empty — not linked via entity_id column.
	})
	_ = srv.store.InsertMemoryAnchors(memID, []string{string(loginID)})

	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "AuthLogin",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	if !strings.Contains(text, "OAuth2") {
		t.Errorf("expected anchored memory content, got: %s", text)
	}
}

func TestGetEntityHistory_DeduplicatesMemories(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	// Insert a memory that is BOTH entity_id-linked AND anchor-linked.
	// Should appear only once in the timeline.
	memID, _ := srv.store.InsertMemory(store.Memory{
		Tier:     "entity",
		Content:  "Dual-linked memory for dedup test",
		EntityID: string(loginID),
		AgentID:  "test-agent",
		Source:   "manual",
	})
	_ = srv.store.InsertMemoryAnchors(memID, []string{string(loginID)})

	res, err := srv.handleGetEntityHistory(context.Background(), callTool(map[string]any{
		"entity": "AuthLogin",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := extractText(t, res)
	// Count occurrences of the memory content — should be exactly 1.
	if count := strings.Count(text, "Dual-linked memory"); count != 1 {
		t.Errorf("expected 1 occurrence (deduped), got %d in: %s", count, text)
	}
}

// ── truncate ────────────────────────────────────────────────────────────────

func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("expected no truncation, got %q", got)
	}
	if got := truncate("this is a long string", 10); !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation with ellipsis, got %q", got)
	}
	// Edge: maxLen 0 returns empty
	if got := truncate("anything", 0); got != "" {
		t.Errorf("expected empty for maxLen=0, got %q", got)
	}
	// Edge: negative maxLen returns empty
	if got := truncate("anything", -5); got != "" {
		t.Errorf("expected empty for negative maxLen, got %q", got)
	}
	// Unicode: CJK characters truncated correctly
	if got := truncate("日本語テスト", 3); got != "日本語…" {
		t.Errorf("expected CJK truncation, got %q", got)
	}
}

// ── compactDetail ───────────────────────────────────────────────────────────

func TestCompactDetail(t *testing.T) {
	// All present
	got := compactDetail("tier", "entity", "source", "manual", "agent", "claude")
	if got != "tier=entity source=manual agent=claude" {
		t.Errorf("all present: %q", got)
	}
	// Some empty
	got = compactDetail("tier", "entity", "source", "", "agent", "claude")
	if got != "tier=entity agent=claude" {
		t.Errorf("some empty: %q", got)
	}
	// All empty
	got = compactDetail("tier", "", "source", "", "agent", "")
	if got != "" {
		t.Errorf("all empty: %q", got)
	}
}

// ── parseTimestamp ──────────────────────────────────────────────────────────

func TestParseTimestamp_Formats(t *testing.T) {
	// RFC3339
	if ts := parseTimestamp("2026-03-20T12:00:00Z"); ts == 0 {
		t.Error("RFC3339 should parse")
	}
	// ISO-8601 with offset
	if ts := parseTimestamp("2026-03-20T12:00:00+05:30"); ts == 0 {
		t.Error("ISO-8601 offset should parse")
	}
	// Date only
	if ts := parseTimestamp("2026-03-20"); ts == 0 {
		t.Error("date-only should parse")
	}
	// Garbage
	if ts := parseTimestamp("not-a-date"); ts != 0 {
		t.Errorf("garbage should return 0, got %d", ts)
	}
	// Empty
	if ts := parseTimestamp(""); ts != 0 {
		t.Errorf("empty should return 0, got %d", ts)
	}
}

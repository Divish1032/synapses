package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
)

// ── stubChangeSource ────────────────────────────────────────────────────────

// stubChangeSource is a minimal ChangeSource for tests.
type stubChangeSource struct {
	events []watcher.ChangeEvent
}

func (s *stubChangeSource) RecentChanges(_ int) []watcher.ChangeEvent {
	return s.events
}

func newStubChangeSource(files ...string) *stubChangeSource {
	cs := &stubChangeSource{}
	for _, f := range files {
		cs.events = append(cs.events, watcher.ChangeEvent{
			File:      f,
			Timestamp: time.Now(),
		})
	}
	return cs
}

// ── helpers ─────────────────────────────────────────────────────────────────

// sessionInitResult calls handleSessionInit and returns the parsed response map.
func sessionInitResult(t *testing.T, s *Server, agentID string) map[string]any {
	t.Helper()
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": agentID,
	}))
	return mustResult(t, res, err)
}

// staleHintsFromResult extracts the stale_context_hints array, or nil if absent.
func staleHintsFromResult(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["stale_context_hints"]
	if !ok {
		return nil
	}
	// Re-marshal + unmarshal to get typed slice.
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal stale_context_hints: %v", err)
	}
	var hints []map[string]any
	if err := json.Unmarshal(b, &hints); err != nil {
		t.Fatalf("unmarshal stale_context_hints: %v", err)
	}
	return hints
}

// addNode adds a function node to the graph with the given file and name.
func addNode(g *graph.Graph, file, name string) graph.NodeID {
	id := g.MakeNodeID(file, name)
	g.AddNode(&graph.Node{
		ID:      id,
		Type:    graph.NodeFunction,
		Name:    name,
		File:    file,
		Line:    1,
		Package: "pkg",
	})
	return id
}

// ── TestStaleContextHints_TaskLinkedNodes ────────────────────────────────────

// TestStaleContextHints_TaskLinkedNodes verifies that a task with linked nodes
// whose file appears in recent changes produces stale hints.
func TestStaleContextHints_TaskLinkedNodes(t *testing.T) {
	s := newTestServer(t)
	g := graph.New("testrepo")
	nodeID := addNode(g, "pkg/auth/auth.go", "AuthLogin")
	s.graph = g
	s.SetChangeSource(newStubChangeSource("pkg/auth/auth.go"))

	// Create a plan with a task linked to the node.
	planID, _, err := s.store.CreatePlan("test", "", "agent-a", []store.TaskInput{
		{Title: "fix auth", Priority: "p0", LinkedNodes: []string{string(nodeID)}},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	// Mark in-progress so it appears in pendingSection.
	tasks, _ := s.store.GetPendingTasks(planID, "")
	if len(tasks) == 0 {
		t.Fatal("expected 1 task")
	}
	s.store.UpdateTask(tasks[0].ID, "in_progress", "", "agent-a")

	m := sessionInitResult(t, s, "agent-a")
	hints := staleHintsFromResult(t, m)

	if len(hints) == 0 {
		t.Fatal("expected stale_context_hints, got none")
	}
	if hints[0]["entity"] != "AuthLogin" {
		t.Errorf("expected entity=AuthLogin, got %q", hints[0]["entity"])
	}
}

// ── TestStaleContextHints_EntityRegister ────────────────────────────────────

// TestStaleContextHints_EntityRegister verifies that entities from a previous
// session log produce stale hints when their file appears in recent changes.
func TestStaleContextHints_EntityRegister(t *testing.T) {
	s := newTestServer(t)
	g := graph.New("testrepo")
	addNode(g, "pkg/store/store.go", "Store.Close")
	s.graph = g
	s.SetChangeSource(newStubChangeSource("pkg/store/store.go"))

	// Simulate a previous session log with "Examined: Store.Close."
	_, err := s.store.InsertMemory(store.Memory{
		Tier:    store.TierSessionLog,
		Content: "Session by agent-b at 2026-03-15 10:00. Examined: Store.Close.",
		AgentID: "agent-b",
		Source:  store.SourceAuto,
		Tags:    `["session_end","auto"]`,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	m := sessionInitResult(t, s, "agent-b")
	hints := staleHintsFromResult(t, m)

	if len(hints) == 0 {
		t.Fatal("expected stale_context_hints from entity register, got none")
	}
	if hints[0]["entity"] != "Store.Close" {
		t.Errorf("expected entity=Store.Close, got %q", hints[0]["entity"])
	}
}

// ── TestStaleContextHints_NoRecentChanges ───────────────────────────────────

// TestStaleContextHints_NoRecentChanges verifies that no hints are produced
// when there are no recently changed files.
func TestStaleContextHints_NoRecentChanges(t *testing.T) {
	s := newTestServer(t)
	// No change source set → recentChanges is empty.

	m := sessionInitResult(t, s, "agent-c")
	if _, ok := m["stale_context_hints"]; ok {
		t.Error("expected no stale_context_hints when no recent changes, but key is present")
	}
}

// ── TestStaleContextHints_FreshEntities ─────────────────────────────────────

// TestStaleContextHints_FreshEntities verifies that entities in unchanged files
// do NOT produce stale hints.
func TestStaleContextHints_FreshEntities(t *testing.T) {
	s := newTestServer(t)
	g := graph.New("testrepo")
	nodeID := addNode(g, "pkg/auth/auth.go", "AuthLogin")
	s.graph = g
	// Changed file is different from the node's file.
	s.SetChangeSource(newStubChangeSource("pkg/store/store.go"))

	_, _, err := s.store.CreatePlan("test", "", "agent-d", []store.TaskInput{
		{Title: "fix auth", Priority: "p0", LinkedNodes: []string{string(nodeID)}},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	m := sessionInitResult(t, s, "agent-d")
	hints := staleHintsFromResult(t, m)
	if len(hints) != 0 {
		t.Errorf("expected no stale hints for unchanged file, got %d", len(hints))
	}
}

// ── TestStaleContextHints_Cap10 ─────────────────────────────────────────────

// TestStaleContextHints_Cap10 verifies the result is capped at 10 hints even
// when more than 10 task-linked nodes are stale.
func TestStaleContextHints_Cap10(t *testing.T) {
	s := newTestServer(t)
	g := graph.New("testrepo")
	s.SetChangeSource(newStubChangeSource("pkg/auth/auth.go"))

	var nodeIDs []string
	for i := 0; i < 15; i++ {
		name := string([]rune{'F', 'u', 'n', 'c', rune('A' + i)})
		id := addNode(g, "pkg/auth/auth.go", name)
		nodeIDs = append(nodeIDs, string(id))
	}
	s.graph = g

	planID, _, err := s.store.CreatePlan("test", "", "agent-e", []store.TaskInput{
		{Title: "big task", Priority: "p0", LinkedNodes: nodeIDs},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, _ := s.store.GetPendingTasks(planID, "")
	s.store.UpdateTask(tasks[0].ID, "in_progress", "", "agent-e")

	m := sessionInitResult(t, s, "agent-e")
	hints := staleHintsFromResult(t, m)

	if len(hints) > 10 {
		t.Errorf("expected at most 10 hints, got %d", len(hints))
	}
	if len(hints) == 0 {
		t.Error("expected some hints, got none")
	}
}

// ── TestStaleContextHints_MalformedNodeID ───────────────────────────────────

// TestStaleContextHints_MalformedNodeID verifies no panic or error for a
// task linked to a node ID that doesn't have the expected "::" format.
func TestStaleContextHints_MalformedNodeID(t *testing.T) {
	s := newTestServer(t)
	s.SetChangeSource(newStubChangeSource("any/file.go"))

	// Create a task with a malformed node ID (missing "::" separators).
	planID, _, err := s.store.CreatePlan("test", "", "agent-f", []store.TaskInput{
		{Title: "task", Priority: "p0", LinkedNodes: []string{"not-a-valid-node-id"}},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, _ := s.store.GetPendingTasks(planID, "")
	s.store.UpdateTask(tasks[0].ID, "in_progress", "", "agent-f")

	// Must not panic.
	m := sessionInitResult(t, s, "agent-f")
	// Malformed ID should simply produce no hints (parts < 3).
	hints := staleHintsFromResult(t, m)
	if len(hints) != 0 {
		t.Errorf("expected no hints for malformed node ID, got %d", len(hints))
	}
}

// ── TestStaleContextHints_DeduplicatesNodeIDs ───────────────────────────────

// TestStaleContextHints_DeduplicatesNodeIDs verifies that if the same node ID
// appears in both task-linked nodes and the entity register, only one hint is
// returned.
func TestStaleContextHints_DeduplicatesNodeIDs(t *testing.T) {
	s := newTestServer(t)
	g := graph.New("testrepo")
	nodeID := addNode(g, "pkg/auth/auth.go", "AuthLogin")
	s.graph = g
	s.SetChangeSource(newStubChangeSource("pkg/auth/auth.go"))

	// Task linked to the same node.
	planID, _, err := s.store.CreatePlan("test", "", "agent-g", []store.TaskInput{
		{Title: "fix", Priority: "p0", LinkedNodes: []string{string(nodeID)}},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, _ := s.store.GetPendingTasks(planID, "")
	s.store.UpdateTask(tasks[0].ID, "in_progress", "", "agent-g")

	// Also record it in the entity register (previous session).
	_, _ = s.store.InsertMemory(store.Memory{
		Tier:    store.TierSessionLog,
		Content: "Session by agent-g at 2026-03-15 10:00. Examined: AuthLogin.",
		AgentID: "agent-g",
		Source:  store.SourceAuto,
		Tags:    `["session_end","auto"]`,
	})

	m := sessionInitResult(t, s, "agent-g")
	hints := staleHintsFromResult(t, m)

	// Should be exactly 1 hint, not 2.
	if len(hints) != 1 {
		t.Errorf("expected exactly 1 hint (dedup), got %d", len(hints))
	}
}

// ── TestParseExaminedEntities_HappyPath ─────────────────────────────────────

func TestParseExaminedEntities_HappyPath(t *testing.T) {
	content := "Session by agent at 2026-03-15 10:00. Files: auth.go. Examined: AuthLogin, Store.Close, Graph.New."
	names := parseExaminedEntities(content)
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d: %v", len(names), names)
	}
	if names[0] != "AuthLogin" {
		t.Errorf("[0] expected AuthLogin, got %q", names[0])
	}
	if names[1] != "Store.Close" {
		t.Errorf("[1] expected Store.Close, got %q", names[1])
	}
	if names[2] != "Graph.New" {
		t.Errorf("[2] expected Graph.New, got %q", names[2])
	}
}

// ── TestParseExaminedEntities_Empty ─────────────────────────────────────────

func TestParseExaminedEntities_Empty(t *testing.T) {
	names := parseExaminedEntities("")
	if len(names) != 0 {
		t.Errorf("expected empty result for empty content, got %v", names)
	}
}

// ── TestParseExaminedEntities_NoMarker ──────────────────────────────────────

func TestParseExaminedEntities_NoMarker(t *testing.T) {
	content := "Session by agent at 2026-03-15 10:00. Files: auth.go. Tasks: task-1."
	names := parseExaminedEntities(content)
	if len(names) != 0 {
		t.Errorf("expected no names when no 'Examined:' marker, got %v", names)
	}
}

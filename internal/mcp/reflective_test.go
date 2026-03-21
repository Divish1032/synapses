package mcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// openMCPTestStore creates a test store by copying pre-initialized template
// databases from TestMain. Avoids re-running 50+ DDL migrations per test.
func openMCPTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	knowledgePath := store.KnowledgePath(dbPath)

	// Copy template files.
	copyTestFile(t, templateGraphDB, dbPath)
	copyTestFile(t, templateKnowledgeDB, knowledgePath)

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("openMCPTestStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func copyTestFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("copyTestFile %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("copyTestFile → %s: %v", dst, err)
	}
}

// buildReflectiveGraph returns a graph where hubFunc has fanin=5 (above the
// threshold of 3) and leafFunc has fanin=0 (below threshold).
func buildReflectiveGraph(t *testing.T) (*graph.Graph, graph.NodeID, graph.NodeID) {
	t.Helper()
	g := graph.New("testrepo")

	addFunc := func(name string) graph.NodeID {
		id := g.MakeNodeID("pkg/hub.go", name)
		g.AddNode(&graph.Node{
			ID:       id,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     "pkg/hub.go",
			Line:     1,
			Package:  "pkg",
			Exported: true,
		})
		return id
	}

	hubID := addFunc("HubFunc")
	leafID := addFunc("LeafFunc")

	// Five callers → hubID gets fanin=5.
	for i := 0; i < 5; i++ {
		callerName := string([]rune{'C', 'a', 'l', 'l', 'e', 'r', rune('A' + i)})
		callerID := addFunc(callerName)
		g.AddEdge(&graph.Edge{
			From: callerID,
			To:   hubID,
			Type: graph.EdgeCalls,
		})
	}
	// leafID has no callers → fanin=0.

	return g, hubID, leafID
}

// TestWriteRetrospectiveAnnotations verifies the B1 Reflective Synthesis auditor:
//   - high-fanin linked nodes (fanin > 3) receive a system annotation
//   - low-fanin linked nodes (fanin ≤ 3) are skipped
//   - the annotation has source='system'
func TestWriteRetrospectiveAnnotations(t *testing.T) {
	st := openMCPTestStore(t)
	g, hubID, leafID := buildReflectiveGraph(t)
	cfg, _ := config.Load(t.TempDir()) // default config, no rules

	srv := New(g, cfg, st)

	// Create a task linked to both nodes.
	planID, _, err := st.CreatePlan("refactor", "", "", []store.TaskInput{
		{
			Title:       "Refactor auth handler",
			Priority:    "p1",
			LinkedNodes: []string{string(hubID), string(leafID)},
		},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, err := st.GetPendingTasks(planID, "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("GetPendingTasks: err=%v count=%d", err, len(tasks))
	}
	taskID := tasks[0].ID

	// Call the auditor directly (unexported method — internal test).
	srv.writeRetrospectiveAnnotations(taskID, "agent-1", "all tests green")

	// Small sleep because writeRetrospectiveAnnotations is called in a goroutine
	// from handleUpdateTask, but here we call it directly so it's synchronous.
	// The sleep protects against any future refactor that makes it async again.
	time.Sleep(20 * time.Millisecond)

	// hubID (fanin=5 > 3) → must have a system annotation.
	hubAnns, err := st.GetAnnotationsForNodes([]string{string(hubID)})
	if err != nil {
		t.Fatalf("GetAnnotationsForNodes(hub): %v", err)
	}
	anns := hubAnns[string(hubID)]
	if len(anns) == 0 {
		t.Fatal("expected a system annotation on HubFunc (fanin=5), got none")
	}
	got := anns[0]
	if got.Source != "system" {
		t.Errorf("source: want 'system', got %q", got.Source)
	}
	if got.AgentID != "" {
		t.Errorf("agent_id: want empty, got %q", got.AgentID)
	}
	if got.Note == "" {
		t.Error("annotation note must not be empty")
	}

	// leafID (fanin=0 ≤ 3) → must NOT be annotated.
	leafAnns, err := st.GetAnnotationsForNodes([]string{string(leafID)})
	if err != nil {
		t.Fatalf("GetAnnotationsForNodes(leaf): %v", err)
	}
	if n := len(leafAnns[string(leafID)]); n != 0 {
		t.Errorf("expected no annotation on LeafFunc (fanin=0), got %d", n)
	}
}

// TestWriteRetrospectiveAnnotations_NoLinkedNodes verifies the auditor is a
// no-op when the task has no linked nodes (no annotations, no panic).
func TestWriteRetrospectiveAnnotations_NoLinkedNodes(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("testrepo")
	cfg, _ := config.Load(t.TempDir())

	srv := New(g, cfg, st)

	planID, _, err := st.CreatePlan("empty", "", "", []store.TaskInput{
		{Title: "No linked nodes", Priority: "p2"},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, _ := st.GetPendingTasks(planID, "")
	if len(tasks) == 0 {
		t.Fatal("expected 1 task")
	}

	// Should not panic or return an error.
	srv.writeRetrospectiveAnnotations(tasks[0].ID, "", "")
}

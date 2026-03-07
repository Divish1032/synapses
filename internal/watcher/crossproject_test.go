package watcher

// White-box unit tests for cross-project reactive propagation (Phase 5).
// These tests do NOT spin up a real filesystem watcher — they build a
// federated graph in-memory and call notifyCrossProjectImpact directly.

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/store"
)

// openTestStore opens an in-memory SQLite store for the test.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// buildFederatedGraph builds a two-project graph:
//   - primary project "proj-a" has one function node in the given file
//   - linked project "proj-b" has one function node that CALLS proj-a's node
//
// Returns the watcher (no FS watch), the primary graph, and the file path.
func buildFederatedGraph(t *testing.T, st *store.Store) (*Watcher, *graph.Graph, string) {
	t.Helper()

	dir := t.TempDir()
	changedFile := filepath.Join(dir, "auth.go")

	// Primary project graph (proj-a).
	ga := graph.New("proj-a")
	ga.SetRoot(dir)

	authID := ga.MakeNodeID(changedFile, "Authenticate")
	ga.AddNode(&graph.Node{
		ID: authID, Type: graph.NodeFunction,
		Name: "Authenticate", Package: "auth", File: changedFile, Line: 1,
	})

	// Linked project graph (proj-b) — separate repoID.
	gb := graph.New("proj-b")
	loginID := gb.MakeNodeID("/proj-b/login.go", "Login")
	gb.AddNode(&graph.Node{
		ID: loginID, Type: graph.NodeFunction,
		Name: "Login", Package: "login", File: "/proj-b/login.go", Line: 1,
	})
	// Merge proj-b into ga (simulates mergeLinkedProject at startup).
	// The cross-project edge must be added to ga AFTER the merge because
	// AddEdge silently drops edges whose endpoints don't exist in the target
	// graph. authID lives in ga; loginID will live in ga after MergeFrom.
	ga.MergeFrom(gb)

	// proj-b's Login CALLS proj-a's Authenticate (cross-project dependency).
	// Both nodes now exist in ga so the edge is accepted.
	ga.AddEdge(&graph.Edge{From: loginID, To: authID, Type: graph.EdgeCalls})

	w, err := New(ga, parser.NewWalker(), st)
	if err != nil {
		t.Fatalf("New watcher: %v", err)
	}
	return w, ga, changedFile
}

func TestNotifyCrossProjectImpact_SendsBroadcast(t *testing.T) {
	st := openTestStore(t)
	w, _, changedFile := buildFederatedGraph(t, st)

	w.notifyCrossProjectImpact(changedFile)

	// One broadcast message should have been written.
	msgs, _, err := st.GetMessages("any-agent", 0, "cross_project_impact", false, 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected cross_project_impact message, got none")
	}

	// Verify payload fields.
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(msgs[0].Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["changed_project"] != "proj-a" {
		t.Errorf("expected changed_project=proj-a, got %v", payload["changed_project"])
	}
	if payload["affected_project"] != "proj-b" {
		t.Errorf("expected affected_project=proj-b, got %v", payload["affected_project"])
	}
	// callers_affected should contain Login (the proj-b node that calls our changed code).
	callers, _ := payload["callers_affected"].([]interface{})
	if len(callers) == 0 {
		t.Error("expected callers_affected to be non-empty")
	}
}

func TestNotifyCrossProjectImpact_NoLinkedProjects_Silent(t *testing.T) {
	st := openTestStore(t)

	dir := t.TempDir()
	changedFile := filepath.Join(dir, "main.go")

	// Single-project graph — no federation.
	g := graph.New("solo-project")
	g.SetRoot(dir)
	nodeID := g.MakeNodeID(changedFile, "Run")
	g.AddNode(&graph.Node{
		ID: nodeID, Type: graph.NodeFunction,
		Name: "Run", Package: "main", File: changedFile, Line: 1,
	})

	w, err := New(g, parser.NewWalker(), st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w.notifyCrossProjectImpact(changedFile)

	msgs, _, err := st.GetMessages("any-agent", 0, "cross_project_impact", false, 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected no messages for single-project graph, got %d", len(msgs))
	}
}

func TestNotifyCrossProjectImpact_NilStore_NoPanic(t *testing.T) {
	dir := t.TempDir()
	changedFile := filepath.Join(dir, "a.go")

	g := graph.New("proj-a")
	g.SetRoot(dir)
	id := g.MakeNodeID(changedFile, "Foo")
	g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: "Foo", File: changedFile})

	w, _ := New(g, parser.NewWalker(), nil) // nil store
	// Must not panic.
	w.notifyCrossProjectImpact(changedFile)
}

func TestNotifyCrossProjectImpact_UnreadOnly_InSessionInit(t *testing.T) {
	st := openTestStore(t)
	w, _, changedFile := buildFederatedGraph(t, st)

	w.notifyCrossProjectImpact(changedFile)

	// unreadOnly=true should return the message (not yet marked read).
	msgs, _, err := st.GetMessages("agent-x", 0, "cross_project_impact", true, 10)
	if err != nil {
		t.Fatalf("GetMessages (unread): %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected unread cross_project_impact message")
	}
}

func TestRepoIDOfNodeID(t *testing.T) {
	tests := []struct {
		nodeID string
		want   string
	}{
		{"proj-a::internal/auth.go::Authenticate", "proj-a"},
		{"myrepo::cmd/main.go::main", "myrepo"},
		{"no-double-colon", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := repoIDOfNodeID(tt.nodeID); got != tt.want {
			t.Errorf("repoIDOfNodeID(%q) = %q, want %q", tt.nodeID, got, tt.want)
		}
	}
}

func TestDedup(t *testing.T) {
	got := dedup([]string{"a", "b", "a", "c", "b"})
	if len(got) != 3 {
		t.Errorf("expected 3 unique items, got %v", got)
	}
	if got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("unexpected order: %v", got)
	}
}

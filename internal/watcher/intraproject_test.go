package watcher

// White-box unit tests for intra-project proactive change alerts (F19).
// Tests call notifyIntraProjectImpact directly — no real FS watcher is spun up.

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── fileInScope unit tests ─────────────────────────────────────────────────────

func TestFileInScope_ExactMatch(t *testing.T) {
	if !fileInScope("internal/watcher/watcher.go", "internal/watcher/watcher.go") {
		t.Error("exact path should match scope")
	}
}

func TestFileInScope_DirectoryPrefix(t *testing.T) {
	if !fileInScope("internal/watcher/watcher.go", "internal/watcher") {
		t.Error("file inside directory scope should match")
	}
}

func TestFileInScope_DirectoryScopeWithTrailingSlash(t *testing.T) {
	if !fileInScope("internal/watcher/watcher.go", "internal/watcher/") {
		t.Error("scope with trailing slash should match file inside directory")
	}
}

func TestFileInScope_NoMatch_DifferentDirectory(t *testing.T) {
	if fileInScope("internal/parser/parser.go", "internal/watcher") {
		t.Error("file outside scope directory should not match")
	}
}

func TestFileInScope_NoMatch_PrefixSubstring(t *testing.T) {
	// "internal/watch" must NOT match "internal/watcher/watcher.go"
	// because it is a substring, not a directory prefix.
	if fileInScope("internal/watcher/watcher.go", "internal/watch") {
		t.Error("partial directory name substring should not match")
	}
}

func TestFileInScope_EmptyScope_NoMatch(t *testing.T) {
	// An empty scope should only match an empty relFile.
	if fileInScope("internal/watcher/watcher.go", "") {
		t.Error("empty scope should not match non-empty relFile")
	}
}

// ── notifyIntraProjectImpact integration tests ────────────────────────────────

// buildIntraFixture creates an in-memory watcher with a single node in changedFile.
// Returns the watcher, the node's ID, and the absolute changedFile path.
func buildIntraFixture(t *testing.T, st *store.Store) (*Watcher, graph.NodeID, string) {
	t.Helper()
	dir := t.TempDir()
	changedFile := filepath.Join(dir, "internal", "auth", "service.go")

	g := graph.New("my-project")
	g.SetRoot(dir)

	nodeID := g.MakeNodeID(changedFile, "Authenticate")
	g.AddNode(&graph.Node{
		ID:   nodeID,
		Type: graph.NodeFunction,
		Name: "Authenticate",
		File: changedFile,
		Line: 10,
	})

	w, err := New(g, parser.NewWalker(), st)
	if err != nil {
		t.Fatalf("New watcher: %v", err)
	}
	return w, nodeID, changedFile
}

func TestNotifyIntraProjectImpact_NilStore_NoPanic(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("proj")
	g.SetRoot(dir)
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Must not panic.
	w.notifyIntraProjectImpact(filepath.Join(dir, "foo.go"))
}

func TestNotifyIntraProjectImpact_ScopeAlert_SendsTargetedMessage(t *testing.T) {
	st := openTestStore(t)
	w, _, changedFile := buildIntraFixture(t, st)

	// Agent "agent-alpha" claims the directory that contains changedFile.
	// Scope is relative: "internal/auth" — changedFile's relFile will be
	// "internal/auth/service.go" after stripping the graph root prefix.
	_, err := st.ClaimWork("agent-alpha", "internal/auth", "directory", 30)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}

	w.notifyIntraProjectImpact(changedFile)

	msgs, _, err := st.GetMessages("agent-alpha", 0, "scope_change_alert", false, 50)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected scope_change_alert message for agent-alpha, got none")
	}
	if msgs[0].ToAgent != "agent-alpha" {
		t.Errorf("message addressed to %q, want agent-alpha", msgs[0].ToAgent)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(msgs[0].Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["claimed_scope"] != "internal/auth" {
		t.Errorf("claimed_scope = %v, want internal/auth", payload["claimed_scope"])
	}
	if payload["project"] != "my-project" {
		t.Errorf("project = %v, want my-project", payload["project"])
	}
}

func TestNotifyIntraProjectImpact_ScopeAlert_OutsideScope_NoMessage(t *testing.T) {
	st := openTestStore(t)
	w, _, changedFile := buildIntraFixture(t, st)

	// Agent claims a different directory — changedFile is in internal/auth,
	// not internal/parser.
	_, err := st.ClaimWork("agent-beta", "internal/parser", "directory", 30)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}

	w.notifyIntraProjectImpact(changedFile)

	msgs, _, err := st.GetMessages("agent-beta", 0, "scope_change_alert", false, 50)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected no scope_change_alert for out-of-scope agent, got %d", len(msgs))
	}
}

func TestNotifyIntraProjectImpact_ScopeAlert_DeduplicatesSameAgent(t *testing.T) {
	st := openTestStore(t)
	w, _, changedFile := buildIntraFixture(t, st)

	// Same agent claims two overlapping scopes that both cover the changed file.
	_, _ = st.ClaimWork("agent-gamma", "internal/auth", "directory", 30)
	_, _ = st.ClaimWork("agent-gamma", "internal", "directory", 30)

	w.notifyIntraProjectImpact(changedFile)

	msgs, _, err := st.GetMessages("agent-gamma", 0, "scope_change_alert", false, 50)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("expected exactly 1 scope_change_alert (deduplicated), got %d", len(msgs))
	}
}

func TestNotifyIntraProjectImpact_ScopeAlert_ExactFileMatch(t *testing.T) {
	st := openTestStore(t)
	w, _, changedFile := buildIntraFixture(t, st)

	// Agent claims the exact relative file path.
	_, err := st.ClaimWork("agent-delta", "internal/auth/service.go", "file", 30)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}

	w.notifyIntraProjectImpact(changedFile)

	msgs, _, err := st.GetMessages("agent-delta", 0, "scope_change_alert", false, 50)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected scope_change_alert for exact file claim, got none")
	}
}

func TestNotifyIntraProjectImpact_TaskNodeAlert_SendsToAssignedAgent(t *testing.T) {
	st := openTestStore(t)
	w, nodeID, changedFile := buildIntraFixture(t, st)

	// Create a plan with a task linked to the node that will change.
	_, err := st.CreatePlan("my-plan", "", "", []store.TaskInput{
		{Title: "Fix auth", Priority: "p1", LinkedNodes: []string{string(nodeID)}},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	// Auto-assign to "agent-omega" by calling GetPendingTasks with that agentID.
	_, _ = st.GetPendingTasks("", "agent-omega")

	w.notifyIntraProjectImpact(changedFile)

	msgs, _, err := st.GetMessages("agent-omega", 0, "task_node_changed", false, 50)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected task_node_changed message for agent-omega, got none")
	}
	if msgs[0].ToAgent != "agent-omega" {
		t.Errorf("message ToAgent = %q, want agent-omega", msgs[0].ToAgent)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(msgs[0].Payload), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["task_title"] != "Fix auth" {
		t.Errorf("task_title = %v, want Fix auth", payload["task_title"])
	}
	nodes, _ := payload["affected_nodes"].([]interface{})
	if len(nodes) == 0 {
		t.Error("expected affected_nodes to be non-empty")
	}
}

func TestNotifyIntraProjectImpact_TaskNodeAlert_UnlinkedNode_NoMessage(t *testing.T) {
	st := openTestStore(t)
	w, _, changedFile := buildIntraFixture(t, st)

	// Task linked to a completely different node ID.
	_, err := st.CreatePlan("unrelated", "", "", []store.TaskInput{
		{Title: "unrelated task", Priority: "p2", LinkedNodes: []string{"other-repo::other.go::OtherFunc"}},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	_, _ = st.GetPendingTasks("", "agent-zeta")

	w.notifyIntraProjectImpact(changedFile)

	msgs, _, err := st.GetMessages("agent-zeta", 0, "task_node_changed", false, 50)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected no task_node_changed for unrelated task, got %d", len(msgs))
	}
}

func TestNotifyIntraProjectImpact_TaskNodeAlert_UnassignedTask_NoMessage(t *testing.T) {
	st := openTestStore(t)
	w, nodeID, changedFile := buildIntraFixture(t, st)

	// Create a task but never assign it to anyone.
	_, err := st.CreatePlan("unassigned-plan", "", "", []store.TaskInput{
		{Title: "unassigned task", Priority: "p1", LinkedNodes: []string{string(nodeID)}},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	// Do NOT call GetPendingTasks with an agentID — leave assigned_to empty.

	w.notifyIntraProjectImpact(changedFile)

	// No task_node_changed message should be sent since there is no assigned agent.
	msgs, _, err := st.GetMessages("", 0, "task_node_changed", false, 50)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected no task_node_changed for unassigned task, got %d", len(msgs))
	}
}

func TestNotifyIntraProjectImpact_BothProbesFire(t *testing.T) {
	st := openTestStore(t)
	w, nodeID, changedFile := buildIntraFixture(t, st)

	// Agent "claimer" holds a scope covering the changed file.
	_, _ = st.ClaimWork("claimer", "internal/auth", "directory", 30)

	// Agent "tasker" owns an in-progress task linked to the node.
	_, err := st.CreatePlan("dual-plan", "", "", []store.TaskInput{
		{Title: "dual task", Priority: "p0", LinkedNodes: []string{string(nodeID)}},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	_, _ = st.GetPendingTasks("", "tasker")

	w.notifyIntraProjectImpact(changedFile)

	scopeMsgs, _, err := st.GetMessages("claimer", 0, "scope_change_alert", false, 50)
	if err != nil {
		t.Fatalf("GetMessages scope: %v", err)
	}
	if len(scopeMsgs) == 0 {
		t.Error("expected scope_change_alert for claimer agent")
	}

	taskMsgs, _, err := st.GetMessages("tasker", 0, "task_node_changed", false, 50)
	if err != nil {
		t.Fatalf("GetMessages task: %v", err)
	}
	if len(taskMsgs) == 0 {
		t.Error("expected task_node_changed for tasker agent")
	}
}

func TestNotifyIntraProjectImpact_NoNodesInFile_ScopeProbeStillFires(t *testing.T) {
	st := openTestStore(t)
	dir := t.TempDir()
	changedFile := filepath.Join(dir, "internal", "auth", "empty.go")

	// Graph has no node for changedFile — simulates an empty or non-Go file.
	g := graph.New("proj-empty")
	g.SetRoot(dir)
	w, err := New(g, parser.NewWalker(), st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, _ = st.ClaimWork("guard-agent", "internal/auth", "directory", 30)

	// Scope probe should still fire even if changedNodes is empty.
	w.notifyIntraProjectImpact(changedFile)

	msgs, _, err := st.GetMessages("guard-agent", 0, "scope_change_alert", false, 50)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Error("expected scope_change_alert even when file has no parsed nodes")
	}
}

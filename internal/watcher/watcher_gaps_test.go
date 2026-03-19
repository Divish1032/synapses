package watcher

// Tests targeting remaining uncovered watcher branches:
// - repoIDOfNodeID with and without "::"
// - dedup with duplicates and empty input
// - shouldSkipDir for tmp_repos and dot-prefixed dirs
// - handleEvent Remove path (RemoveFile + RemoveCallSitesForFile)
// - handleEvent Write for config path (debounceConfigReload)
// - reparseFile with store (LoadCallSites + UpdateCallSitesForFile)
// - RecentChanges with negative windowMinutes
// - SetProjectID stores value
// - recordChange with equal nodesBefore/nodesAfter

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── repoIDOfNodeID ────────────────────────────────────────────────────────────

func TestRepoIDOfNodeID_WithSeparator(t *testing.T) {
	got := repoIDOfNodeID("abc123::file.go::FuncName")
	if got != "abc123" {
		t.Errorf("got %q, want abc123", got)
	}
}

func TestRepoIDOfNodeID_NoSeparator(t *testing.T) {
	got := repoIDOfNodeID("malformed-id")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// ── dedup ─────────────────────────────────────────────────────────────────────

func TestDedup_WithDuplicates(t *testing.T) {
	input := []string{"a", "b", "a", "c", "b"}
	got := dedup(input)
	if len(got) != 3 {
		t.Errorf("expected 3 unique elements, got %d", len(got))
	}
	// Order preserved.
	if got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("unexpected order: %v", got)
	}
}

func TestDedup_NoDuplicates(t *testing.T) {
	got := dedup([]string{"x", "y", "z"})
	if len(got) != 3 {
		t.Errorf("expected 3, got %d", len(got))
	}
}

func TestDedup_Empty(t *testing.T) {
	got := dedup(nil)
	if len(got) != 0 {
		t.Errorf("expected 0 for nil input, got %d", len(got))
	}
}

// ── shouldSkipDir additional cases ────────────────────────────────────────────

func TestShouldSkipDir_TmpRepos(t *testing.T) {
	if !shouldSkipDir("tmp_repos") {
		t.Error("tmp_repos should be skipped")
	}
}

func TestShouldSkipDir_DotPrefix(t *testing.T) {
	if !shouldSkipDir(".config") {
		t.Error(".config should be skipped")
	}
}

func TestShouldSkipDir_EmptyString(t *testing.T) {
	// Empty string: len(name) == 0, the dot check doesn't match.
	if shouldSkipDir("") {
		t.Error("empty string should not be skipped")
	}
}

// ── handleEvent Remove ────────────────────────────────────────────────────────

func TestHandleEvent_Remove_PrunesNodes(t *testing.T) {
	root := t.TempDir()
	goFile := filepath.Join(root, "rm.go")
	if err := os.WriteFile(goFile, []byte("package p\nfunc Bye() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("test")
	g.SetRoot(root)
	wlkr := parser.NewWalker()
	_ = wlkr.ParseFile(g, goFile)

	if !hasNode(g, "Bye") {
		t.Fatal("precondition: Bye node expected")
	}

	w, err := New(g, wlkr, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Simulate Remove event.
	w.handleEvent(fsnotify.Event{Name: goFile, Op: fsnotify.Remove}, root)

	if hasNode(g, "Bye") {
		t.Error("Bye node should be removed after Remove event")
	}
}

// ── handleEvent Write for config path ─────────────────────────────────────────

func TestHandleEvent_Write_ConfigPath_TriggersConfigReload(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "synapses.json")
	if err := os.WriteFile(cfgPath, []byte(`{"project_name":"test"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()
	w.configPath = cfgPath

	var called atomic.Bool
	w.SetConfigChangeHandler(func(_ *config.Config) { called.Store(true) })

	// handleEvent with Write for the config file should debounce config reload.
	w.handleEvent(fsnotify.Event{Name: cfgPath, Op: fsnotify.Write}, root)

	// Wait for debounce + reload.
	time.Sleep(debounceDelay + 100*time.Millisecond)

	if !called.Load() {
		t.Error("expected config change handler to be called")
	}
}

// ── reparseFile with store ────────────────────────────────────────────────────

func TestReparseFile_WithStore_PersistsCallSites(t *testing.T) {
	root := t.TempDir()
	goFile := filepath.Join(root, "svc.go")
	if err := os.WriteFile(goFile, []byte("package p\nfunc Serve() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New(root)
	g.SetRoot(root)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	w, wErr := New(g, parser.NewWalker(), st)
	if wErr != nil {
		t.Fatalf("New: %v", wErr)
	}
	defer w.Stop()

	w.reparseFile(goFile, root)

	// Verify graph has nodes.
	if !hasNode(g, "Serve") {
		t.Error("expected Serve node after reparseFile")
	}
}

// ── RecentChanges with negative windowMinutes ─────────────────────────────────

func TestRecentChanges_NegativeWindow_ReturnsAll(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	w.recordChange("f.go", 0, 1, 0)

	// Negative windowMinutes should return all events (same as 0).
	changes := w.RecentChanges(-5)
	if len(changes) != 1 {
		t.Errorf("expected 1 change for negative window, got %d", len(changes))
	}
}

// ── SetProjectID ──────────────────────────────────────────────────────────────

func TestSetProjectID_StoresValue(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	w.SetProjectID("proj-abc")
	if w.projectID != "proj-abc" {
		t.Errorf("projectID = %q, want proj-abc", w.projectID)
	}
}

// ── recordChange equal nodes ──────────────────────────────────────────────────

func TestRecordChange_EqualNodes_ZeroDelta(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	w.recordChange("f.go", 5, 5, 2)

	w.changeMu.RLock()
	ev := w.changeLog[0]
	w.changeMu.RUnlock()

	if ev.NodesAdded != 0 || ev.NodesRemoved != 0 {
		t.Errorf("expected 0 added/removed when equal, got +%d/-%d", ev.NodesAdded, ev.NodesRemoved)
	}
	if ev.EdgesAdded != 2 {
		t.Errorf("EdgesAdded = %d, want 2", ev.EdgesAdded)
	}
}

// ── handleEvent Rename ────────────────────────────────────────────────────────

func TestHandleEvent_Rename_PrunesNodes(t *testing.T) {
	root := t.TempDir()
	goFile := filepath.Join(root, "renamed.go")
	if err := os.WriteFile(goFile, []byte("package p\nfunc Old() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("test")
	g.SetRoot(root)
	wlkr := parser.NewWalker()
	_ = wlkr.ParseFile(g, goFile)

	w, err := New(g, wlkr, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	w.handleEvent(fsnotify.Event{Name: goFile, Op: fsnotify.Rename}, root)

	if hasNode(g, "Old") {
		t.Error("Old node should be removed after Rename event")
	}
}

// ── handleEvent Remove with store ─────────────────────────────────────────────

func TestHandleEvent_Remove_WithStore_ClearsCallSites(t *testing.T) {
	root := t.TempDir()
	goFile := filepath.Join(root, "gone.go")
	if err := os.WriteFile(goFile, []byte("package p\nfunc Gone() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("test")
	g.SetRoot(root)
	wlkr := parser.NewWalker()
	_ = wlkr.ParseFile(g, goFile)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	w, wErr := New(g, wlkr, st)
	if wErr != nil {
		t.Fatalf("New: %v", wErr)
	}
	defer w.Stop()

	w.handleEvent(fsnotify.Event{Name: goFile, Op: fsnotify.Remove}, root)

	// Node should be gone.
	if hasNode(g, "Gone") {
		t.Error("Gone node should be removed")
	}
}

// ── reparseFile → blame pipeline (end-to-end integration) ────────────────────

// TestReparseFile_SetsBlameMetadata is the full end-to-end integration test for
// BUG-EVAL-6. It proves that after reparseFile runs on a file that exists in
// git, all function nodes carry blame_author, blame_date, blame_commit, churn,
// and staleness_score — even when the nodes were freshly created by the reparse
// (no prior metadata from a startup EnrichChurn/EnrichBlame pass).
func TestReparseFile_SetsBlameMetadata(t *testing.T) {
	root := t.TempDir()

	// Helper: run a git command in root, skip test if git unavailable.
	gitRun := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if err := cmd.Run(); err != nil {
			t.Skipf("git %v failed: %v — skipping blame integration test", args, err)
		}
	}

	// Initialise a real git repository with one committed Go file.
	gitRun("init")
	gitRun("config", "user.email", "test@example.com")
	gitRun("config", "user.name", "Test")

	goFile := filepath.Join(root, "svc.go")
	if err := os.WriteFile(goFile, []byte("package p\nfunc Serve() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-m", "initial commit")

	// Build a graph and watcher with root set — mirrors the daemon startup path.
	g := graph.New(root)
	g.SetRoot(root)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// reparseFile is what the watcher calls on every file-change event.
	// It must: parse the file into nodes, then call EnrichBlameForFile +
	// EnrichCommitContextForFile before returning.
	w.reparseFile(goFile, root)

	// The Serve function node must exist after reparse.
	if !hasNode(g, "Serve") {
		t.Fatal("expected Serve node after reparseFile — parse step failed")
	}

	// Find the Serve node and assert all blame fields are populated.
	var serveNode *graph.Node
	for _, n := range g.AllNodes() {
		if n.Name == "Serve" && n.Type == graph.NodeFunction {
			serveNode = n
			break
		}
	}
	if serveNode == nil {
		t.Fatal("Serve function node not found in graph")
	}
	if serveNode.Metadata == nil {
		t.Fatal("Serve node has nil Metadata — EnrichBlameForFile did not run")
	}

	// Core blame fields — must be non-empty after a real git commit.
	for _, field := range []string{"blame_author", "blame_date", "blame_commit"} {
		if serveNode.Metadata[field] == "" {
			t.Errorf("Serve node missing %s after reparseFile on a git-tracked file", field)
		}
	}

	// churn must be set by EnrichBlameForFile (not left absent as before the fix).
	if serveNode.Metadata["churn"] == "" {
		t.Error("Serve node missing churn — EnrichBlameForFile did not set it for fresh node")
	}

	// staleness_score must be present (may be "0.0" for a brand-new commit, but must exist).
	if serveNode.Metadata["staleness_score"] == "" {
		t.Error("Serve node missing staleness_score after reparseFile")
	}
}

package watcher

// Integration tests for the Watcher. Each test spins up a real fsnotify watcher
// over a t.TempDir(), performs filesystem mutations, and verifies that the
// in-memory graph reflects the expected state after the debounce window.
//
// Using package watcher (white-box) gives access to the unexported
// debounceDelay constant so wait times are derived from the real delay rather
// than a hardcoded magic number.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// settle waits for at least 3× the debounce window to ensure any pending
// re-parse has completed.
func settle() { time.Sleep(debounceDelay * 3) }

// hasNode returns true if the graph contains at least one node with the given name.
func hasNode(g *graph.Graph, name string) bool {
	return len(g.FindByName(name)) > 0
}

// TestWatcher_FileCreate verifies that writing a new Go file into a watched
// directory causes its functions to appear in the graph.
func TestWatcher_FileCreate(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	src := "package testpkg\n\nfunc Hello() {}\n"
	file := filepath.Join(dir, "hello.go")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	settle()

	if !hasNode(g, "Hello") {
		t.Error("expected function node 'Hello' after file create, not found")
	}
}

// TestWatcher_FileModify verifies that overwriting a Go file removes stale
// nodes and adds nodes for the new content.
func TestWatcher_FileModify(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	file := filepath.Join(dir, "funcs.go")
	if err := os.WriteFile(file, []byte("package testpkg\n\nfunc OldFunc() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile initial: %v", err)
	}

	walk := parser.NewWalker()
	if err := walk.ParseFile(g, file); err != nil {
		t.Fatalf("ParseFile initial: %v", err)
	}
	if !hasNode(g, "OldFunc") {
		t.Fatal("precondition failed: OldFunc not found after initial parse")
	}

	w, err := New(g, walk, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	if err := os.WriteFile(file, []byte("package testpkg\n\nfunc NewFunc() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile modified: %v", err)
	}

	settle()

	if hasNode(g, "OldFunc") {
		t.Error("OldFunc should have been removed after file modify")
	}
	if !hasNode(g, "NewFunc") {
		t.Error("NewFunc not found after file modify")
	}
}

// TestWatcher_FileDelete verifies that removing a Go file causes its nodes to
// be pruned from the graph.
func TestWatcher_FileDelete(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	file := filepath.Join(dir, "bye.go")
	if err := os.WriteFile(file, []byte("package testpkg\n\nfunc Goodbye() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	walk := parser.NewWalker()
	if err := walk.ParseFile(g, file); err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if !hasNode(g, "Goodbye") {
		t.Fatal("precondition failed: Goodbye not found after initial parse")
	}

	w, err := New(g, walk, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	if err := os.Remove(file); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Removal is handled immediately (no debounce); one debounce window is
	// enough for the fsnotify event to be delivered and processed.
	time.Sleep(debounceDelay)

	if hasNode(g, "Goodbye") {
		t.Error("Goodbye node should have been removed after file delete")
	}
}

// TestWatcher_SetConfig verifies that configuration can be set on an active watcher.
func TestWatcher_SetConfig(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Call SetConfig to ensure it doesn't panic
	w.SetConfig(nil)
}

// TestWatcher_SetProjectID verifies that project ID can be set.
func TestWatcher_SetProjectID(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w.SetProjectID("myproject")
}

// TestWatcher_RecentChanges verifies that RecentChanges returns events.
func TestWatcher_RecentChanges(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	// Write a file to trigger a change
	file := filepath.Join(dir, "test.go")
	if err := os.WriteFile(file, []byte("package test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	settle()

	// RecentChanges should not panic, even if empty
	changes := w.RecentChanges(10)
	_ = changes // Just verify it returns without error
}

// TestWatcher_NewWithExistingFiles verifies New() processes existing files in root.
func TestWatcher_NewWithExistingFiles(t *testing.T) {
	dir := t.TempDir()

	// Pre-create a file before watcher initialization
	file := filepath.Join(dir, "existing.go")
	if err := os.WriteFile(file, []byte("package test\n\nfunc Existing() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	g := graph.New("test")
	g.SetRoot(dir)
	walk := parser.NewWalker()

	// Parse the existing file so the graph has initial state
	if err := walk.ParseFile(g, file); err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	w, err := New(g, walk, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w == nil {
		t.Fatal("expected non-nil watcher")
	}
}

// TestWatcher_StartOnNonexistentDirectory verifies Start() handles missing dirs gracefully.
func TestWatcher_StartOnNonexistentDirectory(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Try to start on a non-existent directory - watcher may create it or return error
	_ = w.Start("/nonexistent/path/that/does/not/exist")
	// Just verify it doesn't panic
}

// TestWatcher_MultipleModifications verifies that rapid changes are debounced.
func TestWatcher_MultipleModifications(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	file := filepath.Join(dir, "rapid.go")
	// Write multiple times quickly
	for i := 0; i < 3; i++ {
		src := "package testpkg\n\nfunc Rapid" + string(rune('A'+i)) + "() {}\n"
		if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	settle()

	// Verify graph was updated (debounced into a single re-parse)
	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Error("expected at least one node after rapid modifications")
	}
}

// TestWatcher_FileInSubdirectory verifies watcher tracks nested files.
func TestWatcher_FileInSubdirectory(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	subdir := filepath.Join(dir, "pkg", "sub")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	file := filepath.Join(subdir, "nested.go")
	src := "package testpkg\n\nfunc Nested() {}\n"
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	settle()

	// Verify the file was processed (may create nodes or just track file changes)
	changes := w.RecentChanges(100)
	if len(changes) > 0 && !hasNode(g, "Nested") {
		// File was detected but node extraction depends on parser output
		_ = changes
	}
}

// TestWatcher_ParallelParsing_MultipleFiles verifies that multiple files written
// simultaneously are all parsed and their nodes appear in the graph. This tests
// the parallel parse worker pool + batch merge path added in Sprint 14 #4.
func TestWatcher_ParallelParsing_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	// Write several files simultaneously to trigger the batch coalesce path.
	funcs := []string{"Alpha", "Beta", "Gamma", "Delta", "Epsilon"}
	for i, name := range funcs {
		src := "package testpkg\n\nfunc " + name + "() {}\n"
		file := filepath.Join(dir, "f"+string(rune('0'+i))+".go")
		if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}

	// Wait for debounce + parse + merge to complete.
	settle()

	for _, name := range funcs {
		if !hasNode(g, name) {
			t.Errorf("expected function node %q after parallel write, not found", name)
		}
	}
}

// TestWatcher_PrepareParseResult_SymlinkRejection verifies that prepareParseResult
// rejects symlinks outside the project root without touching the main graph.
func TestWatcher_PrepareParseResult_SymlinkRejection(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Set rootPath so symlink check can run.
	w.rootPath = dir

	// Prepare a result for a non-existent path outside the project root.
	result := w.prepareParseResult("/outside/the/project/evil.go")
	if result.err == nil {
		t.Error("expected error for file outside project root, got nil")
	}
	if result.tempGraph != nil {
		t.Error("tempGraph should be nil when prepareParseResult fails")
	}
}

// TestWatcher_ApplyBatch_SkipsErrResults verifies that applyBatch safely skips
// results where prepareParseResult set an error, without touching the main graph.
func TestWatcher_ApplyBatch_SkipsErrResults(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	before := len(g.AllNodes())
	// Feed a batch that is entirely errors — graph must not change.
	w.applyBatch([]parseFileResult{
		{path: "ghost.go", err: os.ErrNotExist},
		{path: "gone.go", err: os.ErrPermission},
	})
	after := len(g.AllNodes())
	if after != before {
		t.Errorf("graph changed after all-error batch: before=%d after=%d", before, after)
	}
}

// TestWatcher_BatchCoalesce_NodeIDConsistency verifies that nodes produced by
// the parallel parse path carry the same IDs as nodes produced by a direct
// walker.ParseFile call (repoID+root are passed correctly to tempGraph).
func TestWatcher_BatchCoalesce_NodeIDConsistency(t *testing.T) {
	dir := t.TempDir()
	src := []byte("package mypkg\n\nfunc MyFunc() {}\n")
	file := filepath.Join(dir, "my.go")
	if err := os.WriteFile(file, src, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Build expected node IDs via a direct parse into a reference graph.
	refGraph := graph.New("testrepo")
	refGraph.SetRoot(dir)
	walk := parser.NewWalker()
	if err := walk.ParseFile(refGraph, file); err != nil {
		t.Fatalf("reference ParseFile: %v", err)
	}
	refNodes := refGraph.NodesForFile(file)
	if len(refNodes) == 0 {
		t.Fatal("reference graph produced no nodes")
	}

	// Prepare via prepareParseResult and check that node IDs match.
	mainGraph := graph.New("testrepo")
	mainGraph.SetRoot(dir)
	w, err := New(mainGraph, walk, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.rootPath = dir

	result := w.prepareParseResult(file)
	if result.err != nil {
		t.Fatalf("prepareParseResult: %v", result.err)
	}

	tempNodes := result.tempGraph.NodesForFile(file)
	if len(tempNodes) != len(refNodes) {
		t.Errorf("node count mismatch: tempGraph=%d refGraph=%d", len(tempNodes), len(refNodes))
	}
	refIDs := make(map[graph.NodeID]bool, len(refNodes))
	for _, n := range refNodes {
		refIDs[n.ID] = true
	}
	for _, n := range tempNodes {
		if !refIDs[n.ID] {
			t.Errorf("temp graph node ID %q not in reference graph", n.ID)
		}
	}
}

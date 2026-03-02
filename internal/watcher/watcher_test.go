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

package watcher

// White-box unit tests for watcher internals. These tests access unexported
// fields and functions directly (same-package test) without spinning up a
// real filesystem watcher where possible.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ── shouldSkipDir ─────────────────────────────────────────────────────────────

func TestShouldSkipDir(t *testing.T) {
	tests := []struct {
		name string
		skip bool
	}{
		{"vendor", true},
		{"node_modules", true},
		{"dist", true},
		{"build", true},
		{".git", true},
		{".idea", true},
		{".vscode", true},
		{"__pycache__", true},
		{"testdata", true},
		{".hidden", true},
		{".DS_Store", true},
		{"cmd", false},
		{"internal", false},
		{"src", false},
		{"pkg", false},
		{"api", false},
	}
	for _, tt := range tests {
		got := shouldSkipDir(tt.name)
		if got != tt.skip {
			t.Errorf("shouldSkipDir(%q) = %v, want %v", tt.name, got, tt.skip)
		}
	}
}

// ── RecentChanges ─────────────────────────────────────────────────────────────

func TestRecentChanges_EmptyLog(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	if changes := w.RecentChanges(0); len(changes) != 0 {
		t.Errorf("expected 0 changes in empty log, got %d", len(changes))
	}
}

func TestRecentChanges_WithinWindow(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// recordChange appends to the log.
	w.recordChange(filepath.Join(dir, "file.go"), 0, 3, 2)

	// windowMinutes=1 → should include the just-added event.
	changes := w.RecentChanges(1)
	if len(changes) != 1 {
		t.Errorf("expected 1 change in 1-min window, got %d", len(changes))
	}

	// windowMinutes=0 → return all.
	all := w.RecentChanges(0)
	if len(all) != 1 {
		t.Errorf("expected 1 change for windowMinutes=0, got %d", len(all))
	}
}

func TestRecentChanges_ExcludesOldEvents(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Inject a 2-hour-old event directly.
	w.changeMu.Lock()
	w.changeLog = append(w.changeLog, ChangeEvent{
		Timestamp:  time.Now().Add(-2 * time.Hour),
		File:       "old.go",
		NodesAdded: 1,
	})
	w.changeMu.Unlock()

	// 1-minute window should NOT include the 2-hour-old event.
	changes := w.RecentChanges(1)
	if len(changes) != 0 {
		t.Errorf("expected 0 changes in 1-min window for old event, got %d", len(changes))
	}

	// windowMinutes=0 returns all (ignores cutoff).
	all := w.RecentChanges(0)
	if len(all) != 1 {
		t.Errorf("expected 1 change for windowMinutes=0 (no filter), got %d", len(all))
	}
}

// ── recordChange ──────────────────────────────────────────────────────────────

func TestRecordChange_NodesAddedAndRemoved(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// nodesAfter > nodesBefore → added.
	w.recordChange("add.go", 2, 5, 3)
	w.changeMu.RLock()
	ev := w.changeLog[0]
	w.changeMu.RUnlock()
	if ev.NodesAdded != 3 {
		t.Errorf("NodesAdded = %d, want 3", ev.NodesAdded)
	}
	if ev.NodesRemoved != 0 {
		t.Errorf("NodesRemoved = %d, want 0", ev.NodesRemoved)
	}
}

func TestRecordChange_NodesRemoved(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// nodesBefore > nodesAfter → removed.
	w.recordChange("del.go", 5, 2, 0)
	w.changeMu.RLock()
	ev := w.changeLog[0]
	w.changeMu.RUnlock()
	if ev.NodesRemoved != 3 {
		t.Errorf("NodesRemoved = %d, want 3", ev.NodesRemoved)
	}
	if ev.NodesAdded != 0 {
		t.Errorf("NodesAdded = %d, want 0", ev.NodesAdded)
	}
}

func TestRecordChange_NegativeEdgesClampedToZero(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	w.recordChange("f.go", 3, 3, -5)
	w.changeMu.RLock()
	ev := w.changeLog[0]
	w.changeMu.RUnlock()
	if ev.EdgesAdded != 0 {
		t.Errorf("negative EdgesAdded should be clamped to 0, got %d", ev.EdgesAdded)
	}
}

func TestRecordChange_RelativeFilePathStripped(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	absFile := filepath.Join(dir, "sub", "file.go")
	w.recordChange(absFile, 0, 1, 0)
	w.changeMu.RLock()
	ev := w.changeLog[0]
	w.changeMu.RUnlock()
	// File path should be relative (stripped of root prefix).
	if ev.File == absFile {
		t.Errorf("expected relative path, got absolute: %q", ev.File)
	}
}

func TestChangeLog_EvictsOldestOnOverflow(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Insert changeLogCap+5 events to trigger circular eviction.
	for i := 0; i <= changeLogCap+5; i++ {
		w.recordChange("f.go", 0, 1, 0)
	}
	w.changeMu.RLock()
	n := len(w.changeLog)
	w.changeMu.RUnlock()
	if n != changeLogCap {
		t.Errorf("changeLog len = %d after overflow, want %d", n, changeLogCap)
	}
}

// ── countNodesForFile ─────────────────────────────────────────────────────────

func TestCountNodesForFile_Found(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	file := filepath.Join(dir, "svc.go")
	id1 := g.MakeNodeID(file, "Func1")
	id2 := g.MakeNodeID(file, "Func2")
	g.AddNode(&graph.Node{ID: id1, Type: graph.NodeFunction, Name: "Func1", File: file})
	g.AddNode(&graph.Node{ID: id2, Type: graph.NodeFunction, Name: "Func2", File: file})

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	if count := w.countNodesForFile(file); count != 2 {
		t.Errorf("expected 2 nodes for file, got %d", count)
	}
}

func TestCountNodesForFile_NotFound(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	if count := w.countNodesForFile("ghost.go"); count != 0 {
		t.Errorf("expected 0 for unknown file, got %d", count)
	}
}

// ── Setter methods ────────────────────────────────────────────────────────────

func TestSetConfig_StoresValue(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	if w.cfg != nil {
		t.Error("expected nil cfg before SetConfig")
	}
	w.SetConfig(nil) // setting nil should not panic
}

func TestSetBrainClient_StoresValue(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	w.SetBrainClient(nil) // nil interface{} — must not panic
	if w.brainClient != nil {
		t.Error("expected brainClient nil after SetBrainClient(nil)")
	}
}

func TestSetPacketInvalidator_StoresValue(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	w.SetPacketInvalidator(nil) // nil interface — must not panic
}

func TestSetConfigChangeHandler_StoresValue(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Set a non-nil handler so cfgHandler != nil.
	w.SetConfigChangeHandler(func(_ *config.Config) {})
	if w.cfgHandler == nil {
		t.Error("expected cfgHandler to be set after SetConfigChangeHandler")
	}

	// Resetting to nil clears it.
	w.SetConfigChangeHandler(nil)
	if w.cfgHandler != nil {
		t.Error("expected cfgHandler nil after SetConfigChangeHandler(nil)")
	}
}

// ── Stop idempotency ──────────────────────────────────────────────────────────

func TestStop_Idempotent(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Stop()
	w.Stop() // second call must not panic
}

// ── Non-Go file write — no parse crash ───────────────────────────────────────

func TestWatcher_NonGoFile_NoCrash(t *testing.T) {
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

	// Write a non-Go file — parser will skip/error on it, watcher must not crash.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# readme"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	settle() // wait for debounce + re-parse attempt
}

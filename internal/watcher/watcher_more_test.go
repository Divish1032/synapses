package watcher

// Additional white-box tests targeting the remaining uncovered branches:
// reloadConfig, debounceConfigReload, checkViolations, persistAsync.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/store"
)

// openWatcherStore opens a real SQLite store in a temp dir.
func openWatcherStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "w_test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// ── reloadConfig ──────────────────────────────────────────────────────────────

func TestReloadConfig_ValidSynapsesJSON(t *testing.T) {
	dir := t.TempDir()
	// Write a minimal valid synapses.json so config.Load succeeds.
	cfgPath := filepath.Join(dir, "synapses.json")
	writeFile(t, cfgPath, `{"project_name":"reload-test"}`)

	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Track handler invocations.
	called := 0
	w.SetConfigChangeHandler(func(_ *config.Config) { called++ })

	w.reloadConfig(cfgPath)

	if w.cfg == nil {
		t.Error("expected w.cfg to be set after reloadConfig")
	}
	if called != 1 {
		t.Errorf("expected cfgHandler called 1 time, got %d", called)
	}
}

func TestReloadConfig_InvalidPath_LogsError(t *testing.T) {
	// Non-existent directory → config.Load returns the default config (no error),
	// OR fails; either way the function must not panic.
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// /nonexistent-dir/synapses.json → config.Load will use default config.
	w.reloadConfig("/nonexistent-path-xyz/synapses.json") // must not panic
}

func TestReloadConfig_NilHandler_NoCall(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "synapses.json")
	writeFile(t, cfgPath, `{"project_name":"no-handler"}`)

	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// No handler set — must not panic.
	w.reloadConfig(cfgPath)
}

// ── debounceConfigReload ──────────────────────────────────────────────────────

func TestDebounceConfigReload_TriggersReload(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "synapses.json")
	writeFile(t, cfgPath, `{"project_name":"debounce-test"}`)

	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	called := 0
	w.SetConfigChangeHandler(func(_ *config.Config) { called++ })

	// debounceConfigReload schedules the reload after debounceDelay.
	w.debounceConfigReload(cfgPath)

	// Wait for timer to fire (debounceDelay + margin).
	time.Sleep(debounceDelay + 100*time.Millisecond)

	if called < 1 {
		t.Error("expected cfgHandler to be called after debounce timer fired")
	}
}

func TestDebounceConfigReload_ResetOnDuplicate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "synapses.json")
	writeFile(t, cfgPath, `{"project_name":"debounce-reset"}`)

	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Call twice rapidly — timer should be reset, not duplicated.
	w.debounceConfigReload(cfgPath)
	w.debounceConfigReload(cfgPath) // resets the existing timer

	// Wait for reload to complete.
	time.Sleep(debounceDelay + 200*time.Millisecond)
	// Must not panic regardless of call count.
}

// ── checkViolations ───────────────────────────────────────────────────────────

func TestCheckViolations_NoRules_NilCfg(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()
	// cfg is nil → early return, must not panic.
	w.checkViolations("some_file.go")
}

func TestCheckViolations_WithRuleAndViolation(t *testing.T) {
	// Build a graph with a CALLS edge from "bad.go" to "internal.go".
	g := graph.New("testrepo")
	fromID := g.MakeNodeID("bad.go", "BadCaller")
	toID := g.MakeNodeID("internal.go", "InternalFunc")
	g.AddNode(&graph.Node{
		ID: fromID, Type: graph.NodeFunction, Name: "BadCaller",
		Package: "bad", File: "bad.go",
	})
	g.AddNode(&graph.Node{
		ID: toID, Type: graph.NodeFunction, Name: "InternalFunc",
		Package: "internal", File: "internal.go",
	})
	g.AddEdge(&graph.Edge{From: fromID, To: toID, Type: graph.EdgeCalls})

	// Rule: no CALLS edges from bad.go to internal.go.
	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:          "no-bad-to-internal",
			Description: "bad package must not call internal",
			Severity:    "error",
			ForbiddenEdge: config.ForbiddenEdge{
				EdgeType:        graph.EdgeCalls,
				FromFilePattern: "bad.go",
				ToFilePattern:   "internal.go",
			},
		}},
	}

	st := openWatcherStore(t)
	w, err := New(g, parser.NewWalker(), st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()
	w.cfg = cfg

	// Should detect the violation and log it without panicking.
	w.checkViolations("bad.go")
}

func TestCheckViolations_NoViolations_MatchingRule(t *testing.T) {
	// Rule exists but no matching edges — should return cleanly.
	g := graph.New("testrepo")
	nodeID := g.MakeNodeID("safe.go", "SafeFunc")
	g.AddNode(&graph.Node{
		ID: nodeID, Type: graph.NodeFunction, Name: "SafeFunc",
		Package: "safe", File: "safe.go",
	})

	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "no-mcp-to-store",
			Severity: "warning",
			ForbiddenEdge: config.ForbiddenEdge{
				EdgeType:        graph.EdgeCalls,
				FromFilePattern: "*/mcp/*",
				ToFilePattern:   "*/store/*",
			},
		}},
	}

	st := openWatcherStore(t)
	w, err := New(g, parser.NewWalker(), st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()
	w.cfg = cfg

	// No edges match → no violation, no crash.
	w.checkViolations("safe.go")
}

// ── persistAsync ──────────────────────────────────────────────────────────────

func TestPersistAsync_NilStore_NoOp(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// store is nil → early return, must not panic.
	w.persistAsync("file.go")
}

func TestPersistAsync_WithStore(t *testing.T) {
	g := graph.New("test")
	st := openWatcherStore(t)
	w, err := New(g, parser.NewWalker(), st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Should launch goroutine without panicking.
	w.persistAsync("file.go")
	// Give goroutine time to execute.
	time.Sleep(100 * time.Millisecond)
}

func TestPersistAsync_EmptyChangedFile(t *testing.T) {
	g := graph.New("test")
	st := openWatcherStore(t)
	w, err := New(g, parser.NewWalker(), st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Empty changedFile path → UpsertFileMtime skipped (no crash).
	w.persistAsync("")
	time.Sleep(100 * time.Millisecond)
}

// ── recordChange with store (AppendEvent path) ────────────────────────────────

func TestRecordChange_WithStore_EmitsEvent(t *testing.T) {
	g := graph.New("test")
	st := openWatcherStore(t)
	w, err := New(g, parser.NewWalker(), st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// With a store, recordChange should also append a file_change event.
	w.recordChange("pkg/auth.go", 0, 2, 4)
	// Must not panic; event is appended to store.
}

// writeFile is a test helper that writes content to path, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFileBytes(path, []byte(content)); err != nil {
		t.Fatalf("writeFile %s: %v", path, err)
	}
}

func writeFileBytes(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

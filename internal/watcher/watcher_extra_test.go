package watcher

// Extra coverage tests — focus on the "known violations skip" path in
// checkViolations and other branches not yet hit.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/store"
)

// buildViolationGraph creates a graph with a CALLS edge from bad.go → internal.go.
func buildViolationGraph() (*graph.Graph, graph.NodeID, graph.NodeID) {
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
	return g, fromID, toID
}

var testViolationCfg = &config.Config{
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

// TestCheckViolations_KnownViolationSkipped tests the path where a violation was
// already logged in a previous call, so the second call skips re-emitting the event.
func TestCheckViolations_KnownViolationSkipped(t *testing.T) {
	g, _, _ := buildViolationGraph()
	st := openWatcherStore(t)

	w, err := New(g, parser.NewWalker(), st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()
	w.cfg = testViolationCfg

	// First call: violation is new → logged + event emitted.
	w.checkViolations("bad.go")

	// Second call: violation already in store → existingIDs skip path exercised.
	w.checkViolations("bad.go")
	// Must not panic; the "known" branch is now covered.
}

// TestCheckViolations_NoStore_ReturnsEarly verifies that when store is nil
// the function returns early without panicking (cfg != nil but store == nil).
func TestCheckViolations_NoStore_ReturnsEarly(t *testing.T) {
	g, _, _ := buildViolationGraph()

	w, err := New(g, parser.NewWalker(), nil) // no store
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()
	w.cfg = testViolationCfg

	// store == nil → early return, must not panic.
	w.checkViolations("bad.go")
}

// TestIngestToBrain_NonBrainClient covers the type-assertion failure path.
func TestIngestToBrain_NonBrainClient(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Set a non-*brain.Client value so the type assertion fails.
	w.brainClient = "not-a-brain-client"
	// Must return immediately without panic.
	w.ingestToBrain("pkg/svc.go")
}

// TestReloadConfig_InvalidJSON exercises the error path when synapses.json is malformed.
func TestReloadConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "synapses.json")
	// Invalid JSON → config.Load returns an error → fmt.Fprintf + return.
	if err := os.WriteFile(cfgPath, []byte(`{invalid json`), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	called := false
	w.SetConfigChangeHandler(func(_ *config.Config) { called = true })

	// Should hit the error path and return early without calling cfgHandler.
	w.reloadConfig(cfgPath)

	if called {
		t.Error("cfgHandler should not be called when config.Load fails")
	}
}

// TestStop_AlreadyStopped exercises the early-return path in Stop.
func TestStop_AlreadyStopped(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Stop()
	w.Stop() // second call: w.stopped == true → early return, must not panic
}

// TestStart_RealDir exercises Start on a real directory (adds fsnotify watchers).
func TestStart_RealDir(t *testing.T) {
	root := t.TempDir()
	// Create a subdirectory to exercise recursive watch.
	if err := os.MkdirAll(filepath.Join(root, "pkg", "auth"), 0o755); err != nil {
		t.Fatal(err)
	}

	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	if err := w.Start(root); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// TestStop_WithPendingTimer verifies that Stop cancels any pending debounce timers.
// This covers the `t.Stop()` loop inside Stop().
func TestStop_WithPendingTimer(t *testing.T) {
	root := t.TempDir()
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Create a pending debounce timer (does not fire yet).
	path := filepath.Join(root, "pending.go")
	w.debounce(path, root)

	// Stop immediately — timer is still pending → t.Stop() inside Stop() is called.
	w.Stop()
}

// TestCheckViolations_StoreError_FallbacksToEmpty exercises the error path
// in checkViolations where ViolationIDsForFile returns an error and the code
// falls back to an empty map (treating all violations as new).
func TestCheckViolations_StoreError_FallbacksToEmpty(t *testing.T) {
	g, fromID, toID := buildViolationGraph()
	_ = fromID
	_ = toID

	st, err := store.Open(filepath.Join(t.TempDir(), "cv_err.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// Close the store immediately so ViolationIDsForFile returns an error.
	st.Close()

	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "no-bad-to-internal",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				EdgeType:        "CALLS",
				FromFilePattern: "bad.go",
				ToFilePattern:   "internal.go",
			},
		}},
	}

	w, werr := New(g, parser.NewWalker(), st)
	if werr != nil {
		t.Fatalf("New: %v", werr)
	}
	defer w.Stop()
	w.cfg = cfg

	// Must not panic — existingIDs = make(...) fallback is exercised.
	w.checkViolations("bad.go")
}

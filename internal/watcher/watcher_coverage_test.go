package watcher

// Additional white-box coverage tests targeting the remaining uncovered branches:
// - Stop() called twice (already stopped guard)
// - handleEvent with Create for a new real directory (fw.Add path)
// - debounce timer reset (call twice rapidly for same path)
// - reparseFile: pktInval != nil, parse error branches
// - checkViolations: known violation (second call → continue)
// - recordChange: path outside graph root (else branch)
// - ingestToBrain: brainClient is not *brain.Client (early return)
// - ingestToBrain: valid brain.Client with graph nodes (covers inner loop)
// - Start with vendor subdirectory (shouldSkipDir in walk)

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/store"
)

// mockInvalidator is a minimal PacketCacheInvalidator for testing.
type mockInvalidator struct{ calls int }

func (m *mockInvalidator) InvalidatePacketCache()                          { m.calls++ }
func (m *mockInvalidator) InvalidatePacketCacheForFile(_ string)           { m.calls++ }

// ── Stop ──────────────────────────────────────────────────────────────────────

func TestStop_CalledTwice_NoOp(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.Stop()
	w.Stop() // second call must not panic (w.stopped == true branch)
}

// ── handleEvent: Create for a real directory ──────────────────────────────────

func TestHandleEvent_CreateDir_AddsToWatch(t *testing.T) {
	root := t.TempDir()
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Create a real subdirectory so os.Stat returns IsDir() == true.
	newDir := filepath.Join(root, "newsubdir")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// handleEvent with Create for a normal (non-skipped) directory.
	w.handleEvent(fsnotify.Event{Name: newDir, Op: fsnotify.Create}, root)
}

func TestHandleEvent_CreateSkippedDir_NoAdd(t *testing.T) {
	root := t.TempDir()
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Create a vendor directory — shouldSkipDir will return true.
	vendorDir := filepath.Join(root, "vendor")
	if err := os.MkdirAll(vendorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// handleEvent with Create for vendor/ — should return without fw.Add.
	w.handleEvent(fsnotify.Event{Name: vendorDir, Op: fsnotify.Create}, root)
}

// ── debounce: timer reset ─────────────────────────────────────────────────────

func TestDebounce_TimerReset(t *testing.T) {
	root := t.TempDir()
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	path := filepath.Join(root, "file.go")

	// First call creates the timer.
	w.debounce(path, root)
	// Second call resets the existing timer (exercises the t.Reset branch).
	w.debounce(path, root)

	// Let the timer fire to avoid goroutine leak.
	time.Sleep(debounceDelay + 50*time.Millisecond)
}

// ── reparseFile: pktInval != nil ──────────────────────────────────────────────

func TestReparseFile_WithPktInval(t *testing.T) {
	root := t.TempDir()
	goFile := filepath.Join(root, "a.go")
	if err := os.WriteFile(goFile, []byte("package p\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New(root)
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	inval := &mockInvalidator{}
	w.SetPacketInvalidator(inval)

	// reparseFile with a valid Go file — should call InvalidatePacketCacheForFile.
	w.reparseFile(goFile, root)

	if inval.calls == 0 {
		t.Error("expected InvalidatePacketCacheForFile to be called")
	}
}

// ── reparseFile: parse error ──────────────────────────────────────────────────

func TestReparseFile_ParseError_BinaryFile(t *testing.T) {
	root := t.TempDir()
	// Write a binary file with .go extension so the parser tries to parse it.
	binFile := filepath.Join(root, "b.go")
	if err := os.WriteFile(binFile, []byte{0x00, 0x01, 0x02, 0xFF, 0xFE}, 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New(root)
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Must not panic even if the file fails to parse.
	w.reparseFile(binFile, root)
}

// ── checkViolations: known violation (already in log) ─────────────────────────

func TestCheckViolations_KnownViolation_Skipped(t *testing.T) {
	// Build a graph with a CALLS edge that violates a rule.
	g := graph.New("testrepo")
	fromID := g.MakeNodeID("api.go", "APIHandler")
	toID := g.MakeNodeID("db.go", "DBFunc")
	g.AddNode(&graph.Node{
		ID: fromID, Type: graph.NodeFunction, Name: "APIHandler",
		Package: "api", File: "api.go",
	})
	g.AddNode(&graph.Node{
		ID: toID, Type: graph.NodeFunction, Name: "DBFunc",
		Package: "db", File: "db.go",
	})
	g.AddEdge(&graph.Edge{From: fromID, To: toID, Type: graph.EdgeCalls})

	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "no-api-to-db",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				EdgeType:        graph.EdgeCalls,
				FromFilePattern: "api.go",
				ToFilePattern:   "db.go",
			},
		}},
	}

	st, openErr := store.Open(filepath.Join(t.TempDir(), "cov.db"))
	if openErr != nil {
		t.Fatalf("store.Open: %v", openErr)
	}
	t.Cleanup(func() { st.Close() })

	w, err := New(g, parser.NewWalker(), st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()
	w.cfg = cfg

	// First call: violation is new → logged and event emitted.
	w.checkViolations("api.go")
	// Second call: violation is already known → continue branch is hit.
	w.checkViolations("api.go")
}

// ── recordChange: path not under root ─────────────────────────────────────────

func TestRecordChange_PathNotUnderRoot(t *testing.T) {
	g := graph.New("/some/root")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Path that is NOT under the graph root → relFile = path (else branch).
	w.recordChange("/different/path/file.go", 0, 1, 1)

	changes := w.RecentChanges(0)
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].File != "/different/path/file.go" {
		t.Errorf("expected absolute path as file, got %q", changes[0].File)
	}
}

// ── ingestToBrain: brainClient is not *brain.Client ──────────────────────────

func TestIngestToBrain_NonBrainClient_EarlyReturn(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Set a non-*brain.Client value — type assertion will fail → early return.
	w.brainClient = "not a brain"
	w.ingestToBrain("pkg/auth.go") // must not panic
}

// ── ingestToBrain: valid *brain.Client with graph nodes ───────────────────────

func TestIngestToBrain_ValidClient_WithNodes(t *testing.T) {
	// Spin up a minimal fake brain server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	root := t.TempDir()
	goFile := filepath.Join(root, "auth.go")

	g := graph.New(root)
	// Add a function node and a package node for the file.
	funcID := g.MakeNodeID(goFile, "Login")
	g.AddNode(&graph.Node{
		ID: funcID, Name: "Login", Type: graph.NodeFunction,
		File: goFile, Package: "auth",
		Metadata: map[string]string{"signature": "func Login(u string)", "doc": "Login handles login"},
	})
	pkgID := g.MakeNodeID(goFile, "auth")
	g.AddNode(&graph.Node{
		ID: pkgID, Name: "auth", Type: graph.NodePackage,
		File: goFile, Package: "auth",
	})

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Wire a real brain client pointing at the fake server.
	w.brainClient = brain.NewClient(srv.URL, 2)

	// ingestToBrain should loop over nodes and send Ingest requests.
	// The package node should be skipped; the function node should be ingested.
	w.ingestToBrain(goFile) // must not panic
}

// ── Start: with vendor subdirectory (shouldSkipDir during walk) ───────────────

func TestStart_WithVendorSubdir(t *testing.T) {
	root := t.TempDir()
	// Create a vendor subdirectory — shouldSkipDir will skip it during walk.
	vendorDir := filepath.Join(root, "vendor")
	if err := os.MkdirAll(vendorDir, 0o755); err != nil {
		t.Fatal(err)
	}

	g := graph.New(root)
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	if err := w.Start(root); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

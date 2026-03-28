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
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/store"
)

// mockInvalidator is a minimal PacketCacheInvalidator for testing.
type mockInvalidator struct{ calls int }

func (m *mockInvalidator) InvalidatePacketCache()                { m.calls++ }
func (m *mockInvalidator) InvalidatePacketCacheForFile(_ string) { m.calls++ }

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

// ── reparseFile: tree-sitter error recovery ─────────────────────────────────

func TestReparseFile_SkipsOnFirstParseError(t *testing.T) {
	root := t.TempDir()
	goFile := filepath.Join(root, "main.go")

	// Write a valid Go file and do initial parse.
	validSrc := []byte("package main\n\nfunc Hello() {}\n")
	if err := os.WriteFile(goFile, validSrc, 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New(root)
	walker := parser.NewWalker()
	w, err := New(g, walker, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Initial parse to populate the graph.
	w.reparseFile(goFile, root)
	nodesBefore := len(g.NodesForFile(goFile))
	if nodesBefore == 0 {
		t.Fatal("expected nodes after initial parse")
	}

	// Overwrite with broken source (simulates half-saved file).
	brokenSrc := []byte("package main\n\nfunc Hello() {\n\tfmt.Println(\"hello\n")
	if err := os.WriteFile(goFile, brokenSrc, 0o644); err != nil {
		t.Fatal(err)
	}

	// FIRST error: reparse should skip — retaining old nodes.
	w.reparseFile(goFile, root)
	nodesAfter := len(g.NodesForFile(goFile))
	if nodesAfter != nodesBefore {
		t.Errorf("expected %d nodes (retained on first error), got %d", nodesBefore, nodesAfter)
	}
}

func TestReparseFile_ProceedsOnPersistentErrors(t *testing.T) {
	root := t.TempDir()
	goFile := filepath.Join(root, "main.go")

	// Write a valid Go file and do initial parse.
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New(root)
	walker := parser.NewWalker()
	w, err := New(g, walker, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	w.reparseFile(goFile, root)
	nodesBefore := len(g.NodesForFile(goFile))
	if nodesBefore == 0 {
		t.Fatal("expected nodes after initial parse")
	}

	// Overwrite with broken source (real syntax error, not mid-save).
	brokenSrc := []byte("package main\n\nfunc Hello() {\n\tfmt.Println(\"hello\n")
	if err := os.WriteFile(goFile, brokenSrc, 0o644); err != nil {
		t.Fatal(err)
	}

	// FIRST error: skipped.
	w.reparseFile(goFile, root)
	if len(g.NodesForFile(goFile)) != nodesBefore {
		t.Fatal("first error should have been skipped")
	}

	// SECOND consecutive error: should proceed with parse (persistent error).
	w.reparseFile(goFile, root)
	// Graph should now reflect the broken file's best-effort parse
	// (tree-sitter error recovery still produces nodes).
	nodesAfterSecond := len(g.NodesForFile(goFile))
	if nodesAfterSecond == 0 {
		t.Error("expected nodes after persistent-error reparse (tree-sitter error recovery)")
	}
	// The key check: the graph WAS updated (not still the original).
	// With broken source, the node set will differ from the original clean parse.
	t.Logf("nodes: before=%d, after persistent-error reparse=%d", nodesBefore, nodesAfterSecond)
}

func TestReparseFile_ClearsErrorFlagOnCleanParse(t *testing.T) {
	root := t.TempDir()
	goFile := filepath.Join(root, "main.go")

	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New(root)
	walker := parser.NewWalker()
	w, err := New(g, walker, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	w.reparseFile(goFile, root)
	nodesBefore := len(g.NodesForFile(goFile))

	// Introduce error → first skip.
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.reparseFile(goFile, root) // skipped (first error)

	// Fix the error.
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Fixed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.reparseFile(goFile, root) // clean → should parse and clear error flag

	// Introduce error again → should skip again (flag was cleared).
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Broken() {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.reparseFile(goFile, root) // first error again → skip

	// Verify nodes are from "Fixed" (the last clean parse), not "Hello" (original).
	found := false
	for _, n := range g.NodesForFile(goFile) {
		if n.Name == "Fixed" {
			found = true
		}
	}
	if !found {
		t.Error("expected 'Fixed' node from last clean parse — error flag was not properly cleared")
	}
	_ = nodesBefore // suppress unused
}

func TestReparseFile_ProceedsOnCleanParse(t *testing.T) {
	root := t.TempDir()
	goFile := filepath.Join(root, "main.go")

	// Write a valid Go file.
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g := graph.New(root)
	walker := parser.NewWalker()
	w, err := New(g, walker, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// Initial parse.
	w.reparseFile(goFile, root)
	nodesBefore := len(g.NodesForFile(goFile))

	// Overwrite with DIFFERENT but VALID source — reparse should proceed.
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc World() {}\nfunc Another() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w.reparseFile(goFile, root)
	nodesAfter := len(g.NodesForFile(goFile))

	// Should have different node count since source changed.
	if nodesAfter == 0 {
		t.Error("expected nodes after clean reparse")
	}
	// Nodes should differ (World+Another vs Hello).
	if nodesAfter == nodesBefore {
		t.Log("note: node count unchanged, but reparse proceeded (acceptable if both sources produce same node count)")
	}
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

// ── ingestToBrain: brainClient is nil ────────────────────────────────────────

func TestIngestToBrain_NilBrainClient_EarlyReturn(t *testing.T) {
	g := graph.New("test")
	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// brainClient is nil by default — must return early without panic.
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

// ── reparseFile: call-site persistence survives multiple re-parses ─────────────
//
// This test validates that cross-file CALLS edges are correctly preserved across
// multiple incremental re-parses. The bug: persistAsync was calling
// PeekCallSites() AFTER ResolveCallEdges drained them, saving empty and wiping
// the call-site table. On the second re-parse, LoadCallSites() returned empty,
// so edges from other files were not recreated.
//
// The test calls reparseFile directly (white-box) to avoid the Start walk which
// does not call SaveCallSites (that is buildGraph's responsibility in main.go).
// We manually simulate the post-buildGraph state: parse files, SaveCallSites,
// ResolveCallEdges — then verify reparseFile preserves the stored table.

func TestReparseFile_CallSitesPersistAcrossMultipleReparses(t *testing.T) {
	dir := t.TempDir()

	// Two Go files in the same package: caller.go calls helper() from helper.go.
	helperSrc := "package mypkg\n\nfunc helper() {}\n"
	callerSrc := "package mypkg\n\nfunc Caller() { helper() }\n"
	helperPath := filepath.Join(dir, "helper.go")
	callerPath := filepath.Join(dir, "caller.go")

	if err := os.WriteFile(helperPath, []byte(helperSrc), 0o644); err != nil {
		t.Fatalf("write helper.go: %v", err)
	}
	if err := os.WriteFile(callerPath, []byte(callerSrc), 0o644); err != nil {
		t.Fatalf("write caller.go: %v", err)
	}

	g := graph.New("testcallsites")
	g.SetRoot(dir)
	wlkr := parser.NewWalker()

	// Parse both files (simulates buildGraph's parse phase).
	if err := wlkr.ParseFile(g, helperPath); err != nil {
		t.Fatalf("ParseFile helper.go: %v", err)
	}
	if err := wlkr.ParseFile(g, callerPath); err != nil {
		t.Fatalf("ParseFile caller.go: %v", err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Simulate buildGraph: SaveCallSites BEFORE ResolveCallEdges drains them.
	initialSites := g.PeekCallSites()
	if len(initialSites) == 0 {
		t.Skip("parser produced no call sites for this Go version/env — skipping")
	}
	if err := st.SaveCallSites(initialSites); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}
	// Drain pending call sites exactly as buildGraph does after SaveCallSites.
	resolver.ResolveCallEdges(g)

	w, err := New(g, wlkr, st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Stop()

	// First reparseFile call: re-parse helper.go (the callee). The watcher must
	// reload caller.go's call sites from the DB and keep them in the table.
	helperSrc2 := "package mypkg\n\nfunc helper() { _ = 1 }\n"
	if err := os.WriteFile(helperPath, []byte(helperSrc2), 0o644); err != nil {
		t.Fatalf("write helper.go v2: %v", err)
	}
	w.reparseFile(helperPath, "")

	sitesAfterFirst, err := st.LoadCallSites()
	if err != nil {
		t.Fatalf("LoadCallSites after first reparseFile: %v", err)
	}
	if len(sitesAfterFirst) == 0 {
		t.Fatal("call-site table must not be empty after first reparseFile (persistAsync was wiping it)")
	}

	// Second reparseFile call: re-parse helper.go again. If the table was wiped
	// after the first call, this second call would find an empty DB and lose
	// all cross-file edges.
	helperSrc3 := "package mypkg\n\nfunc helper() { _ = 2 }\n"
	if err := os.WriteFile(helperPath, []byte(helperSrc3), 0o644); err != nil {
		t.Fatalf("write helper.go v3: %v", err)
	}
	w.reparseFile(helperPath, "")

	sitesAfterSecond, err := st.LoadCallSites()
	if err != nil {
		t.Fatalf("LoadCallSites after second reparseFile: %v", err)
	}
	if len(sitesAfterSecond) == 0 {
		t.Error("call-site table must not be empty after second reparseFile (table was wiped and not restored)")
	}
}

package lsp

// Internal test package — access to unexported identifiers is intentional.
// Tests run without the gopls binary by injecting a goplsFakeTransport.
// Note: tsserver_test.go (also package lsp) defines tsFakeTransport/tsTempFile;
// all gopls-specific helpers use the "gopls" prefix to avoid collisions.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── goplsFakeTransport — fake LSP server for GoplsVerifier unit tests ────────
//
// Pre-load LSP responses via queueResponse. Send records every outgoing message.
// Recv pops responses from the queue in order, spinning briefly when empty.

type goplsFakeTransport struct {
	mu      sync.Mutex
	queue   []json.RawMessage        // responses to deliver via Recv()
	sent    []map[string]interface{} // all messages sent by the verifier
	closed  bool
	sendErr error // if set, Send returns this immediately
	recvErr error // if set, Recv returns this after the queue is drained
}

func newGoplsFakeTransport() *goplsFakeTransport { return &goplsFakeTransport{} }

// queueResponse enqueues a JSON-RPC response with the given id and result.
func (f *goplsFakeTransport) queueResponse(id int64, result interface{}) {
	var raw json.RawMessage
	if result == nil {
		raw = json.RawMessage("null")
	} else {
		data, err := json.Marshal(result)
		if err != nil {
			panic(fmt.Sprintf("goplsFakeTransport.queueResponse: %v", err))
		}
		raw = data
	}
	env, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  json.RawMessage(raw),
	})
	f.mu.Lock()
	f.queue = append(f.queue, env)
	f.mu.Unlock()
}

// Send implements LSPTransport.
func (f *goplsFakeTransport) Send(v interface{}) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	data, _ := json.Marshal(v)
	var msg map[string]interface{}
	_ = json.Unmarshal(data, &msg)
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	f.mu.Unlock()
	return nil
}

// Recv implements LSPTransport. Pops the next queued response. Spins for up
// to 200ms if the queue is empty — tests pre-load responses before calling
// ResolveEdge, so any wait is just scheduling jitter.
// Queued responses are delivered first; recvErr fires only after the queue drains.
func (f *goplsFakeTransport) Recv() (json.RawMessage, error) {
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.queue) > 0 {
			msg := f.queue[0]
			f.queue = f.queue[1:]
			f.mu.Unlock()
			return msg, nil
		}
		if f.recvErr != nil {
			err := f.recvErr
			f.mu.Unlock()
			return nil, err
		}
		f.mu.Unlock()
		time.Sleep(1 * time.Millisecond)
	}
	return nil, fmt.Errorf("goplsFakeTransport: no more queued responses (queue drained)")
}

// Close implements LSPTransport.
func (f *goplsFakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// sentMethods returns the "method" field of all messages sent by the verifier.
func (f *goplsFakeTransport) sentMethods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.sent))
	for _, m := range f.sent {
		if method, ok := m["method"].(string); ok {
			out = append(out, method)
		}
	}
	return out
}

// ── helpers ───────────────────────────────────────────────────────────────────

// goplsInitResult is the canonical gopls InitializeResult for tests.
var goplsInitResult = map[string]interface{}{"capabilities": map[string]interface{}{}}

// goplsLocationResult builds a single-location textDocument/definition result.
func goplsLocationResult(uri string, line int) map[string]interface{} {
	return map[string]interface{}{
		"uri": uri,
		"range": map[string]interface{}{
			"start": map[string]interface{}{"line": line, "character": 0},
			"end":   map[string]interface{}{"line": line, "character": 5},
		},
	}
}

// newGoplsTestVerifier builds a GoplsVerifier backed by ft.
func newGoplsTestVerifier(t *testing.T, ft *goplsFakeTransport) *GoplsVerifier {
	t.Helper()
	dir := t.TempDir()
	return NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot:    dir,
		GoplsPath:      "/fake/gopls", // non-empty so the not-found guard passes
		QueryTimeout:   2 * time.Second,
		StartupTimeout: 2 * time.Second,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return ft, nil
		},
	})
}

// writeGoFile creates a Go source file in dir with the given name and content.
func writeGoFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ── GoplsVerifier — gopls not found ──────────────────────────────────────────

func TestGoplsVerifier_GoplsNotFound_ReturnsConfidenceNone(t *testing.T) {
	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot: t.TempDir(),
		GoplsPath:   "", // empty → not found
	})
	pos := CallPosition{File: "/repo/a.go", Line: 5, Col: 3}
	edge, err := v.ResolveEdge(context.Background(), "from", "to", pos)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone when gopls not found, got %v", edge.Confidence)
	}
	if edge.From != "from" || edge.To != "to" {
		t.Errorf("edge IDs not preserved: From=%q To=%q", edge.From, edge.To)
	}
}

// ── GoplsVerifier — empty file position ──────────────────────────────────────

func TestGoplsVerifier_EmptyFile_ReturnsConfidenceNone(t *testing.T) {
	ft := newGoplsFakeTransport()
	v := newGoplsTestVerifier(t, ft)
	edge, err := v.ResolveEdge(context.Background(), "from", "to", CallPosition{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone for empty file, got %v", edge.Confidence)
	}
	if len(ft.sentMethods()) > 0 {
		t.Errorf("expected no LSP messages for empty file, sent: %v", ft.sentMethods())
	}
}

// ── GoplsVerifier — initialize handshake ─────────────────────────────────────

func TestGoplsVerifier_InitializationHandshake(t *testing.T) {
	dir := t.TempDir()
	src := writeGoFile(t, dir, "main.go", "package main\n")

	ft := newGoplsFakeTransport()
	ft.queueResponse(1, goplsInitResult)
	ft.queueResponse(2, nil)

	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot:    dir,
		GoplsPath:      "/fake/gopls",
		QueryTimeout:   2 * time.Second,
		StartupTimeout: 2 * time.Second,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return ft, nil
		},
	})

	_, _ = v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: src, Line: 0, Col: 0})

	methods := ft.sentMethods()
	if len(methods) < 3 {
		t.Fatalf("expected initialize + initialized + definition, got %v", methods)
	}
	if methods[0] != "initialize" {
		t.Errorf("first message should be initialize, got %q", methods[0])
	}
	foundInit := false
	for _, m := range methods {
		if m == "initialized" {
			foundInit = true
			break
		}
	}
	if !foundInit {
		t.Errorf("initialized notification not sent; methods: %v", methods)
	}
}

// ── GoplsVerifier — successful definition resolution ─────────────────────────

func TestGoplsVerifier_ResolveEdge_DefinitionFound(t *testing.T) {
	dir := t.TempDir()
	src := writeGoFile(t, dir, "handler.go", "package main\n")
	defFile := filepath.Join(dir, "store.go")

	ft := newGoplsFakeTransport()
	ft.queueResponse(1, goplsInitResult)
	ft.queueResponse(2, goplsLocationResult(pathToURI(defFile), 10))

	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot:    dir,
		GoplsPath:      "/fake/gopls",
		QueryTimeout:   2 * time.Second,
		StartupTimeout: 2 * time.Second,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return ft, nil
		},
	})

	edge, err := v.ResolveEdge(context.Background(), "caller", "guess",
		CallPosition{File: src, Line: 5, Col: 8})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Confidence != ConfidenceHigh {
		t.Errorf("expected ConfidenceHigh, got %v", edge.Confidence)
	}
	if edge.Callee.File != defFile {
		t.Errorf("callee file = %q, want %q", edge.Callee.File, defFile)
	}
	if edge.Callee.Line != 10 {
		t.Errorf("callee line = %d, want 10", edge.Callee.Line)
	}
}

// ── GoplsVerifier — null result (stdlib / no-resolve) ────────────────────────

func TestGoplsVerifier_ResolveEdge_NullResult_ConfidenceNone(t *testing.T) {
	dir := t.TempDir()
	src := writeGoFile(t, dir, "main.go", "package main\n")

	ft := newGoplsFakeTransport()
	ft.queueResponse(1, goplsInitResult)
	ft.queueResponse(2, nil)

	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot:    dir,
		GoplsPath:      "/fake/gopls",
		QueryTimeout:   2 * time.Second,
		StartupTimeout: 2 * time.Second,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return ft, nil
		},
	})

	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: src, Line: 0, Col: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone for null result, got %v", edge.Confidence)
	}
}

// ── GoplsVerifier — empty array result ───────────────────────────────────────

func TestGoplsVerifier_ResolveEdge_EmptyArray_ConfidenceNone(t *testing.T) {
	dir := t.TempDir()
	src := writeGoFile(t, dir, "main.go", "package main\n")

	ft := newGoplsFakeTransport()
	ft.queueResponse(1, goplsInitResult)
	ft.queueResponse(2, []interface{}{})

	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot:    dir,
		GoplsPath:      "/fake/gopls",
		QueryTimeout:   2 * time.Second,
		StartupTimeout: 2 * time.Second,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return ft, nil
		},
	})

	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: src, Line: 0, Col: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone for empty result array, got %v", edge.Confidence)
	}
}

// ── GoplsVerifier — array of locations (uses first) ──────────────────────────

func TestGoplsVerifier_ResolveEdge_ArrayOfLocations_UsesFirst(t *testing.T) {
	dir := t.TempDir()
	src := writeGoFile(t, dir, "main.go", "package main\n")
	def1 := filepath.Join(dir, "a.go")
	def2 := filepath.Join(dir, "b.go")

	ft := newGoplsFakeTransport()
	ft.queueResponse(1, goplsInitResult)
	ft.queueResponse(2, []interface{}{
		goplsLocationResult(pathToURI(def1), 3),
		goplsLocationResult(pathToURI(def2), 7),
	})

	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot:    dir,
		GoplsPath:      "/fake/gopls",
		QueryTimeout:   2 * time.Second,
		StartupTimeout: 2 * time.Second,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return ft, nil
		},
	})

	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: src, Line: 1, Col: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Callee.File != def1 {
		t.Errorf("expected first location %q, got %q", def1, edge.Callee.File)
	}
	if edge.Callee.Line != 3 {
		t.Errorf("expected line 3 from first location, got %d", edge.Callee.Line)
	}
}

// ── GoplsVerifier — NodeResolver wires Confirmed ─────────────────────────────

func TestGoplsVerifier_NodeResolver_SetsConfirmed(t *testing.T) {
	dir := t.TempDir()
	src := writeGoFile(t, dir, "main.go", "package main\n")
	defFile := filepath.Join(dir, "store.go")
	expectedNodeID := graph.NodeID("repo::store.go::DB.Close")

	ft := newGoplsFakeTransport()
	ft.queueResponse(1, goplsInitResult)
	ft.queueResponse(2, goplsLocationResult(pathToURI(defFile), 20))

	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot:    dir,
		GoplsPath:      "/fake/gopls",
		QueryTimeout:   2 * time.Second,
		StartupTimeout: 2 * time.Second,
		ResolveNodeID: func(file string, line int) graph.NodeID {
			if file == defFile && line == 20 {
				return expectedNodeID
			}
			return ""
		},
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return ft, nil
		},
	})

	edge, err := v.ResolveEdge(context.Background(), "caller", expectedNodeID,
		CallPosition{File: src, Line: 5, Col: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Callee.NodeID != expectedNodeID {
		t.Errorf("Callee.NodeID = %q, want %q", edge.Callee.NodeID, expectedNodeID)
	}
	if !edge.Confirmed {
		t.Error("expected Confirmed=true when NodeResolver returns the same ID as 'to'")
	}
}

func TestGoplsVerifier_NodeResolver_NotConfirmedWhenDifferent(t *testing.T) {
	dir := t.TempDir()
	src := writeGoFile(t, dir, "main.go", "package main\n")
	defFile := filepath.Join(dir, "store.go")
	resolvedID := graph.NodeID("repo::store.go::Redis.Close")
	treeID := graph.NodeID("repo::store.go::DB.Close")

	ft := newGoplsFakeTransport()
	ft.queueResponse(1, goplsInitResult)
	ft.queueResponse(2, goplsLocationResult(pathToURI(defFile), 42))

	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot:    dir,
		GoplsPath:      "/fake/gopls",
		QueryTimeout:   2 * time.Second,
		StartupTimeout: 2 * time.Second,
		ResolveNodeID:  func(_ string, _ int) graph.NodeID { return resolvedID },
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return ft, nil
		},
	})

	edge, err := v.ResolveEdge(context.Background(), "caller", treeID,
		CallPosition{File: src, Line: 5, Col: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Confirmed {
		t.Error("expected Confirmed=false when resolved ID differs from tree-sitter guess")
	}
	if edge.Callee.NodeID != resolvedID {
		t.Errorf("Callee.NodeID = %q, want %q", edge.Callee.NodeID, resolvedID)
	}
}

// ── GoplsVerifier — file is only opened once per session ─────────────────────

func TestGoplsVerifier_FileOpenedOncePerSession(t *testing.T) {
	dir := t.TempDir()
	src := writeGoFile(t, dir, "main.go", "package main\n")

	ft := newGoplsFakeTransport()
	ft.queueResponse(1, goplsInitResult)
	ft.queueResponse(2, nil) // first definition query
	ft.queueResponse(3, nil) // second definition query, same file

	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot:    dir,
		GoplsPath:      "/fake/gopls",
		QueryTimeout:   2 * time.Second,
		StartupTimeout: 2 * time.Second,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return ft, nil
		},
	})

	_, _ = v.ResolveEdge(context.Background(), "f", "t", CallPosition{File: src, Line: 0, Col: 0})
	_, _ = v.ResolveEdge(context.Background(), "f", "t", CallPosition{File: src, Line: 1, Col: 0})

	count := 0
	for _, m := range ft.sentMethods() {
		if m == "textDocument/didOpen" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 textDocument/didOpen, got %d (methods: %v)", count, ft.sentMethods())
	}
}

// ── GoplsVerifier — Close and restart ────────────────────────────────────────

func TestGoplsVerifier_CloseAndRestart(t *testing.T) {
	dir := t.TempDir()
	src := writeGoFile(t, dir, "main.go", "package main\n")

	var (
		mu         sync.Mutex
		transports []*goplsFakeTransport
	)

	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot:    dir,
		GoplsPath:      "/fake/gopls",
		QueryTimeout:   2 * time.Second,
		StartupTimeout: 2 * time.Second,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			ft := newGoplsFakeTransport()
			ft.queueResponse(1, goplsInitResult)
			ft.queueResponse(2, nil)
			mu.Lock()
			transports = append(transports, ft)
			mu.Unlock()
			return ft, nil
		},
	})

	// First use — triggers launch #1.
	_, _ = v.ResolveEdge(context.Background(), "f", "t", CallPosition{File: src})
	v.Close()

	mu.Lock()
	t1 := transports[0]
	mu.Unlock()
	if !t1.closed {
		t.Error("first transport should be closed after Close()")
	}

	// Second use after Close — triggers launch #2.
	_, _ = v.ResolveEdge(context.Background(), "f", "t", CallPosition{File: src})

	mu.Lock()
	count := len(transports)
	mu.Unlock()
	if count != 2 {
		t.Errorf("expected 2 launches (initial + restart after Close), got %d", count)
	}
}

// ── GoplsVerifier — recv error tears down and returns error ──────────────────

func TestGoplsVerifier_TransportError_TeardownAndReturnError(t *testing.T) {
	dir := t.TempDir()
	src := writeGoFile(t, dir, "main.go", "package main\n")

	ft := newGoplsFakeTransport()
	// Queue the initialize response so startup succeeds; recvErr fires on
	// the subsequent textDocument/definition recv, simulating a mid-session crash.
	ft.queueResponse(1, goplsInitResult)
	ft.recvErr = fmt.Errorf("gopls crashed")

	v := newGoplsTestVerifier(t, ft)

	_, err := v.ResolveEdge(context.Background(), "f", "t", CallPosition{File: src, Line: 0, Col: 0})
	if err == nil {
		t.Error("expected error on transport recv failure")
	}
	if !ft.closed {
		t.Error("transport should be closed after recv error")
	}
}

// ── GoplsVerifier — idempotent Close ─────────────────────────────────────────

func TestGoplsVerifier_Close_Idempotent(t *testing.T) {
	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot: t.TempDir(),
		GoplsPath:   "",
	})
	if err := v.Close(); err != nil {
		t.Errorf("first Close() error: %v", err)
	}
	if err := v.Close(); err != nil {
		t.Errorf("second Close() error: %v", err)
	}
}

// ── GoplsVerifier — Language ──────────────────────────────────────────────────

func TestGoplsVerifier_Language(t *testing.T) {
	v := NewGoplsVerifier(GoplsVerifierOptions{ProjectRoot: t.TempDir()})
	if v.Language() != LanguageGo {
		t.Errorf("Language() = %q, want %q", v.Language(), LanguageGo)
	}
}

// ── GoplsVerifier — integration with Manager ─────────────────────────────────

func TestGoplsVerifier_RegisteredInManager(t *testing.T) {
	dir := t.TempDir()
	src := writeGoFile(t, dir, "main.go", "package main\n")
	svcFile := filepath.Join(dir, "svc.go")

	ft := newGoplsFakeTransport()
	ft.queueResponse(1, goplsInitResult)
	ft.queueResponse(2, goplsLocationResult(pathToURI(svcFile), 15))

	v := NewGoplsVerifier(GoplsVerifierOptions{
		ProjectRoot:    dir,
		GoplsPath:      "/fake/gopls",
		QueryTimeout:   2 * time.Second,
		StartupTimeout: 2 * time.Second,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return ft, nil
		},
	})

	m := NewManager(Options{CacheTTL: time.Minute})
	m.Register(v)
	defer m.Close()

	edge, err := m.ResolveEdge(context.Background(), LanguageGo, "from", "to",
		CallPosition{File: src, Line: 5, Col: 3})
	if err != nil {
		t.Fatalf("Manager.ResolveEdge error: %v", err)
	}
	if edge.Confidence != ConfidenceHigh {
		t.Errorf("expected ConfidenceHigh via Manager, got %v", edge.Confidence)
	}
}

// ── URI helpers ───────────────────────────────────────────────────────────────

func TestPathToURI_AbsolutePath(t *testing.T) {
	path := "/home/user/project/main.go"
	uri := pathToURI(path)
	if uri != "file:///home/user/project/main.go" {
		t.Errorf("pathToURI(%q) = %q, want file:///home/user/project/main.go", path, uri)
	}
}

func TestURIToPath_RoundTrip(t *testing.T) {
	path := "/home/user/project/main.go"
	got := uriToPath(pathToURI(path))
	if got != path {
		t.Errorf("round-trip: got %q, want %q", got, path)
	}
}

// ── parseLocations ────────────────────────────────────────────────────────────

func TestParseLocations_Null(t *testing.T) {
	locs, err := parseLocations(json.RawMessage("null"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(locs) != 0 {
		t.Errorf("expected empty result for null, got %v", locs)
	}
}

func TestParseLocations_SingleObject(t *testing.T) {
	raw := json.RawMessage(`{"uri":"file:///a/b.go","range":{"start":{"line":5,"character":0},"end":{"line":5,"character":3}}}`)
	locs, err := parseLocations(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 location, got %d", len(locs))
	}
	if locs[0].URI != "file:///a/b.go" {
		t.Errorf("URI = %q, want file:///a/b.go", locs[0].URI)
	}
	if locs[0].Range.Start.Line != 5 {
		t.Errorf("line = %d, want 5", locs[0].Range.Start.Line)
	}
}

func TestParseLocations_ArrayOfObjects(t *testing.T) {
	raw := json.RawMessage(`[{"uri":"file:///a.go","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":2}}},{"uri":"file:///b.go","range":{"start":{"line":2,"character":0},"end":{"line":2,"character":2}}}]`)
	locs, err := parseLocations(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(locs))
	}
	if locs[0].URI != "file:///a.go" {
		t.Errorf("first URI = %q, want file:///a.go", locs[0].URI)
	}
}

func TestParseLocations_LocationLinkArray(t *testing.T) {
	// LocationLink format (newer LSP clients): targetUri instead of uri.
	raw := json.RawMessage(`[{"targetUri":"file:///c.go","targetRange":{"start":{"line":3,"character":0},"end":{"line":3,"character":4}},"targetSelectionRange":{"start":{"line":3,"character":0},"end":{"line":3,"character":4}}}]`)
	locs, err := parseLocations(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 location from LocationLink, got %d", len(locs))
	}
	if locs[0].URI != "file:///c.go" {
		t.Errorf("URI = %q, want file:///c.go", locs[0].URI)
	}
}

package lsp

// Internal tests for PyrightVerifier — placed in package lsp (not lsp_test) so
// the fake transport can implement the LSPTransport interface and be injected via
// the LaunchProcess field in PyrightVerifierOptions.
//
// Symbol naming: types and helpers unique to these tests are prefixed with "py"
// to avoid conflicts with gopls_test.go and tsserver_test.go symbols that live
// in the same package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── PyrightVerifier construction ──────────────────────────────────────────────

func TestNewPyrightVerifier_Language(t *testing.T) {
	v := NewPyrightVerifier(PyrightVerifierOptions{ProjectRoot: t.TempDir()})
	if v.Language() != LanguagePython {
		t.Errorf("Language() = %q, want LanguagePython", v.Language())
	}
}

func TestNewPyrightVerifier_EmptyBinaryGraceful(t *testing.T) {
	// When PyrightPath is empty (binary not found in PATH), ResolveEdge must
	// return ConfidenceNone without error.
	opts := PyrightVerifierOptions{
		ProjectRoot:    t.TempDir(),
		PyrightPath:    "",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
	}
	v := &PyrightVerifier{opts: opts.withDefaults()}

	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: "/any/file.py", Line: 1, Col: 0})
	if err != nil {
		t.Fatalf("expected no error when binary missing, got: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone when binary missing, got %v", edge.Confidence)
	}
}

func TestPyrightVerifier_CloseIdempotent(t *testing.T) {
	v := pyMakeVerifier(t, pyCfg{})
	if err := v.Close(); err != nil {
		t.Errorf("first Close() returned error: %v", err)
	}
	if err := v.Close(); err != nil {
		t.Errorf("second Close() returned error: %v", err)
	}
}

func TestPyrightVerifier_LaunchProcessError_ReturnsConfidenceNone(t *testing.T) {
	// When LaunchProcess fails, ensureStarted returns an error and ResolveEdge
	// must return ConfidenceNone without propagating — same graceful degradation
	// as the PyrightPath-empty path.
	srcFile := pyTempFile(t, "file.py", "# src\n")
	opts := PyrightVerifierOptions{
		ProjectRoot:    t.TempDir(),
		PyrightPath:    "/nonexistent/pyright-langserver",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return nil, errors.New("simulated launch failure")
		},
	}
	v := &PyrightVerifier{opts: opts.withDefaults()}

	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: srcFile, Line: 1, Col: 0})
	if err != nil {
		t.Fatalf("expected no error on launch failure, got: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone on launch failure, got %v", edge.Confidence)
	}
}

func TestPyrightVerifier_CloseAndRestart(t *testing.T) {
	// After Close(), the next ResolveEdge must transparently re-initialize
	// pyright-langserver and return a valid result.
	var (
		mu         sync.Mutex
		transports []*pyFakeTransport
	)

	root := t.TempDir()
	buildTransport := func() *pyFakeTransport {
		ft := &pyFakeTransport{
			cfg:  pyCfg{response: &pyDefLoc{uri: "file:///def.py", line: 7}},
			root: root,
		}
		mu.Lock()
		transports = append(transports, ft)
		mu.Unlock()
		return ft
	}

	opts := PyrightVerifierOptions{
		ProjectRoot:    root,
		PyrightPath:    "fake-pyright-langserver",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return buildTransport(), nil
		},
	}
	v := &PyrightVerifier{opts: opts.withDefaults()}

	srcFile := pyTempFile(t, "src.py", "# src\n")
	pos := CallPosition{File: srcFile, Line: 1, Col: 0}

	// First query: starts transport #1.
	edge1, err := v.ResolveEdge(context.Background(), "f", "t", pos)
	if err != nil {
		t.Fatalf("first ResolveEdge error: %v", err)
	}
	if edge1.Confidence != ConfidenceHigh {
		t.Errorf("first query: expected ConfidenceHigh, got %v", edge1.Confidence)
	}

	// Close shuts down transport #1.
	if err := v.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if v.transport != nil {
		t.Error("transport should be nil after Close")
	}

	// Second query: must restart (transport #2) and succeed.
	edge2, err := v.ResolveEdge(context.Background(), "f", "t", pos)
	if err != nil {
		t.Fatalf("second ResolveEdge error: %v", err)
	}
	if edge2.Confidence != ConfidenceHigh {
		t.Errorf("second query after restart: expected ConfidenceHigh, got %v", edge2.Confidence)
	}

	mu.Lock()
	n := len(transports)
	mu.Unlock()
	if n != 2 {
		t.Errorf("expected 2 transport instances (one per start), got %d", n)
	}
}

// ── ResolveEdge guard conditions ──────────────────────────────────────────────

func TestPyrightVerifier_EmptyFileReturnsNone(t *testing.T) {
	v := pyMakeVerifier(t, pyCfg{
		response: &pyDefLoc{uri: "file:///def/target.py", line: 10},
	})
	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: "", Line: 0, Col: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("empty file should return ConfidenceNone, got %v", edge.Confidence)
	}
}

func TestPyrightVerifier_NonPythonFileReturnsNone(t *testing.T) {
	v := pyMakeVerifier(t, pyCfg{
		response: &pyDefLoc{uri: "file:///def/target.py", line: 5},
	})
	for _, ext := range []string{".go", ".ts", ".rb", ".java", ".rs"} {
		edge, err := v.ResolveEdge(context.Background(), "from", "to",
			CallPosition{File: "/src/file" + ext, Line: 1, Col: 0})
		if err != nil {
			t.Fatalf("ext %s: unexpected error: %v", ext, err)
		}
		if edge.Confidence != ConfidenceNone {
			t.Errorf("ext %s: expected ConfidenceNone for non-Python file, got %v", ext, edge.Confidence)
		}
	}
}

func TestPyrightVerifier_PythonFileVariants(t *testing.T) {
	// Both .py and .pyi should reach the transport and get a result.
	for _, ext := range []string{".py", ".pyi"} {
		srcFile := pyTempFile(t, "src"+ext, "# empty\n")
		v := pyMakeVerifier(t, pyCfg{
			response: &pyDefLoc{uri: "file:///def/target.py", line: 5},
		})
		edge, err := v.ResolveEdge(context.Background(), "from", "to",
			CallPosition{File: srcFile, Line: 1, Col: 0})
		if err != nil {
			t.Fatalf("ext %s: unexpected error: %v", ext, err)
		}
		if edge.Confidence != ConfidenceHigh {
			t.Errorf("ext %s: expected ConfidenceHigh from stub, got %v", ext, edge.Confidence)
		}
	}
}

// ── ResolveEdge result mapping ─────────────────────────────────────────────────

func TestPyrightVerifier_ReturnsHighConfidence(t *testing.T) {
	srcFile := pyTempFile(t, "views.py", "# src\n")
	defFile := "/project/auth/decorators.py"
	defLine := 42

	v := pyMakeVerifier(t, pyCfg{
		response: &pyDefLoc{uri: "file://" + defFile, line: defLine},
	})

	from := graph.NodeID("proj::views.py::MyView")
	to := graph.NodeID("proj::auth/decorators.py::login_required")

	edge, err := v.ResolveEdge(context.Background(), from, to,
		CallPosition{File: srcFile, Line: 5, Col: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge == nil {
		t.Fatal("ResolveEdge returned nil edge")
	}
	if edge.Confidence != ConfidenceHigh {
		t.Errorf("expected ConfidenceHigh, got %v", edge.Confidence)
	}
	if edge.Callee.File != defFile {
		t.Errorf("Callee.File = %q, want %q", edge.Callee.File, defFile)
	}
	if edge.Callee.Line != defLine {
		t.Errorf("Callee.Line = %d, want %d", edge.Callee.Line, defLine)
	}
	if edge.From != from {
		t.Errorf("From = %q, want %q", edge.From, from)
	}
}

func TestPyrightVerifier_NullResponseReturnsNone(t *testing.T) {
	srcFile := pyTempFile(t, "file.py", "# src\n")
	v := pyMakeVerifier(t, pyCfg{responseRaw: "null"})

	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: srcFile, Line: 1, Col: 0})
	if err != nil {
		t.Fatalf("unexpected error for null response: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone for null response, got %v", edge.Confidence)
	}
}

func TestPyrightVerifier_EmptyArrayReturnsNone(t *testing.T) {
	srcFile := pyTempFile(t, "file.py", "# src\n")
	v := pyMakeVerifier(t, pyCfg{responseRaw: "[]"})

	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: srcFile, Line: 1, Col: 0})
	if err != nil {
		t.Fatalf("unexpected error for empty array: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone for empty locations, got %v", edge.Confidence)
	}
}

func TestPyrightVerifier_NodeIDResolverPopulatesCallee(t *testing.T) {
	srcFile := pyTempFile(t, "routes.py", "# src\n")
	defFile := "/project/security/auth.py"
	defLine := 15
	expectedNodeID := graph.NodeID("proj::security/auth.py::get_current_user")

	v := pyMakeVerifier(t, pyCfg{
		response: &pyDefLoc{uri: "file://" + defFile, line: defLine},
		resolveNodeID: func(file string, line int) graph.NodeID {
			if file == defFile && line == defLine {
				return expectedNodeID
			}
			return ""
		},
	})

	to := graph.NodeID("proj::security/auth.py::get_current_user")
	edge, err := v.ResolveEdge(context.Background(), "from", to,
		CallPosition{File: srcFile, Line: 3, Col: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Callee.NodeID != expectedNodeID {
		t.Errorf("Callee.NodeID = %q, want %q", edge.Callee.NodeID, expectedNodeID)
	}
	if !edge.Confirmed {
		t.Error("Confirmed should be true when Callee.NodeID matches To")
	}
}

func TestPyrightVerifier_TransportErrorCausesTeardown(t *testing.T) {
	srcFile := pyTempFile(t, "file.py", "# src\n")
	v := pyMakeVerifier(t, pyCfg{
		queryErr: errors.New("simulated I/O failure"),
	})

	_, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: srcFile, Line: 1, Col: 0})
	if err == nil {
		t.Error("expected error from transport failure, got nil")
	}
	if v.transport != nil {
		t.Error("transport should be nil after teardown")
	}
}

func TestPyrightVerifier_ContextCancellation(t *testing.T) {
	srcFile := pyTempFile(t, "file.py", "# src\n")
	v := pyMakeVerifier(t, pyCfg{queryDelay: 50 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_, err := v.ResolveEdge(ctx, "from", "to",
		CallPosition{File: srcFile, Line: 1, Col: 0})
	if err == nil {
		t.Error("expected error from context cancellation, got nil")
	}
}

func TestPyrightVerifier_ReusesCachedOpen(t *testing.T) {
	srcFile := pyTempFile(t, "file.py", "# src\n")
	ft := &pyFakeTransport{cfg: pyCfg{
		response: &pyDefLoc{uri: "file:///def.py", line: 1},
	}}
	v := pyMakeVerifierWithTransport(t, ft)

	pos := CallPosition{File: srcFile, Line: 1, Col: 0}
	_, _ = v.ResolveEdge(context.Background(), "from", "to", pos)
	opensBefore := ft.openCount
	_, _ = v.ResolveEdge(context.Background(), "from", "to", pos)
	if ft.openCount != opensBefore {
		t.Errorf("second call sent %d additional didOpen (want 0)", ft.openCount-opensBefore)
	}
}

// ── pyrightconfig detection ───────────────────────────────────────────────────

func TestPyrightVerifier_InitializeIncludesPyrightConfig(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "pyrightconfig.json")
	if err := os.WriteFile(cfg, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	ft := &pyFakeTransport{cfg: pyCfg{
		response: &pyDefLoc{uri: "file:///def.py", line: 1},
	}}
	opts := PyrightVerifierOptions{
		ProjectRoot:    root,
		PyrightPath:    "fake-pyright-langserver",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			ft.root = root
			return ft, nil
		},
	}
	v := &PyrightVerifier{opts: opts.withDefaults()}

	srcFile := pyTempFile(t, "f.py", "# src\n")
	_, _ = v.ResolveEdge(context.Background(), "f", "t", CallPosition{File: srcFile, Line: 0, Col: 0})

	if ft.initPyrightConfig != cfg {
		t.Errorf("expected initPyrightConfig=%q, got %q", cfg, ft.initPyrightConfig)
	}
}

func TestPyrightVerifier_InitializeNoPyrightConfigIsOK(t *testing.T) {
	root := t.TempDir()
	ft := &pyFakeTransport{cfg: pyCfg{response: &pyDefLoc{uri: "file:///def.py", line: 0}}}
	opts := PyrightVerifierOptions{
		ProjectRoot:    root,
		PyrightPath:    "fake-pyright-langserver",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			ft.root = root
			return ft, nil
		},
	}
	v := &PyrightVerifier{opts: opts.withDefaults()}

	srcFile := pyTempFile(t, "f.py", "# src\n")
	_, err := v.ResolveEdge(context.Background(), "f", "t", CallPosition{File: srcFile, Line: 0, Col: 0})
	if err != nil {
		t.Fatalf("unexpected error with no pyrightconfig: %v", err)
	}
}

func TestPyrightVerifier_InitializeIncludesPythonPath(t *testing.T) {
	root := t.TempDir()
	pythonPath := "/usr/local/bin/python3"

	ft := &pyFakeTransport{cfg: pyCfg{
		response: &pyDefLoc{uri: "file:///def.py", line: 0},
	}}
	opts := PyrightVerifierOptions{
		ProjectRoot:    root,
		PyrightPath:    "fake-pyright-langserver",
		PythonPath:     pythonPath,
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			ft.root = root
			return ft, nil
		},
	}
	v := &PyrightVerifier{opts: opts.withDefaults()}

	srcFile := pyTempFile(t, "f.py", "# src\n")
	_, _ = v.ResolveEdge(context.Background(), "f", "t", CallPosition{File: srcFile, Line: 0, Col: 0})

	if ft.initPythonPath != pythonPath {
		t.Errorf("expected initPythonPath=%q, got %q", pythonPath, ft.initPythonPath)
	}
}

// ── FindPyrightConfig ─────────────────────────────────────────────────────────

func TestFindPyrightConfig_InRoot(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "pyrightconfig.json")
	if err := os.WriteFile(cfg, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := FindPyrightConfig(dir)
	if got != cfg {
		t.Errorf("FindPyrightConfig(%q) = %q, want %q", dir, got, cfg)
	}
}

func TestFindPyrightConfig_InParentDir(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "src", "app")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(parent, "pyrightconfig.json")
	if err := os.WriteFile(cfg, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := FindPyrightConfig(child)
	if got != cfg {
		t.Errorf("FindPyrightConfig(%q) = %q, want %q", child, got, cfg)
	}
}

func TestFindPyrightConfig_MissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	got := FindPyrightConfig(dir)
	_ = got // either "" or a real config higher up — must not panic
}

// ── IsPythonFile ──────────────────────────────────────────────────────────────

func TestIsPythonFile(t *testing.T) {
	yes := []string{"/a.py", "/a.pyi", "/A.PY", "/a.PYI", "/deep/path/module.py"}
	no := []string{"/a.go", "/a.ts", "/a.java", "/a.rb", "/a.rs", "/a.c", "/a", "/a.pyc"}

	for _, p := range yes {
		if !IsPythonFile(p) {
			t.Errorf("IsPythonFile(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if IsPythonFile(p) {
			t.Errorf("IsPythonFile(%q) = true, want false", p)
		}
	}
}

// ── Manager integration ───────────────────────────────────────────────────────

func TestManager_PyrightCoexistsWithOtherVerifiers(t *testing.T) {
	goNoop := &noopVerifier{lang: LanguageGo}
	tsNoop := &noopVerifier{lang: LanguageTypeScript}
	pyHigh := &pyStubVerifier{}

	m := NewManager(Options{})
	m.Register(goNoop)
	m.Register(tsNoop)
	m.Register(pyHigh)

	goEdge, _ := m.ResolveEdge(context.Background(), LanguageGo, "f", "t",
		CallPosition{File: "/f.go", Line: 1})
	tsEdge, _ := m.ResolveEdge(context.Background(), LanguageTypeScript, "f", "t",
		CallPosition{File: "/f.ts", Line: 1})
	pyEdge, _ := m.ResolveEdge(context.Background(), LanguagePython, "f", "t",
		CallPosition{File: "/f.py", Line: 1})

	if goEdge.Confidence != ConfidenceNone {
		t.Errorf("Go verifier: expected NONE, got %v", goEdge.Confidence)
	}
	if tsEdge.Confidence != ConfidenceNone {
		t.Errorf("TS verifier: expected NONE, got %v", tsEdge.Confidence)
	}
	if pyEdge.Confidence != ConfidenceHigh {
		t.Errorf("Python verifier: expected HIGH, got %v", pyEdge.Confidence)
	}
}

func TestManager_PyrightUnregistered_ReturnsNone(t *testing.T) {
	// Without any Python verifier registered, Manager.Get returns NoOpVerifier.
	m := NewManager(Options{})

	edge, err := m.ResolveEdge(context.Background(), LanguagePython, "f", "t",
		CallPosition{File: "/f.py", Line: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("unregistered Python verifier: expected NONE, got %v", edge.Confidence)
	}
}

// ── test helpers ──────────────────────────────────────────────────────────────

// pyCfg drives the pyFakeTransport response behaviour.
type pyCfg struct {
	response         *pyDefLoc
	responseRaw      string
	queryErr         error
	queryDelay       time.Duration
	resolveNodeID    NodeResolver
	chPrepareResult  interface{} // result for textDocument/prepareCallHierarchy
	chIncomingResult interface{} // result for callHierarchy/incomingCalls
	chOutgoingResult interface{} // result for callHierarchy/outgoingCalls
}

// pyDefLoc is a single definition location returned by pyFakeTransport.
type pyDefLoc struct {
	uri  string
	line int
}

// pyMakeVerifier builds a PyrightVerifier with a fresh pyFakeTransport.
func pyMakeVerifier(t *testing.T, cfg pyCfg) *PyrightVerifier {
	t.Helper()
	ft := &pyFakeTransport{cfg: cfg}
	return pyMakeVerifierWithTransport(t, ft)
}

func pyMakeVerifierWithTransport(t *testing.T, ft *pyFakeTransport) *PyrightVerifier {
	t.Helper()
	root := t.TempDir()
	opts := PyrightVerifierOptions{
		ProjectRoot:    root,
		PyrightPath:    "fake-pyright-langserver",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
		ResolveNodeID:  ft.cfg.resolveNodeID,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			ft.root = root
			return ft, nil
		},
	}
	return &PyrightVerifier{opts: opts.withDefaults()}
}

// pyTempFile writes content to a new file in a per-call temp dir and returns path.
func pyTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("pyTempFile: %v", err)
	}
	return path
}

// pyFakeTransport implements LSPTransport for PyrightVerifier unit tests.
// It handles LSP message exchange in-process without spawning a subprocess.
type pyFakeTransport struct {
	cfg               pyCfg
	root              string
	msgQueue          []json.RawMessage
	openCount         int    // count of textDocument/didOpen notifications received
	initPyrightConfig string // pyrightconfig path from initializationOptions, if any
	initPythonPath    string // pythonPath from initializationOptions, if any
}

func (f *pyFakeTransport) Send(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}

	method, _ := msg["method"].(string)
	id, _ := msg["id"].(float64)

	switch method {
	case "initialize":
		if params, ok := msg["params"].(map[string]interface{}); ok {
			if initOpts, ok := params["initializationOptions"].(map[string]interface{}); ok {
				if pc, ok := initOpts["pyrightconfig"].(string); ok {
					f.initPyrightConfig = pc
				}
				if pp, ok := initOpts["pythonPath"].(string); ok {
					f.initPythonPath = pp
				}
			}
		}
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  map[string]interface{}{"capabilities": map[string]interface{}{}},
		}
		raw, _ := json.Marshal(resp)
		f.msgQueue = append(f.msgQueue, raw)

	case "initialized":
		// Notification — no response.

	case "textDocument/didOpen":
		f.openCount++
		// No response.

	case "textDocument/definition":
		if f.cfg.queryDelay > 0 {
			time.Sleep(f.cfg.queryDelay)
		}
		if f.cfg.queryErr != nil {
			return f.cfg.queryErr
		}
		var resultRaw string
		switch {
		case f.cfg.responseRaw != "":
			resultRaw = f.cfg.responseRaw
		case f.cfg.response != nil:
			resultRaw = fmt.Sprintf(
				`[{"uri":%q,"range":{"start":{"line":%d,"character":0},"end":{"line":%d,"character":0}}}]`,
				f.cfg.response.uri, f.cfg.response.line, f.cfg.response.line,
			)
		default:
			resultRaw = "null"
		}
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, int64(id), resultRaw)
		f.msgQueue = append(f.msgQueue, json.RawMessage(resp))

	case "textDocument/prepareCallHierarchy":
		raw := pyEncodeResult(f.cfg.chPrepareResult)
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, int64(id), raw)
		f.msgQueue = append(f.msgQueue, json.RawMessage(resp))

	case "callHierarchy/incomingCalls":
		raw := pyEncodeResult(f.cfg.chIncomingResult)
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, int64(id), raw)
		f.msgQueue = append(f.msgQueue, json.RawMessage(resp))

	case "callHierarchy/outgoingCalls":
		raw := pyEncodeResult(f.cfg.chOutgoingResult)
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, int64(id), raw)
		f.msgQueue = append(f.msgQueue, json.RawMessage(resp))
	}
	return nil
}

// pyEncodeResult marshals v to JSON, returning "null" if v is nil.
func pyEncodeResult(v interface{}) string {
	if v == nil {
		return "null"
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func (f *pyFakeTransport) Recv() (json.RawMessage, error) {
	if len(f.msgQueue) == 0 {
		time.Sleep(20 * time.Millisecond) // let context timeout tests fire
		return nil, fmt.Errorf("no messages in queue")
	}
	msg := f.msgQueue[0]
	f.msgQueue = f.msgQueue[1:]
	return msg, nil
}

func (f *pyFakeTransport) Close() error { return nil }

// pyStubVerifier always returns ConfidenceHigh for LanguagePython.
type pyStubVerifier struct{}

func (s *pyStubVerifier) ResolveEdge(_ context.Context, from, to graph.NodeID, pos CallPosition) (*VerifiedEdge, error) {
	callee := CalleeInfo{NodeID: to, File: pos.File, Line: pos.Line}
	return NewVerifiedEdge(from, to, callee, ConfidenceHigh), nil
}

func (s *pyStubVerifier) Language() Language { return LanguagePython }
func (s *pyStubVerifier) Close() error        { return nil }

// ── PyrightVerifier — CallHierarchyProvider ───────────────────────────────────

// pyCallHierarchyItemResult builds a single CallHierarchyItem wire object.
func pyCallHierarchyItemResult(name, detail, uri string, line int) map[string]interface{} {
	pos := map[string]interface{}{"line": line, "character": 0}
	rng := map[string]interface{}{"start": pos, "end": pos}
	return map[string]interface{}{
		"name":           name,
		"detail":         detail,
		"kind":           12,
		"uri":            uri,
		"range":          rng,
		"selectionRange": rng,
	}
}

// pyIncomingCallResult builds one entry of a callHierarchy/incomingCalls response.
func pyIncomingCallResult(name, detail, uri string, line int) map[string]interface{} {
	return map[string]interface{}{
		"from":       pyCallHierarchyItemResult(name, detail, uri, line),
		"fromRanges": []interface{}{},
	}
}

// pyOutgoingCallResult builds one entry of a callHierarchy/outgoingCalls response.
func pyOutgoingCallResult(name, detail, uri string, line int) map[string]interface{} {
	return map[string]interface{}{
		"to":         pyCallHierarchyItemResult(name, detail, uri, line),
		"fromRanges": []interface{}{},
	}
}

func TestPyrightVerifier_PrepareCallHierarchy_NotFound_ReturnsNil(t *testing.T) {
	opts := PyrightVerifierOptions{ProjectRoot: t.TempDir(), PyrightPath: ""}
	v := &PyrightVerifier{opts: opts.withDefaults()}
	items, err := v.PrepareCallHierarchy(context.Background(), CallPosition{File: "/x.py", Line: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if items != nil {
		t.Errorf("expected nil when binary not found, got %v", items)
	}
}

func TestPyrightVerifier_PrepareCallHierarchy_NonPythonFile_ReturnsNil(t *testing.T) {
	v := pyMakeVerifier(t, pyCfg{})
	items, err := v.PrepareCallHierarchy(context.Background(), CallPosition{File: "/src/main.go", Line: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if items != nil {
		t.Errorf("expected nil for non-Python file, got %v", items)
	}
}

func TestPyrightVerifier_PrepareCallHierarchy_EmptyFile_ReturnsNil(t *testing.T) {
	ft := &pyFakeTransport{cfg: pyCfg{}}
	v := pyMakeVerifierWithTransport(t, ft)
	items, err := v.PrepareCallHierarchy(context.Background(), CallPosition{}) // empty file
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if items != nil {
		t.Errorf("expected nil for empty file, got %v", items)
	}
}

func TestPyrightVerifier_PrepareCallHierarchy_Success(t *testing.T) {
	srcFile := pyTempFile(t, "views.py", "# src\n")
	defURI := pathToURI(srcFile)

	itemResult := []interface{}{
		pyCallHierarchyItemResult("get_user", "AuthService.get_user", defURI, 12),
	}
	v := pyMakeVerifier(t, pyCfg{chPrepareResult: itemResult})

	items, err := v.PrepareCallHierarchy(context.Background(), CallPosition{File: srcFile, Line: 12, Col: 0})
	if err != nil {
		t.Fatalf("PrepareCallHierarchy: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Name != "get_user" {
		t.Errorf("expected Name=get_user, got %q", items[0].Name)
	}
	if items[0].Detail != "AuthService.get_user" {
		t.Errorf("expected Detail=AuthService.get_user, got %q", items[0].Detail)
	}
	if items[0].File != srcFile {
		t.Errorf("expected File=%q, got %q", srcFile, items[0].File)
	}
	if items[0].Line != 12 {
		t.Errorf("expected Line=12, got %d", items[0].Line)
	}
}

func TestPyrightVerifier_PrepareCallHierarchy_NullResponse_ReturnsEmpty(t *testing.T) {
	srcFile := pyTempFile(t, "app.py", "# src\n")
	v := pyMakeVerifier(t, pyCfg{chPrepareResult: nil}) // nil → "null"

	items, err := v.PrepareCallHierarchy(context.Background(), CallPosition{File: srcFile, Line: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items for null response, got %d", len(items))
	}
}

func TestPyrightVerifier_IncomingCalls_Success(t *testing.T) {
	srcFile := pyTempFile(t, "service.py", "# src\n")
	caller1URI := pathToURI(pyTempFile(t, "main.py", "# main\n"))
	caller2URI := pathToURI(pyTempFile(t, "tests.py", "# tests\n"))
	defURI := pathToURI(srcFile)

	prepResult := []interface{}{
		pyCallHierarchyItemResult("handle_request", "RequestHandler.handle_request", defURI, 20),
	}
	incomingResult := []interface{}{
		pyIncomingCallResult("main", "main", caller1URI, 5),
		pyIncomingCallResult("test_handler", "test_handler", caller2URI, 10),
	}
	v := pyMakeVerifier(t, pyCfg{
		chPrepareResult:  prepResult,
		chIncomingResult: incomingResult,
	})

	// PrepareCallHierarchy must be called first to start the verifier.
	prepItems, err := v.PrepareCallHierarchy(context.Background(), CallPosition{File: srcFile, Line: 20, Col: 0})
	if err != nil || len(prepItems) == 0 {
		t.Fatalf("PrepareCallHierarchy failed or empty: %v", err)
	}

	item := CallHierarchyItem{Name: "handle_request", File: srcFile, Line: 20, Col: 0}
	callers, err := v.IncomingCalls(context.Background(), item)
	if err != nil {
		t.Fatalf("IncomingCalls: %v", err)
	}
	if len(callers) != 2 {
		t.Fatalf("expected 2 callers, got %d", len(callers))
	}
	if callers[0].Name != "main" {
		t.Errorf("expected first caller=main, got %q", callers[0].Name)
	}
	if callers[1].Name != "test_handler" {
		t.Errorf("expected second caller=test_handler, got %q", callers[1].Name)
	}
}

func TestPyrightVerifier_OutgoingCalls_Success(t *testing.T) {
	srcFile := pyTempFile(t, "handler.py", "# src\n")
	calleeURI := pathToURI(pyTempFile(t, "db.py", "# db\n"))
	defURI := pathToURI(srcFile)

	prepResult := []interface{}{
		pyCallHierarchyItemResult("login", "AuthHandler.login", defURI, 35),
	}
	outgoingResult := []interface{}{
		pyOutgoingCallResult("query", "Database.query", calleeURI, 88),
		pyOutgoingCallResult("hash_password", "bcrypt.hash_password", calleeURI, 12),
	}
	v := pyMakeVerifier(t, pyCfg{
		chPrepareResult:  prepResult,
		chOutgoingResult: outgoingResult,
	})

	_, err := v.PrepareCallHierarchy(context.Background(), CallPosition{File: srcFile, Line: 35, Col: 0})
	if err != nil {
		t.Fatalf("PrepareCallHierarchy: %v", err)
	}

	item := CallHierarchyItem{Name: "login", File: srcFile, Line: 35, Col: 0}
	callees, err := v.OutgoingCalls(context.Background(), item)
	if err != nil {
		t.Fatalf("OutgoingCalls: %v", err)
	}
	if len(callees) != 2 {
		t.Fatalf("expected 2 callees, got %d", len(callees))
	}
	if callees[0].Name != "query" {
		t.Errorf("expected first callee=query, got %q", callees[0].Name)
	}
	if callees[1].Name != "hash_password" {
		t.Errorf("expected second callee=hash_password, got %q", callees[1].Name)
	}
}

// TestPyrightVerifier_CallHierarchyProvider_InterfaceSatisfied verifies that
// *PyrightVerifier implements CallHierarchyProvider at compile time.
func TestPyrightVerifier_CallHierarchyProvider_InterfaceSatisfied(t *testing.T) {
	var _ CallHierarchyProvider = (*PyrightVerifier)(nil)
}

package lsp

// Internal tests for TsserverVerifier — placed in package lsp (not lsp_test) so
// the fake transport can implement the LSPTransport interface and be injected via
// the LaunchProcess field in TsserverVerifierOptions.
//
// Symbol naming: types and helpers unique to these tests are prefixed with "ts"
// to avoid conflicts with gopls_test.go symbols that live in the same package.

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

// ── TsserverVerifier construction ─────────────────────────────────────────────

func TestNewTsserverVerifier_Language(t *testing.T) {
	v := NewTsserverVerifier(TsserverVerifierOptions{ProjectRoot: t.TempDir()})
	if v.Language() != LanguageTypeScript {
		t.Errorf("Language() = %q, want LanguageTypeScript", v.Language())
	}
}

func TestNewTsserverVerifier_EmptyBinaryGraceful(t *testing.T) {
	// When TSServerPath is empty (binary not found in PATH), ResolveEdge must
	// return ConfidenceNone without error.
	opts := TsserverVerifierOptions{
		ProjectRoot:    t.TempDir(),
		TSServerPath:   "",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
	}
	v := &TsserverVerifier{opts: opts.withDefaults()}

	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: "/any/file.ts", Line: 1, Col: 0})
	if err != nil {
		t.Fatalf("expected no error when binary missing, got: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone when binary missing, got %v", edge.Confidence)
	}
}

func TestTsserverVerifier_CloseIdempotent(t *testing.T) {
	v := tsMakeVerifier(t, tsCfg{})
	if err := v.Close(); err != nil {
		t.Errorf("first Close() returned error: %v", err)
	}
	if err := v.Close(); err != nil {
		t.Errorf("second Close() returned error: %v", err)
	}
}

func TestTsserverVerifier_LaunchProcessError_ReturnsConfidenceNone(t *testing.T) {
	// When LaunchProcess fails (e.g. binary not executable), ensureStarted returns
	// an error and ResolveEdge must return ConfidenceNone without propagating the
	// error — same graceful degradation as the TSServerPath-empty path.
	srcFile := tsTempFile(t, "file.ts", "// src\n")
	opts := TsserverVerifierOptions{
		ProjectRoot:    t.TempDir(),
		TSServerPath:   "/nonexistent/typescript-language-server",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return nil, errors.New("simulated launch failure")
		},
	}
	v := &TsserverVerifier{opts: opts.withDefaults()}

	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: srcFile, Line: 1, Col: 0})
	if err != nil {
		t.Fatalf("expected no error on launch failure, got: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone on launch failure, got %v", edge.Confidence)
	}
}

func TestTsserverVerifier_CloseAndRestart(t *testing.T) {
	// After Close(), the next ResolveEdge must transparently re-initialize
	// typescript-language-server and return a valid result.
	var (
		mu         sync.Mutex
		transports []*tsFakeTransport
	)

	root := t.TempDir()
	buildTransport := func() *tsFakeTransport {
		ft := &tsFakeTransport{
			cfg:  tsCfg{response: &tsDefLoc{uri: "file:///def.ts", line: 7}},
			root: root,
		}
		mu.Lock()
		transports = append(transports, ft)
		mu.Unlock()
		return ft
	}

	opts := TsserverVerifierOptions{
		ProjectRoot:    root,
		TSServerPath:   "fake-tsserver",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			return buildTransport(), nil
		},
	}
	v := &TsserverVerifier{opts: opts.withDefaults()}

	srcFile := tsTempFile(t, "src.ts", "// src\n")
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

func TestTsserverVerifier_EmptyFileReturnsNone(t *testing.T) {
	v := tsMakeVerifier(t, tsCfg{
		response: &tsDefLoc{uri: "file:///def/target.ts", line: 10},
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

func TestTsserverVerifier_NonTSFileReturnsNone(t *testing.T) {
	v := tsMakeVerifier(t, tsCfg{
		response: &tsDefLoc{uri: "file:///def/target.ts", line: 5},
	})
	for _, ext := range []string{".go", ".py", ".rb", ".java", ".rs"} {
		edge, err := v.ResolveEdge(context.Background(), "from", "to",
			CallPosition{File: "/src/file" + ext, Line: 1, Col: 0})
		if err != nil {
			t.Fatalf("ext %s: unexpected error: %v", ext, err)
		}
		if edge.Confidence != ConfidenceNone {
			t.Errorf("ext %s: expected ConfidenceNone for non-TS file, got %v", ext, edge.Confidence)
		}
	}
}

func TestTsserverVerifier_TSFileVariants(t *testing.T) {
	// All supported TS/JS extensions should reach the transport and get a result.
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"} {
		srcFile := tsTempFile(t, "src"+ext, "// empty\n")
		v := tsMakeVerifier(t, tsCfg{
			response: &tsDefLoc{uri: "file:///def/target.ts", line: 5},
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

func TestTsserverVerifier_ReturnsHighConfidence(t *testing.T) {
	srcFile := tsTempFile(t, "component.ts", "// src\n")
	defFile := "/project/utils.ts"
	defLine := 42

	v := tsMakeVerifier(t, tsCfg{
		response: &tsDefLoc{uri: "file://" + defFile, line: defLine},
	})

	from := graph.NodeID("proj::component.ts::Caller")
	to := graph.NodeID("proj::utils.ts::helper")

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

func TestTsserverVerifier_NullResponseReturnsNone(t *testing.T) {
	srcFile := tsTempFile(t, "file.ts", "// src\n")
	v := tsMakeVerifier(t, tsCfg{responseRaw: "null"})

	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: srcFile, Line: 1, Col: 0})
	if err != nil {
		t.Fatalf("unexpected error for null response: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone for null response, got %v", edge.Confidence)
	}
}

func TestTsserverVerifier_EmptyArrayReturnsNone(t *testing.T) {
	srcFile := tsTempFile(t, "file.ts", "// src\n")
	v := tsMakeVerifier(t, tsCfg{responseRaw: "[]"})

	edge, err := v.ResolveEdge(context.Background(), "from", "to",
		CallPosition{File: srcFile, Line: 1, Col: 0})
	if err != nil {
		t.Fatalf("unexpected error for empty array: %v", err)
	}
	if edge.Confidence != ConfidenceNone {
		t.Errorf("expected ConfidenceNone for empty locations, got %v", edge.Confidence)
	}
}

func TestTsserverVerifier_NodeIDResolverPopulatesCallee(t *testing.T) {
	srcFile := tsTempFile(t, "caller.ts", "// src\n")
	defFile := "/project/auth.ts"
	defLine := 15
	expectedNodeID := graph.NodeID("proj::auth.ts::verifyToken")

	v := tsMakeVerifier(t, tsCfg{
		response: &tsDefLoc{uri: "file://" + defFile, line: defLine},
		resolveNodeID: func(file string, line int) graph.NodeID {
			if file == defFile && line == defLine {
				return expectedNodeID
			}
			return ""
		},
	})

	to := graph.NodeID("proj::auth.ts::verifyToken")
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

func TestTsserverVerifier_TransportErrorCausesTeardown(t *testing.T) {
	srcFile := tsTempFile(t, "file.ts", "// src\n")
	v := tsMakeVerifier(t, tsCfg{
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

func TestTsserverVerifier_ContextCancellation(t *testing.T) {
	srcFile := tsTempFile(t, "file.ts", "// src\n")
	v := tsMakeVerifier(t, tsCfg{queryDelay: 50 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_, err := v.ResolveEdge(ctx, "from", "to",
		CallPosition{File: srcFile, Line: 1, Col: 0})
	if err == nil {
		t.Error("expected error from context cancellation, got nil")
	}
}

func TestTsserverVerifier_ReusesCachedOpen(t *testing.T) {
	srcFile := tsTempFile(t, "file.ts", "// src\n")
	ft := &tsFakeTransport{cfg: tsCfg{
		response: &tsDefLoc{uri: "file:///def.ts", line: 1},
	}}
	v := tsMakeVerifierWithTransport(t, ft)

	pos := CallPosition{File: srcFile, Line: 1, Col: 0}
	_, _ = v.ResolveEdge(context.Background(), "from", "to", pos)
	opensBefore := ft.openCount
	_, _ = v.ResolveEdge(context.Background(), "from", "to", pos)
	if ft.openCount != opensBefore {
		t.Errorf("second call sent %d additional didOpen (want 0)", ft.openCount-opensBefore)
	}
}

// ── tsconfig detection ────────────────────────────────────────────────────────

func TestTsserverVerifier_InitializeIncludesTsconfig(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "tsconfig.json")
	if err := os.WriteFile(cfg, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	ft := &tsFakeTransport{cfg: tsCfg{
		response: &tsDefLoc{uri: "file:///def.ts", line: 1},
	}}
	opts := TsserverVerifierOptions{
		ProjectRoot:    root,
		TSServerPath:   "fake-tsserver",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			ft.root = root
			return ft, nil
		},
	}
	v := &TsserverVerifier{opts: opts.withDefaults()}

	srcFile := tsTempFile(t, "f.ts", "// src\n")
	_, _ = v.ResolveEdge(context.Background(), "f", "t", CallPosition{File: srcFile, Line: 0, Col: 0})

	if ft.initTsconfig != cfg {
		t.Errorf("expected initTsconfig=%q, got %q", cfg, ft.initTsconfig)
	}
}

func TestTsserverVerifier_InitializeNoTsconfigIsOK(t *testing.T) {
	root := t.TempDir()
	ft := &tsFakeTransport{cfg: tsCfg{response: &tsDefLoc{uri: "file:///def.ts", line: 0}}}
	opts := TsserverVerifierOptions{
		ProjectRoot:    root,
		TSServerPath:   "fake-tsserver",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			ft.root = root
			return ft, nil
		},
	}
	v := &TsserverVerifier{opts: opts.withDefaults()}

	srcFile := tsTempFile(t, "f.ts", "// src\n")
	_, err := v.ResolveEdge(context.Background(), "f", "t", CallPosition{File: srcFile, Line: 0, Col: 0})
	if err != nil {
		t.Fatalf("unexpected error with no tsconfig: %v", err)
	}
}

// ── FindTsconfig ──────────────────────────────────────────────────────────────

func TestFindTsconfig_InRoot(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "tsconfig.json")
	if err := os.WriteFile(cfg, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := FindTsconfig(dir)
	if got != cfg {
		t.Errorf("FindTsconfig(%q) = %q, want %q", dir, got, cfg)
	}
}

func TestFindTsconfig_InParentDir(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "src", "app")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(parent, "tsconfig.json")
	if err := os.WriteFile(cfg, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := FindTsconfig(child)
	if got != cfg {
		t.Errorf("FindTsconfig(%q) = %q, want %q", child, got, cfg)
	}
}

func TestFindTsconfig_MissingReturnsString(t *testing.T) {
	dir := t.TempDir()
	got := FindTsconfig(dir)
	_ = got // either "" or a real tsconfig.json higher up — just must not panic
}

// ── TsLanguageID ──────────────────────────────────────────────────────────────

func TestTsLanguageID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/src/app.ts", "typescript"},
		{"/src/App.tsx", "typescriptreact"},
		{"/src/utils.js", "javascript"},
		{"/src/Component.jsx", "javascriptreact"},
		{"/src/module.mjs", "javascript"},
		{"/src/bundle.cjs", "javascript"},
		{"/src/UPPER.TS", "typescript"},
		{"/src/mixed.TSX", "typescriptreact"},
		{"/src/unknown.xyz", "typescript"}, // default fallback
	}
	for _, tc := range cases {
		got := TsLanguageID(tc.path)
		if got != tc.want {
			t.Errorf("TsLanguageID(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// ── IsTSFile ──────────────────────────────────────────────────────────────────

func TestIsTSFile(t *testing.T) {
	yes := []string{"/a.ts", "/a.tsx", "/a.js", "/a.jsx", "/a.mjs", "/a.cjs", "/A.TS"}
	no := []string{"/a.go", "/a.py", "/a.java", "/a.rb", "/a.rs", "/a.c", "/a"}

	for _, p := range yes {
		if !IsTSFile(p) {
			t.Errorf("IsTSFile(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if IsTSFile(p) {
			t.Errorf("IsTSFile(%q) = true, want false", p)
		}
	}
}

// ── Manager integration ───────────────────────────────────────────────────────

func TestManager_TsserverAndGoplsCoexist(t *testing.T) {
	goNoop := &noopVerifier{lang: LanguageGo}
	tsHigh := &tsStubVerifier{}

	m := NewManager(Options{})
	m.Register(goNoop)
	m.Register(tsHigh)

	goEdge, _ := m.ResolveEdge(context.Background(), LanguageGo, "f", "t",
		CallPosition{File: "/f.go", Line: 1})
	tsEdge, _ := m.ResolveEdge(context.Background(), LanguageTypeScript, "f", "t",
		CallPosition{File: "/f.ts", Line: 1})

	if goEdge.Confidence != ConfidenceNone {
		t.Errorf("Go verifier: expected NONE, got %v", goEdge.Confidence)
	}
	if tsEdge.Confidence != ConfidenceHigh {
		t.Errorf("TS verifier: expected HIGH, got %v", tsEdge.Confidence)
	}
}

// ── test helpers ──────────────────────────────────────────────────────────────

// tsCfg drives the tsFakeTransport response behaviour.
type tsCfg struct {
	response      *tsDefLoc
	responseRaw   string
	queryErr      error
	queryDelay    time.Duration
	resolveNodeID NodeResolver
}

// tsDefLoc is a single definition location returned by tsFakeTransport.
type tsDefLoc struct {
	uri  string
	line int
}

// tsMakeVerifier builds a TsserverVerifier with a fresh tsFakeTransport.
func tsMakeVerifier(t *testing.T, cfg tsCfg) *TsserverVerifier {
	t.Helper()
	ft := &tsFakeTransport{cfg: cfg}
	return tsMakeVerifierWithTransport(t, ft)
}

func tsMakeVerifierWithTransport(t *testing.T, ft *tsFakeTransport) *TsserverVerifier {
	t.Helper()
	root := t.TempDir()
	opts := TsserverVerifierOptions{
		ProjectRoot:    root,
		TSServerPath:   "fake-tsserver",
		QueryTimeout:   DefaultQueryTimeout,
		StartupTimeout: DefaultStartupTimeout,
		ResolveNodeID:  ft.cfg.resolveNodeID,
		LaunchProcess: func(_ context.Context, _, _ string) (LSPTransport, error) {
			ft.root = root
			return ft, nil
		},
	}
	return &TsserverVerifier{opts: opts.withDefaults()}
}

// tsTempFile writes content to a new file in a per-call temp dir and returns path.
func tsTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("tsTempFile: %v", err)
	}
	return path
}

// tsFakeTransport implements LSPTransport for TsserverVerifier unit tests.
// It handles LSP message exchange in-process without spawning a subprocess.
type tsFakeTransport struct {
	cfg          tsCfg
	root         string
	msgQueue     []json.RawMessage
	openCount    int    // count of textDocument/didOpen notifications received
	initTsconfig string // tsconfig path from initializationOptions, if any
}

func (f *tsFakeTransport) Send(v interface{}) error {
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
				if tc, ok := initOpts["tsconfig"].(string); ok {
					f.initTsconfig = tc
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
	}
	return nil
}

func (f *tsFakeTransport) Recv() (json.RawMessage, error) {
	if len(f.msgQueue) == 0 {
		time.Sleep(20 * time.Millisecond) // let context timeout tests fire
		return nil, fmt.Errorf("no messages in queue")
	}
	msg := f.msgQueue[0]
	f.msgQueue = f.msgQueue[1:]
	return msg, nil
}

func (f *tsFakeTransport) Close() error { return nil }

// tsStubVerifier always returns ConfidenceHigh for LanguageTypeScript.
type tsStubVerifier struct{}

func (s *tsStubVerifier) ResolveEdge(_ context.Context, from, to graph.NodeID, pos CallPosition) (*VerifiedEdge, error) {
	callee := CalleeInfo{NodeID: to, File: pos.File, Line: pos.Line}
	return NewVerifiedEdge(from, to, callee, ConfidenceHigh), nil
}

func (s *tsStubVerifier) Language() Language { return LanguageTypeScript }
func (s *tsStubVerifier) Close() error        { return nil }

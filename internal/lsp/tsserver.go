package lsp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TsserverVerifierOptions configures a TsserverVerifier.
type TsserverVerifierOptions struct {
	// ProjectRoot is the directory typescript-language-server is started in.
	// Should be the directory containing the project's tsconfig.json (or its
	// ancestor). Required.
	ProjectRoot string

	// TSServerPath is the absolute or PATH-relative path to the
	// typescript-language-server binary (the npm package, not the TypeScript
	// compiler's tsserver directly). If empty, TsserverVerifier searches PATH.
	// If not found, all ResolveEdge calls return ConfidenceNone.
	TSServerPath string

	// ResolveNodeID, when non-nil, maps definition file:line → graph NodeID.
	// Used to populate CalleeInfo.NodeID so callers can determine whether the
	// tree-sitter guess matched the LSP result (VerifiedEdge.Confirmed).
	// When nil, Callee.NodeID is always empty (file:line is still populated).
	ResolveNodeID NodeResolver

	// QueryTimeout caps each textDocument/definition round-trip.
	// Defaults to DefaultQueryTimeout.
	QueryTimeout time.Duration

	// StartupTimeout caps the Initialize handshake.
	// Defaults to DefaultStartupTimeout.
	StartupTimeout time.Duration

	// LaunchProcess is the factory for LSPTransport. When nil the production
	// factory (newTsserverTransport) is used. Override in tests to inject a fake
	// transport without requiring the typescript-language-server binary.
	LaunchProcess func(ctx context.Context, tsserverPath, root string) (LSPTransport, error)
}

func (o *TsserverVerifierOptions) withDefaults() TsserverVerifierOptions {
	out := *o
	if out.QueryTimeout <= 0 {
		out.QueryTimeout = DefaultQueryTimeout
	}
	if out.StartupTimeout <= 0 {
		out.StartupTimeout = DefaultStartupTimeout
	}
	if out.LaunchProcess == nil {
		out.LaunchProcess = newTsserverTransport
	}
	return out
}

// TsserverVerifier implements EdgeVerifier using typescript-language-server as
// the TypeScript type-system oracle. It starts the server lazily on the first
// ResolveEdge call, communicates using the Language Server Protocol over stdio,
// and shuts down when Close is called (or when Manager fires its idle timeout).
//
// On restart after Close the verifier re-initialises transparently on the next
// ResolveEdge call.
//
// TsserverVerifier is safe for concurrent use: a mutex serialises all subprocess
// I/O so that simultaneous ResolveEdge calls are queued rather than racing.
type TsserverVerifier struct {
	opts TsserverVerifierOptions

	mu        sync.Mutex
	transport LSPTransport   // nil when not started
	nextID    int64
	openFiles map[string]int // path → didOpen version counter
}

// NewTsserverVerifier constructs a TsserverVerifier.
// If opts.TSServerPath is empty, the binary is located via PATH under the name
// "typescript-language-server". If not found, every ResolveEdge call returns
// ConfidenceNone without error.
// Call Register(v) on a lsp.Manager to activate this verifier.
func NewTsserverVerifier(opts TsserverVerifierOptions) *TsserverVerifier {
	o := opts.withDefaults()

	if o.TSServerPath == "" {
		if p, err := exec.LookPath("typescript-language-server"); err == nil {
			o.TSServerPath = p
		}
		// If not found, TSServerPath remains "". ResolveEdge detects this and
		// returns ConfidenceNone gracefully.
	}

	return &TsserverVerifier{opts: o}
}

// Language returns LanguageTypeScript — this verifier handles TypeScript projects.
func (t *TsserverVerifier) Language() Language { return LanguageTypeScript }

// ResolveEdge queries typescript-language-server for the go-to-definition
// result at pos and returns a VerifiedEdge with the resolved callee information.
//
// ResolveEdge only handles TypeScript and JavaScript files (extensions
// .ts, .tsx, .js, .jsx, .mjs, .cjs). Any other extension returns ConfidenceNone.
//
// When tsserver is not available, not yet analysed the position, or returns an
// empty result, ResolveEdge returns ConfidenceNone without error.
// Transient I/O failures are returned as errors; the Manager propagates them.
func (ts *TsserverVerifier) ResolveEdge(ctx context.Context, from, to graph.NodeID, pos CallPosition) (*VerifiedEdge, error) {
	if ts.opts.TSServerPath == "" {
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}
	if pos.File == "" {
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}
	if !IsTSFile(pos.File) {
		// Not a TypeScript/JavaScript file — this verifier cannot help.
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	if err := ts.ensureStarted(); err != nil {
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, ts.opts.QueryTimeout)
	defer cancel()

	locs, err := ts.queryDefinition(queryCtx, pos)
	if err != nil {
		ts.teardown()
		return nil, fmt.Errorf("tsserver definition query: %w", err)
	}
	if len(locs) == 0 {
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}

	loc := locs[0]
	defFile := uriToPath(loc.URI)
	defLine := loc.Range.Start.Line

	var nodeID graph.NodeID
	if ts.opts.ResolveNodeID != nil {
		nodeID = ts.opts.ResolveNodeID(defFile, defLine)
	}

	callee := CalleeInfo{
		NodeID:        nodeID,
		File:          defFile,
		Line:          defLine,
		QualifiedName: loc.QualifiedName,
	}
	return NewVerifiedEdge(from, to, callee, ConfidenceHigh), nil
}

// Close shuts down the typescript-language-server subprocess. Idempotent.
// The next ResolveEdge call after Close will re-start the server lazily.
func (ts *TsserverVerifier) Close() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.teardown()
	return nil
}

// ── internal ──────────────────────────────────────────────────────────────────

// ensureStarted starts typescript-language-server and runs the LSP Initialize
// handshake if the transport is not already running.
// Must be called with ts.mu held.
func (ts *TsserverVerifier) ensureStarted() error {
	if ts.transport != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), ts.opts.StartupTimeout)
	defer cancel()

	tr, err := ts.opts.LaunchProcess(ctx, ts.opts.TSServerPath, ts.opts.ProjectRoot)
	if err != nil {
		return fmt.Errorf("launch tsserver: %w", err)
	}

	if err := ts.initialize(ctx, tr); err != nil {
		_ = tr.Close()
		return fmt.Errorf("tsserver initialize: %w", err)
	}

	ts.transport = tr
	ts.nextID = 2 // initialize used id=1; next request starts at 2
	ts.openFiles = make(map[string]int)
	return nil
}

// teardown closes the transport and resets state. Must be called with ts.mu held.
func (ts *TsserverVerifier) teardown() {
	if ts.transport != nil {
		_ = ts.transport.Close()
		ts.transport = nil
	}
	ts.openFiles = nil
}

// initialize performs the LSP Initialize + Initialized handshake with
// typescript-language-server.
func (ts *TsserverVerifier) initialize(ctx context.Context, tr LSPTransport) error {
	rootURI := pathToURI(ts.opts.ProjectRoot)

	// Build initializationOptions. Pass tsconfig.json location if discoverable
	// so tsserver can load the correct compiler options immediately.
	initOpts := map[string]interface{}{}
	if tscfg := FindTsconfig(ts.opts.ProjectRoot); tscfg != "" {
		initOpts["tsconfig"] = tscfg
	}

	req := lspRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]interface{}{
			"processId": os.Getpid(),
			"rootUri":   rootURI,
			"capabilities": map[string]interface{}{
				"textDocument": map[string]interface{}{
					"definition": map[string]interface{}{
						"dynamicRegistration": false,
						"linkSupport":         false,
					},
				},
			},
			"initializationOptions": initOpts,
		},
	}
	if err := tr.Send(req); err != nil {
		return err
	}

	if _, err := readResponseWithID(ctx, tr, 1); err != nil {
		return fmt.Errorf("initialize response: %w", err)
	}

	notif := lspNotification{
		JSONRPC: "2.0",
		Method:  "initialized",
		Params:  map[string]interface{}{},
	}
	return tr.Send(notif)
}

// queryDefinition opens the file if needed, then sends textDocument/definition.
// Returns the first resolved location(s). Must be called with ts.mu held.
func (ts *TsserverVerifier) queryDefinition(ctx context.Context, pos CallPosition) ([]lspLocation, error) {
	if err := ts.ensureOpen(pos.File); err != nil {
		return nil, err
	}

	id := ts.nextID
	ts.nextID++

	req := lspRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "textDocument/definition",
		Params: map[string]interface{}{
			"textDocument": map[string]interface{}{
				"uri": pathToURI(pos.File),
			},
			"position": map[string]interface{}{
				"line":      pos.Line,
				"character": pos.Col,
			},
		},
	}
	if err := ts.transport.Send(req); err != nil {
		return nil, err
	}

	raw, err := readResponseWithID(ctx, ts.transport, id)
	if err != nil {
		return nil, err
	}
	if raw == nil || string(raw) == "null" {
		return nil, nil
	}

	return parseLocations(raw)
}

// ensureOpen sends textDocument/didOpen for path if not yet opened this session.
// Must be called with ts.mu held.
func (ts *TsserverVerifier) ensureOpen(path string) error {
	if _, ok := ts.openFiles[path]; ok {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read source file for didOpen %s: %w", path, err)
	}

	version := 1
	ts.openFiles[path] = version

	notif := lspNotification{
		JSONRPC: "2.0",
		Method:  "textDocument/didOpen",
		Params: map[string]interface{}{
			"textDocument": map[string]interface{}{
				"uri":        pathToURI(path),
				"languageId": TsLanguageID(path),
				"version":    version,
				"text":       string(data),
			},
		},
	}
	return ts.transport.Send(notif)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// IsTSFile reports whether path is a TypeScript or JavaScript file supported
// by typescript-language-server. Exported for use by callers that filter files
// before querying the verifier.
func IsTSFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return true
	default:
		return false
	}
}

// TsLanguageID returns the LSP languageId for a TypeScript/JavaScript file.
// The languageId is sent in textDocument/didOpen so tsserver applies the
// correct language service. Exported for use by callers that build their own
// didOpen notifications or need to map file extensions to LSP language IDs.
func TsLanguageID(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	default:
		return "typescript"
	}
}

// FindTsconfig walks upward from dir looking for a tsconfig.json file.
// Returns the absolute path of the first tsconfig.json found, or "" if none.
// This mirrors how typescript-language-server itself discovers the project
// configuration when started in a subdirectory. Exported for use by project
// detectors that need to locate the TypeScript configuration root.
func FindTsconfig(dir string) string {
	current := dir
	for {
		candidate := filepath.Join(current, "tsconfig.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached filesystem root — tsconfig.json not found.
			return ""
		}
		current = parent
	}
}

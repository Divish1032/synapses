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

// PyrightVerifierOptions configures a PyrightVerifier.
type PyrightVerifierOptions struct {
	// ProjectRoot is the directory pyright-langserver is started in.
	// Should be the directory containing pyrightconfig.json (or its ancestor).
	// Required.
	ProjectRoot string

	// PyrightPath is the absolute or PATH-relative path to the
	// pyright-langserver binary (from the npm package "pyright"). If empty,
	// PyrightVerifier searches PATH. If not found, all ResolveEdge calls
	// return ConfidenceNone.
	//
	// Note: use "pyright-langserver", not "pyright" — the latter is the
	// type-checker CLI, not the Language Server Protocol binary.
	PyrightPath string

	// PythonPath is the path to the Python interpreter for type resolution.
	// When provided, it is passed to pyright-langserver via initializationOptions
	// so Pyright can locate installed packages. If empty, Pyright uses its
	// built-in interpreter detection.
	PythonPath string

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
	// factory (newPyrightTransport) is used. Override in tests to inject a fake
	// transport without requiring the pyright-langserver binary.
	LaunchProcess func(ctx context.Context, pyrightPath, root string) (LSPTransport, error)
}

func (o *PyrightVerifierOptions) withDefaults() PyrightVerifierOptions {
	out := *o
	if out.QueryTimeout <= 0 {
		out.QueryTimeout = DefaultQueryTimeout
	}
	if out.StartupTimeout <= 0 {
		out.StartupTimeout = DefaultStartupTimeout
	}
	if out.LaunchProcess == nil {
		out.LaunchProcess = newPyrightTransport
	}
	return out
}

// PyrightVerifier implements EdgeVerifier using pyright-langserver as the
// Python type-system oracle. It starts the server lazily on the first
// ResolveEdge call, communicates using the Language Server Protocol over stdio,
// and shuts down when Close is called (or when Manager fires its idle timeout).
//
// PyrightVerifier only handles Python source files (.py, .pyi). Calls for
// other file types return ConfidenceNone immediately without spawning a process.
//
// On restart after Close the verifier re-initialises transparently on the next
// ResolveEdge call.
//
// PyrightVerifier is safe for concurrent use: a mutex serialises all subprocess
// I/O so that simultaneous ResolveEdge calls are queued rather than racing.
type PyrightVerifier struct {
	opts PyrightVerifierOptions

	mu        sync.Mutex
	transport LSPTransport   // nil when not started
	nextID    int64
	openFiles map[string]int // path → didOpen version counter
}

// NewPyrightVerifier constructs a PyrightVerifier.
// If opts.PyrightPath is empty, the binary is located via PATH under the name
// "pyright-langserver". If not found, every ResolveEdge call returns
// ConfidenceNone without error.
// Call Register(v) on a lsp.Manager to activate this verifier.
func NewPyrightVerifier(opts PyrightVerifierOptions) *PyrightVerifier {
	o := opts.withDefaults()

	if o.PyrightPath == "" {
		if p, err := exec.LookPath("pyright-langserver"); err == nil {
			o.PyrightPath = p
		}
		// If not found, PyrightPath remains "". ResolveEdge detects this and
		// returns ConfidenceNone gracefully.
	}

	return &PyrightVerifier{opts: o}
}

// Language returns LanguagePython — this verifier handles Python projects.
func (pv *PyrightVerifier) Language() Language { return LanguagePython }

// ResolveEdge queries pyright-langserver for the go-to-definition result at
// pos and returns a VerifiedEdge with the resolved callee information.
//
// ResolveEdge only handles Python source files (.py, .pyi). Any other
// extension returns ConfidenceNone.
//
// When pyright-langserver is not available, not yet analysed the position, or
// returns an empty result, ResolveEdge returns ConfidenceNone without error.
// Transient I/O failures are returned as errors; the Manager propagates them.
func (pv *PyrightVerifier) ResolveEdge(ctx context.Context, from, to graph.NodeID, pos CallPosition) (*VerifiedEdge, error) {
	if pv.opts.PyrightPath == "" {
		// pyright-langserver not found on this machine — degrade gracefully.
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}
	if pos.File == "" {
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}
	if !IsPythonFile(pos.File) {
		// Not a Python file — this verifier cannot help.
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}

	pv.mu.Lock()
	defer pv.mu.Unlock()

	if err := pv.ensureStarted(); err != nil {
		// Start failure is non-fatal: return ConfidenceNone so the caller
		// continues with tree-sitter's best guess.
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, pv.opts.QueryTimeout)
	defer cancel()

	locs, err := pv.queryDefinition(queryCtx, pos)
	if err != nil {
		// I/O error — the process may have crashed. Tear down so the next call
		// triggers a fresh start.
		pv.teardown()
		return nil, fmt.Errorf("pyright definition query: %w", err)
	}
	if len(locs) == 0 {
		// Pyright resolved nothing (e.g. a stdlib function with no project source).
		// Not an error — ConfidenceNone is correct.
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}

	loc := locs[0]
	defFile := uriToPath(loc.URI)
	defLine := loc.Range.Start.Line

	var nodeID graph.NodeID
	if pv.opts.ResolveNodeID != nil {
		nodeID = pv.opts.ResolveNodeID(defFile, defLine)
	}

	callee := CalleeInfo{
		NodeID:        nodeID,
		File:          defFile,
		Line:          defLine,
		QualifiedName: loc.QualifiedName,
	}
	return NewVerifiedEdge(from, to, callee, ConfidenceHigh), nil
}

// Close shuts down the pyright-langserver subprocess. Idempotent.
// The next ResolveEdge call after Close will re-start the server lazily.
func (pv *PyrightVerifier) Close() error {
	pv.mu.Lock()
	defer pv.mu.Unlock()
	pv.teardown()
	return nil
}

// ── internal ──────────────────────────────────────────────────────────────────

// ensureStarted starts pyright-langserver and runs the LSP Initialize
// handshake if the transport is not already running.
// Must be called with pv.mu held.
func (pv *PyrightVerifier) ensureStarted() error {
	if pv.transport != nil {
		return nil // already running
	}

	ctx, cancel := context.WithTimeout(context.Background(), pv.opts.StartupTimeout)
	defer cancel()

	tr, err := pv.opts.LaunchProcess(ctx, pv.opts.PyrightPath, pv.opts.ProjectRoot)
	if err != nil {
		return fmt.Errorf("launch pyright-langserver: %w", err)
	}

	if err := pv.initialize(ctx, tr); err != nil {
		_ = tr.Close()
		return fmt.Errorf("pyright initialize: %w", err)
	}

	pv.transport = tr
	pv.nextID = 2 // initialize used id=1; next request starts at 2
	pv.openFiles = make(map[string]int)
	return nil
}

// teardown closes the transport and resets state. Must be called with pv.mu held.
func (pv *PyrightVerifier) teardown() {
	if pv.transport != nil {
		_ = pv.transport.Close()
		pv.transport = nil
	}
	pv.openFiles = nil
}

// initialize performs the LSP Initialize + Initialized handshake with
// pyright-langserver.
func (pv *PyrightVerifier) initialize(ctx context.Context, tr LSPTransport) error {
	rootURI := pathToURI(pv.opts.ProjectRoot)

	// Build initializationOptions. Pass pyrightconfig.json location and
	// Python interpreter path when available so Pyright can resolve imports.
	initOpts := map[string]interface{}{}
	if cfg := FindPyrightConfig(pv.opts.ProjectRoot); cfg != "" {
		initOpts["pyrightconfig"] = cfg
	}
	if pv.opts.PythonPath != "" {
		initOpts["pythonPath"] = pv.opts.PythonPath
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
// Returns the resolved locations. Must be called with pv.mu held.
func (pv *PyrightVerifier) queryDefinition(ctx context.Context, pos CallPosition) ([]lspLocation, error) {
	if err := pv.ensureOpen(pos.File); err != nil {
		return nil, err
	}

	id := pv.nextID
	pv.nextID++

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
	if err := pv.transport.Send(req); err != nil {
		return nil, err
	}

	raw, err := readResponseWithID(ctx, pv.transport, id)
	if err != nil {
		return nil, err
	}
	if raw == nil || string(raw) == "null" {
		return nil, nil
	}

	return parseLocations(raw)
}

// ensureOpen sends textDocument/didOpen for path if not yet opened this session.
// Must be called with pv.mu held.
func (pv *PyrightVerifier) ensureOpen(path string) error {
	if _, ok := pv.openFiles[path]; ok {
		return nil // already opened this session
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read source file for didOpen %s: %w", path, err)
	}

	version := 1
	pv.openFiles[path] = version

	notif := lspNotification{
		JSONRPC: "2.0",
		Method:  "textDocument/didOpen",
		Params: map[string]interface{}{
			"textDocument": map[string]interface{}{
				"uri":        pathToURI(path),
				"languageId": "python",
				"version":    version,
				"text":       string(data),
			},
		},
	}
	return pv.transport.Send(notif)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// IsPythonFile reports whether path is a Python source or stub file supported
// by pyright-langserver. Exported for use by callers that filter files before
// querying the verifier.
func IsPythonFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py", ".pyi":
		return true
	default:
		return false
	}
}

// FindPyrightConfig walks upward from dir looking for a pyrightconfig.json file.
// Returns the absolute path of the first pyrightconfig.json found, or "" if none.
// This mirrors how pyright-langserver itself discovers the project configuration
// when started in a subdirectory. Exported for use by project detectors that need
// to locate the Pyright configuration root.
func FindPyrightConfig(dir string) string {
	current := dir
	for {
		candidate := filepath.Join(current, "pyrightconfig.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached filesystem root — pyrightconfig.json not found.
			return ""
		}
		current = parent
	}
}

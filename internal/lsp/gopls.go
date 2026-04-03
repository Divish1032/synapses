package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// DefaultQueryTimeout is the maximum time GoplsVerifier waits for a single
// gopls response. The Sprint 28 success criterion is <500ms per verification
// query; this timeout caps the tail while leaving headroom for cold starts.
const DefaultQueryTimeout = 2 * time.Second

// DefaultStartupTimeout is the maximum time to wait for gopls to respond to
// the Initialize handshake. gopls typically responds in <1s on a warm machine.
const DefaultStartupTimeout = 10 * time.Second

// NodeResolver maps a definition location (absolute file path, zero-indexed
// line) to the corresponding Synapses graph NodeID.
// Returns an empty NodeID when no graph node exists at that location (e.g. a
// stdlib function or a method in a vendored dependency).
type NodeResolver func(file string, line int) graph.NodeID

// GoplsVerifierOptions configures a GoplsVerifier.
type GoplsVerifierOptions struct {
	// ProjectRoot is the directory gopls is started in (usually the go.mod root).
	// Required.
	ProjectRoot string

	// GoplsPath is the absolute or PATH-relative path to the gopls binary.
	// If empty, GoplsVerifier searches PATH at construction time.
	// If gopls is not found, all ResolveEdge calls return ConfidenceNone.
	GoplsPath string

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
	// factory (newGoplsTransport) is used. Override in tests to inject a fake
	// transport without requiring the gopls binary on the test machine.
	LaunchProcess func(ctx context.Context, goplsPath, root string) (LSPTransport, error)
}

func (o *GoplsVerifierOptions) withDefaults() GoplsVerifierOptions {
	out := *o
	if out.QueryTimeout <= 0 {
		out.QueryTimeout = DefaultQueryTimeout
	}
	if out.StartupTimeout <= 0 {
		out.StartupTimeout = DefaultStartupTimeout
	}
	if out.LaunchProcess == nil {
		out.LaunchProcess = newGoplsTransport
	}
	return out
}

// GoplsVerifier implements EdgeVerifier using gopls as the Go type-system
// oracle. It starts gopls lazily on the first ResolveEdge call, communicates
// using the Language Server Protocol over stdio, and shuts down when Close is
// called (or when the Manager fires its idle timeout and calls Close).
//
// On restart after Close the verifier re-initialises gopls transparently on
// the next ResolveEdge call.
//
// GoplsVerifier is safe for concurrent use: a mutex serialises all subprocess
// I/O so that simultaneous ResolveEdge calls are queued rather than racing.
type GoplsVerifier struct {
	opts GoplsVerifierOptions

	mu        sync.Mutex
	transport LSPTransport   // nil when not started
	nextID    int64
	openFiles map[string]int // path → didOpen version counter
}

// NewGoplsVerifier constructs a GoplsVerifier.
// If opts.GoplsPath is empty, the binary is located via PATH. If gopls is not
// found, every ResolveEdge call returns ConfidenceNone without error.
// Call Register(v) on a lsp.Manager to activate this verifier.
func NewGoplsVerifier(opts GoplsVerifierOptions) *GoplsVerifier {
	o := opts.withDefaults()

	if o.GoplsPath == "" {
		if p, err := exec.LookPath("gopls"); err == nil {
			o.GoplsPath = p
		}
		// If not found, GoplsPath remains "". ResolveEdge detects this and
		// returns ConfidenceNone gracefully.
	}

	return &GoplsVerifier{opts: o}
}

// Language returns LanguageGo — this verifier handles Go projects.
func (g *GoplsVerifier) Language() Language { return LanguageGo }

// ResolveEdge queries gopls for the go-to-definition result at pos and returns
// a VerifiedEdge with the resolved callee information.
//
// When gopls is not available, not yet analysed the position, or returns an
// empty result, ResolveEdge returns ConfidenceNone without error.
// Transient I/O failures are returned as errors; the Manager will propagate
// them to the caller.
func (g *GoplsVerifier) ResolveEdge(ctx context.Context, from, to graph.NodeID, pos CallPosition) (*VerifiedEdge, error) {
	if g.opts.GoplsPath == "" {
		// gopls not found on this machine — degrade gracefully.
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}
	if pos.File == "" {
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.ensureStarted(); err != nil {
		// Start failure is non-fatal: return ConfidenceNone so the caller
		// continues with tree-sitter's best guess.
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}

	// Honour the caller's context for the overall operation but impose our own
	// query deadline so a hung gopls does not block indefinitely.
	queryCtx, cancel := context.WithTimeout(ctx, g.opts.QueryTimeout)
	defer cancel()

	locs, err := g.queryDefinition(queryCtx, pos)
	if err != nil {
		// I/O error — the process may have crashed. Tear down so the next call
		// triggers a fresh start.
		g.teardown()
		return nil, fmt.Errorf("gopls definition query: %w", err)
	}
	if len(locs) == 0 {
		// gopls resolved nothing (e.g. the call is to a stdlib function with
		// no project source). Not an error — ConfidenceNone is correct.
		return &VerifiedEdge{From: from, To: to, Confidence: ConfidenceNone}, nil
	}

	loc := locs[0]
	defFile := uriToPath(loc.URI)
	defLine := loc.Range.Start.Line // zero-indexed, matches CallPosition.Line

	var nodeID graph.NodeID
	if g.opts.ResolveNodeID != nil {
		nodeID = g.opts.ResolveNodeID(defFile, defLine)
	}

	callee := CalleeInfo{
		NodeID:        nodeID,
		File:          defFile,
		Line:          defLine,
		QualifiedName: loc.QualifiedName,
	}
	return NewVerifiedEdge(from, to, callee, ConfidenceHigh), nil
}

// Close shuts down the gopls subprocess. Idempotent.
// The next ResolveEdge call after Close will re-start gopls lazily.
func (g *GoplsVerifier) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.teardown()
	return nil
}

// ── internal ─────────────────────────────────────────────────────────────────

// ensureStarted starts gopls and runs the LSP Initialize handshake if the
// transport is not already running. Must be called with g.mu held.
func (g *GoplsVerifier) ensureStarted() error {
	if g.transport != nil {
		return nil // already running
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.opts.StartupTimeout)
	defer cancel()

	t, err := g.opts.LaunchProcess(ctx, g.opts.GoplsPath, g.opts.ProjectRoot)
	if err != nil {
		return fmt.Errorf("launch gopls: %w", err)
	}

	if err := g.initialize(ctx, t); err != nil {
		_ = t.Close()
		return fmt.Errorf("gopls initialize: %w", err)
	}

	g.transport = t
	g.nextID = 2 // initialize used id=1; next request starts at 2
	g.openFiles = make(map[string]int)
	return nil
}

// teardown closes the transport and resets state. Must be called with g.mu held.
func (g *GoplsVerifier) teardown() {
	if g.transport != nil {
		_ = g.transport.Close()
		g.transport = nil
	}
	g.openFiles = nil
}

// initialize performs the LSP Initialize + Initialized handshake.
func (g *GoplsVerifier) initialize(ctx context.Context, t LSPTransport) error {
	rootURI := pathToURI(g.opts.ProjectRoot)
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
			"initializationOptions": map[string]interface{}{},
		},
	}
	if err := t.Send(req); err != nil {
		return err
	}

	// Read responses until we get the InitializeResult (id == 1).
	// gopls may emit log messages before the response — discard them.
	if _, err := readResponseWithID(ctx, t, 1); err != nil {
		return fmt.Errorf("initialize response: %w", err)
	}

	// Send the mandatory Initialized notification.
	notif := lspNotification{
		JSONRPC: "2.0",
		Method:  "initialized",
		Params:  map[string]interface{}{},
	}
	return t.Send(notif)
}

// queryDefinition opens the file if needed, then sends textDocument/definition.
// Returns the first resolved location(s). Must be called with g.mu held.
func (g *GoplsVerifier) queryDefinition(ctx context.Context, pos CallPosition) ([]lspLocation, error) {
	if err := g.ensureOpen(pos.File); err != nil {
		return nil, err
	}

	id := g.nextID
	g.nextID++

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
	if err := g.transport.Send(req); err != nil {
		return nil, err
	}

	raw, err := readResponseWithID(ctx, g.transport, id)
	if err != nil {
		return nil, err
	}
	if raw == nil || string(raw) == "null" {
		return nil, nil
	}

	return parseLocations(raw)
}

// ensureOpen sends textDocument/didOpen for path if it has not been opened in
// this gopls session. Reading the file from disk is necessary because the LSP
// spec requires the full text on open. Must be called with g.mu held.
func (g *GoplsVerifier) ensureOpen(path string) error {
	if _, ok := g.openFiles[path]; ok {
		return nil // already opened this session
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read source file for didOpen %s: %w", path, err)
	}

	version := 1
	g.openFiles[path] = version

	notif := lspNotification{
		JSONRPC: "2.0",
		Method:  "textDocument/didOpen",
		Params: map[string]interface{}{
			"textDocument": map[string]interface{}{
				"uri":        pathToURI(path),
				"languageId": "go",
				"version":    version,
				"text":       string(data),
			},
		},
	}
	return g.transport.Send(notif)
}

// ── LSP protocol helpers ──────────────────────────────────────────────────────

type lspRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type lspNotification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type lspEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"` // present on notifications/requests from server
	Result  json.RawMessage `json:"result"`
	Error   *lspError       `json:"error"`
}

type lspError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *lspError) Error() string {
	return fmt.Sprintf("LSP error %d: %s", e.Code, e.Message)
}

// lspPosition is a zero-indexed line + character offset per LSP spec.
type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

// lspLocation is the result of textDocument/definition.
// QualifiedName is not part of standard LSP — it is populated from the
// symbol name extracted from the definition URI + range when possible.
type lspLocation struct {
	URI           string   `json:"uri"`
	Range         lspRange `json:"range"`
	QualifiedName string   `json:"-"` // derived, not from wire
}

// lspLocationLink is the optional LocationLink format from newer LSP clients.
type lspLocationLink struct {
	TargetURI   string   `json:"targetUri"`
	TargetRange lspRange `json:"targetRange"`
}

// readResponseWithID reads messages from t until it receives a response whose
// id matches wantID, respecting ctx for cancellation. Server-initiated
// notifications and responses to other IDs are silently discarded — for
// our synchronous single-request pattern this is always correct.
func readResponseWithID(ctx context.Context, t LSPTransport, wantID int64) (json.RawMessage, error) {
	type result struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan result, 1)

	go func() {
		for {
			msg, err := t.Recv()
			if err != nil {
				ch <- result{err: err}
				return
			}
			var env lspEnvelope
			if err := json.Unmarshal(msg, &env); err != nil {
				continue // skip malformed message
			}
			if env.ID == nil {
				continue // notification — no ID, discard
			}
			if *env.ID != wantID {
				continue // response to a different request — discard
			}
			if env.Error != nil {
				ch <- result{err: env.Error}
				return
			}
			ch <- result{raw: env.Result}
			return
		}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for LSP response id=%d: %w", wantID, ctx.Err())
	case r := <-ch:
		return r.raw, r.err
	}
}

// parseLocations parses the textDocument/definition result, which may be:
//   - null
//   - a single Location object
//   - an array of Location objects
//   - an array of LocationLink objects
func parseLocations(raw json.RawMessage) ([]lspLocation, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	// Try array first (most common from gopls).
	if raw[0] == '[' {
		// Try []lspLocation
		var locs []lspLocation
		if err := json.Unmarshal(raw, &locs); err == nil && len(locs) > 0 && locs[0].URI != "" {
			return locs, nil
		}
		// Try []lspLocationLink (LocationLink format)
		var links []lspLocationLink
		if err := json.Unmarshal(raw, &links); err == nil {
			out := make([]lspLocation, 0, len(links))
			for _, l := range links {
				if l.TargetURI != "" {
					out = append(out, lspLocation{URI: l.TargetURI, Range: l.TargetRange})
				}
			}
			return out, nil
		}
		return nil, fmt.Errorf("unparseable definition array: %s", raw)
	}

	// Single Location object.
	var loc lspLocation
	if err := json.Unmarshal(raw, &loc); err != nil {
		return nil, fmt.Errorf("unparseable definition object: %w", err)
	}
	if loc.URI == "" {
		return nil, nil
	}
	return []lspLocation{loc}, nil
}

// pathToURI converts an absolute filesystem path to a file:// URI.
// Handles both Unix (/abs/path) and Windows (C:\abs\path) paths.
func pathToURI(path string) string {
	// Normalise separator to forward slash for the URI path component.
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		// Windows absolute path: C:/foo → /C:/foo
		path = "/" + path
	}
	return "file://" + path
}

// uriToPath converts a file:// URI to an absolute filesystem path.
func uriToPath(uri string) string {
	path := strings.TrimPrefix(uri, "file://")
	path = filepath.FromSlash(path)
	// On Windows the path is /C:/foo — strip the leading slash.
	if len(path) >= 3 && path[0] == os.PathSeparator && path[2] == ':' {
		path = path[1:]
	}
	return path
}

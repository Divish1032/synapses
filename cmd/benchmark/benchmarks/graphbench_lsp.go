// graphbench_lsp.go — LSP call hierarchy runner for GraphBench comparison.
//
// Wraps internal/lsp to provide call hierarchy queries from function definition
// positions (file:line) obtained from get_context format=json responses.
// Used only when --gb-compare-lsp is passed; the graphbench core does not depend
// on it otherwise.
package benchmarks

import (
	"context"
	"time"

	"github.com/SynapsesOS/synapses/internal/lsp"
)

// defaultLSPQueryTimeout caps each LSP call hierarchy round-trip.
// The benchmark cannot afford to wait longer — LSP must be <500ms per query.
const defaultLSPQueryTimeout = 500 * time.Millisecond

// defaultLSPStartupTimeout caps the LSP initialize handshake.
// gopls and tsserver both start within 3s on cold disk; 10s is generous.
const defaultLSPStartupTimeout = 10 * time.Second

// LSPBenchRunner wraps an LSP verifier for GraphBench call hierarchy comparison.
// It uses textDocument/prepareCallHierarchy + callHierarchy/incomingCalls /
// callHierarchy/outgoingCalls (LSP 3.16) to resolve callers and callees from
// function definition positions.
//
// Definition positions (file:line) are obtained from get_context format=json
// responses (contextResponse.Root.File / Root.Line), making this approach
// independent of call-site positions that are absent from graph edges.
//
// Only Go and TypeScript are supported; all other languages return nil, nil from
// NewLSPBenchRunner.
type LSPBenchRunner struct {
	provider lsp.CallHierarchyProvider
	ev       lsp.EdgeVerifier // same underlying object, held for Close()
	lang     string
}

// NewLSPBenchRunner creates an LSP runner for the given language and repo root.
// Returns nil, nil if the language is not supported or the LSP binary is not
// found in PATH. The caller must call Close() when done.
func NewLSPBenchRunner(lang, repoRoot string) (*LSPBenchRunner, error) {
	switch lang {
	case "go":
		v := lsp.NewGoplsVerifier(lsp.GoplsVerifierOptions{
			ProjectRoot:    repoRoot,
			QueryTimeout:   defaultLSPQueryTimeout,
			StartupTimeout: defaultLSPStartupTimeout,
		})
		chp, ok := any(v).(lsp.CallHierarchyProvider)
		if !ok {
			return nil, nil
		}
		return &LSPBenchRunner{provider: chp, ev: v, lang: lang}, nil

	case "typescript", "javascript":
		v := lsp.NewTsserverVerifier(lsp.TsserverVerifierOptions{
			ProjectRoot:    repoRoot,
			QueryTimeout:   defaultLSPQueryTimeout,
			StartupTimeout: defaultLSPStartupTimeout,
		})
		chp, ok := any(v).(lsp.CallHierarchyProvider)
		if !ok {
			return nil, nil
		}
		return &LSPBenchRunner{provider: chp, ev: v, lang: lang}, nil

	default:
		return nil, nil
	}
}

// QueryCallers returns names of functions that directly call the function
// defined at file:line (zero-indexed per LSP spec). Returns nil if LSP cannot
// resolve the position or the call hierarchy query fails.
func (r *LSPBenchRunner) QueryCallers(ctx context.Context, file string, line int) []string {
	pos := lsp.CallPosition{File: file, Line: line, Col: 0}
	items, err := r.provider.PrepareCallHierarchy(ctx, pos)
	if err != nil || len(items) == 0 {
		return nil
	}
	callers, err := r.provider.IncomingCalls(ctx, items[0])
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(callers))
	for _, c := range callers {
		if c.Name != "" {
			names = append(names, c.Name)
		}
	}
	return names
}

// QueryCallees returns names of functions directly called by the function
// defined at file:line (zero-indexed per LSP spec). Returns nil if LSP cannot
// resolve the position or the call hierarchy query fails.
func (r *LSPBenchRunner) QueryCallees(ctx context.Context, file string, line int) []string {
	pos := lsp.CallPosition{File: file, Line: line, Col: 0}
	items, err := r.provider.PrepareCallHierarchy(ctx, pos)
	if err != nil || len(items) == 0 {
		return nil
	}
	callees, err := r.provider.OutgoingCalls(ctx, items[0])
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(callees))
	for _, c := range callees {
		if c.Name != "" {
			names = append(names, c.Name)
		}
	}
	return names
}

// Close shuts down the underlying LSP process. Idempotent.
func (r *LSPBenchRunner) Close() error {
	if r.ev != nil {
		return r.ev.Close()
	}
	return nil
}

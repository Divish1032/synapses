package lsp

import (
	"context"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// EdgeVerifier resolves ambiguous CALLS edges using type-system information.
// Implementations are expected to be long-lived (per-language process) with
// lazy startup semantics: start on first use, idle after inactivity.
//
// Each method must be safe for concurrent use from multiple goroutines.
type EdgeVerifier interface {
	// ResolveEdge queries the type system to determine the definitive callee at
	// the given call position. The pos parameter is the primary query key —
	// LSP go-to-definition works on file:line:col, not on NodeIDs.
	//
	// Parameters:
	//   - ctx:  caller context; cancellation stops an in-flight LSP query.
	//   - from: NodeID of the calling function (used for cache key + logging).
	//   - to:   NodeID of the current tree-sitter guess; empty if unresolved.
	//   - pos:  file:line:col of the call expression (zero-indexed per LSP spec).
	//
	// Returns a non-nil *VerifiedEdge on success (including when Confidence is
	// ConfidenceNone — the caller should never receive nil without an error).
	// Returns an error only for transient failures (process crash, timeout);
	// a call that LSP cannot resolve is NOT an error — return ConfidenceNone.
	ResolveEdge(ctx context.Context, from, to graph.NodeID, pos CallPosition) (*VerifiedEdge, error)

	// Language returns the language this verifier handles.
	Language() Language

	// Close shuts down the underlying LSP process if running. Idempotent.
	// Called by Manager during shutdown or when idle timeout fires.
	Close() error
}

// noopVerifier is the default EdgeVerifier used when no LSP process is
// registered for a language. Returns ConfidenceNone with zero work.
type noopVerifier struct {
	lang Language
}

// NoOpVerifier returns an EdgeVerifier that always returns ConfidenceNone.
// It is safe for concurrent use and imposes zero overhead.
// Used when LSP is not configured or not yet started for a language.
func NoOpVerifier(lang Language) EdgeVerifier {
	return &noopVerifier{lang: lang}
}

// ResolveEdge returns a VerifiedEdge with ConfidenceNone. No LSP query is made.
func (n *noopVerifier) ResolveEdge(_ context.Context, from, to graph.NodeID, _ CallPosition) (*VerifiedEdge, error) {
	return &VerifiedEdge{
		From:       from,
		To:         to,
		Confidence: ConfidenceNone,
	}, nil
}

// Language returns the language this no-op verifier represents.
func (n *noopVerifier) Language() Language { return n.lang }

// Close is a no-op for the null implementation.
func (n *noopVerifier) Close() error { return nil }

// ── Call Hierarchy ────────────────────────────────────────────────────────────

// CallHierarchyProvider is an optional capability that EdgeVerifier
// implementations may provide. Use a type assertion to check availability:
//
//	if chp, ok := verifier.(lsp.CallHierarchyProvider); ok {
//	    items, err := chp.PrepareCallHierarchy(ctx, pos)
//	    ...
//	}
//
// Implementations must be safe for concurrent use from multiple goroutines.
//
// The call hierarchy protocol is a three-step query:
//  1. PrepareCallHierarchy: resolve a position to a callable item.
//  2. IncomingCalls: enumerate all direct callers of that item.
//  3. OutgoingCalls: enumerate all direct callees of that item.
//
// Steps 2 and 3 are independent — call one or both after step 1.
type CallHierarchyProvider interface {
	// PrepareCallHierarchy resolves the call hierarchy item at pos.
	// pos should point to a function or method definition (not a call site).
	// Returns an empty slice when pos does not resolve to a callable item,
	// or nil, nil when the verifier cannot answer (e.g. binary not available).
	PrepareCallHierarchy(ctx context.Context, pos CallPosition) ([]CallHierarchyItem, error)

	// IncomingCalls returns the direct callers of item (i.e. "find callers").
	// Each element describes one calling function, including its definition
	// location. Returns nil, nil on graceful failure.
	IncomingCalls(ctx context.Context, item CallHierarchyItem) ([]CallHierarchyItem, error)

	// OutgoingCalls returns the direct callees of item (i.e. "find callees").
	// Each element describes one called function. Returns nil, nil on graceful failure.
	OutgoingCalls(ctx context.Context, item CallHierarchyItem) ([]CallHierarchyItem, error)
}

// Package lsp implements the LSP-as-enrichment-oracle architecture for Synapses.
//
// # Design Philosophy
//
// Tree-sitter builds the call graph via name-based heuristics. Ambiguous method
// calls (e.g. `store.Close()`) may resolve to the wrong callee or remain
// unresolved. LSP answers targeted questions about these ambiguous edges with
// type-system precision — it is a verification oracle, NOT a graph replacement.
//
// The lifecycle is:
//
//  1. Tree-sitter parses all files → graph built with heuristic CALLS edges.
//  2. On ambiguous edges, the caller queries Manager.Get(lang).ResolveEdge(...).
//  3. If an LSP verifier is registered for the language, it starts lazily on the
//     first query, answers 10-50 questions to resolve ambiguous edges, then idles.
//  4. After [Options.IdleTimeout], the verifier process is killed to reclaim resources.
//  5. On next query, it starts again (lazy restart).
//
// # The Interface
//
//	ResolveEdge(ctx, from, to, pos) → *VerifiedEdge
//
//   - from: NodeID of the calling function (context, used for caching/logging)
//   - to:   NodeID of the current tree-sitter guess (empty if unresolved)
//   - pos:  CallPosition — file:line:col of the call expression (what LSP uses)
//   - →     VerifiedEdge with Confidence and the resolved callee info
//
// Confidence levels:
//
//   - ConfidenceHigh   — LSP or import-level verified (reliable)
//   - ConfidenceMedium — tree-sitter name match, consistent pattern (probable)
//   - ConfidenceLow    — heuristic, name-only (possible)
//   - ConfidenceNone   — unknown (LSP not available or call not resolved)
//
// # Null Implementation
//
// [NoOpVerifier] is the default when no LSP is registered. It always returns
// ConfidenceNone with zero overhead. This ensures all downstream code compiles
// and works correctly before any LSP integration is added.
//
// # Thread Safety
//
// [Manager] is safe for concurrent use after construction. [EdgeVerifier]
// implementations are responsible for their own internal synchronisation.
package lsp

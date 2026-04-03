// Package security — lsp_enricher.go: Sprint 28.5 LSP-triggered re-verification.
//
// When tree-sitter produces a security finding with MEDIUM or LOW confidence
// (name-based match, not type-verified), the LSPEnricher asks the language server
// whether the flagged entity actually exists at the expected source location. If the
// type system confirms the entity's identity, confidence is upgraded:
//
//	MEDIUM ("ast-call-pattern") → HIGH ("lsp-type-verified")
//	LOW    ("function-name-heuristic") → MEDIUM ("lsp-partial-verified")
//
// This directly eliminates false positives from name shadowing across packages —
// the most common cause of tree-sitter MEDIUM confidence findings being wrong.
//
// Only function-name Targets benefit from enrichment; route-path Targets (e.g.
// "/admin/users") have no corresponding graph node and are left unchanged.
// When LSP is unavailable or the binary is not installed, enrichment is a no-op
// and existing confidence values are preserved.
package security

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// SymbolVerifier confirms whether a code entity exists at a given source position
// using type-system information. Implemented by *lsp.Manager via VerifySymbol.
//
// lang is the language string ("go", "typescript", "python"). file is an absolute path.
// line and col are zero-indexed (LSP specification).
//
// Returns (true, nil) when the type system confirms the symbol's definition is at
// that exact file:line. Returns (false, nil) when unverifiable or unavailable.
// Returns (false, err) only for transient failures.
type SymbolVerifier interface {
	VerifySymbol(ctx context.Context, lang, file string, line, col int) (bool, error)
}

// LSPEnricher upgrades MEDIUM and LOW confidence security findings to higher
// confidence when LSP confirms the entity identity via the type system.
// Safe for concurrent use: it holds no mutable state beyond the SymbolVerifier
// reference, which must itself be safe for concurrent use.
type LSPEnricher struct {
	verifier SymbolVerifier
}

// NewLSPEnricher creates an LSPEnricher backed by the given SymbolVerifier.
// If verifier is nil, Enrich is a no-op (all violations are returned unchanged).
func NewLSPEnricher(verifier SymbolVerifier) *LSPEnricher {
	return &LSPEnricher{verifier: verifier}
}

// Enrich walks violations and upgrades confidence for any finding whose Target
// can be confirmed by the type system. The input slice is never modified in place;
// a new slice is returned with enriched copies.
//
// Enrichment rules:
//   - MEDIUM findings whose handler node is LSP-confirmed → upgraded to HIGH
//   - LOW findings whose handler node is LSP-confirmed → upgraded to MEDIUM
//   - HIGH findings → skipped (already maximally confident)
//   - Findings whose Target is a route path (not a function name) → skipped
//   - Findings whose File is not in the graph → skipped
//   - Any LSP error → finding unchanged (graceful degradation)
func (e *LSPEnricher) Enrich(ctx context.Context, violations []Violation, g *graph.Graph) []Violation {
	if e.verifier == nil || len(violations) == 0 || g == nil {
		return violations
	}

	// Copy to avoid mutating the caller's slice.
	result := make([]Violation, len(violations))
	copy(result, violations)

	for i, v := range result {
		if v.Confidence != ConfidenceMedium && v.Confidence != ConfidenceLow {
			continue // HIGH already, or unset — nothing to do
		}

		node := findViolationNode(g, v.File, v.Target)
		if node == nil {
			continue // Target is a route path or entity not in graph — skip
		}
		if node.Line <= 0 {
			continue // node has no valid line information — skip to avoid invalid LSP query
		}

		lang := languageForFile(v.File)
		if lang == "" {
			continue // unsupported language — no verifier available
		}

		// Node.Line is 1-indexed (Synapses convention); LSP expects 0-indexed.
		confirmed, err := e.verifier.VerifySymbol(ctx, lang, node.File, node.Line-1, 0)
		if err != nil || !confirmed {
			continue // LSP unavailable, process error, or definition mismatch — no change
		}

		// LSP confirmed: the entity tree-sitter identified is the same entity the
		// type system knows about. The finding is no longer a name guess.
		switch v.Confidence {
		case ConfidenceMedium:
			result[i].Confidence = ConfidenceHigh
			result[i].ConfidenceReason = "lsp-type-verified"
		case ConfidenceLow:
			result[i].Confidence = ConfidenceMedium
			result[i].ConfidenceReason = "lsp-partial-verified"
		}
	}

	return result
}

// findViolationNode looks up the graph node for a security violation's Target entity.
// Returns nil when the Target is a route path (starts with "/"), an import path
// (contains "/"), or no node with that name exists in the violation's file.
func findViolationNode(g *graph.Graph, file, target string) *graph.Node {
	if target == "" || file == "" {
		return nil
	}
	// Route paths and import paths cannot be graph node names.
	if strings.HasPrefix(target, "/") || strings.Contains(target, "/") {
		return nil
	}
	nodes := g.FindByFile(file)
	for _, n := range nodes {
		if n.Name == target {
			return n
		}
	}
	return nil
}

// languageForFile returns the LSP language key for a file path based on its
// extension. Returns an empty string for unsupported or unknown file types.
// The returned string matches the lang parameter accepted by SymbolVerifier.VerifySymbol
// and lsp.Manager.VerifySymbol (language strings, not language constants, to avoid
// an import cycle between security and lsp packages).
func languageForFile(file string) string {
	ext := strings.ToLower(filepath.Ext(file))
	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "typescript" // tsserver handles JS files when allowJs is set
	case ".py", ".pyi":
		return "python"
	default:
		return ""
	}
}

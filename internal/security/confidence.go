// Package security — confidence.go: detection confidence levels for security findings.
//
// Sprint 28.4: Every security finding carries a ConfidenceLevel that tells the agent
// HOW certain Synapses is about the finding, independent of severity (which tells
// the agent WHAT to do).
//
// Confidence levels map to detection method quality:
//
//	HIGH   — import-level check or literal value match; no type inference needed.
//	         Examples: unknown package (registry lookup), hardcoded secret (regex on
//	         actual literal), direct import from handler (import path string match).
//
//	MEDIUM — tree-sitter AST pattern match; name-based, not type-verified.
//	         Examples: missing auth middleware (call graph name match), missing annotation
//	         (annotation name check), observed norm deviation (sibling ratio).
//
//	LOW    — heuristic; function-name BFS, not type-resolved.
//	         Example: data flow path check (BFS through call names — "may lack validation").
//
// LSP integration (Sprint 28.5) will upgrade MEDIUM findings to HIGH when gopls/tsserver
// confirms the type of the matched call. Until then, MEDIUM is the correct default for
// AST name-based patterns.
package security

// ConfidenceLevel represents how certain Synapses is about a security finding.
// It is orthogonal to Severity (which drives agent behavior) — a finding can be
// CRITICAL severity with MEDIUM confidence, meaning the agent should act but should
// also verify the finding is not a false positive.
type ConfidenceLevel string

const (
	// ConfidenceHigh means the finding was produced by an exact check that requires
	// no type inference: import path string match, literal value regex, or registry
	// lookup. False positive rate is near zero.
	ConfidenceHigh ConfidenceLevel = "HIGH"

	// ConfidenceMedium means the finding was produced by a tree-sitter AST pattern
	// or structural norm observation. Name-based, not type-verified. False positive
	// rate is low but non-zero — LSP enrichment (Sprint 28.5) can upgrade to HIGH.
	ConfidenceMedium ConfidenceLevel = "MEDIUM"

	// ConfidenceLow means the finding was produced by a heuristic: BFS through call
	// graph node names without type resolution. Express as "may lack X" not "lacks X".
	// False positive rate is meaningful — treat as a prompt to verify, not a finding
	// to act on immediately.
	ConfidenceLow ConfidenceLevel = "LOW"
)

// ConfidenceForCheckType returns the default ConfidenceLevel for a given CheckType.
// This is the mapping used by CheckFile and CheckProject to stamp confidence on
// all pattern-produced violations. CheckNorms and CheckImports set confidence
// directly in their own code paths.
func ConfidenceForCheckType(ct CheckType) ConfidenceLevel {
	switch ct {
	case CheckTypeDirectImport:
		// Import path is compared as an exact string against a forbidden list.
		return ConfidenceHigh
	case CheckTypeHardcodedSecret:
		// Regex match against the actual string literal value — not a name guess.
		return ConfidenceHigh
	case CheckTypeLayerMapping:
		// Import-path structural analysis across the full import graph.
		return ConfidenceHigh
	case CheckTypeMissingMiddleware:
		// Tree-sitter call graph: checks whether a function with a matching name
		// is reachable from the handler. Name-based, not type-verified.
		return ConfidenceMedium
	case CheckTypeMissingAnnotation:
		// Annotation name match in Metadata["signature"] — not type-resolved.
		return ConfidenceMedium
	case CheckTypeAdminElevation:
		// Route path pattern + handler name match — structural, but name-based.
		return ConfidenceMedium
	case CheckTypeCrossTransportAuth:
		// Cross-transport structural analysis; transport type detection is name-based.
		return ConfidenceMedium
	case CheckTypeDataFlowPath:
		// BFS through call names — "may lack validation" heuristic, not type-resolved.
		return ConfidenceLow
	default:
		return ConfidenceMedium
	}
}

// ConfidenceReasonForCheckType returns a short explanation of WHY a check type
// produces its confidence level. Included in the ConfidenceReason field of Violation
// so agents and users understand the basis for the confidence assessment.
func ConfidenceReasonForCheckType(ct CheckType) string {
	switch ct {
	case CheckTypeDirectImport:
		return "import-path-match"
	case CheckTypeHardcodedSecret:
		return "literal-value-match"
	case CheckTypeLayerMapping:
		return "import-path-analysis"
	case CheckTypeMissingMiddleware:
		return "ast-call-pattern"
	case CheckTypeMissingAnnotation:
		return "ast-annotation-pattern"
	case CheckTypeAdminElevation:
		return "path-name-pattern"
	case CheckTypeCrossTransportAuth:
		return "structural-observation"
	case CheckTypeDataFlowPath:
		return "function-name-heuristic"
	default:
		return "ast-pattern"
	}
}

// setFindingConfidence stamps Confidence and ConfidenceReason on every violation
// in the slice. Called immediately after each check algorithm dispatch in CheckFile
// and CheckProject. Idempotent: safe to call on an already-stamped slice.
// If violations is nil or empty, this is a no-op.
func setFindingConfidence(violations []Violation, level ConfidenceLevel, reason string) {
	for i := range violations {
		violations[i].Confidence = level
		violations[i].ConfidenceReason = reason
	}
}

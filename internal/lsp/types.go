package lsp

import "github.com/SynapsesOS/synapses/internal/graph"

// Confidence classifies how reliable a resolved edge is.
// Levels are assigned by the verifier based on the evidence source.
type Confidence int

const (
	// ConfidenceNone means the edge could not be verified. The tree-sitter
	// heuristic result should be used as-is (or the edge treated as ambiguous).
	// Returned by NoOpVerifier and when LSP is unavailable or unresponsive.
	ConfidenceNone Confidence = iota

	// ConfidenceLow is a heuristic or name-only match with no structural backing.
	// Acceptable for informational display but not for security enforcement.
	ConfidenceLow

	// ConfidenceMedium is a tree-sitter name-based match with consistent pattern
	// support (e.g. the receiver type appears in the import list). Reliable for
	// most queries; upgrade to High via LSP when security decisions depend on it.
	ConfidenceMedium

	// ConfidenceHigh is an import-level or LSP-verified result. The callee is
	// definitively identified by the type system. Safe for security enforcement.
	ConfidenceHigh
)

// String returns the human-readable confidence label used in tool responses.
func (c Confidence) String() string {
	switch c {
	case ConfidenceHigh:
		return "HIGH"
	case ConfidenceMedium:
		return "MEDIUM"
	case ConfidenceLow:
		return "LOW"
	default:
		return "NONE"
	}
}

// Language identifies the programming language for LSP verifier selection.
type Language string

const (
	// LanguageGo selects the gopls verifier (Sprint 28.2).
	LanguageGo Language = "go"
	// LanguageTypeScript selects the tsserver verifier (Sprint 28.3).
	LanguageTypeScript Language = "typescript"
	// LanguagePython selects the Pyright verifier (Sprint 28.6, stretch).
	LanguagePython Language = "python"
)

// CallPosition is the source location of a call expression in a file.
// This is the primary input to LSP go-to-definition queries. Line and Col
// are zero-indexed to match the LSP specification (Position.line, Position.character).
type CallPosition struct {
	// File is the absolute path to the source file containing the call.
	File string
	// Line is the zero-indexed line number of the call expression.
	Line int
	// Col is the zero-indexed character offset within the line (UTF-16 code units,
	// as required by the LSP specification).
	Col int
}

// CalleeInfo holds the resolved callee information returned by LSP.
// Populated when Confidence >= ConfidenceMedium; may be zero when ConfidenceNone.
type CalleeInfo struct {
	// NodeID is the Synapses graph node for the resolved callee.
	// Empty when LSP returned a location that doesn't correspond to any graph node
	// (e.g. a stdlib function not present in the project graph).
	NodeID graph.NodeID

	// File is the absolute path to the file containing the callee definition.
	// Always populated when LSP resolves the location (even if NodeID is empty).
	File string

	// Line is the zero-indexed line of the callee definition in File.
	Line int

	// QualifiedName is the fully-qualified name of the callee as reported by LSP
	// (e.g. "(*database/sql.DB).Close" for Go, "sql.DB.Close" for TypeScript).
	// Used for display and deduplication; may be empty when Confidence is Low.
	QualifiedName string
}

// VerifiedEdge is the result of an LSP edge resolution query.
// It enriches the tree-sitter call graph with type-system precision.
type VerifiedEdge struct {
	// From is the NodeID of the calling function (input, unchanged from query).
	From graph.NodeID

	// To is the NodeID from the tree-sitter heuristic guess (input).
	// May be empty if tree-sitter did not resolve the call at all.
	To graph.NodeID

	// Callee is the LSP-resolved callee information.
	// Only meaningful when Confidence >= ConfidenceMedium.
	Callee CalleeInfo

	// Confidence classifies the reliability of this resolution.
	Confidence Confidence

	// Confirmed is true when LSP resolution agrees with the tree-sitter guess
	// (i.e. Callee.NodeID == To and both are non-empty). False means the edge
	// target changed, was previously unresolved, or To was empty.
	//
	// Invariant: Confirmed == (Callee.NodeID != "" && Callee.NodeID == To).
	// Use NewVerifiedEdge to construct correctly and enforce this invariant.
	Confirmed bool
}

// NewVerifiedEdge constructs a VerifiedEdge with the Confirmed field derived
// automatically from whether Callee.NodeID matches to. Verifier implementations
// should use this constructor to avoid setting Confirmed incorrectly.
func NewVerifiedEdge(from, to graph.NodeID, callee CalleeInfo, conf Confidence) *VerifiedEdge {
	return &VerifiedEdge{
		From:       from,
		To:         to,
		Callee:     callee,
		Confidence: conf,
		Confirmed:  callee.NodeID != "" && callee.NodeID == to,
	}
}

// ── Call Hierarchy ────────────────────────────────────────────────────────────

// CallHierarchyItem represents a function or method that appears in a call
// hierarchy result. Produced by PrepareCallHierarchy and consumed as input to
// IncomingCalls and OutgoingCalls queries.
//
// Positions are zero-indexed per the LSP specification.
type CallHierarchyItem struct {
	// Name is the short symbol name (e.g. "Use", "Close", "handleLogin").
	Name string
	// Detail is the fully-qualified name when the LSP server provides it
	// (e.g. "(*gin.Engine).Use" for Go, "Router.use" for TypeScript).
	// Empty when the server omits this field.
	Detail string
	// File is the absolute path to the file containing the symbol definition.
	File string
	// Line is the zero-indexed line of the symbol's selection range start.
	Line int
	// Col is the zero-indexed character offset of the selection range start.
	Col int
}

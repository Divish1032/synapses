package security

import (
	"context"
	"errors"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// mockVerifier implements SymbolVerifier for tests.
type mockVerifier struct {
	// confirmed maps "file:line" (zero-indexed) to whether VerifySymbol returns true.
	confirmed map[string]bool
	// err, when non-nil, is returned for all calls.
	err error
}

func (m *mockVerifier) VerifySymbol(_ context.Context, _, file string, line, _ int) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	if m.confirmed == nil {
		return false, nil
	}
	import_key := file + ":" + itoa(line)
	return m.confirmed[import_key], nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// buildGraph constructs a minimal graph containing a single function node.
func buildGraph(t *testing.T, file, funcName string, line int) *graph.Graph {
	t.Helper()
	g := graph.New("test")
	g.SetRoot("/repo")
	n := &graph.Node{
		ID:   graph.NodeID("n1"),
		Name: funcName,
		File: file,
		Line: line,
		Type: graph.NodeFunction,
	}
	g.AddNode(n)
	return g
}

// buildViolation creates a minimal Violation for tests.
func buildViolation(file, target string, confidence ConfidenceLevel, reason string) Violation {
	return Violation{
		PatternID:        "test-pattern",
		PatternName:      "Test Pattern",
		Severity:         SeverityMedium,
		Action:           "inform",
		File:             file,
		Target:           target,
		Message:          "test finding",
		Evidence:         "test evidence",
		Confidence:       confidence,
		ConfidenceReason: reason,
	}
}

func TestEnrich_NilVerifier(t *testing.T) {
	e := NewLSPEnricher(nil)
	v := buildViolation("/repo/handler.go", "adminHandler", ConfidenceMedium, "ast-call-pattern")
	g := graph.New("t")
	g.SetRoot("/repo")
	got := e.Enrich(context.Background(), []Violation{v}, g)
	if got[0].Confidence != ConfidenceMedium {
		t.Errorf("nil verifier: want MEDIUM, got %s", got[0].Confidence)
	}
}

func TestEnrich_EmptyViolations(t *testing.T) {
	e := NewLSPEnricher(&mockVerifier{})
	g := graph.New("t")
	g.SetRoot("/repo")
	got := e.Enrich(context.Background(), nil, g)
	if len(got) != 0 {
		t.Errorf("want 0 violations, got %d", len(got))
	}
}

func TestEnrich_NilGraph(t *testing.T) {
	e := NewLSPEnricher(&mockVerifier{})
	v := buildViolation("/repo/handler.go", "adminHandler", ConfidenceMedium, "ast-call-pattern")
	got := e.Enrich(context.Background(), []Violation{v}, nil)
	if got[0].Confidence != ConfidenceMedium {
		t.Errorf("nil graph: want MEDIUM, got %s", got[0].Confidence)
	}
}

func TestEnrich_HighConfidenceSkipped(t *testing.T) {
	e := NewLSPEnricher(&mockVerifier{confirmed: map[string]bool{"/repo/handler.go:44": true}})
	g := buildGraph(t, "/repo/handler.go", "adminHandler", 45)
	v := buildViolation("/repo/handler.go", "adminHandler", ConfidenceHigh, "import-path-match")
	got := e.Enrich(context.Background(), []Violation{v}, g)
	if got[0].Confidence != ConfidenceHigh || got[0].ConfidenceReason != "import-path-match" {
		t.Errorf("HIGH should be skipped: got %s/%s", got[0].Confidence, got[0].ConfidenceReason)
	}
}

func TestEnrich_MediumConfirmedUpgradesToHigh(t *testing.T) {
	// Node at line 45 (1-indexed) → LSP query at line 44 (0-indexed).
	mv := &mockVerifier{confirmed: map[string]bool{"/repo/handler.go:44": true}}
	e := NewLSPEnricher(mv)
	g := buildGraph(t, "/repo/handler.go", "adminHandler", 45)
	v := buildViolation("/repo/handler.go", "adminHandler", ConfidenceMedium, "ast-call-pattern")

	got := e.Enrich(context.Background(), []Violation{v}, g)
	if got[0].Confidence != ConfidenceHigh {
		t.Errorf("want HIGH after LSP confirm, got %s", got[0].Confidence)
	}
	if got[0].ConfidenceReason != "lsp-type-verified" {
		t.Errorf("want reason lsp-type-verified, got %q", got[0].ConfidenceReason)
	}
}

func TestEnrich_MediumNotConfirmedUnchanged(t *testing.T) {
	// LSP returns false for this position → no upgrade.
	mv := &mockVerifier{confirmed: map[string]bool{}}
	e := NewLSPEnricher(mv)
	g := buildGraph(t, "/repo/handler.go", "adminHandler", 45)
	v := buildViolation("/repo/handler.go", "adminHandler", ConfidenceMedium, "ast-call-pattern")

	got := e.Enrich(context.Background(), []Violation{v}, g)
	if got[0].Confidence != ConfidenceMedium {
		t.Errorf("unconfirmed: want MEDIUM, got %s", got[0].Confidence)
	}
	if got[0].ConfidenceReason != "ast-call-pattern" {
		t.Errorf("reason should be unchanged, got %q", got[0].ConfidenceReason)
	}
}

func TestEnrich_LowConfirmedUpgradesToMedium(t *testing.T) {
	mv := &mockVerifier{confirmed: map[string]bool{"/repo/service.go:9": true}}
	e := NewLSPEnricher(mv)
	g := buildGraph(t, "/repo/service.go", "processInput", 10)
	v := buildViolation("/repo/service.go", "processInput", ConfidenceLow, "function-name-heuristic")

	got := e.Enrich(context.Background(), []Violation{v}, g)
	if got[0].Confidence != ConfidenceMedium {
		t.Errorf("low+confirm: want MEDIUM, got %s", got[0].Confidence)
	}
	if got[0].ConfidenceReason != "lsp-partial-verified" {
		t.Errorf("want lsp-partial-verified, got %q", got[0].ConfidenceReason)
	}
}

func TestEnrich_RoutePathTargetSkipped(t *testing.T) {
	mv := &mockVerifier{confirmed: map[string]bool{"/repo/handler.go:44": true}}
	e := NewLSPEnricher(mv)
	g := buildGraph(t, "/repo/handler.go", "adminHandler", 45)
	// Target is a route path, not a function name.
	v := buildViolation("/repo/handler.go", "/admin/users", ConfidenceMedium, "ast-call-pattern")

	got := e.Enrich(context.Background(), []Violation{v}, g)
	if got[0].Confidence != ConfidenceMedium {
		t.Errorf("route target: want unchanged MEDIUM, got %s", got[0].Confidence)
	}
}

func TestEnrich_NodeNotInGraph(t *testing.T) {
	mv := &mockVerifier{confirmed: map[string]bool{"/repo/handler.go:44": true}}
	e := NewLSPEnricher(mv)
	// Graph has a different function name.
	g := buildGraph(t, "/repo/handler.go", "otherHandler", 45)
	v := buildViolation("/repo/handler.go", "adminHandler", ConfidenceMedium, "ast-call-pattern")

	got := e.Enrich(context.Background(), []Violation{v}, g)
	if got[0].Confidence != ConfidenceMedium {
		t.Errorf("missing node: want MEDIUM unchanged, got %s", got[0].Confidence)
	}
}

func TestEnrich_LSPError_GracefulDegradation(t *testing.T) {
	mv := &mockVerifier{err: errors.New("gopls crashed")}
	e := NewLSPEnricher(mv)
	g := buildGraph(t, "/repo/handler.go", "adminHandler", 45)
	v := buildViolation("/repo/handler.go", "adminHandler", ConfidenceMedium, "ast-call-pattern")

	got := e.Enrich(context.Background(), []Violation{v}, g)
	if got[0].Confidence != ConfidenceMedium {
		t.Errorf("LSP error: want MEDIUM unchanged, got %s", got[0].Confidence)
	}
}

func TestEnrich_InputSliceNotMutated(t *testing.T) {
	mv := &mockVerifier{confirmed: map[string]bool{"/repo/handler.go:44": true}}
	e := NewLSPEnricher(mv)
	g := buildGraph(t, "/repo/handler.go", "adminHandler", 45)
	v := buildViolation("/repo/handler.go", "adminHandler", ConfidenceMedium, "ast-call-pattern")
	original := []Violation{v}

	_ = e.Enrich(context.Background(), original, g)
	if original[0].Confidence != ConfidenceMedium {
		t.Error("Enrich must not mutate the input slice")
	}
}

func TestEnrich_MultipleViolationsMixedConfidence(t *testing.T) {
	// One MEDIUM (confirmed), one LOW (not confirmed), one HIGH (skipped).
	mv := &mockVerifier{confirmed: map[string]bool{"/repo/handler.go:44": true}}
	e := NewLSPEnricher(mv)
	g := buildGraph(t, "/repo/handler.go", "adminHandler", 45)

	violations := []Violation{
		buildViolation("/repo/handler.go", "adminHandler", ConfidenceMedium, "ast-call-pattern"),
		buildViolation("/repo/handler.go", "adminHandler", ConfidenceLow, "function-name-heuristic"),
		buildViolation("/repo/handler.go", "adminHandler", ConfidenceHigh, "import-path-match"),
	}

	got := e.Enrich(context.Background(), violations, g)
	if got[0].Confidence != ConfidenceHigh {
		t.Errorf("[0] want HIGH, got %s", got[0].Confidence)
	}
	if got[1].Confidence != ConfidenceMedium {
		t.Errorf("[1] want MEDIUM (from LOW+confirm), got %s", got[1].Confidence)
	}
	if got[2].Confidence != ConfidenceHigh || got[2].ConfidenceReason != "import-path-match" {
		t.Errorf("[2] want HIGH/import-path-match unchanged, got %s/%s", got[2].Confidence, got[2].ConfidenceReason)
	}
}

func TestEnrich_NodeLineZero_Skipped(t *testing.T) {
	// Node with Line=0 must be skipped to avoid sending negative LSP line (Line-1 = -1).
	mv := &mockVerifier{confirmed: map[string]bool{"/repo/handler.go:-1": true}}
	e := NewLSPEnricher(mv)
	g := graph.New("t")
	g.SetRoot("/repo")
	n := &graph.Node{
		ID:   graph.NodeID("n1"),
		Name: "adminHandler",
		File: "/repo/handler.go",
		Line: 0, // pathological: no line info
		Type: graph.NodeFunction,
	}
	g.AddNode(n)
	v := buildViolation("/repo/handler.go", "adminHandler", ConfidenceMedium, "ast-call-pattern")

	got := e.Enrich(context.Background(), []Violation{v}, g)
	if got[0].Confidence != ConfidenceMedium {
		t.Errorf("Line=0 node: want MEDIUM unchanged, got %s", got[0].Confidence)
	}
}

func TestFindViolationNode_RoutePathReturnsNil(t *testing.T) {
	g := buildGraph(t, "/repo/handler.go", "adminHandler", 45)
	if findViolationNode(g, "/repo/handler.go", "/admin/users") != nil {
		t.Error("route path should return nil")
	}
}

func TestFindViolationNode_ImportPathReturnsNil(t *testing.T) {
	g := buildGraph(t, "/repo/handler.go", "adminHandler", 45)
	if findViolationNode(g, "/repo/handler.go", "github.com/foo/bar") != nil {
		t.Error("import path should return nil")
	}
}

func TestLanguageForFile(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"handler.go", "go"},
		{"/abs/path/service.go", "go"},
		{"app.ts", "typescript"},
		{"component.tsx", "typescript"},
		{"index.js", "typescript"},
		{"main.mjs", "typescript"},
		{"auth.py", "python"},
		{"Cargo.rs", ""},
		{"build.gradle", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := languageForFile(tc.file); got != tc.want {
			t.Errorf("languageForFile(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

package security

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ──────────────────────────────────────────────────────────────────────────────
// CheckNorms tests (Sprint 27.5)
// ──────────────────────────────────────────────────────────────────────────────

// buildNormsGraph creates a graph with siblingCount route-registering sibling
// files each calling normCall, plus a target file with a route node. targetCalls
// controls what the target file calls (empty = target does NOT call normCall).
//
// Returns the graph, a zero-pattern Engine, and the absolute path to the target file.
func buildNormsGraph(
	t *testing.T,
	dir string,
	siblingCount int,
	normCall string,
	targetCalls ...string,
) (*graph.Graph, *Engine, string) {
	t.Helper()
	g := buildTestGraph(t)

	for i := 0; i < siblingCount; i++ {
		f := dir + "/sib" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), normCall)
	}

	target := dir + "/target.go"
	addFileWithImports(g, target)
	addRouteNode(g, target, "GET", "/target")
	if len(targetCalls) > 0 {
		addFunctionWithCalls(g, target, "RegisterTarget", targetCalls...)
	}

	return g, NewEngine(nil), target
}

// TestCheckNorms_HighConfidence verifies that 8/8 siblings calling "AuthMiddleware"
// produces a HIGH violation on the target file that doesn't call it.
func TestCheckNorms_HighConfidence(t *testing.T) {
	g, e, target := buildNormsGraph(t, "/project/internal/handler", 8, "AuthMiddleware")

	violations := e.CheckNorms(g, target)

	if len(violations) == 0 {
		t.Fatal("expected at least one norm violation, got none")
	}
	found := false
	for _, v := range violations {
		if !strings.Contains(v.PatternID, "AuthMiddleware") {
			continue
		}
		found = true
		if v.Severity != SeverityHigh {
			t.Errorf("severity = %q, want HIGH (8/8 siblings)", v.Severity)
		}
		if v.Action != "warn" {
			t.Errorf("action = %q, want warn", v.Action)
		}
		if !strings.Contains(v.Evidence, "8/8") {
			t.Errorf("evidence %q does not mention 8/8", v.Evidence)
		}
	}
	if !found {
		t.Errorf("no norm violation for AuthMiddleware; violations: %+v", violations)
	}
}

// TestCheckNorms_MediumConfidence verifies that 3/5 siblings (ratio=0.60) produces
// a MEDIUM violation.
func TestCheckNorms_MediumConfidence(t *testing.T) {
	const dir = "/project/internal/api"
	g := buildTestGraph(t)

	// 5 siblings: 3 call "RateLimit", 2 do not.
	for i := 0; i < 3; i++ {
		f := dir + "/has" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "RateLimit")
	}
	for i := 0; i < 2; i++ {
		f := dir + "/norate" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/s"+string(rune('a'+i)))
		// no calls to RateLimit
	}
	target := dir + "/target.go"
	addFileWithImports(g, target)
	addRouteNode(g, target, "GET", "/target")

	e := NewEngine(nil)
	violations := e.CheckNorms(g, target)

	found := false
	for _, v := range violations {
		if !strings.Contains(v.PatternID, "RateLimit") {
			continue
		}
		found = true
		if v.Severity != SeverityMedium {
			t.Errorf("severity = %q, want MEDIUM (3/5 siblings)", v.Severity)
		}
		if v.Action != "inform" {
			t.Errorf("action = %q, want inform", v.Action)
		}
		if !strings.Contains(v.Evidence, "3/5") {
			t.Errorf("evidence %q does not mention 3/5", v.Evidence)
		}
	}
	if !found {
		t.Errorf("no norm violation for RateLimit; violations: %+v", violations)
	}
}

// TestCheckNorms_BelowThreshold verifies that 1/3 siblings (ratio≈0.33) produces
// no violation — below the 50% minimum threshold.
func TestCheckNorms_BelowThreshold(t *testing.T) {
	const dir = "/project/internal/svc"
	g := buildTestGraph(t)

	for i := 0; i < 3; i++ {
		f := dir + "/sib" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		if i == 0 {
			addFunctionWithCalls(g, f, "Reg", "SpecialFunc")
		}
	}
	target := dir + "/target.go"
	addFileWithImports(g, target)
	addRouteNode(g, target, "GET", "/target")

	e := NewEngine(nil)
	for _, v := range e.CheckNorms(g, target) {
		if strings.Contains(v.PatternID, "SpecialFunc") {
			t.Errorf("unexpected violation for SpecialFunc at 1/3 (ratio 0.33 < 0.50): %+v", v)
		}
	}
}

// TestCheckNorms_TargetAlreadyCalls verifies no violation when the target calls
// the normed function.
func TestCheckNorms_TargetAlreadyCalls(t *testing.T) {
	g, e, target := buildNormsGraph(t, "/project/internal/ctrl", 4, "AuthMiddleware", "AuthMiddleware")

	for _, v := range e.CheckNorms(g, target) {
		if strings.Contains(v.PatternID, "AuthMiddleware") {
			t.Errorf("unexpected violation when target already calls AuthMiddleware: %+v", v)
		}
	}
}

// TestCheckNorms_NotARouteFile verifies no violation for files without NodeRoute nodes.
func TestCheckNorms_NotARouteFile(t *testing.T) {
	const dir = "/project/internal/util"
	g := buildTestGraph(t)

	for i := 0; i < 4; i++ {
		f := dir + "/sib" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "AuthMiddleware")
	}
	// target has no route node
	target := dir + "/helper.go"
	addFileWithImports(g, target)
	addFunctionWithCalls(g, target, "HelperFunc")

	e := NewEngine(nil)
	if v := e.CheckNorms(g, target); len(v) != 0 {
		t.Errorf("expected no violations for non-route file, got %+v", v)
	}
}

// TestCheckNorms_TooFewSiblings verifies no violation when only 1 sibling route
// file exists (can't establish a norm with fewer than 2).
func TestCheckNorms_TooFewSiblings(t *testing.T) {
	const dir = "/project/internal/lonely"
	g := buildTestGraph(t)

	sib := dir + "/sib.go"
	addFileWithImports(g, sib)
	addRouteNode(g, sib, "GET", "/r")
	addFunctionWithCalls(g, sib, "Reg", "AuthMiddleware")

	target := dir + "/target.go"
	addFileWithImports(g, target)
	addRouteNode(g, target, "GET", "/target")

	e := NewEngine(nil)
	if v := e.CheckNorms(g, target); len(v) != 0 {
		t.Errorf("expected no violations with only 1 sibling route file, got %+v", v)
	}
}

// TestCheckNorms_NilGuards verifies nil engine and nil graph don't panic and
// return nil.
func TestCheckNorms_NilGuards(t *testing.T) {
	g := buildTestGraph(t)
	const path = "/project/internal/h/target.go"

	var e *Engine
	if v := e.CheckNorms(g, path); v != nil {
		t.Errorf("nil engine: want nil, got %+v", v)
	}

	e2 := NewEngine(nil)
	if v := e2.CheckNorms(nil, path); v != nil {
		t.Errorf("nil graph: want nil, got %+v", v)
	}
	if v := e2.CheckNorms(g, ""); v != nil {
		t.Errorf("empty path: want nil, got %+v", v)
	}
}

// TestCheckNorms_TestFileSkipped verifies that test files (*_test.go) are never
// flagged regardless of sibling norms.
func TestCheckNorms_TestFileSkipped(t *testing.T) {
	const dir = "/project/internal/handler"
	g := buildTestGraph(t)

	for i := 0; i < 4; i++ {
		f := dir + "/sib" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "AuthMiddleware")
	}
	// Target is a test file — should always be skipped.
	target := dir + "/routes_test.go"
	addFileWithImports(g, target)
	addRouteNode(g, target, "GET", "/target")

	e := NewEngine(nil)
	if v := e.CheckNorms(g, target); len(v) != 0 {
		t.Errorf("expected no violations for test file, got %+v", v)
	}
}

// TestCheckNorms_SortOrder verifies that HIGH violations appear before MEDIUM
// ones in the output, and within the same tier violations are sorted alphabetically
// by PatternID.
func TestCheckNorms_SortOrder(t *testing.T) {
	const dir = "/project/internal/sorted"
	g := buildTestGraph(t)

	// 4 siblings: all call "HighNormFunc" (4/4=100% → HIGH with ≥3 siblings).
	// First 2 also call "MedNormFunc" (2/4=50% → MEDIUM).
	for i := 0; i < 4; i++ {
		f := dir + "/sib" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		if i < 2 {
			addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "HighNormFunc", "MedNormFunc")
		} else {
			addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "HighNormFunc")
		}
	}
	target := dir + "/target.go"
	addFileWithImports(g, target)
	addRouteNode(g, target, "GET", "/target")
	// target calls neither function

	e := NewEngine(nil)
	violations := e.CheckNorms(g, target)
	if len(violations) < 2 {
		t.Fatalf("expected ≥2 violations, got %d: %+v", len(violations), violations)
	}
	if violations[0].Severity != SeverityHigh {
		t.Errorf("first violation severity = %q, want HIGH", violations[0].Severity)
	}
	if violations[1].Severity != SeverityMedium {
		t.Errorf("second violation severity = %q, want MEDIUM", violations[1].Severity)
	}
}

// TestCheckNorms_ExactlyTwoSiblings verifies that with exactly 2 sibling route
// files both calling the norm (2/2 = 100%) the result is MEDIUM, not HIGH —
// because siblingRouteCount=2 is below the ≥3 threshold for HIGH.
func TestCheckNorms_ExactlyTwoSiblings(t *testing.T) {
	g, e, target := buildNormsGraph(t, "/project/internal/exact2", 2, "AuthMiddleware")

	violations := e.CheckNorms(g, target)

	found := false
	for _, v := range violations {
		if !strings.Contains(v.PatternID, "AuthMiddleware") {
			continue
		}
		found = true
		if v.Severity != SeverityMedium {
			t.Errorf("severity = %q, want MEDIUM (2 siblings < 3 required for HIGH)", v.Severity)
		}
		if v.Action != "inform" {
			t.Errorf("action = %q, want inform", v.Action)
		}
	}
	if !found {
		t.Errorf("expected a norm violation; got %+v", violations)
	}
}

// TestCheckNorms_SeventyFivePercentBoundary verifies that 3/4 siblings (exactly
// 75%) with ≥3 sibling route files produces a HIGH violation.
func TestCheckNorms_SeventyFivePercentBoundary(t *testing.T) {
	const dir = "/project/internal/boundary75"
	g := buildTestGraph(t)

	// 4 sibling route files: 3 call "AuthMiddleware", 1 does not.
	for i := 0; i < 3; i++ {
		f := dir + "/with" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "AuthMiddleware")
	}
	withoutSib := dir + "/without.go"
	addFileWithImports(g, withoutSib)
	addRouteNode(g, withoutSib, "GET", "/s")
	// withoutSib does NOT call AuthMiddleware

	target := dir + "/target.go"
	addFileWithImports(g, target)
	addRouteNode(g, target, "GET", "/target")

	e := NewEngine(nil)
	violations := e.CheckNorms(g, target)

	found := false
	for _, v := range violations {
		if !strings.Contains(v.PatternID, "AuthMiddleware") {
			continue
		}
		found = true
		if v.Severity != SeverityHigh {
			t.Errorf("severity = %q, want HIGH (3/4=75%% with 4≥3 siblings)", v.Severity)
		}
		if !strings.Contains(v.Evidence, "3/4") {
			t.Errorf("evidence %q does not mention 3/4", v.Evidence)
		}
	}
	if !found {
		t.Errorf("expected HIGH norm violation for AuthMiddleware; got %+v", violations)
	}
}

// TestCheckNorms_FiftyPercentBoundary verifies that exactly 1/2 siblings (50%)
// produces a MEDIUM violation — the minimum qualifying threshold.
func TestCheckNorms_FiftyPercentBoundary(t *testing.T) {
	const dir = "/project/internal/boundary50"
	g := buildTestGraph(t)

	// 2 sibling route files: only 1 calls "AuthMiddleware".
	with := dir + "/with.go"
	addFileWithImports(g, with)
	addRouteNode(g, with, "GET", "/r")
	addFunctionWithCalls(g, with, "Reg", "AuthMiddleware")

	without := dir + "/without.go"
	addFileWithImports(g, without)
	addRouteNode(g, without, "GET", "/s")
	// without does NOT call AuthMiddleware

	target := dir + "/target.go"
	addFileWithImports(g, target)
	addRouteNode(g, target, "GET", "/target")

	e := NewEngine(nil)
	violations := e.CheckNorms(g, target)

	found := false
	for _, v := range violations {
		if strings.Contains(v.PatternID, "AuthMiddleware") {
			found = true
			if v.Severity != SeverityMedium {
				t.Errorf("severity = %q, want MEDIUM (1/2=50%% exact boundary)", v.Severity)
			}
		}
	}
	if !found {
		t.Errorf("expected MEDIUM norm violation at exactly 50%% threshold; got %+v", violations)
	}
}

// TestCheckNorms_BareFilename verifies that a filePath with no directory component
// (dir == ".") does not panic and returns nil — no package scope can be inferred.
func TestCheckNorms_BareFilename(t *testing.T) {
	g := buildTestGraph(t)
	// Add a route node for the bare filename so we pass the route check.
	addFileWithImports(g, "target.go")
	addRouteNode(g, "target.go", "GET", "/target")

	e := NewEngine(nil)
	if v := e.CheckNorms(g, "target.go"); v != nil {
		t.Errorf("bare filename should return nil (dir=='.'), got %+v", v)
	}
}

// TestCheckNorms_MediumEvidence verifies that the Evidence field for a MEDIUM
// violation contains the correct ratio string.
func TestCheckNorms_MediumEvidence(t *testing.T) {
	const dir = "/project/internal/evidence"
	g := buildTestGraph(t)

	for i := 0; i < 3; i++ {
		f := dir + "/sib" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "RateLimiter")
	}
	for i := 0; i < 2; i++ {
		f := dir + "/no" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/s"+string(rune('a'+i)))
	}
	target := dir + "/target.go"
	addFileWithImports(g, target)
	addRouteNode(g, target, "GET", "/target")

	e := NewEngine(nil)
	violations := e.CheckNorms(g, target)

	for _, v := range violations {
		if !strings.Contains(v.PatternID, "RateLimiter") {
			continue
		}
		if !strings.Contains(v.Evidence, "3/5") {
			t.Errorf("evidence %q does not mention 3/5 ratio", v.Evidence)
		}
		if !strings.Contains(v.Message, "RateLimiter") {
			t.Errorf("message %q does not name the normed function", v.Message)
		}
	}
}

// TestCheckNorms_DifferentLanguagesIgnored verifies that sibling files in a
// different language (TypeScript vs Go) do not contribute to Go norms.
func TestCheckNorms_DifferentLanguagesIgnored(t *testing.T) {
	const dir = "/project/internal/mixed"
	g := buildTestGraph(t)

	// 3 TypeScript siblings each calling "Auth".
	for i := 0; i < 3; i++ {
		f := dir + "/sib" + string(rune('a'+i)) + ".ts"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "Auth")
	}
	// Go target — TS siblings must NOT influence Go norms.
	target := dir + "/target.go"
	addFileWithImports(g, target)
	addRouteNode(g, target, "GET", "/target")

	e := NewEngine(nil)
	if v := e.CheckNorms(g, target); len(v) != 0 {
		t.Errorf("TS siblings must not influence Go norms, got %+v", v)
	}
}

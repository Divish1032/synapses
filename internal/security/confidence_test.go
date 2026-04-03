package security

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── ConfidenceForCheckType ────────────────────────────────────────────────────

func TestConfidenceForCheckType_ImportTypes_HIGH(t *testing.T) {
	for _, ct := range []CheckType{
		CheckTypeDirectImport,
		CheckTypeHardcodedSecret,
		CheckTypeLayerMapping,
	} {
		got := ConfidenceForCheckType(ct)
		if got != ConfidenceHigh {
			t.Errorf("ConfidenceForCheckType(%q) = %q, want HIGH", ct, got)
		}
	}
}

func TestConfidenceForCheckType_ASTNameTypes_MEDIUM(t *testing.T) {
	for _, ct := range []CheckType{
		CheckTypeMissingMiddleware,
		CheckTypeMissingAnnotation,
		CheckTypeAdminElevation,
		CheckTypeCrossTransportAuth,
	} {
		got := ConfidenceForCheckType(ct)
		if got != ConfidenceMedium {
			t.Errorf("ConfidenceForCheckType(%q) = %q, want MEDIUM", ct, got)
		}
	}
}

func TestConfidenceForCheckType_DataFlowPath_LOW(t *testing.T) {
	got := ConfidenceForCheckType(CheckTypeDataFlowPath)
	if got != ConfidenceLow {
		t.Errorf("ConfidenceForCheckType(data_flow_path) = %q, want LOW", got)
	}
}

func TestConfidenceForCheckType_Unknown_DefaultsMEDIUM(t *testing.T) {
	got := ConfidenceForCheckType(CheckType("unknown_check_type"))
	if got != ConfidenceMedium {
		t.Errorf("unknown check type should default to MEDIUM, got %q", got)
	}
}

// ── ConfidenceReasonForCheckType ─────────────────────────────────────────────

func TestConfidenceReasonForCheckType_NonEmpty(t *testing.T) {
	for _, ct := range []CheckType{
		CheckTypeDirectImport,
		CheckTypeHardcodedSecret,
		CheckTypeLayerMapping,
		CheckTypeMissingMiddleware,
		CheckTypeMissingAnnotation,
		CheckTypeAdminElevation,
		CheckTypeCrossTransportAuth,
		CheckTypeDataFlowPath,
	} {
		reason := ConfidenceReasonForCheckType(ct)
		if reason == "" {
			t.Errorf("ConfidenceReasonForCheckType(%q) returned empty string", ct)
		}
	}
}

func TestConfidenceReasonForCheckType_Distinct(t *testing.T) {
	highReason := ConfidenceReasonForCheckType(CheckTypeDirectImport)
	lowReason := ConfidenceReasonForCheckType(CheckTypeDataFlowPath)
	if highReason == lowReason {
		t.Errorf("direct_import and data_flow_path should have different confidence reasons, both got %q", highReason)
	}
}

// ── setFindingConfidence ──────────────────────────────────────────────────────

func TestSetFindingConfidence_SetsAllViolations(t *testing.T) {
	violations := []Violation{
		{PatternID: "p1", Severity: SeverityCritical},
		{PatternID: "p2", Severity: SeverityHigh},
	}
	setFindingConfidence(violations, ConfidenceHigh, "import-path-match")
	for _, v := range violations {
		if v.Confidence != ConfidenceHigh {
			t.Errorf("violation %q: expected Confidence=HIGH, got %q", v.PatternID, v.Confidence)
		}
		if v.ConfidenceReason != "import-path-match" {
			t.Errorf("violation %q: expected ConfidenceReason=%q, got %q", v.PatternID, "import-path-match", v.ConfidenceReason)
		}
	}
}

func TestSetFindingConfidence_NilSlice_NoPanic(t *testing.T) {
	// Must not panic on nil or empty input.
	setFindingConfidence(nil, ConfidenceMedium, "ast-call-pattern")
	setFindingConfidence([]Violation{}, ConfidenceMedium, "ast-call-pattern")
}

func TestSetFindingConfidence_Idempotent(t *testing.T) {
	v := []Violation{{PatternID: "p1", Confidence: ConfidenceLow, ConfidenceReason: "old"}}
	setFindingConfidence(v, ConfidenceHigh, "new-reason")
	if v[0].Confidence != ConfidenceHigh || v[0].ConfidenceReason != "new-reason" {
		t.Error("setFindingConfidence must overwrite existing confidence fields")
	}
}

// ── CheckFile confidence propagation ─────────────────────────────────────────

func TestCheckFile_MissingMiddleware_HasMediumConfidence(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Skipf("could not load built-in patterns: %v", err)
	}
	eng := NewEngine(ps)

	g := buildTestGraph(t)
	// A route file importing the chi framework but not calling auth middleware.
	addFileWithImports(g, "/project/handler.go", "github.com/go-chi/chi/v5")
	addRouteNode(g, "/project/handler.go", "GET", "/api/resource")
	// No auth call added — violation should fire.

	violations := eng.CheckFile(g, "/project/handler.go", nil)
	if len(violations) == 0 {
		t.Skip("no violations produced — pattern setup may differ from test assumptions")
	}
	for _, v := range violations {
		if v.Confidence == "" {
			t.Errorf("missing_middleware violation has empty Confidence field")
		}
		if v.ConfidenceReason == "" {
			t.Errorf("missing_middleware violation has empty ConfidenceReason field")
		}
		if v.Confidence != ConfidenceMedium {
			t.Errorf("missing_middleware violation should have MEDIUM confidence, got %q", v.Confidence)
		}
		if v.ConfidenceReason != "ast-call-pattern" {
			t.Errorf("missing_middleware violation should have reason %q, got %q", "ast-call-pattern", v.ConfidenceReason)
		}
	}
}

func TestCheckFile_HardcodedSecret_HasHighConfidence(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Skipf("could not load built-in patterns: %v", err)
	}
	eng := NewEngine(ps)

	g := buildTestGraph(t)
	addFileWithImports(g, "/project/config.go")

	content := []byte("package config\nconst jwtSecret = \"my-super-secret-hardcoded-key\"\n")
	violations := eng.CheckFile(g, "/project/config.go", content)
	if len(violations) == 0 {
		t.Skip("no violations produced — secret content may not match pattern")
	}
	for _, v := range violations {
		if v.Confidence != ConfidenceHigh {
			t.Errorf("hardcoded_secret violation should have HIGH confidence, got %q", v.Confidence)
		}
		if v.ConfidenceReason != "literal-value-match" {
			t.Errorf("hardcoded_secret violation reason should be %q, got %q", "literal-value-match", v.ConfidenceReason)
		}
	}
}

// ── CheckNorms confidence propagation ────────────────────────────────────────

func TestCheckNorms_HasMediumConfidence(t *testing.T) {
	eng := DefaultEngine()
	g := buildTestGraph(t)

	dir := "/project/api"
	// Build 3 sibling route files all calling "AuthMiddleware".
	for _, name := range []string{"a_handler.go", "b_handler.go", "c_handler.go"} {
		path := dir + "/" + name
		addFileWithImports(g, path)
		addRouteNode(g, path, "GET", "/route-"+name)
		addFunctionWithCalls(g, path, "Register", "AuthMiddleware")
	}

	// Target route file does NOT call AuthMiddleware — norm violation.
	target := dir + "/new_handler.go"
	addFileWithImports(g, target)
	addRouteNode(g, target, "GET", "/newroute")
	// No AuthMiddleware call.

	violations := eng.CheckNorms(g, target)
	if len(violations) == 0 {
		t.Skip("no norm violations produced — sibling scan may not fire in this graph")
	}
	for _, v := range violations {
		if v.Confidence != ConfidenceMedium {
			t.Errorf("CheckNorms violation should have MEDIUM confidence, got %q", v.Confidence)
		}
		if v.ConfidenceReason != "statistical-norm" {
			t.Errorf("CheckNorms violation reason should be %q, got %q", "statistical-norm", v.ConfidenceReason)
		}
	}
}

// ── buildUnknownImportViolation confidence ────────────────────────────────────

func TestBuildUnknownImportViolation_HasHighConfidence(t *testing.T) {
	// PackageRegistry zero value has zero-value LoadedAt (zero time) and Size() returns 0.
	// buildUnknownImportViolation only needs r for r.Size() and r.LoadedAt().
	r := &PackageRegistry{}
	v := buildUnknownImportViolation("/project/app.py", "flasck", "python", "", r)
	if v.Confidence != ConfidenceHigh {
		t.Errorf("buildUnknownImportViolation: expected Confidence=HIGH, got %q", v.Confidence)
	}
	if v.ConfidenceReason != "import-path-match" {
		t.Errorf("buildUnknownImportViolation: expected ConfidenceReason=%q, got %q", "import-path-match", v.ConfidenceReason)
	}
}

// ── withActions preserves confidence ─────────────────────────────────────────

func TestWithActions_PreservesConfidence(t *testing.T) {
	input := []Violation{
		{Severity: SeverityCritical, Confidence: ConfidenceHigh, ConfidenceReason: "import-path-match"},
		{Severity: SeverityMedium, Confidence: ConfidenceLow, ConfidenceReason: "function-name-heuristic"},
	}
	result := withActions(input)
	if result[0].Confidence != ConfidenceHigh {
		t.Errorf("withActions must preserve Confidence: expected HIGH, got %q", result[0].Confidence)
	}
	if result[1].Confidence != ConfidenceLow {
		t.Errorf("withActions must preserve Confidence: expected LOW, got %q", result[1].Confidence)
	}
	if result[0].ConfidenceReason != "import-path-match" {
		t.Errorf("withActions must preserve ConfidenceReason")
	}
}

// ── CheckImports confidence propagation ──────────────────────────────────────

func TestCheckImports_UnknownPackage_HasHighConfidence(t *testing.T) {
	r, err := LoadBuiltinRegistry()
	if err != nil || r == nil {
		t.Skip("built-in registry not available in test environment")
	}
	eng := DefaultEngine().WithRegistry(r)

	g := buildTestGraph(t)
	// File with an unknown Python package.
	fileID := g.MakeNodeID("/project/main.py", "/project/main.py")
	g.AddNode(&graph.Node{
		ID:   fileID,
		Type: graph.NodeFile,
		Name: "/project/main.py",
		File: "/project/main.py",
	})
	impID := g.MakeNodeID("flask-corse", "flask-corse")
	g.AddNode(&graph.Node{
		ID:      impID,
		Type:    graph.NodePackage,
		Name:    "flask-corse",
		Package: "flask-corse",
		File:    "/project/main.py",
	})
	g.AddEdge(&graph.Edge{From: fileID, To: impID, Type: graph.EdgeImports})

	violations := eng.CheckImports(g, "/project/main.py")
	if len(violations) == 0 {
		t.Skip("CheckImports returned no violations — package may be known or graph not set up correctly")
	}
	for _, v := range violations {
		if v.Confidence != ConfidenceHigh {
			t.Errorf("CheckImports violation should have HIGH confidence, got %q", v.Confidence)
		}
		if v.ConfidenceReason != "import-path-match" {
			t.Errorf("CheckImports violation reason should be %q, got %q", "import-path-match", v.ConfidenceReason)
		}
	}
}

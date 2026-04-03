package security

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ──────────────────────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────────────────────

// layerViolationPattern returns a minimal SecurityPattern that activates
// checkLayerMapping with optional custom layers. Pass nil layerConfig to use
// built-in auto-inference.
func layerViolationPattern(layerConfig []LayerDef) SecurityPattern {
	return SecurityPattern{
		ID:          "test-layer-violation",
		Name:        "Test layer violation",
		Language:    "*",
		Framework:   "*",
		PatternType: PatternTypeLayerViolation,
		Severity:    SeverityMedium,
		Description: "test",
		Detection: Detection{
			CheckType:   CheckTypeLayerMapping,
			Scope:       ScopeProject,
			LayerConfig: layerConfig,
		},
		Message: "File {file} ({src_layer}) directly imports '{target}' ({dst_layer}).",
	}
}

// buildLayerGraph creates a graph with a single NodeFile and the given imports.
func buildLayerGraph(t *testing.T, filePath string, imports ...string) *graph.Graph {
	t.Helper()
	g := buildTestGraph(t)
	addFileWithImports(g, filePath, imports...)
	return g
}

// ──────────────────────────────────────────────────────────────────────────────
// inferLayerFromPath
// ──────────────────────────────────────────────────────────────────────────────

func TestInferLayerFromPath_DefaultConfig(t *testing.T) {
	layers := defaultLayerConfig()

	cases := []struct {
		path      string
		wantName  string
		wantIdx   int
	}{
		// presentation layer
		{"/project/internal/handler/orders.go", "presentation", 0},
		{"/project/internal/handlers/user.go", "presentation", 0},
		{"/project/pkg/api/v1/routes.go", "presentation", 0},
		{"/project/internal/controller/payment.go", "presentation", 0},
		{"/project/internal/controllers/billing.go", "presentation", 0},
		{"/project/internal/route/auth.go", "presentation", 0},
		{"/project/internal/routes/admin.go", "presentation", 0},
		{"/project/internal/transport/grpc.go", "presentation", 0},
		// service layer
		{"/project/internal/service/orders.go", "service", 1},
		{"/project/internal/services/user.go", "service", 1},
		{"/project/internal/usecase/place_order.go", "service", 1},
		{"/project/internal/usecases/auth.go", "service", 1},
		{"/project/internal/business/checkout.go", "service", 1},
		{"/project/internal/domain/order.go", "service", 1},
		// data layer
		{"/project/internal/repo/order.go", "data", 2},
		{"/project/internal/repository/user.go", "data", 2},
		{"/project/internal/repositories/payment.go", "data", 2},
		{"/project/internal/dal/db.go", "data", 2},
		// unknown layer
		{"/project/internal/config/settings.go", "", -1},
		{"/project/internal/utils/helper.go", "", -1},
		{"/project/cmd/main.go", "", -1},
		// import path with mixed segments
		{"github.com/myapp/internal/repository", "data", 2},
		{"github.com/myapp/internal/handler", "presentation", 0},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			name, idx := inferLayerFromPath(tc.path, layers)
			if name != tc.wantName || idx != tc.wantIdx {
				t.Errorf("inferLayerFromPath(%q) = (%q, %d), want (%q, %d)",
					tc.path, name, idx, tc.wantName, tc.wantIdx)
			}
		})
	}
}

func TestInferLayerFromPath_CaseInsensitive(t *testing.T) {
	layers := defaultLayerConfig()

	// Keywords should match regardless of case in the path.
	cases := []struct {
		path    string
		wantIdx int
	}{
		{"/project/HANDLER/orders.go", 0},   // uppercase
		{"/project/Handler/orders.go", 0},   // mixed case
		{"/project/Repository/user.go", 2},  // mixed case
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			_, idx := inferLayerFromPath(tc.path, layers)
			if idx != tc.wantIdx {
				t.Errorf("inferLayerFromPath(%q): got idx %d, want %d", tc.path, idx, tc.wantIdx)
			}
		})
	}
}

func TestInferLayerFromPath_ExactSegmentMatch(t *testing.T) {
	layers := defaultLayerConfig()

	// "repo-utils" should NOT match the "repo" keyword — not an exact segment.
	// Verify this by checking that the inferred layer is -1 (unknown).
	// Note: "repo-utils" as a single path segment won't split further, so "repo-utils" != "repo".
	name, idx := inferLayerFromPath("/project/repo-utils/helper.go", layers)
	if idx != -1 || name != "" {
		t.Errorf("repo-utils should not match repo keyword; got (%q, %d)", name, idx)
	}
}

func TestInferLayerFromPath_DeepestLayerWins(t *testing.T) {
	// A path that contains both "handler" (presentation) and "repository" (data)
	// should resolve to data (higher index wins).
	layers := defaultLayerConfig()
	// Contrived path: a repo package that happens to live inside a handler dir.
	name, idx := inferLayerFromPath("/project/handler/repository/user.go", layers)
	if name != "data" || idx != 2 {
		t.Errorf("expected data layer (2), got (%q, %d)", name, idx)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// checkLayerMapping — basic violation detection
// ──────────────────────────────────────────────────────────────────────────────

// TestCheckLayerMapping_PresentationToData verifies that a handler file
// directly importing an internal repository package produces a MEDIUM violation.
func TestCheckLayerMapping_PresentationToData(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/internal/handler/orders.go",
		"github.com/myapp/internal/repository",
	)

	p := layerViolationPattern(nil) // auto-infer layers
	violations := checkLayerMapping(g, p)

	if len(violations) == 0 {
		t.Fatal("expected violation for handler→repository import, got none")
	}
	v := violations[0]
	if v.Severity != SeverityMedium {
		t.Errorf("severity = %q, want MEDIUM", v.Severity)
	}
	if !strings.Contains(v.Evidence, "presentation") {
		t.Errorf("evidence should mention source layer 'presentation': %q", v.Evidence)
	}
	if !strings.Contains(v.Evidence, "data") {
		t.Errorf("evidence should mention dest layer 'data': %q", v.Evidence)
	}
	if !strings.Contains(v.Evidence, "service") {
		t.Errorf("evidence should mention skipped 'service' layer: %q", v.Evidence)
	}
	if v.Target != "github.com/myapp/internal/repository" {
		t.Errorf("target = %q, want import path", v.Target)
	}
}

// TestCheckLayerMapping_PresentationToService verifies that a handler→service
// import (adjacent layers) does NOT fire a violation.
func TestCheckLayerMapping_PresentationToService(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/internal/handler/orders.go",
		"github.com/myapp/internal/service",
	)
	p := layerViolationPattern(nil)
	violations := checkLayerMapping(g, p)
	if len(violations) != 0 {
		t.Errorf("expected no violation for handler→service (adjacent), got %d: %+v", len(violations), violations)
	}
}

// TestCheckLayerMapping_ServiceToData verifies that a service→repo import
// (adjacent layers) does NOT fire.
func TestCheckLayerMapping_ServiceToData(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/internal/service/orders.go",
		"github.com/myapp/internal/repository",
	)
	p := layerViolationPattern(nil)
	violations := checkLayerMapping(g, p)
	if len(violations) != 0 {
		t.Errorf("expected no violation for service→data (adjacent), got %d", len(violations))
	}
}

// TestCheckLayerMapping_UnknownSourceLayer verifies that files in unknown
// layers (utils, config, cmd) never produce violations.
func TestCheckLayerMapping_UnknownSourceLayer(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/internal/config/settings.go",
		"github.com/myapp/internal/repository",
	)
	p := layerViolationPattern(nil)
	violations := checkLayerMapping(g, p)
	if len(violations) != 0 {
		t.Errorf("expected no violation for unknown source layer, got %d", len(violations))
	}
}

// TestCheckLayerMapping_UnknownImportLayer verifies that imports from
// unknown-layer packages (external libs with no layer keywords) do not fire.
func TestCheckLayerMapping_UnknownImportLayer(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/internal/handler/orders.go",
		"github.com/gin-gonic/gin",
		"encoding/json",
		"fmt",
	)
	p := layerViolationPattern(nil)
	violations := checkLayerMapping(g, p)
	if len(violations) != 0 {
		t.Errorf("expected no violation for external/stdlib imports, got %d", len(violations))
	}
}

// TestCheckLayerMapping_MultipleViolations verifies that a handler importing
// both repository and dal packages produces two violations.
func TestCheckLayerMapping_MultipleViolations(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/internal/handler/payment.go",
		"github.com/myapp/internal/repository", // data layer
		"github.com/myapp/internal/dal",         // data layer
		"github.com/myapp/internal/service",     // service layer — OK
	)
	p := layerViolationPattern(nil)
	violations := checkLayerMapping(g, p)

	if len(violations) != 2 {
		t.Errorf("expected 2 violations (repo + dal), got %d: %+v", len(violations), violations)
	}
}

// TestCheckLayerMapping_Dedup verifies that the same (file, import) pair is
// reported at most once even if the graph has duplicate IMPORTS edges.
func TestCheckLayerMapping_Dedup(t *testing.T) {
	// addFileWithImports adds each import once. Manually add a second IMPORTS
	// edge to simulate a duplicate in the graph.
	g := buildTestGraph(t)
	fileID := addFileWithImports(g, "/project/internal/handler/orders.go",
		"github.com/myapp/internal/repository",
	)
	// Add second IMPORTS edge to the same NodePackage (duplicate).
	impID := g.MakeNodeID("github.com/myapp/internal/repository", "github.com/myapp/internal/repository")
	g.AddEdge(&graph.Edge{From: fileID, To: impID, Type: graph.EdgeImports})

	p := layerViolationPattern(nil)
	violations := checkLayerMapping(g, p)

	if len(violations) != 1 {
		t.Errorf("expected exactly 1 violation (dedup), got %d", len(violations))
	}
}

// TestCheckLayerMapping_TestFilesSkipped verifies that _test.go files are excluded.
func TestCheckLayerMapping_TestFilesSkipped(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/internal/handler/orders_test.go",
		"github.com/myapp/internal/repository",
	)
	p := layerViolationPattern(nil)
	violations := checkLayerMapping(g, p)
	if len(violations) != 0 {
		t.Errorf("expected no violation for test file, got %d", len(violations))
	}
}

// TestCheckLayerMapping_VendorSkipped verifies that files under vendor/ are excluded.
func TestCheckLayerMapping_VendorSkipped(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/vendor/somelib/handler/proxy.go",
		"github.com/myapp/internal/repository",
	)
	p := layerViolationPattern(nil)
	violations := checkLayerMapping(g, p)
	if len(violations) != 0 {
		t.Errorf("expected no violation for vendored file, got %d", len(violations))
	}
}

// TestCheckLayerMapping_CustomLayerConfig verifies that a caller-supplied
// LayerConfig overrides the built-in defaults.
func TestCheckLayerMapping_CustomLayerConfig(t *testing.T) {
	customLayers := []LayerDef{
		{Name: "http", Keywords: []string{"http", "rest"}},
		{Name: "logic", Keywords: []string{"logic", "biz"}},
		{Name: "store", Keywords: []string{"store", "storage"}},
	}

	// http → store should fire (skip logic).
	g := buildLayerGraph(t,
		"/project/internal/http/orders.go",
		"github.com/myapp/internal/store",
	)
	p := layerViolationPattern(customLayers)
	violations := checkLayerMapping(g, p)
	if len(violations) == 0 {
		t.Fatal("expected violation with custom layer config (http→store skips logic)")
	}
	if !strings.Contains(violations[0].Evidence, "http") {
		t.Errorf("evidence should contain custom src layer 'http': %q", violations[0].Evidence)
	}

	// http → logic should NOT fire (adjacent).
	g2 := buildLayerGraph(t,
		"/project/internal/http/orders.go",
		"github.com/myapp/internal/logic",
	)
	p2 := layerViolationPattern(customLayers)
	v2 := checkLayerMapping(g2, p2)
	if len(v2) != 0 {
		t.Errorf("expected no violation for http→logic (adjacent custom layers), got %d", len(v2))
	}
}

// TestCheckLayerMapping_ActionSet verifies that violations from checkLayerMapping
// receive the correct Action field when processed through CheckProject→withActions.
func TestCheckLayerMapping_ActionSet(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/internal/handler/orders.go",
		"github.com/myapp/internal/repository",
	)
	p := layerViolationPattern(nil)
	ps := newPatternSet([]SecurityPattern{p})
	engine := NewEngine(ps)

	violations := engine.CheckProject(g)
	if len(violations) == 0 {
		t.Fatal("expected violation from CheckProject")
	}
	v := violations[0]
	if v.Action != "inform" {
		t.Errorf("MEDIUM violation action = %q, want 'inform'", v.Action)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Integration: CheckProject dispatches layer_mapping patterns
// ──────────────────────────────────────────────────────────────────────────────

// TestCheckProject_LayerMappingDispatched verifies that CheckProject correctly
// dispatches CheckTypeLayerMapping patterns alongside CheckTypeCrossTransportAuth.
func TestCheckProject_LayerMappingDispatched(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/internal/controller/orders.go",
		"github.com/myapp/internal/dal",
	)

	lp := layerViolationPattern(nil)
	ps := newPatternSet([]SecurityPattern{lp})
	engine := NewEngine(ps)

	violations := engine.CheckProject(g)
	if len(violations) == 0 {
		t.Fatal("CheckProject should dispatch layer_mapping and find violation")
	}
	found := false
	for _, v := range violations {
		if v.PatternID == "test-layer-violation" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected violation with patternID 'test-layer-violation'; violations: %+v", violations)
	}
}

// TestCheckProject_NilEngine is a defensive guard.
func TestCheckProject_NilEngine(t *testing.T) {
	var e *Engine
	g := buildLayerGraph(t, "/project/internal/handler/x.go", "github.com/myapp/internal/repository")
	if v := e.CheckProject(g); v != nil {
		t.Errorf("nil engine should return nil, got %v", v)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Builtin pattern validation
// ──────────────────────────────────────────────────────────────────────────────

// TestBuiltinLayerPattern_LoadsAndValid verifies that the built-in generic
// layer-violation pattern passes SecurityPattern.Validate() and can be loaded.
func TestBuiltinLayerPattern_LoadsAndValid(t *testing.T) {
	ps, err := LoadBuiltin()
	if err != nil {
		t.Fatalf("LoadBuiltin failed: %v", err)
	}

	patterns := ps.ForCheckType(CheckTypeLayerMapping)
	if len(patterns) == 0 {
		t.Fatal("expected at least one layer_mapping pattern in built-in set")
	}

	for _, p := range patterns {
		if err := p.Validate(); err != nil {
			t.Errorf("pattern %q failed validation: %v", p.ID, err)
		}
		if p.Severity != SeverityMedium {
			t.Errorf("pattern %q: severity = %q, want MEDIUM", p.ID, p.Severity)
		}
		if p.Detection.Scope != ScopeProject {
			t.Errorf("pattern %q: scope = %q, want 'project'", p.ID, p.Detection.Scope)
		}
	}
}

// TestBuiltinLayerPattern_FiresEndToEnd runs the full engine with built-in
// patterns and confirms a layer violation is detected in a realistic scenario.
func TestBuiltinLayerPattern_FiresEndToEnd(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/internal/api/orders.go",         // presentation layer (api/)
		"github.com/myapp/internal/repositories",  // data layer (repositories/)
	)

	engine := DefaultEngine()
	violations := engine.CheckProject(g)

	found := false
	for _, v := range violations {
		if v.PatternID == "generic-layer-violation" {
			found = true
			if v.Severity != SeverityMedium {
				t.Errorf("expected MEDIUM, got %q", v.Severity)
			}
			break
		}
	}
	if !found {
		t.Error("built-in layer violation pattern did not fire for api→repositories import")
	}
}

// TestCheckLayerMapping_EmptyGraph returns nil and does not panic.
func TestCheckLayerMapping_EmptyGraph(t *testing.T) {
	g := buildTestGraph(t)
	p := layerViolationPattern(nil)
	violations := checkLayerMapping(g, p)
	if violations != nil {
		t.Errorf("expected nil for empty graph, got %v", violations)
	}
}

// TestCheckLayerMapping_MessageTemplate verifies that fillTemplate correctly
// substitutes {file}, {src_layer}, {dst_layer} and {skip_layer} placeholders in
// the violation message. This is the only test that asserts v.Message content.
func TestCheckLayerMapping_MessageTemplate(t *testing.T) {
	g := buildLayerGraph(t,
		"/project/internal/handler/orders.go",
		"github.com/myapp/internal/repository",
	)

	p := layerViolationPattern(nil)
	// Message template set by layerViolationPattern:
	// "File {file} ({src_layer}) directly imports '{target}' ({dst_layer})."
	violations := checkLayerMapping(g, p)
	if len(violations) == 0 {
		t.Fatal("expected a violation for handler→repository, got none")
	}

	msg := violations[0].Message
	if !strings.Contains(msg, "/project/internal/handler/orders.go") {
		t.Errorf("message should contain the file path; got: %q", msg)
	}
	if !strings.Contains(msg, "presentation") {
		t.Errorf("message should contain src_layer 'presentation'; got: %q", msg)
	}
	if !strings.Contains(msg, "data") {
		t.Errorf("message should contain dst_layer 'data'; got: %q", msg)
	}
	if !strings.Contains(msg, "github.com/myapp/internal/repository") {
		t.Errorf("message should contain target import path; got: %q", msg)
	}
	// Ensure no unreplaced placeholders remain.
	if strings.Contains(msg, "{") {
		t.Errorf("message still contains unreplaced placeholder(s): %q", msg)
	}
}

// TestLayerDefValidation verifies that SecurityPattern.Validate() rejects
// LayerDef entries with an empty name or an empty keywords list.
func TestLayerDefValidation(t *testing.T) {
	base := SecurityPattern{
		ID:          "test-layer",
		Name:        "Test",
		Language:    "*",
		Framework:   "*",
		PatternType: PatternTypeLayerViolation,
		Severity:    SeverityMedium,
		Description: "desc",
		Message:     "msg",
		Detection: Detection{
			CheckType: CheckTypeLayerMapping,
			Scope:     ScopeProject,
		},
	}

	// Valid LayerConfig — should pass.
	base.Detection.LayerConfig = []LayerDef{
		{Name: "web", Keywords: []string{"handler"}},
		{Name: "db", Keywords: []string{"repo"}},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid LayerConfig rejected: %v", err)
	}

	// Empty name — should fail.
	base.Detection.LayerConfig = []LayerDef{
		{Name: "", Keywords: []string{"handler"}},
	}
	if err := base.Validate(); err == nil {
		t.Error("expected error for LayerDef with empty name, got nil")
	}

	// Empty keywords — should fail.
	base.Detection.LayerConfig = []LayerDef{
		{Name: "web", Keywords: []string{}},
	}
	if err := base.Validate(); err == nil {
		t.Error("expected error for LayerDef with empty keywords, got nil")
	}
}

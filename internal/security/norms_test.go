package security

import (
	"strings"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────────
// DiscoverNorms — route call norm tests
// ──────────────────────────────────────────────────────────────────────────────

// TestDiscoverNorms_NilGuards verifies that nil Engine and nil graph are both
// handled gracefully without panic.
func TestDiscoverNorms_NilGuards(t *testing.T) {
	g := buildTestGraph(t)
	addFileWithImports(g, "/project/internal/handler/a.go")
	addRouteNode(g, "/project/internal/handler/a.go", "GET", "/a")
	addFunctionWithCalls(g, "/project/internal/handler/a.go", "Register", "AuthMiddleware")

	// nil engine
	var e *Engine
	if got := e.DiscoverNorms(g); got != nil {
		t.Errorf("nil Engine.DiscoverNorms: want nil, got %+v", got)
	}

	// nil graph
	e = NewEngine(nil)
	if got := e.DiscoverNorms(nil); got != nil {
		t.Errorf("DiscoverNorms(nil graph): want nil, got %+v", got)
	}
}

// TestDiscoverNorms_InsufficientRouteSamples verifies that fewer than 3
// route-registering files produce no route call norms.
func TestDiscoverNorms_InsufficientRouteSamples(t *testing.T) {
	g := buildTestGraph(t)
	// Only 2 route files — below the minimum sample threshold.
	for _, f := range []string{
		"/project/internal/handler/a.go",
		"/project/internal/handler/b.go",
	} {
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/"+f)
		addFunctionWithCalls(g, f, "Register", "AuthMiddleware")
	}

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	// Route call norms need ≥3 route files. Layer norms need ≥3 handler files.
	// Neither threshold is met here — expect nil.
	if norms != nil {
		// Filter for route_call_norm specifically; layer norm might have fired.
		for _, n := range norms {
			if n.Category == "route_call_norm" {
				t.Errorf("expected no route_call_norm with only 2 route files, got %+v", n)
			}
		}
	}
}

// TestDiscoverNorms_RouteCallNorm_HighAdherence verifies that when 5/5 route
// files all call "AuthMiddleware", a norm with adherence=1.0 and SuggestRule=true
// is returned.
func TestDiscoverNorms_RouteCallNorm_HighAdherence(t *testing.T) {
	g := buildTestGraph(t)
	for i := 0; i < 5; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/route"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Register"+string(rune('A'+i)), "AuthMiddleware")
	}

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	if len(norms) == 0 {
		t.Fatal("expected at least one discovered norm, got none")
	}

	var authNorm *DiscoveredNorm
	for i := range norms {
		if norms[i].Category == "route_call_norm" && strings.Contains(norms[i].Evidence, "AuthMiddleware") {
			authNorm = &norms[i]
			break
		}
	}
	if authNorm == nil {
		t.Fatalf("no route_call_norm for AuthMiddleware; norms: %+v", norms)
	}

	if authNorm.Adherence != 1.0 {
		t.Errorf("adherence = %f, want 1.0", authNorm.Adherence)
	}
	if authNorm.Compliant != 5 || authNorm.Total != 5 {
		t.Errorf("compliant/total = %d/%d, want 5/5", authNorm.Compliant, authNorm.Total)
	}
	if !authNorm.SuggestRule {
		t.Error("SuggestRule should be true for 100% adherence with 5 samples")
	}
	if authNorm.Confidence != "HIGH" {
		t.Errorf("confidence = %q, want HIGH (5 samples, 100%% adherence)", authNorm.Confidence)
	}
}

// TestDiscoverNorms_RouteCallNorm_PartialAdherence verifies that 4/5 route files
// (80%) calling a function surfaces a norm, but SuggestRule is false and
// Confidence is MEDIUM.
func TestDiscoverNorms_RouteCallNorm_PartialAdherence(t *testing.T) {
	g := buildTestGraph(t)
	// 4 out of 5 files call "RateLimiter".
	for i := 0; i < 4; i++ {
		f := "/project/internal/api/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/api/"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "RateLimiter")
	}
	// 5th file has a route but doesn't call RateLimiter.
	addFileWithImports(g, "/project/internal/api/e.go")
	addRouteNode(g, "/project/internal/api/e.go", "GET", "/api/e")
	addFunctionWithCalls(g, "/project/internal/api/e.go", "RegE", "SomethingElseFunc")

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	var rlNorm *DiscoveredNorm
	for i := range norms {
		if norms[i].Category == "route_call_norm" && strings.Contains(norms[i].Evidence, "RateLimiter") {
			rlNorm = &norms[i]
			break
		}
	}
	if rlNorm == nil {
		t.Fatalf("no route_call_norm for RateLimiter; norms: %+v", norms)
	}

	if rlNorm.Compliant != 4 || rlNorm.Total != 5 {
		t.Errorf("compliant/total = %d/%d, want 4/5", rlNorm.Compliant, rlNorm.Total)
	}
	// 4/5 = 0.80 < 0.95 threshold → SuggestRule must be false.
	if rlNorm.SuggestRule {
		t.Error("SuggestRule should be false for 80% adherence")
	}
	// 5 samples but only 80% adherence → MEDIUM (needs ≥95% for HIGH).
	if rlNorm.Confidence != "MEDIUM" {
		t.Errorf("confidence = %q, want MEDIUM (80%% adherence)", rlNorm.Confidence)
	}
}

// TestDiscoverNorms_RouteCallNorm_BelowThreshold verifies that 2/4 route files
// (50%) calling a function does NOT produce a norm — below the 75% threshold.
func TestDiscoverNorms_RouteCallNorm_BelowThreshold(t *testing.T) {
	g := buildTestGraph(t)
	for i := 0; i < 2; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "SpecialMiddleware")
	}
	for i := 2; i < 4; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		// no SpecialMiddleware call
	}

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	for _, n := range norms {
		if n.Category == "route_call_norm" && strings.Contains(n.Evidence, "SpecialMiddleware") {
			t.Errorf("got unexpected norm for SpecialMiddleware at 50%% adherence: %+v", n)
		}
	}
}

// TestDiscoverNorms_ShortNameFiltered verifies that callee names shorter than 4
// characters (e.g. "Get", "Use", "Set") are not surfaced as norms.
func TestDiscoverNorms_ShortNameFiltered(t *testing.T) {
	g := buildTestGraph(t)
	// All 4 files call "Use" (3 chars — should be filtered out) and "AuthMiddleware" (14 chars — ok).
	for i := 0; i < 4; i++ {
		f := "/project/internal/route/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/rt"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "Use", "AuthMiddleware")
	}

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	for _, n := range norms {
		if n.Category == "route_call_norm" && strings.Contains(n.Evidence, `"Use"`) {
			t.Errorf("short callee 'Use' should be filtered but surfaced as norm: %+v", n)
		}
	}

	// AuthMiddleware (14 chars) should still appear — it's above the 4-char threshold.
	found := false
	for _, n := range norms {
		if n.Category == "route_call_norm" && strings.Contains(n.Evidence, "AuthMiddleware") {
			found = true
		}
	}
	if !found {
		t.Error("AuthMiddleware (14 chars) should appear as a norm but was not found")
	}
}

// TestDiscoverNorms_TestFilesExcluded verifies that *_test.go files are not
// counted as route files for norm computation.
func TestDiscoverNorms_TestFilesExcluded(t *testing.T) {
	g := buildTestGraph(t)

	// 3 real route files all calling "AuthMiddleware".
	for i := 0; i < 3; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "AuthMiddleware")
	}
	// A test file with a route node — must not contribute to norm.
	addFileWithImports(g, "/project/internal/handler/handler_test.go")
	addRouteNode(g, "/project/internal/handler/handler_test.go", "GET", "/test")
	// The test file intentionally does NOT call AuthMiddleware.

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	// With test file excluded, all 3/3 real files call AuthMiddleware → 100%.
	for _, n := range norms {
		if n.Category == "route_call_norm" && strings.Contains(n.Evidence, "AuthMiddleware") {
			if n.Compliant != n.Total {
				t.Errorf("test file exclusion failed: got %d/%d, want equal (all compliant)", n.Compliant, n.Total)
			}
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DiscoverNorms — layer isolation norm tests
// ──────────────────────────────────────────────────────────────────────────────

// TestDiscoverNorms_LayerIsolation_PerfectAdherence verifies that 4 handler files
// none of which import data-layer packages produces a layer isolation norm with
// adherence=1.0 and SuggestRule=true.
func TestDiscoverNorms_LayerIsolation_PerfectAdherence(t *testing.T) {
	g := buildTestGraph(t)
	// 4 handler files importing only a service-layer package.
	for i := 0; i < 4; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f, "/project/internal/service")
	}

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	var layerNorm *DiscoveredNorm
	for i := range norms {
		if norms[i].Category == "layer_isolation" {
			layerNorm = &norms[i]
			break
		}
	}
	if layerNorm == nil {
		t.Fatalf("expected a layer_isolation norm; norms: %+v", norms)
	}

	if layerNorm.Adherence != 1.0 {
		t.Errorf("adherence = %f, want 1.0", layerNorm.Adherence)
	}
	if layerNorm.Compliant != 4 || layerNorm.Total != 4 {
		t.Errorf("compliant/total = %d/%d, want 4/4", layerNorm.Compliant, layerNorm.Total)
	}
	if !layerNorm.SuggestRule {
		t.Error("SuggestRule should be true for 100% adherence with 4 samples")
	}
	// "0/4 presentation-layer files import data-layer packages directly"
	if !strings.Contains(layerNorm.Evidence, "0/4") {
		t.Errorf("evidence %q does not mention 0/4", layerNorm.Evidence)
	}
}

// TestDiscoverNorms_LayerIsolation_OneViolation verifies that 3 clean handler
// files and 1 violating handler file (imports repo directly) produces a layer
// isolation norm with 3/4 adherence (0.75) — exactly at the surfacing threshold.
func TestDiscoverNorms_LayerIsolation_OneViolation(t *testing.T) {
	g := buildTestGraph(t)
	// 3 clean handlers.
	for i := 0; i < 3; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f, "/project/internal/service")
	}
	// 1 violating handler that imports repo (data layer) directly.
	addFileWithImports(g, "/project/internal/handler/d.go", "/project/internal/repo")

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	var layerNorm *DiscoveredNorm
	for i := range norms {
		if norms[i].Category == "layer_isolation" {
			layerNorm = &norms[i]
			break
		}
	}
	if layerNorm == nil {
		t.Fatalf("expected a layer_isolation norm (3/4 = 75%% adherence); norms: %+v", norms)
	}

	if layerNorm.Compliant != 3 || layerNorm.Total != 4 {
		t.Errorf("compliant/total = %d/%d, want 3/4", layerNorm.Compliant, layerNorm.Total)
	}
	// 3/4 = 0.75 < 0.95 → SuggestRule false.
	if layerNorm.SuggestRule {
		t.Error("SuggestRule should be false for 75% adherence")
	}
}

// TestDiscoverNorms_LayerIsolation_BelowSurfacingThreshold verifies that when
// fewer than 75% of handler files are clean, no layer isolation norm is returned.
func TestDiscoverNorms_LayerIsolation_BelowSurfacingThreshold(t *testing.T) {
	g := buildTestGraph(t)
	// 2 clean handlers, 2 violating — 50% adherence.
	for i := 0; i < 2; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f, "/project/internal/service")
	}
	for i := 2; i < 4; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f, "/project/internal/repo")
	}

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	for _, n := range norms {
		if n.Category == "layer_isolation" {
			t.Errorf("got unexpected layer_isolation norm at 50%% adherence: %+v", n)
		}
	}
}

// TestDiscoverNorms_LayerIsolation_InsufficientSamples verifies that fewer than 3
// presentation-layer files produces no layer isolation norm.
func TestDiscoverNorms_LayerIsolation_InsufficientSamples(t *testing.T) {
	g := buildTestGraph(t)
	// Only 2 handler files.
	for i := 0; i < 2; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f, "/project/internal/service")
	}

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	for _, n := range norms {
		if n.Category == "layer_isolation" {
			t.Errorf("expected no layer_isolation norm with only 2 handler files, got %+v", n)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// discoverLayerNorms — custom layer config tests
// ──────────────────────────────────────────────────────────────────────────────

// TestDiscoverLayerNorms_TwoLayerConfig verifies that a 2-layer config (no
// intermediate service layer) returns nil — a skip violation is not possible
// when there is no layer to skip.
func TestDiscoverLayerNorms_TwoLayerConfig(t *testing.T) {
	g := buildTestGraph(t)
	// 4 handler files importing a repo package directly.
	for i := 0; i < 4; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f, "/project/internal/repo")
	}

	twoLayer := []LayerDef{
		{Name: "presentation", Keywords: []string{"handler"}},
		{Name: "data", Keywords: []string{"repo"}},
	}
	norms := discoverLayerNorms(g, twoLayer)
	if len(norms) != 0 {
		t.Errorf("2-layer config: expected nil (no skip possible), got %+v", norms)
	}
}

// TestDiscoverLayerNorms_CustomFourLayerConfig verifies that a 4-tier config
// (presentation/gateway/service/data) correctly detects presentation→data skips
// while allowing presentation→gateway imports.
func TestDiscoverLayerNorms_CustomFourLayerConfig(t *testing.T) {
	g := buildTestGraph(t)
	fourLayer := []LayerDef{
		{Name: "presentation", Keywords: []string{"handler"}},
		{Name: "gateway", Keywords: []string{"gateway"}},
		{Name: "service", Keywords: []string{"service"}},
		{Name: "data", Keywords: []string{"repo"}},
	}

	// 3 clean handler files that import only the gateway layer.
	for i := 0; i < 3; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f, "/project/internal/gateway")
	}

	norms := discoverLayerNorms(g, fourLayer)
	if len(norms) == 0 {
		t.Fatal("4-layer config: expected a layer_isolation norm for clean handler files, got none")
	}
	n := norms[0]
	if n.Adherence != 1.0 {
		t.Errorf("adherence = %f, want 1.0", n.Adherence)
	}
	if n.Total != 3 {
		t.Errorf("total = %d, want 3", n.Total)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// FormatDiscoveredNorm
// ──────────────────────────────────────────────────────────────────────────────

func TestFormatDiscoveredNorm_WithoutSuggestRule(t *testing.T) {
	n := DiscoveredNorm{
		Description: "Consistent route middleware: AuthMiddleware",
		Evidence:    "8/10 route-registering files call \"AuthMiddleware\"",
		SuggestRule: false,
	}
	got := FormatDiscoveredNorm(n)
	if !strings.Contains(got, "AuthMiddleware") {
		t.Errorf("formatted norm missing description: %q", got)
	}
	if !strings.Contains(got, "8/10") {
		t.Errorf("formatted norm missing evidence: %q", got)
	}
	if strings.Contains(got, "promote to enforced rule") {
		t.Errorf("unexpected promote-to-rule text when SuggestRule=false: %q", got)
	}
}

func TestFormatDiscoveredNorm_WithSuggestRule(t *testing.T) {
	n := DiscoveredNorm{
		Description: "Layer isolation: no handler-layer file imports data-layer packages directly",
		Evidence:    "0/6 handler files import repo packages directly",
		SuggestRule: true,
	}
	got := FormatDiscoveredNorm(n)
	if !strings.Contains(got, "promote to enforced rule?") {
		t.Errorf("expected promote-to-rule prompt when SuggestRule=true: %q", got)
	}
}

func TestFormatDiscoveredNorm_TruncatesLongStrings(t *testing.T) {
	// Build a description longer than 160 runes to trigger truncation.
	long := strings.Repeat("x", 200)
	n := DiscoveredNorm{
		Description: long,
		Evidence:    long,
		SuggestRule: false,
	}
	got := FormatDiscoveredNorm(n)
	runes := []rune(got)
	if len(runes) > 160 {
		t.Errorf("formatted norm not truncated: got %d runes, want ≤160", len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated norm missing ellipsis suffix: %q", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DiscoverNorms — sort order and cap
// ──────────────────────────────────────────────────────────────────────────────

// TestDiscoverNorms_SortedByAdherence verifies that norms with higher adherence
// appear first in the result slice.
func TestDiscoverNorms_SortedByAdherence(t *testing.T) {
	g := buildTestGraph(t)
	// 4 files call "AuthMiddleware" (4/4 = 1.0) and 3 call "RateLimiter" (3/4 = 0.75).
	for i := 0; i < 4; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		if i < 3 {
			addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "AuthMiddleware", "RateLimiter")
		} else {
			addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), "AuthMiddleware")
		}
	}

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	if len(norms) < 2 {
		t.Fatalf("expected at least 2 norms, got %d: %+v", len(norms), norms)
	}

	// The first norm should have adherence ≥ the second.
	if norms[0].Adherence < norms[1].Adherence {
		t.Errorf("norms not sorted by descending adherence: [0]=%f < [1]=%f",
			norms[0].Adherence, norms[1].Adherence)
	}
}

// TestDiscoverNorms_Cap verifies that at most 5 norms are returned even when
// many qualify.
func TestDiscoverNorms_Cap(t *testing.T) {
	g := buildTestGraph(t)
	// 3 route files each calling 10 different functions (all ≥75%).
	callees := []string{
		"AlphaMiddleware", "BetaMiddleware", "GammaMiddleware", "DeltaMiddleware",
		"EpsilonMiddleware", "ZetaMiddleware", "EtaMiddleware", "ThetaMiddleware",
		"IotaMiddleware", "KappaMiddleware",
	}
	for i := 0; i < 3; i++ {
		f := "/project/internal/handler/" + string(rune('a'+i)) + ".go"
		addFileWithImports(g, f)
		addRouteNode(g, f, "GET", "/r"+string(rune('a'+i)))
		addFunctionWithCalls(g, f, "Reg"+string(rune('A'+i)), callees...)
	}

	e := NewEngine(nil)
	norms := e.DiscoverNorms(g)

	const maxNorms = 5
	if len(norms) > maxNorms {
		t.Errorf("got %d norms, want ≤%d (cap enforced)", len(norms), maxNorms)
	}
}

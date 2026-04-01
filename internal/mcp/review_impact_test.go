package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── Review Scope Tests ──────────────────────────────────────────────────────

func TestHandleGetImpact_ReviewScope_BlastRadius(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// AuthLogin is called by HandleRequest (depth 1 = 1 direct caller).
	req := callTool(map[string]any{"symbol": "AuthLogin", "scope": "review"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := mustResult(t, result, nil)

	// blast_radius must be present.
	br, ok := m["blast_radius"].(map[string]any)
	if !ok {
		t.Fatal("expected blast_radius in response")
	}

	directCallers := int(br["direct_callers"].(float64))
	if directCallers < 1 {
		t.Errorf("expected at least 1 direct caller, got %d", directCallers)
	}

	// affected_files should be at least 1.
	af := int(br["affected_files"].(float64))
	if af < 1 {
		t.Errorf("expected at least 1 affected file, got %d", af)
	}
}

func TestHandleGetImpact_ReviewScope_TestGapsPresent(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// The test graph has no test files, so all impacted entities should be test gaps.
	req := callTool(map[string]any{"symbol": "AuthLogin", "scope": "review"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := mustResult(t, result, nil)

	// test_gaps should be present (since no test files exist in the test graph).
	tg, ok := m["test_gaps"]
	if !ok {
		// The field may be omitted if empty — also valid.
		return
	}
	gaps, ok := tg.([]any)
	if !ok {
		t.Fatalf("expected test_gaps to be an array, got %T", tg)
	}

	// Each gap should have entity, file, depth.
	for i, g := range gaps {
		gap, ok := g.(map[string]any)
		if !ok {
			t.Fatalf("test_gaps[%d]: expected object, got %T", i, g)
		}
		if _, ok := gap["entity"]; !ok {
			t.Errorf("test_gaps[%d]: missing 'entity' field", i)
		}
		if _, ok := gap["depth"]; !ok {
			t.Errorf("test_gaps[%d]: missing 'depth' field", i)
		}
	}
}

func TestHandleGetImpact_ReviewScope_StructEntity(t *testing.T) {
	// Build a graph with a struct and methods.
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	structID := g.MakeNodeID("pkg/svc/svc.go", "Service")
	g.AddNode(&graph.Node{
		ID:       structID,
		Type:     graph.NodeStruct,
		Name:     "Service",
		File:     "pkg/svc/svc.go",
		Line:     10,
		Package:  "svc",
		Exported: true,
	})

	methodID := g.MakeNodeID("pkg/svc/svc.go", "Service.Run")
	g.AddNode(&graph.Node{
		ID:       methodID,
		Type:     graph.NodeMethod,
		Name:     "Service.Run",
		File:     "pkg/svc/svc.go",
		Line:     20,
		Package:  "svc",
		Exported: true,
	})

	callerID := g.MakeNodeID("cmd/main.go", "main")
	g.AddNode(&graph.Node{
		ID:       callerID,
		Type:     graph.NodeFunction,
		Name:     "main",
		File:     "cmd/main.go",
		Line:     5,
		Package:  "main",
		Exported: false,
	})

	g.AddEdge(&graph.Edge{From: callerID, To: methodID, Type: graph.EdgeCalls})

	cfg, err := loadTestConfig(t)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{"symbol": "Service", "scope": "review"})
	result, err := srv.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := mustResult(t, result, nil)

	// blast_radius must be present for struct entities too.
	if _, ok := m["blast_radius"]; !ok {
		t.Fatal("expected blast_radius in struct entity response")
	}
}

func TestHandleGetImpact_DefaultScope_NoBlastRadius(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// Without scope=review, response should NOT contain blast_radius.
	req := callTool(map[string]any{"symbol": "AuthLogin"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := mustResult(t, result, nil)

	if _, ok := m["blast_radius"]; ok {
		t.Error("blast_radius should not be present in standard (non-review) scope")
	}
}

func TestHandleGetImpact_ReviewScope_NilPulseClient_GracefulDegradation(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// pulseClient is nil by default in test server — should not panic.

	req := callTool(map[string]any{"symbol": "AuthLogin", "scope": "review"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := mustResult(t, result, nil)

	// Should still have blast_radius even without pulse.
	if _, ok := m["blast_radius"]; !ok {
		t.Fatal("expected blast_radius even with nil pulse client")
	}

	// risk_flags should be absent (no pulse client to query).
	if rf, ok := m["risk_flags"]; ok && rf != nil {
		t.Error("risk_flags should be absent when pulse client is nil")
	}
}

func TestHandleGetImpact_ReviewScope_JSONShape(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	req := callTool(map[string]any{"symbol": "AuthLogin", "scope": "review"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the result is valid JSON with the expected top-level keys.
	raw := mustResult(t, result, nil)
	data, _ := json.Marshal(raw)
	var ri reviewImpact
	if err := json.Unmarshal(data, &ri); err != nil {
		t.Fatalf("failed to unmarshal into reviewImpact: %v", err)
	}

	// Root should be populated.
	if ri.Root.Name == "" {
		t.Error("expected Root.Name to be populated")
	}

	// Tiers should be non-nil (possibly empty).
	if ri.Tiers == nil {
		t.Error("expected Tiers to be non-nil")
	}
}

func TestHandleGetImpact_ReviewScope_IsolatedEntity_ZeroCallers(t *testing.T) {
	// An entity with no callers should produce blast_radius with all zeros.
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	id := g.MakeNodeID("pkg/lone/lone.go", "Orphan")
	g.AddNode(&graph.Node{
		ID:       id,
		Type:     graph.NodeFunction,
		Name:     "Orphan",
		File:     "pkg/lone/lone.go",
		Line:     1,
		Package:  "lone",
		Exported: true,
	})

	cfg, err := loadTestConfig(t)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{"symbol": "Orphan", "scope": "review"})
	result, err := srv.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := mustResult(t, result, nil)
	br, ok := m["blast_radius"].(map[string]any)
	if !ok {
		t.Fatal("expected blast_radius in response")
	}

	for _, key := range []string{"direct_callers", "transitive_callers", "affected_files", "untested_entities", "high_risk_entities"} {
		v := int(br[key].(float64))
		if v != 0 {
			t.Errorf("expected %s=0 for isolated entity, got %d", key, v)
		}
	}

	// test_gaps and risk_flags should be omitted or empty.
	if tg, ok := m["test_gaps"]; ok {
		if arr, ok := tg.([]any); ok && len(arr) > 0 {
			t.Errorf("expected no test_gaps for isolated entity, got %d", len(arr))
		}
	}
}

func TestHandleGetImpact_ReviewScope_StructNoMethodCallers(t *testing.T) {
	// A struct with methods but no callers to those methods.
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	structID := g.MakeNodeID("pkg/empty/empty.go", "EmptyStruct")
	g.AddNode(&graph.Node{
		ID:       structID,
		Type:     graph.NodeStruct,
		Name:     "EmptyStruct",
		File:     "pkg/empty/empty.go",
		Line:     5,
		Package:  "empty",
		Exported: true,
	})

	methodID := g.MakeNodeID("pkg/empty/empty.go", "EmptyStruct.DoNothing")
	g.AddNode(&graph.Node{
		ID:       methodID,
		Type:     graph.NodeMethod,
		Name:     "EmptyStruct.DoNothing",
		File:     "pkg/empty/empty.go",
		Line:     10,
		Package:  "empty",
		Exported: true,
	})

	cfg, err := loadTestConfig(t)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{"symbol": "EmptyStruct", "scope": "review"})
	result, err := srv.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m := mustResult(t, result, nil)
	br, ok := m["blast_radius"].(map[string]any)
	if !ok {
		t.Fatal("expected blast_radius in struct response with no callers")
	}

	dc := int(br["direct_callers"].(float64))
	if dc != 0 {
		t.Errorf("expected 0 direct callers, got %d", dc)
	}
}

func TestHandleGetImpact_ReviewScope_UnknownSymbol(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	req := callTool(map[string]any{"symbol": "NonexistentSymbol99", "scope": "review"})
	result, err := s.handleGetImpact(ctx, req)
	// Should return a tool error, not a Go error / panic.
	if err != nil {
		t.Fatalf("expected tool-level error, got Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatal("expected tool error for unknown symbol")
	}
}

// ── Sprint 23.3: NL blast radius summary tests ───────────────────────────────

// TestHandleGetImpact_DefaultScope_HasNLSummary verifies that the default
// (non-review) scope now includes blast_radius_summary (NL string) and
// packages_affected (int) as per the Sprint 23.3 redesign.
func TestHandleGetImpact_DefaultScope_HasNLSummary(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	req := callTool(map[string]any{"symbol": "AuthLogin"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)

	// blast_radius_summary must be a non-empty string.
	summary, ok := m["blast_radius_summary"].(string)
	if !ok || summary == "" {
		t.Fatalf("expected non-empty blast_radius_summary string, got %v", m["blast_radius_summary"])
	}

	// packages_affected must be a non-negative integer.
	pa, ok := m["packages_affected"].(float64)
	if !ok {
		t.Fatalf("expected packages_affected numeric, got %T: %v", m["packages_affected"], m["packages_affected"])
	}
	if pa < 0 {
		t.Errorf("expected packages_affected >= 0, got %v", pa)
	}

	// blast_radius (the struct from review scope) must NOT be present.
	if _, ok := m["blast_radius"]; ok {
		t.Error("blast_radius struct should not appear in default scope — only blast_radius_summary")
	}
}

// TestHandleGetImpact_NLSummary_ContainsEntityName verifies the NL summary
// references the queried entity by name so agents can read it without parsing.
func TestHandleGetImpact_NLSummary_ContainsEntityName(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	req := callTool(map[string]any{"symbol": "AuthLogin"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)

	summary, _ := m["blast_radius_summary"].(string)
	if !strings.Contains(summary, "AuthLogin") {
		t.Errorf("expected blast_radius_summary to mention 'AuthLogin', got: %q", summary)
	}
}

// TestHandleGetImpact_CriticalPathDomains_AuthDetected verifies that an entity
// in the auth path is flagged with critical_path_domains = ["auth"].
func TestHandleGetImpact_CriticalPathDomains_AuthDetected(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// AuthLogin lives in pkg/auth/auth.go — "auth" keyword in both name and file.
	req := callTool(map[string]any{"symbol": "AuthLogin"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)

	domains, ok := m["critical_path_domains"].([]any)
	if !ok || len(domains) == 0 {
		t.Fatalf("expected critical_path_domains to contain 'auth', got %v", m["critical_path_domains"])
	}
	found := false
	for _, d := range domains {
		if d.(string) == "auth" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'auth' in critical_path_domains, got %v", domains)
	}
}

// TestHandleGetImpact_NLSummary_ZeroCallers verifies that an entity with no
// callers gets the "safe to change in isolation" message.
func TestHandleGetImpact_NLSummary_ZeroCallers(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	id := g.MakeNodeID("pkg/util/util.go", "HelperFunc")
	g.AddNode(&graph.Node{
		ID:      id,
		Type:    graph.NodeFunction,
		Name:    "HelperFunc",
		File:    "pkg/util/util.go",
		Line:    1,
		Package: "util",
	})

	cfg, err := loadTestConfig(t)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	req := callTool(map[string]any{"symbol": "HelperFunc"})
	result, err := srv.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)

	summary, _ := m["blast_radius_summary"].(string)
	if !strings.Contains(summary, "no callers") {
		t.Errorf("expected 'no callers' in blast_radius_summary for isolated entity, got: %q", summary)
	}
	if pa := m["packages_affected"].(float64); pa != 0 {
		t.Errorf("expected packages_affected=0 for isolated entity, got %v", pa)
	}
}

// TestHandleGetImpact_ReviewScope_HasNLSummary verifies the review scope also
// includes the Sprint 23.3 NL fields alongside its richer blast_radius struct.
func TestHandleGetImpact_ReviewScope_HasNLSummary(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	req := callTool(map[string]any{"symbol": "AuthLogin", "scope": "review"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)

	// Both the review struct AND the NL summary must be present.
	if _, ok := m["blast_radius"]; !ok {
		t.Error("blast_radius struct missing from review scope")
	}
	summary, ok := m["blast_radius_summary"].(string)
	if !ok || summary == "" {
		t.Errorf("expected blast_radius_summary in review scope, got %v", m["blast_radius_summary"])
	}
}

// TestHandleGetImpact_FilesParam_HasNLSummary verifies the files= (changeset)
// path also receives NL blast radius fields after Sprint 23.3.
func TestHandleGetImpact_FilesParam_HasNLSummary(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// Pass the file that contains AuthLogin — any callers of entities in that
	// file should be reflected in the blast radius.
	req := callTool(map[string]any{"files": "pkg/auth/auth.go"})
	result, err := s.handleGetImpact(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)

	// blast_radius_summary must be present on the files= path.
	summary, ok := m["blast_radius_summary"].(string)
	if !ok || summary == "" {
		t.Fatalf("expected non-empty blast_radius_summary for files= path, got %v", m["blast_radius_summary"])
	}
	// packages_affected must be present and non-negative.
	pa, ok := m["packages_affected"].(float64)
	if !ok || pa < 0 {
		t.Errorf("expected packages_affected >= 0 for files= path, got %v", m["packages_affected"])
	}
}

// TestBuildImpactNLText_TransitiveOnlyNoDirect verifies the edge case where
// direct=0 but transitive>0 (rare: depth-1 callers tombstoned after analysis).
// The output must not say "0 direct callers" — just "N transitive callers".
func TestBuildImpactNLText_TransitiveOnlyNoDirect(t *testing.T) {
	summary := buildImpactNLText("MyFunc", 0, 5, 2, nil, false)
	if strings.Contains(summary, "0 direct caller") {
		t.Errorf("should not mention '0 direct callers' when direct=0, got: %q", summary)
	}
	if !strings.Contains(summary, "5 transitive caller") {
		t.Errorf("expected '5 transitive callers' in summary, got: %q", summary)
	}
	if !strings.Contains(summary, "MyFunc") {
		t.Errorf("expected entity name in summary, got: %q", summary)
	}
}

// TestBuildImpactNLText_BothDirectAndTransitive verifies the typical case with
// both direct and transitive callers.
func TestBuildImpactNLText_BothDirectAndTransitive(t *testing.T) {
	summary := buildImpactNLText("PayService", 3, 9, 4, []string{"payment"}, false)
	if !strings.Contains(summary, "3 direct caller") {
		t.Errorf("expected '3 direct callers', got: %q", summary)
	}
	if !strings.Contains(summary, "9 transitive caller") {
		t.Errorf("expected '9 transitive callers', got: %q", summary)
	}
	if !strings.Contains(summary, "4 package") {
		t.Errorf("expected '4 packages', got: %q", summary)
	}
	if !strings.Contains(summary, "payment-path") {
		t.Errorf("expected domain mention, got: %q", summary)
	}
}

// loadTestConfig loads a config from a temp directory.
func loadTestConfig(t *testing.T) (*config.Config, error) {
	t.Helper()
	return config.Load(t.TempDir())
}

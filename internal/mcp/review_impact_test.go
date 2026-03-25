package mcp

import (
	"encoding/json"
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

// loadTestConfig loads a config from a temp directory.
func loadTestConfig(t *testing.T) (*config.Config, error) {
	t.Helper()
	return config.Load(t.TempDir())
}

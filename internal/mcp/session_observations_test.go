package mcp

// Sprint 29.1: Tests for the session observation extraction pipeline.
// Covers the pure extraction logic (unit) and the end_session integration
// path (the observations appear in the store after end_session is called).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── Unit tests for extractSessionObservations ─────────────────────────────────

// TestExtractObservations_ToolUsage verifies that heavy validate usage produces
// a correct tool_usage observation.
func TestExtractObservations_ToolUsage(t *testing.T) {
	retro := &store.ToolCallSummary{
		TotalCalls: 20,
		TopTools: []store.ToolCallCount{
			{ToolName: "get_context", Count: 8},
			{ToolName: "validate", Count: 6},
			{ToolName: "memory", Count: 4},
		},
	}

	obs := extractSessionObservations("agent", "sess-1", "proj-1", nil, retro, nil)

	keys := obsKeys(obs)
	if !keys["heavy_validate_usage"] {
		t.Error("expected heavy_validate_usage observation")
	}
	if !keys["frequent_memory_saves"] {
		t.Error("expected frequent_memory_saves observation")
	}
}

// TestExtractObservations_SomeValidate verifies that low validate usage (1-4 calls)
// produces some_validate_usage (not heavy_validate_usage).
func TestExtractObservations_SomeValidate(t *testing.T) {
	retro := &store.ToolCallSummary{
		TotalCalls: 10,
		TopTools: []store.ToolCallCount{
			{ToolName: "get_context", Count: 7},
			{ToolName: "validate", Count: 2},
			{ToolName: "get_impact", Count: 4},
		},
	}

	obs := extractSessionObservations("agent", "sess-some-v", "proj-1", nil, retro, nil)

	keys := obsKeys(obs)
	if !keys["some_validate_usage"] {
		t.Error("expected some_validate_usage observation")
	}
	if keys["heavy_validate_usage"] {
		t.Error("unexpected heavy_validate_usage for count=2")
	}
	if !keys["uses_impact_analysis"] {
		t.Error("expected uses_impact_analysis observation for get_impact count=4")
	}
}

// TestExtractObservations_NoValidate verifies that the absence of validate with
// a high call volume produces a no_validate_usage observation.
func TestExtractObservations_NoValidate(t *testing.T) {
	retro := &store.ToolCallSummary{
		TotalCalls: 15,
		TopTools: []store.ToolCallCount{
			{ToolName: "get_context", Count: 10},
			{ToolName: "search", Count: 5},
		},
	}

	obs := extractSessionObservations("agent", "sess-2", "proj-1", nil, retro, nil)

	keys := obsKeys(obs)
	if !keys["no_validate_usage"] {
		t.Error("expected no_validate_usage observation")
	}
	if keys["heavy_validate_usage"] {
		t.Error("unexpected heavy_validate_usage")
	}
}

// TestExtractObservations_TestingPattern verifies Go test file detection.
func TestExtractObservations_TestingPattern(t *testing.T) {
	sess := &sessionSummary{
		FilesTouched: []string{
			"internal/handler/auth.go",
			"internal/handler/auth_test.go",
			"internal/service/user_test.go",
		},
	}

	obs := extractSessionObservations("agent", "sess-3", "proj-1", sess, nil, nil)

	keys := obsKeys(obs)
	if !keys["go_test_files_touched"] {
		t.Error("expected go_test_files_touched observation")
	}
	if keys["ts_test_files_touched"] {
		t.Error("unexpected ts_test_files_touched")
	}

	// Confidence should be >= 0.6 for 2 test files.
	for _, o := range obs {
		if o.Key == "go_test_files_touched" && o.Confidence < 0.6 {
			t.Errorf("low confidence for 2 test files: %v", o.Confidence)
		}
	}
}

// TestExtractObservations_LayeredArchitecture verifies that touching handler +
// service + repo files produces the layered_architecture_touched observation.
func TestExtractObservations_LayeredArchitecture(t *testing.T) {
	sess := &sessionSummary{
		FilesTouched: []string{
			"internal/handler/user.go",
			"internal/service/user.go",
			"internal/repo/user.go",
		},
	}

	obs := extractSessionObservations("agent", "sess-4", "proj-1", sess, nil, nil)

	keys := obsKeys(obs)
	if !keys["layered_architecture_touched"] {
		t.Errorf("expected layered_architecture_touched, got keys: %v", keys)
	}
}

// TestExtractObservations_LibraryUsage verifies that IMPORTS edges to known
// libraries produce library_usage observations.
func TestExtractObservations_LibraryUsage(t *testing.T) {
	g := graph.New("test-repo")

	// auth.go imports testify/assert and chi.
	authFile := "internal/handler/auth.go"
	testifyNode := g.MakeNodeID("vendor/github.com/stretchr/testify", "assert")
	chiNode := g.MakeNodeID("vendor/github.com/go-chi/chi", "chi")

	g.AddNode(&graph.Node{ID: testifyNode, Type: graph.NodeFile, File: "vendor/github.com/stretchr/testify/assert"})
	g.AddNode(&graph.Node{ID: chiNode, Type: graph.NodeFile, File: "vendor/github.com/go-chi/chi"})
	authNode := g.MakeNodeID(authFile, "auth")
	g.AddNode(&graph.Node{ID: authNode, Type: graph.NodeFile, File: authFile})

	// Add IMPORTS edges: auth → testify, auth → chi.
	g.AddEdge(&graph.Edge{From: authNode, To: testifyNode, Type: graph.EdgeImports})
	g.AddEdge(&graph.Edge{From: authNode, To: chiNode, Type: graph.EdgeImports})

	sess := &sessionSummary{
		FilesTouched: []string{authFile},
	}

	obs := extractSessionObservations("agent", "sess-5", "proj-1", sess, nil, g)

	keys := obsKeys(obs)
	if !keys["uses_testify"] {
		t.Error("expected uses_testify observation")
	}
	if !keys["uses_chi_router"] {
		t.Error("expected uses_chi_router observation")
	}
}

// TestExtractObservations_ApproachOutcome_Productive verifies that a session with
// files touched AND tasks updated produces a productive_session outcome.
func TestExtractObservations_ApproachOutcome_Productive(t *testing.T) {
	sess := &sessionSummary{
		FilesTouched: []string{"internal/auth.go"},
		TasksUpdated: []string{"task-42"},
	}

	obs := extractSessionObservations("agent", "sess-6", "proj-1", sess, nil, nil)

	keys := obsKeys(obs)
	if !keys["productive_session"] {
		t.Errorf("expected productive_session, got %v", keys)
	}
}

// TestExtractObservations_NilInputs verifies that nil retro and nil graph
// are handled gracefully (no panic).
func TestExtractObservations_NilInputs(t *testing.T) {
	obs := extractSessionObservations("agent", "sess-nil", "proj-1", nil, nil, nil)
	// Must not panic; should still produce at least an approach_outcome observation.
	found := false
	for _, o := range obs {
		if o.Category == store.ObsCategoryApproachOutcome {
			found = true
		}
	}
	if !found {
		t.Error("expected at least one approach_outcome observation with nil inputs")
	}
}

// TestExtractObservations_EmptySessionIDSkipped verifies that an empty sessionID
// produces no observations (guard against invalid DB inserts).
func TestExtractObservations_EmptySessionIDSkipped(t *testing.T) {
	obs := extractSessionObservations("agent", "", "proj-1", nil, nil, nil)
	if len(obs) != 0 {
		t.Errorf("expected 0 observations for empty sessionID, got %d", len(obs))
	}
}

// ── Integration test: end_session stores observations in the store ────────────

// TestEndSession_StoresObservations verifies that calling end_session causes
// session observations to be persisted and that ObservationsSaved > 0 appears
// in the response.
func TestEndSession_StoresObservations(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// Simulate a session_init so we have a valid synapseSessionID to attach
	// observations to. Without it, synapseSessionID is empty and the pipeline skips.
	ctx := context.Background()
	initRes, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "obs-agent",
		"intent":   "test observation pipeline",
	}))
	if err != nil || initRes.IsError {
		t.Fatalf("session_init failed: err=%v isError=%v", err, initRes.IsError)
	}

	res, err := srv.handleEndSession(ctx, callTool(map[string]any{
		"agent_id": "obs-agent",
		"summary":  "Implemented feature X",
	}))
	if err != nil {
		t.Fatalf("handleEndSession: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleEndSession error: %s", extractErrorText(t, res))
	}

	// Parse the result JSON.
	text := firstTextContent(res)
	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}

	// ObservationsSaved must be present and > 0.
	obs, _ := result["observations_saved"].(float64)
	if obs == 0 {
		t.Errorf("expected observations_saved > 0, got %v", result["observations_saved"])
	}
}

// ── Helpers ────────────────────────────────────────────────────────────────────

// obsKeys converts a slice of observations to a key→bool set for easy assertions.
func obsKeys(obs []store.SessionObservation) map[string]bool {
	m := make(map[string]bool, len(obs))
	for _, o := range obs {
		m[o.Key] = true
	}
	return m
}

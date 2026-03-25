package mcp

// HyDE (Hypothetical Document Embeddings) tests for handleSemanticSearch.
//
// These tests verify the fallback contract and integration wiring. The positive
// HyDE path (brain generates a non-empty hypothesis → embed hypothesis → vector
// search) is exercised through integration tests against a live Ollama instance;
// unit tests here cover all code paths reachable without a real LLM.

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain"
)

// hydeSearch is a test helper that calls handleSemanticSearch and returns the
// decoded JSON response map. Fatals on any error.
func hydeSearch(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	result, err := s.handleSemanticSearch(ctx, callTool(args))
	return mustResult(t, result, err)
}

// TestHyDE_NilBrainClient verifies that when brainClient is nil (brain not
// configured), handleSemanticSearch succeeds with no "hyde_hypothesis" key.
func TestHyDE_NilBrainClient(t *testing.T) {
	s := newTestServer(t)
	// brainClient is nil by default in newTestServer.
	m := hydeSearch(t, s, map[string]any{
		"query": "rate limiter",
		"mode":  "semantic",
	})
	if _, ok := m["hyde_hypothesis"]; ok {
		t.Error("hyde_hypothesis should not appear when brainClient is nil")
	}
}

// TestHyDE_NullBrainReturnsEmpty verifies that when a NullBrain client is wired
// (brain disabled), GenerateHypothetical returns "" and the search falls back to
// raw query embedding. No "hyde_hypothesis" key in the response.
func TestHyDE_NullBrainReturnsEmpty(t *testing.T) {
	s := newTestServer(t)
	s.brainClient = brain.NewInProcess(nil) // NullBrain: Generate always returns ""

	m := hydeSearch(t, s, map[string]any{
		"query": "authentication middleware",
		"mode":  "semantic",
	})
	// NullBrain returns empty hypothesis → raw query used → no hyde_hypothesis.
	if _, ok := m["hyde_hypothesis"]; ok {
		t.Error("hyde_hypothesis should not appear when brain returns empty hypothesis")
	}
}

// TestHyDE_FalseParam verifies that hyde=false skips hypothesis generation even
// when a brain client is available. The response must not contain "hyde_hypothesis".
func TestHyDE_FalseParam(t *testing.T) {
	s := newTestServer(t)
	s.brainClient = brain.NewInProcess(nil) // NullBrain (brain "available")

	m := hydeSearch(t, s, map[string]any{
		"query": "token bucket",
		"mode":  "semantic",
		"hyde":  false, // explicit opt-out
	})
	if _, ok := m["hyde_hypothesis"]; ok {
		t.Error("hyde_hypothesis should not appear when hyde=false")
	}
}

// TestHyDE_TrueParamExplicit verifies that hyde=true (explicit) behaves the same
// as omitting the parameter — HyDE is attempted, hypothesis may be empty from
// NullBrain, and no "hyde_hypothesis" key appears when hypothesis is empty.
func TestHyDE_TrueParamExplicit(t *testing.T) {
	s := newTestServer(t)
	s.brainClient = brain.NewInProcess(nil) // NullBrain returns ""

	m := hydeSearch(t, s, map[string]any{
		"query": "error handling middleware",
		"mode":  "semantic",
		"hyde":  true, // explicit opt-in (default)
	})
	// NullBrain → empty hypothesis → no hyde_hypothesis in response.
	if _, ok := m["hyde_hypothesis"]; ok {
		t.Error("hyde_hypothesis should not appear when hypothesis is empty (NullBrain)")
	}
}

// TestHyDE_FulltextModeSkipsHyDE verifies that mode=fulltext never triggers HyDE,
// even when a brain client is wired. search_mode must not contain "+hyde".
func TestHyDE_FulltextModeSkipsHyDE(t *testing.T) {
	s := newTestServer(t)
	s.brainClient = brain.NewInProcess(nil)

	m := hydeSearch(t, s, map[string]any{
		"query": "circuit breaker pattern",
		"mode":  "fulltext",
	})
	if _, ok := m["hyde_hypothesis"]; ok {
		t.Error("hyde_hypothesis must not appear for mode=fulltext")
	}
	if sm, _ := m["search_mode"].(string); strings.Contains(sm, "hyde") {
		t.Errorf("search_mode %q must not contain 'hyde' for mode=fulltext", sm)
	}
}

// TestHyDE_SearchModeNoHydeSuffix verifies that when HyDE is attempted but the
// hypothesis is empty (NullBrain), search_mode does not include "+hyde".
func TestHyDE_SearchModeNoHydeSuffix(t *testing.T) {
	s := newTestServer(t)
	s.brainClient = brain.NewInProcess(nil)

	m := hydeSearch(t, s, map[string]any{
		"query": "dependency injection",
		"mode":  "semantic",
	})
	if sm, _ := m["search_mode"].(string); strings.Contains(sm, "hyde") {
		t.Errorf("search_mode %q must not contain 'hyde' when hypothesis is empty", sm)
	}
}

// TestHyDE_SemanticModeWithNilBrainNoError verifies that mode=semantic with a nil
// brainClient completes without error — the nil-check guard works correctly.
func TestHyDE_SemanticModeWithNilBrainNoError(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	s.brainClient = nil

	req := callTool(map[string]any{
		"query": "AuthLogin",
		"mode":  "semantic",
	})
	// Must not panic or return a Go error.
	result, err := s.handleSemanticSearch(ctx, req)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatal("expected clean result, not a tool error")
	}
}

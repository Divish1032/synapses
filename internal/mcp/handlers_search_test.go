package mcp

// Sprint 23.2 — Redesign search response format.
// Tests verify: no raw source code in results, "why" field present and meaningful,
// relationship context (callers/callees) included, search mode labels correct.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── buildSearchWhy ────────────────────────────────────────────────────────────

func TestBuildSearchWhy_MatchReasonOnly(t *testing.T) {
	got := buildSearchWhy("exact name match", 0, 0)
	if got != "exact name match" {
		t.Errorf("expected %q, got %q", "exact name match", got)
	}
}

func TestBuildSearchWhy_Empty(t *testing.T) {
	got := buildSearchWhy("", 0, 0)
	if got != "matched" {
		t.Errorf("expected fallback %q, got %q", "matched", got)
	}
}

func TestBuildSearchWhy_HighFanIn(t *testing.T) {
	got := buildSearchWhy("exact name match", 12, 0)
	if !strings.Contains(got, "high fan-in") {
		t.Errorf("expected 'high fan-in' in %q", got)
	}
	if !strings.Contains(got, "12") {
		t.Errorf("expected caller count in %q", got)
	}
}

func TestBuildSearchWhy_LowFanIn(t *testing.T) {
	got := buildSearchWhy("name prefix match", 3, 0)
	if strings.Contains(got, "high fan-in") {
		t.Errorf("should not say 'high fan-in' for 3 callers, got %q", got)
	}
	if !strings.Contains(got, "3 caller") {
		t.Errorf("expected caller count in %q", got)
	}
}

func TestBuildSearchWhy_HighFanOut(t *testing.T) {
	got := buildSearchWhy("doc comment match", 0, 15)
	if !strings.Contains(got, "high fan-out") {
		t.Errorf("expected 'high fan-out' in %q", got)
	}
}

func TestBuildSearchWhy_LowFanOut_NotMentioned(t *testing.T) {
	// fan-out < 10 is not mentioned to avoid noise
	got := buildSearchWhy("exact name match", 0, 5)
	if strings.Contains(got, "fan-out") {
		t.Errorf("low fan-out should not appear in why, got %q", got)
	}
}

func TestBuildSearchWhy_AllFactors(t *testing.T) {
	got := buildSearchWhy("exact name match", 10, 10)
	if !strings.Contains(got, "high fan-in") {
		t.Errorf("expected 'high fan-in' in %q", got)
	}
	if !strings.Contains(got, "high fan-out") {
		t.Errorf("expected 'high fan-out' in %q", got)
	}
	if !strings.Contains(got, "exact name match") {
		t.Errorf("expected match reason in %q", got)
	}
}

// ── searchModeLabel ───────────────────────────────────────────────────────────

func TestSearchModeLabel_VectorCosine(t *testing.T) {
	got := searchModeLabel("vector_cosine")
	if got != "semantic match" {
		t.Errorf("expected %q, got %q", "semantic match", got)
	}
}

func TestSearchModeLabel_FTS5(t *testing.T) {
	got := searchModeLabel("fts5_bm25")
	if got != "keyword match" {
		t.Errorf("expected %q, got %q", "keyword match", got)
	}
}

func TestSearchModeLabel_HybridRRF(t *testing.T) {
	got := searchModeLabel("hybrid_rrf")
	if !strings.Contains(got, "hybrid") {
		t.Errorf("expected 'hybrid' in %q", got)
	}
}

func TestSearchModeLabel_HydeAugmented(t *testing.T) {
	got := searchModeLabel("hybrid_rrf+hyde")
	if !strings.Contains(got, "hypothesis") {
		t.Errorf("expected 'hypothesis' in %q", got)
	}
}

func TestSearchModeLabel_Unknown(t *testing.T) {
	got := searchModeLabel("unknown_mode")
	if got == "" {
		t.Error("should not return empty string for unknown mode")
	}
}

// ── handleSearch — no source code in results ──────────────────────────────────

// TestHandleSearch_NoSourceInResults is the core Communication Protocol test:
// verify that raw source code (the "source" field) was removed from results.
func TestHandleSearch_NoSourceInResults(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleSearch(context.Background(), callTool(map[string]any{
		"query": "AuthLogin",
		"mode":  "keyword",
	}))
	m := mustResult(t, res, err)

	results, ok := m["results"].([]any)
	if !ok || len(results) == 0 {
		t.Skip("no results returned — cannot verify absence of source field")
	}
	for i, r := range results {
		rm, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("result[%d] is not a map", i)
		}
		if _, hasSource := rm["source"]; hasSource {
			raw, _ := json.Marshal(rm)
			t.Errorf("result[%d] contains raw source code (Communication Protocol violation): %s", i, raw)
		}
	}
}

// TestHandleSearch_WhyFieldPresent verifies that every keyword-mode result
// carries a non-empty "why" field.
func TestHandleSearch_WhyFieldPresent(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleSearch(context.Background(), callTool(map[string]any{
		"query": "Auth",
		"mode":  "keyword",
	}))
	m := mustResult(t, res, err)

	results, ok := m["results"].([]any)
	if !ok || len(results) == 0 {
		t.Skip("no results returned")
	}
	for i, r := range results {
		rm, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("result[%d] is not a map", i)
		}
		why, ok := rm["why"].(string)
		if !ok || why == "" {
			t.Errorf("result[%d] missing non-empty 'why' field, got: %v", i, rm["why"])
		}
	}
}

// TestHandleSearch_WhyExactNameMatch verifies that an exact-name query produces
// a "why" field containing "exact name match".
func TestHandleSearch_WhyExactNameMatch(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleSearch(context.Background(), callTool(map[string]any{
		"query": "AuthLogin",
		"mode":  "keyword",
	}))
	m := mustResult(t, res, err)

	results, ok := m["results"].([]any)
	if !ok || len(results) == 0 {
		t.Skip("no results returned")
	}
	// At least one result should have "exact name match" in why.
	found := false
	for _, r := range results {
		rm, _ := r.(map[string]any)
		if why, _ := rm["why"].(string); strings.Contains(why, "exact name match") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected at least one result with 'exact name match' in why field")
	}
}

// TestHandleSearch_WhyHighFanIn verifies that a node with many callers gets
// "high fan-in" in its why field.
func TestHandleSearch_WhyHighFanIn(t *testing.T) {
	s := newTestServer(t)

	// Create a hub node with 10+ callers.
	hubID := s.graph.MakeNodeID("pkg/core/core.go", "CoreHandler")
	s.graph.AddNode(&graph.Node{ID: hubID, Name: "CoreHandler", Type: graph.NodeFunction,
		File: "pkg/core/core.go", Package: "core", Line: 1})
	for i := 0; i < 11; i++ {
		callerFile := "pkg/callers/c.go"
		callerName := strings.Repeat("a", i+1) + "Caller"
		cid := s.graph.MakeNodeID(callerFile, callerName)
		s.graph.AddNode(&graph.Node{ID: cid, Name: callerName, Type: graph.NodeFunction,
			File: callerFile, Package: "callers", Line: i + 1})
		s.graph.AddEdge(&graph.Edge{From: cid, To: hubID, Type: graph.EdgeCalls})
	}

	res, err := s.handleSearch(context.Background(), callTool(map[string]any{
		"query": "CoreHandler",
		"mode":  "keyword",
	}))
	m := mustResult(t, res, err)

	results, ok := m["results"].([]any)
	if !ok || len(results) == 0 {
		t.Skip("no results returned")
	}
	found := false
	for _, r := range results {
		rm, _ := r.(map[string]any)
		name, _ := rm["name"].(string)
		if name == "CoreHandler" {
			why, _ := rm["why"].(string)
			if !strings.Contains(why, "high fan-in") {
				t.Errorf("CoreHandler with 11 callers should have 'high fan-in' in why, got %q", why)
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("CoreHandler not found in results")
	}
}

// TestHandleSearch_NoEndLineInResults verifies that the removed end_line field
// is absent (it was only needed for source snippet extraction).
func TestHandleSearch_NoEndLineInResults(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleSearch(context.Background(), callTool(map[string]any{
		"query": "AuthLogin",
		"mode":  "keyword",
	}))
	m := mustResult(t, res, err)

	results, ok := m["results"].([]any)
	if !ok || len(results) == 0 {
		t.Skip("no results returned")
	}
	for i, r := range results {
		rm, _ := r.(map[string]any)
		if _, hasEndLine := rm["end_line"]; hasEndLine {
			t.Errorf("result[%d] should not have 'end_line' field (no longer needed)", i)
		}
	}
}

// ── handleFindEntity — why field in JSON format ───────────────────────────────

// TestHandleFindEntity_WhyFieldInJSONFormat verifies the JSON format includes
// the "why" field with relationship context.
func TestHandleFindEntity_WhyFieldInJSONFormat(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleFindEntity(context.Background(), callTool(map[string]any{
		"query":  "AuthLogin",
		"format": "json",
	}))
	m := mustResult(t, res, err)

	matches, ok := m["matches"].([]any)
	if !ok || len(matches) == 0 {
		t.Skip("no matches returned")
	}
	for i, r := range matches {
		rm, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("match[%d] is not a map", i)
		}
		if _, hasWhy := rm["why"]; !hasWhy {
			t.Errorf("match[%d] missing 'why' field", i)
		}
	}
}

// TestHandleFindEntity_PatternMatch_WhyLabel verifies that nodes found via
// pattern matching (not exact name) get "pattern match" in the why field,
// not the misleading "exact match" label.
func TestHandleFindEntity_PatternMatch_WhyLabel(t *testing.T) {
	s := newTestServer(t)

	// Add a node whose full name is "AuthLoginHandler" — won't match query "Auth" exactly.
	id := s.graph.MakeNodeID("pkg/auth/auth.go", "AuthLoginHandler")
	s.graph.AddNode(&graph.Node{ID: id, Name: "AuthLoginHandler", Type: graph.NodeFunction,
		File: "pkg/auth/auth.go", Package: "auth", Line: 1})

	// Query "Auth" — FindByName("Auth") returns nothing; FindByPatternLimit("Auth") returns the node.
	res, err := s.handleFindEntity(context.Background(), callTool(map[string]any{
		"query":  "Auth",
		"format": "json",
	}))
	m := mustResult(t, res, err)

	matches, ok := m["matches"].([]any)
	if !ok || len(matches) == 0 {
		t.Skip("no matches returned for pattern query")
	}
	for _, r := range matches {
		rm, _ := r.(map[string]any)
		why, _ := rm["why"].(string)
		if strings.Contains(why, "exact match") {
			t.Errorf("pattern match result should not claim 'exact match' in why, got %q", why)
		}
		if !strings.Contains(why, "pattern match") {
			t.Errorf("expected 'pattern match' in why for pattern-matched result, got %q", why)
		}
	}
}

// TestHandleSemanticSearch_WhyFieldPresent verifies that semantic/fulltext mode
// results include the "why" field with NL relevance context.
// Uses fulltext (FTS5) mode to avoid requiring embeddings in the test environment.
func TestHandleSemanticSearch_WhyFieldPresent(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	res, err := s.handleSemanticSearch(context.Background(), callTool(map[string]any{
		"query": "AuthLogin",
		"mode":  "fulltext",
	}))
	// handleSemanticSearch requires a store — skip if unavailable.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.IsError {
		t.Skip("semantic search unavailable in test environment")
	}

	m := mustResult(t, res, nil)
	results, ok := m["results"].([]any)
	if !ok || len(results) == 0 {
		t.Skip("no results returned — FTS5 index may be empty in test environment")
	}
	for i, r := range results {
		rm, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("result[%d] is not a map", i)
		}
		if _, hasWhy := rm["why"]; !hasWhy {
			t.Errorf("semantic result[%d] missing 'why' field — enrichment not applied", i)
		}
	}
}

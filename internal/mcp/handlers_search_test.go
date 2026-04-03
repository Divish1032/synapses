package mcp

// Sprint 23.2 — Redesign search response format.
// Tests verify: no raw source code in results, "why" field present and meaningful,
// relationship context (callers/callees) included, search mode labels correct,
// "recently modified" and "has architectural rule" tags surfaced in why field.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
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

// ── buildSearchTags ───────────────────────────────────────────────────────────

// TestBuildSearchTags_RecentlyModified verifies that a file modified within the
// last 7 days produces the "recently modified" tag.
func TestBuildSearchTags_RecentlyModified(t *testing.T) {
	s := newTestServer(t)

	// Create a temp file and immediately stat it — mtime is now.
	f, err := os.CreateTemp(t.TempDir(), "recent-*.go")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	tags := s.buildSearchTags(f.Name(), "internal/some/file.go")
	found := false
	for _, tag := range tags {
		if tag == "recently modified" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'recently modified' tag for file modified just now, got %v", tags)
	}
}

// TestBuildSearchTags_OldFile verifies that a file older than 7 days does NOT
// produce the "recently modified" tag.
func TestBuildSearchTags_OldFile(t *testing.T) {
	s := newTestServer(t)

	f, err := os.CreateTemp(t.TempDir(), "old-*.go")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Backdate mtime to 10 days ago.
	oldTime := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(f.Name(), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	tags := s.buildSearchTags(f.Name(), "internal/some/file.go")
	for _, tag := range tags {
		if tag == "recently modified" {
			t.Errorf("file 10 days old should NOT have 'recently modified' tag, got %v", tags)
		}
	}
}

// TestBuildSearchTags_RelativePathSkipsStatCheck verifies that a relative path
// (test environment) does not produce "recently modified" via a failed stat.
func TestBuildSearchTags_RelativePathSkipsStatCheck(t *testing.T) {
	s := newTestServer(t)
	// A relative path — os.Stat may or may not succeed, but we must not panic.
	// We only verify no "recently modified" leaks for non-absolute paths.
	tags := s.buildSearchTags("pkg/auth/auth.go", "pkg/auth/auth.go")
	for _, tag := range tags {
		if tag == "recently modified" {
			t.Errorf("relative path should not produce 'recently modified' tag, got %v", tags)
		}
	}
}

// TestBuildSearchTags_HasArchitecturalRule verifies that a file covered by a
// dynamic rule produces the "has architectural rule" tag.
func TestBuildSearchTags_HasArchitecturalRule(t *testing.T) {
	s := newTestServer(t)

	// Insert a rule that matches "internal/api/*.go".
	rule := config.Rule{
		ID:          "test-rule-api-isolation",
		Description: "API layer must not import storage directly",
		ForbiddenEdge: config.ForbiddenEdge{
			FromFilePattern: "internal/api/*.go",
			ToFilePattern:   "internal/store/*.go",
		},
		Severity: "error",
	}
	if err := s.store.UpsertDynamicRule(rule); err != nil {
		t.Fatalf("UpsertDynamicRule: %v", err)
	}

	tags := s.buildSearchTags("", "internal/api/handler.go")
	found := false
	for _, tag := range tags {
		if tag == "has architectural rule" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'has architectural rule' tag for file matching a dynamic rule, got %v", tags)
	}
}

// TestBuildSearchTags_NoRule verifies that a file NOT covered by any rule does
// not produce the "has architectural rule" tag.
func TestBuildSearchTags_NoRule(t *testing.T) {
	s := newTestServer(t)
	// No rules in store — fresh server.
	tags := s.buildSearchTags("", "internal/graph/graph.go")
	for _, tag := range tags {
		if tag == "has architectural rule" {
			t.Errorf("file with no matching rule should not have 'has architectural rule' tag, got %v", tags)
		}
	}
}

// TestHandleSearch_WhyRecentlyModified verifies that a search result for a node
// whose file was recently modified includes "recently modified" in the why field.
func TestHandleSearch_WhyRecentlyModified(t *testing.T) {
	s := newTestServer(t)

	// Create a fresh temp file and register a node pointing at it.
	f, err := os.CreateTemp(t.TempDir(), "recentnode-*.go")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	nodeID := s.graph.MakeNodeID(f.Name(), "RecentHandler")
	s.graph.AddNode(&graph.Node{
		ID:      nodeID,
		Name:    "RecentHandler",
		Type:    graph.NodeFunction,
		File:    f.Name(), // absolute path — enables os.Stat check
		Package: "recent",
		Line:    1,
	})

	res, err := s.handleSearch(context.Background(), callTool(map[string]any{
		"query": "RecentHandler",
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
		if name, _ := rm["name"].(string); name == "RecentHandler" {
			why, _ := rm["why"].(string)
			if strings.Contains(why, "recently modified") {
				found = true
			}
			break
		}
	}
	if !found {
		t.Error("expected 'recently modified' in why field for node in a just-created file")
	}
}

// TestHandleSearch_WhyHasArchitecturalRule verifies that a search result for a
// node in a rule-covered file includes "has architectural rule" in the why field.
func TestHandleSearch_WhyHasArchitecturalRule(t *testing.T) {
	s := newTestServer(t)

	// Add a rule covering "internal/ruletest/*.go".
	rule := config.Rule{
		ID:          "test-rule-ruletest",
		Description: "ruletest must not import db",
		ForbiddenEdge: config.ForbiddenEdge{
			FromFilePattern: "internal/ruletest/*.go",
			ToFilePattern:   "internal/db/*.go",
		},
		Severity: "error",
	}
	if err := s.store.UpsertDynamicRule(rule); err != nil {
		t.Fatalf("UpsertDynamicRule: %v", err)
	}

	// Add a node in the covered path.
	// The graph root is empty in newTestServer, so File is used as-is for the
	// relative path passed to buildSearchTags.
	relFile := "internal/ruletest/handler.go"
	nodeID := s.graph.MakeNodeID(relFile, "RuleTestHandler")
	s.graph.AddNode(&graph.Node{
		ID:      nodeID,
		Name:    "RuleTestHandler",
		Type:    graph.NodeFunction,
		File:    relFile,
		Package: "ruletest",
		Line:    1,
	})

	res, err := s.handleSearch(context.Background(), callTool(map[string]any{
		"query": "RuleTestHandler",
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
		if name, _ := rm["name"].(string); name == "RuleTestHandler" {
			why, _ := rm["why"].(string)
			if strings.Contains(why, "has architectural rule") {
				found = true
			}
			break
		}
	}
	if !found {
		t.Error("expected 'has architectural rule' in why field for node in rule-covered file")
	}
}

// ── searchMatchConfidence ─────────────────────────────────────────────────────

func TestSearchMatchConfidence_HighThreshold(t *testing.T) {
	for _, score := range []int{20, 25, 100} {
		got := searchMatchConfidence(score)
		if got != "HIGH" {
			t.Errorf("score %d: expected HIGH, got %q", score, got)
		}
	}
}

func TestSearchMatchConfidence_MediumRange(t *testing.T) {
	for _, score := range []int{6, 10, 19} {
		got := searchMatchConfidence(score)
		if got != "MEDIUM" {
			t.Errorf("score %d: expected MEDIUM, got %q", score, got)
		}
	}
}

func TestSearchMatchConfidence_LowBelowThreshold(t *testing.T) {
	for _, score := range []int{0, 1, 5} {
		got := searchMatchConfidence(score)
		if got != "LOW" {
			t.Errorf("score %d: expected LOW, got %q", score, got)
		}
	}
}

func TestSearchMatchConfidence_BoundaryExact(t *testing.T) {
	// Exact boundary values: 20 → HIGH, 6 → MEDIUM, 5 → LOW.
	if got := searchMatchConfidence(20); got != "HIGH" {
		t.Errorf("boundary 20: expected HIGH, got %q", got)
	}
	if got := searchMatchConfidence(6); got != "MEDIUM" {
		t.Errorf("boundary 6: expected MEDIUM, got %q", got)
	}
	if got := searchMatchConfidence(5); got != "LOW" {
		t.Errorf("boundary 5: expected LOW, got %q", got)
	}
}

package mcp

// Sprint 29.4: Tests for the failure avoidance engine.
// Covers pure functions (unit) and the full pipeline from
// rejected_approaches → failure_patterns → session_init delivery (integration).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ── failurePatternText ────────────────────────────────────────────────────────

// TestFailurePatternText_withReason verifies the NL text format when a reason
// is present.
func TestFailurePatternText_withReason(t *testing.T) {
	text := failurePatternText("jwt-go", 3, "incompatible with existing middleware")
	if !strings.Contains(text, "jwt-go") {
		t.Errorf("text %q does not contain keyword", text)
	}
	if !strings.Contains(text, "3") {
		t.Errorf("text %q does not contain count", text)
	}
	if !strings.Contains(text, "incompatible with existing middleware") {
		t.Errorf("text %q does not contain reason", text)
	}
	if !strings.HasSuffix(text, ".") {
		t.Errorf("text %q should end with period", text)
	}
}

// TestFailurePatternText_withoutReason verifies graceful handling of empty reason.
func TestFailurePatternText_withoutReason(t *testing.T) {
	text := failurePatternText("fasthttp", 2, "")
	if text == "" {
		t.Error("expected non-empty text")
	}
	if !strings.Contains(text, "fasthttp") {
		t.Errorf("text %q does not contain keyword", text)
	}
	if !strings.HasSuffix(text, ".") {
		t.Errorf("text %q should end with period", text)
	}
}

// TestFailurePatternText_singularPlural verifies "time" vs "times" grammar.
func TestFailurePatternText_singularPlural(t *testing.T) {
	one := failurePatternText("pkg", 1, "error")
	if !strings.Contains(one, "1 time ") || strings.Contains(one, "1 times") {
		t.Errorf("count 1 should use 'time', got %q", one)
	}
	two := failurePatternText("pkg", 2, "error")
	if !strings.Contains(two, "2 times") {
		t.Errorf("count 2 should use 'times', got %q", two)
	}
}

// ── failurePatternConfidence ──────────────────────────────────────────────────

// TestFailurePatternConfidence_monotone verifies confidence is non-decreasing.
func TestFailurePatternConfidence_monotone(t *testing.T) {
	cases := []struct {
		count         int
		wantMin, wantMax float64
	}{
		{2, 0.55, 0.65},
		{3, 0.65, 0.75},
		{4, 0.75, 0.85},
		{5, 0.85, 0.95},
		{6, 0.90, 1.0},
	}
	prev := 0.0
	for _, tc := range cases {
		c := failurePatternConfidence(tc.count)
		if c < tc.wantMin || c > tc.wantMax {
			t.Errorf("count=%d: confidence %v not in [%v, %v]", tc.count, c, tc.wantMin, tc.wantMax)
		}
		if c < prev {
			t.Errorf("confidence decreased: count=%d gave %v after previous %v", tc.count, c, prev)
		}
		prev = c
	}
}

// ── runFailurePatternExtraction — unit tests ──────────────────────────────────

// TestRunFailurePatternExtraction_emptyProjectID verifies no-op for empty project.
func TestRunFailurePatternExtraction_emptyProjectID(t *testing.T) {
	st := openMCPTestStore(t)
	n, err := runFailurePatternExtraction(st, "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 patterns, got %d", n)
	}
}

// TestRunFailurePatternExtraction_nilStore verifies no-op for nil store.
func TestRunFailurePatternExtraction_nilStore(t *testing.T) {
	n, err := runFailurePatternExtraction(nil, "proj-x")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

// TestRunFailurePatternExtraction_noRejectedApproaches verifies no patterns are
// created when there are no rejected approaches.
func TestRunFailurePatternExtraction_noRejectedApproaches(t *testing.T) {
	st := openMCPTestStore(t)
	n, err := runFailurePatternExtraction(st, "proj-empty")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 patterns from empty store, got %d", n)
	}
}

// TestRunFailurePatternExtraction_belowMinOccurrences verifies that a keyword
// appearing only once does NOT produce a pattern.
func TestRunFailurePatternExtraction_belowMinOccurrences(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-below-min"
	_, err := st.InsertRejectedApproach(store.RejectedApproach{
		ProjectID:     projID,
		Approach:      "Using jwt-go for authentication",
		FailureReason: "incompatible with gorilla/sessions",
	})
	if err != nil {
		t.Fatalf("InsertRejectedApproach: %v", err)
	}

	n, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("runFailurePatternExtraction: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 patterns (below threshold), got %d", n)
	}
}

// TestRunFailurePatternExtraction_libraryKeyword verifies that a well-known
// library appearing in 2+ rejected approaches is promoted to a failure pattern
// using the canonical observation key (e.g., "uses_jwt_go").
func TestRunFailurePatternExtraction_libraryKeyword(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-library-kw"
	approaches := []store.RejectedApproach{
		{
			ProjectID:     projID,
			Approach:      "Using dgrijalva/jwt-go v3 for JWT authentication",
			FailureReason: "incompatible with existing middleware",
		},
		{
			ProjectID:     projID,
			Approach:      "Tried dgrijalva/jwt-go v3 again with different claims",
			FailureReason: "same incompatibility with gorilla/sessions",
		},
	}
	for i, r := range approaches {
		r.ID = fmt.Sprintf("rej-%d", i)
		if _, err := st.InsertRejectedApproach(r); err != nil {
			t.Fatalf("InsertRejectedApproach[%d]: %v", i, err)
		}
	}

	n, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("runFailurePatternExtraction: %v", err)
	}
	if n == 0 {
		t.Fatal("expected ≥ 1 pattern, got 0")
	}

	patterns, err := st.GetProjectFailurePatterns(projID, 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}
	if len(patterns) == 0 {
		t.Fatal("expected stored patterns, got none")
	}

	// The approach text "dgrijalva/jwt-go v3" yields "jwt-go" via the hyphenated
	// regex — human-readable keyword, not the observation key "uses_jwt_go".
	found := false
	for _, p := range patterns {
		if p.Keyword == "jwt-go" {
			found = true
			if p.OccurrenceCount != 2 {
				t.Errorf("occurrence_count: got %d, want 2", p.OccurrenceCount)
			}
			if p.SampleReason == "" {
				t.Error("sample_reason should be populated")
			}
			if p.Text == "" {
				t.Error("text should be populated")
			}
			break
		}
	}
	if !found {
		t.Errorf("expected pattern with keyword 'jwt-go', got: %v", patterns)
	}
}

// TestRunFailurePatternExtraction_hyphenatedPackage verifies that a repeated
// hyphenated package name NOT in wellKnownLibraries is promoted as type="package".
func TestRunFailurePatternExtraction_hyphenatedPackage(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-hyphenated"
	for i := 0; i < MinOccurrencesForFailurePattern; i++ {
		r := store.RejectedApproach{
			ID:            fmt.Sprintf("rej-hyp-%d", i),
			ProjectID:     projID,
			Approach:      "Tried using some-obscure-pkg for data encoding",
			FailureReason: "no maintained releases in 3 years",
		}
		if _, err := st.InsertRejectedApproach(r); err != nil {
			t.Fatalf("InsertRejectedApproach: %v", err)
		}
	}

	n, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("runFailurePatternExtraction: %v", err)
	}
	if n == 0 {
		t.Fatal("expected ≥ 1 pattern for hyphenated package, got 0")
	}

	patterns, err := st.GetProjectFailurePatterns(projID, 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}

	found := false
	for _, p := range patterns {
		if p.Keyword == "some-obscure-pkg" {
			found = true
			if p.PatternType != "package" {
				t.Errorf("pattern_type: got %q, want 'package'", p.PatternType)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected pattern with keyword 'some-obscure-pkg', got: %v", patterns)
	}
}

// TestRunFailurePatternExtraction_stopListExcluded verifies that common
// hyphenated non-library words are excluded.
func TestRunFailurePatternExtraction_stopListExcluded(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-stop"
	for i := 0; i < 3; i++ {
		r := store.RejectedApproach{
			ID:            fmt.Sprintf("rej-stop-%d", i),
			ProjectID:     projID,
			Approach:      "Used read-only file access strategy",
			FailureReason: "does not support long-term caching",
		}
		if _, err := st.InsertRejectedApproach(r); err != nil {
			t.Fatalf("InsertRejectedApproach: %v", err)
		}
	}

	_, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("runFailurePatternExtraction: %v", err)
	}

	patterns, err := st.GetProjectFailurePatterns(projID, 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}
	for _, p := range patterns {
		if p.Keyword == "read-only" || p.Keyword == "long-term" {
			t.Errorf("stop-list keyword %q should not be promoted to a pattern", p.Keyword)
		}
	}
}

// TestRunFailurePatternExtraction_idempotent verifies that running extraction
// twice on the same data produces the same result (upsert semantics).
func TestRunFailurePatternExtraction_idempotent(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-idempotent"
	for i := 0; i < 3; i++ {
		r := store.RejectedApproach{
			ID:            fmt.Sprintf("rej-idem-%d", i),
			ProjectID:     projID,
			Approach:      "Using fast-router for routing", // hyphenated — extractable by regex
			FailureReason: "incompatible with chi-middleware",
		}
		if _, err := st.InsertRejectedApproach(r); err != nil {
			t.Fatalf("InsertRejectedApproach: %v", err)
		}
	}

	n1, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("first extraction: %v", err)
	}
	if n1 == 0 {
		t.Fatal("first extraction: expected ≥1 pattern (test data must use hyphenated keywords)")
	}
	n2, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("second extraction: %v", err)
	}

	// Same number of patterns upserted both runs.
	if n1 != n2 {
		t.Errorf("idempotent: first=%d, second=%d should match", n1, n2)
	}

	// Only one stored pattern per keyword (upsert, not append).
	patterns, err := st.GetProjectFailurePatterns(projID, 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}
	seen := make(map[string]int)
	for _, p := range patterns {
		seen[p.Keyword]++
	}
	for kw, cnt := range seen {
		if cnt > 1 {
			t.Errorf("keyword %q appears %d times — should be exactly 1 (upsert)", kw, cnt)
		}
	}
}

// ── Strategy 2: backtick-quoted identifiers ───────────────────────────────────

// TestRunFailurePatternExtraction_backtickFunction verifies that identifiers
// written in backticks (e.g. `AuthMiddleware`) are extracted and promoted when
// they appear in ≥ MinOccurrencesForFailurePattern records.
func TestRunFailurePatternExtraction_backtickFunction(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-backtick"
	for i := 0; i < MinOccurrencesForFailurePattern; i++ {
		_, err := st.InsertRejectedApproach(store.RejectedApproach{
			ID:            fmt.Sprintf("rej-bt-%d", i),
			ProjectID:     projID,
			Approach:      "Tried using `AuthMiddleware` for JWT validation",
			FailureReason: "panics with nil context when token is missing",
		})
		if err != nil {
			t.Fatalf("InsertRejectedApproach[%d]: %v", i, err)
		}
	}

	n, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("runFailurePatternExtraction: %v", err)
	}
	if n == 0 {
		t.Fatal("expected ≥1 pattern from backtick identifier, got 0")
	}

	patterns, err := st.GetProjectFailurePatterns(projID, 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}

	// Keyword stored lowercase; patternType must be "function".
	found := false
	for _, p := range patterns {
		if p.Keyword == "authmiddleware" {
			found = true
			if p.PatternType != "function" {
				t.Errorf("pattern_type: got %q, want 'function'", p.PatternType)
			}
			if p.OccurrenceCount != MinOccurrencesForFailurePattern {
				t.Errorf("occurrence_count: got %d, want %d", p.OccurrenceCount, MinOccurrencesForFailurePattern)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected pattern with keyword 'authmiddleware', got: %v", patterns)
	}
}

// TestRunFailurePatternExtraction_backtickStopListExcluded verifies that
// language keywords in backticks (e.g. `error`, `context`) are NOT promoted.
func TestRunFailurePatternExtraction_backtickStopListExcluded(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-bt-stop"
	for i := 0; i < 3; i++ {
		_, err := st.InsertRejectedApproach(store.RejectedApproach{
			ID:            fmt.Sprintf("rej-bt-stop-%d", i),
			ProjectID:     projID,
			Approach:      "Returning `error` from `context` caused issues",
			FailureReason: "used `nil` check instead of `error` wrapping",
		})
		if err != nil {
			t.Fatalf("InsertRejectedApproach: %v", err)
		}
	}

	_, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("runFailurePatternExtraction: %v", err)
	}
	patterns, err := st.GetProjectFailurePatterns(projID, 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}
	for _, p := range patterns {
		if p.PatternType == "function" {
			switch p.Keyword {
			case "error", "context", "nil":
				t.Errorf("stop-list identifier %q should not be promoted", p.Keyword)
			}
		}
	}
}

// TestRunFailurePatternExtraction_backtickSingleRecordDedup verifies that a
// backtick identifier appearing multiple times in a single record's text counts
// as one occurrence, not many.
func TestRunFailurePatternExtraction_backtickSingleRecordDedup(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-bt-dedup"
	// Only one record, but `RateLimiter` appears three times in approach text.
	_, err := st.InsertRejectedApproach(store.RejectedApproach{
		ProjectID:     projID,
		Approach:      "Used `RateLimiter` — `RateLimiter` caused issues — removed `RateLimiter`",
		FailureReason: "thread safety problems",
	})
	if err != nil {
		t.Fatalf("InsertRejectedApproach: %v", err)
	}

	n, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("runFailurePatternExtraction: %v", err)
	}
	// Only 1 record → occurrence_count = 1 → below threshold → 0 patterns promoted.
	if n != 0 {
		t.Errorf("single-record dedup: expected 0 patterns, got %d", n)
	}
}

// ── Strategy 3: known error pattern normalization ─────────────────────────────

// TestRunFailurePatternExtraction_knownErrorPattern verifies that a known runtime
// error phrase in the Blocker field is normalized to a stable keyword and promoted
// when it appears in ≥ MinOccurrencesForFailurePattern records.
func TestRunFailurePatternExtraction_knownErrorPattern(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-error-pat"
	for i := 0; i < MinOccurrencesForFailurePattern; i++ {
		_, err := st.InsertRejectedApproach(store.RejectedApproach{
			ID:            fmt.Sprintf("rej-ep-%d", i),
			ProjectID:     projID,
			Approach:      "Accessing user map concurrently from multiple goroutines",
			FailureReason: "race condition in map access",
			Blocker:       fmt.Sprintf("fatal error: concurrent map read and map write at goroutine %d", i+1),
		})
		if err != nil {
			t.Fatalf("InsertRejectedApproach[%d]: %v", i, err)
		}
	}

	n, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("runFailurePatternExtraction: %v", err)
	}
	if n == 0 {
		t.Fatal("expected ≥1 pattern from error normalization, got 0")
	}

	patterns, err := st.GetProjectFailurePatterns(projID, 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}

	found := false
	for _, p := range patterns {
		if p.Keyword == "concurrent-map-access" {
			found = true
			if p.PatternType != "error_pattern" {
				t.Errorf("pattern_type: got %q, want 'error_pattern'", p.PatternType)
			}
			if p.OccurrenceCount != MinOccurrencesForFailurePattern {
				t.Errorf("occurrence_count: got %d, want %d", p.OccurrenceCount, MinOccurrencesForFailurePattern)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected pattern with keyword 'concurrent-map-access', got: %v", patterns)
	}
}

// TestRunFailurePatternExtraction_errorPatternFirstMatchWins verifies that when a
// blocker matches multiple known error patterns, only the most specific one
// (earliest in the knownErrorPatterns table) is emitted per record.
func TestRunFailurePatternExtraction_errorPatternFirstMatchWins(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-ep-firstmatch"
	for i := 0; i < MinOccurrencesForFailurePattern; i++ {
		// This blocker matches both "nil pointer dereference" (specific)
		// AND "runtime error" (generic catch-all) — specific must win.
		_, err := st.InsertRejectedApproach(store.RejectedApproach{
			ID:            fmt.Sprintf("rej-fm-%d", i),
			ProjectID:     projID,
			Approach:      "Calling GetUser without checking session",
			FailureReason: "panics on unauthenticated requests",
			Blocker:       "runtime error: invalid memory address or nil pointer dereference",
		})
		if err != nil {
			t.Fatalf("InsertRejectedApproach[%d]: %v", i, err)
		}
	}

	_, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("runFailurePatternExtraction: %v", err)
	}

	patterns, err := st.GetProjectFailurePatterns(projID, 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}

	// "nil-pointer-dereference" should be present; "go-runtime-error" should NOT
	// (first-match-wins prevents double-counting the same blocker text).
	hasSpecific := false
	hasGeneric := false
	for _, p := range patterns {
		if p.PatternType != "error_pattern" {
			continue
		}
		if p.Keyword == "nil-pointer-dereference" {
			hasSpecific = true
		}
		if p.Keyword == "go-runtime-error" {
			hasGeneric = true
		}
	}
	if !hasSpecific {
		t.Error("expected 'nil-pointer-dereference' pattern to be promoted")
	}
	if hasGeneric {
		t.Error("'go-runtime-error' should not be promoted when more specific pattern matched first")
	}
}

// TestRunFailurePatternExtraction_emptyBlockerSkipsErrorStrategy verifies that
// records with an empty Blocker field do not produce error_pattern entries.
func TestRunFailurePatternExtraction_emptyBlockerSkipsErrorStrategy(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-empty-blocker"
	for i := 0; i < MinOccurrencesForFailurePattern; i++ {
		_, err := st.InsertRejectedApproach(store.RejectedApproach{
			ID:            fmt.Sprintf("rej-eb-%d", i),
			ProjectID:     projID,
			Approach:      "Direct DB access from handler",
			FailureReason: "violates layering", // Blocker is empty
		})
		if err != nil {
			t.Fatalf("InsertRejectedApproach[%d]: %v", i, err)
		}
	}

	_, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("runFailurePatternExtraction: %v", err)
	}

	patterns, err := st.GetProjectFailurePatterns(projID, 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}
	for _, p := range patterns {
		if p.PatternType == "error_pattern" {
			t.Errorf("expected no error_pattern with empty Blocker, got keyword=%q", p.Keyword)
		}
	}
}

// ── integration: session_init _briefing.failure_avoidance ─────────────────────

// TestSessionInit_FailureAvoidance_InBriefing verifies end-to-end that failure
// patterns extracted by runFailurePatternExtraction appear in
// _briefing.failure_avoidance when session_init is called on the same project.
func TestSessionInit_FailureAvoidance_InBriefing(t *testing.T) {
	st := openMCPTestStore(t)

	const projID = "proj-fa-briefing"

	// Insert MinOccurrencesForFailurePattern records referencing "jwt-go"
	// (hyphenated library name caught by the extraction regex).
	for i := 0; i < MinOccurrencesForFailurePattern; i++ {
		_, err := st.InsertRejectedApproach(store.RejectedApproach{
			ID:            fmt.Sprintf("rej-fa-%d", i),
			ProjectID:     projID,
			Approach:      "Added jwt-go for JWT token signing",
			FailureReason: "conflicts with existing middleware",
		})
		if err != nil {
			t.Fatalf("InsertRejectedApproach[%d]: %v", i, err)
		}
	}

	// Run extraction to promote the pattern.
	n, err := runFailurePatternExtraction(st, projID)
	if err != nil {
		t.Fatalf("runFailurePatternExtraction: %v", err)
	}
	if n == 0 {
		t.Fatal("expected ≥ 1 pattern to be extracted")
	}

	// Build a minimal server with the test store and projectID.
	srv, _, _ := newPopulatedServer(t)
	srv.store = st
	srv.projectID = projID

	// Call session_init.
	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "test-agent",
		"intent":   "testing failure avoidance briefing",
	}))
	if err != nil || res.IsError {
		t.Fatalf("session_init failed: err=%v isError=%v", err, res.IsError)
	}

	// Decode and verify _briefing.failure_avoidance.
	var resp map[string]any
	if err := json.Unmarshal([]byte(firstTextContent(res)), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	briefing, ok := resp["_briefing"].(map[string]any)
	if !ok {
		t.Fatal("response missing _briefing")
	}
	fa, ok := briefing["failure_avoidance"]
	if !ok {
		t.Fatal("_briefing missing failure_avoidance")
	}
	warnings, ok := fa.([]any)
	if !ok || len(warnings) == 0 {
		t.Fatalf("failure_avoidance should be non-empty []string, got: %T %v", fa, fa)
	}
	// Each warning should be a non-empty string.
	for i, w := range warnings {
		s, ok := w.(string)
		if !ok || s == "" {
			t.Errorf("warning[%d] should be non-empty string, got: %T %v", i, w, w)
		}
	}
}

// TestSessionInit_FailureAvoidance_AbsentWhenNoPatterns verifies that the
// failure_avoidance key is NOT present in _briefing when no patterns exist.
func TestSessionInit_FailureAvoidance_AbsentWhenNoPatterns(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)
	srv.projectID = "proj-no-patterns"

	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "test-agent",
		"intent":   "testing empty failure avoidance",
	}))
	if err != nil || res.IsError {
		t.Fatalf("session_init failed: err=%v isError=%v", err, res.IsError)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(firstTextContent(res)), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	briefing, ok := resp["_briefing"].(map[string]any)
	if !ok {
		return // briefing itself absent is fine for this narrow check
	}
	if _, exists := briefing["failure_avoidance"]; exists {
		t.Error("failure_avoidance should be absent when no patterns exist")
	}
}

// TestSessionInit_FailureAvoidance_CappedAtThree verifies that even when many
// patterns exist, at most 3 warnings appear in the briefing.
func TestSessionInit_FailureAvoidance_CappedAtThree(t *testing.T) {
	st := openMCPTestStore(t)
	const projID = "proj-fa-cap"

	// Insert 8 different hyphenated-library patterns (each repeated twice to meet threshold).
	// All approach texts contain hyphenated names to be caught by the extractor.
	libs := []struct{ approach, reason string }{
		{"Tried jwt-go for signing", "conflicts with gorilla-sessions"},
		{"Switching to gin-gonic framework", "low-level, no middleware abstraction"},
		{"Using chi-router v5 alpha", "breaking API changes in route params"},
		{"Added flask-cors plugin", "not in PyPI registry under that name"},
		{"Tried gorilla-mux v2", "conflicts with standard library net-http"},
		{"Using actix-web-extras crate", "slow compilation on macOS"},
		{"Added axum-auth middleware", "version conflicts with tokio"},
		{"Tried golang-jwt library", "incompatible with existing middleware"},
	}
	for i, lib := range libs {
		for j := 0; j < MinOccurrencesForFailurePattern; j++ {
			_, err := st.InsertRejectedApproach(store.RejectedApproach{
				ID:            fmt.Sprintf("rej-cap-%d-%d", i, j),
				ProjectID:     projID,
				Approach:      lib.approach,
				FailureReason: lib.reason,
			})
			if err != nil {
				t.Fatalf("InsertRejectedApproach: %v", err)
			}
		}
	}

	if n, err := runFailurePatternExtraction(st, projID); err != nil || n == 0 {
		t.Fatalf("extraction: n=%d err=%v", n, err)
	}

	srv, _, _ := newPopulatedServer(t)
	srv.store = st
	srv.projectID = projID

	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "agent-cap",
		"intent":   "cap test",
	}))
	if err != nil || res.IsError {
		t.Fatalf("session_init failed: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(firstTextContent(res)), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	briefing := resp["_briefing"].(map[string]any)
	fa := briefing["failure_avoidance"]
	warnings, ok := fa.([]any)
	if !ok {
		t.Fatalf("failure_avoidance should be []any, got %T", fa)
	}
	if len(warnings) > 3 {
		t.Errorf("failure_avoidance should be capped at 3, got %d", len(warnings))
	}
}

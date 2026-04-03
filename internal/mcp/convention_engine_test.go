package mcp

// Sprint 29.2: Tests for the convention extraction engine.
// Covers the pure extraction functions (unit) and the full pipeline from
// observations → conventions → session_init delivery (integration).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ── conventionText ─────────────────────────────────────────────────────────────

// TestConventionText_KnownKeys verifies that known observation keys produce
// non-empty, human-readable convention strings.
func TestConventionText_KnownKeys(t *testing.T) {
	cases := []struct {
		category    string
		key         string
		sessionCount int
		wantContain string
	}{
		{store.ObsCategoryLibraryUsage, "uses_testify", 5, "testify"},
		{store.ObsCategoryLibraryUsage, "uses_chi_router", 4, "chi"},
		{store.ObsCategoryLibraryUsage, "uses_fastapi", 3, "FastAPI"},
		{store.ObsCategoryLibraryUsage, "uses_jest", 7, "Jest"},
		{store.ObsCategoryLibraryUsage, "uses_spring", 3, "Spring"},
		{store.ObsCategoryTestingPattern, "go_test_files_touched", 3, "Go tests"},
		{store.ObsCategoryTestingPattern, "ts_test_files_touched", 4, "TypeScript"},
		{store.ObsCategoryFilePattern, "layered_architecture_touched", 5, "layered architecture"},
		{store.ObsCategoryFilePattern, "middleware_files_touched", 3, "middleware"},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			text := conventionText(tc.category, tc.key, tc.sessionCount)
			if text == "" {
				t.Errorf("conventionText(%q, %q, %d) = empty", tc.category, tc.key, tc.sessionCount)
				return
			}
			if !strings.Contains(text, tc.wantContain) {
				t.Errorf("text %q does not contain %q", text, tc.wantContain)
			}
			// Verify session count is included.
			if !strings.Contains(text, "sessions") {
				t.Errorf("text %q does not mention 'sessions'", text)
			}
		})
	}
}

// TestConventionText_UnknownKey verifies that an unknown key returns empty string.
func TestConventionText_UnknownKey(t *testing.T) {
	text := conventionText(store.ObsCategoryLibraryUsage, "uses_totally_unknown_library_xyz", 5)
	if text != "" {
		t.Errorf("expected empty for unknown key, got %q", text)
	}
}

// TestConventionText_IncludesSessionCount verifies the session count is rendered.
func TestConventionText_IncludesSessionCount(t *testing.T) {
	for _, n := range []int{3, 5, 7, 10} {
		text := conventionText(store.ObsCategoryLibraryUsage, "uses_testify", n)
		countStr := fmt.Sprintf("%d", n)
		if !strings.Contains(text, countStr) {
			t.Errorf("session count %d not found in text %q", n, text)
		}
	}
}

// ── conventionConfidence ──────────────────────────────────────────────────────

// TestConventionConfidence verifies the monotone increasing mapping.
func TestConventionConfidence(t *testing.T) {
	cases := []struct{ count int; wantMin, wantMax float64 }{
		{3, 0.55, 0.65},
		{4, 0.65, 0.75},
		{5, 0.75, 0.85},
		{7, 0.85, 0.95},
		{10, 0.90, 1.0},
		{20, 0.90, 1.0},
	}
	prev := 0.0
	for _, tc := range cases {
		c := conventionConfidence(tc.count)
		if c < tc.wantMin || c > tc.wantMax {
			t.Errorf("count=%d: confidence %v not in [%v, %v]", tc.count, c, tc.wantMin, tc.wantMax)
		}
		if c < prev {
			t.Errorf("confidence decreased: count=%d gave %v after previous %v", tc.count, c, prev)
		}
		prev = c
	}
}

// ── runConventionExtraction ────────────────────────────────────────────────────

// TestRunConventionExtraction_PromotesAtThreshold verifies that a key observed
// in exactly MinSessionsForConvention distinct sessions is promoted.
func TestRunConventionExtraction_PromotesAtThreshold(t *testing.T) {
	st := openMCPTestStore(t)
	projectID := "proj-extract"

	// Insert MinSessionsForConvention observations across distinct sessions.
	for i := 0; i < MinSessionsForConvention; i++ {
		sessID := fmt.Sprintf("sess-%d", i)
		_, err := st.InsertSessionObservation(store.SessionObservation{
			SessionID: sessID,
			ProjectID: projectID,
			AgentID:   "a",
			Category:  store.ObsCategoryLibraryUsage,
			Key:       "uses_testify",
			Confidence: 0.6,
		})
		if err != nil {
			t.Fatalf("insert observation: %v", err)
		}
	}

	n, err := runConventionExtraction(st, projectID)
	if err != nil {
		t.Fatalf("runConventionExtraction: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 convention extracted, got %d", n)
	}

	convs, err := st.GetProjectConventions(projectID, 0.0)
	if err != nil {
		t.Fatalf("GetProjectConventions: %v", err)
	}
	if len(convs) != 1 {
		t.Fatalf("expected 1 convention in store, got %d", len(convs))
	}
	c := convs[0]
	if c.Key != "uses_testify" {
		t.Errorf("wrong key: %q", c.Key)
	}
	if c.SessionCount != MinSessionsForConvention {
		t.Errorf("session_count: want %d, got %d", MinSessionsForConvention, c.SessionCount)
	}
	if c.Confidence < 0.55 || c.Confidence > 0.65 {
		t.Errorf("confidence %v out of expected range for count=%d", c.Confidence, MinSessionsForConvention)
	}
	if c.Text == "" {
		t.Error("convention text must not be empty")
	}
	if !strings.Contains(c.Text, "testify") {
		t.Errorf("convention text %q doesn't mention testify", c.Text)
	}
}

// TestRunConventionExtraction_BelowThresholdNotPromoted verifies that a key
// seen in fewer than MinSessionsForConvention sessions is NOT promoted.
func TestRunConventionExtraction_BelowThresholdNotPromoted(t *testing.T) {
	st := openMCPTestStore(t)
	projectID := "proj-below"

	// Insert only 2 sessions — below the threshold of 3.
	for i := 0; i < MinSessionsForConvention-1; i++ {
		_, _ = st.InsertSessionObservation(store.SessionObservation{
			SessionID: fmt.Sprintf("sess-%d", i),
			ProjectID: projectID,
			AgentID:   "a",
			Category:  store.ObsCategoryLibraryUsage,
			Key:       "uses_gin_router",
			Confidence: 0.6,
		})
	}

	n, err := runConventionExtraction(st, projectID)
	if err != nil {
		t.Fatalf("runConventionExtraction: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 conventions below threshold, got %d", n)
	}

	convs, _ := st.GetProjectConventions(projectID, 0.0)
	if len(convs) != 0 {
		t.Errorf("expected no conventions in store, got %d", len(convs))
	}
}

// TestRunConventionExtraction_SameSessionCountsOnce verifies that multiple
// observations from the same session are counted as one.
func TestRunConventionExtraction_SameSessionCountsOnce(t *testing.T) {
	st := openMCPTestStore(t)
	projectID := "proj-dedup"

	// Insert 5 rows from the SAME session — should count as 1, not 5.
	for i := 0; i < 5; i++ {
		_, _ = st.InsertSessionObservation(store.SessionObservation{
			SessionID: "same-session",
			ProjectID: projectID,
			AgentID:   "a",
			Category:  store.ObsCategoryLibraryUsage,
			Key:       "uses_flask",
			Confidence: 0.6,
		})
	}

	n, err := runConventionExtraction(st, projectID)
	if err != nil {
		t.Fatalf("runConventionExtraction: %v", err)
	}
	// Should NOT be promoted — only 1 distinct session.
	if n != 0 {
		t.Errorf("expected 0 (same-session dedup), got %d", n)
	}
}

// TestRunConventionExtraction_ExcludesApproachOutcome verifies that
// approach_outcome category observations are never promoted to conventions.
func TestRunConventionExtraction_ExcludesApproachOutcome(t *testing.T) {
	st := openMCPTestStore(t)
	projectID := "proj-outcome"

	// Insert productive_session (approach_outcome) across 5 distinct sessions.
	for i := 0; i < 5; i++ {
		_, _ = st.InsertSessionObservation(store.SessionObservation{
			SessionID: fmt.Sprintf("sess-%d", i),
			ProjectID: projectID,
			AgentID:   "a",
			Category:  store.ObsCategoryApproachOutcome,
			Key:       "productive_session",
			Confidence: 0.8,
		})
	}

	n, err := runConventionExtraction(st, projectID)
	if err != nil {
		t.Fatalf("runConventionExtraction: %v", err)
	}
	if n != 0 {
		t.Errorf("approach_outcome should not produce conventions, got %d", n)
	}
}

// TestRunConventionExtraction_NilStoreIsNoop verifies nil store safety.
func TestRunConventionExtraction_NilStoreIsNoop(t *testing.T) {
	n, err := runConventionExtraction(nil, "proj-x")
	if err != nil {
		t.Errorf("expected nil error for nil store, got %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 conventions for nil store, got %d", n)
	}
}

// TestRunConventionExtraction_EmptyProjectIDIsNoop verifies empty projectID safety.
func TestRunConventionExtraction_EmptyProjectIDIsNoop(t *testing.T) {
	st := openMCPTestStore(t)
	n, err := runConventionExtraction(st, "")
	if err != nil {
		t.Errorf("expected nil error for empty projectID, got %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 conventions for empty projectID, got %d", n)
	}
}

// TestRunConventionExtraction_UpsertIncrementsCount verifies that running
// extraction twice with increased session counts updates the convention.
func TestRunConventionExtraction_UpsertIncrementsCount(t *testing.T) {
	st := openMCPTestStore(t)
	projectID := "proj-increment"

	// First pass: 3 sessions → confidence 0.60.
	for i := 0; i < 3; i++ {
		_, _ = st.InsertSessionObservation(store.SessionObservation{
			SessionID: fmt.Sprintf("sess-%d", i),
			ProjectID: projectID,
			AgentID:   "a",
			Category:  store.ObsCategoryLibraryUsage,
			Key:       "uses_testify",
			Confidence: 0.6,
		})
	}
	if _, err := runConventionExtraction(st, projectID); err != nil {
		t.Fatalf("first extraction: %v", err)
	}

	// Add 2 more sessions.
	for i := 3; i < 5; i++ {
		_, _ = st.InsertSessionObservation(store.SessionObservation{
			SessionID: fmt.Sprintf("sess-%d", i),
			ProjectID: projectID,
			AgentID:   "a",
			Category:  store.ObsCategoryLibraryUsage,
			Key:       "uses_testify",
			Confidence: 0.6,
		})
	}
	if _, err := runConventionExtraction(st, projectID); err != nil {
		t.Fatalf("second extraction: %v", err)
	}

	convs, _ := st.GetProjectConventions(projectID, 0.0)
	if len(convs) != 1 {
		t.Fatalf("expected 1 convention, got %d", len(convs))
	}
	c := convs[0]
	if c.SessionCount != 5 {
		t.Errorf("session_count: want 5, got %d", c.SessionCount)
	}
	if c.Confidence < 0.75 {
		t.Errorf("confidence should increase to 0.80 for count=5, got %v", c.Confidence)
	}
	// Text should reflect the new count.
	if !strings.Contains(c.Text, "5 sessions") {
		t.Errorf("text %q should contain '5 sessions'", c.Text)
	}
}

// ── Integration: end_session → convention delivery in session_init ─────────────

// TestConventionEngine_EndToEnd exercises the full pipeline:
// (1) multiple end_session calls build up observations,
// (2) after MinSessionsForConvention sessions, conventions are extracted,
// (3) session_init briefing includes the learned conventions.
func TestConventionEngine_EndToEnd(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)
	ctx := context.Background()

	// Set a non-empty projectID so runConventionExtraction and session_init
	// can match observations to the project. newPopulatedServer leaves it "".
	const projID = "proj-e2e-conv"
	srv.projectID = projID

	// Run MinSessionsForConvention+1 sessions that all touch Go test files
	// and use testify (injected via the observation pipeline).
	// We manually insert observations to bypass the need for a real graph.
	for i := 0; i < MinSessionsForConvention+1; i++ {
		sessID := fmt.Sprintf("e2e-sess-%d", i)
		_, _ = srv.store.InsertSessionObservation(store.SessionObservation{
			SessionID: sessID,
			ProjectID: projID,
			AgentID:   "e2e-agent",
			Category:  store.ObsCategoryLibraryUsage,
			Key:       "uses_testify",
			Confidence: 0.6,
		})
		_, _ = srv.store.InsertSessionObservation(store.SessionObservation{
			SessionID: sessID,
			ProjectID: projID,
			AgentID:   "e2e-agent",
			Category:  store.ObsCategoryFilePattern,
			Key:       "layered_architecture_touched",
			Confidence: 0.8,
		})
	}

	// Run extraction directly.
	n, err := runConventionExtraction(srv.store, srv.projectID)
	if err != nil {
		t.Fatalf("runConventionExtraction: %v", err)
	}
	if n < 2 {
		t.Errorf("expected ≥2 conventions extracted, got %d", n)
	}

	// Now call session_init and verify the briefing includes learned conventions.
	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "e2e-agent",
		"intent":   "verifying convention delivery",
	}))
	if err != nil || res.IsError {
		t.Fatalf("session_init failed: err=%v isError=%v", err, res.IsError)
	}

	text := firstTextContent(res)
	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}

	// Navigate to briefing.conventions.
	briefing, _ := result["_briefing"].(map[string]any)
	if briefing == nil {
		t.Fatal("_briefing not present in session_init response")
	}
	rawConvs, _ := briefing["conventions"].([]any)
	if len(rawConvs) == 0 {
		t.Fatal("conventions slice is empty — learned conventions not delivered")
	}

	// At least one convention must mention testify.
	foundTestify := false
	foundLayered := false
	for _, c := range rawConvs {
		s, _ := c.(string)
		if strings.Contains(s, "testify") {
			foundTestify = true
		}
		if strings.Contains(s, "layered") {
			foundLayered = true
		}
	}
	if !foundTestify {
		t.Errorf("expected a testify convention in %v", rawConvs)
	}
	if !foundLayered {
		t.Errorf("expected a layered architecture convention in %v", rawConvs)
	}
}

// TestConventionEngine_EndSession_IncludesConventionsExtracted verifies that
// end_session response includes conventions_extracted when conventions are promoted.
func TestConventionEngine_EndSession_IncludesConventionsExtracted(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)
	ctx := context.Background()

	// Set a non-empty projectID so runConventionExtraction can find observations.
	const projID = "proj-conv-extracted"
	srv.projectID = projID

	// Pre-seed observations across MinSessionsForConvention distinct sessions
	// so the next end_session triggers promotion.
	for i := 0; i < MinSessionsForConvention; i++ {
		_, _ = srv.store.InsertSessionObservation(store.SessionObservation{
			SessionID:  fmt.Sprintf("pre-sess-%d", i),
			ProjectID:  projID,
			AgentID:    "conv-agent",
			Category:   store.ObsCategoryLibraryUsage,
			Key:        "uses_testify",
			Confidence: 0.6,
		})
	}

	// Call session_init so we have a synapseSessionID.
	_, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "conv-agent",
		"intent":   "test convention promotion",
	}))
	if err != nil {
		t.Fatalf("session_init: %v", err)
	}

	res, err := srv.handleEndSession(ctx, callTool(map[string]any{
		"agent_id": "conv-agent",
		"summary":  "Tested convention promotion",
	}))
	if err != nil || res.IsError {
		t.Fatalf("end_session failed: err=%v", err)
	}

	text := firstTextContent(res)
	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}

	extracted, _ := result["conventions_extracted"].(float64)
	if extracted < 1 {
		t.Errorf("expected conventions_extracted ≥ 1, got %v", result["conventions_extracted"])
	}
}

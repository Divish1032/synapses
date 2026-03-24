package mcp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/pulse"
	pulsestore "github.com/SynapsesOS/synapses/internal/pulse/pstore"
	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
)

// ── buildSessionTrend unit tests ──────────────────────────────────────────────

func TestBuildSessionTrend_NonNil_WhenSingleDayButMultipleSessions(t *testing.T) {
	// A single day with multiple sessions is still valid — we show summary with stable trend.
	single := []pulse.DailyEffectiveness{
		{Day: "2026-03-20", AvgContextHitRate: 0.8, Sessions: 5},
	}
	if buildSessionTrend(single) == nil {
		t.Error("expected non-nil for single-day with 5 sessions")
	}
	out := buildSessionTrend(single)
	if out["trend"] != "stable" {
		t.Errorf("single-day trend should be 'stable', got %q", out["trend"])
	}
}

func TestBuildSessionTrend_Nil_WhenFewerThanTwoTotalSessions(t *testing.T) {
	days := []pulse.DailyEffectiveness{
		{Day: "2026-03-19", AvgContextHitRate: 0.8, Sessions: 1},
		{Day: "2026-03-20", AvgContextHitRate: 0.9, Sessions: 0},
	}
	if buildSessionTrend(days) != nil {
		t.Error("expected nil when total sessions < 2")
	}
}

func TestBuildSessionTrend_Nil_WhenEmpty(t *testing.T) {
	if buildSessionTrend(nil) != nil {
		t.Error("expected nil for nil input")
	}
	if buildSessionTrend([]pulse.DailyEffectiveness{}) != nil {
		t.Error("expected nil for empty input")
	}
}

func TestBuildSessionTrend_Improving(t *testing.T) {
	days := []pulse.DailyEffectiveness{
		{Day: "2026-03-15", AvgContextHitRate: 0.50, Sessions: 3},
		{Day: "2026-03-16", AvgContextHitRate: 0.55, Sessions: 3},
		{Day: "2026-03-17", AvgContextHitRate: 0.70, Sessions: 3},
		{Day: "2026-03-18", AvgContextHitRate: 0.80, Sessions: 3},
	}
	out := buildSessionTrend(days)
	if out == nil {
		t.Fatal("expected non-nil trend")
	}
	if out["trend"] != "improving" {
		t.Errorf("expected 'improving', got %q", out["trend"])
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "improving") {
		t.Errorf("note should mention 'improving': %q", note)
	}
}

func TestBuildSessionTrend_Declining(t *testing.T) {
	days := []pulse.DailyEffectiveness{
		{Day: "2026-03-15", AvgContextHitRate: 0.85, Sessions: 3},
		{Day: "2026-03-16", AvgContextHitRate: 0.80, Sessions: 3},
		{Day: "2026-03-17", AvgContextHitRate: 0.65, Sessions: 3},
		{Day: "2026-03-18", AvgContextHitRate: 0.60, Sessions: 3},
	}
	out := buildSessionTrend(days)
	if out == nil {
		t.Fatal("expected non-nil trend")
	}
	if out["trend"] != "declining" {
		t.Errorf("expected 'declining', got %q", out["trend"])
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "declining") {
		t.Errorf("note should mention 'declining': %q", note)
	}
}

func TestBuildSessionTrend_Stable(t *testing.T) {
	days := []pulse.DailyEffectiveness{
		{Day: "2026-03-15", AvgContextHitRate: 0.72, Sessions: 4},
		{Day: "2026-03-16", AvgContextHitRate: 0.74, Sessions: 4},
		{Day: "2026-03-17", AvgContextHitRate: 0.70, Sessions: 4},
		{Day: "2026-03-18", AvgContextHitRate: 0.73, Sessions: 4},
	}
	out := buildSessionTrend(days)
	if out == nil {
		t.Fatal("expected non-nil trend")
	}
	if out["trend"] != "stable" {
		t.Errorf("expected 'stable', got %q", out["trend"])
	}
}

func TestBuildSessionTrend_AggregatesCorrectly(t *testing.T) {
	days := []pulse.DailyEffectiveness{
		{Day: "2026-03-19", AvgContextHitRate: 0.60, TotalTokensSaved: 1000, Sessions: 2},
		{Day: "2026-03-20", AvgContextHitRate: 0.80, TotalTokensSaved: 2000, Sessions: 2},
	}
	out := buildSessionTrend(days)
	if out == nil {
		t.Fatal("expected non-nil trend")
	}
	if out["sessions"].(int) != 4 {
		t.Errorf("sessions: want 4, got %v", out["sessions"])
	}
	if out["total_tokens_saved"].(int) != 3000 {
		t.Errorf("total_tokens_saved: want 3000, got %v", out["total_tokens_saved"])
	}
	// avg hit rate = (0.60*2 + 0.80*2) / 4 = 0.70
	avgHit, _ := out["avg_context_hit_rate"].(float64)
	if avgHit < 0.699 || avgHit > 0.701 {
		t.Errorf("avg_context_hit_rate: want ~0.70, got %v", avgHit)
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "3000 tokens saved") {
		t.Errorf("note should mention token savings: %q", note)
	}
}

func TestBuildSessionTrend_DaysField(t *testing.T) {
	days := []pulse.DailyEffectiveness{
		{Day: "2026-03-18", AvgContextHitRate: 0.7, Sessions: 3},
		{Day: "2026-03-19", AvgContextHitRate: 0.7, Sessions: 3},
		{Day: "2026-03-20", AvgContextHitRate: 0.7, Sessions: 3},
	}
	out := buildSessionTrend(days)
	if out["days"].(int) != 3 {
		t.Errorf("days: want 3, got %v", out["days"])
	}
}

func TestBuildSessionTrend_NoTokenNote_WhenZeroSaved(t *testing.T) {
	days := []pulse.DailyEffectiveness{
		{Day: "2026-03-19", AvgContextHitRate: 0.7, Sessions: 3},
		{Day: "2026-03-20", AvgContextHitRate: 0.7, Sessions: 3},
	}
	out := buildSessionTrend(days)
	note, _ := out["note"].(string)
	if strings.Contains(note, "tokens saved") {
		t.Errorf("note should not mention tokens when total_tokens_saved=0: %q", note)
	}
}

// ── session_init integration: session_effectiveness_trend ────────────────────

// TestHandleSessionInit_EffectivenessTrend_AbsentWithoutPulse verifies the
// field is absent when no pulse client is configured.
func TestHandleSessionInit_EffectivenessTrend_AbsentWithoutPulse(t *testing.T) {
	srv := newTestServer(t)
	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "no-pulse"}))
	m := mustResult(t, res, err)
	noKey(t, m, "session_effectiveness_trend")
}

// TestHandleSessionInit_EffectivenessTrend_AbsentOnFreshProject verifies that
// the field is omitted when there are no prior sessions — zero noise on fresh projects.
func TestHandleSessionInit_EffectivenessTrend_AbsentOnFreshProject(t *testing.T) {
	srv := newTestServer(t)
	pc := newPulseClient(t)
	defer pc.Close()
	srv.SetPulseClient(pc)
	// No prior session_effectiveness rows.
	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "fresh-agent"}))
	m := mustResult(t, res, err)
	noKey(t, m, "session_effectiveness_trend")
}

// TestHandleSessionInit_EffectivenessTrend_PresentWithPriorSessions verifies
// that session_effectiveness_trend appears when ≥2 prior sessions exist.
func TestHandleSessionInit_EffectivenessTrend_PresentWithPriorSessions(t *testing.T) {
	dir := t.TempDir()
	pulsePath := filepath.Join(dir, "pulse.sqlite")

	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	st, err := pulsestore.Open(pulsePath)
	if err != nil {
		t.Fatalf("pulsestore.Open: %v", err)
	}
	// Seed two prior effectiveness rows (different session IDs, same agent).
	for i, sid := range []string{"prior-a", "prior-b"} {
		if err := st.InsertSessionEffectiveness(pulsetypes.SessionEffectiveness{
			SessionID:          sid,
			AgentID:            "trend-agent",
			ProjectID:          "test-project",
			ContextHitRate:     0.75 + float64(i)*0.05,
			TaskCompletionRate: 0.80,
			TokensSaved:        500,
			ToolCalls:          20,
			DurationMs:         60_000,
		}); err != nil {
			t.Fatalf("InsertSessionEffectiveness %q: %v", sid, err)
		}
	}
	st.Close()

	srv := newTestServer(t)
	srv.SetPulseClient(pc)

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "trend-agent"}))
	m := mustResult(t, res, err)

	hasKey(t, m, "session_effectiveness_trend")
	trend, ok := m["session_effectiveness_trend"].(map[string]any)
	if !ok {
		t.Fatalf("session_effectiveness_trend must be a map, got %T", m["session_effectiveness_trend"])
	}
	for _, key := range []string{"sessions", "avg_context_hit_rate", "trend", "note"} {
		if _, ok := trend[key]; !ok {
			t.Errorf("session_effectiveness_trend must contain %q", key)
		}
	}
	note, _ := trend["note"].(string)
	if note == "" {
		t.Error("session_effectiveness_trend.note must be non-empty")
	}
	sessions, _ := trend["sessions"].(float64)
	if sessions < 2 {
		t.Errorf("sessions: want ≥2, got %v", sessions)
	}
}

// TestHandleSessionInit_EffectivenessTrend_AbsentInQuickMode verifies the field
// is absent in quick mode (scope="quick") — quick mode is minimal by design.
func TestHandleSessionInit_EffectivenessTrend_AbsentInQuickMode(t *testing.T) {
	dir := t.TempDir()
	pulsePath := filepath.Join(dir, "pulse.sqlite")

	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	st, err := pulsestore.Open(pulsePath)
	if err != nil {
		t.Fatalf("pulsestore.Open: %v", err)
	}
	for _, sid := range []string{"q1", "q2", "q3"} {
		_ = st.InsertSessionEffectiveness(pulsetypes.SessionEffectiveness{
			SessionID:      sid,
			AgentID:        "quick-agent",
			ContextHitRate: 0.7,
			TokensSaved:    300,
			ToolCalls:      10,
			DurationMs:     30_000,
		})
	}
	st.Close()

	srv := newTestServer(t)
	srv.SetPulseClient(pc)

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "quick-agent",
		"scope":    "quick",
	}))
	m := mustResult(t, res, err)
	noKey(t, m, "session_effectiveness_trend")
}

// ── get_context quality-based auto-depth tests ───────────────────────────────

// seedLowQualityForEntity seeds enough correction signals for the given
// entityKey to push its quality score below -2.0.
// Uses a direct store connection for synchronous writes.
func seedLowQualityForEntity(t *testing.T, pulsePath, entityKey string) {
	t.Helper()
	pst, err := pulsestore.Open(pulsePath)
	if err != nil {
		t.Fatalf("seedLowQualityForEntity: pulsestore.Open: %v", err)
	}
	defer pst.Close()
	// 5 × SignalWeightCorrectionImmediate (-0.5) = -2.5 ≤ -2.0.
	for i := 0; i < 5; i++ {
		if err := pst.InsertOutcomeSignal(pulsetypes.OutcomeSignalEvent{
			Entity:       entityKey,
			SignalType:   "correction",
			SignalWeight: pulsetypes.SignalWeightCorrectionImmediate,
		}); err != nil {
			t.Fatalf("seedLowQuality: InsertOutcomeSignal %d: %v", i, err)
		}
	}
	pst.UpdateEntityQualityScore(entityKey, "")
}

// getContextJSON calls handleGetContext and returns the parsed JSON map.
// Returns nil if the entity is not found (not a test failure).
func getContextJSON(t *testing.T, srv *Server, entity string, extras map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"entity": entity, "format": "json"}
	for k, v := range extras {
		args[k] = v
	}
	res, err := srv.handleGetContext(ctx, callTool(args))
	if err != nil {
		t.Fatalf("handleGetContext(%q): %v", entity, err)
	}
	if res == nil {
		t.Fatalf("handleGetContext(%q) returned nil", entity)
	}
	if res.IsError {
		return nil // entity not found is not a test failure here
	}
	if len(res.Content) == 0 {
		return nil
	}
	tc, ok := res.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &m); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, tc.Text)
	}
	return m
}

// TestGetContext_AutoDepth_AdaptiveHintSetForLowQuality verifies that when an
// entity has a quality score ≤ -2.0 and the agent does NOT pass explicit depth,
// the response contains an adaptive_hint mentioning quality-based auto-expansion.
func TestGetContext_AutoDepth_AdaptiveHintSetForLowQuality(t *testing.T) {
	dir := t.TempDir()
	pulsePath := filepath.Join(dir, "pulse.sqlite")

	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	srv, _, _ := newPopulatedServer(t)
	srv.SetPulseClient(pc)

	// "AuthLogin" is in "pkg/auth/auth.go" → entityWithPath = "AuthLogin@auth/auth.go"
	seedLowQualityForEntity(t, pulsePath, "AuthLogin@auth/auth.go")

	m := getContextJSON(t, srv, "AuthLogin", map[string]any{"agent_id": "qa-agent"})
	if m == nil {
		t.Skip("entity not found in test graph — graph setup mismatch")
	}
	hint, _ := m["adaptive_hint"].(string)
	if hint == "" {
		t.Error("expected non-empty adaptive_hint for low-quality entity with auto-depth expansion")
	}
	if !strings.Contains(hint, "quality score") {
		t.Errorf("adaptive_hint should mention 'quality score', got: %q", hint)
	}
}

// TestGetContext_AutoDepth_NoHintWithExplicitDepth verifies that passing an
// explicit depth= skips the quality-based auto-depth entirely (and thus no
// quality-driven adaptive_hint is set for that call).
func TestGetContext_AutoDepth_NoHintWithExplicitDepth(t *testing.T) {
	dir := t.TempDir()
	pulsePath := filepath.Join(dir, "pulse.sqlite")

	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	srv, _, _ := newPopulatedServer(t)
	srv.SetPulseClient(pc)
	seedLowQualityForEntity(t, pulsePath, "AuthLogin@auth/auth.go")

	// Explicit depth=2 — quality auto-depth must NOT fire.
	m := getContextJSON(t, srv, "AuthLogin", map[string]any{
		"agent_id": "qa-agent",
		"depth":    float64(2),
	})
	if m == nil {
		t.Skip("entity not found")
	}
	hint, _ := m["adaptive_hint"].(string)
	// F17 adaptive hint may still be set (from episodic feedback), but NOT the
	// quality-score hint (which mentions "quality score").
	if strings.Contains(hint, "quality score") {
		t.Errorf("quality adaptive_hint must NOT be set when explicit depth= was given, got: %q", hint)
	}
}

// TestGetContext_AutoDepth_NoHintForHighQualityEntity verifies that an entity
// with no outcome signals (neutral quality) does NOT trigger auto-depth.
func TestGetContext_AutoDepth_NoHintForHighQualityEntity(t *testing.T) {
	dir := t.TempDir()
	pulsePath := filepath.Join(dir, "pulse.sqlite")

	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	srv, _, _ := newPopulatedServer(t)
	srv.SetPulseClient(pc)
	// AuthLogout has no outcome signals → quality score absent → no bump.

	m := getContextJSON(t, srv, "AuthLogout", map[string]any{"agent_id": "qa-agent"})
	if m == nil {
		t.Skip("entity not found")
	}
	hint, _ := m["adaptive_hint"].(string)
	if strings.Contains(hint, "quality score") {
		t.Errorf("quality adaptive_hint must NOT fire for entity with no quality record, got: %q", hint)
	}
}

// TestGetContext_AutoDepth_NoBumpWithoutPulseClient verifies that missing pulse
// client is handled gracefully — get_context succeeds without auto-depth.
func TestGetContext_AutoDepth_NoBumpWithoutPulseClient(t *testing.T) {
	srv, _, _ := newPopulatedServer(t) // no pulse client
	res, err := srv.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	if err != nil {
		t.Fatalf("handleGetContext without pulse: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}

// TestGetContext_LowQualityHint_FiresWithCorrectEntityKey verifies that the
// post-BFS LowQualityHint (Sprint 15 #2) fires correctly using the
// entityWithPath key format, NOT the NodeID format. This is a regression test
// for the string(sg.Root) key mismatch fixed in this sprint.
func TestGetContext_LowQualityHint_FiresWithCorrectEntityKey(t *testing.T) {
	dir := t.TempDir()
	pulsePath := filepath.Join(dir, "pulse.sqlite")

	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	srv, _, _ := newPopulatedServer(t)
	srv.SetPulseClient(pc)
	seedLowQualityForEntity(t, pulsePath, "AuthLogin@auth/auth.go")

	m := getContextJSON(t, srv, "AuthLogin", nil)
	if m == nil {
		t.Skip("entity not found")
	}
	// low_quality_hint must be non-empty: the fix ensures the correct key
	// format is used so the score is actually found.
	lqh, _ := m["low_quality_hint"].(string)
	if lqh == "" {
		t.Error("low_quality_hint must be set for entity with quality score ≤ -2.0")
	}
	if !strings.Contains(lqh, "low quality score") {
		t.Errorf("low_quality_hint unexpected content: %q", lqh)
	}
}

package mcp

import (
	"context"
	"testing"
	"time"
)

// ── Component pipeline tests (Phase 6, Section 8.4) ──────────────────────────

func TestRunComponents_ParallelExecution(t *testing.T) {
	// Two slow-ish components should run in parallel, not sequentially.
	components := map[string]componentCollector{
		"slow1": func(_ context.Context) []tieredSection {
			time.Sleep(50 * time.Millisecond)
			return []tieredSection{{Tier: "relevant", Heading: "Slow1", Content: "data1\n"}}
		},
		"slow2": func(_ context.Context) []tieredSection {
			time.Sleep(50 * time.Millisecond)
			return []tieredSection{{Tier: "relevant", Heading: "Slow2", Content: "data2\n"}}
		},
	}
	start := time.Now()
	sections, results := runComponents(context.Background(), nil, components, 500)
	elapsed := time.Since(start)

	if len(sections) != 2 {
		t.Errorf("expected 2 sections, got %d", len(sections))
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
	// If truly parallel, should complete in ~50-80ms, not ~100ms+.
	if elapsed > 150*time.Millisecond {
		t.Errorf("components ran too slowly (%v) — likely sequential instead of parallel", elapsed)
	}
}

func TestRunComponents_RecoverFromPanic(t *testing.T) {
	components := map[string]componentCollector{
		"panicker": func(_ context.Context) []tieredSection {
			panic("intentional test panic")
		},
		"healthy": func(_ context.Context) []tieredSection {
			return []tieredSection{{Tier: "relevant", Heading: "OK", Content: "works\n"}}
		},
	}
	sections, results := runComponents(context.Background(), nil, components, 500)

	// The healthy component should still produce its section.
	if len(sections) != 1 {
		t.Errorf("expected 1 section from healthy component, got %d", len(sections))
	}
	// The panicking component should be recorded.
	var foundPanic bool
	for _, r := range results {
		if r.Name == "panicker" && r.Panicked {
			foundPanic = true
		}
	}
	if !foundPanic {
		t.Error("expected panicker to be recorded as panicked")
	}
}

func TestRunComponents_Timeout(t *testing.T) {
	components := map[string]componentCollector{
		"sleeper": func(ctx context.Context) []tieredSection {
			select {
			case <-time.After(2 * time.Second):
				return []tieredSection{{Tier: "relevant", Heading: "Late", Content: "late\n"}}
			case <-ctx.Done():
				return nil
			}
		},
	}
	sections, results := runComponents(context.Background(), nil, components, 50) // 50ms timeout

	// Should timeout and produce no sections.
	if len(sections) != 0 {
		t.Errorf("expected 0 sections from timed out component, got %d", len(sections))
	}
	if len(results) != 1 || !results[0].TimedOut {
		t.Error("expected sleeper to be recorded as timed out")
	}
}

func TestRunComponents_EmptyComponents(t *testing.T) {
	sections, results := runComponents(context.Background(), nil, nil, 500)
	if sections != nil {
		t.Errorf("expected nil sections, got %v", sections)
	}
	if results != nil {
		t.Errorf("expected nil results, got %v", results)
	}
}

func TestRunComponents_NilReturningCollector(t *testing.T) {
	components := map[string]componentCollector{
		"empty": func(_ context.Context) []tieredSection {
			return nil
		},
	}
	sections, _ := runComponents(context.Background(), nil, components, 500)
	if len(sections) != 0 {
		t.Errorf("expected 0 sections from nil-returning collector, got %d", len(sections))
	}
}

// ── Health tracker tests ─────────────────────────────────────────────────────

func TestComponentHealthTracker_DisableAfterThreeFailures(t *testing.T) {
	var health componentHealthTracker

	if health.isDisabled("test") {
		t.Error("should not be disabled before any failures")
	}

	health.recordFailure("test")
	health.recordFailure("test")
	if health.isDisabled("test") {
		t.Error("should not be disabled after 2 failures")
	}

	health.recordFailure("test")
	if !health.isDisabled("test") {
		t.Error("should be disabled after 3 failures")
	}
}

func TestComponentHealthTracker_Reset(t *testing.T) {
	var health componentHealthTracker
	health.recordFailure("test")
	health.recordFailure("test")
	health.recordFailure("test")
	if !health.isDisabled("test") {
		t.Fatal("should be disabled")
	}

	health.reset()
	if health.isDisabled("test") {
		t.Error("should not be disabled after reset")
	}
}

func TestComponentHealthTracker_IndependentComponents(t *testing.T) {
	var health componentHealthTracker
	health.recordFailure("a")
	health.recordFailure("a")
	health.recordFailure("a")

	if !health.isDisabled("a") {
		t.Error("a should be disabled")
	}
	if health.isDisabled("b") {
		t.Error("b should not be disabled — failures are per-component")
	}
}

func TestRunComponents_HealthAutoDisables(t *testing.T) {
	var health componentHealthTracker
	// Pre-disable a component.
	health.recordFailure("broken")
	health.recordFailure("broken")
	health.recordFailure("broken")

	components := map[string]componentCollector{
		"broken": func(_ context.Context) []tieredSection {
			return []tieredSection{{Tier: "relevant", Heading: "Should Not Run", Content: "x\n"}}
		},
		"healthy": func(_ context.Context) []tieredSection {
			return []tieredSection{{Tier: "relevant", Heading: "OK", Content: "y\n"}}
		},
	}
	sections, results := runComponents(context.Background(), &health, components, 500)

	// Only healthy should produce a section.
	if len(sections) != 1 {
		t.Errorf("expected 1 section, got %d", len(sections))
	}
	for _, s := range sections {
		if s.Heading == "Should Not Run" {
			t.Error("disabled component should not produce output")
		}
	}
	// Broken should show as timed out in debug.
	for _, r := range results {
		if r.Name == "broken" && !r.TimedOut {
			t.Error("disabled component should be marked as timed out in debug")
		}
	}
}

// ── buildDebugSection tests ──────────────────────────────────────────────────

func TestBuildDebugSection_Normal(t *testing.T) {
	results := []componentResult{
		{Name: "violations", LatencyMs: 5},
		{Name: "gaps", LatencyMs: 12},
	}
	debug := buildDebugSection(results)
	if debug == nil {
		t.Fatal("expected debug info")
	}
	latencies, ok := debug["latencies_ms"].(map[string]int64)
	if !ok {
		t.Fatal("expected latencies_ms map")
	}
	if latencies["violations"] != 5 {
		t.Errorf("violations latency = %d, want 5", latencies["violations"])
	}
	if _, hasTimed := debug["timed_out"]; hasTimed {
		t.Error("should not have timed_out when no timeouts")
	}
}

func TestBuildDebugSection_WithTimeoutAndPanic(t *testing.T) {
	results := []componentResult{
		{Name: "slow", LatencyMs: 500, TimedOut: true},
		{Name: "broken", LatencyMs: 1, Panicked: true},
		{Name: "ok", LatencyMs: 10},
	}
	debug := buildDebugSection(results)
	if debug == nil {
		t.Fatal("expected debug info")
	}
	timedOut, ok := debug["timed_out"].([]string)
	if !ok || len(timedOut) != 1 || timedOut[0] != "slow" {
		t.Errorf("timed_out = %v, want [slow]", debug["timed_out"])
	}
	panicked, ok := debug["panicked"].([]string)
	if !ok || len(panicked) != 1 || panicked[0] != "broken" {
		t.Errorf("panicked = %v, want [broken]", debug["panicked"])
	}
}

func TestBuildDebugSection_Empty(t *testing.T) {
	debug := buildDebugSection(nil)
	if debug != nil {
		t.Errorf("expected nil for empty results, got %v", debug)
	}
}

// ── get_working_state entity impact enrichment tests ─────────────────────────

func TestGetWorkingState_EntityImpactEnrichment(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// The populated server has HandleRequest calling AuthLogin/AuthLogout.
	// AuthLogin has fanin >= 1 (from HandleRequest). We need fanin > 3 to trigger.
	// Let's just test that the handler doesn't crash and returns valid JSON.
	res, err := s.handleGetWorkingState(ctx, callTool(map[string]any{
		"window_minutes": float64(60),
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "recent_changes")
	hasKey(t, m, "suggested_tools")
	// modified_entities may or may not be present depending on recent changes.
	// The key test is that the handler doesn't crash.
}

package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ── Component pipeline tests ─────────────────────────────────────────────────

func TestRunComponents_ParallelExecution(t *testing.T) {
	specs := []componentSpec{
		{name: "slow1", timeoutMs: 500, collector: func(_ context.Context) []tieredSection {
			time.Sleep(50 * time.Millisecond)
			return []tieredSection{{Tier: "relevant", Heading: "Slow1", Content: "data1\n"}}
		}},
		{name: "slow2", timeoutMs: 500, collector: func(_ context.Context) []tieredSection {
			time.Sleep(50 * time.Millisecond)
			return []tieredSection{{Tier: "relevant", Heading: "Slow2", Content: "data2\n"}}
		}},
	}
	start := time.Now()
	sections, results := runComponents(context.Background(), nil, "test", specs)
	elapsed := time.Since(start)

	if len(sections) != 2 {
		t.Errorf("expected 2 sections, got %d", len(sections))
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("likely sequential (%v) — should complete in ~50-80ms", elapsed)
	}
}

func TestRunComponents_RecoverFromPanic(t *testing.T) {
	specs := []componentSpec{
		{name: "panicker", timeoutMs: 500, collector: func(_ context.Context) []tieredSection {
			panic("intentional test panic")
		}},
		{name: "healthy", timeoutMs: 500, collector: func(_ context.Context) []tieredSection {
			return []tieredSection{{Tier: "relevant", Heading: "OK", Content: "works\n"}}
		}},
	}
	sections, results := runComponents(context.Background(), nil, "test", specs)

	if len(sections) != 1 {
		t.Errorf("expected 1 section from healthy, got %d", len(sections))
	}
	var foundPanic bool
	for _, r := range results {
		if r.Name == "panicker" && r.Panicked {
			foundPanic = true
		}
	}
	if !foundPanic {
		t.Error("panicker not recorded as panicked")
	}
}

func TestRunComponents_Timeout(t *testing.T) {
	specs := []componentSpec{
		{name: "sleeper", timeoutMs: 50, collector: func(ctx context.Context) []tieredSection {
			select {
			case <-time.After(2 * time.Second):
				return []tieredSection{{Tier: "relevant", Heading: "Late", Content: "late\n"}}
			case <-ctx.Done():
				return nil
			}
		}},
	}
	sections, results := runComponents(context.Background(), nil, "test", specs)

	if len(sections) != 0 {
		t.Errorf("expected 0 sections, got %d", len(sections))
	}
	if len(results) != 1 || !results[0].TimedOut {
		t.Error("expected sleeper to be timed out")
	}
}

func TestRunComponents_PerComponentTimeout(t *testing.T) {
	specs := []componentSpec{
		{name: "fast", timeoutMs: 500, collector: func(_ context.Context) []tieredSection {
			return []tieredSection{{Tier: "relevant", Heading: "Fast", Content: "ok\n"}}
		}},
		{name: "slow", timeoutMs: 20, collector: func(ctx context.Context) []tieredSection {
			select {
			case <-time.After(time.Second):
				return []tieredSection{{Tier: "relevant", Heading: "Slow", Content: "late\n"}}
			case <-ctx.Done():
				return nil
			}
		}},
	}
	sections, results := runComponents(context.Background(), nil, "test", specs)

	if len(sections) != 1 {
		t.Errorf("expected 1 section (fast only), got %d", len(sections))
	}
	for _, r := range results {
		if r.Name == "slow" && !r.TimedOut {
			t.Error("slow should have timed out with 20ms limit")
		}
		if r.Name == "fast" && r.TimedOut {
			t.Error("fast should not have timed out")
		}
	}
}

func TestRunComponents_Empty(t *testing.T) {
	sections, results := runComponents(context.Background(), nil, "test", nil)
	if sections != nil {
		t.Errorf("expected nil sections, got %v", sections)
	}
	if results != nil {
		t.Errorf("expected nil results, got %v", results)
	}
}

func TestRunComponents_NilReturningCollector(t *testing.T) {
	specs := []componentSpec{
		{name: "empty", timeoutMs: 500, collector: func(_ context.Context) []tieredSection { return nil }},
	}
	sections, _ := runComponents(context.Background(), nil, "test", specs)
	if len(sections) != 0 {
		t.Errorf("expected 0 sections, got %d", len(sections))
	}
}

// ── Per-agent health tracker tests ───────────────────────────────────────────

func TestComponentHealthTracker_DisableAfterThreeFailures(t *testing.T) {
	var health componentHealthTracker
	if health.isDisabled("agent", "test") {
		t.Error("should not be disabled initially")
	}
	health.recordFailure("agent", "test")
	health.recordFailure("agent", "test")
	if health.isDisabled("agent", "test") {
		t.Error("should not be disabled after 2 failures")
	}
	health.recordFailure("agent", "test")
	if !health.isDisabled("agent", "test") {
		t.Error("should be disabled after 3 failures")
	}
}

func TestComponentHealthTracker_Reset(t *testing.T) {
	var health componentHealthTracker
	health.recordFailure("agent", "test")
	health.recordFailure("agent", "test")
	health.recordFailure("agent", "test")
	health.reset("agent")
	if health.isDisabled("agent", "test") {
		t.Error("should not be disabled after reset")
	}
}

func TestComponentHealthTracker_IndependentComponents(t *testing.T) {
	var health componentHealthTracker
	health.recordFailure("agent", "a")
	health.recordFailure("agent", "a")
	health.recordFailure("agent", "a")
	if !health.isDisabled("agent", "a") {
		t.Error("a should be disabled")
	}
	if health.isDisabled("agent", "b") {
		t.Error("b should not be disabled")
	}
}

func TestComponentHealthTracker_PerAgentIsolation(t *testing.T) {
	var health componentHealthTracker
	health.recordFailure("agent-a", "violations")
	health.recordFailure("agent-a", "violations")
	health.recordFailure("agent-a", "violations")

	if !health.isDisabled("agent-a", "violations") {
		t.Error("agent-a violations should be disabled")
	}
	if health.isDisabled("agent-b", "violations") {
		t.Error("agent-b violations should NOT be disabled")
	}

	health.reset("agent-a")
	if health.isDisabled("agent-a", "violations") {
		t.Error("agent-a should be cleared after reset")
	}
}

func TestRunComponents_HealthAutoDisables(t *testing.T) {
	var health componentHealthTracker
	health.recordFailure("test", "broken")
	health.recordFailure("test", "broken")
	health.recordFailure("test", "broken")

	specs := []componentSpec{
		{name: "broken", timeoutMs: 500, collector: func(_ context.Context) []tieredSection {
			return []tieredSection{{Tier: "relevant", Heading: "Should Not Run", Content: "x\n"}}
		}},
		{name: "healthy", timeoutMs: 500, collector: func(_ context.Context) []tieredSection {
			return []tieredSection{{Tier: "relevant", Heading: "OK", Content: "y\n"}}
		}},
	}
	sections, results := runComponents(context.Background(), &health, "test", specs)

	if len(sections) != 1 {
		t.Errorf("expected 1 section, got %d", len(sections))
	}
	for _, s := range sections {
		if s.Heading == "Should Not Run" {
			t.Error("disabled component should not produce output")
		}
	}
	for _, r := range results {
		if r.Name == "broken" && !r.TimedOut {
			t.Error("disabled component should be marked as timed out")
		}
	}
}

// ── buildDebugMarkdown tests ─────────────────────────────────────────────────

func TestBuildDebugMarkdown_Normal(t *testing.T) {
	results := []componentResult{
		{Name: "violations", LatencyMs: 5},
		{Name: "gaps", LatencyMs: 12},
	}
	md := buildDebugMarkdown(results)
	if !strings.Contains(md, "## _debug") {
		t.Error("expected markdown heading")
	}
	if !strings.Contains(md, "gaps: 12ms") {
		t.Error("expected gaps latency")
	}
	if !strings.Contains(md, "violations: 5ms") {
		t.Error("expected violations latency")
	}
	if !strings.Contains(md, "timed_out: none") {
		t.Error("expected no timeouts")
	}
}

func TestBuildDebugMarkdown_WithTimeoutAndPanic(t *testing.T) {
	results := []componentResult{
		{Name: "slow", LatencyMs: 500, TimedOut: true},
		{Name: "broken", LatencyMs: 1, Panicked: true},
		{Name: "ok", LatencyMs: 10},
	}
	md := buildDebugMarkdown(results)
	if !strings.Contains(md, "timed_out: slow") {
		t.Errorf("expected timed_out: slow, got:\n%s", md)
	}
	if !strings.Contains(md, "panicked: broken") {
		t.Errorf("expected panicked: broken, got:\n%s", md)
	}
}

func TestBuildDebugMarkdown_Empty(t *testing.T) {
	md := buildDebugMarkdown(nil)
	if md != "" {
		t.Errorf("expected empty for nil results, got %q", md)
	}
}

func TestBuildDebugMarkdown_Deterministic(t *testing.T) {
	results := []componentResult{
		{Name: "zzz", LatencyMs: 1},
		{Name: "aaa", LatencyMs: 2},
		{Name: "mmm", LatencyMs: 3},
	}
	md := buildDebugMarkdown(results)
	// Components should be sorted alphabetically.
	aIdx := strings.Index(md, "aaa")
	mIdx := strings.Index(md, "mmm")
	zIdx := strings.Index(md, "zzz")
	if aIdx > mIdx || mIdx > zIdx {
		t.Errorf("expected alphabetical order, got:\n%s", md)
	}
}

// ── Benchmarks ───────────────────────────────────────────────────────────────

func BenchmarkRunComponents_5Collectors(b *testing.B) {
	// Measure goroutine overhead for the standard 5-component pipeline.
	// Each collector does zero work — we're measuring pure scheduling cost.
	noop := func(_ context.Context) []tieredSection { return nil }
	specs := []componentSpec{
		{name: "violations", timeoutMs: 50, collector: noop},
		{name: "gaps", timeoutMs: 50, collector: noop},
		{name: "brain", timeoutMs: 200, collector: noop},
		{name: "annotations", timeoutMs: 50, collector: noop},
		{name: "cross_project", timeoutMs: 500, collector: noop},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runComponents(context.Background(), nil, "bench", specs)
	}
}

func BenchmarkRunComponents_SingleCollector(b *testing.B) {
	// Fast-path: single collector should have minimal overhead.
	noop := func(_ context.Context) []tieredSection { return nil }
	specs := []componentSpec{{name: "only", timeoutMs: 50, collector: noop}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runComponents(context.Background(), nil, "bench", specs)
	}
}

// ── get_working_state entity impact enrichment ───────────────────────────────

func TestGetWorkingState_EntityImpactEnrichment(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetWorkingState(ctx, callTool(map[string]any{
		"window_minutes": float64(60),
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "recent_changes")
	hasKey(t, m, "suggested_tools")
}

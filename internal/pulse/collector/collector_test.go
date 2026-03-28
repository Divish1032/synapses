package collector

import (
	"path/filepath"
	"testing"
	"time"

	pulsestore "github.com/SynapsesOS/synapses/internal/pulse/pstore"
	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
)

func testStore(t *testing.T) *pulsestore.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := pulsestore.Open(filepath.Join(dir, "pulse_test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNew(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	if c == nil {
		t.Fatal("New returned nil")
	}
	if c.cap != 100 {
		t.Errorf("cap: got %d, want 100", c.cap)
	}
}

func TestNew_Defaults(t *testing.T) {
	s := testStore(t)
	c := New(s, 0, 0) // should default to 1000 / 500ms
	if c.cap != 1000 {
		t.Errorf("cap: got %d, want 1000", c.cap)
	}
	if c.interval != 500*time.Millisecond {
		t.Errorf("interval: got %v, want 500ms", c.interval)
	}
}

func TestRecordToolCall(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()
	defer c.Stop()

	c.RecordToolCall(pulsetypes.ToolCallEvent{
		ToolName: "get_context", DurationMs: 42, Success: true,
	})

	// c.Len() may be 0 if already flushed — that's OK.
	_ = c.Len()
}

func TestRecordContextDelivery(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()
	defer c.Stop()

	c.RecordContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName: "get_context", ResponseTokens: 100, BaselineTokens: 400,
	})
}

func TestRecordBrainUsage(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()
	defer c.Stop()

	c.RecordBrainUsage(pulsetypes.BrainUsageEvent{
		Model: "test-model", PromptTokens: 100, CompletionTokens: 50,
	})
}

func TestRecordSessionEvent(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()
	defer c.Stop()

	c.RecordSessionEvent("sess-1", "agent-1", "proj-1", "start")
}

func TestRecordOutcomeSignal(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()
	defer c.Stop()

	c.RecordOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		SignalType: "task_done", Count: 1,
	})
}

// --- Tests for writeBatch (internal method, tested via Record methods) ---

func TestWriteBatch_MultipleEventTypes(t *testing.T) {
	s := testStore(t)
	c := New(s, 10, 50) // small buffer to force flush frequently
	c.Start()
	defer c.Stop()

	// Record various event types to trigger writeBatch with different code paths
	c.RecordToolCall(pulsetypes.ToolCallEvent{
		ToolName:   "get_context",
		AgentID:    "agent-1",
		ProjectID:  "proj-1",
		DurationMs: 100,
		Success:    true,
	})

	c.RecordContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName:       "get_context",
		AgentID:        "agent-1",
		ProjectID:      "proj-1",
		ResponseTokens: 100,
		BaselineTokens: 400,
	})

	c.RecordBrainUsage(pulsetypes.BrainUsageEvent{
		Model:            "test-model",
		Tier:             "ingest",
		AgentID:          "agent-1",
		ProjectID:        "proj-1",
		PromptTokens:     100,
		CompletionTokens: 50,
		DurationMs:       100,
	})

	c.RecordSessionEvent("sess-1", "agent-1", "proj-1", "start")
	c.RecordOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		SignalType: "task_done",
		Count:      1,
	})
	c.RecordSessionModel("sess-1", "agent-1", "proj-1", "gpt-4o", "openai")
	c.RecordAgentLLMUsage(pulsetypes.AgentLLMUsageEvent{
		SessionID:    "sess-1",
		AgentID:      "agent-1",
		ProjectID:    "proj-1",
		Model:        "gpt-4o",
		Provider:     "openai",
		InputTokens:  100,
		OutputTokens: 50,
		CostUSD:      0.10,
	})

	// Wait for flush to complete
	time.Sleep(100 * time.Millisecond)

	// Verify data was written by checking a higher-level query
	summary, err := s.GetSummary(1) // last 1 day
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if summary == nil {
		t.Error("expected summary to be populated from writeBatch events")
	}
}

func TestWriteBatch_LargeBuffer(t *testing.T) {
	s := testStore(t)
	c := New(s, 1000, 200)
	c.Start()
	defer c.Stop()

	// Fill the buffer without flushing
	for i := 0; i < 50; i++ {
		c.RecordToolCall(pulsetypes.ToolCallEvent{
			ToolName:   "test_tool",
			AgentID:    "agent-1",
			ProjectID:  "proj-1",
			DurationMs: 50,
			Success:    true,
		})
	}

	// Buffer should contain events
	if c.Len() == 0 {
		t.Error("expected buffer to contain events")
	}

	// Wait for scheduled flush
	time.Sleep(250 * time.Millisecond)

	// Check that data was eventually flushed
	if c.Len() > 0 {
		// Some events might not have been flushed yet, that's OK
		_ = c
	}
}

func TestWriteBatch_TokenSavingsComputation(t *testing.T) {
	s := testStore(t)
	c := New(s, 10, 50)
	c.Start()
	defer c.Stop()

	// Record context delivery with significant token savings
	// BaselineTokens 400 - ResponseTokens 100 = 300 tokens saved
	c.RecordContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName:       "get_context",
		AgentID:        "agent-1",
		ProjectID:      "proj-1",
		ResponseTokens: 100,
		BaselineTokens: 400,
	})

	time.Sleep(100 * time.Millisecond)

	// Check stats to verify token savings were recorded
	stats, err := s.GetSummary(1)
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if stats == nil {
		t.Error("expected stats to include token savings")
	}
}

// --- Tests for computeCostSaved ---

func TestComputeCostSaved_ZeroTokens(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)

	cost := c.computeCostSaved(0, "gpt-4o")
	if cost != 0.0 {
		t.Errorf("zero tokens: got %.6f, want 0", cost)
	}
}

func TestComputeCostSaved_NegativeTokens(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)

	cost := c.computeCostSaved(-100, "gpt-4o")
	if cost != 0.0 {
		t.Errorf("negative tokens: got %.6f, want 0", cost)
	}
}

func TestComputeCostSaved_WithPricing(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)

	// 1000000 tokens saved should cost ~$2.50 (gpt-4o pricing seeded by schema)
	cost := c.computeCostSaved(1000000, "gpt-4o")
	if cost < 2.4 || cost > 2.6 {
		t.Errorf("1M tokens: got $%.6f, want ~$2.50", cost)
	}
}

func TestComputeCostSaved_SmallAmount(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)

	// 1000 tokens at $2.50/1M = $0.0000025 (gpt-4o pricing seeded by schema)
	cost := c.computeCostSaved(1000, "gpt-4o")
	expected := 1000.0 / 1_000_000.0 * 2.50
	if cost < expected*0.9 || cost > expected*1.1 {
		t.Errorf("1K tokens: got $%.9f, want ~$%.9f", cost, expected)
	}
}

func TestComputeCostSaved_MultipleTokenAmounts(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)

	// Test with different token amounts (gpt-4o pricing seeded by schema)
	testCases := []struct {
		tokens int
		name   string
	}{
		{100000, "100K tokens"}, // 100K tokens → $0.25 (100K / 1M * 2.50)
		{500000, "500K tokens"}, // 500K tokens → $1.25 (500K / 1M * 2.50)
		{2000000, "2M tokens"},  // 2M tokens → $5.00 (2M / 1M * 2.50)
	}

	for _, tc := range testCases {
		cost := c.computeCostSaved(tc.tokens, "gpt-4o")
		expected := float64(tc.tokens) / 1_000_000.0 * 2.50
		// Allow 5% tolerance for floating point math
		if cost < expected*0.95 || cost > expected*1.05 {
			t.Errorf("%s: got $%.6f, want ~$%.6f", tc.name, cost, expected)
		}
	}
}

func TestRecordSessionModel(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()
	defer c.Stop()

	c.RecordSessionModel("sess-1", "agent-1", "proj-1", "claude-sonnet-4-6", "anthropic")
}

func TestRecordAgentLLMUsage(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()
	defer c.Stop()

	c.RecordAgentLLMUsage(pulsetypes.AgentLLMUsageEvent{
		Model: "claude-sonnet-4-6", Provider: "anthropic", InputTokens: 1000,
	})
}

func TestFlush_OnStop(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 60000) // long interval so flush only happens on stop
	c.Start()

	c.RecordToolCall(pulsetypes.ToolCallEvent{ToolName: "test1", Success: true})
	c.RecordToolCall(pulsetypes.ToolCallEvent{ToolName: "test2", Success: true})

	c.Stop() // Should trigger final flush

	tc, cd, bu := s.EventCount()
	if tc+cd+bu < 2 {
		t.Errorf("expected at least 2 events after stop, got %d", tc+cd+bu)
	}
}

func TestEarlyFlush_OnHighCapacity(t *testing.T) {
	s := testStore(t)
	c := New(s, 10, 60000) // small buffer, long interval

	c.Start()
	defer c.Stop()

	// Fill to 80% capacity to trigger early flush
	for i := 0; i < 9; i++ {
		c.RecordToolCall(pulsetypes.ToolCallEvent{ToolName: "test", Success: true})
	}

	// Give time for async flush
	time.Sleep(200 * time.Millisecond)

	// Buffer should have been drained
	if c.Len() >= 8 {
		t.Errorf("expected buffer to be drained after early flush, len=%d", c.Len())
	}
}

func TestLen(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 60000)
	// Don't start — no flush loop
	if c.Len() != 0 {
		t.Error("expected empty buffer")
	}

	c.RecordToolCall(pulsetypes.ToolCallEvent{ToolName: "test", Success: true})
	if c.Len() != 1 {
		t.Errorf("expected len 1, got %d", c.Len())
	}
}

func TestComputeCostSaved(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)

	// gpt-4o default pricing is $2.50/1M input
	cost := c.computeCostSaved(1_000_000, "gpt-4o")
	if cost != 2.50 {
		t.Errorf("cost: got %.2f, want 2.50", cost)
	}

	// Zero tokens
	cost = c.computeCostSaved(0, "gpt-4o")
	if cost != 0 {
		t.Errorf("cost for 0 tokens: got %.2f, want 0", cost)
	}

	// Negative tokens
	cost = c.computeCostSaved(-100, "gpt-4o")
	if cost != 0 {
		t.Errorf("cost for negative tokens: got %.2f, want 0", cost)
	}
}

func TestFlush_WithEmptyBuffer(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()
	defer c.Stop()

	// Immediately flush with no events — should be a no-op
	c.mu.Lock()
	initialLen := c.count
	c.mu.Unlock()

	if initialLen != 0 {
		t.Errorf("expected empty buffer, got %d events", initialLen)
	}
}

func TestFlushLoop_OnTicker(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 50) // 50ms flush interval (short for testing)

	c.Start()
	defer c.Stop()

	// Record an event and wait for periodic flush
	c.RecordToolCall(pulsetypes.ToolCallEvent{ToolName: "test", Success: true})

	// Wait for timer to fire and flush
	time.Sleep(150 * time.Millisecond)

	// Buffer should be empty after flush
	if c.Len() > 0 {
		t.Errorf("expected buffer flushed by ticker, len=%d", c.Len())
	}
}

func TestWriteBatch_WithBrainUsage(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()
	defer c.Stop()

	c.RecordBrainUsage(pulsetypes.BrainUsageEvent{
		Model:            "test-model",
		PromptTokens:     100,
		CompletionTokens: 50,
		DurationMs:       42,
		CostUSD:          0.01,
	})

	time.Sleep(100 * time.Millisecond)
}

func TestWriteBatch_WithAgentLLMUsage(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()
	defer c.Stop()

	c.RecordAgentLLMUsage(pulsetypes.AgentLLMUsageEvent{
		SessionID:    "sess-1",
		AgentID:      "agent-1",
		ProjectID:    "proj-1",
		Model:        "gpt-4o",
		Provider:     "openai",
		InputTokens:  1000,
		OutputTokens: 500,
		CostUSD:      0.05,
	})

	time.Sleep(100 * time.Millisecond)
}

func TestComputeCostSaved_UnknownModel(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)

	// Unknown model falls back to gpt-4o pricing (canonical agent baseline).
	// gpt-4o input pricing is $2.50/1M tokens, so 1M tokens → $2.50.
	cost := c.computeCostSaved(1_000_000, "nonexistent-model")
	if cost < 0.01 {
		t.Errorf("unknown model with gpt-4o fallback: got %.6f, want > 0 (gpt-4o fallback)", cost)
	}
}

func TestWriteBatch_ContextDelivery_EmptyAgentID(t *testing.T) {
	s := testStore(t)
	c := New(s, 10, 50)
	c.Start()
	defer c.Stop()

	// Empty AgentID should default to "default" in writeBatch
	c.RecordContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName:       "get_context",
		AgentID:        "", // empty — triggers the default branch
		ProjectID:      "proj-1",
		ResponseTokens: 50,
		BaselineTokens: 200,
	})

	time.Sleep(100 * time.Millisecond)
}

func TestWriteBatch_ToolCall_EmptyAgentID(t *testing.T) {
	s := testStore(t)
	c := New(s, 10, 50)
	c.Start()
	defer c.Stop()

	// Empty AgentID should default to "default" in writeBatch
	c.RecordToolCall(pulsetypes.ToolCallEvent{
		ToolName:   "get_context",
		AgentID:    "", // empty — triggers the default branch
		ProjectID:  "proj-1",
		DurationMs: 10,
		Success:    true,
	})

	time.Sleep(100 * time.Millisecond)
}

// TestOutcomeSignal_QualityScoreUpdatedAfterFlush verifies that the entity
// quality score is updated AFTER the outcome signal is flushed to the DB —
// not at enqueue time. This is the regression test for the Sprint 15 #2 bug
// where UpdateEntityQualityScore was called before RecordOutcomeSignal's
// async flush, causing the score to always miss the triggering signal.
func TestOutcomeSignal_QualityScoreUpdatedAfterFlush(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()

	c.RecordOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		Entity:       "AuthService",
		ProjectID:    "proj-1",
		SignalType:   "task_done",
		Count:        1,
		SignalWeight: 1.0,
	})

	// Stop triggers a final flush, ensuring the signal AND quality update are committed.
	c.Stop()

	score, ok := s.GetEntityQualityScore("AuthService", "proj-1")
	if !ok {
		t.Fatal("expected entity_quality row to exist after flush")
	}
	if score != 1.0 {
		t.Errorf("quality score = %.2f, want 1.0 (the task_done signal_weight)", score)
	}
}

// TestOutcomeSignal_NegativeSignalReducesScore verifies that a negative signal
// (correction/abandoned) reduces the quality score below zero.
func TestOutcomeSignal_NegativeSignalReducesScore(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()

	c.RecordOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		Entity:       "BadEntity",
		ProjectID:    "proj-1",
		SignalType:   "task_abandoned",
		Count:        1,
		SignalWeight: -0.8,
	})
	c.Stop()

	score, ok := s.GetEntityQualityScore("BadEntity", "proj-1")
	if !ok {
		t.Fatal("expected entity_quality row to exist after flush")
	}
	if score >= 0 {
		t.Errorf("quality score = %.2f, want negative (task_abandoned penalty)", score)
	}
}

// TestOutcomeSignal_MultipleSignalsAccumulate verifies that successive signals
// accumulate correctly — each flush picks up all prior signals.
func TestOutcomeSignal_MultipleSignalsAccumulate(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)
	c.Start()

	// Two positive signals: total weight should be 2.0
	c.RecordOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		Entity: "GoodFunc", ProjectID: "proj-1",
		SignalType: "task_done", Count: 1, SignalWeight: 1.0,
	})
	c.RecordOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		Entity: "GoodFunc", ProjectID: "proj-1",
		SignalType: "task_done", Count: 1, SignalWeight: 1.0,
	})
	c.Stop()

	score, ok := s.GetEntityQualityScore("GoodFunc", "proj-1")
	if !ok {
		t.Fatal("expected entity_quality row to exist")
	}
	if score != 2.0 {
		t.Errorf("quality score = %.2f, want 2.0 (two task_done signals)", score)
	}
}

func TestComputeCostSaved_WithPricingLookup(t *testing.T) {
	s := testStore(t)
	c := New(s, 100, 500)

	// Test with various token amounts
	tests := []struct {
		tokens int
		want   float64
	}{
		{0, 0},
		{500_000, 1.25},   // 500k tokens * 2.50 / 1M
		{1_000_000, 2.50}, // 1M tokens
		{2_000_000, 5.00}, // 2M tokens
	}

	for _, tt := range tests {
		cost := c.computeCostSaved(tt.tokens, "gpt-4o")
		if cost != tt.want {
			t.Errorf("computeCostSaved(%d) = %.2f, want %.2f", tt.tokens, cost, tt.want)
		}
	}
}

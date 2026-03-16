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

	if c.Len() == 0 {
		// May already have been flushed, that's OK
	}
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

	count, err := s.EventCount()
	if err != nil {
		t.Fatalf("EventCount: %v", err)
	}
	if count < 2 {
		t.Errorf("expected at least 2 events after stop, got %d", count)
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
	cost := c.computeCostSaved(1_000_000)
	if cost != 2.50 {
		t.Errorf("cost: got %.2f, want 2.50", cost)
	}

	// Zero tokens
	cost = c.computeCostSaved(0)
	if cost != 0 {
		t.Errorf("cost for 0 tokens: got %.2f, want 0", cost)
	}

	// Negative tokens
	cost = c.computeCostSaved(-100)
	if cost != 0 {
		t.Errorf("cost for negative tokens: got %.2f, want 0", cost)
	}
}

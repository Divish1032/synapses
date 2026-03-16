package aggregator

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
	a := New(s, 3600)
	if a == nil {
		t.Fatal("New returned nil")
	}
	if a.interval != 3600*time.Second {
		t.Errorf("interval: got %v, want 3600s", a.interval)
	}
}

func TestNew_DefaultInterval(t *testing.T) {
	s := testStore(t)
	a := New(s, 0) // should default to 3600
	if a.interval != 3600*time.Second {
		t.Errorf("interval: got %v, want 3600s", a.interval)
	}

	a2 := New(s, -1) // negative should also default
	if a2.interval != 3600*time.Second {
		t.Errorf("interval: got %v, want 3600s", a2.interval)
	}
}

func TestRollupNow_EmptyDB(t *testing.T) {
	s := testStore(t)
	a := New(s, 3600)
	// Should not panic on empty DB
	a.RollupNow()
}

func TestRollupNow_WithData(t *testing.T) {
	s := testStore(t)
	a := New(s, 3600)

	// Seed some events
	_ = s.InsertToolCall(pulsetypes.ToolCallEvent{
		ToolName: "get_context", DurationMs: 100, Success: true,
	})
	_ = s.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName: "get_context", ResponseTokens: 100, BaselineTokens: 400,
	})

	a.RollupNow()
	// Verify rollup was written by checking GetSummary
	sum, err := s.GetSummary(1)
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if sum.TotalToolCalls == 0 {
		t.Error("expected non-zero tool calls after rollup")
	}
}

func TestStartStop(t *testing.T) {
	s := testStore(t)
	a := New(s, 1) // 1 second interval for fast test
	a.Start()
	time.Sleep(100 * time.Millisecond)
	a.Stop()
	// Should not hang
}

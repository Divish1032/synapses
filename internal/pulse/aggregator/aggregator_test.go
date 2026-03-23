package aggregator

import (
	"path/filepath"
	"sync"
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

func TestRollup_EmptyDB(t *testing.T) {
	s := testStore(t)
	a := New(s, 3600)
	// Should not panic on empty DB
	a.rollup()
}

func TestRollup_WithData(t *testing.T) {
	s := testStore(t)
	a := New(s, 3600)

	// Seed some events
	_ = s.InsertToolCall(pulsetypes.ToolCallEvent{
		ToolName: "get_context", DurationMs: 100, Success: true,
	})
	_ = s.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName: "get_context", ResponseTokens: 100, BaselineTokens: 400,
	})

	a.rollup()
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

// TestRollupSnapshotIsolation verifies that rollup() produces internally
// consistent metrics even while a goroutine continuously flushes new events.
// Consistency invariant: tool_calls >= 0, sessions >= 0, no NaN.
func TestRollupSnapshotIsolation(t *testing.T) {
	s := testStore(t)
	a := New(s, 3600)

	// Seed initial baseline data so rollup has something to read.
	for i := 0; i < 5; i++ {
		_ = s.InsertToolCall(pulsetypes.ToolCallEvent{
			ToolName: "search", DurationMs: 50, Success: true,
		})
		_ = s.UpsertSessionWithVersion(
			"sess-init", "agent-snap", "proj-snap", "start", "v1",
		)
	}

	// Writer goroutine: continuously flush new tool calls for >= 100 ms.
	stop := make(chan struct{})
	var writerDone sync.WaitGroup
	writerDone.Add(1)
	go func() {
		defer writerDone.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.InsertToolCall(pulsetypes.ToolCallEvent{
					ToolName:   "concurrent_tool",
					DurationMs: int64(i % 200),
					Success:    i%3 != 0,
				})
				i++
			}
		}
	}()

	// Run several rollup passes while the writer is active.
	deadline := time.Now().Add(150 * time.Millisecond)
	var lastSum *pulsestore.Summary
	for time.Now().Before(deadline) {
		a.rollup()

		sum, err := s.GetSummary(1)
		if err != nil {
			t.Errorf("GetSummary during concurrent writes: %v", err)
			continue
		}
		lastSum = sum

		// Invariant: no negative counts.
		if sum.TotalToolCalls < 0 {
			t.Errorf("TotalToolCalls negative: %d", sum.TotalToolCalls)
		}
		if sum.Sessions < 0 {
			t.Errorf("Sessions negative: %d", sum.Sessions)
		}
		// CacheHitRate must be in [0, 1].
		if sum.CacheHitRate < 0 || sum.CacheHitRate > 1 {
			t.Errorf("CacheHitRate out of range: %f", sum.CacheHitRate)
		}
	}

	// Stop writer, then do one final rollup check.
	close(stop)
	writerDone.Wait()

	a.rollup()
	finalSum, err := s.GetSummary(1)
	if err != nil {
		t.Fatalf("final GetSummary: %v", err)
	}
	if finalSum.TotalToolCalls < 0 {
		t.Errorf("final TotalToolCalls negative: %d", finalSum.TotalToolCalls)
	}
	// After writer stopped, tool calls must be >= last snapshot (monotonic).
	if lastSum != nil && finalSum.TotalToolCalls < lastSum.TotalToolCalls {
		t.Errorf("tool calls decreased: last=%d final=%d", lastSum.TotalToolCalls, finalSum.TotalToolCalls)
	}
}

package collector

import (
	"fmt"
	"testing"
)

// suppressEarlyFlush prevents the early-flush goroutine from firing during
// ring buffer unit tests. It sets the earlyFlushRunning flag so enqueue's
// 80%-capacity check never triggers a background drain.
func suppressEarlyFlush(c *Collector) {
	c.earlyFlushRunning.Store(1)
}

func TestRingBuffer_EnqueueUpToCapacity_DrainPreservesOrder(t *testing.T) {
	s := testStore(t)
	c := New(s, 5, 60000) // cap=5, long interval so no auto-flush
	suppressEarlyFlush(c)

	for i := 0; i < 5; i++ {
		c.enqueue(event{kind: "tool_call", data: fmt.Sprintf("ev-%d", i)})
	}

	if c.Len() != 5 {
		t.Fatalf("expected len 5, got %d", c.Len())
	}

	c.mu.Lock()
	batch := c.drainLocked()
	c.mu.Unlock()

	if len(batch) != 5 {
		t.Fatalf("drain: expected 5 events, got %d", len(batch))
	}
	for i, ev := range batch {
		want := fmt.Sprintf("ev-%d", i)
		if ev.data.(string) != want {
			t.Errorf("batch[%d]: got %v, want %v", i, ev.data, want)
		}
	}

	if c.Len() != 0 {
		t.Errorf("after drain: expected len 0, got %d", c.Len())
	}
	if c.dropped.Load() != 0 {
		t.Errorf("no overflow: dropped should be 0, got %d", c.dropped.Load())
	}
}

func TestRingBuffer_Overflow_DropsOldestAndIncrementsDropped(t *testing.T) {
	s := testStore(t)
	c := New(s, 4, 60000) // cap=4
	suppressEarlyFlush(c)

	// Enqueue 7 events into a buffer of capacity 4.
	// Events 0-2 should be dropped; events 3-6 should remain.
	for i := 0; i < 7; i++ {
		c.enqueue(event{kind: "tool_call", data: fmt.Sprintf("ev-%d", i)})
	}

	if c.Len() != 4 {
		t.Fatalf("expected len 4 (capped), got %d", c.Len())
	}

	if dropped := c.dropped.Load(); dropped != 3 {
		t.Errorf("dropped: got %d, want 3", dropped)
	}

	c.mu.Lock()
	batch := c.drainLocked()
	c.mu.Unlock()

	if len(batch) != 4 {
		t.Fatalf("drain: expected 4 events, got %d", len(batch))
	}
	// The oldest 3 events (ev-0, ev-1, ev-2) were overwritten.
	for i, ev := range batch {
		want := fmt.Sprintf("ev-%d", i+3)
		if ev.data.(string) != want {
			t.Errorf("batch[%d]: got %v, want %v", i, ev.data, want)
		}
	}
}

func TestRingBuffer_Wraparound_PreservesOrder(t *testing.T) {
	s := testStore(t)
	c := New(s, 5, 60000)
	suppressEarlyFlush(c)

	// Phase 1: enqueue 3 events and drain.
	for i := 0; i < 3; i++ {
		c.enqueue(event{kind: "tool_call", data: fmt.Sprintf("a-%d", i)})
	}
	c.mu.Lock()
	batch1 := c.drainLocked()
	c.mu.Unlock()

	if len(batch1) != 3 {
		t.Fatalf("phase 1 drain: expected 3, got %d", len(batch1))
	}
	for i, ev := range batch1 {
		want := fmt.Sprintf("a-%d", i)
		if ev.data.(string) != want {
			t.Errorf("batch1[%d]: got %v, want %v", i, ev.data, want)
		}
	}

	// Phase 2: enqueue 4 more events (causes tail to wrap around the ring).
	for i := 0; i < 4; i++ {
		c.enqueue(event{kind: "tool_call", data: fmt.Sprintf("b-%d", i)})
	}

	if c.Len() != 4 {
		t.Fatalf("phase 2: expected len 4, got %d", c.Len())
	}

	c.mu.Lock()
	batch2 := c.drainLocked()
	c.mu.Unlock()

	if len(batch2) != 4 {
		t.Fatalf("phase 2 drain: expected 4, got %d", len(batch2))
	}
	for i, ev := range batch2 {
		want := fmt.Sprintf("b-%d", i)
		if ev.data.(string) != want {
			t.Errorf("batch2[%d]: got %v, want %v", i, ev.data, want)
		}
	}

	if c.dropped.Load() != 0 {
		t.Errorf("no overflow expected: dropped = %d", c.dropped.Load())
	}
}

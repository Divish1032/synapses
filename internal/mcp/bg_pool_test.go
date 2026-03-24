package mcp

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestGoBackground_ExecutesWork verifies that goBackground enqueues work
// and workers process it before Close() returns.
func TestGoBackground_ExecutesWork(t *testing.T) {
	srv := newTestServer(t)
	srv.StartBackground()

	var counter int64
	for i := 0; i < 50; i++ {
		srv.goBackground(func() {
			atomic.AddInt64(&counter, 1)
		})
	}

	srv.Close()
	got := atomic.LoadInt64(&counter)
	if got != 50 {
		t.Fatalf("expected 50 work items processed, got %d", got)
	}
}

// TestGoBackground_ShutdownDrain verifies that Close() drains all queued
// work before returning.
func TestGoBackground_ShutdownDrain(t *testing.T) {
	srv := newTestServer(t)
	srv.StartBackground()

	var counter int64

	// Enqueue work that takes a moment to process.
	for i := 0; i < 20; i++ {
		srv.goBackground(func() {
			time.Sleep(time.Millisecond)
			atomic.AddInt64(&counter, 1)
		})
	}

	srv.Close()
	got := atomic.LoadInt64(&counter)
	if got != 20 {
		t.Fatalf("Close() returned before all work drained: got %d/20", got)
	}
}

// TestGoBackground_AfterCloseDropped verifies that goBackground silently
// drops work after Close() has been called.
func TestGoBackground_AfterCloseDropped(t *testing.T) {
	srv := newTestServer(t)
	srv.StartBackground()
	srv.Close()

	var counter int64
	srv.goBackground(func() {
		atomic.AddInt64(&counter, 1)
	})

	// Give a moment for any accidental processing.
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt64(&counter) != 0 {
		t.Fatal("goBackground executed work after Close()")
	}
}

// TestGoBackground_PanicContainment verifies that a panicking work item
// does not crash the server and subsequent work items process normally.
func TestGoBackground_PanicContainment(t *testing.T) {
	srv := newTestServer(t)
	srv.StartBackground()

	var counter int64

	// Enqueue a panicking item followed by normal items.
	srv.goBackground(func() {
		panic("test panic — should be contained")
	})
	for i := 0; i < 10; i++ {
		srv.goBackground(func() {
			atomic.AddInt64(&counter, 1)
		})
	}

	srv.Close()
	got := atomic.LoadInt64(&counter)
	if got != 10 {
		t.Fatalf("panic killed worker pool: only %d/10 items processed", got)
	}
}

// TestGoBackground_BackPressure verifies that when the queue is full,
// goBackground drops work instead of blocking.
func TestGoBackground_BackPressure(t *testing.T) {
	// Create a minimal server without using newTestServer (which starts workers).
	// We need workers NOT running so the queue fills up.
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())
	srv := New(g, cfg, nil) // nil store — no memoryExpiryLoop
	t.Cleanup(func() { srv.Close() }) // safety net if test fails before explicit Close() below
	// Deliberately do NOT call StartBackground — no workers consuming.

	for i := 0; i < bgQueueCap+50; i++ {
		srv.goBackground(func() {})
	}

	// Verify the queue is full.
	if len(srv.bgQueue) != bgQueueCap {
		t.Fatalf("expected queue to be full (%d), got %d", bgQueueCap, len(srv.bgQueue))
	}

	// Start workers and close to drain.
	srv.StartBackground()
	srv.Close()
}

// TestGoBackground_ConcurrentEnqueueAndClose verifies no race between
// concurrent goBackground calls and Close().
func TestGoBackground_ConcurrentEnqueueAndClose(t *testing.T) {
	srv := newTestServer(t)
	srv.StartBackground()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			srv.goBackground(func() {
				time.Sleep(100 * time.Microsecond)
			})
		}
	}()

	// Close while enqueueing is in progress.
	time.Sleep(time.Millisecond)
	srv.Close()
	<-done
}

// TestClose_Idempotent verifies that calling Close() multiple times is safe.
func TestClose_Idempotent(t *testing.T) {
	srv := newTestServer(t)
	srv.StartBackground()
	srv.Close()
	srv.Close() // must not panic
	srv.Close() // must not panic
}

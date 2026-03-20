package watcher

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// TestStop_DrainsWaitGroup verifies that Stop() blocks until all goroutines
// tracked by the WaitGroup have completed. We manually increment the wg to
// simulate in-flight work and verify Stop() doesn't return until they finish.
func TestStop_DrainsWaitGroup(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Simulate an in-flight goroutine that takes 200ms to complete.
	var finished atomic.Bool
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		time.Sleep(200 * time.Millisecond)
		finished.Store(true)
	}()

	// Stop must block until the goroutine finishes.
	w.Stop()

	if !finished.Load() {
		t.Error("Stop() returned before in-flight goroutine finished")
	}
}

// TestStop_BrainWriteBackExitsOnStopCh verifies that the brain summary
// write-back goroutine (which has a 15-second sleep) exits promptly when
// stopCh is closed, rather than blocking shutdown for the full 15 seconds.
func TestStop_BrainWriteBackExitsOnStopCh(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Simulate the brain write-back pattern: a goroutine that sleeps a long
	// time but respects stopCh.
	var exited atomic.Bool
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		select {
		case <-time.After(15 * time.Second):
		case <-w.stopCh:
		}
		exited.Store(true)
	}()

	start := time.Now()
	w.Stop()
	elapsed := time.Since(start)

	if !exited.Load() {
		t.Error("goroutine did not exit after Stop()")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Stop() took %v — brain write-back should exit promptly via stopCh", elapsed)
	}
}

// TestStop_MultipleGoroutinesDrain verifies that Stop() waits for multiple
// concurrent goroutines to complete, not just one.
func TestStop_MultipleGoroutinesDrain(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var count atomic.Int32
	const n = 10
	for i := 0; i < n; i++ {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			time.Sleep(100 * time.Millisecond)
			count.Add(1)
		}()
	}

	w.Stop()

	if got := count.Load(); got != n {
		t.Errorf("only %d of %d goroutines completed before Stop() returned", got, n)
	}
}

// TestStop_IdempotentWithWaitGroup verifies that calling Stop() twice does not
// panic or block indefinitely when goroutines are tracked by the WaitGroup.
func TestStop_IdempotentWithWaitGroup(t *testing.T) {
	dir := t.TempDir()
	g := graph.New("test")
	g.SetRoot(dir)

	w, err := New(g, parser.NewWalker(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}

	w.Stop()

	// Second Stop should be a no-op with no panic.
	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second Stop() call blocked for >2s")
	}
}

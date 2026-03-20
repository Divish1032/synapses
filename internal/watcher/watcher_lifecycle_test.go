package watcher

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// TestStop_DrainsWaitGroup verifies that Stop() blocks until all goroutines
// tracked by the WaitGroup have completed.
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

	// Launch a tracked goroutine that takes 200ms to complete.
	var finished atomic.Bool
	w.trackGo(func() {
		time.Sleep(200 * time.Millisecond)
		finished.Store(true)
	})

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
	w.trackGo(func() {
		select {
		case <-time.After(15 * time.Second):
		case <-w.stopCh:
		}
		exited.Store(true)
	})

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
		w.trackGo(func() {
			time.Sleep(100 * time.Millisecond)
			count.Add(1)
		})
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

// TestTrackGo_AfterStop verifies that trackGo returns false and does NOT
// launch the goroutine after Stop() has been called. This prevents the race
// where a debounce timer callback is already executing reparseFile when Stop()
// is called — without this guard, wg.Add could happen after wg.Wait returns.
func TestTrackGo_AfterStop(t *testing.T) {
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

	var ran atomic.Bool
	ok := w.trackGo(func() {
		ran.Store(true)
	})

	if ok {
		t.Error("trackGo returned true after Stop()")
	}
	// Give a moment for any accidentally-launched goroutine to run.
	time.Sleep(50 * time.Millisecond)
	if ran.Load() {
		t.Error("goroutine was launched after Stop()")
	}
}

// TestTrackGo_ConcurrentWithStop exercises the race between trackGo and Stop.
// Multiple goroutines call trackGo while another calls Stop. No goroutine
// should write after Stop returns, and no panic should occur.
func TestTrackGo_ConcurrentWithStop(t *testing.T) {
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

	// Track how many goroutines actually ran and how many were rejected.
	var launched atomic.Int32
	var rejected atomic.Int32
	var afterStop atomic.Int32

	stopDone := make(chan struct{})

	// Spawn 50 goroutines that race to call trackGo.
	const n = 50
	for i := 0; i < n; i++ {
		go func() {
			ok := w.trackGo(func() {
				// If we're running, check whether Stop has already returned.
				select {
				case <-stopDone:
					afterStop.Add(1)
				default:
				}
				launched.Add(1)
			})
			if !ok {
				rejected.Add(1)
			}
		}()
	}

	// Give the goroutines a moment to start racing, then Stop.
	time.Sleep(10 * time.Millisecond)
	w.Stop()
	close(stopDone)

	// Wait for all spawner goroutines to finish.
	time.Sleep(100 * time.Millisecond)

	total := launched.Load() + rejected.Load()
	if total != n {
		t.Errorf("expected %d total (launched+rejected), got %d", n, total)
	}
	if afterStop.Load() > 0 {
		t.Errorf("%d goroutine(s) ran AFTER Stop() returned — lifecycle violation", afterStop.Load())
	}
}

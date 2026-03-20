package embed_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/embed"
)

// TestConcurrentEmbed_MutexSerialization verifies that multiple goroutines
// calling Embed() concurrently are correctly handled by the pipeline pool
// and do not panic or corrupt state. Uses a path that forces init failure
// so the test runs without model download.
func TestConcurrentEmbed_MutexSerialization(t *testing.T) {
	// Use impossible path to avoid actual model download.
	e := embed.NewBuiltinEmbedder("/dev/null/impossible/path")
	defer e.Close()

	const goroutines = 10
	var wg sync.WaitGroup

	results := make([][]float32, goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = e.Embed(context.Background(), "concurrent test text")
		}(i)
	}
	wg.Wait()

	// All calls should fail with the same init error (not panic).
	for i := 0; i < goroutines; i++ {
		if errs[i] == nil {
			t.Errorf("goroutine %d: expected error from failed init, got nil", i)
		}
		if results[i] != nil {
			t.Errorf("goroutine %d: expected nil result, got %d dims", i, len(results[i]))
		}
	}

	// Verify embedder is in a consistent state after concurrent failures.
	if e.IsReady() {
		t.Error("embedder should not be ready after failed init")
	}
	if e.StatusDetail() != "unavailable" {
		t.Errorf("expected status 'unavailable', got %q", e.StatusDetail())
	}
}

// TestConcurrentEmbed_ContextCancellation verifies that concurrent callers
// with mixed cancelled/active contexts are handled correctly — cancelled
// contexts fail fast before or after the lock, active ones proceed to init.
func TestConcurrentEmbed_ContextCancellation(t *testing.T) {
	e := embed.NewBuiltinEmbedder("/dev/null/impossible/path")
	defer e.Close()

	const goroutines = 10
	var wg sync.WaitGroup

	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx := context.Background()
			if idx%2 == 0 {
				// Even goroutines get cancelled contexts.
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, errs[idx] = e.Embed(ctx, "hello world")
		}(i)
	}
	wg.Wait()

	// Every goroutine should have failed — either context.Canceled or init error.
	for i := 0; i < goroutines; i++ {
		if errs[i] == nil {
			t.Errorf("goroutine %d: expected error, got nil", i)
		}
	}
}

// TestConcurrentClose verifies that Close() called concurrently with Embed()
// does not panic or cause a data race.
func TestConcurrentClose(t *testing.T) {
	e := embed.NewBuiltinEmbedder("/dev/null/impossible/path")

	const goroutines = 5
	var wg sync.WaitGroup

	// Start some Embed calls.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Embed(context.Background(), "test") //nolint:errcheck
		}()
	}

	// Concurrently close.
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.Close() //nolint:errcheck
	}()

	wg.Wait()
	// If we get here without a panic or race detector failure, the test passes.
}

// TestPoolSize_Default verifies the default pool size is 3.
func TestPoolSize_Default(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	defer e.Close()
	if got := e.PoolSize(); got != 3 {
		t.Errorf("default pool size = %d, want 3", got)
	}
}

// TestPoolSize_Custom verifies custom pool sizes and clamping.
func TestPoolSize_Custom(t *testing.T) {
	tests := []struct {
		input int
		want  int
	}{
		{1, 1},
		{2, 2},
		{5, 5},
		{8, 8},
		{0, 1},  // clamped to 1
		{-1, 1}, // clamped to 1
		{9, 8},  // clamped to 8
		{100, 8},
	}
	for _, tc := range tests {
		e := embed.NewBuiltinEmbedderWithPoolSize(t.TempDir(), tc.input)
		defer e.Close()
		if got := e.PoolSize(); got != tc.want {
			t.Errorf("NewBuiltinEmbedderWithPoolSize(_, %d).PoolSize() = %d, want %d", tc.input, got, tc.want)
		}
	}
}

// TestCloseIdempotent verifies that calling Close() multiple times is safe.
func TestCloseIdempotent(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())

	// Close twice — should not panic.
	if err := e.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestEmbedAfterClose verifies that Embed returns an error after Close.
func TestEmbedAfterClose(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	e.Close()

	_, err := e.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error after Close, got nil")
	}
	if got := err.Error(); got != "builtin embedder: closed" {
		t.Errorf("error = %q, want %q", got, "builtin embedder: closed")
	}
}

// TestConcurrentClose_Multiple verifies concurrent Close calls are safe.
func TestConcurrentClose_Multiple(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Close() //nolint:errcheck
		}()
	}
	wg.Wait()
}

// TestConcurrentEmbed_CloseUnblocksWaiters verifies that Close() unblocks
// goroutines waiting for a pool slot or failing on init.
func TestConcurrentEmbed_CloseUnblocksWaiters(t *testing.T) {
	e := embed.NewBuiltinEmbedderWithPoolSize("/dev/null/impossible/path", 1)

	var wg sync.WaitGroup
	var errCount atomic.Int32

	// Launch goroutines that will all fail on init (since path is impossible).
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.Embed(context.Background(), "test")
			if err != nil {
				errCount.Add(1)
			}
		}()
	}

	// Give goroutines time to attempt init, then close.
	time.Sleep(10 * time.Millisecond)
	e.Close()
	wg.Wait()

	// All should have errored (init failure or closed).
	if got := errCount.Load(); got != 5 {
		t.Errorf("error count = %d, want 5", got)
	}
}

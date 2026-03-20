package embed_test

import (
	"context"
	"sync"
	"testing"

	"github.com/SynapsesOS/synapses/internal/embed"
)

// TestConcurrentEmbed_MutexSerialization verifies that multiple goroutines
// calling Embed() concurrently are correctly serialized by the mutex and
// do not panic or corrupt state. Uses a path that forces init failure so
// the test runs without model download.
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

	// Even-numbered goroutines should have context.Canceled.
	// Odd-numbered should have init failure. But order is non-deterministic
	// (a cancelled goroutine might acquire the lock after init attempt),
	// so we just verify all returned errors — no panics.
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

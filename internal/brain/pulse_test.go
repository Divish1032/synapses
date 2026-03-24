package brain

import (
	"sync"
	"testing"
	"time"
)

// TestSystemPulseStartStop verifies that Start/Stop complete without panic or
// deadlock and that Stop is idempotent (safe to call multiple times).
func TestSystemPulseStartStop(t *testing.T) {
	p := NewSystemPulse()
	p.Start()
	p.Stop()
	// Second Stop must not panic or block.
	p.Stop()
}

// TestSystemPulseCurrentReturnsState verifies that after Start(), Current()
// returns a state with a non-zero SampledAt and a valid HealthLevel.
func TestSystemPulseCurrentReturnsState(t *testing.T) {
	p := NewSystemPulse()
	p.Start()
	defer p.Stop()

	state := p.Current()

	if state.SampledAt.IsZero() {
		t.Fatal("expected SampledAt to be non-zero after Start()")
	}
	if state.SampledAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("SampledAt %v is in the future", state.SampledAt)
	}
	if state.Health < HealthGreen || state.Health > HealthRed {
		t.Fatalf("unexpected HealthLevel %d", state.Health)
	}
}

// TestCurrentBeforeStart verifies that Current() before Start() returns a
// zero-value state (SampledAt.IsZero() == true) without panic.
func TestCurrentBeforeStart(t *testing.T) {
	p := NewSystemPulse()
	state := p.Current()
	if !state.SampledAt.IsZero() {
		t.Fatalf("expected zero SampledAt before Start(), got %v", state.SampledAt)
	}
}

// TestComputeHealth exercises all boundary conditions of the health classifier.
func TestComputeHealth(t *testing.T) {
	const (
		gb   = int64(1024 * 1024 * 1024)
		mb   = int64(1024 * 1024)
		ram4 = 4 * gb
		ram2 = 2 * gb
		ram1 = gb
	)

	tests := []struct {
		name     string
		ram      int64
		cpu      float64
		expected HealthLevel
	}{
		// Green: RAM > 3 GB AND CPU < 0.7
		{name: "green_nominal", ram: ram4, cpu: 0.5, expected: HealthGreen},
		{name: "green_high_ram", ram: 10 * gb, cpu: 0.0, expected: HealthGreen},
		{name: "green_cpu_at_threshold", ram: ram4, cpu: 0.69, expected: HealthGreen},

		// Yellow: RAM 1.5-3 GB (CPU fine)
		{name: "yellow_ram_mid", ram: ram2, cpu: 0.5, expected: HealthYellow},
		{name: "yellow_ram_exactly_1500mb", ram: 1536 * mb, cpu: 0.5, expected: HealthYellow},
		{name: "yellow_ram_just_under_3gb", ram: 3*gb - mb, cpu: 0.5, expected: HealthYellow},

		// Yellow: CPU 0.7-0.9 (RAM fine)
		{name: "yellow_cpu_at_green_threshold", ram: ram4, cpu: 0.7, expected: HealthYellow},
		{name: "yellow_cpu_mid", ram: ram4, cpu: 0.8, expected: HealthYellow},
		{name: "yellow_cpu_at_red_threshold", ram: ram4, cpu: 0.9, expected: HealthYellow},

		// Red: RAM < 1.5 GB
		{name: "red_low_ram", ram: ram1, cpu: 0.5, expected: HealthRed},
		{name: "red_ram_just_under_1500mb", ram: 1536*mb - 1, cpu: 0.0, expected: HealthRed},
		{name: "red_zero_ram", ram: 0, cpu: 0.0, expected: HealthRed},

		// Red: CPU > 0.9 (RAM fine)
		{name: "red_high_cpu", ram: ram4, cpu: 0.95, expected: HealthRed},
		{name: "red_cpu_1.0", ram: ram4, cpu: 1.0, expected: HealthRed},

		// Red wins over Yellow: both conditions bad
		{name: "red_both_bad", ram: ram1, cpu: 0.95, expected: HealthRed},

		// Yellow wins: CPU yellow + RAM green
		{name: "yellow_cpu_yellow_ram_green", ram: ram4, cpu: 0.75, expected: HealthYellow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeHealth(tc.ram, tc.cpu)
			if got != tc.expected {
				t.Errorf("computeHealth(ram=%d, cpu=%.2f) = %v, want %v",
					tc.ram, tc.cpu, got, tc.expected)
			}
		})
	}
}

// TestPollOllamaNoServer verifies that when Ollama is not running,
// OllamaModelLoaded is set to "" without returning an error.
func TestPollOllamaNoServer(t *testing.T) {
	p := NewSystemPulse()
	// pollOllama uses p.httpClient which has a 2-second timeout.
	// Since no server is listening on 11434 in CI, the connection is
	// refused immediately (not a 2-second wait).
	p.pollOllama()

	state := p.Current()
	if state.OllamaModelLoaded != "" {
		t.Errorf("expected empty OllamaModelLoaded when Ollama is not running, got %q",
			state.OllamaModelLoaded)
	}
}

// TestSamplePlatformReturnsPositiveValues verifies the Linux /proc reader
// returns RAM > 0 and CPU >= 0 on the current host.
func TestSamplePlatformReturnsPositiveValues(t *testing.T) {
	p := NewSystemPulse()
	ram, cpu, err := p.samplePlatform()
	if err != nil {
		t.Fatalf("samplePlatform() error: %v", err)
	}
	if ram <= 0 {
		t.Errorf("expected RAM > 0, got %d", ram)
	}
	if cpu < 0 {
		t.Errorf("expected CPU >= 0, got %f", cpu)
	}
}

// TestHealthLevelString verifies the String() method for all health levels.
func TestHealthLevelString(t *testing.T) {
	tests := []struct {
		level    HealthLevel
		expected string
	}{
		{HealthGreen, "green"},
		{HealthYellow, "yellow"},
		{HealthRed, "red"},
		{HealthLevel(99), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.level.String(); got != tc.expected {
			t.Errorf("HealthLevel(%d).String() = %q, want %q", tc.level, got, tc.expected)
		}
	}
}

// TestNumCPUSafe verifies it always returns >= 1.
func TestNumCPUSafe(t *testing.T) {
	if n := numCPUSafe(); n < 1 {
		t.Errorf("numCPUSafe() = %f, want >= 1", n)
	}
}

// TestConcurrentCurrentCalls verifies that many concurrent reads of Current()
// do not cause a race or panic while the background loop is running.
func TestConcurrentCurrentCalls(t *testing.T) {
	p := NewSystemPulse()
	p.Start()
	defer p.Stop()

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					_ = p.Current()
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(done)
}

// TestConcurrentStartStop verifies that Start() and Stop() called from
// different goroutines simultaneously do not data-race or deadlock.
// This specifically targets the channel-reassignment race that existed in the
// original implementation (p.stopped = make(chan struct{}) in Start() vs
// <-p.stopped in Stop()).
func TestConcurrentStartStop(t *testing.T) {
	for i := 0; i < 50; i++ {
		p := NewSystemPulse()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); p.Start() }()
		go func() { defer wg.Done(); p.Stop() }()
		wg.Wait()
		// After both return, the pulse must be in a clean stopped state —
		// a second Stop() must not block or panic.
		p.Stop()
	}
}

// TestStopWithoutStartDoesNotBlock verifies that Stop() called without a prior
// Start() returns immediately (does not block forever on a channel receive).
func TestStopWithoutStartDoesNotBlock(t *testing.T) {
	done := make(chan struct{})
	go func() {
		p := NewSystemPulse()
		p.Stop() // must not block
		close(done)
	}()
	select {
	case <-done:
		// pass
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() without Start() blocked for more than 2 seconds")
	}
}

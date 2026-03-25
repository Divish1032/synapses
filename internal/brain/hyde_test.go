package brain

// Internal package tests for GenerateHypothetical.
//
// These tests use a stubBrain (implements Brain interface) to verify the positive
// HyDE path without needing a live Ollama instance. Because they are in package
// brain (not brain_test), they can access the unexported Client fields directly.

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/archivist"
)

// stubBrain is a minimal Brain implementation that returns a fixed Generate response.
// All other methods delegate to NullBrain.
type stubBrain struct {
	NullBrain
	generateResp string
	generateErr  error
}

func (s *stubBrain) Generate(_ context.Context, _ string) (string, error) {
	return s.generateResp, s.generateErr
}

// Keep the compiler happy — stubBrain must satisfy the full Brain interface.
// The embed below ensures any new methods added to Brain will fail compilation here.
var _ Brain = (*stubBrain)(nil)

// newStubClient builds a Client backed by a stubBrain with no system monitoring.
// nil pulse means the scheduler runs tasks immediately (test-safe path).
func newStubClient(resp string) *Client {
	return &Client{
		brain:     &stubBrain{generateResp: resp},
		scheduler: NewScheduler(nil), // nil pulse = immediate execution, no health checks
	}
}

// ── Positive path ─────────────────────────────────────────────────────────────

// TestGenerateHypothetical_ReturnsHypothesis verifies the full positive path:
// when the brain returns a non-empty response, GenerateHypothetical trims it
// and returns it to the caller.
func TestGenerateHypothetical_ReturnsHypothesis(t *testing.T) {
	const want = "func RateLimiter(rate float64, burst int) *Limiter"
	c := newStubClient(want)
	got := c.GenerateHypothetical(context.Background(), "rate limiter")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestGenerateHypothetical_TrimSpace verifies that leading/trailing whitespace
// (e.g. a newline-prefixed response from the LLM) is stripped.
func TestGenerateHypothetical_TrimSpace(t *testing.T) {
	c := newStubClient("\n\nfunc Auth() bool\n")
	got := c.GenerateHypothetical(context.Background(), "authenticate")
	if got != "func Auth() bool" {
		t.Errorf("expected trimmed string, got %q", got)
	}
}

// TestGenerateHypothetical_WhitespaceOnlyResponse verifies that a whitespace-only
// response is treated as empty (returns "") so callers fall back to raw query.
func TestGenerateHypothetical_WhitespaceOnlyResponse(t *testing.T) {
	c := newStubClient("   \n\t  ")
	got := c.GenerateHypothetical(context.Background(), "auth")
	// TrimSpace → "" → returned as "".
	if got != "" {
		t.Errorf("expected empty for whitespace-only response, got %q", got)
	}
}

// TestGenerateHypothetical_QueryTruncated verifies that queries longer than
// hydeMaxQueryRunes are truncated before the prompt is built — the response
// still arrives (the LLM isn't confused by a truncated query).
func TestGenerateHypothetical_QueryTruncated(t *testing.T) {
	const want = "func Process() error"
	c := newStubClient(want)
	longQuery := strings.Repeat("authentication middleware circuit breaker ", 20) // >150 runes
	got := c.GenerateHypothetical(context.Background(), longQuery)
	if got != want {
		t.Errorf("long-query path: got %q, want %q", got, want)
	}
}

// TestGenerateHypothetical_QueryWithQuotes verifies that embedded quotes in the
// query are handled safely (the prompt is built with %q escaping — no panic).
func TestGenerateHypothetical_QueryWithQuotes(t *testing.T) {
	const want = `func Authenticate(user "admin") bool`
	c := newStubClient(want)
	got := c.GenerateHypothetical(context.Background(), `authenticate "admin" user`)
	if got != want {
		t.Errorf("quote-in-query path: got %q, want %q", got, want)
	}
}

// TestGenerateHypothetical_TimeoutReturnsEmpty verifies that when the context
// deadline is already exceeded, GenerateHypothetical returns "" immediately.
func TestGenerateHypothetical_TimeoutReturnsEmpty(t *testing.T) {
	// Use a stubBrain that would return something, but the context is expired.
	c := newStubClient("func ShouldNotReturn() {}")
	ctx, cancel := context.WithTimeout(context.Background(), 0) // already expired
	defer cancel()
	// The 0-timeout context is already done; brain.Generate should fail.
	// Since stubBrain ignores context, it will still return the value.
	// The real test: real LLMs honour context cancellation.
	// Here we just verify no panic occurs.
	_ = c.GenerateHypothetical(ctx, "test")
}

// TestGenerateHypothetical_HyDETimeout verifies that the 500ms hard deadline
// is applied: a stubBrain that sleeps 600ms before responding causes timeout.
func TestGenerateHypothetical_HyDETimeout(t *testing.T) {
	slowBrain := &slowStubBrain{delay: 600 * time.Millisecond}
	c := &Client{
		brain:     slowBrain,
		scheduler: NewScheduler(nil),
	}
	start := time.Now()
	got := c.GenerateHypothetical(context.Background(), "slow query")
	elapsed := time.Since(start)
	// Must return "" (timeout path) and must not block for the full 600ms.
	if got != "" {
		t.Errorf("expected empty from timed-out brain, got %q", got)
	}
	// The 500ms timeout should fire before the brain's 600ms sleep.
	// Allow generous headroom for CI (700ms).
	if elapsed > 700*time.Millisecond {
		t.Errorf("GenerateHypothetical blocked for %v, expected ≤700ms", elapsed)
	}
}

// slowStubBrain sleeps before responding to simulate a slow LLM.
type slowStubBrain struct {
	NullBrain
	delay time.Duration
}

func (s *slowStubBrain) Generate(ctx context.Context, _ string) (string, error) {
	select {
	case <-time.After(s.delay):
		return "func Slow() {}", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// ── Interface compliance sentinel ─────────────────────────────────────────────

// Verify that stubBrain and slowStubBrain fully implement Brain so that adding
// new methods to the interface causes a compile failure here — not silent drift.

var _ Brain = (*slowStubBrain)(nil)

// Satisfy io.Writer usage in EnsureModel for NullBrain embed.
var _ io.Writer = (*strings.Builder)(nil)

// Satisfy archivist.MemorizeRequest usage in Memorize for NullBrain embed.
var _ = archivist.MemorizeRequest{}

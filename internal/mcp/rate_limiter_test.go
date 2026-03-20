package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/config"
)

// okHandler is a trivial handler that always returns success.
var okHandler server.ToolHandlerFunc = func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("ok"), nil
}

// allowBucket is a single-goroutine test helper that simulates one allow() call
// using the refill+hasToken+consumeOne sequence. tokenBucket is not self-locking
// (callers hold sessionBuckets.mu in production); in unit tests we run
// single-goroutine, so no lock is needed.
func allowBucket(b *tokenBucket) (bool, time.Duration) {
	now := time.Now()
	b.refill(now)
	ok, ra := b.hasToken()
	if ok {
		b.consumeOne()
	}
	return ok, ra
}

// ── tokenBucket unit tests ─────────────────────────────────────────────────

func TestTokenBucket_allowsUpToCapacity(t *testing.T) {
	b := newTokenBucket(3)
	for i := range 3 {
		ok, _ := allowBucket(&b)
		if !ok {
			t.Fatalf("call %d: expected allowed, got denied", i+1)
		}
	}
}

func TestTokenBucket_deniesWhenEmpty(t *testing.T) {
	b := newTokenBucket(2)
	allowBucket(&b) //nolint:errcheck
	allowBucket(&b) //nolint:errcheck

	ok, retryAfter := allowBucket(&b)
	if ok {
		t.Fatal("expected denied on 3rd call (bucket empty)")
	}
	if retryAfter <= 0 {
		t.Fatalf("expected positive retryAfter, got %v", retryAfter)
	}
	// capacity=2 → rate = 2/60 tokens/sec → 1 token takes 30s.
	// Allow ±1s slack for clock jitter.
	if retryAfter < 29*time.Second || retryAfter > 31*time.Second {
		t.Errorf("retryAfter %v outside expected range [29s, 31s] for 2/min bucket", retryAfter)
	}
}

func TestTokenBucket_disabledAllowsAll(t *testing.T) {
	b := newTokenBucket(-1)
	for range 100 {
		if ok, _ := allowBucket(&b); !ok {
			t.Fatal("disabled bucket (perMinute=-1) must always allow")
		}
	}
}

func TestTokenBucket_zeroPerMinuteDisabled(t *testing.T) {
	// Zero is treated as "use default" at the config layer but newTokenBucket(0)
	// produces capacity ≤ 0 → disabled.
	b := newTokenBucket(0)
	for range 10 {
		if ok, _ := allowBucket(&b); !ok {
			t.Fatal("zero-rate bucket should be disabled (always allow)")
		}
	}
}

func TestTokenBucket_refillsOverTime(t *testing.T) {
	b := newTokenBucket(1) // 1/min → one token per 60s, starts with 1

	// Consume the initial token.
	allowBucket(&b) //nolint:errcheck
	ok, _ := allowBucket(&b)
	if ok {
		t.Fatal("bucket should be empty after exhausting the single token")
	}

	// Advance lastRefill by 61s (simulates idle time without sleeping).
	b.lastRefill = b.lastRefill.Add(-61 * time.Second)

	ok, _ = allowBucket(&b)
	if !ok {
		t.Fatal("expected allowed after simulated 61s refill")
	}
}

func TestTokenBucket_noBurstBeyondCapacity(t *testing.T) {
	b := newTokenBucket(5)

	// Simulate 10 minutes of idle — tokens must cap at 5, not reach 50.
	b.lastRefill = b.lastRefill.Add(-600 * time.Second)

	allowed := 0
	for range 10 {
		if ok, _ := allowBucket(&b); ok {
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("expected exactly 5 allowed calls (capacity cap), got %d", allowed)
	}
}

func TestTokenBucket_hasTokenDoesNotConsume(t *testing.T) {
	b := newTokenBucket(1)
	now := time.Now()
	b.refill(now)

	// Calling hasToken multiple times must not reduce the token count.
	ok1, _ := b.hasToken()
	ok2, _ := b.hasToken()
	if !ok1 || !ok2 {
		t.Fatal("hasToken must not consume: repeated calls on a full bucket should all return true")
	}

	// A single consume brings it to zero.
	b.consumeOne()
	ok, _ := b.hasToken()
	if ok {
		t.Fatal("after consumeOne the bucket should be empty")
	}
}

func TestTokenBucket_refillUsesProvidedTime(t *testing.T) {
	b := newTokenBucket(60) // 1 token/sec, start full at 60

	// Drain all 60 tokens.
	for range 60 {
		allowBucket(&b)
	}
	ok, _ := allowBucket(&b)
	if ok {
		t.Fatal("bucket should be empty after 61 calls with capacity=60")
	}

	// Advance time by exactly 5s → should get exactly 5 tokens back.
	future := time.Now().Add(5 * time.Second)
	b.refill(future)

	allowed := 0
	for range 10 {
		if ok, _ := b.hasToken(); ok {
			b.consumeOne()
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("expected 5 tokens after 5s refill at 1/sec, got %d", allowed)
	}
}

// ── newRateLimiter config ──────────────────────────────────────────────────

func TestNewRateLimiter_defaultsApplied(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{})
	if rl.writeLimitPerMin != defaultWriteOpsPerMinute {
		t.Errorf("write limit: got %d, want %d", rl.writeLimitPerMin, defaultWriteOpsPerMinute)
	}
	if rl.expensiveReadLimitPerMin != defaultExpensiveReadsPerMinute {
		t.Errorf("expensive read limit: got %d, want %d", rl.expensiveReadLimitPerMin, defaultExpensiveReadsPerMinute)
	}
	if rl.crossProjectLimitPerMin != defaultCrossProjectPerMinute {
		t.Errorf("cross-project limit: got %d, want %d", rl.crossProjectLimitPerMin, defaultCrossProjectPerMinute)
	}
}

func TestNewRateLimiter_configOverridesDefaults(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{
		WriteOpsPerMinute:       5,
		ExpensiveReadsPerMinute: 3,
		CrossProjectPerMinute:   10,
	})
	if rl.writeLimitPerMin != 5 {
		t.Errorf("write limit: got %d, want 5", rl.writeLimitPerMin)
	}
	if rl.expensiveReadLimitPerMin != 3 {
		t.Errorf("expensive read limit: got %d, want 3", rl.expensiveReadLimitPerMin)
	}
	if rl.crossProjectLimitPerMin != 10 {
		t.Errorf("cross-project limit: got %d, want 10", rl.crossProjectLimitPerMin)
	}
}

func TestNewRateLimiter_negativeOneDisablesCategory(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: -1})
	if rl.writeLimitPerMin != -1 {
		t.Errorf("expected -1 (disabled), got %d", rl.writeLimitPerMin)
	}
}

// ── rateLimiter.check correctness ─────────────────────────────────────────

// TestRateLimiter_check_noTokenBleedOnFailure is the critical correctness test:
// a multi-category call that fails on the second category must not consume
// tokens from the first category (true peek-then-consume under a single lock).
func TestRateLimiter_check_noTokenBleedOnFailure(t *testing.T) {
	// write=10 (plenty), cross_project=1 (tight).
	rl := newRateLimiter(config.RateLimitConfig{
		WriteOpsPerMinute:     10,
		CrossProjectPerMinute: 1,
	})

	args := map[string]interface{}{"projects": "*"}

	// First call: write + cross_project both pass; tokens consumed from both.
	res := rl.check("sess", "remember", args)
	if !res.allowed {
		t.Fatal("first call should be allowed")
	}

	// Second call: cross_project is exhausted.
	// CRITICAL: the write bucket must NOT have been charged for this rejection.
	res = rl.check("sess", "remember", args)
	if res.allowed {
		t.Fatal("second call should be denied (cross_project exhausted)")
	}
	if res.category != "cross_project" {
		t.Errorf("expected cross_project category, got %q", res.category)
	}

	// A plain write (no projects=) must still succeed: 9 write tokens remain.
	res = rl.check("sess", "remember", nil)
	if !res.allowed {
		t.Fatal("plain write should succeed — write bucket must not have been charged on the rejected cross_project call")
	}
}

func TestRateLimiter_writeToolBlocked(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 1})

	res := rl.check("sess1", "remember", nil)
	if !res.allowed {
		t.Fatal("first call should be allowed")
	}
	res = rl.check("sess1", "remember", nil)
	if res.allowed {
		t.Fatal("second call should be denied")
	}
	if res.category != "write_ops" {
		t.Errorf("expected category write_ops, got %q", res.category)
	}
	if res.retryAfter <= 0 {
		t.Errorf("expected positive retryAfter, got %v", res.retryAfter)
	}
}

func TestRateLimiter_expensiveReadToolBlocked(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{ExpensiveReadsPerMinute: 1})

	res := rl.check("sess1", "recall", nil)
	if !res.allowed {
		t.Fatal("first recall should be allowed")
	}
	res = rl.check("sess1", "recall", nil)
	if res.allowed {
		t.Fatal("second recall should be denied")
	}
	if res.category != "expensive_reads" {
		t.Errorf("expected category expensive_reads, got %q", res.category)
	}
}

func TestRateLimiter_crossProjectBlocked(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{CrossProjectPerMinute: 1})

	args := map[string]interface{}{"projects": "*"}
	res := rl.check("sess1", "get_events", args)
	if !res.allowed {
		t.Fatal("first cross-project call should be allowed")
	}
	res = rl.check("sess1", "get_events", args)
	if res.allowed {
		t.Fatal("second cross-project call should be denied")
	}
	if res.category != "cross_project" {
		t.Errorf("expected category cross_project, got %q", res.category)
	}
}

func TestRateLimiter_separateSessionsIndependent(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 1})

	rl.check("sess1", "remember", nil)
	res := rl.check("sess1", "remember", nil)
	if res.allowed {
		t.Fatal("sess1 second call should be denied")
	}

	res = rl.check("sess2", "remember", nil)
	if !res.allowed {
		t.Fatal("sess2 must have an independent budget")
	}
}

func TestRateLimiter_clearSession(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 1})

	rl.check("sess1", "remember", nil)
	res := rl.check("sess1", "remember", nil)
	if res.allowed {
		t.Fatal("sess1 should be rate limited")
	}

	rl.clearSession("sess1")
	res = rl.check("sess1", "remember", nil)
	if !res.allowed {
		t.Fatal("after clearSession a fresh bucket with full budget should be created")
	}
}

func TestRateLimiter_nonRateLimitedToolsNotChecked(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{
		WriteOpsPerMinute:       1,
		ExpensiveReadsPerMinute: 1,
		CrossProjectPerMinute:   1,
	})

	// Exhaust all session buckets.
	rl.check("sess1", "remember", nil)
	rl.check("sess1", "recall", nil)
	rl.check("sess1", "get_context", map[string]interface{}{"projects": "*"})

	// A tool not in any rate-limited set, with no projects= arg, must pass freely.
	res := rl.check("sess1", "get_context", nil)
	if !res.allowed {
		t.Fatal("get_context (no projects=) must not be rate limited even when other buckets are full")
	}
}

// ── Concurrency: TOCTOU absence test ──────────────────────────────────────

// TestRateLimiter_check_concurrentSameSession verifies that concurrent calls
// from the same session never exceed the limit — proving the single-lock
// peek+consume design eliminates the TOCTOU race.
//
// With limit=5 and 50 goroutines all calling check() simultaneously, exactly 5
// must be allowed and the rest denied. Any count > 5 would indicate the TOCTOU
// bug (two goroutines both peeking a token before either consumes it).
func TestRateLimiter_check_concurrentSameSession(t *testing.T) {
	const limit = 5
	const goroutines = 50

	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: limit})

	var wg sync.WaitGroup
	allowed := make([]bool, goroutines)
	start := make(chan struct{})

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // all goroutines start simultaneously
			res := rl.check("shared-session", "remember", nil)
			allowed[idx] = res.allowed
		}(i)
	}

	close(start) // release all goroutines at once
	wg.Wait()

	count := 0
	for _, ok := range allowed {
		if ok {
			count++
		}
	}
	if count != limit {
		t.Errorf("expected exactly %d allowed calls, got %d — TOCTOU race detected", limit, count)
	}
}

// ── wrap — attack vector and integration tests ─────────────────────────────

// TestRateLimiter_wrap_blocksFloodingRemember is the primary security proof:
// flooding write tools must be rejected after the budget is exhausted.
func TestRateLimiter_wrap_blocksFloodingRemember(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 2})
	wrapped := rl.wrap("remember", okHandler)

	ctx := context.Background()
	makeReq := func() mcp.CallToolRequest {
		req := mcp.CallToolRequest{}
		req.Params.Name = "remember"
		req.Params.Arguments = map[string]interface{}{"decision": "fact"}
		return req
	}

	for i := range 2 {
		result, err := wrapped(ctx, makeReq())
		if err != nil || result.IsError {
			t.Fatalf("call %d: expected success, got err=%v isError=%v", i+1, err, result.IsError)
		}
	}

	// Third call: must be rate-limited (attack vector closed).
	result, err := wrapped(ctx, makeReq())
	if err != nil {
		t.Fatalf("wrap must not return a Go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("3rd call should be rate-limited — attack vector still open")
	}

	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatal("expected TextContent in rate-limit result")
	}
	if !strings.Contains(tc.Text, "rate_limit_exceeded") {
		t.Errorf("expected rate_limit_exceeded in message, got: %q", tc.Text)
	}
	if !strings.Contains(tc.Text, "retry_after=") {
		t.Errorf("expected retry_after= in message, got: %q", tc.Text)
	}
	if !strings.Contains(tc.Text, "write_ops") {
		t.Errorf("expected write_ops category in message, got: %q", tc.Text)
	}
}

func TestRateLimiter_wrap_blocksFloodingRecall(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{ExpensiveReadsPerMinute: 1})
	wrapped := rl.wrap("recall", okHandler)

	ctx := context.Background()
	makeReq := func() mcp.CallToolRequest {
		req := mcp.CallToolRequest{}
		req.Params.Name = "recall"
		req.Params.Arguments = map[string]interface{}{"query": "auth handler"}
		return req
	}

	result, _ := wrapped(ctx, makeReq())
	if result.IsError {
		t.Fatal("first recall should be allowed")
	}
	result, err := wrapped(ctx, makeReq())
	if err != nil || !result.IsError {
		t.Fatalf("second recall should be rate-limited: err=%v isError=%v", err, result.IsError)
	}
	tc := result.Content[0].(mcp.TextContent)
	if !strings.Contains(tc.Text, "expensive_reads") {
		t.Errorf("expected expensive_reads in message, got: %q", tc.Text)
	}
}

func TestRateLimiter_wrap_blocksCrossProjectFlooding(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{CrossProjectPerMinute: 1})
	wrapped := rl.wrap("get_events", okHandler)

	ctx := context.Background()
	makeReq := func() mcp.CallToolRequest {
		req := mcp.CallToolRequest{}
		req.Params.Name = "get_events"
		req.Params.Arguments = map[string]interface{}{"projects": "*"}
		return req
	}

	result, _ := wrapped(ctx, makeReq())
	if result.IsError {
		t.Fatal("first cross-project call should be allowed")
	}
	result, err := wrapped(ctx, makeReq())
	if err != nil || !result.IsError {
		t.Fatalf("second cross-project call should be rate-limited: err=%v isError=%v", err, result.IsError)
	}
	tc := result.Content[0].(mcp.TextContent)
	if !strings.Contains(tc.Text, "cross_project") {
		t.Errorf("expected cross_project in message, got: %q", tc.Text)
	}
}

func TestRateLimiter_wrap_nonRateLimitedPassThrough(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{
		WriteOpsPerMinute:       1,
		ExpensiveReadsPerMinute: 1,
		CrossProjectPerMinute:   1,
	})

	wrapped := rl.wrap("get_context", okHandler)
	ctx := context.Background()
	for i := range 5 {
		req := mcp.CallToolRequest{}
		req.Params.Name = "get_context"
		req.Params.Arguments = map[string]interface{}{"entity": "SomeEntity"}
		result, err := wrapped(ctx, req)
		if err != nil || result.IsError {
			t.Fatalf("call %d: get_context (no projects=) must not be rate limited", i+1)
		}
	}
}

func TestRateLimiter_wrap_sessionIsolationViaContext(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 1})
	wrapped := rl.wrap("remember", okHandler)

	req := mcp.CallToolRequest{}
	req.Params.Name = "remember"
	req.Params.Arguments = map[string]interface{}{"decision": "x"}

	ctxA := WithSessionID(context.Background(), "session-A")
	wrapped(ctxA, req) //nolint:errcheck
	resultA, _ := wrapped(ctxA, req)
	if !resultA.IsError {
		t.Fatal("session-A second call should be denied")
	}

	ctxB := WithSessionID(context.Background(), "session-B")
	resultB, _ := wrapped(ctxB, req)
	if resultB.IsError {
		t.Fatal("session-B must have an independent budget, unaffected by session-A")
	}
}

func TestRateLimiter_wrap_nilArgsNoRace(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 5})
	wrapped := rl.wrap("remember", okHandler)

	ctx := context.Background()
	req := mcp.CallToolRequest{}
	req.Params.Name = "remember"
	req.Params.Arguments = nil // nil args: valid in MCP; must not panic

	result, err := wrapped(ctx, req)
	if err != nil || result.IsError {
		t.Fatalf("nil args must not produce an error: err=%v isError=%v", err, result.IsError)
	}
}

func TestRateLimiter_wrap_disabledCategoryAlwaysAllows(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: -1})
	wrapped := rl.wrap("remember", okHandler)

	ctx := context.Background()
	for i := range 100 {
		req := mcp.CallToolRequest{}
		req.Params.Name = "remember"
		req.Params.Arguments = map[string]interface{}{"decision": "x"}
		result, _ := wrapped(ctx, req)
		if result.IsError {
			t.Fatalf("call %d: disabled category must always allow", i+1)
		}
	}
}

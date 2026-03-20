package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/config"
)

// okHandler is a trivial handler that returns success. Used to verify that
// rate-limit checks don't interfere with normal calls.
var okHandler server.ToolHandlerFunc = func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("ok"), nil
}

// ── tokenBucket ────────────────────────────────────────────────────────────

func TestTokenBucket_allowsUpToCapacity(t *testing.T) {
	// With capacity=3, the first 3 calls must be allowed.
	b := newTokenBucket(3)
	for i := range 3 {
		ok, _ := b.allow()
		if !ok {
			t.Fatalf("call %d: expected allowed (bucket has tokens), got denied", i+1)
		}
	}
}

func TestTokenBucket_deniesWhenEmpty(t *testing.T) {
	b := newTokenBucket(2)
	b.allow() //nolint:errcheck
	b.allow() //nolint:errcheck

	ok, retryAfter := b.allow()
	if ok {
		t.Fatal("expected denied on 3rd call (bucket empty), got allowed")
	}
	if retryAfter <= 0 {
		t.Fatalf("expected positive retryAfter, got %v", retryAfter)
	}
	// With capacity=2, rate = 2/60 tokens/sec.
	// From 0 tokens, need 1 → wait = 30s.
	// Allow some slack for clock jitter (29s–31s).
	if retryAfter < 29*time.Second || retryAfter > 31*time.Second {
		t.Errorf("retryAfter %v outside expected range [29s, 31s] for 2/min bucket", retryAfter)
	}
}

func TestTokenBucket_disabledAllowsAll(t *testing.T) {
	b := newTokenBucket(-1)
	for range 100 {
		if ok, _ := b.allow(); !ok {
			t.Fatal("disabled bucket (perMinute=-1) should always allow")
		}
	}
}

func TestTokenBucket_zeroPerMinuteDisabled(t *testing.T) {
	// Zero means "use default" at the config layer, but newTokenBucket(0)
	// should behave as disabled (capacity ≤ 0).
	b := newTokenBucket(0)
	for range 10 {
		if ok, _ := b.allow(); !ok {
			t.Fatal("zero-rate bucket should be disabled (always allow)")
		}
	}
}

func TestTokenBucket_refillsOverTime(t *testing.T) {
	b := newTokenBucket(1) // 1/min → 1 token per 60s

	// Exhaust the single token.
	b.allow() //nolint:errcheck
	ok, _ := b.allow()
	if ok {
		t.Fatal("bucket should be empty after 1 call with capacity=1")
	}

	// Manually advance lastRefill by 61s to simulate ~1 minute of wall time.
	b.mu.Lock()
	b.lastRefill = b.lastRefill.Add(-61 * time.Second)
	b.mu.Unlock()

	// Now allow() should refill ~1 token and succeed.
	ok, _ = b.allow()
	if !ok {
		t.Fatal("expected allowed after simulated 61s refill")
	}
}

func TestTokenBucket_noBurstBeyondCapacity(t *testing.T) {
	b := newTokenBucket(5)

	// Simulate 10 minutes of idle (600s). Tokens should cap at capacity (5).
	b.mu.Lock()
	b.lastRefill = b.lastRefill.Add(-600 * time.Second)
	b.mu.Unlock()

	// Drain the bucket — exactly 5 calls should succeed.
	allowed := 0
	for range 10 {
		if ok, _ := b.allow(); ok {
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("expected exactly 5 allowed calls (capacity), got %d", allowed)
	}
}

func TestTokenBucket_peekDoesNotConsume(t *testing.T) {
	b := newTokenBucket(1)

	// Peek twice — both should see the token as available.
	ok1, _ := b.peek()
	ok2, _ := b.peek()
	if !ok1 || !ok2 {
		t.Fatal("two consecutive peeks should both succeed (peek must not consume)")
	}

	// Now consume — first allow() should succeed.
	ok, _ := b.allow()
	if !ok {
		t.Fatal("allow() after peek should succeed (token was not consumed by peek)")
	}

	// Bucket is now empty.
	ok, _ = b.allow()
	if ok {
		t.Fatal("second allow() should fail (bucket is now empty)")
	}
}

func TestTokenBucket_peekAfterEmpty(t *testing.T) {
	b := newTokenBucket(1)
	b.allow() //nolint:errcheck

	ok, retryAfter := b.peek()
	if ok {
		t.Fatal("peek on empty bucket should return false")
	}
	if retryAfter <= 0 {
		t.Errorf("peek retryAfter should be positive, got %v", retryAfter)
	}
}

func TestTokenBucket_consumeAfterPeek(t *testing.T) {
	b := newTokenBucket(3)

	ok, _ := b.peek()
	if !ok {
		t.Fatal("peek should succeed on fresh bucket")
	}
	b.consume()

	// Only 2 tokens remain after one consume.
	for range 2 {
		ok, _ := b.allow()
		if !ok {
			t.Fatal("should still have 2 tokens after one consume")
		}
	}
	ok, _ = b.allow()
	if ok {
		t.Fatal("4th call should fail (3 tokens total, 3 consumed)")
	}
}

// ── newRateLimiter ─────────────────────────────────────────────────────────

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
		t.Errorf("expected -1, got %d", rl.writeLimitPerMin)
	}
}

// ── rateLimiter.check — peek-then-consume ─────────────────────────────────

// Critical correctness property: a multi-category call that fails on a later
// category must NOT consume tokens from earlier categories.
func TestRateLimiter_check_noTokenBleedOnFailure(t *testing.T) {
	// write limit=10 (plenty), cross-project limit=1 (tight).
	rl := newRateLimiter(config.RateLimitConfig{
		WriteOpsPerMinute:     10,
		CrossProjectPerMinute: 1,
	})

	args := map[string]interface{}{"projects": "*"}

	// First call: both write and cross-project pass; tokens consumed from both.
	res := rl.check("sess", "remember", args)
	if !res.allowed {
		t.Fatal("first call should be allowed")
	}

	// Second call: cross-project is exhausted.
	// CRITICAL: write tokens must NOT have been consumed on the first rejection.
	res = rl.check("sess", "remember", args)
	if res.allowed {
		t.Fatal("second call should be denied (cross-project exhausted)")
	}
	if res.category != "cross_project" {
		t.Errorf("expected category cross_project, got %q", res.category)
	}

	// Verify write bucket was NOT charged for the failed second call.
	// A plain (non-cross-project) write should still succeed.
	res = rl.check("sess", "remember", nil)
	if !res.allowed {
		t.Fatal("write-only call should succeed — write tokens must not have been charged on the failed cross-project call")
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
		t.Fatal("sess2 should have independent budget")
	}
}

func TestRateLimiter_clearSession(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 1})

	rl.check("sess1", "remember", nil)
	res := rl.check("sess1", "remember", nil)
	if res.allowed {
		t.Fatal("sess1 should be rate limited")
	}

	// After clear, the session entry is removed. Next call creates a fresh bucket.
	rl.clearSession("sess1")
	res = rl.check("sess1", "remember", nil)
	if !res.allowed {
		t.Fatal("after clearSession, a new full budget should be created")
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

	// A tool not in any rate-limited set, with no projects= arg, must always pass.
	res := rl.check("sess1", "get_context", nil)
	if !res.allowed {
		t.Fatal("get_context (no projects=) must not be rate limited, even when other buckets are full")
	}
}

// ── rateLimiter.wrap — attack vector tests ─────────────────────────────────

// TestRateLimiter_wrap_blocksFloodingRemember is the primary security proof:
// the attack vector (flooding write tools) must be closed.
func TestRateLimiter_wrap_blocksFloodingRemember(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 2})
	wrapped := rl.wrap("remember", okHandler)

	ctx := context.Background()
	makeReq := func() mcp.CallToolRequest {
		req := mcp.CallToolRequest{}
		req.Params.Name = "remember"
		req.Params.Arguments = map[string]interface{}{"decision": "important fact"}
		return req
	}

	// First two calls: allowed (budget = 2).
	for i := range 2 {
		result, err := wrapped(ctx, makeReq())
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i+1, err)
		}
		if result.IsError {
			t.Fatalf("call %d: expected success, got error: %v", i+1, result.Content)
		}
	}

	// Third call: must be rate-limited. Attack vector closed.
	result, err := wrapped(ctx, makeReq())
	if err != nil {
		t.Fatalf("wrap must not return a Go error, got: %v", err)
	}
	if !result.IsError {
		t.Fatal("3rd call should be rate-limited but was allowed — attack vector still open")
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

	// Second call: denied. Attack vector closed.
	result, err := wrapped(ctx, makeReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("second recall should be rate-limited")
	}
	tc := result.Content[0].(mcp.TextContent)
	if !strings.Contains(tc.Text, "expensive_reads") {
		t.Errorf("expected expensive_reads in message, got: %q", tc.Text)
	}
}

func TestRateLimiter_wrap_blocksCrossProjectFlooding(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{CrossProjectPerMinute: 1})
	// Use a non-write, non-expensive-read tool to isolate the cross-project path.
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("second cross-project call should be rate-limited")
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

	// get_context with no projects= arg is not in any rate-limited set.
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

	// Session A: exhaust its budget.
	ctxA := WithSessionID(context.Background(), "session-A")
	wrapped(ctxA, req) //nolint:errcheck
	resultA, _ := wrapped(ctxA, req)
	if !resultA.IsError {
		t.Fatal("session-A second call should be denied")
	}

	// Session B: must be completely independent.
	ctxB := WithSessionID(context.Background(), "session-B")
	resultB, _ := wrapped(ctxB, req)
	if resultB.IsError {
		t.Fatal("session-B should not be affected by session-A's exhausted budget")
	}
}

// TestRateLimiter_wrap_nilArgsNoRace verifies that a tool call with nil
// arguments (valid in MCP) does not panic or data-race.
func TestRateLimiter_wrap_nilArgsNoRace(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 5})
	wrapped := rl.wrap("remember", okHandler)

	ctx := context.Background()
	req := mcp.CallToolRequest{}
	req.Params.Name = "remember"
	req.Params.Arguments = nil // nil args — must not panic

	result, err := wrapped(ctx, req)
	if err != nil {
		t.Fatalf("nil args must not produce a Go error: %v", err)
	}
	if result.IsError {
		t.Fatalf("nil args should not be rate-limited (first call): %v", result.Content)
	}
}

// TestRateLimiter_wrap_disabledCategoryAlwaysAllows verifies that setting a
// category to -1 in config disables it for that tool, regardless of call count.
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
			t.Fatalf("call %d: disabled category should always allow", i+1)
		}
	}
}

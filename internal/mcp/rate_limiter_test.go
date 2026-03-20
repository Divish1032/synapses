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

// ── fixedWindowBucket ──────────────────────────────────────────────────────

func TestFixedWindowBucket_allowsUpToLimit(t *testing.T) {
	b := &fixedWindowBucket{limit: 3}
	for i := range 3 {
		ok, _ := b.allow()
		if !ok {
			t.Fatalf("call %d: expected allowed, got denied", i+1)
		}
	}
}

func TestFixedWindowBucket_deniesOverLimit(t *testing.T) {
	b := &fixedWindowBucket{limit: 2}
	b.allow() //nolint:errcheck
	b.allow() //nolint:errcheck
	ok, retryAfter := b.allow()
	if ok {
		t.Fatal("expected denied on 3rd call, got allowed")
	}
	if retryAfter <= 0 {
		t.Fatalf("expected positive retryAfter, got %v", retryAfter)
	}
	if retryAfter > time.Minute {
		t.Fatalf("retryAfter %v > 1 minute (the window)", retryAfter)
	}
}

func TestFixedWindowBucket_disabledAllowsAll(t *testing.T) {
	b := &fixedWindowBucket{limit: -1}
	for range 100 {
		if ok, _ := b.allow(); !ok {
			t.Fatal("disabled bucket should always allow")
		}
	}
}

func TestFixedWindowBucket_zeroLimitDisabled(t *testing.T) {
	b := &fixedWindowBucket{limit: 0}
	for range 10 {
		if ok, _ := b.allow(); !ok {
			t.Fatal("zero-limit bucket should behave as disabled (always allow)")
		}
	}
}

func TestFixedWindowBucket_resetsAfterWindow(t *testing.T) {
	b := &fixedWindowBucket{limit: 1}
	b.allow() //nolint:errcheck

	// Manually rewind windowStart so the bucket thinks the window has passed.
	b.mu.Lock()
	b.windowStart = time.Now().Add(-2 * time.Minute)
	b.mu.Unlock()

	ok, _ := b.allow()
	if !ok {
		t.Fatal("expected allowed after window reset")
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
	rl := newRateLimiter(config.RateLimitConfig{
		WriteOpsPerMinute: -1,
	})
	if rl.writeLimitPerMin != -1 {
		t.Errorf("expected -1, got %d", rl.writeLimitPerMin)
	}
}

// ── rateLimiter.check ──────────────────────────────────────────────────────

func TestRateLimiter_writeToolBlocked(t *testing.T) {
	// Limit=1: first call allowed, second denied.
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 1})

	ok, _, _ := rl.check("sess1", "remember", nil)
	if !ok {
		t.Fatal("first call should be allowed")
	}
	ok, retryAfter, category := rl.check("sess1", "remember", nil)
	if ok {
		t.Fatal("second call should be denied")
	}
	if category != "write_ops" {
		t.Errorf("expected category write_ops, got %q", category)
	}
	if retryAfter <= 0 {
		t.Errorf("expected positive retryAfter, got %v", retryAfter)
	}
}

func TestRateLimiter_expensiveReadToolBlocked(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{ExpensiveReadsPerMinute: 1})

	ok, _, _ := rl.check("sess1", "recall", nil)
	if !ok {
		t.Fatal("first recall should be allowed")
	}
	ok, _, category := rl.check("sess1", "recall", nil)
	if ok {
		t.Fatal("second recall should be denied")
	}
	if category != "expensive_reads" {
		t.Errorf("expected category expensive_reads, got %q", category)
	}
}

func TestRateLimiter_crossProjectBlocked(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{CrossProjectPerMinute: 1})

	args := map[string]interface{}{"projects": "*"}
	ok, _, _ := rl.check("sess1", "get_events", args)
	if !ok {
		t.Fatal("first cross-project call should be allowed")
	}
	ok, _, category := rl.check("sess1", "get_events", args)
	if ok {
		t.Fatal("second cross-project call should be denied")
	}
	if category != "cross_project" {
		t.Errorf("expected category cross_project, got %q", category)
	}
}

func TestRateLimiter_separateSessionsIndependent(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 1})

	// Exhaust sess1's write budget.
	rl.check("sess1", "remember", nil)
	ok, _, _ := rl.check("sess1", "remember", nil)
	if ok {
		t.Fatal("sess1 second call should be denied")
	}

	// sess2 should still have its own fresh budget.
	ok, _, _ = rl.check("sess2", "remember", nil)
	if !ok {
		t.Fatal("sess2 should have independent budget")
	}
}

func TestRateLimiter_clearSession(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 1})

	// Exhaust sess1.
	rl.check("sess1", "remember", nil)
	ok, _, _ := rl.check("sess1", "remember", nil)
	if ok {
		t.Fatal("sess1 should be rate limited")
	}

	// After clear, the session entry is removed (not reset).
	// The next call will create a fresh bucket.
	rl.clearSession("sess1")
	ok, _, _ = rl.check("sess1", "remember", nil)
	if !ok {
		t.Fatal("after clearSession, a new budget should be created")
	}
}

func TestRateLimiter_nonRateLimitedToolsNotBlocked(t *testing.T) {
	// Even with a very low limit, non-rate-limited tools should pass through.
	rl := newRateLimiter(config.RateLimitConfig{
		WriteOpsPerMinute:       1,
		ExpensiveReadsPerMinute: 1,
		CrossProjectPerMinute:   1,
	})

	// Exhaust all buckets for a session.
	rl.check("sess1", "remember", nil)
	rl.check("sess1", "recall", nil)
	rl.check("sess1", "get_context", map[string]interface{}{"projects": "*"})

	// A tool that is NOT in any rate-limited set and has no projects= arg.
	ok, _, _ := rl.check("sess1", "get_context", nil)
	if !ok {
		t.Fatal("get_context (no projects=) should not be rate limited even when other buckets are full")
	}
}

// ── rateLimiter.wrap ───────────────────────────────────────────────────────

// Attack vector test: proves that the rate limiter rejects flooding of write tools.
// This is the security verification test — it must fail (allow) WITHOUT the limiter
// and pass (reject) WITH the limiter.
func TestRateLimiter_wrap_blocksFloodingRemember(t *testing.T) {
	// Limit to 2 writes/minute so we can test without exhausting 30.
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 2})

	wrapped := rl.wrap("remember", okHandler)

	ctx := context.Background()
	makeReq := func() mcp.CallToolRequest {
		req := mcp.CallToolRequest{}
		req.Params.Name = "remember"
		req.Params.Arguments = map[string]interface{}{"decision": "important fact"}
		return req
	}

	// First two calls: allowed.
	for i := range 2 {
		result, err := wrapped(ctx, makeReq())
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i+1, err)
		}
		if result.IsError {
			t.Fatalf("call %d: expected success, got error: %v", i+1, result.Content)
		}
	}

	// Third call: must be rate-limited (attack vector closed).
	result, err := wrapped(ctx, makeReq())
	if err != nil {
		t.Fatalf("wrap should not return error, got: %v", err)
	}
	if !result.IsError {
		t.Fatal("3rd call should be rate-limited but was allowed — attack vector still open")
	}
	// Verify the error message contains the expected fields.
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatal("expected TextContent in result")
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

	// First call: allowed.
	result, _ := wrapped(ctx, makeReq())
	if result.IsError {
		t.Fatal("first recall should be allowed")
	}

	// Second call: denied (attack vector closed).
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

	// First call: allowed.
	result, _ := wrapped(ctx, makeReq())
	if result.IsError {
		t.Fatal("first cross-project call should be allowed")
	}

	// Second call: denied.
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
	// Even with limits set to 0 (disabled), non-rate-limited tools pass.
	rl := newRateLimiter(config.RateLimitConfig{
		WriteOpsPerMinute:       1,
		ExpensiveReadsPerMinute: 1,
		CrossProjectPerMinute:   1,
	})

	// Tool is not in any rate-limited set and has no projects= arg.
	wrapped := rl.wrap("get_context", okHandler)
	ctx := context.Background()
	for i := range 5 {
		req := mcp.CallToolRequest{}
		req.Params.Name = "get_context"
		req.Params.Arguments = map[string]interface{}{"entity": "SomeEntity"}
		result, err := wrapped(ctx, req)
		if err != nil || result.IsError {
			t.Fatalf("call %d: get_context should not be rate limited", i+1)
		}
	}
}

func TestRateLimiter_wrap_sessionIsolationViaContext(t *testing.T) {
	rl := newRateLimiter(config.RateLimitConfig{WriteOpsPerMinute: 1})
	wrapped := rl.wrap("remember", okHandler)

	// Session A: exhaust its budget.
	ctxA := WithSessionID(context.Background(), "session-A")
	req := mcp.CallToolRequest{}
	req.Params.Name = "remember"
	req.Params.Arguments = map[string]interface{}{"decision": "x"}

	wrapped(ctxA, req) //nolint:errcheck
	resultA, _ := wrapped(ctxA, req)
	if !resultA.IsError {
		t.Fatal("session-A second call should be denied")
	}

	// Session B: should have its own independent budget.
	ctxB := WithSessionID(context.Background(), "session-B")
	resultB, _ := wrapped(ctxB, req)
	if resultB.IsError {
		t.Fatal("session-B should not be affected by session-A's exhausted budget")
	}
}

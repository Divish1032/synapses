package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/config"
)

// Default per-session rate limits (calls per minute). All defaults are
// intentionally generous — they guard against runaway agents, not normal use.
const (
	defaultWriteOpsPerMinute       = 30
	defaultExpensiveReadsPerMinute = 20
	defaultCrossProjectPerMinute   = 60
)

// writeRateLimitedTools is the set of tool names subject to the write-ops
// rate limit. These tools mutate persistent storage and are the most expensive
// to flood: they perform SQLite writes, index updates, or cross-agent fan-outs.
var writeRateLimitedTools = map[string]bool{
	"remember":      true,
	"send_message":  true,
	"annotate_node": true,
	"upsert_rule":   true,
	"create_plan":   true,
}

// expensiveReadTools is the set of tool names subject to the expensive-reads
// rate limit. recall always invokes FTS5 + optional vector search — both are
// significantly heavier than simple graph traversals.
var expensiveReadTools = map[string]bool{
	"recall": true,
}

// tokenBucket implements a continuous token-bucket rate limiter.
//
// NOT thread-safe on its own. All methods must be called while the caller
// holds sessionBuckets.mu. This is intentional: it allows the check+consume
// sequence across multiple buckets to be atomic under a single session lock,
// eliminating the TOCTOU race that would arise if each bucket had its own mutex
// (two goroutines could both peek successfully, then both consume from the same
// 1-token bucket, letting two calls through when only one should be allowed).
//
// Design: tokens accumulate continuously at ratePerSec = capacity/60. Starting
// full means the first burst (up to capacity calls) is always granted. Idle
// sessions regain tokens over time, capped at capacity.
type tokenBucket struct {
	tokens     float64   // current token count (fractional for precision)
	lastRefill time.Time // time of last refill, used to compute elapsed
	ratePerSec float64   // tokens per second = perMinute / 60
	capacity   float64   // maximum token count = perMinute; ≤ 0 means disabled
}

// newTokenBucket creates a full token bucket for the given per-minute limit.
// perMinute ≤ 0 disables rate limiting for this category (all calls allowed).
func newTokenBucket(perMinute int) tokenBucket {
	if perMinute <= 0 {
		return tokenBucket{capacity: float64(perMinute)} // capacity ≤ 0 → disabled
	}
	cap := float64(perMinute)
	return tokenBucket{
		tokens:     cap,
		lastRefill: time.Now(),
		ratePerSec: cap / 60.0,
		capacity:   cap,
	}
}

// refill adds tokens earned since lastRefill, capping at capacity.
// Must be called before hasToken or consumeOne. Accepts an external 'now'
// so a single check() call uses consistent time across all three buckets.
func (b *tokenBucket) refill(now time.Time) {
	if b.capacity <= 0 {
		return
	}
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.ratePerSec
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.lastRefill = now
}

// hasToken reports whether the bucket holds at least 1 token.
// Returns (true, 0) when available. Returns (false, retryAfter) when empty,
// where retryAfter is the exact time until one token accumulates.
// Caller must call refill(now) before this.
func (b *tokenBucket) hasToken() (bool, time.Duration) {
	if b.capacity <= 0 {
		return true, 0 // disabled — always available
	}
	if b.tokens >= 1.0 {
		return true, 0
	}
	tokensNeeded := 1.0 - b.tokens
	waitSecs := tokensNeeded / b.ratePerSec
	// Add 1ms safety buffer so agents don't retry a millisecond too early.
	return false, time.Duration(waitSecs*float64(time.Second)) + time.Millisecond
}

// consumeOne removes one token. Must only be called after hasToken returned
// true within the same critical section (i.e. while sessionBuckets.mu is held).
func (b *tokenBucket) consumeOne() {
	if b.capacity <= 0 {
		return
	}
	b.tokens--
	if b.tokens < 0 {
		b.tokens = 0 // guard against floating-point underflow
	}
}

// sessionBuckets holds the three independent rate-limit buckets for one MCP
// connection. mu serialises ALL bucket operations for this session — this is
// the key invariant that makes peek-then-consume atomic (no TOCTOU).
type sessionBuckets struct {
	mu            sync.Mutex // guards write, expensiveRead, crossProject atomically
	write         tokenBucket
	expensiveRead tokenBucket
	crossProject  tokenBucket
}

// rateLimiter manages per-session rate limit state. Embedded in Server and
// wired into addOrDefer so every applicable tool is protected uniformly.
// Thread-safe: getOrCreate serialises session creation; each session's mu
// serialises all bucket access within that session.
type rateLimiter struct {
	mu       sync.Mutex
	sessions map[string]*sessionBuckets

	writeLimitPerMin         int
	expensiveReadLimitPerMin int
	crossProjectLimitPerMin  int
}

// newRateLimiter constructs a rateLimiter from config. Zero fields fall back to
// built-in defaults. A field set to -1 disables that category entirely.
func newRateLimiter(cfg config.RateLimitConfig) *rateLimiter {
	rl := &rateLimiter{
		sessions:                 make(map[string]*sessionBuckets),
		writeLimitPerMin:         defaultWriteOpsPerMinute,
		expensiveReadLimitPerMin: defaultExpensiveReadsPerMinute,
		crossProjectLimitPerMin:  defaultCrossProjectPerMinute,
	}
	if cfg.WriteOpsPerMinute != 0 {
		rl.writeLimitPerMin = cfg.WriteOpsPerMinute
	}
	if cfg.ExpensiveReadsPerMinute != 0 {
		rl.expensiveReadLimitPerMin = cfg.ExpensiveReadsPerMinute
	}
	if cfg.CrossProjectPerMinute != 0 {
		rl.crossProjectLimitPerMin = cfg.CrossProjectPerMinute
	}
	return rl
}

// getOrCreate returns the bucket set for sessionKey, creating it if absent.
func (rl *rateLimiter) getOrCreate(sessionKey string) *sessionBuckets {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	sb, ok := rl.sessions[sessionKey]
	if !ok {
		sb = &sessionBuckets{
			write:         newTokenBucket(rl.writeLimitPerMin),
			expensiveRead: newTokenBucket(rl.expensiveReadLimitPerMin),
			crossProject:  newTokenBucket(rl.crossProjectLimitPerMin),
		}
		rl.sessions[sessionKey] = sb
	}
	return sb
}

// clearSession removes rate-limit state for the given session. Called when a
// connection closes to prevent unbounded memory growth in long-running daemons.
func (rl *rateLimiter) clearSession(sessionKey string) {
	rl.mu.Lock()
	delete(rl.sessions, sessionKey)
	rl.mu.Unlock()
}

// checkResult holds the outcome of a multi-category rate-limit check.
type checkResult struct {
	allowed    bool
	retryAfter time.Duration
	category   string
}

// check evaluates all applicable rate-limit categories for a tool call.
//
// The entire peek+consume sequence for all applicable buckets runs under a
// single session-level lock (sb.mu), making it atomically correct even when
// multiple goroutines serve the same session concurrently (possible in daemon
// mode where REST and MCP handlers run in parallel goroutine pools).
//
// A single 'now' timestamp is shared across all bucket refills so that time
// does not drift between category checks within the same call.
//
// Failure in one category never charges tokens from another (true
// peek-then-consume: all buckets are verified before any token is consumed).
func (rl *rateLimiter) check(sessionKey, toolName string, args map[string]interface{}) checkResult {
	isWrite := writeRateLimitedTools[toolName]
	isExpensive := expensiveReadTools[toolName]
	projects, _ := args["projects"].(string)
	isCrossProject := projects != ""

	if !isWrite && !isExpensive && !isCrossProject {
		return checkResult{allowed: true}
	}

	sb := rl.getOrCreate(sessionKey)

	// Single lock for the entire peek+consume sequence.
	sb.mu.Lock()
	defer sb.mu.Unlock()

	now := time.Now()

	// Phase 1 — peek: refill then verify all applicable buckets. No tokens
	// are consumed yet. First failing category returns immediately so the
	// agent gets the most actionable limit name and retry hint.
	if isWrite {
		sb.write.refill(now)
		if ok, ra := sb.write.hasToken(); !ok {
			return checkResult{allowed: false, retryAfter: ra, category: "write_ops"}
		}
	}
	if isExpensive {
		sb.expensiveRead.refill(now)
		if ok, ra := sb.expensiveRead.hasToken(); !ok {
			return checkResult{allowed: false, retryAfter: ra, category: "expensive_reads"}
		}
	}
	if isCrossProject {
		sb.crossProject.refill(now)
		if ok, ra := sb.crossProject.hasToken(); !ok {
			return checkResult{allowed: false, retryAfter: ra, category: "cross_project"}
		}
	}

	// Phase 2 — consume: all checks passed; deduct one token per applicable
	// bucket. Still under sb.mu so no other goroutine can interleave.
	if isWrite {
		sb.write.consumeOne()
	}
	if isExpensive {
		sb.expensiveRead.consumeOne()
	}
	if isCrossProject {
		sb.crossProject.consumeOne()
	}

	return checkResult{allowed: true}
}

// wrap returns a handler that applies rate limiting before calling h.
//
// Fast path: if the tool is not in any rate-limited set and the call carries
// no projects= arg, the handler is called directly with zero mutex overhead.
//
// When a limit is exceeded the underlying handler is NOT called; a 429-style
// error result is returned with the limit category and retry_after hint.
func (rl *rateLimiter) wrap(toolName string, h server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, _ := req.Params.Arguments.(map[string]interface{})

		// Fast path: not rate-limited and no cross-project arg.
		isWrite := writeRateLimitedTools[toolName]
		isExpensive := expensiveReadTools[toolName]
		projects, _ := args["projects"].(string)
		isCrossProject := projects != ""

		if !isWrite && !isExpensive && !isCrossProject {
			return h(ctx, req)
		}

		sessionKey := synapseSessionKey(SessionIDFromContext(ctx))
		res := rl.check(sessionKey, toolName, args)
		if !res.allowed {
			secs := int(res.retryAfter.Seconds()) + 1
			return mcp.NewToolResultError(fmt.Sprintf(
				"rate_limit_exceeded: tool %q has exceeded the %s rate limit for this session. "+
					"retry_after=%ds. Limits refill continuously (token bucket). "+
					"Configure rate_limits in synapses.json to adjust.",
				toolName, res.category, secs,
			)), nil
		}

		return h(ctx, req)
	}
}

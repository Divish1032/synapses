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
// Tokens accumulate at a constant rate (perMinute/60 per second) up to a
// maximum capacity equal to perMinute. Each allowed call consumes one token.
// When the bucket is empty, calls are rejected with a retryAfter hint equal to
// the time until the next token is available.
//
// This is strictly better than a fixed-window limiter for our use case:
//   - No double-burst at window boundaries (a fixed-window allows 2× the limit
//     in the 2 seconds straddling a window reset).
//   - retryAfter is exact: it reflects the actual wait time, not "some time
//     before the next window".
//   - An idle session that returns after minutes gets tokens back gradually
//     (capped at capacity), which matches natural agent usage patterns.
type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64   // current token count (fractional for precision)
	lastRefill time.Time // timestamp of last refill computation
	ratePerSec float64   // tokens added per second = perMinute / 60
	capacity   float64   // maximum token count = perMinute; ≤0 means disabled
}

// newTokenBucket creates a full token bucket for the given per-minute rate.
// The bucket starts full so the first burst (up to perMinute calls) is always
// allowed — sessions start with their complete budget, not an empty one.
// perMinute ≤ 0 disables rate limiting for this category (always allow).
func newTokenBucket(perMinute int) tokenBucket {
	if perMinute <= 0 {
		// capacity ≤ 0 signals "disabled" — allow() always returns true.
		return tokenBucket{capacity: float64(perMinute)}
	}
	cap := float64(perMinute)
	return tokenBucket{
		tokens:     cap,
		lastRefill: time.Now(),
		ratePerSec: cap / 60.0,
		capacity:   cap,
	}
}

// allow returns (true, 0) when a token is available, consuming it.
// Returns (false, retryAfter) when the bucket is empty; retryAfter is the
// exact duration until one token accumulates at the current refill rate.
func (b *tokenBucket) allow() (bool, time.Duration) {
	if b.capacity <= 0 {
		return true, 0 // disabled — always allow
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Refill tokens proportional to elapsed wall time.
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.ratePerSec
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.lastRefill = now

	if b.tokens >= 1.0 {
		b.tokens--
		return true, 0
	}

	// Exact wait until the bucket holds 1.0 tokens, plus a 1 ms safety buffer
	// so the agent doesn't retry a millisecond too early.
	tokensNeeded := 1.0 - b.tokens
	waitSecs := tokensNeeded / b.ratePerSec
	retryAfter := time.Duration(waitSecs*float64(time.Second)) + time.Millisecond
	return false, retryAfter
}

// sessionBuckets holds the three independent rate-limit buckets for one MCP
// connection (identified by its session key). Created lazily on first access.
type sessionBuckets struct {
	write         tokenBucket
	expensiveRead tokenBucket
	crossProject  tokenBucket
}

// rateLimiter manages per-session rate limit state. It is embedded in Server
// and wired into addOrDefer so every applicable tool is protected without
// per-handler boilerplate. Thread-safe.
type rateLimiter struct {
	mu       sync.Mutex
	sessions map[string]*sessionBuckets

	writeLimitPerMin        int
	expensiveReadLimitPerMin int
	crossProjectLimitPerMin  int
}

// newRateLimiter constructs a rateLimiter from synapses.json config.
// Any zero-value field in cfg falls back to the built-in default.
// A field set to exactly -1 disables that category (limit ≤ 0 → always allow).
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
// Caller must NOT hold rl.mu.
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
	allowed     bool
	retryAfter  time.Duration
	category    string
}

// check evaluates all applicable rate-limit categories for the given tool call
// using a peek-then-consume strategy: all applicable limits are verified before
// any tokens are consumed, so a failure in one category never silently charges
// tokens from another.
//
// Categories checked:
//   - write: if toolName is in writeRateLimitedTools
//   - expensiveRead: if toolName is in expensiveReadTools
//   - crossProject: if args["projects"] is non-empty (any cross-project query)
func (rl *rateLimiter) check(sessionKey, toolName string, args map[string]interface{}) checkResult {
	isWrite := writeRateLimitedTools[toolName]
	isExpensive := expensiveReadTools[toolName]
	projects, _ := args["projects"].(string)
	isCrossProject := projects != ""

	if !isWrite && !isExpensive && !isCrossProject {
		return checkResult{allowed: true}
	}

	sb := rl.getOrCreate(sessionKey)

	// Phase 1 — peek: verify all applicable limits without consuming tokens.
	// This prevents charging a write token when a cross-project limit would
	// reject the call anyway (e.g. recall(projects="*") exhausts cross_project
	// first, without burning an expensive_read token for a call that won't run).
	if isWrite {
		if ok, ra := sb.write.peek(); !ok {
			return checkResult{allowed: false, retryAfter: ra, category: "write_ops"}
		}
	}
	if isExpensive {
		if ok, ra := sb.expensiveRead.peek(); !ok {
			return checkResult{allowed: false, retryAfter: ra, category: "expensive_reads"}
		}
	}
	if isCrossProject {
		if ok, ra := sb.crossProject.peek(); !ok {
			return checkResult{allowed: false, retryAfter: ra, category: "cross_project"}
		}
	}

	// Phase 2 — consume: all checks passed; consume one token per applicable bucket.
	if isWrite {
		sb.write.consume()
	}
	if isExpensive {
		sb.expensiveRead.consume()
	}
	if isCrossProject {
		sb.crossProject.consume()
	}

	return checkResult{allowed: true}
}

// peek checks whether the bucket has at least 1 token without consuming it.
// Returns (true, 0) if a token is available, (false, retryAfter) otherwise.
// Thread-safe; uses the same lock as allow().
func (b *tokenBucket) peek() (bool, time.Duration) {
	if b.capacity <= 0 {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	current := b.tokens + elapsed*b.ratePerSec
	if current > b.capacity {
		current = b.capacity
	}

	if current >= 1.0 {
		return true, 0
	}
	tokensNeeded := 1.0 - current
	waitSecs := tokensNeeded / b.ratePerSec
	return false, time.Duration(waitSecs*float64(time.Second)) + time.Millisecond
}

// consume removes one token. Must only be called after a successful peek()
// within the same logical operation. It re-runs the refill so the time elapsed
// between peek() and consume() is accounted for (always safe: more elapsed
// time means more tokens, never fewer).
func (b *tokenBucket) consume() {
	if b.capacity <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.ratePerSec
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.lastRefill = now

	// Guaranteed ≥ 1 token because we peeked successfully; clamp to zero on
	// the rare nanosecond-level race where elapsed rounds down to exactly 0.
	b.tokens--
	if b.tokens < 0 {
		b.tokens = 0
	}
}

// wrap returns a handler that applies rate limiting before calling h.
//
// For tools not subject to any limit (not write, not expensive-read, and no
// projects= arg), the call is forwarded immediately with zero overhead beyond
// a map lookup and a type assertion.
//
// When a limit is exceeded, the original handler is NOT called; a 429-style
// error is returned with tool name, limit category, and retry_after hint.
// This matches the "429 with retry_after hint" behaviour specified in ROADMAP.
func (rl *rateLimiter) wrap(toolName string, h server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, _ := req.Params.Arguments.(map[string]interface{})

		// Fast path: tool is not in any rate-limited set and the call has no
		// projects= arg — no rate limiting applies, no mutex acquired.
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
					"retry_after=%ds. Limits reset gradually (token bucket). "+
					"Configure rate_limits in synapses.json to adjust.",
				toolName, res.category, secs,
			)), nil
		}

		return h(ctx, req)
	}
}

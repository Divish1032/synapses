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
	defaultWriteOpsPerMinute        = 30
	defaultExpensiveReadsPerMinute  = 20
	defaultCrossProjectPerMinute    = 60
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

// fixedWindowBucket is a per-category fixed-window rate limiter.
// A fixed window is simpler than a token bucket and sufficient for these rates:
// agents that burst within 1 second will hit the limit on the 2nd minute's
// window, not mid-burst. This is acceptable for our threat model (defending
// against runaway loops, not precise traffic shaping).
type fixedWindowBucket struct {
	mu          sync.Mutex
	count       int
	windowStart time.Time
	limit       int // calls per minute; ≤0 means disabled
}

// allow returns (true, 0) when the call is within the limit, or (false,
// retryAfter) when the limit is exceeded. retryAfter is the time until the
// current window resets — agents should wait at least this long.
func (b *fixedWindowBucket) allow() (bool, time.Duration) {
	if b.limit <= 0 {
		return true, 0 // disabled
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Sub(b.windowStart) >= time.Minute {
		// Start a new 1-minute window.
		b.windowStart = now
		b.count = 0
	}
	if b.count >= b.limit {
		remaining := time.Minute - now.Sub(b.windowStart)
		if remaining < 0 {
			remaining = 0
		}
		return false, remaining
	}
	b.count++
	return true, 0
}

// sessionBuckets holds the three independent rate-limit buckets for one MCP
// connection (identified by its session key). Created lazily on first access.
type sessionBuckets struct {
	write        fixedWindowBucket
	expensiveRead fixedWindowBucket
	crossProject  fixedWindowBucket
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
// A field set to exactly -1 disables that category (limit ≤0 → always allow).
func newRateLimiter(cfg config.RateLimitConfig) *rateLimiter {
	rl := &rateLimiter{
		sessions:                make(map[string]*sessionBuckets),
		writeLimitPerMin:        defaultWriteOpsPerMinute,
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
			write:        fixedWindowBucket{limit: rl.writeLimitPerMin},
			expensiveRead: fixedWindowBucket{limit: rl.expensiveReadLimitPerMin},
			crossProject:  fixedWindowBucket{limit: rl.crossProjectLimitPerMin},
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

// check evaluates all applicable rate-limit categories for the given tool call.
// Returns (true, 0, "") when the call is allowed.
// Returns (false, retryAfter, category) when any category is exhausted.
//
// Categories checked:
//   - write: if toolName is in writeRateLimitedTools
//   - expensiveRead: if toolName is in expensiveReadTools
//   - crossProject: if args["projects"] is non-empty (any cross-project query)
func (rl *rateLimiter) check(sessionKey, toolName string, args map[string]interface{}) (allowed bool, retryAfter time.Duration, category string) {
	sb := rl.getOrCreate(sessionKey)

	if writeRateLimitedTools[toolName] {
		if ok, ra := sb.write.allow(); !ok {
			return false, ra, "write_ops"
		}
	}

	if expensiveReadTools[toolName] {
		if ok, ra := sb.expensiveRead.allow(); !ok {
			return false, ra, "expensive_reads"
		}
	}

	// Cross-project check: any non-empty projects= argument triggers this limit.
	if projects, _ := args["projects"].(string); projects != "" {
		if ok, ra := sb.crossProject.allow(); !ok {
			return false, ra, "cross_project"
		}
	}

	return true, 0, ""
}

// wrap returns a handler that applies rate limiting before calling h.
// Tools not in any rate-limited set are passed through unchanged.
// When a limit is exceeded, the handler returns a 429-style error with a
// retry_after hint — the underlying handler is NOT called.
func (rl *rateLimiter) wrap(toolName string, h server.ToolHandlerFunc) server.ToolHandlerFunc {
	// Fast path: if this tool is not subject to any rate limit and has no
	// cross-project capability, skip wrapping. This keeps the hot path for
	// read-only tools free of mutex overhead.
	isCrossProjectCapable := true // conservative: all tools may receive projects= arg
	isRateLimited := writeRateLimitedTools[toolName] || expensiveReadTools[toolName] || isCrossProjectCapable
	if !isRateLimited {
		return h
	}

	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, _ := req.Params.Arguments.(map[string]interface{})

		// Only tools in rate-limited sets or tools with projects= arg are checked.
		// For tools not in either set, check cross-project only if projects= is set.
		isWrite := writeRateLimitedTools[toolName]
		isExpensive := expensiveReadTools[toolName]
		projects, _ := args["projects"].(string)
		isCrossProject := projects != ""

		if !isWrite && !isExpensive && !isCrossProject {
			return h(ctx, req)
		}

		sessionKey := synapseSessionKey(SessionIDFromContext(ctx))
		allowed, retryAfter, category := rl.check(sessionKey, toolName, args)
		if !allowed {
			secs := int(retryAfter.Seconds()) + 1
			return mcp.NewToolResultError(fmt.Sprintf(
				"rate_limit_exceeded: tool %q has exceeded the %s rate limit for this session. "+
					"retry_after=%ds. The limit resets every minute. "+
					"If you need higher limits, configure rate_limits in synapses.json.",
				toolName, category, secs,
			)), nil
		}

		return h(ctx, req)
	}
}

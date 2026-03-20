package mcp

import (
	"context"
	"fmt"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// loopGuard detects agent loops by tracking per-session call fingerprints.
//
// A "fingerprint" is the tool name plus its primary argument (the first
// non-empty value found among the canonical arg keys: target, query, entity,
// symbol, file, name, title, id). Calls with no primary argument fingerprint
// as the tool name alone.
//
// Thresholds (evaluated over a sliding window of the last 20 calls):
//   - 3 identical fingerprints → warning appended to the successful result
//   - 5 identical fingerprints → circuit breaker: tool is NOT called, error returned
//
// The window resets on every file-change event (via Server.InvalidatePacketCacheForFile).
// This is domain-agnostic — it applies to all registered tools uniformly.
type loopGuard struct {
	mu       sync.Mutex
	sessions map[string]*loopGuardSession
}

func newLoopGuard() *loopGuard {
	return &loopGuard{
		sessions: make(map[string]*loopGuardSession),
	}
}

// loopGuardSession holds the sliding window for one MCP connection.
type loopGuardSession struct {
	window [loopGuardWindowSize]string
	head   int // next write index (wraps modulo loopGuardWindowSize)
	size   int // valid entries in window (0..loopGuardWindowSize)
}

const (
	loopGuardWindowSize   = 20 // sliding window length
	loopGuardWarnAt       = 3  // identical calls before warning
	loopGuardCircuitBreak = 5  // identical calls before hard rejection
)

// push records fp in the circular window and returns the count of occurrences
// of fp currently in the window (including the entry just added).
func (s *loopGuardSession) push(fp string) int {
	s.window[s.head] = fp
	s.head = (s.head + 1) % loopGuardWindowSize
	if s.size < loopGuardWindowSize {
		s.size++
	}
	count := 0
	for i := 0; i < s.size; i++ {
		if s.window[i] == fp {
			count++
		}
	}
	return count
}

// reset clears the sliding window (called on file-change events).
func (s *loopGuardSession) reset() {
	s.size = 0
	s.head = 0
}

// record records a fingerprint for the given session and returns its current
// count within the sliding window. Thread-safe.
func (g *loopGuard) record(sessionKey, fp string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	sess, ok := g.sessions[sessionKey]
	if !ok {
		sess = &loopGuardSession{}
		g.sessions[sessionKey] = sess
	}
	return sess.push(fp)
}

// resetAll resets all session windows. Called on any file-change event so that
// agents whose loop was caused by stale data get a clean slate immediately.
func (g *loopGuard) resetAll() {
	g.mu.Lock()
	for _, sess := range g.sessions {
		sess.reset()
	}
	g.mu.Unlock()
}

// clearSession removes the session entry entirely. Called when an MCP connection
// closes to prevent unbounded memory growth in long-running daemons.
func (g *loopGuard) clearSession(sessionKey string) {
	g.mu.Lock()
	delete(g.sessions, sessionKey)
	g.mu.Unlock()
}

// fingerprintCall builds a compact call identifier from a tool name and its
// arguments. Checks a fixed priority list of argument keys; uses the first
// non-empty string value found. Truncates the primary arg to 200 bytes.
func fingerprintCall(toolName string, args map[string]interface{}) string {
	for _, key := range []string{"target", "query", "entity", "symbol", "file", "name", "title", "id"} {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				if len(s) > 200 {
					s = s[:200]
				}
				return toolName + ":" + s
			}
		}
	}
	return toolName
}

// wrap returns a handler that applies loop-guard logic before and after h.
//
//   - Below loopGuardWarnAt: h runs unmodified.
//   - At loopGuardWarnAt..loopGuardCircuitBreak-1: h runs; warning is appended
//     to the first text content block in the result.
//   - At loopGuardCircuitBreak and above: h is NOT called; an error result is
//     returned immediately describing the loop and suggesting alternatives.
func (g *loopGuard) wrap(h server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionKey := synapseSessionKey(SessionIDFromContext(ctx))
		args, _ := req.Params.Arguments.(map[string]interface{})
		fp := fingerprintCall(req.Params.Name, args)
		count := g.record(sessionKey, fp)

		if count >= loopGuardCircuitBreak {
			return mcp.NewToolResultError(fmt.Sprintf(
				"[CIRCUIT BREAKER] Tool %q called %d times with identical arguments "+
					"in this session (fingerprint: %q). This pattern indicates an agent loop.\n\n"+
					"Suggestions:\n"+
					"  1. Re-read the result you already received — it may contain what you need.\n"+
					"  2. Try a different tool or a different argument.\n"+
					"  3. Call end_session() and reconsider your approach.\n\n"+
					"The loop counter resets automatically when a file change is detected.",
				req.Params.Name, count, fp,
			)), nil
		}

		result, err := h(ctx, req)
		if err != nil || result == nil {
			return result, err
		}

		if count >= loopGuardWarnAt && !result.IsError {
			warning := fmt.Sprintf(
				"\n\n---\n[LOOP WARNING] Tool %q has been called %d times with the same "+
					"arguments in this session. If you are not making progress, consider "+
					"a different tool or approach. Circuit breaker activates at %d identical calls.",
				req.Params.Name, count, loopGuardCircuitBreak,
			)
			appendWarningToResult(result, warning)
		}

		return result, nil
	}
}

// appendWarningToResult appends warning text to the first TextContent block in
// result.Content. If no TextContent block exists, the result is left unchanged.
func appendWarningToResult(result *mcp.CallToolResult, warning string) {
	for i, block := range result.Content {
		if tc, ok := block.(mcp.TextContent); ok {
			result.Content[i] = mcp.TextContent{
				Type: tc.Type,
				Text: tc.Text + warning,
			}
			return
		}
	}
}

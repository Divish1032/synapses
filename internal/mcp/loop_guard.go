package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/pulse"
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
	mu             sync.Mutex
	sessions       map[string]*loopGuardSession
	lastActivity   map[string]time.Time
	stopCh         chan struct{}
	wg             sync.WaitGroup
	pc             *pulse.Client // set via SetPulseClient; nil if pulse not configured
	projectID      string
	resolveSession func(string) string // P8-2: MCP session key → Synapses session UUID
}

func newLoopGuard() *loopGuard {
	g := &loopGuard{
		sessions:     make(map[string]*loopGuardSession),
		lastActivity: make(map[string]time.Time),
		stopCh:       make(chan struct{}),
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				g.gcIdleSessions(1 * time.Hour)
			case <-g.stopCh:
				return
			}
		}
	}()
	return g
}

// close signals the background GC goroutine to stop and waits briefly for
// it to exit. Under heavy load (e.g. 1000+ concurrent tests with -race) the
// goroutine may be CPU-starved; the 200ms cap prevents Close() from blocking
// indefinitely — the goroutine WILL exit once scheduled, it just won't hold
// up the caller. 200ms is generous for a goroutine that only reads a channel.
func (g *loopGuard) close() {
	close(g.stopCh)
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		// Goroutine will exit on its own once the runtime schedules it.
	}
}

// gcIdleSessions removes sessions that have been idle longer than maxIdle.
func (g *loopGuard) gcIdleSessions(maxIdle time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cutoff := time.Now().Add(-maxIdle)
	for key, last := range g.lastActivity {
		if last.Before(cutoff) {
			delete(g.sessions, key)
			delete(g.lastActivity, key)
		}
	}
}

// SetPulseClient wires a pulse client so loop-guard can emit guard events.
func (g *loopGuard) SetPulseClient(pc *pulse.Client) { g.pc = pc }

func (g *loopGuard) getPulseClient() *pulse.Client {
	return g.pc
}

// loopGuardSession holds the sliding window for one MCP connection.
type loopGuardSession struct {
	window   [loopGuardWindowSize]string
	head     int // next write index (wraps modulo loopGuardWindowSize)
	size     int // valid entries in window (0..loopGuardWindowSize)
	lastFP   string    // last fingerprint seen — used for auto-reset on change
	lastCall time.Time // last call time — used for time-based decay
}

const (
	loopGuardWindowSize   = 30 // sliding window length (enough for cycles up to length 10 repeated 3x)
	loopGuardWarnAt       = 3  // cycle repetitions before warning
	loopGuardCircuitBreak = 5  // cycle repetitions before hard rejection
)

// push records fp in the circular window and returns the maximum number of
// consecutive suffix cycle repetitions detected. This catches:
//   - Same call repeated (A,A,A,A,A) → cycle len 1, reps 5
//   - Alternating calls (A,B,A,B,A,B) → cycle len 2, reps 3
//   - Short cycles (A,B,C,A,B,C,A,B,C) → cycle len 3, reps 3
func (s *loopGuardSession) push(fp string) int {
	s.window[s.head] = fp
	s.head = (s.head + 1) % loopGuardWindowSize
	if s.size < loopGuardWindowSize {
		s.size++
	}
	return s.detectCycleRepetitions()
}

// detectCycleRepetitions checks for repeating suffix patterns of length 1..maxCycleLen.
// Returns the highest repetition count found across all cycle lengths.
func (s *loopGuardSession) detectCycleRepetitions() int {
	if s.size == 0 {
		return 0
	}

	// Linearize the circular buffer into a flat slice for easier indexing.
	flat := make([]string, s.size)
	for i := 0; i < s.size; i++ {
		idx := (s.head - s.size + i + loopGuardWindowSize) % loopGuardWindowSize
		flat[i] = s.window[idx]
	}

	maxReps := 1
	// Check cycles up to length W/2 (need at least 2 reps to detect).
	// The warn/circuit-break thresholds handle whether it's actionable.
	maxCycleLen := s.size / 2
	if maxCycleLen > 10 {
		maxCycleLen = 10 // cap to keep O(W*maxCycleLen) bounded
	}

	for cycleLen := 1; cycleLen <= maxCycleLen; cycleLen++ {
		// Extract the candidate pattern = last cycleLen elements.
		patternStart := len(flat) - cycleLen
		reps := 1

		// Count how many times this pattern repeats backwards.
		for pos := patternStart - cycleLen; pos >= 0; pos -= cycleLen {
			match := true
			for j := 0; j < cycleLen; j++ {
				if flat[pos+j] != flat[patternStart+j] {
					match = false
					break
				}
			}
			if !match {
				break
			}
			reps++
		}

		if reps > maxReps {
			maxReps = reps
		}
	}

	return maxReps
}

// reset clears the sliding window (called on file-change events).
func (s *loopGuardSession) reset() {
	s.size = 0
	s.head = 0
}

// record records a fingerprint for the given session and returns its current
// count within the sliding window. Thread-safe.
//
// Auto-resets the window when:
//   - The fingerprint differs from the last one (agent made progress)
//   - More than 60 seconds have elapsed since the last call (agent paused)
func (g *loopGuard) record(sessionKey, fp string) int {
	// REST calls create unique "rest-N" session IDs that are never cleaned up.
	// Don't track them to prevent unbounded memory growth.
	if strings.HasPrefix(sessionKey, "rest-") {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	sess, ok := g.sessions[sessionKey]
	if !ok {
		sess = &loopGuardSession{}
		g.sessions[sessionKey] = sess
	}

	now := time.Now()

	// Time-based decay: if the agent hasn't called in > 60s, clear the window.
	if !sess.lastCall.IsZero() && now.Sub(sess.lastCall) > 60*time.Second {
		sess.reset()
	}

	// Auto-reset on fingerprint change: the agent tried something different,
	// which is evidence of progress. Clear the loop counter.
	if sess.lastFP != "" && sess.lastFP != fp {
		sess.reset()
	}

	sess.lastFP = fp
	sess.lastCall = now
	g.lastActivity[sessionKey] = now
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
	delete(g.lastActivity, sessionKey)
	g.mu.Unlock()
}

// loopGuardExempt lists tools that must never be circuit-broken. These are
// session-lifecycle tools whose availability is unconditional:
//   - session_init: agents may call this on reconnection or startup — blocking
//     it strands the agent permanently.
//   - end_session: cleanup must always succeed — blocking it means the agent
//     can never terminate its session, and the circuit-breaker error message
//     itself suggests calling end_session, which would be contradictory.
var loopGuardExempt = map[string]bool{
	"session_init": true,
	"end_session":  true,
}

// fingerprintCall builds a compact call identifier from a tool name and its
// arguments. Checks a fixed priority list of argument keys; uses the first
// non-empty string value found. Truncates the primary arg to 200 bytes at a
// valid UTF-8 rune boundary.
func fingerprintCall(toolName string, args map[string]interface{}) string {
	for _, key := range []string{"target", "query", "entity", "symbol", "file", "name", "title", "id"} {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return toolName + ":" + truncateUTF8(s, 200)
			}
		}
	}
	return toolName
}

// wrap returns a handler that applies loop-guard logic before and after h.
//
//   - Exempt tools (session_init, end_session): always pass through unchanged.
//   - Below loopGuardWarnAt: h runs unmodified.
//   - At loopGuardWarnAt..loopGuardCircuitBreak-1: h runs; warning is appended
//     to the result so the agent knows to change approach.
//   - At loopGuardCircuitBreak and above: h is NOT called; an error result is
//     returned immediately describing the loop and suggesting alternatives.
func (g *loopGuard) wrap(h server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Lifecycle tools are unconditionally exempt — see loopGuardExempt.
		if loopGuardExempt[req.Params.Name] {
			return h(ctx, req)
		}

		sessionKey := synapseSessionKey(SessionIDFromContext(ctx))
		args, _ := req.Params.Arguments.(map[string]interface{})
		fp := fingerprintCall(req.Params.Name, args)
		count := g.record(sessionKey, fp)

		if count >= loopGuardCircuitBreak {
			if pc := g.getPulseClient(); pc != nil {
				sessID := ""
				if g.resolveSession != nil {
					sessID = g.resolveSession(SessionIDFromContext(ctx))
				}
				pc.RecordGuardEvent(pulse.GuardEvent{
					GuardType: "loop_circuit_break",
					ToolName:  req.Params.Name,
					AgentID:   SessionIDFromContext(ctx),
					ProjectID: g.projectID,
					SessionID: sessID,
				})
			}
			return mcp.NewToolResultError(fmt.Sprintf(
				"[CIRCUIT BREAKER] Tool %q called %d times with identical arguments "+
					"in this session (fingerprint: %q). This pattern indicates an agent loop.\n\n"+
					"Suggestions:\n"+
					"  1. Re-read the result you already received — it may contain what you need.\n"+
					"  2. Try a different tool or a different argument.\n"+
					"  3. Stop and reconsider your approach before continuing.\n\n"+
					"The loop counter resets automatically when a file change is detected.",
				req.Params.Name, count, fp,
			)), nil
		}

		result, err := h(ctx, req)
		if err != nil || result == nil {
			return result, err
		}

		if count >= loopGuardWarnAt && !result.IsError {
			if pc := g.getPulseClient(); pc != nil {
				sessID := ""
				if g.resolveSession != nil {
					sessID = g.resolveSession(SessionIDFromContext(ctx))
				}
				pc.RecordGuardEvent(pulse.GuardEvent{
					GuardType: "loop_warning",
					ToolName:  req.Params.Name,
					AgentID:   SessionIDFromContext(ctx),
					ProjectID: g.projectID,
					SessionID: sessID,
				})
			}
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
// result.Content. All fields of the TextContent value (including Annotations)
// are preserved — only the Text field is extended. If no TextContent block
// exists (e.g. the tool returned only ImageContent), a new TextContent block
// is appended so the warning is never silently dropped.
func appendWarningToResult(result *mcp.CallToolResult, warning string) {
	for i, block := range result.Content {
		if tc, ok := block.(mcp.TextContent); ok {
			// Copy the full value to preserve all fields (e.g. Annotations),
			// then extend Text in place.
			tc.Text += warning
			result.Content[i] = tc
			return
		}
	}
	// Fallback: no TextContent block found — append a dedicated warning block
	// so the warning is never silently lost.
	result.Content = append(result.Content, mcp.TextContent{
		Type: "text",
		Text: warning,
	})
}

package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// ── fingerprintCall ────────────────────────────────────────────────────────────

func TestFingerprintCall_primaryArgPriority(t *testing.T) {
	// "target" takes priority over all other keys.
	fp := fingerprintCall("my_tool", map[string]interface{}{
		"target": "AuthService",
		"query":  "auth",
		"entity": "Login",
	})
	if fp != "my_tool:AuthService" {
		t.Errorf("got %q, want %q", fp, "my_tool:AuthService")
	}
}

func TestFingerprintCall_fallsThrough(t *testing.T) {
	// "entity" used when target and query are absent.
	fp := fingerprintCall("get_context", map[string]interface{}{
		"entity": "LoginHandler",
	})
	if fp != "get_context:LoginHandler" {
		t.Errorf("got %q, want %q", fp, "get_context:LoginHandler")
	}
}

func TestFingerprintCall_noArgs(t *testing.T) {
	// Falls back to tool name alone when no recognised arg is present.
	fp := fingerprintCall("find_entity", nil)
	if fp != "find_entity" {
		t.Errorf("got %q, want %q", fp, "find_entity")
	}
}

func TestFingerprintCall_emptyArgValue(t *testing.T) {
	// Empty string value for a recognised key should be skipped.
	fp := fingerprintCall("search", map[string]interface{}{
		"query": "",
		"name":  "Foo",
	})
	if fp != "search:Foo" {
		t.Errorf("got %q, want %q", fp, "search:Foo")
	}
}

func TestFingerprintCall_longArgTruncated(t *testing.T) {
	long := strings.Repeat("x", 300)
	fp := fingerprintCall("recall", map[string]interface{}{"query": long})
	if len(fp) > len("recall:")+200 {
		t.Errorf("fingerprint not truncated: len=%d", len(fp))
	}
}

func TestFingerprintCall_utf8SafeTruncation(t *testing.T) {
	// 3-byte UTF-8 rune: '€' = 0xE2 0x82 0xAC. Build a string that is exactly
	// 201 bytes long ending mid-rune so a naive byte slice would split it.
	// truncateUTF8 must walk back to the last complete rune.
	rune3 := "€"                          // 3 bytes
	s := strings.Repeat("a", 199) + rune3 // 202 bytes; rune starts at byte 199
	fp := fingerprintCall("tool", map[string]interface{}{"query": s})
	suffix := strings.TrimPrefix(fp, "tool:")
	if !strings.HasSuffix(suffix, "a") {
		// The 3-byte rune should have been dropped; last char must be 'a'
		t.Errorf("UTF-8 truncation unsafe: suffix ends with %q", suffix[len(suffix)-4:])
	}
	if len(suffix) > 200 {
		t.Errorf("truncated suffix still exceeds 200 bytes: len=%d", len(suffix))
	}
}

// ── loopGuardSession window behaviour ─────────────────────────────────────────

func TestLoopGuardSession_push(t *testing.T) {
	var s loopGuardSession

	// First call: cycle of len 1 repeated 1 time.
	if n := s.push("a"); n != 1 {
		t.Fatalf("call 1: want 1, got %d", n)
	}
	// Same call again: cycle [a] repeated 2 times.
	if n := s.push("a"); n != 2 {
		t.Fatalf("call 2: want 2, got %d", n)
	}
	// Different call breaks the cycle: [a,a,b] → no cycle > 1 rep.
	if n := s.push("b"); n != 1 {
		t.Fatalf("call 3: want 1, got %d", n)
	}
	// Alternating pattern: [a,a,b,a] → no clear cycle yet.
	s.push("a")
	// Now add b: [a,a,b,a,b] → cycle [a,b] repeated 2 times.
	if n := s.push("b"); n != 2 {
		t.Fatalf("call 5 (a,b cycle): want 2, got %d", n)
	}
	// Continue the alternation: [a,a,b,a,b,a] → cycle [b,a] repeated 2 or [a,b,a] check...
	// Let's just verify the core: 5 identical calls = 5 repetitions.
	s.reset()
	for i := 0; i < 5; i++ {
		s.push("x")
	}
	if n := s.push("x"); n != 6 {
		t.Fatalf("6th identical: want 6, got %d", n)
	}
}

func TestLoopGuardSession_cycleDetection(t *testing.T) {
	// Test that alternating patterns are detected as cycles.
	t.Run("alternating_AB", func(t *testing.T) {
		var s loopGuardSession
		// Build A-B-A-B-A-B pattern
		for i := 0; i < 3; i++ {
			s.push("a")
			s.push("b")
		}
		// Window: [a,b,a,b,a,b] → cycle [a,b] repeated 3 times
		// Last push("b") should detect this.
		// Re-check by pushing one more:
		n := s.push("a")
		// Window: [a,b,a,b,a,b,a] → cycle [b,a] repeated 3 times
		if n < 3 {
			t.Errorf("alternating A-B: want >=3 reps, got %d", n)
		}
	})

	t.Run("triple_cycle_ABC", func(t *testing.T) {
		var s loopGuardSession
		// Build A-B-C-A-B-C-A-B-C pattern
		for i := 0; i < 3; i++ {
			s.push("a")
			s.push("b")
			s.push("c")
		}
		// Window: [a,b,c,a,b,c,a,b,c] → cycle [a,b,c] repeated 3 times
		if n := s.detectCycleRepetitions(); n < 3 {
			t.Errorf("triple cycle A-B-C: want >=3 reps, got %d", n)
		}
	})

	t.Run("no_cycle", func(t *testing.T) {
		var s loopGuardSession
		// Diverse calls: no cycle
		for _, fp := range []string{"a", "b", "c", "d", "e", "f"} {
			s.push(fp)
		}
		if n := s.detectCycleRepetitions(); n > 1 {
			t.Errorf("diverse calls should not detect cycle: got %d reps", n)
		}
	})
}

func TestLoopGuardSession_windowEviction(t *testing.T) {
	var s loopGuardSession

	// Fill the window with 20 distinct "other" entries to push "a" out.
	s.push("a")
	for i := 0; i < loopGuardWindowSize; i++ {
		s.push("other")
	}
	// Now the window is full of "other"; "a" should have been evicted.
	// Next push of "a" should return 1, not 2.
	if n := s.push("a"); n != 1 {
		t.Errorf("after full window rotation, want count=1 for 'a', got %d", n)
	}
}

func TestLoopGuardSession_reset(t *testing.T) {
	var s loopGuardSession
	for i := 0; i < 3; i++ {
		s.push("loop")
	}
	s.reset()
	if n := s.push("loop"); n != 1 {
		t.Errorf("after reset, want count=1, got %d", n)
	}
}

// ── loopGuard struct ──────────────────────────────────────────────────────────

func TestLoopGuard_sessionIsolation(t *testing.T) {
	g := newLoopGuard()
	// Session A calls "x" three times.
	for i := 0; i < 3; i++ {
		g.record("session-a", "x")
	}
	// Session B should see count 1 for "x" (not inheriting A's count).
	if n := g.record("session-b", "x"); n != 1 {
		t.Errorf("session-b should be isolated: want 1, got %d", n)
	}
}

func TestLoopGuard_resetAll(t *testing.T) {
	g := newLoopGuard()
	for i := 0; i < 4; i++ {
		g.record("sess", "fp")
	}
	g.resetAll()
	if n := g.record("sess", "fp"); n != 1 {
		t.Errorf("after resetAll, want count=1, got %d", n)
	}
}

func TestLoopGuard_clearSession(t *testing.T) {
	g := newLoopGuard()
	for i := 0; i < 4; i++ {
		g.record("sess", "fp")
	}
	g.clearSession("sess")
	// After clear, next record starts fresh.
	if n := g.record("sess", "fp"); n != 1 {
		t.Errorf("after clearSession, want count=1, got %d", n)
	}
	// The map entry should have been deleted and re-created.
	g.mu.Lock()
	_, exists := g.sessions["sess"]
	g.mu.Unlock()
	if !exists {
		t.Error("session entry should be recreated on next record")
	}
}

// ── wrap: behaviour at each threshold ─────────────────────────────────────────

// makeMockHandler returns a handler that always succeeds with the given text.
func makeMockHandler(text string) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(text), nil
	}
}

func TestLoopGuard_belowWarningThreshold(t *testing.T) {
	g := newLoopGuard()
	wrapped := g.wrap(makeMockHandler("ok"))

	// 2 identical calls: no warning.
	for i := 0; i < loopGuardWarnAt-1; i++ {
		req := mcp.CallToolRequest{}
		req.Params.Name = "search"
		req.Params.Arguments = map[string]interface{}{"query": "auth"}
		result, err := wrapped(context.Background(), req)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i+1, err)
		}
		if result.IsError {
			t.Fatalf("call %d: unexpected error result", i+1)
		}
		tc := result.Content[0].(mcp.TextContent)
		if strings.Contains(tc.Text, "LOOP WARNING") {
			t.Errorf("call %d: unexpected LOOP WARNING at count < %d", i+1, loopGuardWarnAt)
		}
	}
}

func TestLoopGuard_warningAtThreshold(t *testing.T) {
	g := newLoopGuard()
	wrapped := g.wrap(makeMockHandler("result text"))

	req := mcp.CallToolRequest{}
	req.Params.Name = "recall"
	req.Params.Arguments = map[string]interface{}{"query": "auth caching"}

	var lastResult *mcp.CallToolResult
	for i := 0; i < loopGuardWarnAt; i++ {
		var err error
		lastResult, err = wrapped(context.Background(), req)
		if err != nil {
			t.Fatalf("call %d: error: %v", i+1, err)
		}
	}
	// The loopGuardWarnAt-th call should carry a warning.
	if lastResult.IsError {
		t.Fatal("warning call should not be an error result")
	}
	tc := lastResult.Content[0].(mcp.TextContent)
	if !strings.Contains(tc.Text, "LOOP WARNING") {
		t.Errorf("expected LOOP WARNING in call %d, got: %s", loopGuardWarnAt, tc.Text)
	}
}

func TestLoopGuard_circuitBreaker(t *testing.T) {
	// SECURITY VERIFICATION: proves the attack vector (agent loop) is closed.
	//
	// Without the loop guard, a malfunctioning or compromised agent could call
	// the same tool indefinitely, hammering the SQLite store and starving other
	// agents. This test proves the circuit breaker fires at exactly
	// loopGuardCircuitBreak identical calls and rejects without executing the tool.
	callCount := 0
	counting := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		callCount++
		return mcp.NewToolResultText("executed"), nil
	}
	g := newLoopGuard()
	wrapped := g.wrap(counting)

	req := mcp.CallToolRequest{}
	req.Params.Name = "get_context"
	req.Params.Arguments = map[string]interface{}{"entity": "AuthService"}

	var lastResult *mcp.CallToolResult
	for i := 0; i < loopGuardCircuitBreak; i++ {
		var err error
		lastResult, err = wrapped(context.Background(), req)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i+1, err)
		}
	}

	// The 5th call should trip the circuit breaker.
	if !lastResult.IsError {
		t.Fatal("circuit breaker must return an error result on the 5th identical call")
	}
	tc := lastResult.Content[0].(mcp.TextContent)
	if !strings.Contains(tc.Text, "CIRCUIT BREAKER") {
		t.Errorf("expected CIRCUIT BREAKER message, got: %s", tc.Text)
	}

	// Tool must NOT have been executed on the circuit-breaking call.
	if callCount >= loopGuardCircuitBreak {
		t.Errorf("tool was executed %d times; should stop at %d", callCount, loopGuardCircuitBreak-1)
	}
}

func TestLoopGuard_circuitBreakerDoesNotExecute(t *testing.T) {
	// Complementary check: after circuit breaker trips, tool is never called again.
	executed := false
	handler := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		executed = true
		return mcp.NewToolResultText("executed"), nil
	}
	g := newLoopGuard()
	wrapped := g.wrap(handler)

	req := mcp.CallToolRequest{}
	req.Params.Name = "remember"
	req.Params.Arguments = map[string]interface{}{"query": "same query"}

	// Prime the counter to threshold.
	for i := 0; i < loopGuardCircuitBreak; i++ {
		wrapped(context.Background(), req) //nolint:errcheck // return values not needed here
	}

	// This call is above the threshold — tool must not run.
	executed = false
	result, _ := wrapped(context.Background(), req)
	if executed {
		t.Error("handler should NOT be called when circuit breaker is tripped")
	}
	if !result.IsError {
		t.Error("result must be an error when circuit breaker is tripped")
	}
}

func TestLoopGuard_differentArgsNotCounted(t *testing.T) {
	g := newLoopGuard()
	wrapped := g.wrap(makeMockHandler("ok"))

	// Call with 5 different argument values — each is a distinct fingerprint.
	for i := 0; i < loopGuardCircuitBreak; i++ {
		req := mcp.CallToolRequest{}
		req.Params.Name = "search"
		req.Params.Arguments = map[string]interface{}{"query": strings.Repeat("q", i+1)}
		result, err := wrapped(context.Background(), req)
		if err != nil {
			t.Fatalf("call %d: error: %v", i+1, err)
		}
		if result.IsError {
			t.Errorf("call %d with distinct arg should not trip circuit breaker", i+1)
		}
	}
}

func TestLoopGuard_resetOnFileChange(t *testing.T) {
	g := newLoopGuard()
	wrapped := g.wrap(makeMockHandler("ok"))

	req := mcp.CallToolRequest{}
	req.Params.Name = "get_context"
	req.Params.Arguments = map[string]interface{}{"entity": "Service"}

	// Drive to one below circuit breaker.
	for i := 0; i < loopGuardCircuitBreak-1; i++ {
		wrapped(context.Background(), req) //nolint:errcheck
	}

	// Simulate file change → reset.
	g.resetAll()

	// Now the same call should succeed (counter reset to 1, well below threshold).
	result, err := wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("after reset: error: %v", err)
	}
	if result.IsError {
		t.Error("after reset, first identical call should succeed — loop guard reset not working")
	}
	tc := result.Content[0].(mcp.TextContent)
	if strings.Contains(tc.Text, "LOOP WARNING") {
		t.Error("after reset, should not carry warning on first call")
	}
}

// ── Integration: loop guard is active on all registered tools ─────────────────

func TestLoopGuard_integratedWithServer(t *testing.T) {
	// This test proves that the loop guard is wired into the server's dispatch
	// table — calling any tool (e.g. search) enough times trips the breaker.
	s := newTestServer(t)

	ctx := context.Background()
	req := mcp.CallToolRequest{}
	req.Params.Name = "search"
	req.Params.Arguments = map[string]interface{}{"mode": "exact"}

	var lastResult *mcp.CallToolResult
	for i := 0; i < loopGuardCircuitBreak; i++ {
		var err error
		lastResult, err = s.DispatchTool(ctx, "search", map[string]interface{}{"mode": "exact"})
		if err != nil {
			t.Fatalf("call %d: error: %v", i+1, err)
		}
		_ = req
	}

	if !lastResult.IsError {
		t.Fatal("dispatch: circuit breaker must trip after loopGuardCircuitBreak identical calls")
	}
	tc := lastResult.Content[0].(mcp.TextContent)
	if !strings.Contains(tc.Text, "CIRCUIT BREAKER") {
		t.Errorf("dispatch: expected CIRCUIT BREAKER, got: %s", tc.Text)
	}
}

func TestLoopGuard_resetOnFingerprintChange(t *testing.T) {
	s := newTestServer(t)

	// Drive search(mode=exact) close to the circuit breaker.
	args := map[string]interface{}{"query": "test", "mode": "exact"}
	for i := 0; i < loopGuardCircuitBreak-1; i++ {
		s.DispatchTool(context.Background(), "search", args) //nolint:errcheck
	}

	// Call a DIFFERENT tool — this changes the fingerprint, which auto-resets
	// the loop guard window (proving the agent made progress).
	// Sprint 23.9: get_file_context removed; use get_context as fingerprint-change tool.
	s.DispatchTool(context.Background(), "get_context", map[string]interface{}{"entity": "test"}) //nolint:errcheck

	// After fingerprint change, the original call should succeed.
	result, err := s.DispatchTool(context.Background(), "search", args)
	if err != nil {
		t.Fatalf("after fingerprint change: error: %v", err)
	}
	if result.IsError {
		t.Error("after fingerprint change, tool should not be circuit-broken")
	}
}

// ── Bug-fix regression tests ──────────────────────────────────────────────────

// Bug 1: session_init and end_session must be unconditionally exempt.
// A circuit-broken end_session means agents can never clean up. A circuit-broken
// session_init means agents can never reconnect after an error-recovery loop.
func TestLoopGuard_exemptToolsNeverBlocked(t *testing.T) {
	g := newLoopGuard()

	for _, exemptTool := range []string{"session_init", "end_session"} {
		callCount := 0
		handler := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			callCount++
			return mcp.NewToolResultText("ok"), nil
		}
		wrapped := g.wrap(handler)

		req := mcp.CallToolRequest{}
		req.Params.Name = exemptTool
		req.Params.Arguments = map[string]interface{}{}

		// Call well past the circuit breaker threshold.
		calls := loopGuardCircuitBreak + 5
		for i := 0; i < calls; i++ {
			result, err := wrapped(context.Background(), req)
			if err != nil {
				t.Fatalf("%s call %d: error: %v", exemptTool, i+1, err)
			}
			if result.IsError {
				t.Errorf("%s call %d: exempt tool was circuit-broken (must NEVER happen)", exemptTool, i+1)
			}
		}
		if callCount != calls {
			t.Errorf("%s: handler called %d times, want %d (circuit breaker fired on exempt tool)", exemptTool, callCount, calls)
		}
	}
}

// Bug 1 (integration): session_init is exempt on the real server dispatch table.
func TestLoopGuard_exemptToolsIntegrated(t *testing.T) {
	s := newTestServer(t)

	// Call session_init more than the circuit breaker threshold.
	for i := 0; i < loopGuardCircuitBreak+2; i++ {
		result, err := s.DispatchTool(context.Background(), "session_init", map[string]interface{}{
			"agent_id": "test-agent",
		})
		if err != nil {
			t.Fatalf("session_init call %d: error: %v", i+1, err)
		}
		if result != nil && result.IsError {
			tc, _ := result.Content[0].(mcp.TextContent)
			if strings.Contains(tc.Text, "CIRCUIT BREAKER") {
				t.Fatalf("session_init call %d: circuit breaker fired on exempt tool — this is a critical production bug", i+1)
			}
		}
	}
}

// Bug 2: appendWarningToResult must preserve the embedded Annotated field
// (and therefore Annotations) on TextContent — not just Type and Text.
func TestAppendWarningToResult_preservesAnnotations(t *testing.T) {
	// Build a TextContent with non-nil Annotations via the embedded Annotated field.
	ann := &mcp.Annotations{Audience: []mcp.Role{mcp.RoleUser}}
	annotated := mcp.TextContent{
		Annotated: mcp.Annotated{Annotations: ann},
		Type:      "text",
		Text:      "original",
	}
	result := &mcp.CallToolResult{
		Content: []mcp.Content{annotated},
	}
	appendWarningToResult(result, " [warn]")

	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatal("Content[0] is not TextContent after append")
	}
	if !strings.HasSuffix(tc.Text, " [warn]") {
		t.Errorf("warning not appended: got %q", tc.Text)
	}
	if tc.Annotations == nil {
		t.Error("Annotations were dropped by appendWarningToResult — must be preserved")
	}
	if len(tc.Annotations.Audience) != 1 || tc.Annotations.Audience[0] != mcp.RoleUser {
		t.Errorf("Annotations.Audience corrupted: %v", tc.Annotations.Audience)
	}
}

// Bug 3: warning must not be silently dropped when result has no TextContent.
// Some tools may return only ImageContent or EmbeddedResource blocks.
func TestAppendWarningToResult_fallbackWhenNoTextContent(t *testing.T) {
	// Result with no TextContent — only an embedded resource block.
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.EmbeddedResource{
				Type: "resource",
				Resource: mcp.TextResourceContents{
					URI:      "synapses://active-context",
					MIMEType: "text/plain",
					Text:     "some resource",
				},
			},
		},
	}
	appendWarningToResult(result, " [warn]")

	if len(result.Content) < 2 {
		t.Fatal("fallback TextContent block was not appended when no TextContent existed")
	}
	// The second block should be the warning TextContent.
	tc, ok := result.Content[1].(mcp.TextContent)
	if !ok {
		t.Fatalf("fallback block is not TextContent, got %T", result.Content[1])
	}
	if !strings.Contains(tc.Text, "[warn]") {
		t.Errorf("fallback block missing warning text: %q", tc.Text)
	}
	// Original EmbeddedResource must be untouched.
	if _, ok := result.Content[0].(mcp.EmbeddedResource); !ok {
		t.Error("original EmbeddedResource was replaced rather than left untouched")
	}
}

// Bug 3: empty Content slice also gets a fallback warning block.
func TestAppendWarningToResult_emptyContent(t *testing.T) {
	result := &mcp.CallToolResult{Content: nil}
	appendWarningToResult(result, " [warn]")
	if len(result.Content) != 1 {
		t.Fatalf("expected 1 content block after fallback, got %d", len(result.Content))
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("fallback is not TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(tc.Text, "[warn]") {
		t.Errorf("fallback missing warning: %q", tc.Text)
	}
}

// Bug 4 (UTF-8 safety): already covered by TestFingerprintCall_utf8SafeTruncation above.
// This test verifies the circuit-breaker error message no longer references
// end_session (which would be contradictory since end_session is now exempt).
func TestLoopGuard_circuitBreakerMessageConsistency(t *testing.T) {
	g := newLoopGuard()
	wrapped := g.wrap(makeMockHandler("ok"))

	req := mcp.CallToolRequest{}
	req.Params.Name = "recall"
	req.Params.Arguments = map[string]interface{}{"query": "stuck query"}

	var result *mcp.CallToolResult
	for i := 0; i < loopGuardCircuitBreak; i++ {
		var err error
		result, err = wrapped(context.Background(), req)
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if !result.IsError {
		t.Fatal("expected circuit breaker error")
	}
	tc := result.Content[0].(mcp.TextContent)
	// The message must NOT suggest calling end_session because that tool
	// itself was previously guarded (the bug). It now says "reconsider".
	if strings.Contains(tc.Text, "Call end_session()") {
		t.Error("circuit breaker message should not reference end_session() as a recovery action — end_session was previously guarded itself")
	}
	if !strings.Contains(tc.Text, "CIRCUIT BREAKER") {
		t.Errorf("missing CIRCUIT BREAKER header: %s", tc.Text)
	}
}

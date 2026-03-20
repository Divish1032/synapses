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
	fp := fingerprintCall("discover_tools", nil)
	if fp != "discover_tools" {
		t.Errorf("got %q, want %q", fp, "discover_tools")
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

// ── loopGuardSession window behaviour ─────────────────────────────────────────

func TestLoopGuardSession_push(t *testing.T) {
	var s loopGuardSession

	// First call: count 1.
	if n := s.push("a"); n != 1 {
		t.Fatalf("call 1: want 1, got %d", n)
	}
	// Second distinct call: count for "b" is 1.
	s.push("b")
	// Third call to "a": count 2.
	if n := s.push("a"); n != 2 {
		t.Fatalf("call 3 (2nd 'a'): want 2, got %d", n)
	}
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
	// table — calling any tool (e.g. discover_tools) enough times trips the breaker.
	s := newTestServer(t)

	ctx := context.Background()
	req := mcp.CallToolRequest{}
	req.Params.Name = "discover_tools"
	req.Params.Arguments = map[string]interface{}{}

	var lastResult *mcp.CallToolResult
	for i := 0; i < loopGuardCircuitBreak; i++ {
		var err error
		lastResult, err = s.DispatchTool(ctx, "discover_tools", map[string]interface{}{})
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

func TestLoopGuard_integratedResetOnFileChange(t *testing.T) {
	s := newTestServer(t)

	// Drive discover_tools close to the circuit breaker.
	for i := 0; i < loopGuardCircuitBreak-1; i++ {
		s.DispatchTool(context.Background(), "discover_tools", map[string]interface{}{}) //nolint:errcheck
	}

	// Simulate file change via InvalidatePacketCacheForFile.
	s.InvalidatePacketCacheForFile("")

	// After reset, the same call should succeed.
	result, err := s.DispatchTool(context.Background(), "discover_tools", map[string]interface{}{})
	if err != nil {
		t.Fatalf("after file change reset: error: %v", err)
	}
	if result.IsError {
		t.Error("after file change reset, tool should not be circuit-broken")
	}
}

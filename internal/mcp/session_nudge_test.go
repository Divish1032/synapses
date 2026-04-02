package mcp

// session_nudge_test.go — Sprint 24.7/24.8: token budget awareness & save nudge.

import (
	"encoding/json"
	"strings"
	"testing"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── helpers ────────────────────────────────────────────────────────────────

// newNudgeServer creates a test server with nudge config set.
// nudgeThreshold=0 disables count-based nudge; budgetPct=0 disables token nudge.
func newNudgeServer(t *testing.T, nudgeThreshold int, budgetPct float64) *Server {
	t.Helper()
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg := &config.Config{
		Session: config.SessionConfig{
			NudgeThreshold: nudgeThreshold,
			TokenBudgetPct: budgetPct,
		},
	}
	s := New(g, cfg, st)
	s.StartBackground()
	t.Cleanup(func() { s.Close() })
	return s
}

// nudgeFromResult extracts the memory_nudge field from a JSON tool result.
// Returns "" if not present.
func nudgeFromResult(result *mcp.CallToolResult) string {
	if result == nil || result.IsError || len(result.Content) == 0 {
		return ""
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &obj); err != nil {
		return ""
	}
	msg, _ := obj["memory_nudge"].(string)
	return msg
}

// makeJSONResult returns a successful *mcp.CallToolResult with a JSON object body.
func makeJSONResult(payload map[string]any) *mcp.CallToolResult {
	b, _ := json.Marshal(payload)
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: string(b)}},
	}
}

// ── modelContextWindow ─────────────────────────────────────────────────────

func TestModelContextWindow_KnownModel(t *testing.T) {
	if got := modelContextWindow("claude-sonnet-4-6"); got != 200000 {
		t.Errorf("claude-sonnet-4-6: want 200000, got %d", got)
	}
	if got := modelContextWindow("gpt-4"); got != 8192 {
		t.Errorf("gpt-4: want 8192, got %d", got)
	}
	if got := modelContextWindow("gemini-2.0-flash"); got != 1000000 {
		t.Errorf("gemini-2.0-flash: want 1000000, got %d", got)
	}
}

func TestModelContextWindow_UnknownModel(t *testing.T) {
	if got := modelContextWindow("unknown-model-xyz"); got != 0 {
		t.Errorf("unknown model: want 0, got %d", got)
	}
	if got := modelContextWindow(""); got != 0 {
		t.Errorf("empty model: want 0, got %d", got)
	}
}

// ── injectNudgeIntoResult ──────────────────────────────────────────────────

func TestInjectNudgeIntoResult_JSON(t *testing.T) {
	result := makeJSONResult(map[string]any{"status": "ok"})
	injectNudgeIntoResult(result, "save your work")
	if got := nudgeFromResult(result); got != "save your work" {
		t.Errorf("want nudge injected, got %q", got)
	}
}

func TestInjectNudgeIntoResult_EmptyMsg(t *testing.T) {
	result := makeJSONResult(map[string]any{"status": "ok"})
	injectNudgeIntoResult(result, "")
	if got := nudgeFromResult(result); got != "" {
		t.Errorf("empty msg: want no nudge, got %q", got)
	}
}

func TestInjectNudgeIntoResult_ErrorResult(t *testing.T) {
	result := &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "error occurred"}},
	}
	injectNudgeIntoResult(result, "save your work")
	// Error results must not be modified.
	tc := result.Content[0].(mcp.TextContent)
	if tc.Text != "error occurred" {
		t.Errorf("error result was modified: %q", tc.Text)
	}
}

func TestInjectNudgeIntoResult_PlainText(t *testing.T) {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "plain text, not JSON"}},
	}
	injectNudgeIntoResult(result, "save your work")
	// Plain-text content must not be modified.
	tc := result.Content[0].(mcp.TextContent)
	if tc.Text != "plain text, not JSON" {
		t.Errorf("plain text result was modified: %q", tc.Text)
	}
}

func TestInjectNudgeIntoResult_NilResult(t *testing.T) {
	// Must not panic.
	injectNudgeIntoResult(nil, "save your work")
}

// ── checkNudgeMessage: count-based ─────────────────────────────────────────

func TestCheckNudgeMessage_CountBased_FiresAtThreshold(t *testing.T) {
	s := newNudgeServer(t, 5, 0) // threshold=5, token budget disabled

	// 4 calls — should not fire yet.
	for i := 0; i < 4; i++ {
		msg := s.checkNudgeMessage("sess1", "agent-a", "", 100)
		if msg != "" {
			t.Fatalf("call %d: expected no nudge, got %q", i+1, msg)
		}
	}
	// 5th call crosses threshold.
	msg := s.checkNudgeMessage("sess1", "agent-a", "", 100)
	if msg == "" {
		t.Fatal("5th call: expected nudge, got none")
	}
	if !strings.Contains(msg, "5") {
		t.Errorf("nudge should mention call count: %q", msg)
	}
}

func TestCheckNudgeMessage_CountBased_FiresOnce(t *testing.T) {
	s := newNudgeServer(t, 3, 0)

	for i := 0; i < 3; i++ {
		s.checkNudgeMessage("sess2", "agent-b", "", 100)
	}
	// Fire happened on 3rd call. Subsequent calls must NOT re-fire.
	for i := 0; i < 10; i++ {
		msg := s.checkNudgeMessage("sess2", "agent-b", "", 100)
		if msg != "" {
			t.Errorf("subsequent call %d: nudge should not re-fire, got %q", i+1, msg)
		}
	}
}

func TestCheckNudgeMessage_CountBased_ResetsAfterSave(t *testing.T) {
	s := newNudgeServer(t, 3, 0)

	for i := 0; i < 3; i++ {
		s.checkNudgeMessage("sess3", "agent-c", "", 100)
	}
	// Nudge fired. Now simulate a memory save (reset).
	s.resetSaveCounter("sess3", "agent-c")

	// Counter reset: next threshold calls should not fire.
	for i := 0; i < 2; i++ {
		msg := s.checkNudgeMessage("sess3", "agent-c", "", 100)
		if msg != "" {
			t.Errorf("after reset, call %d: expected no nudge, got %q", i+1, msg)
		}
	}
	// 3rd call after reset must fire again.
	msg := s.checkNudgeMessage("sess3", "agent-c", "", 100)
	if msg == "" {
		t.Fatal("3rd call after reset: expected nudge, got none")
	}
}

func TestCheckNudgeMessage_CountBased_Disabled(t *testing.T) {
	s := newNudgeServer(t, 0, 0) // both disabled

	for i := 0; i < 100; i++ {
		msg := s.checkNudgeMessage("sess4", "agent-d", "", 100)
		if msg != "" {
			t.Errorf("disabled nudge: unexpected message %q", msg)
		}
	}
}

// ── checkNudgeMessage: token-budget-based ──────────────────────────────────

func TestCheckNudgeMessage_TokenBudget_FiresAtThreshold(t *testing.T) {
	// gpt-4 has 8192 token window. Set budget to 50%. Threshold = 4096 tokens.
	// Each call sends 1000 tokens → should fire after 5th call (5000 > 4096).
	s := newNudgeServer(t, 0, 50.0)

	var lastMsg string
	for i := 0; i < 10; i++ {
		msg := s.checkNudgeMessage("sess5", "agent-e", "gpt-4", 1000)
		if msg != "" {
			lastMsg = msg
			// Verify it mentions the percentage.
			if !strings.Contains(msg, "%") {
				t.Errorf("budget nudge should mention percentage: %q", msg)
			}
			break
		}
	}
	if lastMsg == "" {
		t.Fatal("token-budget nudge never fired")
	}
}

func TestCheckNudgeMessage_TokenBudget_FiresOnce(t *testing.T) {
	s := newNudgeServer(t, 0, 50.0)

	// Exceed budget in one big call.
	s.checkNudgeMessage("sess6", "agent-f", "gpt-4", 5000)

	// Subsequent calls must not re-fire.
	for i := 0; i < 5; i++ {
		msg := s.checkNudgeMessage("sess6", "agent-f", "gpt-4", 1000)
		if msg != "" {
			t.Errorf("token nudge re-fired on call %d: %q", i+1, msg)
		}
	}
}

func TestCheckNudgeMessage_TokenBudget_ResetsAfterSave(t *testing.T) {
	s := newNudgeServer(t, 0, 50.0)

	// Exceed gpt-4 50% threshold.
	s.checkNudgeMessage("sess7", "agent-g", "gpt-4", 5000)

	// Simulate save.
	s.resetSaveCounter("sess7", "agent-g")

	// Must be able to fire again after reset.
	var fired bool
	for i := 0; i < 10; i++ {
		msg := s.checkNudgeMessage("sess7", "agent-g", "gpt-4", 1000)
		if msg != "" {
			fired = true
			break
		}
	}
	if !fired {
		t.Fatal("token-budget nudge should re-arm after resetSaveCounter")
	}
}

func TestCheckNudgeMessage_TokenBudget_PrefersOverCountWhenModelKnown(t *testing.T) {
	// Both enabled. Model is known → token budget should be used, count ignored.
	s := newNudgeServer(t, 2, 50.0) // count threshold=2, budget=50%

	// 2 calls with model known — count threshold reached, but we use budget.
	// Each call: 100 tokens, window=8192 (gpt-4), 50% = 4096. 200 << 4096 → no nudge.
	msg1 := s.checkNudgeMessage("sess8", "agent-h", "gpt-4", 100)
	msg2 := s.checkNudgeMessage("sess8", "agent-h", "gpt-4", 100)
	if msg1 != "" || msg2 != "" {
		t.Errorf("with known model, count-based should not fire: msg1=%q msg2=%q", msg1, msg2)
	}
}

func TestCheckNudgeMessage_UnknownModel_FallsBackToCount(t *testing.T) {
	s := newNudgeServer(t, 3, 50.0)

	// No model declared → falls back to count-based (threshold=3).
	var fired bool
	for i := 0; i < 3; i++ {
		msg := s.checkNudgeMessage("sess9", "agent-i", "", 1000)
		if msg != "" {
			fired = true
		}
	}
	if !fired {
		t.Fatal("with unknown model, count-based fallback should fire at threshold")
	}
}

func TestCheckNudgeMessage_UnrecognisedModel_FallsBackToCount(t *testing.T) {
	// Model string is non-empty but not in modelContextWindow → window=0.
	// Should fall back to count-based nudge.
	s := newNudgeServer(t, 3, 50.0)

	var fired bool
	for i := 0; i < 3; i++ {
		msg := s.checkNudgeMessage("sess11", "agent-k", "some-future-model-xyz", 1000)
		if msg != "" {
			fired = true
		}
	}
	if !fired {
		t.Fatal("unrecognised model with window=0: count-based fallback should fire")
	}
}

// ── resetSaveCounter ────────────────────────────────────────────────────────

func TestResetSaveCounter_EmptyArgs(t *testing.T) {
	s := newNudgeServer(t, 5, 0)
	// Must not panic.
	s.resetSaveCounter("", "")
}

func TestResetSaveCounter_NonexistentEntry(t *testing.T) {
	s := newNudgeServer(t, 5, 0)
	// Must not panic when entry doesn't exist.
	s.resetSaveCounter("no-such-sess", "no-such-agent")
}

// ── integration: nudge injected into tool result via server logic ───────────

func TestNudgeInjectedIntoToolResult(t *testing.T) {
	// Test that injectNudgeIntoResult + checkNudgeMessage work together to add
	// the memory_nudge field to a JSON tool result.
	s := newNudgeServer(t, 3, 0)

	result := makeJSONResult(map[string]any{"entities": []string{"Foo"}})

	// Simulate 3 calls — the 3rd should produce a nudge.
	for i := 0; i < 3; i++ {
		responseTokens := 100
		nudgeMsg := s.checkNudgeMessage("sess10", "agent-j", "", responseTokens)
		if i == 2 {
			if nudgeMsg == "" {
				t.Fatal("3rd call: expected nudge message, got none")
			}
			injectNudgeIntoResult(result, nudgeMsg)
		}
	}

	got := nudgeFromResult(result)
	if got == "" {
		t.Fatal("memory_nudge not found in result after injection")
	}
	if !strings.Contains(got, "memory") {
		t.Errorf("nudge message should mention memory: %q", got)
	}
}

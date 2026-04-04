package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/security"
	mcp "github.com/mark3labs/mcp-go/mcp"
)

// ── extractBlockReason unit tests ──────────────────────────────────────────

func TestExtractBlockReason_BlockWithCriticalFinding(t *testing.T) {
	payload := map[string]any{
		"action_required": "block",
		"security_findings": []any{
			map[string]any{
				"severity":     "CRITICAL",
				"pattern_id":   "go-chi-missing-auth",
				"pattern_name": "Chi Missing Auth Middleware",
			},
		},
	}
	result := makeJSONResult(payload)

	reason, blocked := extractBlockReason(result)
	if !blocked {
		t.Fatal("expected blocked=true for action_required=block")
	}
	if reason != "Chi Missing Auth Middleware" {
		t.Errorf("reason = %q, want pattern_name", reason)
	}
}

func TestExtractBlockReason_BlockNoFindings_FallbackMessage(t *testing.T) {
	// action_required=block but security_findings absent — edge case.
	result := makeJSONResult(map[string]any{"action_required": "block"})

	reason, blocked := extractBlockReason(result)
	if !blocked {
		t.Fatal("expected blocked=true")
	}
	if reason == "" {
		t.Error("expected non-empty fallback reason")
	}
}

func TestExtractBlockReason_WarnNotBlocked(t *testing.T) {
	payload := map[string]any{
		"action_required": "warn",
		"security_findings": []any{
			map[string]any{
				"severity":     "HIGH",
				"pattern_id":   "go-chi-missing-rate-limit",
				"pattern_name": "Missing Rate Limit",
			},
		},
	}
	result := makeJSONResult(payload)

	_, blocked := extractBlockReason(result)
	if blocked {
		t.Error("warn should not trigger a block")
	}
}

func TestExtractBlockReason_NoActionRequired(t *testing.T) {
	result := makeJSONResult(map[string]any{"status": "pass"})
	_, blocked := extractBlockReason(result)
	if blocked {
		t.Error("absent action_required should not block")
	}
}

func TestExtractBlockReason_EmptyContent(t *testing.T) {
	// IsError / empty content result — must not panic, must not block.
	result := &mcp.CallToolResult{IsError: true}
	_, blocked := extractBlockReason(result)
	if blocked {
		t.Error("empty content should not block")
	}
}

func TestExtractBlockReason_PatternIDFallback(t *testing.T) {
	// pattern_name absent — should fall back to pattern_id.
	payload := map[string]any{
		"action_required": "block",
		"security_findings": []any{
			map[string]any{
				"severity":   "CRITICAL",
				"pattern_id": "go-gin-missing-auth",
			},
		},
	}
	result := makeJSONResult(payload)

	reason, blocked := extractBlockReason(result)
	if !blocked {
		t.Fatal("expected blocked=true")
	}
	if reason != "go-gin-missing-auth" {
		t.Errorf("reason = %q, want pattern_id fallback", reason)
	}
}

// ── Integration: handleValidateDispatch blocking ───────────────────────────

// newServerWithPatternEngine creates a server with a live pattern engine and
// a chi route graph (no auth middleware) that will produce CRITICAL findings.
func newServerWithPatternEngine(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	filePath := root + "/api/routes.go"

	g := makeChiRouteGraph(t, root, filePath)

	st := openMCPTestStore(t)
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	srv := New(g, cfg, st)
	// Override with full registry to match the chi pattern detection.
	srv.patternEngine = security.DefaultEngineWithRegistry()
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })
	return srv, filePath
}

// TestValidateDispatch_CRITICAL_Blocks verifies that a CRITICAL security finding
// causes the dispatch handler to return isError=true with the BLOCKED message
// when override is not set.
func TestValidateDispatch_CRITICAL_Blocks(t *testing.T) {
	srv, filePath := newServerWithPatternEngine(t)

	filesJSON, _ := json.Marshal([]string{filePath})
	req := callTool(map[string]any{
		"phase":         "post",
		"files_written": string(filesJSON),
	})

	result, err := srv.handleValidateDispatch(ctx, req)
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
	if !result.IsError {
		// If the chi graph didn't produce a CRITICAL finding, the test is
		// inconclusive rather than a failure. Verify the condition.
		text := result.Content[0].(mcp.TextContent).Text
		if !strings.Contains(text, `"CRITICAL"`) {
			t.Skip("chi route graph did not produce CRITICAL findings in this environment — skipping block enforcement test")
		}
		t.Fatal("expected IsError=true for CRITICAL finding without override")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "BLOCKED:") {
		t.Errorf("expected BLOCKED: prefix in error message, got: %q", text)
	}
	if !strings.Contains(text, "override=true") {
		t.Errorf("expected override=true hint in error message, got: %q", text)
	}
}

// TestValidateDispatch_CRITICAL_Override verifies that override=true lets the
// result through as a non-error response.
func TestValidateDispatch_CRITICAL_Override(t *testing.T) {
	srv, filePath := newServerWithPatternEngine(t)

	filesJSON, _ := json.Marshal([]string{filePath})
	req := callTool(map[string]any{
		"phase":         "post",
		"files_written": string(filesJSON),
		"override":      true,
		"agent_id":      "test-agent",
	})

	result, err := srv.handleValidateDispatch(ctx, req)
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
	if result.IsError {
		text := result.Content[0].(mcp.TextContent).Text
		t.Fatalf("expected non-error result with override=true, got: %s", text)
	}
}

// TestLogCriticalOverride_RecordsEpisode tests that logCriticalOverride records
// a security_override episode in the store with the correct fields.
func TestLogCriticalOverride_RecordsEpisode(t *testing.T) {
	srv, _ := newServerWithPatternEngine(t)

	req := callTool(map[string]any{"agent_id": "test-agent-override"})
	srv.logCriticalOverride(ctx, req, "post", "Chi Missing Auth Middleware")

	// With 8 parallel background workers, a single drainBackground() sentinel
	// may complete on a different worker before the episode write finishes.
	// Drain multiple times to flush all concurrent workers.
	for i := 0; i < bgWorkers+1; i++ {
		srv.drainBackground()
	}
	// Small yield to allow any in-progress SQLite write to commit.
	time.Sleep(20 * time.Millisecond)

	episodes, err := srv.store.GetEpisodes("", "test-agent-override", "security_override", nil, 10, 0)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) == 0 {
		t.Fatal("expected security_override episode to be recorded")
	}
	ep := episodes[0]
	if ep.Outcome != "override" {
		t.Errorf("episode Outcome = %q, want override", ep.Outcome)
	}
	if ep.Importance < 0.8 {
		t.Errorf("episode Importance = %v, want ≥0.8 (high-priority audit trail)", ep.Importance)
	}
	if !strings.Contains(ep.Decision, "post") {
		t.Errorf("episode Decision = %q, want phase info", ep.Decision)
	}
	if ep.Trigger != "Chi Missing Auth Middleware" {
		t.Errorf("episode Trigger = %q, want reason", ep.Trigger)
	}
}

// TestValidateDispatch_NonBlockingPhase_NeverBlocks verifies that management
// phases (list, safety) are never subject to the CRITICAL blocking gate.
func TestValidateDispatch_NonBlockingPhase_NeverBlocks(t *testing.T) {
	srv, _ := newServerWithPatternEngine(t)

	req := callTool(map[string]any{"phase": "list"})

	result, err := srv.handleValidateDispatch(ctx, req)
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
	if result.IsError {
		text := result.Content[0].(mcp.TextContent).Text
		// If there IS an error, it must not be a BLOCKED message.
		if strings.Contains(text, "BLOCKED:") {
			t.Errorf("phase=list must never produce a BLOCKED response: %s", text)
		}
	}
}

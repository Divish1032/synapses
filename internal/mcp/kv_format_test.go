package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

func TestFormatKV_Basic(t *testing.T) {
	fields := []KVField{
		{Key: "Status", Value: "0 pending tasks"},
		{Key: "Branch", Value: "main"},
	}
	out := FormatKV("SESSION", "proj/foo", fields, 0)

	if !strings.HasPrefix(out, "# SESSION (proj/foo)\n") {
		t.Errorf("unexpected header: %q", out)
	}
	if !strings.Contains(out, "Status: 0 pending tasks") {
		t.Errorf("missing Status field: %q", out)
	}
	if !strings.Contains(out, "Branch: main") {
		t.Errorf("missing Branch field: %q", out)
	}
}

func TestFormatKV_NoSubtitle(t *testing.T) {
	out := FormatKV("VALIDATE", "", []KVField{{Key: "Finding", Value: "missing auth"}}, 0)
	if !strings.HasPrefix(out, "# VALIDATE\n") {
		t.Errorf("unexpected header: %q", out)
	}
}

func TestFormatKV_ImportantFieldsFirst(t *testing.T) {
	fields := []KVField{
		{Key: "Regular", Value: "regular value"},
		{Key: "Critical", Value: "critical value", Important: true},
	}
	out := FormatKV("TEST", "", fields, 0)
	// Important field must appear before regular field
	critIdx := strings.Index(out, "Critical:")
	regIdx := strings.Index(out, "Regular:")
	if critIdx == -1 || regIdx == -1 {
		t.Fatalf("expected both fields in output: %q", out)
	}
	if critIdx > regIdx {
		t.Errorf("important field should appear before regular field, got critIdx=%d regIdx=%d", critIdx, regIdx)
	}
}

func TestFormatKV_EmptyFieldsSkipped(t *testing.T) {
	fields := []KVField{
		{Key: "Present", Value: "yes"},
		{Key: "", Value: "orphan"},
		{Key: "Empty", Value: ""},
	}
	out := FormatKV("TEST", "", fields, 0)
	if strings.Contains(out, "orphan") {
		t.Errorf("empty-key field should be skipped: %q", out)
	}
	if strings.Contains(out, "Empty:") {
		t.Errorf("empty-value field should be skipped: %q", out)
	}
	if !strings.Contains(out, "Present: yes") {
		t.Errorf("present field missing: %q", out)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"hello", 2},    // 5/4 = 1.25 → ceil → 2
		{"hello world", 3}, // 11/4 = 2.75 → ceil → 3
	}
	for _, tc := range cases {
		got := EstimateTokens(tc.s)
		if got != tc.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

func TestTrimToTokenBudget_FitsWithinBudget(t *testing.T) {
	s := "short text"
	out := TrimToTokenBudget(s, 1000)
	if out != s {
		t.Errorf("expected unchanged, got %q", out)
	}
}

func TestTrimToTokenBudget_ZeroBudget(t *testing.T) {
	s := "any text"
	out := TrimToTokenBudget(s, 0)
	if out != s {
		t.Errorf("budget=0 should return unchanged, got %q", out)
	}
}

func TestTrimToTokenBudget_Truncates(t *testing.T) {
	// Build a string longer than 10 tokens (40 chars)
	lines := []string{
		"Line one content here",
		"Line two content here",
		"Line three content here",
		"Line four content here",
	}
	s := strings.Join(lines, "\n")
	// Tiny budget = 5 tokens → must truncate
	out := TrimToTokenBudget(s, 5)
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected truncation indicator, got %q", out)
	}
	// Must preserve complete lines (no mid-line cut)
	// The output before the trailer must end at a newline boundary
	trailerIdx := strings.Index(out, "[truncated")
	before := out[:trailerIdx]
	if len(before) > 0 && before[len(before)-1] != '\n' {
		t.Errorf("truncation should happen at line boundary, got %q", before)
	}
}

func TestTrimToTokenBudget_LargeBudgetNoTruncation(t *testing.T) {
	s := strings.Repeat("a", 100)
	out := TrimToTokenBudget(s, 100) // 100 tokens = 400 chars > 100 chars
	if out != s {
		t.Errorf("expected no truncation for budget=100, got %q", out)
	}
}

func TestKvContentHash_Deterministic(t *testing.T) {
	h1 := kvContentHash("hello world")
	h2 := kvContentHash("hello world")
	if h1 != h2 {
		t.Errorf("hash should be deterministic: %q vs %q", h1, h2)
	}
}

func TestKvContentHash_DifferentInputs(t *testing.T) {
	h1 := kvContentHash("content A")
	h2 := kvContentHash("content B")
	if h1 == h2 {
		t.Errorf("different content should produce different hash")
	}
}

func TestKvContentHash_Length(t *testing.T) {
	h := kvContentHash("test")
	if len(h) != 16 {
		t.Errorf("expected 16-char hex hash, got len=%d: %q", len(h), h)
	}
}

// ── renderSessionInitKV integration tests ────────────────────────────────────

func TestRenderSessionInitKV_StatusField(t *testing.T) {
	resp := map[string]interface{}{
		"pending_tasks": map[string]interface{}{
			"count": 2,
		},
		"working_state": map[string]interface{}{
			"current_branch":    "dev/0.9.5",
			"active_violations": 1,
		},
	}
	var tracker sessionDeliveredTracker
	out := renderSessionInitKV(resp, "sess1", "summary", 500, &tracker)

	if !strings.Contains(out, "# SESSION (dev/0.9.5)") {
		t.Errorf("expected SESSION header with branch, got: %q", out)
	}
	if !strings.Contains(out, "Status:") {
		t.Errorf("expected Status field, got: %q", out)
	}
	if !strings.Contains(out, "2 pending task(s)") {
		t.Errorf("expected task count in Status, got: %q", out)
	}
	if !strings.Contains(out, "1 active violation(s)") {
		t.Errorf("expected violation count in Status, got: %q", out)
	}
}

func TestRenderSessionInitKV_WarningDedup(t *testing.T) {
	resp := map[string]interface{}{
		"warnings": []string{"jwt-go deprecated — use golang-jwt/jwt"},
	}
	var tracker sessionDeliveredTracker

	out1 := renderSessionInitKV(resp, "sess1", "summary", 500, &tracker)
	if !strings.Contains(out1, "Warning:") {
		t.Errorf("expected warning on first call: %q", out1)
	}

	// Second call: same warning should be deduplicated
	out2 := renderSessionInitKV(resp, "sess1", "summary", 500, &tracker)
	if strings.Contains(out2, "jwt-go deprecated") {
		t.Errorf("warning should be deduplicated on second call: %q", out2)
	}
}

func TestRenderSessionInitKV_SignalMode(t *testing.T) {
	resp := map[string]interface{}{
		"_summary": "2 tasks pending",
		"pending_tasks": map[string]interface{}{
			"count": 2,
		},
	}
	var tracker sessionDeliveredTracker
	out := renderSessionInitKV(resp, "sess1", "signal", 500, &tracker)

	// Signal mode should include the Summary field
	if !strings.Contains(out, "Summary:") {
		t.Errorf("signal mode should include Summary field: %q", out)
	}
	// Status should still be present (Important=true fields survive signal mode)
	if !strings.Contains(out, "Status:") {
		t.Errorf("signal mode should include Status (important) field: %q", out)
	}
}

func TestRenderSessionInitKV_ConventionDedup(t *testing.T) {
	resp := map[string]interface{}{
		"active_prompts": map[string]interface{}{
			"conventions": []interface{}{"use table-driven tests"},
		},
	}
	var tracker sessionDeliveredTracker

	out1 := renderSessionInitKV(resp, "sess1", "summary", 500, &tracker)
	if !strings.Contains(out1, "Convention:") {
		t.Errorf("expected convention on first call: %q", out1)
	}

	out2 := renderSessionInitKV(resp, "sess1", "summary", 500, &tracker)
	if strings.Contains(out2, "table-driven") {
		t.Errorf("convention should be deduplicated on second call: %q", out2)
	}
}

func TestRenderSessionInitKV_EmptySession(t *testing.T) {
	// Empty sessionID means no dedup tracking — should always deliver
	resp := map[string]interface{}{
		"warnings": []string{"test warning"},
	}
	var tracker sessionDeliveredTracker

	out1 := renderSessionInitKV(resp, "", "summary", 500, &tracker)
	out2 := renderSessionInitKV(resp, "", "summary", 500, &tracker)

	// Both calls should include the warning (no dedup for empty session)
	if !strings.Contains(out1, "test warning") {
		t.Errorf("expected warning on first call: %q", out1)
	}
	if !strings.Contains(out2, "test warning") {
		t.Errorf("expected warning on second call (no dedup for empty session): %q", out2)
	}
}

// ── reformatValidateKV integration tests ─────────────────────────────────────

func makeValidateResult(body map[string]interface{}) *mcp.CallToolResult {
	b, _ := json.Marshal(body)
	return mcp.NewToolResultText(string(b))
}

func TestReformatValidateKV_SecurityFinding(t *testing.T) {
	result := makeValidateResult(map[string]interface{}{
		"action": "BLOCK",
		"security_findings": []interface{}{
			map[string]interface{}{
				"severity":   "CRITICAL",
				"message":    "endpoint lacks auth middleware",
				"pattern_id": "missing-auth",
			},
		},
	})
	req := callTool(map[string]any{"phase": "post"})
	out := reformatValidateKV(result, "post", req, "summary", 300)

	text := kvExtractText(t, out)
	if !strings.Contains(text, "[CRITICAL] missing-auth") {
		t.Errorf("expected CRITICAL finding with pattern_id, got: %q", text)
	}
	if !strings.Contains(text, "endpoint lacks auth middleware") {
		t.Errorf("expected finding message, got: %q", text)
	}
	if !strings.Contains(text, "Action:") {
		t.Errorf("expected Action field, got: %q", text)
	}
}

func TestReformatValidateKV_EmptySeverity(t *testing.T) {
	// Security finding with no severity should not render as "[]"
	result := makeValidateResult(map[string]interface{}{
		"security_findings": []interface{}{
			map[string]interface{}{
				"severity": "",
				"message":  "some issue",
			},
		},
	})
	req := callTool(map[string]any{})
	out := reformatValidateKV(result, "pre", req, "summary", 300)

	text := kvExtractText(t, out)
	if strings.Contains(text, "[]") {
		t.Errorf("empty severity should not render as '[]', got: %q", text)
	}
	if !strings.Contains(text, "[UNKNOWN]") {
		t.Errorf("empty severity should render as [UNKNOWN], got: %q", text)
	}
}

func TestReformatValidateKV_CleanResult(t *testing.T) {
	// No findings → should show Status field
	result := makeValidateResult(map[string]interface{}{
		"status": "ok",
	})
	req := callTool(map[string]any{})
	out := reformatValidateKV(result, "pre", req, "summary", 300)

	text := kvExtractText(t, out)
	if !strings.Contains(text, "Status:") {
		t.Errorf("clean result should show Status field, got: %q", text)
	}
}

func TestReformatValidateKV_SignalMode_SkipsNonCritical(t *testing.T) {
	result := makeValidateResult(map[string]interface{}{
		"security_findings": []interface{}{
			map[string]interface{}{"severity": "MEDIUM", "message": "medium issue"},
			map[string]interface{}{"severity": "CRITICAL", "message": "critical issue"},
		},
	})
	req := callTool(map[string]any{})
	out := reformatValidateKV(result, "pre", req, "signal", 300)

	text := kvExtractText(t, out)
	if strings.Contains(text, "medium issue") {
		t.Errorf("signal mode should skip non-CRITICAL, got: %q", text)
	}
	if !strings.Contains(text, "critical issue") {
		t.Errorf("signal mode should include CRITICAL, got: %q", text)
	}
}

func TestReformatValidateKV_NonJSONPassthrough(t *testing.T) {
	// Non-JSON content → return as-is
	result := mcp.NewToolResultText("plain text response")
	req := callTool(map[string]any{})
	out := reformatValidateKV(result, "pre", req, "summary", 300)

	text := kvExtractText(t, out)
	if text != "plain text response" {
		t.Errorf("non-JSON should pass through unchanged, got: %q", text)
	}
}

// ── toolParamDocs tests ───────────────────────────────────────────────────────

func TestToolParamDocs_AllToolsKnown(t *testing.T) {
	tools := []string{"session_init", "get_context", "validate", "search", "get_impact", "memory", "tasks", "end_session"}
	for _, tool := range tools {
		docs := toolParamDocs(tool)
		if docs == "" {
			t.Errorf("toolParamDocs(%q) returned empty — tool should have docs", tool)
		}
		if !strings.Contains(docs, "Parameters") && !strings.Contains(docs, "Actions") && !strings.Contains(docs, "Phases") {
			t.Errorf("toolParamDocs(%q) missing parameter section, got: %q", tool, docs[:kvMin(100, len(docs))])
		}
	}
}

func TestToolParamDocs_UnknownTool(t *testing.T) {
	docs := toolParamDocs("nonexistent_tool")
	if docs != "" {
		t.Errorf("unknown tool should return empty, got: %q", docs)
	}
}

// kvExtractText pulls the text content from a CallToolResult for test assertions.
// TestRenderSessionInitKV_ProjectValueMetrics verifies that the ProjectValue
// field appears in summary/full modes and is absent in signal mode.
func TestRenderSessionInitKV_ProjectValueMetrics(t *testing.T) {
	resp := map[string]interface{}{
		"project_value_metrics": map[string]interface{}{
			"days":              30,
			"memory_retrievals": 47,
			"validate_blocks":   3,
			"files_from_graph":  340,
			"summary":           "30d: 47 memory retrievals, 3 validate blocks, 340 files served from graph",
		},
	}

	var tracker sessionDeliveredTracker

	// summary mode: ProjectValue field must be present.
	out := renderSessionInitKV(resp, "sess-pvm", "summary", 500, &tracker)
	if !strings.Contains(out, "ProjectValue:") {
		t.Errorf("summary mode: expected ProjectValue field, got:\n%s", out)
	}
	if !strings.Contains(out, "47 memory retrievals") {
		t.Errorf("summary mode: expected metric counts in ProjectValue, got:\n%s", out)
	}

	// signal mode: ProjectValue must NOT appear (signal is status+warnings only).
	var tracker2 sessionDeliveredTracker
	outSignal := renderSessionInitKV(resp, "sess-pvm-sig", "signal", 500, &tracker2)
	if strings.Contains(outSignal, "ProjectValue:") {
		t.Errorf("signal mode: ProjectValue should be absent, got:\n%s", outSignal)
	}
}

func kvExtractText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil {
		t.Fatal("nil result")
	}
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatal("no text content in result")
	return ""
}

func kvMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

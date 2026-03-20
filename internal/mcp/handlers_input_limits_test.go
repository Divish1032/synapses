package mcp

import (
	"strings"
	"testing"
)

// Security F5: MCP Input Size Limits
//
// These tests verify the attack vector is CLOSED: oversized MCP string args
// must be rejected or truncated before reaching SQLite / the embedder.
// Each test demonstrates what a malicious agent could send and proves the
// guard fires correctly.

// ── stringArg truncation ──────────────────────────────────────────────────────

func TestStringArg_TruncatesAtDefaultLimit(t *testing.T) {
	oversized := strings.Repeat("x", maxArgLength+1)
	req := callTool(map[string]any{"key": oversized})
	got := stringArg(req, "key")
	if len(got) != maxArgLength {
		t.Errorf("stringArg: want len=%d, got len=%d", maxArgLength, len(got))
	}
}

func TestStringArg_AcceptsExactlyAtLimit(t *testing.T) {
	exact := strings.Repeat("x", maxArgLength)
	req := callTool(map[string]any{"key": exact})
	got := stringArg(req, "key")
	if len(got) != maxArgLength {
		t.Errorf("stringArg: want len=%d, got len=%d", maxArgLength, len(got))
	}
}

func TestStringArg_AcceptsShortValue(t *testing.T) {
	req := callTool(map[string]any{"key": "hello"})
	got := stringArg(req, "key")
	if got != "hello" {
		t.Errorf("stringArg: want %q, got %q", "hello", got)
	}
}

// ── stringArgLimited rejection ────────────────────────────────────────────────

func TestStringArgLimited_RejectsOversized(t *testing.T) {
	oversized := strings.Repeat("x", maxArgLengthDecision+1)
	req := callTool(map[string]any{"decision": oversized})
	_, err := stringArgLimited(req, "decision", maxArgLengthDecision)
	if err == nil {
		t.Fatal("stringArgLimited: expected error for oversized input, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum length") {
		t.Errorf("stringArgLimited: error message should mention limit, got: %v", err)
	}
}

func TestStringArgLimited_AcceptsExactlyAtLimit(t *testing.T) {
	exact := strings.Repeat("x", maxArgLengthDecision)
	req := callTool(map[string]any{"decision": exact})
	got, err := stringArgLimited(req, "decision", maxArgLengthDecision)
	if err != nil {
		t.Fatalf("stringArgLimited: unexpected error at exact limit: %v", err)
	}
	if len(got) != maxArgLengthDecision {
		t.Errorf("stringArgLimited: want len=%d, got len=%d", maxArgLengthDecision, len(got))
	}
}

// ── handleRemember: decision field (4 KB limit) ───────────────────────────────

// Attack: malicious agent sends 1 MB decision to exhaust SQLite/embedder.
// Guard must reject with an error before any storage occurs.
func TestHandleRemember_OversizedDecision_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	oversized := strings.Repeat("a", maxArgLengthDecision+1)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "attacker",
		"decision": oversized,
		"outcome":  "success",
	}))
	msg := mustErrorResult(t, res, err)
	if !strings.Contains(msg, "decision") || !strings.Contains(msg, "exceeds") {
		t.Errorf("error message should mention decision limit, got: %q", msg)
	}
}

func TestHandleRemember_DecisionAtLimit_Accepted(t *testing.T) {
	s := newTestServer(t)
	atLimit := strings.Repeat("b", maxArgLengthDecision)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "agent",
		"decision": atLimit,
		"outcome":  "success",
	}))
	mustResult(t, res, err)
}

// ── handleAnnotateNode: note field (8 KB limit) ───────────────────────────────

// Attack: oversized note floods annotation store and embedder.
func TestHandleAnnotateNode_OversizedNote_ReturnsError(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	oversized := strings.Repeat("n", maxArgLengthNote+1)
	res, err := s.handleAnnotateNode(ctx, callTool(map[string]any{
		"node_id":  string(loginID),
		"note":     oversized,
		"agent_id": "attacker",
	}))
	msg := mustErrorResult(t, res, err)
	if !strings.Contains(msg, "note") || !strings.Contains(msg, "exceeds") {
		t.Errorf("error message should mention note limit, got: %q", msg)
	}
}

func TestHandleAnnotateNode_NoteAtLimit_Accepted(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	atLimit := strings.Repeat("n", maxArgLengthNote)
	res, err := s.handleAnnotateNode(ctx, callTool(map[string]any{
		"node_id":  string(loginID),
		"note":     atLimit,
		"agent_id": "agent",
	}))
	mustResult(t, res, err)
}

// ── handleSendMessage: payload field (16 KB limit) ────────────────────────────

// Attack: oversized JSON payload bloats the message bus table.
func TestHandleSendMessage_OversizedPayload_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	// Construct valid JSON that exceeds the payload limit.
	inner := strings.Repeat("x", maxArgLengthPayload)
	oversized := `{"data":"` + inner + `"}`
	res, err := s.handleSendMessage(ctx, callTool(map[string]any{
		"from_agent": "attacker",
		"topic":      "flood",
		"payload":    oversized,
	}))
	msg := mustErrorResult(t, res, err)
	if !strings.Contains(msg, "payload") || !strings.Contains(msg, "exceeds") {
		t.Errorf("error message should mention payload limit, got: %q", msg)
	}
}

func TestHandleSendMessage_PayloadAtLimit_Accepted(t *testing.T) {
	s := newTestServer(t)
	// Build JSON exactly at the limit: {"d":"<N bytes>"} — account for wrapper.
	innerLen := maxArgLengthPayload - len(`{"d":""}`)
	inner := strings.Repeat("y", innerLen)
	atLimit := `{"d":"` + inner + `"}`
	if len(atLimit) > maxArgLengthPayload {
		t.Fatalf("test setup: payload too large (%d > %d)", len(atLimit), maxArgLengthPayload)
	}
	res, err := s.handleSendMessage(ctx, callTool(map[string]any{
		"from_agent": "agent",
		"to_agent":   "receiver", // directed message avoids approval gate
		"topic":      "ok",
		"payload":    atLimit,
	}))
	mustResult(t, res, err)
}

// ── handleWebAnnotate: note field (8 KB limit) ────────────────────────────────

// Attack: oversized note in web annotation floods storage.
func TestHandleWebAnnotate_OversizedNote_ReturnsError(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	oversized := strings.Repeat("w", maxArgLengthNote+1)
	res, err := s.handleWebAnnotate(ctx, callTool(map[string]any{
		"node_id":  string(loginID),
		"note":     oversized,
		"agent_id": "attacker",
	}))
	msg := mustErrorResult(t, res, err)
	if !strings.Contains(msg, "note") || !strings.Contains(msg, "exceeds") {
		t.Errorf("error message should mention note limit, got: %q", msg)
	}
}

// ── global stringArg cap ─────────────────────────────────────────────────────

// Verify that any arg beyond 64KB is silently truncated at the base layer,
// providing defense-in-depth even for fields that don't use stringArgLimited.
func TestStringArg_MassiveInputTruncated(t *testing.T) {
	massive := strings.Repeat("z", 10*1024*1024) // 10 MB
	req := callTool(map[string]any{"entity": massive})
	got := stringArg(req, "entity")
	if len(got) > maxArgLength {
		t.Errorf("stringArg: 10MB input not truncated, got len=%d (limit=%d)", len(got), maxArgLength)
	}
}

// ── truncateUTF8: valid UTF-8 at rune boundaries ──────────────────────────────

// Regression: v[:n] can cut in the middle of a multi-byte UTF-8 character,
// producing invalid UTF-8 that corrupts SQLite data and JSON serialization.
// Uses the existing isValidUTF8 helper from get_edge_types_test.go.

func TestTruncateUTF8_ASCIIOnlyNoBoundaryIssue(t *testing.T) {
	s := strings.Repeat("a", maxArgLength+100)
	got := truncateUTF8(s, maxArgLength)
	if len(got) != maxArgLength {
		t.Errorf("ASCII truncate: want %d, got %d", maxArgLength, len(got))
	}
	if !isValidUTF8(got) {
		t.Error("truncated ASCII is not valid UTF-8")
	}
}

func TestTruncateUTF8_CJKBoundary(t *testing.T) {
	// Each CJK character is 3 bytes in UTF-8.
	// Build a string that when naively cut at maxBytes, cuts a 3-byte char.
	// We use a small limit to make the test fast.
	const limit = 10
	// "你好世界" = 4 chars × 3 bytes = 12 bytes; cutting at 10 cuts "界" in half.
	s := strings.Repeat("你好世界", 3) // 36 bytes
	got := truncateUTF8(s, limit)
	if !isValidUTF8(got) {
		t.Errorf("CJK truncate produced invalid UTF-8: %q", got)
	}
	if len(got) > limit {
		t.Errorf("CJK truncate exceeded limit: len=%d > %d", len(got), limit)
	}
	// Must be exactly 9 bytes (3 complete chars × 3 bytes), not 10.
	if len(got) != 9 {
		t.Errorf("CJK truncate: want 9 bytes (3 complete chars), got %d", len(got))
	}
}

func TestTruncateUTF8_EmojiAt4Bytes(t *testing.T) {
	// Emoji are 4 bytes in UTF-8. Cutting at a 4-byte boundary should work.
	// "😀" = 4 bytes. Repeat 3x = 12 bytes. Cut at 10 = cuts last emoji mid-char.
	const limit = 10
	s := "😀😀😀" // 12 bytes
	got := truncateUTF8(s, limit)
	if !isValidUTF8(got) {
		t.Errorf("emoji truncate produced invalid UTF-8: %q", got)
	}
	// Must be exactly 8 bytes (2 complete emoji × 4 bytes), not 10.
	if len(got) != 8 {
		t.Errorf("emoji truncate: want 8 bytes (2 complete emoji), got %d", len(got))
	}
}

func TestTruncateUTF8_ExactlyAtLimit(t *testing.T) {
	s := strings.Repeat("a", 100)
	got := truncateUTF8(s, 100)
	if got != s {
		t.Error("string at exact limit should not be truncated")
	}
}

func TestTruncateUTF8_EmptyString(t *testing.T) {
	got := truncateUTF8("", 100)
	if got != "" {
		t.Errorf("empty string truncate: want \"\", got %q", got)
	}
}

func TestStringArg_CJKInputProducesValidUTF8(t *testing.T) {
	// Create CJK string just over the 64KB limit.
	// Each char is 3 bytes, so 64KB / 3 ≈ 21845 chars.
	// Use enough chars so naive byte-slice would cut mid-char.
	cjkChar := "你" // 3 bytes
	// Fill to maxArgLength + a few chars to trigger the boundary issue.
	n := maxArgLength/3 + 1 // just over limit
	s := strings.Repeat(cjkChar, n)
	req := callTool(map[string]any{"key": s})
	got := stringArg(req, "key")
	if !isValidUTF8(got) {
		t.Error("stringArg with CJK input produced invalid UTF-8")
	}
	if len(got) > maxArgLength {
		t.Errorf("stringArg: result exceeds limit, len=%d", len(got))
	}
}


// ── handleRemember: rationale field (4 KB limit) ─────────────────────────────

// Attack: oversized rationale bypasses decision limit via concatenation before
// embedding — combined content would be up to 68KB fed to the embedder.
func TestHandleRemember_OversizedRationale_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	oversized := strings.Repeat("r", maxArgLengthRationale+1)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":  "attacker",
		"decision":  "valid decision",
		"rationale": oversized,
		"outcome":   "success",
	}))
	msg := mustErrorResult(t, res, err)
	if !strings.Contains(msg, "rationale") || !strings.Contains(msg, "exceeds") {
		t.Errorf("error message should mention rationale limit, got: %q", msg)
	}
}

func TestHandleRemember_RationaleAtLimit_Accepted(t *testing.T) {
	s := newTestServer(t)
	atLimit := strings.Repeat("r", maxArgLengthRationale)
	res, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":  "agent",
		"decision":  "valid decision",
		"rationale": atLimit,
		"outcome":   "success",
	}))
	mustResult(t, res, err)
}

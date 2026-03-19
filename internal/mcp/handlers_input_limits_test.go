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

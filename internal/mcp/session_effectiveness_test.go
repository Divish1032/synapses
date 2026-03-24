package mcp

import (
	"strings"
	"testing"
)

// ── buildEffectivenessMessage ──────────────────────────────────────────────

func TestBuildEffectivenessMessage_NoDeliveries(t *testing.T) {
	r := &EffectivenessReport{
		TotalDeliveries: 0,
		ToolCalls:       12,
		DurationMs:      30_000,
	}
	msg := buildEffectivenessMessage(r)
	if !strings.Contains(msg, "No context deliveries") {
		t.Errorf("expected 'No context deliveries' in message, got: %q", msg)
	}
	if !strings.Contains(msg, "12 tool calls") {
		t.Errorf("expected '12 tool calls' in message, got: %q", msg)
	}
}

func TestBuildEffectivenessMessage_WithDeliveries(t *testing.T) {
	r := &EffectivenessReport{
		TotalDeliveries:    16,
		FirstFetchRight:    14,
		ContextHitRate:     0.85,
		TaskCompletionRate: 0.92,
		ToolCalls:          47,
		TokensSaved:        3000,
		DurationMs:         240_000,
	}
	msg := buildEffectivenessMessage(r)
	// Must contain the first-fetch fraction.
	if !strings.Contains(msg, "14/16") {
		t.Errorf("expected '14/16' in message, got: %q", msg)
	}
	// Must contain the hit rate percentage.
	if !strings.Contains(msg, "85%") {
		t.Errorf("expected '85%%' in message, got: %q", msg)
	}
	// Must mention tool calls.
	if !strings.Contains(msg, "47 tool calls") {
		t.Errorf("expected '47 tool calls' in message, got: %q", msg)
	}
	// Must include token savings.
	if !strings.Contains(msg, "3000 tokens saved") {
		t.Errorf("expected token savings in message, got: %q", msg)
	}
}

func TestBuildEffectivenessMessage_PerfectFirstFetch(t *testing.T) {
	r := &EffectivenessReport{
		TotalDeliveries: 5,
		FirstFetchRight: 5,
		ContextHitRate:  1.0,
		ToolCalls:       20,
		DurationMs:      60_000,
	}
	msg := buildEffectivenessMessage(r)
	if !strings.Contains(msg, "5/5") {
		t.Errorf("expected '5/5' in message, got: %q", msg)
	}
	if !strings.Contains(msg, "100%") {
		t.Errorf("expected '100%%' in message, got: %q", msg)
	}
}

func TestBuildEffectivenessMessage_ZeroTokensSaved_NoSavingsLine(t *testing.T) {
	r := &EffectivenessReport{
		TotalDeliveries: 2,
		FirstFetchRight: 1,
		ContextHitRate:  0.5,
		ToolCalls:       5,
		TokensSaved:     0,
		DurationMs:      10_000,
	}
	msg := buildEffectivenessMessage(r)
	if strings.Contains(msg, "tokens saved") {
		t.Errorf("expected no 'tokens saved' when TokensSaved=0, got: %q", msg)
	}
}

// ── EffectivenessReport struct zero-value safety ──────────────────────────

func TestBuildEffectivenessMessage_AllZero(t *testing.T) {
	// Zero-value report must not panic.
	r := &EffectivenessReport{}
	msg := buildEffectivenessMessage(r)
	if msg == "" {
		t.Error("expected non-empty message for zero-value report")
	}
}

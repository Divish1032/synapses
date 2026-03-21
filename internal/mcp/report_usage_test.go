package mcp

import (
	"testing"
)

// ── handleReportUsage ─────────────────────────────────────────────────────────

func TestHandleReportUsage_AllFields_ReturnsRecorded(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleReportUsage(ctx, callTool(map[string]any{
		"model":         "claude-sonnet-4-6",
		"provider":      "anthropic",
		"agent_id":      "test-agent",
		"input_tokens":  float64(1000),
		"output_tokens": float64(500),
		"cost_usd":      0.005,
	}))
	m := mustResult(t, res, err)
	if recorded, _ := m["recorded"].(bool); !recorded {
		t.Errorf("expected recorded=true, got %v", m["recorded"])
	}
	if m["model"] != "claude-sonnet-4-6" {
		t.Errorf("expected model=claude-sonnet-4-6, got %v", m["model"])
	}
	hasKey(t, m, "note")
}

func TestHandleReportUsage_MissingModel_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleReportUsage(ctx, callTool(map[string]any{
		"input_tokens":  float64(500),
		"output_tokens": float64(100),
	}))
	mustErrorResult(t, res, err)
}

func TestHandleReportUsage_EmptyModel_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleReportUsage(ctx, callTool(map[string]any{
		"model": "",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleReportUsage_NoAgentID_UsesLastAgent(t *testing.T) {
	s := newTestServer(t)
	// First call a session_init to register a last agent.
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "implicit-agent"}))

	res, err := s.handleReportUsage(ctx, callTool(map[string]any{
		"model": "gpt-4o",
		// No agent_id — should fall back to getLastAgent().
	}))
	m := mustResult(t, res, err)
	if recorded, _ := m["recorded"].(bool); !recorded {
		t.Errorf("expected recorded=true even with no agent_id, got %v", m["recorded"])
	}
}

func TestHandleReportUsage_NilPulseClient_GracefulDegradation(t *testing.T) {
	s := newTestServer(t)
	// newTestServer leaves pulse client nil — verifies graceful degradation.
	res, err := s.handleReportUsage(ctx, callTool(map[string]any{
		"model":        "claude-haiku-4-5-20251001",
		"agent_id":     "test-agent",
		"input_tokens": float64(200),
	}))
	m := mustResult(t, res, err)
	if recorded, _ := m["recorded"].(bool); !recorded {
		t.Errorf("expected recorded=true with nil pulse client, got %v", m["recorded"])
	}
}

func TestHandleReportUsage_ZeroTokens_Succeeds(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleReportUsage(ctx, callTool(map[string]any{
		"model":         "claude-sonnet-4-6",
		"input_tokens":  float64(0),
		"output_tokens": float64(0),
	}))
	m := mustResult(t, res, err)
	if recorded, _ := m["recorded"].(bool); !recorded {
		t.Errorf("expected recorded=true for zero tokens, got %v", m["recorded"])
	}
}

func TestHandleReportUsage_ModelOnlyMinimal_Succeeds(t *testing.T) {
	s := newTestServer(t)
	// Only required field: model.
	res, err := s.handleReportUsage(ctx, callTool(map[string]any{
		"model": "claude-opus-4-6",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "recorded")
	hasKey(t, m, "model")
	hasKey(t, m, "note")
}

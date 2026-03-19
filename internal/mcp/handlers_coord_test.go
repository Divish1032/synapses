package mcp

import (
	"testing"
)

// ── handleGetAgents ───────────────────────────────────────────────────────────

func TestHandleGetAgents_Empty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetAgents(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "agents")
}

func TestHandleGetAgents_AfterSessionInit(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "visible-agent"}))

	res, err := s.handleGetAgents(ctx, callTool(nil))
	m := mustResult(t, res, err)
	agents, _ := m["agents"].([]any)
	found := false
	for _, a := range agents {
		agent, _ := a.(map[string]any)
		if agent["id"] == "visible-agent" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected visible-agent in get_agents after session_init")
	}
}

func TestHandleGetAgents_IncludesPresence(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "presence-agent"}))

	res, err := s.handleGetAgents(ctx, callTool(nil))
	m := mustResult(t, res, err)
	agents, _ := m["agents"].([]any)
	for _, a := range agents {
		agent, _ := a.(map[string]any)
		if agent["id"] == "presence-agent" {
			if _, ok := agent["presence"]; !ok {
				t.Error("expected presence field on agent")
			}
			return
		}
	}
	t.Error("presence-agent not found")
}

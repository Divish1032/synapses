package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
)

// newKnowledgeServer creates a Server in knowledge mode with a real temp store.
func newKnowledgeServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{Mode: "knowledge"}
	st := openTestStore(t)
	srv := NewKnowledge(cfg, st)
	srv.projectPath = "/test/marketing"
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })
	return srv
}

func TestKnowledgeMode_FlagSet(t *testing.T) {
	srv := newKnowledgeServer(t)
	if !srv.knowledgeMode {
		t.Error("expected knowledgeMode to be true")
	}
	if srv.graph != nil {
		t.Error("expected graph to be nil in knowledge mode")
	}
}

func TestKnowledgeMode_SessionInit(t *testing.T) {
	srv := newKnowledgeServer(t)

	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "marketing-agent",
		"scope":    "full",
	}))
	m := mustResult(t, res, err)

	// Should have pending_tasks, working_state, scale_guidance.
	hasKey(t, m, "pending_tasks")
	hasKey(t, m, "working_state")
	hasKey(t, m, "scale_guidance")

	// Project identity should include mode=knowledge.
	if pi, ok := m["project_identity"].(map[string]any); ok {
		if mode, ok := pi["mode"].(string); ok {
			if mode != "knowledge" {
				t.Errorf("expected mode=knowledge in project_identity, got %q", mode)
			}
		} else {
			t.Error("expected mode field in project_identity")
		}
	} else {
		t.Error("expected project_identity in response")
	}
}

func TestKnowledgeMode_Remember(t *testing.T) {
	srv := newKnowledgeServer(t)

	res, err := srv.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id": "test-agent",
		"decision": "Marketing Q1 campaign uses brand voice v2",
	}))
	m := mustResult(t, res, err)
	if _, ok := m["episode_id"]; !ok {
		t.Error("expected episode_id in remember response")
	}
}

func TestKnowledgeMode_Recall(t *testing.T) {
	srv := newKnowledgeServer(t)

	// First remember something.
	srv.handleRemember(context.Background(), callTool(map[string]any{
		"agent_id": "test-agent",
		"decision": "QA decided to use Playwright for E2E tests",
	}))

	// Then recall it.
	res, err := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query": "Playwright",
	}))
	m := mustResult(t, res, err)
	if _, ok := m["episodes"]; !ok {
		t.Error("expected episodes in recall response")
	}
}

func TestKnowledgeMode_CreatePlan(t *testing.T) {
	srv := newKnowledgeServer(t)

	res, err := srv.handleCreatePlan(context.Background(), callTool(map[string]any{
		"title": "Q1 Marketing Launch",
		"tasks": `[{"title":"Draft copy","description":"Write landing page copy","priority":"p0"}]`,
	}))
	m := mustResult(t, res, err)
	if _, ok := m["plan_id"]; !ok {
		t.Error("expected plan_id in create_plan response")
	}
}

func TestKnowledgeMode_SendMessage(t *testing.T) {
	srv := newKnowledgeServer(t)

	// Broadcast requires approval (OF-E3). Step 1: request approval.
	res, err := srv.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "marketing-agent",
		"topic":      "review_needed",
		"payload":    `{"item":"Q1 copy"}`,
	}))
	m := mustResult(t, res, err)
	if m["requires_approval"] != true {
		t.Fatal("expected requires_approval=true for broadcast send_message")
	}
	// Token must not appear in response.
	if _, hasToken := m["approval_token"]; hasToken {
		t.Fatal("approval_token must not appear in MCP response")
	}

	// Step 2: simulate user approval via `synapses approve`.
	p := findPendingFor(t, "broadcast_message", "marketing-agent")
	if err := ApproveRequest(p.Token); err != nil {
		t.Fatalf("ApproveRequest: %v", err)
	}

	// Step 3: agent retries without approval_token.
	res, err = srv.handleSendMessage(context.Background(), callTool(map[string]any{
		"from_agent": "marketing-agent",
		"topic":      "review_needed",
		"payload":    `{"item":"Q1 copy"}`,
	}))
	m = mustResult(t, res, err)
	if _, ok := m["message_id"]; !ok {
		t.Error("expected message_id in send_message response after approval")
	}
}

func TestKnowledgeMode_EndSession(t *testing.T) {
	srv := newKnowledgeServer(t)

	// Start a session first.
	srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "test-agent",
	}))

	res, err := srv.handleEndSession(context.Background(), callTool(map[string]any{
		"agent_id": "test-agent",
		"summary":  "Completed marketing review",
	}))
	m := mustResult(t, res, err)
	if status, ok := m["status"].(string); !ok || status != "ok" {
		t.Errorf("expected status=ok, got %v", m["status"])
	}
}

func TestKnowledgeMode_DiscoverToolsFiltered(t *testing.T) {
	srv := newKnowledgeServer(t)

	// Empty query should only show knowledge-mode tools.
	res, err := srv.handleDiscoverTools(context.Background(), callTool(map[string]any{}))
	m := mustResult(t, res, err)

	if mode, ok := m["mode"].(string); !ok || mode != "knowledge" {
		t.Errorf("expected mode=knowledge, got %v", m["mode"])
	}

	// Verify categories don't contain graph tools.
	cats, _ := m["categories"].(map[string]any)
	for _, toolList := range cats {
		if tools, ok := toolList.([]any); ok {
			for _, raw := range tools {
				tool, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				name, _ := tool["name"].(string)
				if name == "get_context" || name == "get_impact" || name == "find_entity" || name == "search" {
					t.Errorf("graph tool %q should not appear in knowledge mode discover_tools", name)
				}
			}
		}
	}
}

func TestKnowledgeMode_GraphToolStub(t *testing.T) {
	// Verify that the server is created correctly in knowledge mode
	// and that knowledgeMode flag is set properly.
	srv := newKnowledgeServer(t)

	if !srv.knowledgeMode {
		t.Error("expected knowledgeMode to be true")
	}

	// Verify that knowledge tools are in the set.
	for _, name := range []string{"session_init", "memory", "tasks", "validate"} {
		if !knowledgeTools[name] {
			t.Errorf("expected %q to be in knowledgeTools", name)
		}
	}

	// Verify graph tools are NOT in the knowledge set.
	for _, name := range []string{"get_context", "get_impact", "search", "annotate"} {
		if knowledgeTools[name] {
			t.Errorf("%q should NOT be in knowledgeTools", name)
		}
	}
}

func TestNewKnowledge_NilConfig(t *testing.T) {
	st := openTestStore(t)
	srv := NewKnowledge(nil, st)
	if !srv.knowledgeMode {
		t.Error("expected knowledgeMode to be true even with nil config")
	}
}

func TestNewKnowledge_ExplicitConfig(t *testing.T) {
	st := openTestStore(t)
	cfg := &config.Config{Version: "1"}
	srv := NewKnowledge(cfg, st)
	if !srv.knowledgeMode {
		t.Error("expected knowledgeMode to be true")
	}
	if cfg.Mode != "knowledge" {
		t.Error("expected NewKnowledge to set cfg.Mode to knowledge")
	}
}

func TestKnowledgeMode_ScaleGuidanceMessage(t *testing.T) {
	srv := newKnowledgeServer(t)

	res, err := srv.handleSessionInit(context.Background(), callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	m := mustResult(t, res, err)

	guidance, _ := m["scale_guidance"].(string)
	if !strings.Contains(guidance, "Knowledge mode") {
		t.Errorf("expected scale_guidance to mention Knowledge mode, got: %s", guidance)
	}
}

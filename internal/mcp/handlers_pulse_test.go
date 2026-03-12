package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/pulse"
)

// TestLastAgentGetSet verifies that setLastAgent / getLastAgent round-trip
// correctly and start empty.
func TestLastAgentGetSet(t *testing.T) {
	srv := newTestServer(t)
	if got := srv.getLastAgent(); got != "" {
		t.Errorf("expected empty initial lastAgent, got %q", got)
	}
	srv.setLastAgent("claude-code")
	if got := srv.getLastAgent(); got != "claude-code" {
		t.Errorf("expected lastAgent claude-code, got %q", got)
	}
}

// fakeContextDeliveryServer returns a test HTTP server that captures the first
// context-delivery event into the returned channel.
func fakeContextDeliveryServer(t *testing.T) (*httptest.Server, <-chan pulse.ContextDeliveryEvent) {
	t.Helper()
	ch := make(chan pulse.ContextDeliveryEvent, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ingest/context-delivery" {
			var ev pulse.ContextDeliveryEvent
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &ev)
			select {
			case ch <- ev:
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return ts, ch
}

// TestGetContextAgentIDFallback: when get_context is called without an
// agent_id arg but session_init previously set the lastAgent, the context
// delivery event sent to pulse must carry that lastAgent as AgentID — not "".
func TestGetContextAgentIDFallback(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	ts, captured := fakeContextDeliveryServer(t)
	defer ts.Close()
	srv.SetPulseClient(pulse.NewClient(ts.URL, 2))

	// Simulate what session_init does: record the agent.
	srv.setLastAgent("claude-code")

	// Call get_context without agent_id in args (the common case).
	req := callTool(map[string]any{"entity": "AuthLogin"})
	_, err := srv.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}

	select {
	case ev := <-captured:
		if ev.AgentID != "claude-code" {
			t.Errorf("context delivery AgentID = %q, want \"claude-code\"", ev.AgentID)
		}
		if ev.ToolName != "get_context" {
			t.Errorf("ToolName = %q, want \"get_context\"", ev.ToolName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for context delivery event from pulse")
	}
}

// TestGetContextAgentIDExplicit: when agent_id IS provided in args it must
// take precedence over the lastAgent.
func TestGetContextAgentIDExplicit(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	ts, captured := fakeContextDeliveryServer(t)
	defer ts.Close()
	srv.SetPulseClient(pulse.NewClient(ts.URL, 2))

	srv.setLastAgent("session-default")

	// Explicit agent_id in args overrides lastAgent.
	req := callTool(map[string]any{"entity": "AuthLogin", "agent_id": "explicit-agent"})
	_, err := srv.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}

	select {
	case ev := <-captured:
		if ev.AgentID != "explicit-agent" {
			t.Errorf("context delivery AgentID = %q, want \"explicit-agent\"", ev.AgentID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for context delivery event from pulse")
	}
}

// TestGetFileContextAgentIDFallback: same scenario for get_file_context.
func TestGetFileContextAgentIDFallback(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	ts, captured := fakeContextDeliveryServer(t)
	defer ts.Close()
	srv.SetPulseClient(pulse.NewClient(ts.URL, 2))

	srv.setLastAgent("cursor-agent")

	// Call get_file_context without agent_id.
	req := callTool(map[string]any{"file": "auth.go"})
	_, err := srv.handleGetFileContext(ctx, req)
	if err != nil {
		t.Fatalf("handleGetFileContext: %v", err)
	}

	select {
	case ev := <-captured:
		if ev.AgentID != "cursor-agent" {
			t.Errorf("file context delivery AgentID = %q, want \"cursor-agent\"", ev.AgentID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for file context delivery event from pulse")
	}
}

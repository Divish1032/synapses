package mcp

import (
	"path/filepath"
	"testing"

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

// newPulseClient creates an in-process pulse client backed by a temp DB.
// The caller must call Close() when done.
func newPulseClient(t *testing.T) *pulse.Client {
	t.Helper()
	dir := t.TempDir()
	cli, err := pulse.New(filepath.Join(dir, "pulse.sqlite"))
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	return cli
}

// TestGetContextAgentIDFallback: when get_context is called without an
// agent_id arg but session_init previously set the lastAgent, the handler
// should succeed and record a pulse event (fire-and-forget).
func TestGetContextAgentIDFallback(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	pulseCli := newPulseClient(t)
	defer pulseCli.Close()
	srv.SetPulseClient(pulseCli)

	// Simulate what session_init does: record the agent.
	srv.setLastAgent("claude-code")

	// Call get_context without agent_id in args (the common case).
	req := callTool(map[string]any{"entity": "AuthLogin"})
	_, err := srv.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}

	// Verify that lastAgent is still correctly stored after the call.
	if got := srv.getLastAgent(); got != "claude-code" {
		t.Errorf("lastAgent = %q after handleGetContext, want \"claude-code\"", got)
	}
}

// TestGetContextAgentIDExplicit: when agent_id IS provided in args it must
// be set as the lastAgent.
func TestGetContextAgentIDExplicit(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	pulseCli := newPulseClient(t)
	defer pulseCli.Close()
	srv.SetPulseClient(pulseCli)

	srv.setLastAgent("session-default")

	// Explicit agent_id in args should update lastAgent.
	req := callTool(map[string]any{"entity": "AuthLogin", "agent_id": "explicit-agent"})
	_, err := srv.handleGetContext(ctx, req)
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}

	// lastAgent is NOT updated by get_context; only session_init updates it.
	// The handler should succeed and preserve the previous lastAgent.
	if got := srv.getLastAgent(); got != "session-default" {
		t.Errorf("lastAgent = %q after get_context, want \"session-default\" (only session_init updates it)", got)
	}
}

// TestGetFileContextAgentIDFallback: same scenario for get_file_context.
func TestGetFileContextAgentIDFallback(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	pulseCli := newPulseClient(t)
	defer pulseCli.Close()
	srv.SetPulseClient(pulseCli)

	srv.setLastAgent("cursor-agent")

	// Call get_file_context without agent_id.
	req := callTool(map[string]any{"file": "auth.go"})
	_, err := srv.handleGetFileContext(ctx, req)
	if err != nil {
		t.Fatalf("handleGetFileContext: %v", err)
	}

	// lastAgent should be preserved.
	if got := srv.getLastAgent(); got != "cursor-agent" {
		t.Errorf("lastAgent = %q after handleGetFileContext, want \"cursor-agent\"", got)
	}
}

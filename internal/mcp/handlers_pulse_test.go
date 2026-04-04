package mcp

import (
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/pulse"
	pulsestore "github.com/SynapsesOS/synapses/internal/pulse/pstore"
	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
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

// Sprint 23.9: TestGetFileContextAgentIDFallback removed — get_file_context tool removed.

// ---------------------------------------------------------------------------
// Sprint 30.6: project_value_metrics in session_init
// ---------------------------------------------------------------------------

// TestSessionInit_ProjectValueMetrics verifies that session_init includes
// project_value_metrics when pulse is available and that the three proxies
// (memory_retrievals, validate_blocks, files_from_graph) reflect seeded data.
func TestSessionInit_ProjectValueMetrics(t *testing.T) {
	pulsePath := filepath.Join(t.TempDir(), "pulse.sqlite")

	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	srv := newTestServer(t)
	srv.SetPulseClient(pc)

	// Open a direct pstore connection for synchronous inserts (avoids async collector).
	st, err := pulsestore.Open(pulsePath)
	if err != nil {
		t.Fatalf("pulsestore.Open: %v", err)
	}
	defer st.Close()

	// Seed: 3 recall_hits + 1 cross_session_hit = 4 memory retrievals.
	for i := 0; i < 3; i++ {
		if err := st.InsertMemoryOp(pulsetypes.MemoryOperationEvent{
			Operation: "recall_hit",
		}); err != nil {
			t.Fatalf("InsertMemoryOp recall_hit %d: %v", i, err)
		}
	}
	if err := st.InsertMemoryOp(pulsetypes.MemoryOperationEvent{
		Operation: "cross_session_hit",
	}); err != nil {
		t.Fatalf("InsertMemoryOp cross_session_hit: %v", err)
	}
	// A recall_miss must NOT count.
	if err := st.InsertMemoryOp(pulsetypes.MemoryOperationEvent{
		Operation: "recall_miss",
	}); err != nil {
		t.Fatalf("InsertMemoryOp recall_miss: %v", err)
	}

	// Seed: 2 validation events with violations, 1 clean (violation_count=0).
	for _, vc := range []int{2, 1, 0} {
		if err := st.InsertValidationEvent(pulsetypes.ValidationEvent{
			ToolName:       "validate",
			ViolationCount: vc,
		}); err != nil {
			t.Fatalf("InsertValidationEvent %d: %v", vc, err)
		}
	}

	// Seed: 3 context deliveries — 2 refetched=false (from graph), 1 refetched=true.
	for _, refetched := range []bool{false, false, true} {
		if err := st.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
			ToolName:  "get_context",
			Refetched: refetched,
		}); err != nil {
			t.Fatalf("InsertContextDelivery: %v", err)
		}
	}

	// Call session_init with full scope so value metrics are included.
	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"scope":    "full",
	}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}
	m := mustResult(t, res, nil)

	vmRaw, ok := m["project_value_metrics"]
	if !ok {
		t.Fatal("project_value_metrics missing from session_init response")
	}
	vm, ok := vmRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("project_value_metrics: expected map, got %T", vmRaw)
	}

	checkInt := func(key string, want int) {
		t.Helper()
		v, ok := vm[key]
		if !ok {
			t.Errorf("%s: key missing from project_value_metrics", key)
			return
		}
		// JSON numbers decode as float64.
		got := int(v.(float64))
		if got != want {
			t.Errorf("%s: got %d, want %d", key, got, want)
		}
	}
	checkInt("memory_retrievals", 4)
	checkInt("validate_blocks", 2)
	checkInt("files_from_graph", 2)

	// Summary line must be non-empty.
	if summary, _ := vm["summary"].(string); summary == "" {
		t.Error("project_value_metrics.summary: expected non-empty string")
	}
}

// TestSessionInit_ProjectValueMetrics_AbsentInQuickMode verifies that
// project_value_metrics is NOT included when scope="quick".
func TestSessionInit_ProjectValueMetrics_AbsentInQuickMode(t *testing.T) {
	pulsePath := filepath.Join(t.TempDir(), "pulse.sqlite")
	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	srv := newTestServer(t)
	srv.SetPulseClient(pc)

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "quick-agent",
		"scope":    "quick",
	}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}
	m := mustResult(t, res, nil)

	if _, ok := m["project_value_metrics"]; ok {
		t.Error("project_value_metrics should NOT appear in quick mode")
	}
}

// TestSessionInit_ProjectValueMetrics_AbsentWithoutPulse verifies graceful
// degradation when pulse is not configured.
func TestSessionInit_ProjectValueMetrics_AbsentWithoutPulse(t *testing.T) {
	srv := newTestServer(t) // no pulse client

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "no-pulse"}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}
	m := mustResult(t, res, nil)

	// Should not error and should not contain project_value_metrics.
	if _, ok := m["project_value_metrics"]; ok {
		t.Error("project_value_metrics should NOT appear when pulse is nil")
	}
}

// TestSessionInit_ProjectValueMetrics_AllZeroOmitted verifies that when pulse
// is active but no events have been recorded yet (first session), the
// project_value_metrics field is omitted rather than showing all-zero noise.
func TestSessionInit_ProjectValueMetrics_AllZeroOmitted(t *testing.T) {
	pulsePath := filepath.Join(t.TempDir(), "pulse.sqlite")
	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	srv := newTestServer(t)
	srv.SetPulseClient(pc)
	// No events seeded — all metrics will be zero.

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "first-session-agent",
		"scope":    "full",
	}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}
	m := mustResult(t, res, nil)

	if _, ok := m["project_value_metrics"]; ok {
		t.Error("project_value_metrics should be omitted when all metrics are zero (first session)")
	}
}

// TestSessionInit_ProjectValueMetrics_AbsentInResumeMode verifies that
// project_value_metrics is NOT included when scope="resume".
func TestSessionInit_ProjectValueMetrics_AbsentInResumeMode(t *testing.T) {
	pulsePath := filepath.Join(t.TempDir(), "pulse.sqlite")
	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	srv := newTestServer(t)
	srv.SetPulseClient(pc)

	// Seed data so metrics would be non-zero if the field were included.
	st, err := pulsestore.Open(pulsePath)
	if err != nil {
		t.Fatalf("pulsestore.Open: %v", err)
	}
	defer st.Close()
	if err := st.InsertMemoryOp(pulsetypes.MemoryOperationEvent{Operation: "recall_hit"}); err != nil {
		t.Fatalf("InsertMemoryOp: %v", err)
	}

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "resume-agent",
		"scope":    "resume",
	}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}
	m := mustResult(t, res, nil)

	if _, ok := m["project_value_metrics"]; ok {
		t.Error("project_value_metrics should NOT appear in resume mode")
	}
}

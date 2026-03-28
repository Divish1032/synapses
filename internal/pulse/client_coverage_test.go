package pulse

import (
	"path/filepath"
	"testing"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	dir := t.TempDir()
	c, err := New(filepath.Join(dir, "pulse_test.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestNew_And_Close(t *testing.T) {
	c := testClient(t)
	if c == nil {
		t.Fatal("client is nil")
	}
}

func TestNilClient_Methods(t *testing.T) {
	var c *Client
	// All methods should be nil-safe
	c.RecordToolCall(ToolCallEvent{ToolName: "test"})
	c.RecordContextDelivery(ContextDeliveryEvent{ToolName: "test"})
	c.RecordSessionEvent("agent", "proj", "start")
	c.RecordOutcomeSignal(OutcomeSignalEvent{SignalType: "task_done"})
	c.RecordSessionModel("agent", "proj", "model", "provider")
	c.RecordBrainUsage(BrainUsageEvent{Model: "test"})
	c.RecordAgentLLMUsage(AgentLLMUsageEvent{Model: "test"})
	c.Close()

	if c.FetchEffectiveness("proj", 2) != nil {
		t.Error("expected nil from nil client")
	}

	sum := c.GetSummary(7)
	if sum == nil {
		t.Fatal("expected non-nil summary even from nil client")
	}
	if sum.Days != 7 {
		t.Errorf("days: got %d, want 7", sum.Days)
	}
}

func TestRecordSessionModel_EmptyModel(t *testing.T) {
	c := testClient(t)
	// Empty model should be no-op
	c.RecordSessionModel("agent", "proj", "", "provider")
}

func TestRecordToolCall(t *testing.T) {
	c := testClient(t)
	c.RecordToolCall(ToolCallEvent{
		ToolName:      "get_context",
		AgentID:       "agent-1",
		DurationMs:    42,
		Success:       true,
		ResponseBytes: 1024,
	})
}

func TestRecordContextDelivery(t *testing.T) {
	c := testClient(t)
	c.RecordContextDelivery(ContextDeliveryEvent{
		ToolName:       "get_context",
		ResponseTokens: 100,
		BaselineTokens: 400,
	})
}

func TestRecordSessionEvent(t *testing.T) {
	c := testClient(t)
	c.RecordSessionEvent("agent-1", "proj-1", "start")
	c.RecordSessionEvent("agent-1", "proj-1", "end")
}

func TestRecordBrainUsage(t *testing.T) {
	c := testClient(t)
	c.RecordBrainUsage(BrainUsageEvent{
		Model: "test-model", PromptTokens: 100,
	})
}

func TestRecordAgentLLMUsage(t *testing.T) {
	c := testClient(t)
	c.RecordAgentLLMUsage(AgentLLMUsageEvent{
		Model: "claude-sonnet-4-6", InputTokens: 1000,
	})
}

func TestGetSummary(t *testing.T) {
	c := testClient(t)
	sum := c.GetSummary(7)
	if sum == nil {
		t.Fatal("expected non-nil summary")
	}
	if sum.Days != 7 {
		t.Errorf("days: got %d, want 7", sum.Days)
	}
	if sum.Summary == nil {
		t.Error("expected non-nil Summary field")
	}
}

func TestGetSummary_DefaultDays(t *testing.T) {
	c := testClient(t)
	sum := c.GetSummary(0) // should default to 7
	if sum.Days != 7 {
		t.Errorf("days: got %d, want 7", sum.Days)
	}
}

func TestGetSummary_LargeDays(t *testing.T) {
	c := testClient(t)
	sum := c.GetSummary(30) // timeline should use 30 instead of 14
	if sum.Days != 30 {
		t.Errorf("days: got %d, want 30", sum.Days)
	}
}

func TestFetchEffectiveness_EmptyDB(t *testing.T) {
	c := testClient(t)
	results := c.FetchEffectiveness("proj-1", 2)
	if results != nil {
		t.Error("expected nil for empty DB")
	}
}

func TestDefaultDBPath(t *testing.T) {
	path, err := DefaultDBPath()
	if err != nil {
		t.Fatalf("DefaultDBPath: %v", err)
	}
	if path == "" {
		t.Error("expected non-empty path")
	}
}

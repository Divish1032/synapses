package pulsestore

import (
	"path/filepath"
	"testing"
	"time"

	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "pulse_test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpen_And_Close(t *testing.T) {
	s := testStore(t)
	if s == nil {
		t.Fatal("store is nil")
	}
	// Close is deferred via t.Cleanup
}

func TestInsertToolCall(t *testing.T) {
	s := testStore(t)
	err := s.InsertToolCall(pulsetypes.ToolCallEvent{
		ToolName:      "get_context",
		AgentID:       "agent-1",
		ProjectID:     "proj-1",
		Entity:        "MyFunc",
		DurationMs:    42,
		Success:       true,
		ResponseBytes: 1024,
	})
	if err != nil {
		t.Fatalf("InsertToolCall: %v", err)
	}
}

func TestInsertContextDelivery(t *testing.T) {
	s := testStore(t)
	err := s.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName:       "get_context",
		AgentID:        "agent-1",
		ProjectID:      "proj-1",
		Entity:         "MyFunc",
		ResponseBytes:  500,
		ResponseTokens: 100,
		BaselineTokens: 400,
		NodesDelivered: 5,
		Truncated:      false,
		CacheHit:       true,
		BrainEnriched:  false,
	})
	if err != nil {
		t.Fatalf("InsertContextDelivery: %v", err)
	}
}

func TestInsertBrainUsage(t *testing.T) {
	s := testStore(t)
	err := s.InsertBrainUsage(pulsetypes.BrainUsageEvent{
		Model:            "test-model",
		Tier:             "ingest",
		Endpoint:         "/v1/ingest",
		PromptTokens:     200,
		CompletionTokens: 100,
		DurationMs:       50,
		CostUSD:          0.01,
	})
	if err != nil {
		t.Fatalf("InsertBrainUsage: %v", err)
	}
}

func TestUpsertSession_StartEnd(t *testing.T) {
	s := testStore(t)

	err := s.UpsertSession("sess-1", "agent-1", "proj-1", "start")
	if err != nil {
		t.Fatalf("UpsertSession start: %v", err)
	}

	err = s.UpsertSession("sess-1", "agent-1", "proj-1", "end")
	if err != nil {
		t.Fatalf("UpsertSession end: %v", err)
	}

	err = s.UpsertSession("sess-1", "agent-1", "proj-1", "task_done")
	if err != nil {
		t.Fatalf("UpsertSession task_done: %v", err)
	}

	// Unknown event type — should be no-op
	err = s.UpsertSession("sess-1", "agent-1", "proj-1", "unknown")
	if err != nil {
		t.Fatalf("UpsertSession unknown: %v", err)
	}
}

func TestUpdateSessionStats(t *testing.T) {
	s := testStore(t)
	err := s.UpdateSessionStats("sess-1", "agent-1", "proj-1", 100, 0.05)
	if err != nil {
		t.Fatalf("UpdateSessionStats: %v", err)
	}

	// Call again to test upsert path
	err = s.UpdateSessionStats("sess-1", "agent-1", "proj-1", 200, 0.10)
	if err != nil {
		t.Fatalf("UpdateSessionStats 2nd: %v", err)
	}
}

func TestAddSessionTokensSaved(t *testing.T) {
	s := testStore(t)
	err := s.AddSessionTokensSaved("sess-1", "agent-1", "proj-1", 500, 0.25)
	if err != nil {
		t.Fatalf("AddSessionTokensSaved: %v", err)
	}
}

func TestUpsertPricing(t *testing.T) {
	s := testStore(t)
	err := s.UpsertPricing("test-model", 5.0, 15.0, "test")
	if err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	in, out, found := s.GetPricing("test-model")
	if !found {
		t.Fatal("pricing not found")
	}
	if in != 5.0 || out != 15.0 {
		t.Errorf("pricing: got in=%.2f out=%.2f, want 5.0/15.0", in, out)
	}
}

func TestGetPricing_NotFound(t *testing.T) {
	s := testStore(t)
	_, _, found := s.GetPricing("nonexistent-model")
	if found {
		t.Error("expected not found for nonexistent model")
	}
}

func TestGetPricing_DefaultModels(t *testing.T) {
	s := testStore(t)
	in, _, found := s.GetPricing("gpt-4o")
	if !found {
		t.Fatal("default gpt-4o pricing not found")
	}
	if in != 2.50 {
		t.Errorf("gpt-4o input pricing: got %.2f, want 2.50", in)
	}
}

func TestUpsertDailyRollup(t *testing.T) {
	s := testStore(t)
	today := time.Now().UTC().Format("2006-01-02")
	err := s.UpsertDailyRollup(today, "tokens_saved", 1000)
	if err != nil {
		t.Fatalf("UpsertDailyRollup: %v", err)
	}

	// Upsert same day+metric — should replace
	err = s.UpsertDailyRollup(today, "tokens_saved", 2000)
	if err != nil {
		t.Fatalf("UpsertDailyRollup 2nd: %v", err)
	}
}

func TestGetSummary(t *testing.T) {
	s := testStore(t)

	// Insert some data
	_ = s.InsertToolCall(pulsetypes.ToolCallEvent{ToolName: "get_context", DurationMs: 50, Success: true})
	_ = s.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName: "get_context", ResponseTokens: 100, BaselineTokens: 400,
	})
	_ = s.UpsertSession("sess-1", "agent-1", "proj-1", "start")

	sum, err := s.GetSummary(7)
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if sum.TotalToolCalls != 1 {
		t.Errorf("TotalToolCalls: got %d, want 1", sum.TotalToolCalls)
	}
	if sum.ContextDeliveries != 1 {
		t.Errorf("ContextDeliveries: got %d, want 1", sum.ContextDeliveries)
	}
	if sum.TokensSaved != 300 {
		t.Errorf("TokensSaved: got %d, want 300", sum.TokensSaved)
	}
}

func TestGetSummaryForDay(t *testing.T) {
	s := testStore(t)
	today := time.Now().UTC().Format("2006-01-02")
	sum, err := s.GetSummaryForDay(today)
	if err != nil {
		t.Fatalf("GetSummaryForDay: %v", err)
	}
	if sum.TotalToolCalls != 0 {
		t.Error("expected 0 tool calls on empty DB")
	}
}

func TestGetTimeline(t *testing.T) {
	s := testStore(t)
	_ = s.InsertToolCall(pulsetypes.ToolCallEvent{ToolName: "test", Success: true})

	points, err := s.GetTimeline(7)
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if len(points) == 0 {
		t.Error("expected at least one timeline point")
	}
}

func TestGetToolStats(t *testing.T) {
	s := testStore(t)
	_ = s.InsertToolCall(pulsetypes.ToolCallEvent{ToolName: "get_context", DurationMs: 100, Success: true})
	_ = s.InsertToolCall(pulsetypes.ToolCallEvent{ToolName: "get_context", DurationMs: 200, Success: false})

	stats, err := s.GetToolStats(7)
	if err != nil {
		t.Fatalf("GetToolStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 tool stat, got %d", len(stats))
	}
	if stats[0].Calls != 2 {
		t.Errorf("Calls: got %d, want 2", stats[0].Calls)
	}
}

func TestGetAgentStats(t *testing.T) {
	s := testStore(t)
	_ = s.UpsertSession("sess-1", "agent-1", "proj-1", "start")
	_ = s.UpdateSessionStats("sess-1", "agent-1", "proj-1", 100, 0.05)

	stats, err := s.GetAgentStats(7)
	if err != nil {
		t.Fatalf("GetAgentStats: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("expected at least one agent stat")
	}
}

func TestGetBrainCosts(t *testing.T) {
	s := testStore(t)
	_ = s.InsertBrainUsage(pulsetypes.BrainUsageEvent{
		Model: "test-model", Tier: "ingest", PromptTokens: 100, CompletionTokens: 50,
	})

	costs, err := s.GetBrainCosts(7)
	if err != nil {
		t.Fatalf("GetBrainCosts: %v", err)
	}
	if costs.TotalTokens != 150 {
		t.Errorf("TotalTokens: got %d, want 150", costs.TotalTokens)
	}
}

func TestEventCount(t *testing.T) {
	s := testStore(t)
	count, err := s.EventCount()
	if err != nil {
		t.Fatalf("EventCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 events, got %d", count)
	}

	_ = s.InsertToolCall(pulsetypes.ToolCallEvent{ToolName: "test", Success: true})
	count, err = s.EventCount()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 event, got %d", count)
	}
}

func TestPruneOldEvents(t *testing.T) {
	s := testStore(t)
	_ = s.InsertToolCall(pulsetypes.ToolCallEvent{ToolName: "test", Success: true})

	deleted, err := s.PruneOldEvents(0) // 0 days = prune everything older than now
	if err != nil {
		t.Fatalf("PruneOldEvents: %v", err)
	}
	// Events just inserted should not be pruned (they're from "now")
	_ = deleted
}

func TestInsertOutcomeSignal(t *testing.T) {
	s := testStore(t)
	err := s.InsertOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		ProjectID:  "proj-1",
		AgentID:    "agent-1",
		Entity:     "MyFunc",
		SignalType: "task_done",
		Count:      1,
	})
	if err != nil {
		t.Fatalf("InsertOutcomeSignal: %v", err)
	}
}

func TestGetEffectiveness(t *testing.T) {
	s := testStore(t)
	// Insert enough signals
	for i := 0; i < 3; i++ {
		_ = s.InsertOutcomeSignal(pulsetypes.OutcomeSignalEvent{
			ProjectID: "proj-1", Entity: "MyFunc", SignalType: "task_done", Count: 1,
		})
	}
	_ = s.InsertOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		ProjectID: "proj-1", Entity: "MyFunc", SignalType: "correction", Count: 1,
	})

	results, err := s.GetEffectiveness("proj-1", 2)
	if err != nil {
		t.Fatalf("GetEffectiveness: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].Entity != "MyFunc" {
		t.Errorf("entity: got %s, want MyFunc", results[0].Entity)
	}
}

func TestUpdateSessionModel(t *testing.T) {
	s := testStore(t)
	err := s.UpdateSessionModel("sess-1", "agent-1", "proj-1", "claude-sonnet-4-6", "anthropic")
	if err != nil {
		t.Fatalf("UpdateSessionModel: %v", err)
	}
}

func TestInsertAgentLLMUsage(t *testing.T) {
	s := testStore(t)
	err := s.InsertAgentLLMUsage(pulsetypes.AgentLLMUsageEvent{
		SessionID:    "sess-1",
		AgentID:      "agent-1",
		Model:        "claude-sonnet-4-6",
		Provider:     "anthropic",
		InputTokens:  1000,
		OutputTokens: 500,
		CostUSD:      0.05,
	})
	if err != nil {
		t.Fatalf("InsertAgentLLMUsage: %v", err)
	}
}

func TestGetAgentLLMStats(t *testing.T) {
	s := testStore(t)
	_ = s.InsertAgentLLMUsage(pulsetypes.AgentLLMUsageEvent{
		Model: "claude-sonnet-4-6", Provider: "anthropic", InputTokens: 1000, OutputTokens: 500,
	})

	stats, err := s.GetAgentLLMStats(7)
	if err != nil {
		t.Fatalf("GetAgentLLMStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 stat, got %d", len(stats))
	}
	if stats[0].InputTokens != 1000 {
		t.Errorf("InputTokens: got %d, want 1000", stats[0].InputTokens)
	}
}

func TestTopEntities(t *testing.T) {
	s := testStore(t)
	_ = s.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName: "get_context", Entity: "MyFunc", ResponseTokens: 100, BaselineTokens: 200,
	})

	entities, err := s.TopEntities(7, 10)
	if err != nil {
		t.Fatalf("TopEntities: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(entities))
	}
}

func TestMigrateColumns_Idempotent(t *testing.T) {
	s := testStore(t)
	// Running migrate again should not fail
	err := s.migrateColumns()
	if err != nil {
		t.Fatalf("migrateColumns (2nd): %v", err)
	}
}

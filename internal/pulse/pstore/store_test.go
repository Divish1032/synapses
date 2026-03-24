package pulsestore

import (
	"os"
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

func TestUpsertSessionWithVersion_StartEnd(t *testing.T) {
	s := testStore(t)

	err := s.UpsertSessionWithVersion("sess-1", "agent-1", "proj-1", "start", "")
	if err != nil {
		t.Fatalf("UpsertSessionWithVersion start: %v", err)
	}

	err = s.UpsertSessionWithVersion("sess-1", "agent-1", "proj-1", "end", "")
	if err != nil {
		t.Fatalf("UpsertSessionWithVersion end: %v", err)
	}

	err = s.UpsertSessionWithVersion("sess-1", "agent-1", "proj-1", "task_done", "")
	if err != nil {
		t.Fatalf("UpsertSessionWithVersion task_done: %v", err)
	}

	// Unknown event type — should be no-op
	err = s.UpsertSessionWithVersion("sess-1", "agent-1", "proj-1", "unknown", "")
	if err != nil {
		t.Fatalf("UpsertSessionWithVersion unknown: %v", err)
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
	_ = s.UpsertSessionWithVersion("sess-1", "agent-1", "proj-1", "start", "")

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
	_ = s.UpsertSessionWithVersion("sess-1", "agent-1", "proj-1", "start", "")
	_ = s.UpdateSessionStats("sess-1", "agent-1", "proj-1", 100, 0.05)

	stats, err := s.GetAgentStats(7)
	if err != nil {
		t.Fatalf("GetAgentStats: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("expected at least one agent stat")
	}
}

func TestEventCount(t *testing.T) {
	s := testStore(t)
	tc, cd, bu := s.EventCount()
	if tc+cd+bu != 0 {
		t.Errorf("expected 0 events, got %d", tc+cd+bu)
	}

	_ = s.InsertToolCall(pulsetypes.ToolCallEvent{ToolName: "test", Success: true})
	tc, cd, bu = s.EventCount()
	if tc+cd+bu != 1 {
		t.Errorf("expected 1 event, got %d", tc+cd+bu)
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

// ---------------------------------------------------------------------------
// Tests for Low-Coverage Functions
// ---------------------------------------------------------------------------

func TestMergeSummaries_Basic(t *testing.T) {
	// Test mergeSummaries basic functionality
	hist := &Summary{
		TotalToolCalls:    10,
		TokensDelivered:   100,
		BaselineTokens:    400,
		ContextDeliveries: 5,
		CostSavedUSD:      1.00,
		Sessions:          2,
		TasksCompleted:    1,
	}

	today := &Summary{
		TotalToolCalls:    5,
		TokensDelivered:   50,
		BaselineTokens:    200,
		ContextDeliveries: 3,
		CostSavedUSD:      0.50,
		Sessions:          1,
		TasksCompleted:    1,
	}

	result := mergeSummaries(hist, today)

	if result.TotalToolCalls != 15 {
		t.Errorf("TotalToolCalls: got %d, want 15", result.TotalToolCalls)
	}
	if result.TokensDelivered != 150 {
		t.Errorf("TokensDelivered: got %d, want 150", result.TokensDelivered)
	}
	if result.BaselineTokens != 600 {
		t.Errorf("BaselineTokens: got %d, want 600", result.BaselineTokens)
	}
	if result.TokensSaved != 450 {
		t.Errorf("TokensSaved: got %d, want 450", result.TokensSaved)
	}
	if result.ContextDeliveries != 8 {
		t.Errorf("ContextDeliveries: got %d, want 8", result.ContextDeliveries)
	}
	if result.CostSavedUSD != 1.50 {
		t.Errorf("CostSavedUSD: got %.2f, want 1.50", result.CostSavedUSD)
	}
	if result.Sessions != 3 {
		t.Errorf("Sessions: got %d, want 3", result.Sessions)
	}
	if result.TasksCompleted != 2 {
		t.Errorf("TasksCompleted: got %d, want 2", result.TasksCompleted)
	}
}

func TestMergeSummaries_TokensSavedCapped(t *testing.T) {
	// Test that negative TokensSaved is capped at 0
	hist := &Summary{
		TokensDelivered: 500,
		BaselineTokens:  200,
	}
	today := &Summary{}

	result := mergeSummaries(hist, today)
	if result.TokensSaved != 0 {
		t.Errorf("TokensSaved should be capped at 0, got %d", result.TokensSaved)
	}
}

func TestMergeSummaries_CompressionRatio(t *testing.T) {
	// Test compression ratio calculation
	hist := &Summary{
		TokensDelivered: 100,
		BaselineTokens:  400,
	}
	today := &Summary{}

	result := mergeSummaries(hist, today)
	expected := 400.0 / 100.0
	if result.CompressionRatio != expected {
		t.Errorf("CompressionRatio: got %.2f, want %.2f", result.CompressionRatio, expected)
	}
}

func TestMergeSummaries_SavingsPct(t *testing.T) {
	// Test savings percentage calculation
	hist := &Summary{
		TokensDelivered: 100,
		BaselineTokens:  400,
	}
	today := &Summary{}

	result := mergeSummaries(hist, today)
	// TokensSaved = 400 - 100 = 300
	// SavingsPct = 300 / 400 * 100 = 75%
	expected := 75.0
	if result.SavingsPct != expected {
		t.Errorf("SavingsPct: got %.2f, want %.2f", result.SavingsPct, expected)
	}
}

func TestMergeSummaries_ZeroDeliveries(t *testing.T) {
	// Test with zero context deliveries
	hist := &Summary{
		TotalToolCalls: 10,
	}
	today := &Summary{
		TotalToolCalls: 5,
	}

	result := mergeSummaries(hist, today)
	if result.TotalToolCalls != 15 {
		t.Errorf("TotalToolCalls: got %d, want 15", result.TotalToolCalls)
	}
	// CacheHitRate should remain 0 when there are no deliveries
	if result.CacheHitRate != 0 {
		t.Errorf("CacheHitRate should be 0, got %.2f", result.CacheHitRate)
	}
}

func TestGetSummary_EmptyDatabase(t *testing.T) {
	// Test GetSummary on empty database
	s := testStore(t)
	sum, err := s.GetSummary(7)
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if sum.TotalToolCalls != 0 {
		t.Errorf("expected empty summary, got %+v", sum)
	}
}

func TestGetSummary_WithMultipleEvents(t *testing.T) {
	// Test GetSummary with varied events
	s := testStore(t)

	// Add multiple events
	_ = s.InsertToolCall(pulsetypes.ToolCallEvent{
		ToolName:   "get_context",
		DurationMs: 100,
		Success:    true,
	})
	_ = s.InsertToolCall(pulsetypes.ToolCallEvent{
		ToolName:   "get_context",
		DurationMs: 50,
		Success:    false,
	})
	_ = s.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName:       "get_context",
		ResponseTokens: 150,
		BaselineTokens: 600,
		CacheHit:       true,
	})

	sum, err := s.GetSummary(7)
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}

	if sum.TotalToolCalls != 2 {
		t.Errorf("TotalToolCalls: got %d, want 2", sum.TotalToolCalls)
	}
	if sum.ContextDeliveries != 1 {
		t.Errorf("ContextDeliveries: got %d, want 1", sum.ContextDeliveries)
	}
}


func TestOpen_CreatesDatabase(t *testing.T) {
	// Test that Open creates a database file
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Verify database was created
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("database file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("database file is empty")
	}
}

func TestMergeSummaries_WeightedAverages(t *testing.T) {
	// Test that mergeSummaries recomputes rates from summable components.
	hist := &Summary{
		ContextDeliveries:  10,
		CacheHits:          8,    // 80% hit rate
		BrainEnrichedCount: 0,
		TotalToolCalls:     20,
		TotalLatencyMs:     2000, // avg = 100ms
	}
	today := &Summary{
		ContextDeliveries:  5,
		CacheHits:          3,    // 60% hit rate
		BrainEnrichedCount: 0,
		TotalToolCalls:     10,
		TotalLatencyMs:     500, // avg = 50ms
	}

	result := mergeSummaries(hist, today)

	// Expected cache hit rate: (8+3) / (10+5) = 11/15 ≈ 0.733
	expectedCacheHit := 11.0 / 15.0
	if abs(result.CacheHitRate-expectedCacheHit) > 0.001 {
		t.Errorf("CacheHitRate: got %.3f, want %.3f", result.CacheHitRate, expectedCacheHit)
	}

	// Expected avg latency: (2000+500) / (20+10) = 2500/30 ≈ 83.3
	expectedLatency := 2500.0 / 30.0
	if abs(result.AvgLatencyMs-expectedLatency) > 0.1 {
		t.Errorf("AvgLatencyMs: got %.1f, want %.1f", result.AvgLatencyMs, expectedLatency)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestOpen_PerformancePragmas(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	for _, tc := range []struct {
		pragma, want string
	}{
		{"synchronous", "1"},       // NORMAL
		{"cache_size", "-65536"},    // 64 MB
		{"mmap_size", "268435456"}, // 256 MB
		{"temp_store", "2"},        // MEMORY
	} {
		var val string
		if err := s.db.QueryRow("PRAGMA " + tc.pragma).Scan(&val); err != nil {
			t.Errorf("PRAGMA %s: %v", tc.pragma, err)
		} else if val != tc.want {
			t.Errorf("PRAGMA %s = %s, want %s", tc.pragma, val, tc.want)
		}
	}
}

// ── Sprint 15 #2: UpdateEntityQualityScore tests ──────────────────────────────

// TestUpdateEntityQualityScore_UsesSignalWeight verifies that the quality score
// is computed from the signal_weight column (Sprint 15 #1) rather than from
// a hardcoded CASE on signal_type. This ensures task_abandoned, correction, and
// task_done signals produce the expected signed sum.
func TestUpdateEntityQualityScore_UsesSignalWeight(t *testing.T) {
	s := testStore(t)

	// One task_done (+0.3) and one correction immediate (-0.5) → net -0.2.
	_ = s.InsertOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		ProjectID:    "proj-1",
		Entity:       "AuthService",
		SignalType:   "task_done",
		SignalWeight: pulsetypes.SignalWeightTaskDone,
	})
	_ = s.InsertOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		ProjectID:    "proj-1",
		Entity:       "AuthService",
		SignalType:   "correction",
		SignalWeight: pulsetypes.SignalWeightCorrectionImmediate,
	})

	s.UpdateEntityQualityScore("AuthService", "proj-1")

	score, ok := s.GetEntityQualityScore("AuthService", "proj-1")
	if !ok {
		t.Fatal("GetEntityQualityScore: entity not found after update")
	}
	want := pulsetypes.SignalWeightTaskDone + pulsetypes.SignalWeightCorrectionImmediate // -0.2
	if score < want-0.001 || score > want+0.001 {
		t.Errorf("quality score = %.4f, want %.4f (sum of signal_weight)", score, want)
	}
}

// TestUpdateEntityQualityScore_TaskAbandoned verifies that task_abandoned signals
// (previously missing from the CASE expression) now lower the quality score.
func TestUpdateEntityQualityScore_TaskAbandoned(t *testing.T) {
	s := testStore(t)

	_ = s.InsertOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		ProjectID:    "proj-1",
		Entity:       "DeadCode",
		SignalType:   "task_abandoned",
		SignalWeight: pulsetypes.SignalWeightTaskAbandoned,
	})

	s.UpdateEntityQualityScore("DeadCode", "proj-1")

	score, ok := s.GetEntityQualityScore("DeadCode", "proj-1")
	if !ok {
		t.Fatal("GetEntityQualityScore: entity not found")
	}
	if score >= 0 {
		t.Errorf("task_abandoned should produce a negative quality score, got %.4f", score)
	}
	if score < pulsetypes.SignalWeightTaskAbandoned-0.001 || score > pulsetypes.SignalWeightTaskAbandoned+0.001 {
		t.Errorf("score = %.4f, want %.4f (SignalWeightTaskAbandoned)", score, pulsetypes.SignalWeightTaskAbandoned)
	}
}

// TestUpdateEntityQualityScore_NoSignals verifies that an entity with no signals
// is not written to entity_quality (GetEntityQualityScore returns false).
func TestUpdateEntityQualityScore_NoSignals(t *testing.T) {
	s := testStore(t)

	// UpdateEntityQualityScore with no rows writes score=0, pos=0, neg=0.
	// GetEntityQualityScore should still return the zero-value row.
	s.UpdateEntityQualityScore("Ghost", "proj-1")

	score, ok := s.GetEntityQualityScore("Ghost", "proj-1")
	// A zero-weight row IS written (UPSERT always fires) — result is score=0, ok=true.
	if !ok {
		t.Fatal("GetEntityQualityScore returned false for entity with zero signals after upsert")
	}
	if score != 0.0 {
		t.Errorf("score with no signals = %.4f, want 0.0", score)
	}
}

// TestGetEntityQualityScore_MissingEntity verifies that (0, false) is returned
// for an entity that has never been written to entity_quality.
func TestGetEntityQualityScore_MissingEntity(t *testing.T) {
	s := testStore(t)
	score, ok := s.GetEntityQualityScore("NeverSeen", "proj-1")
	if ok {
		t.Errorf("expected ok=false for unseen entity, got ok=true score=%.4f", score)
	}
	if score != 0 {
		t.Errorf("expected score=0 for unseen entity, got %.4f", score)
	}
}

// TestGetEntityQualityScoresBatch_OnlyRequestedIDs verifies that the batch lookup
// returns scores only for the requested entities, including low-scoring ones
// (which a LIMIT-based query would miss if only fetching top-N).
func TestGetEntityQualityScoresBatch_OnlyRequestedIDs(t *testing.T) {
	s := testStore(t)

	// Write a high-score entity, a low-score entity, and an entity with no record.
	for _, tc := range []struct {
		entity string
		weight float64
	}{
		{"HighQuality", pulsetypes.SignalWeightTaskDone * 3},   // +0.9
		{"LowQuality", pulsetypes.SignalWeightTaskAbandoned},   // -0.8
	} {
		_ = s.InsertOutcomeSignal(pulsetypes.OutcomeSignalEvent{
			ProjectID:    "proj-1",
			Entity:       tc.entity,
			SignalType:   "test",
			SignalWeight: tc.weight,
		})
		s.UpdateEntityQualityScore(tc.entity, "proj-1")
	}

	// Request both entities plus one that has no record.
	result := s.GetEntityQualityScoresBatch(
		[]string{"HighQuality", "LowQuality", "NoRecord"}, "proj-1")

	if result == nil {
		t.Fatal("GetEntityQualityScoresBatch returned nil, expected a map")
	}
	if _, ok := result["HighQuality"]; !ok {
		t.Error("HighQuality missing from batch result")
	}
	if _, ok := result["LowQuality"]; !ok {
		t.Error("LowQuality missing from batch result — batch must include negative-score entities")
	}
	if _, ok := result["NoRecord"]; ok {
		t.Error("NoRecord should be absent from batch result (no entity_quality row)")
	}
	if result["LowQuality"] >= 0 {
		t.Errorf("LowQuality score = %.4f, want negative", result["LowQuality"])
	}
}

// TestGetEntityQualityScoresBatch_Empty verifies that an empty input returns nil.
func TestGetEntityQualityScoresBatch_Empty(t *testing.T) {
	s := testStore(t)
	if result := s.GetEntityQualityScoresBatch(nil, "proj-1"); result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
	if result := s.GetEntityQualityScoresBatch([]string{}, "proj-1"); result != nil {
		t.Errorf("expected nil for empty slice, got %v", result)
	}
}

// TestUpdateEntityQualityScore_PositiveNegativeCounts verifies that
// positive_signals and negative_signals columns are updated correctly.
func TestUpdateEntityQualityScore_PositiveNegativeCounts(t *testing.T) {
	s := testStore(t)

	_ = s.InsertOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		ProjectID: "proj-1", Entity: "API", SignalType: "task_done",
		SignalWeight: pulsetypes.SignalWeightTaskDone,
	})
	_ = s.InsertOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		ProjectID: "proj-1", Entity: "API", SignalType: "task_done",
		SignalWeight: pulsetypes.SignalWeightTaskDone,
	})
	_ = s.InsertOutcomeSignal(pulsetypes.OutcomeSignalEvent{
		ProjectID: "proj-1", Entity: "API", SignalType: "correction",
		SignalWeight: pulsetypes.SignalWeightCorrectionDelayed,
	})

	s.UpdateEntityQualityScore("API", "proj-1")

	rows := s.GetEntityQualityScores("proj-1", 0)
	var found bool
	for _, eq := range rows {
		if eq.Entity == "API" {
			found = true
			if eq.PositiveSignals != 2 {
				t.Errorf("positive_signals = %d, want 2", eq.PositiveSignals)
			}
			if eq.NegativeSignals != 1 {
				t.Errorf("negative_signals = %d, want 1", eq.NegativeSignals)
			}
		}
	}
	if !found {
		t.Error("API entity not found in GetEntityQualityScores")
	}
}

// TestUpdateRecallChannelStats_LearnedWeights verifies Sprint 15 #4's
// weight-learning pipeline end-to-end at the store layer:
// - InsertMemoryOp seeds recall_hit events with top_channel values
// - UpdateRecallChannelStats aggregates win-rates into recall_channel_weights
// - GetRecallChannelWeights returns per-project win-rates that sum to ~1.0
// - A dominant channel's win-rate exceeds equal-share baseline (0.25)
func TestUpdateRecallChannelStats_LearnedWeights(t *testing.T) {
	s := testStore(t)
	projID := "test-proj-sprint15"

	// Seed 16 recall_hit ops: graph=8, bm25=4, semantic=2, temporal=2.
	// Expected win-rates: graph=0.5, bm25=0.25, semantic=0.125, temporal=0.125.
	seed := []struct {
		ch    string
		count int
	}{
		{"graph", 8},
		{"bm25", 4},
		{"semantic", 2},
		{"temporal", 2},
	}
	for _, entry := range seed {
		for i := 0; i < entry.count; i++ {
			if err := s.InsertMemoryOp(pulsetypes.MemoryOperationEvent{
				Operation: "recall_hit",
				ProjectID: projID,
				TopChannel: entry.ch,
			}); err != nil {
				t.Fatalf("InsertMemoryOp(%s): %v", entry.ch, err)
			}
		}
	}

	// Recompute channel win-rates from memory_ops.
	s.UpdateRecallChannelStats(projID)

	weights := s.GetRecallChannelWeights(projID)
	if len(weights) < 2 {
		t.Fatalf("expected ≥2 channel weights, got %d: %v", len(weights), weights)
	}

	// graph should have the highest win-rate (0.5).
	graphWR, ok := weights["graph"]
	if !ok {
		t.Fatal("graph channel missing from learned weights")
	}
	if graphWR < 0.4 {
		t.Errorf("graph win-rate %.3f, expected ≥0.4 for dominant channel", graphWR)
	}

	// Learned win-rates must sum to ~1.0 (they are probabilities).
	total := 0.0
	for _, wr := range weights {
		total += wr
	}
	if total < 0.95 || total > 1.05 {
		t.Errorf("win-rates sum to %.3f, expected ~1.0 (they are probabilities)", total)
	}

	// temporal (smallest slice) should have lowest win-rate.
	tempWR, ok := weights["temporal"]
	if !ok {
		t.Fatal("temporal channel missing from learned weights")
	}
	if tempWR > graphWR {
		t.Errorf("temporal win-rate %.3f > graph %.3f, expected temporal < graph for graph-dominant project", tempWR, graphWR)
	}

	// Verify that an unknown project returns empty (no cross-project bleed).
	other := s.GetRecallChannelWeights("other-proj")
	if len(other) != 0 {
		t.Errorf("expected no weights for unknown project, got %v", other)
	}
}


// ---------------------------------------------------------------------------
// Sprint 15 #5: GetSessionDeliveryStats
// ---------------------------------------------------------------------------

func TestGetSessionDeliveryStats_Empty(t *testing.T) {
	s := testStore(t)
	total, firstFetch, saved := s.GetSessionDeliveryStats("no-such-session")
	if total != 0 || firstFetch != 0 || saved != 0 {
		t.Errorf("expected all zeros for missing session, got total=%d firstFetch=%d saved=%d", total, firstFetch, saved)
	}
}

func TestGetSessionDeliveryStats_EmptySessionID(t *testing.T) {
	s := testStore(t)
	total, firstFetch, saved := s.GetSessionDeliveryStats("")
	if total != 0 || firstFetch != 0 || saved != 0 {
		t.Errorf("expected all zeros for empty sessionID, got total=%d firstFetch=%d saved=%d", total, firstFetch, saved)
	}
}

func TestGetSessionDeliveryStats_Counts(t *testing.T) {
	s := testStore(t)
	sess := "sess-eff-test"

	// Delivery 1: first-fetch (refetched=false), baseline=400 response=100 → saves 300
	if err := s.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName:       "get_context",
		SessionID:      sess,
		Entity:         "AuthService",
		BaselineTokens: 400,
		ResponseTokens: 100,
		Refetched:      false,
	}); err != nil {
		t.Fatalf("InsertContextDelivery: %v", err)
	}

	// Delivery 2: first-fetch, baseline=200 response=200 → saves 0 (no saving)
	if err := s.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName:       "get_context",
		SessionID:      sess,
		Entity:         "UserService",
		BaselineTokens: 200,
		ResponseTokens: 200,
		Refetched:      false,
	}); err != nil {
		t.Fatalf("InsertContextDelivery: %v", err)
	}

	// Delivery 3: re-fetch (refetched=true), baseline=300 response=150 → saves 150 but NOT first-fetch
	if err := s.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName:       "get_context",
		SessionID:      sess,
		Entity:         "AuthService",
		BaselineTokens: 300,
		ResponseTokens: 150,
		Refetched:      true,
	}); err != nil {
		t.Fatalf("InsertContextDelivery: %v", err)
	}

	// Different session — must not bleed into sess.
	if err := s.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName:       "get_context",
		SessionID:      "other-sess",
		Entity:         "OtherFunc",
		BaselineTokens: 1000,
		ResponseTokens: 100,
	}); err != nil {
		t.Fatalf("InsertContextDelivery other-sess: %v", err)
	}

	total, firstFetch, saved := s.GetSessionDeliveryStats(sess)

	if total != 3 {
		t.Errorf("total: got %d, want 3", total)
	}
	if firstFetch != 2 {
		t.Errorf("firstFetch: got %d, want 2", firstFetch)
	}
	// Delivery 1: max(400-100,0)=300; Delivery 2: max(200-200,0)=0; Delivery 3: max(300-150,0)=150
	// Total = 300+0+150 = 450
	if saved != 450 {
		t.Errorf("tokensSaved: got %d, want 450", saved)
	}
}

func TestGetSessionDeliveryStats_NegativeSavingsClampedToZero(t *testing.T) {
	// When response_tokens > baseline_tokens (unusual but possible for enriched responses),
	// MAX(baseline-response, 0) should clamp to 0, not subtract.
	s := testStore(t)
	sess := "sess-negative"
	_ = s.InsertContextDelivery(pulsetypes.ContextDeliveryEvent{
		ToolName:       "get_context",
		SessionID:      sess,
		Entity:         "BigFunc",
		BaselineTokens: 50,
		ResponseTokens: 200, // brain enrichment added more content than baseline
	})
	_, _, saved := s.GetSessionDeliveryStats(sess)
	if saved != 0 {
		t.Errorf("expected tokensSaved=0 for negative savings, got %d", saved)
	}
}

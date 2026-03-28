package brain_test

// Brain integration tests — require a running Ollama with registered identities.
//
// Run with:
//
//	BRAIN_EVAL=1 go test ./internal/brain/... -run TestEval -v -timeout 300s
//
// Prerequisites:
//   - Ollama running at http://localhost:11434
//   - All 5 synapses/* identities registered (run register_models.sh)
//
// All tests skip automatically when BRAIN_EVAL != "1".

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/brain/archivist"
	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
)

func evalBrain(t *testing.T) brain.Brain {
	t.Helper()
	if os.Getenv("BRAIN_EVAL") != "1" {
		t.Skip("set BRAIN_EVAL=1 to run brain integration tests")
	}
	cfg := brainconfig.DefaultConfig()
	cfg.Enabled = true
	cfg.Backend = "ollama"
	cfg.OllamaURL = "http://localhost:11434"
	cfg.IntelligenceMode = brainconfig.ModeStandard
	cfg.AutoConfigureModels(0)
	cfg.TimeoutMS = 30000

	b := brain.New(cfg)
	if b == nil {
		t.Fatal("brain.New returned nil")
	}
	if !b.Available() {
		t.Skip("brain not available — Ollama not running or model not pulled")
	}
	return b
}

// ── Ingest ──────────────────────────────────────────────────────────────────

func TestEval_Ingest_NonTrivialNode(t *testing.T) {
	b := evalBrain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := b.Ingest(ctx, brain.IngestRequest{
		ProjectID: "eval-test",
		NodeID:    "auth-service-001",
		NodeName:  "AuthService",
		NodeType:  "struct",
		Package:   "auth",
		Code:      "type AuthService struct {\n\tdb *sql.DB\n\tcache *redis.Client\n\ttokenKey []byte\n}",
	})
	if err != nil {
		t.Fatalf("Ingest error: %v", err)
	}
	if resp.Summary == "" {
		t.Error("expected non-empty summary for non-trivial node")
	}
	if len(resp.Summary) < 20 {
		t.Errorf("summary too short (%d chars): %q", len(resp.Summary), resp.Summary)
	}
	t.Logf("Summary: %s", resp.Summary)
	t.Logf("Tags: %v", resp.Tags)
}

func TestEval_Ingest_TrivialNode_SkipsLLM(t *testing.T) {
	b := evalBrain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := b.Ingest(ctx, brain.IngestRequest{
		ProjectID: "eval-test",
		NodeID:    "test-helper-001",
		NodeName:  "TestAuthService_Login",
		NodeType:  "function",
		Package:   "auth_test",
		Code:      "func TestAuthService_Login(t *testing.T) { ... }",
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Ingest error: %v", err)
	}
	if resp.Summary == "" {
		t.Error("expected non-empty summary even for trivial node")
	}
	// Deterministic path should be <50ms (no LLM call).
	if elapsed > 500*time.Millisecond {
		t.Errorf("trivial node took %v — deterministic fast path may not have fired", elapsed)
	}
	t.Logf("Summary: %s (latency: %v)", resp.Summary, elapsed)
}

// ── Enrich ──────────────────────────────────────────────────────────────────

func TestEval_Enrich_ProducesInsight(t *testing.T) {
	b := evalBrain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := b.Enrich(ctx, brain.EnrichRequest{
		ProjectID:   "eval-test",
		RootID:      "auth-service-001",
		RootName:    "AuthService",
		RootType:    "struct",
		CalleeNames: []string{"db.Query", "cache.Get", "token.Sign"},
		CallerNames: []string{"HandleLogin", "HandleRegister", "middleware.Auth"},
		AllNodeIDs:  []string{"auth-service-001"},
	})
	if err != nil {
		t.Fatalf("Enrich error: %v", err)
	}
	if resp.Insight == "" {
		t.Error("expected non-empty insight")
	}
	t.Logf("Insight: %s", resp.Insight)
	t.Logf("Concerns: %v", resp.Concerns)
	t.Logf("LLMUsed: %v", resp.LLMUsed)
}

// ── Guardian ────────────────────────────────────────────────────────────────

func TestEval_Guardian_ExplainsViolation(t *testing.T) {
	b := evalBrain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := b.ExplainViolation(ctx, brain.ViolationRequest{
		RuleID:       "no-cross-layer-imports",
		RuleSeverity: "error",
		Description:  "API handlers must not import store implementations directly",
		SourceFile:   "internal/api/handler.go",
		TargetName:   "internal/store/sqlite.go",
	})
	if err != nil {
		t.Fatalf("ExplainViolation error: %v", err)
	}
	if resp.Explanation == "" {
		t.Error("expected non-empty explanation")
	}
	if resp.Fix == "" {
		t.Error("expected non-empty fix suggestion")
	}
	t.Logf("Explanation: %s", resp.Explanation)
	t.Logf("Fix: %s", resp.Fix)
}

// ── Coordinate ──────────────────────────────────────────────────────────────

func TestEval_Coordinate_NoConflict(t *testing.T) {
	b := evalBrain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := b.Coordinate(ctx, brain.CoordinateRequest{
		NewAgentID: "agent-2",
		NewScope:   "internal/auth",
		ConflictingClaims: []brain.WorkClaim{
			{AgentID: "agent-1", Scope: "internal/graph", ScopeType: "package"},
		},
	})
	if err != nil {
		t.Fatalf("Coordinate error: %v", err)
	}
	if resp.Suggestion == "" {
		t.Error("expected non-empty suggestion")
	}
	t.Logf("Suggestion: %s", resp.Suggestion)
	t.Logf("AlternativeScope: %s", resp.AlternativeScope)
}

func TestEval_Coordinate_WithConflict(t *testing.T) {
	b := evalBrain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := b.Coordinate(ctx, brain.CoordinateRequest{
		NewAgentID: "agent-2",
		NewScope:   "internal/store",
		ConflictingClaims: []brain.WorkClaim{
			{AgentID: "agent-1", Scope: "internal/store", ScopeType: "package"},
		},
	})
	if err != nil {
		t.Fatalf("Coordinate error: %v", err)
	}
	if resp.Suggestion == "" {
		t.Error("expected non-empty suggestion for overlapping scopes")
	}
	t.Logf("Suggestion: %s", resp.Suggestion)
	t.Logf("AlternativeScope: %s", resp.AlternativeScope)
}

// ── Memorize ────────────────────────────────────────────────────────────────

func TestEval_Memorize_RichSession(t *testing.T) {
	b := evalBrain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := b.Memorize(ctx, archivist.MemorizeRequest{
		SessionEvents: []archivist.SessionEvent{
			{Tool: "get_context", Entity: "AuthService", Result: "struct with 12 callers, 3 callees, hub node in auth package"},
			{Tool: "get_context", Entity: "TokenStore", Result: "interface with 5 implementations, used by AuthService"},
			{Tool: "get_impact", Entity: "AuthService", Result: "96 transitive dependents across 12 packages"},
		},
		ExistingMemory: []string{},
	})
	if err != nil {
		t.Fatalf("Memorize error: %v", err)
	}
	// A rich 3-event session should produce at least one memory.
	if len(resp.NewMemories) == 0 && len(resp.Annotations) == 0 {
		t.Error("expected at least one memory or annotation for a rich multi-event session")
	}
	for i, m := range resp.NewMemories {
		t.Logf("Memory[%d]: key=%s, content=%s, entities=%v", i, m.Key, m.Content, m.Entities)
	}
	for i, a := range resp.Annotations {
		t.Logf("Annotation[%d]: node=%s, note=%s", i, a.Node, a.Note)
	}
}

func TestEval_Memorize_TrivialSession(t *testing.T) {
	b := evalBrain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := b.Memorize(ctx, archivist.MemorizeRequest{
		SessionEvents: []archivist.SessionEvent{
			{Tool: "get_context", Entity: "Store", Result: "looked it up"},
		},
		ExistingMemory: []string{},
	})
	if err != nil {
		t.Fatalf("Memorize error: %v", err)
	}
	t.Logf("Trivial session: memories=%d, annotations=%d", len(resp.NewMemories), len(resp.Annotations))
}

// ── Stats ───────────────────────────────────────────────────────────────────

func TestEval_Stats_AccumulateAcrossCalls(t *testing.T) {
	b := evalBrain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Make a few calls to populate stats.
	_, _ = b.Ingest(ctx, brain.IngestRequest{
		ProjectID: "eval-test", NodeID: "stats-1", NodeName: "init",
		NodeType: "function", Package: "main",
	})
	_, _ = b.Ingest(ctx, brain.IngestRequest{
		ProjectID: "eval-test", NodeID: "stats-2", NodeName: "HandleRequest",
		NodeType: "function", Package: "api",
		Code: "func HandleRequest(w http.ResponseWriter, r *http.Request) { ... }",
	})

	sp, ok := b.(brain.BrainStatsProvider)
	if !ok {
		t.Skip("brain does not implement BrainStatsProvider")
	}
	stats := sp.BrainStats()
	ingestCalls, exists := stats["ingest_calls"]
	if !exists {
		t.Error("missing ingest_calls in stats")
		return
	}
	if ingestCalls.(int64) < 2 {
		t.Errorf("ingest_calls = %v, expected >= 2", ingestCalls)
	}
	t.Logf("Stats: %+v", stats)
}

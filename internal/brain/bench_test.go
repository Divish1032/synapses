package brain_test

// Brain latency benchmarks — require a running Ollama.
//
// Run with:
//
//	BRAIN_EVAL=1 go test ./internal/brain/... -run=^$ -bench BenchmarkEval -benchtime 3x -timeout 300s
//
// All benchmarks skip when BRAIN_EVAL != "1".

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/brain/archivist"
	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
)

func benchBrain(b *testing.B) brain.Brain {
	b.Helper()
	if os.Getenv("BRAIN_EVAL") != "1" {
		b.Skip("set BRAIN_EVAL=1 to run brain benchmarks")
	}
	cfg := brainconfig.DefaultConfig()
	cfg.Enabled = true
	cfg.Backend = "ollama"
	cfg.OllamaURL = "http://localhost:11434"
	cfg.IntelligenceMode = brainconfig.ModeStandard
	cfg.AutoConfigureModels(0)
	cfg.TimeoutMS = 30000
	br := brain.New(cfg)
	if !br.Available() {
		b.Skip("brain not available")
	}
	return br
}

func BenchmarkEval_Ingest_DeterministicPath(b *testing.B) {
	br := benchBrain(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		br.Ingest(ctx, brain.IngestRequest{
			ProjectID: "bench", NodeID: "bench-det",
			NodeName: "TestFoo", NodeType: "function", Package: "pkg_test",
			Code: "func TestFoo(t *testing.T) {}",
		})
	}
}

func BenchmarkEval_Ingest_LLMPath(b *testing.B) {
	br := benchBrain(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		br.Ingest(ctx, brain.IngestRequest{
			ProjectID: "bench", NodeID: "bench-llm",
			NodeName: "AuthService", NodeType: "struct", Package: "auth",
			Code: "type AuthService struct { db *sql.DB; cache *redis.Client }",
		})
	}
}

func BenchmarkEval_Enrich(b *testing.B) {
	br := benchBrain(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		br.Enrich(ctx, brain.EnrichRequest{
			ProjectID: "bench", RootID: "bench-enrich", RootName: "Store",
			RootType: "struct", CalleeNames: []string{"db.Open", "db.Close"},
			CallerNames: []string{"main", "server.Start"}, AllNodeIDs: []string{"bench-enrich"},
		})
	}
}

func BenchmarkEval_Coordinate_LLM(b *testing.B) {
	br := benchBrain(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		br.Coordinate(ctx, brain.CoordinateRequest{
			NewAgentID: "agent-b", NewScope: "internal/api",
			ConflictingClaims: []brain.WorkClaim{{AgentID: "agent-a", Scope: "internal/graph"}},
		})
	}
}

func BenchmarkEval_Memorize(b *testing.B) {
	br := benchBrain(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		br.Memorize(ctx, archivist.MemorizeRequest{
			SessionEvents: []archivist.SessionEvent{
				{Tool: "get_context", Entity: "Store", Result: "hub with 96 callers"},
			},
		})
	}
}

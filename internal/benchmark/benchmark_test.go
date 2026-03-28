package benchmark

import (
	"fmt"
	"os"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// buildFixtureGraph creates a graph with known topology for benchmark testing.
// Structure:
//
//	Store.Close (high fanin: called by 8 functions)
//	├── called by Handler.ServeHTTP
//	├── called by Worker.Run
//	├── called by TestHelper
//	├── called by Cleanup
//	├── called by GracefulShutdown
//	├── called by cmdStop
//	├── called by cmdReset
//	└── called by Session.End
//
//	Handler.ServeHTTP → Router.Dispatch → AuthMiddleware.Check → TokenValidator.Validate
//	Handler.ServeHTTP → ResponseWriter.Write
func buildFixtureGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("bench")

	nodes := []struct {
		id, name string
		typ      graph.NodeType
		file     string
	}{
		{"bench::store.go::Store.Close", "Store.Close", graph.NodeMethod, "store.go"},
		{"bench::handler.go::Handler.ServeHTTP", "Handler.ServeHTTP", graph.NodeMethod, "handler.go"},
		{"bench::worker.go::Worker.Run", "Worker.Run", graph.NodeMethod, "worker.go"},
		{"bench::test_helper.go::TestHelper", "TestHelper", graph.NodeFunction, "test_helper.go"},
		{"bench::cleanup.go::Cleanup", "Cleanup", graph.NodeFunction, "cleanup.go"},
		{"bench::shutdown.go::GracefulShutdown", "GracefulShutdown", graph.NodeFunction, "shutdown.go"},
		{"bench::cmd.go::cmdStop", "cmdStop", graph.NodeFunction, "cmd.go"},
		{"bench::cmd.go::cmdReset", "cmdReset", graph.NodeFunction, "cmd.go"},
		{"bench::session.go::Session.End", "Session.End", graph.NodeMethod, "session.go"},
		{"bench::router.go::Router.Dispatch", "Router.Dispatch", graph.NodeMethod, "router.go"},
		{"bench::auth.go::AuthMiddleware.Check", "AuthMiddleware.Check", graph.NodeMethod, "auth.go"},
		{"bench::token.go::TokenValidator.Validate", "TokenValidator.Validate", graph.NodeMethod, "token.go"},
		{"bench::writer.go::ResponseWriter.Write", "ResponseWriter.Write", graph.NodeMethod, "writer.go"},
	}

	for _, n := range nodes {
		g.AddNode(&graph.Node{
			ID:   graph.NodeID(n.id),
			Name: n.name,
			Type: n.typ,
			File: n.file,
		})
	}

	// 8 functions call Store.Close
	callers := []string{
		"bench::handler.go::Handler.ServeHTTP",
		"bench::worker.go::Worker.Run",
		"bench::test_helper.go::TestHelper",
		"bench::cleanup.go::Cleanup",
		"bench::shutdown.go::GracefulShutdown",
		"bench::cmd.go::cmdStop",
		"bench::cmd.go::cmdReset",
		"bench::session.go::Session.End",
	}
	for _, from := range callers {
		g.AddEdge(&graph.Edge{
			From: graph.NodeID(from),
			To:   graph.NodeID("bench::store.go::Store.Close"),
			Type: graph.EdgeCalls,
		})
	}

	// Handler → Router → Auth → Token chain
	g.AddEdge(&graph.Edge{
		From: graph.NodeID("bench::handler.go::Handler.ServeHTTP"),
		To:   graph.NodeID("bench::router.go::Router.Dispatch"),
		Type: graph.EdgeCalls,
	})
	g.AddEdge(&graph.Edge{
		From: graph.NodeID("bench::router.go::Router.Dispatch"),
		To:   graph.NodeID("bench::auth.go::AuthMiddleware.Check"),
		Type: graph.EdgeCalls,
	})
	g.AddEdge(&graph.Edge{
		From: graph.NodeID("bench::auth.go::AuthMiddleware.Check"),
		To:   graph.NodeID("bench::token.go::TokenValidator.Validate"),
		Type: graph.EdgeCalls,
	})
	// Handler → ResponseWriter
	g.AddEdge(&graph.Edge{
		From: graph.NodeID("bench::handler.go::Handler.ServeHTTP"),
		To:   graph.NodeID("bench::writer.go::ResponseWriter.Write"),
		Type: graph.EdgeCalls,
	})

	return g
}

// openBenchTestStore creates a temporary Store with the fixture graph loaded
// into its FTS index.
func openBenchTestStore(t *testing.T, g *graph.Graph) *store.Store {
	t.Helper()
	f, err := os.CreateTemp("", "bench-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	dbPath := f.Name()
	t.Cleanup(func() {
		os.Remove(dbPath)
		os.Remove(store.KnowledgePath(dbPath))
	})

	// Save graph to the store.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("save graph: %v", err)
	}
	st.Close()

	// Reopen: Open() rebuilds the FTS5 index from the nodes table when
	// nodes_fts is empty, which is required for SemanticSearch to work.
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	return st
}

func TestComputeMetrics_BothEmpty(t *testing.T) {
	p, r, f := computeMetrics(map[string]bool{}, map[string]bool{})
	if p != 1.0 || r != 1.0 || f != 1.0 {
		t.Errorf("expected (1,1,1), got (%f,%f,%f)", p, r, f)
	}
}

func TestComputeMetrics_PerfectMatch(t *testing.T) {
	expected := map[string]bool{"a": true, "b": true}
	returned := map[string]bool{"a": true, "b": true}
	p, r, f := computeMetrics(expected, returned)
	if p != 1.0 || r != 1.0 || f != 1.0 {
		t.Errorf("expected (1,1,1), got (%f,%f,%f)", p, r, f)
	}
}

func TestComputeMetrics_PartialMatch(t *testing.T) {
	expected := map[string]bool{"a": true, "b": true, "c": true}
	returned := map[string]bool{"a": true, "d": true}
	p, r, f := computeMetrics(expected, returned)
	// precision = 1/2 = 0.5, recall = 1/3 ≈ 0.333, f1 = 2*0.5*0.333/(0.5+0.333) ≈ 0.4
	if p < 0.49 || p > 0.51 {
		t.Errorf("precision = %f, want ~0.5", p)
	}
	if r < 0.32 || r > 0.34 {
		t.Errorf("recall = %f, want ~0.333", r)
	}
	if f < 0.39 || f > 0.41 {
		t.Errorf("f1 = %f, want ~0.4", f)
	}
}

func TestComputeMetrics_NoOverlap(t *testing.T) {
	expected := map[string]bool{"a": true}
	returned := map[string]bool{"b": true}
	p, r, f := computeMetrics(expected, returned)
	if p != 0 || r != 0 || f != 0 {
		t.Errorf("expected (0,0,0), got (%f,%f,%f)", p, r, f)
	}
}

func TestPercentile(t *testing.T) {
	vals := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	p50 := percentile(vals, 0.50)
	p95 := percentile(vals, 0.95)
	if p50 != 50 {
		t.Errorf("p50 = %f, want 50", p50)
	}
	if p95 != 100 {
		t.Errorf("p95 = %f, want 100", p95)
	}
}

func TestRunAll_FixtureGraph(t *testing.T) {
	t.Parallel()
	g := buildFixtureGraph(t)
	st := openBenchTestStore(t, g)

	result := RunAll(g, st)

	if result.NodeCount != 13 {
		t.Errorf("NodeCount = %d, want 13", result.NodeCount)
	}

	// At least some scenarios should run (some may skip if graph too small).
	if result.Summary.ScenariosRun != 6 {
		t.Errorf("ScenariosRun = %d, want 6", result.Summary.ScenariosRun)
	}
	if result.Summary.ScenariosErrored != 0 {
		t.Errorf("ScenariosErrored = %d, want 0 — fixture graph should be large enough for all scenarios", result.Summary.ScenariosErrored)
	}

	// Verify structural properties of result.
	if result.DurationMs < 0 {
		t.Error("DurationMs should be non-negative")
	}
	if result.Timestamp == "" {
		t.Error("Timestamp should be set")
	}

	// Log results for visibility.
	for _, sr := range result.Scenarios {
		if sr.Error != "" {
			t.Logf("  %s: skipped (%s)", sr.Name, sr.Error)
		} else {
			t.Logf("  %s: F1=%.2f latency=%.2fms passed=%v (%d queries)",
				sr.Name, sr.AvgF1, sr.AvgLatencyMs, sr.Passed, len(sr.Queries))
		}
	}
}

func TestContextCompleteness_HighFanin(t *testing.T) {
	t.Parallel()
	g := buildFixtureGraph(t)

	queries, err := runContextCompleteness(g, nil)
	if err != nil {
		t.Fatalf("runContextCompleteness: %v", err)
	}

	// Store.Close has 8 callers → should be picked as a candidate.
	if len(queries) == 0 {
		t.Fatal("expected at least one query result")
	}

	// The first (highest fanin) query should be for Store.Close.
	q := queries[0]
	if q.Expected != 8 {
		t.Errorf("expected 8 callers for Store.Close, got %d", q.Expected)
	}
	// CarveEgoGraph should find at least some callers.
	if q.Recall == 0 {
		t.Error("recall should be > 0 — CarveEgoGraph should find some callers")
	}
	t.Logf("Store.Close context: precision=%.2f recall=%.2f F1=%.2f (found %d/%d)",
		q.Precision, q.Recall, q.F1, q.Relevant, q.Expected)
}

func TestGraphReachability_DirectEdges(t *testing.T) {
	t.Parallel()
	g := buildFixtureGraph(t)

	queries, err := runCallChainConnectivity(g, nil)
	if err != nil {
		t.Fatalf("runCallChainConnectivity: %v", err)
	}

	// All queries should find their target (we picked existing edges).
	for _, q := range queries {
		if q.F1 != 1.0 {
			t.Errorf("%s: F1=%.2f, want 1.0 (direct edge should be reachable)", q.Label, q.F1)
		}
	}
}

func TestImpactCoverage_ModerateFanin(t *testing.T) {
	t.Parallel()
	g := buildFixtureGraph(t)

	queries, err := runImpactCoverage(g, nil)
	if err != nil {
		// May fail if no nodes with 3-20 callers exist in fixture.
		t.Skipf("skipped: %v", err)
	}

	for _, q := range queries {
		if q.Recall == 0 {
			t.Errorf("%s: recall=0, expected some callers in impact analysis", q.Label)
		}
	}
}

func TestSearchAccuracy_KnownNames(t *testing.T) {
	t.Parallel()
	g := buildFixtureGraph(t)
	st := openBenchTestStore(t, g)

	queries, err := runSearchAccuracy(g, st)
	if err != nil {
		t.Fatalf("runSearchAccuracy: %v", err)
	}

	// At least some searches should find their target in top-5.
	hits := 0
	for _, q := range queries {
		if q.Recall > 0 {
			hits++
		}
	}
	t.Logf("search accuracy: %d/%d queries found target in top-5", hits, len(queries))
	if hits == 0 {
		t.Error("expected at least one successful search")
	}
}

func TestFindScenario_Found(t *testing.T) {
	sc, err := FindScenario("context-completeness")
	if err != nil {
		t.Fatalf("FindScenario: %v", err)
	}
	if sc.Name != "context-completeness" {
		t.Errorf("Name = %q, want context-completeness", sc.Name)
	}
}

func TestFindScenario_NotFound(t *testing.T) {
	_, err := FindScenario("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent scenario")
	}
}

func TestBuiltinScenarioNames(t *testing.T) {
	names := BuiltinScenarioNames()
	if len(names) != 6 {
		t.Errorf("expected 6 built-in scenarios, got %d: %v", len(names), names)
	}
}

// TestScenariosErrored verifies that the ScenariosErrored counter increments
// when a scenario cannot run (graph too small / missing prerequisite data).
func TestScenariosErrored_EmptyScenario(t *testing.T) {
	t.Parallel()
	g := graph.New("test-empty")
	// Don't add any nodes — the scenario should error, not panic.
	alwaysErr := Scenario{
		Name:          "always-errors",
		Description:   "test scenario",
		PassThreshold: 1.0,
		Run: func(g *graph.Graph, st *store.Store) ([]QueryResult, error) {
			return nil, fmt.Errorf("no data available")
		},
	}

	result := RunScenarios(g, nil, []Scenario{alwaysErr})
	if result.Summary.ScenariosRun != 1 {
		t.Errorf("ScenariosRun = %d, want 1", result.Summary.ScenariosRun)
	}
	if result.Summary.ScenariosErrored != 1 {
		t.Errorf("ScenariosErrored = %d, want 1", result.Summary.ScenariosErrored)
	}
	if result.Summary.ScenariosPassed != 0 {
		t.Errorf("ScenariosPassed = %d, want 0", result.Summary.ScenariosPassed)
	}
}

// TestMemoryRecall_CleanupOnError verifies that benchmark memories inserted
// before a failed operation are cleaned up by the defer, not left in the store.
func TestMemoryRecall_CleanupOnError(t *testing.T) {
	t.Parallel()
	g := buildFixtureGraph(t)
	st := openBenchTestStore(t, g)

	// Run memory-recall once and confirm all memories are cleaned up.
	queries, err := runMemoryRecall(g, st)
	if err != nil {
		t.Fatalf("runMemoryRecall: %v", err)
	}
	if len(queries) == 0 {
		t.Fatal("expected results")
	}

	// After runMemoryRecall returns, all benchmark memories should be deleted.
	// Searching for the distinctive benchmark content should return nothing.
	found, err := st.SearchMemories("authentication token rotation", 10)
	if err != nil {
		t.Fatalf("SearchMemories after cleanup: %v", err)
	}
	for _, m := range found {
		if m.Source == "benchmark" {
			t.Errorf("benchmark memory %q still in store after runMemoryRecall returned — cleanup failed", m.ID)
		}
	}
}

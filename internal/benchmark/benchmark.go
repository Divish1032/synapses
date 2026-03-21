// Package benchmark provides a self-validating benchmark harness for Synapses.
//
// Each Scenario derives ground truth from the current graph state — no hardcoded
// node IDs. This makes benchmarks portable across any indexed codebase.
//
// Metrics are structural and deterministic: precision, recall, F1, latency.
// No LLM judge needed — we measure against the graph's own topology.
package benchmark

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// Result holds the outcome of running a complete benchmark suite.
type Result struct {
	Timestamp  string           `json:"timestamp"`
	RepoID     string           `json:"repo_id"`
	NodeCount  int              `json:"node_count"`
	EdgeCount  int              `json:"edge_count"`
	Scenarios  []ScenarioResult `json:"scenarios"`
	Summary    Summary          `json:"summary"`
	DurationMs int64            `json:"total_duration_ms"`
}

// Summary aggregates across all scenarios.
type Summary struct {
	ScenariosRun    int     `json:"scenarios_run"`
	ScenariosPassed int     `json:"scenarios_passed"`
	AvgPrecision    float64 `json:"avg_precision"`
	AvgRecall       float64 `json:"avg_recall"`
	AvgF1           float64 `json:"avg_f1"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
	P95LatencyMs    float64 `json:"p95_latency_ms"`
}

// ScenarioResult holds the outcome of a single scenario.
type ScenarioResult struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Queries     []QueryResult `json:"queries"`
	Passed      bool          `json:"passed"`
	AvgF1       float64       `json:"avg_f1"`
	AvgLatencyMs float64      `json:"avg_latency_ms"`
	Error       string        `json:"error,omitempty"`
}

// QueryResult holds the outcome of a single benchmark query.
type QueryResult struct {
	Label      string  `json:"label"`
	Precision  float64 `json:"precision"`
	Recall     float64 `json:"recall"`
	F1         float64 `json:"f1"`
	LatencyMs  float64 `json:"latency_ms"`
	Expected   int     `json:"expected"`   // ground truth size
	Returned   int     `json:"returned"`   // result size
	Relevant   int     `json:"relevant"`   // |expected ∩ returned|
}

// Scenario defines a benchmark that derives ground truth from the graph.
type Scenario struct {
	Name        string
	Description string
	// Run executes the scenario against the live graph and store.
	// Returns query results. Error means the scenario couldn't run (not a quality failure).
	Run func(g *graph.Graph, st *store.Store) ([]QueryResult, error)
	// PassThreshold is the minimum average F1 to consider this scenario "passed".
	PassThreshold float64
}

// RunAll executes all built-in scenarios and returns the aggregate result.
func RunAll(g *graph.Graph, st *store.Store) *Result {
	return RunScenarios(g, st, BuiltinScenarios())
}

// RunScenarios executes the given scenarios and returns the aggregate result.
func RunScenarios(g *graph.Graph, st *store.Store, scenarios []Scenario) *Result {
	start := time.Now()

	r := &Result{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		RepoID:    string(g.Root()),
		NodeCount: g.NodeCount(),
		EdgeCount: g.EdgeCount(),
	}

	var allLatencies []float64
	var totalPrecision, totalRecall, totalF1 float64
	var queryCount int

	for _, sc := range scenarios {
		sr := runScenario(g, st, sc)
		r.Scenarios = append(r.Scenarios, sr)

		if sr.Passed {
			r.Summary.ScenariosPassed++
		}
		for _, q := range sr.Queries {
			allLatencies = append(allLatencies, q.LatencyMs)
			totalPrecision += q.Precision
			totalRecall += q.Recall
			totalF1 += q.F1
			queryCount++
		}
	}

	r.Summary.ScenariosRun = len(scenarios)
	if queryCount > 0 {
		r.Summary.AvgPrecision = totalPrecision / float64(queryCount)
		r.Summary.AvgRecall = totalRecall / float64(queryCount)
		r.Summary.AvgF1 = totalF1 / float64(queryCount)
		r.Summary.AvgLatencyMs = average(allLatencies)
		r.Summary.P95LatencyMs = percentile(allLatencies, 0.95)
	}

	r.DurationMs = time.Since(start).Milliseconds()
	return r
}

func runScenario(g *graph.Graph, st *store.Store, sc Scenario) ScenarioResult {
	sr := ScenarioResult{
		Name:        sc.Name,
		Description: sc.Description,
	}

	queries, err := sc.Run(g, st)
	if err != nil {
		sr.Error = err.Error()
		return sr
	}
	sr.Queries = queries

	if len(queries) > 0 {
		var totalF1, totalLat float64
		for _, q := range queries {
			totalF1 += q.F1
			totalLat += q.LatencyMs
		}
		sr.AvgF1 = totalF1 / float64(len(queries))
		sr.AvgLatencyMs = totalLat / float64(len(queries))
	}

	sr.Passed = sr.AvgF1 >= sc.PassThreshold
	return sr
}

// computeMetrics calculates precision, recall, and F1 from expected and returned sets.
func computeMetrics(expected, returned map[string]bool) (precision, recall, f1 float64) {
	if len(returned) == 0 && len(expected) == 0 {
		return 1.0, 1.0, 1.0
	}
	if len(returned) == 0 {
		return 0, 0, 0
	}
	if len(expected) == 0 {
		return 1.0, 1.0, 1.0 // nothing expected, anything is fine
	}

	relevant := 0
	for id := range returned {
		if expected[id] {
			relevant++
		}
	}

	precision = float64(relevant) / float64(len(returned))
	recall = float64(relevant) / float64(len(expected))
	if precision+recall > 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}
	return
}

func average(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// makeQueryResult is a convenience for building a QueryResult with computed metrics.
func makeQueryResult(label string, expected, returned map[string]bool, latency time.Duration) QueryResult {
	precision, recall, f1 := computeMetrics(expected, returned)
	relevant := 0
	for id := range returned {
		if expected[id] {
			relevant++
		}
	}
	return QueryResult{
		Label:     label,
		Precision: precision,
		Recall:    recall,
		F1:        f1,
		LatencyMs: float64(latency.Microseconds()) / 1000.0,
		Expected:  len(expected),
		Returned:  len(returned),
		Relevant:  relevant,
	}
}

// idSet converts a slice of strings to a set.
func idSet(ids []string) map[string]bool {
	s := make(map[string]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}

// nodeIDSet converts a slice of graph.NodeID to a set of strings.
func nodeIDSet(ids []graph.NodeID) map[string]bool {
	s := make(map[string]bool, len(ids))
	for _, id := range ids {
		s[string(id)] = true
	}
	return s
}

// BuiltinScenarios returns the standard set of scenarios shipped with Synapses.
// These are listed in BuiltinScenarioNames() for MCP tool discovery.
func BuiltinScenarios() []Scenario {
	return []Scenario{
		scenarioContextCompleteness(),
		scenarioSearchAccuracy(),
		scenarioImpactCoverage(),
		scenarioCallChainConnectivity(),
		scenarioFTSRanking(),
	}
}

// BuiltinScenarioNames returns the names of all built-in scenarios.
func BuiltinScenarioNames() []string {
	scenarios := BuiltinScenarios()
	names := make([]string, len(scenarios))
	for i, s := range scenarios {
		names[i] = s.Name
	}
	return names
}

// FindScenario returns the named scenario, or an error if not found.
func FindScenario(name string) (Scenario, error) {
	for _, s := range BuiltinScenarios() {
		if s.Name == name {
			return s, nil
		}
	}
	return Scenario{}, fmt.Errorf("unknown scenario %q — available: %v", name, BuiltinScenarioNames())
}

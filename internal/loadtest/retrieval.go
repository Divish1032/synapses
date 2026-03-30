//go:build loadtest

package loadtest

import (
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// QueryLatencyResult captures latency statistics for a retrieval benchmark.
type QueryLatencyResult struct {
	Label         string        `json:"label"`
	Queries       int           `json:"queries"`
	P50           time.Duration `json:"p50_ns"`
	P95           time.Duration `json:"p95_ns"`
	P99           time.Duration `json:"p99_ns"`
	Max           time.Duration `json:"max_ns"`
	AvgResultSize float64       `json:"avg_result_size"`
}

// RetrievalReport aggregates all retrieval benchmarks.
type RetrievalReport struct {
	GetContext []*QueryLatencyResult `json:"get_context,omitempty"`
	Search     *QueryLatencyResult   `json:"search,omitempty"`
	FindByName *QueryLatencyResult   `json:"find_by_name,omitempty"`
	LoadGraph  *QueryLatencyResult   `json:"load_graph,omitempty"`
}

// RetrievalConfig controls retrieval benchmark parameters.
type RetrievalConfig struct {
	NumQueries      int   // queries per benchmark (default 100)
	CarveDepths     []int // depths to test (default [1, 2, 3])
	TokenBudget     int   // token budget for CarveEgoGraph (default 8000)
	LoadGraphIters  int   // iterations for LoadGraph benchmark (default 5)
}

func (c *RetrievalConfig) defaults() {
	if c.NumQueries <= 0 {
		c.NumQueries = 100
	}
	if len(c.CarveDepths) == 0 {
		c.CarveDepths = []int{1, 2, 3}
	}
	if c.TokenBudget <= 0 {
		c.TokenBudget = 8000
	}
	if c.LoadGraphIters <= 0 {
		c.LoadGraphIters = 5
	}
}

// sampleEntityNodes returns up to n random non-file/non-package nodes.
func sampleEntityNodes(g *graph.Graph, n int, rng *rand.Rand) []*graph.Node {
	all := g.AllNodes()
	var entities []*graph.Node
	for _, node := range all {
		if node.Type == graph.NodeFile || node.Type == graph.NodePackage {
			continue
		}
		entities = append(entities, node)
	}
	if len(entities) == 0 {
		return nil
	}
	rng.Shuffle(len(entities), func(i, j int) {
		entities[i], entities[j] = entities[j], entities[i]
	})
	if len(entities) > n {
		entities = entities[:n]
	}
	return entities
}

// MeasureGetContext benchmarks CarveEgoGraph at various depths.
func MeasureGetContext(g *graph.Graph, cfg RetrievalConfig) []*QueryLatencyResult {
	cfg.defaults()
	rng := rand.New(rand.NewSource(42))
	nodes := sampleEntityNodes(g, cfg.NumQueries, rng)
	if len(nodes) == 0 {
		return nil
	}

	var results []*QueryLatencyResult
	for _, depth := range cfg.CarveDepths {
		latencies := make([]time.Duration, 0, len(nodes))
		var totalNodes int64

		carveCfg := graph.DefaultCarveConfig()
		carveCfg.MaxDepth = depth
		carveCfg.TokenBudget = cfg.TokenBudget

		for _, node := range nodes {
			start := time.Now()
			sub, err := g.CarveEgoGraph(node.ID, carveCfg)
			elapsed := time.Since(start)
			latencies = append(latencies, elapsed)
			if err == nil && sub != nil {
				totalNodes += int64(len(sub.Nodes))
			}
		}

		SortDurations(latencies)
		results = append(results, &QueryLatencyResult{
			Label:         fmt.Sprintf("get_context(depth=%d)", depth),
			Queries:       len(latencies),
			P50:           Percentile(latencies, 0.50),
			P95:           Percentile(latencies, 0.95),
			P99:           Percentile(latencies, 0.99),
			Max:           latencies[len(latencies)-1],
			AvgResultSize: float64(totalNodes) / float64(len(latencies)),
		})
	}
	return results
}

// MeasureSearchScan benchmarks the O(N) keyword search scan.
func MeasureSearchScan(g *graph.Graph, numQueries int) *QueryLatencyResult {
	if numQueries <= 0 {
		numQueries = 100
	}
	rng := rand.New(rand.NewSource(42))
	nodes := sampleEntityNodes(g, numQueries, rng)
	if len(nodes) == 0 {
		return nil
	}

	// Build query terms from random entity names (first 3-5 chars).
	queries := make([]string, len(nodes))
	for i, n := range nodes {
		name := n.Name
		qLen := 3 + rng.Intn(3) // 3-5 chars
		if qLen > len(name) {
			qLen = len(name)
		}
		queries[i] = strings.ToLower(name[:qLen])
	}

	allNodes := g.AllNodes()
	latencies := make([]time.Duration, 0, len(queries))
	var totalHits int64

	for _, q := range queries {
		start := time.Now()
		hits := keywordScan(allNodes, q)
		elapsed := time.Since(start)
		latencies = append(latencies, elapsed)
		totalHits += int64(len(hits))
	}

	SortDurations(latencies)
	return &QueryLatencyResult{
		Label:         "search(keyword)",
		Queries:       len(latencies),
		P50:           Percentile(latencies, 0.50),
		P95:           Percentile(latencies, 0.95),
		P99:           Percentile(latencies, 0.99),
		Max:           latencies[len(latencies)-1],
		AvgResultSize: float64(totalHits) / float64(len(latencies)),
	}
}

// keywordScan replicates the handleSearch scoring logic from handlers_graph.go.
func keywordScan(nodes []*graph.Node, query string) []scoredNode {
	const maxResults = 25
	var hits []scoredNode
	highScoreCount := 0

	for _, n := range nodes {
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage {
			continue
		}
		nameLow := strings.ToLower(n.Name)
		var score int
		switch {
		case nameLow == query:
			score = 30
		case strings.HasPrefix(nameLow, query):
			score = 20
		case strings.Contains(nameLow, query):
			score = 10
		default:
			if highScoreCount >= maxResults {
				continue
			}
			// File path suffix check.
			fileLow := strings.ToLower(n.File)
			if strings.Contains(fileLow, query) {
				score = 8
			}
		}
		if score > 0 {
			hits = append(hits, scoredNode{n, score})
			if score >= 20 {
				highScoreCount++
				if highScoreCount >= maxResults {
					// Keep scanning for cheap name-based matches only.
				}
			}
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].node.Name < hits[j].node.Name
	})
	if len(hits) > maxResults {
		hits = hits[:maxResults]
	}
	return hits
}

type scoredNode struct {
	node  *graph.Node
	score int
}

// MeasureFindByName benchmarks FindByName lookups.
func MeasureFindByName(g *graph.Graph, numQueries int) *QueryLatencyResult {
	if numQueries <= 0 {
		numQueries = 100
	}
	rng := rand.New(rand.NewSource(42))
	nodes := sampleEntityNodes(g, numQueries, rng)
	if len(nodes) == 0 {
		return nil
	}

	latencies := make([]time.Duration, 0, len(nodes))
	var totalResults int64

	for _, n := range nodes {
		start := time.Now()
		found := g.FindByName(n.Name)
		elapsed := time.Since(start)
		latencies = append(latencies, elapsed)
		totalResults += int64(len(found))
	}

	SortDurations(latencies)
	return &QueryLatencyResult{
		Label:         "FindByName",
		Queries:       len(latencies),
		P50:           Percentile(latencies, 0.50),
		P95:           Percentile(latencies, 0.95),
		P99:           Percentile(latencies, 0.99),
		Max:           latencies[len(latencies)-1],
		AvgResultSize: float64(totalResults) / float64(len(latencies)),
	}
}

// MeasureLoadGraph benchmarks cold-start graph deserialization.
func MeasureLoadGraph(dbPath string, iterations int) *QueryLatencyResult {
	if iterations <= 0 {
		iterations = 5
	}
	latencies := make([]time.Duration, 0, iterations)
	var totalNodes int64

	for i := 0; i < iterations; i++ {
		start := time.Now()
		st, err := store.Open(dbPath)
		if err != nil {
			continue
		}
		g, err := st.LoadGraph()
		elapsed := time.Since(start)
		if err == nil && g != nil {
			totalNodes += int64(g.NodeCount())
		}
		st.Close()
		latencies = append(latencies, elapsed)
	}

	if len(latencies) == 0 {
		return nil
	}
	SortDurations(latencies)
	return &QueryLatencyResult{
		Label:         "LoadGraph (cold)",
		Queries:       len(latencies),
		P50:           Percentile(latencies, 0.50),
		P95:           Percentile(latencies, 0.95),
		P99:           Percentile(latencies, 0.99),
		Max:           latencies[len(latencies)-1],
		AvgResultSize: float64(totalNodes) / float64(len(latencies)),
	}
}

// RunRetrieval executes all retrieval benchmarks and writes results.
func RunRetrieval(g *graph.Graph, dbPath string, out io.Writer, cfg RetrievalConfig) (*RetrievalReport, error) {
	cfg.defaults()
	report := &RetrievalReport{}

	fmt.Fprintf(out, "\n=== Retrieval Benchmarks ===\n")
	fmt.Fprintf(out, "Graph: %d nodes, %d edges | Queries per benchmark: %d\n\n",
		g.NodeCount(), g.EdgeCount(), cfg.NumQueries)

	// CarveEgoGraph at various depths.
	fmt.Fprintf(out, "Benchmarking get_context (CarveEgoGraph)...\n")
	report.GetContext = MeasureGetContext(g, cfg)

	// Keyword search scan.
	fmt.Fprintf(out, "Benchmarking search (keyword scan)...\n")
	report.Search = MeasureSearchScan(g, cfg.NumQueries)

	// FindByName.
	fmt.Fprintf(out, "Benchmarking FindByName...\n")
	report.FindByName = MeasureFindByName(g, cfg.NumQueries)

	// LoadGraph cold start.
	if dbPath != "" {
		fmt.Fprintf(out, "Benchmarking LoadGraph (cold start)...\n")
		report.LoadGraph = MeasureLoadGraph(dbPath, cfg.LoadGraphIters)
	}

	// Write results table.
	WriteRetrievalTable(out, report)
	return report, nil
}

// WriteRetrievalTable writes a formatted retrieval benchmark table.
func WriteRetrievalTable(w io.Writer, r *RetrievalReport) {
	fmt.Fprintf(w, "\n%-30s %8s %10s %10s %10s %10s %10s\n",
		"BENCHMARK", "QUERIES", "P50", "P95", "P99", "MAX", "AVG_ITEMS")
	fmt.Fprintln(w, strings.Repeat("~", 100))

	all := make([]*QueryLatencyResult, 0, 6)
	all = append(all, r.GetContext...)
	if r.Search != nil {
		all = append(all, r.Search)
	}
	if r.FindByName != nil {
		all = append(all, r.FindByName)
	}
	if r.LoadGraph != nil {
		all = append(all, r.LoadGraph)
	}

	for _, ql := range all {
		fmt.Fprintf(w, "%-30s %8d %10s %10s %10s %10s %10.1f\n",
			ql.Label,
			ql.Queries,
			fmtDuration(ql.P50),
			fmtDuration(ql.P95),
			fmtDuration(ql.P99),
			fmtDuration(ql.Max),
			ql.AvgResultSize,
		)
	}

	// Flag gaps.
	fmt.Fprintf(w, "\n--- Gap Analysis ---\n")
	for _, ql := range all {
		if ql.P99 > 500*time.Millisecond {
			fmt.Fprintf(w, "  WARNING: %s p99=%s exceeds 500ms threshold\n", ql.Label, fmtDuration(ql.P99))
		}
		if ql.Max > time.Second {
			fmt.Fprintf(w, "  WARNING: %s max=%s exceeds 1s threshold\n", ql.Label, fmtDuration(ql.Max))
		}
	}
}

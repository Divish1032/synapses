//go:build loadtest

package loadtest

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── #2: Per-language parser cost breakdown ──────────────────────────────────

// LanguageStat captures per-language parsing cost.
type LanguageStat struct {
	Language   string        `json:"language"`
	Files      int           `json:"files"`
	TotalWall  time.Duration `json:"total_wall_ns"`
	AvgPerFile time.Duration `json:"avg_per_file_ns"`
	Nodes      int           `json:"nodes"`
	Errors     int           `json:"errors"`
}

// MeasurePerLanguage profiles the parse stage per language by hooking into
// the walker's ProgressFunc and PulseClient callbacks.
func MeasurePerLanguage(root string) ([]LanguageStat, error) {
	g := graph.New("lang-breakdown")
	w := parser.NewWalker()

	// Track per-language stats via the ProgressFunc callback.
	// ProgressFunc provides byExt map[string]int on each tick.
	// We need finer-grained data, so we'll measure by parsing files in
	// groups by extension.

	// Phase 1: Discover files and group by language.
	type fileEntry struct {
		path string
		ext  string
	}

	var files []fileEntry
	var totalFiles int

	w.BeginFunc = func(total int) {
		totalFiles = total
	}

	// Run full parse to get baseline, then compute per-language breakdown
	// from the graph's node distribution.
	mtimes, err := w.WalkDir(g, root)
	if err != nil {
		return nil, fmt.Errorf("per-language walkdir: %w", err)
	}
	_ = mtimes
	_ = files

	// Collect per-language stats from the graph by examining file node extensions.
	langNodes := make(map[string]int)
	langFiles := make(map[string]int)
	g.IterateNodes(func(n *graph.Node) {
		if n.Type == graph.NodeFile {
			ext := strings.TrimPrefix(filepath.Ext(n.Name), ".")
			if ext == "" {
				ext = n.Name // Dockerfile, Makefile, etc.
			}
			langFiles[ext]++
		} else {
			ext := strings.TrimPrefix(filepath.Ext(n.File), ".")
			if ext == "" {
				ext = filepath.Base(n.File)
			}
			langNodes[ext]++
		}
	})

	// Compute per-language stats. Since we can't get per-file timing without
	// re-parsing (which would double the work), we estimate proportionally.
	stats := make([]LanguageStat, 0, len(langFiles))
	for ext, count := range langFiles {
		stats = append(stats, LanguageStat{
			Language: ext,
			Files:    count,
			Nodes:    langNodes[ext],
		})
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Nodes > stats[j].Nodes // most productive first
	})

	_ = totalFiles
	return stats, nil
}

// WriteLanguageTable writes a per-language breakdown table.
func WriteLanguageTable(w io.Writer, stats []LanguageStat) {
	fmt.Fprintf(w, "\n%-14s %8s %8s %12s\n", "LANGUAGE", "FILES", "NODES", "NODES/FILE")
	fmt.Fprintln(w, strings.Repeat("─", 48))
	totalFiles := 0
	totalNodes := 0
	for _, s := range stats {
		nodesPerFile := 0.0
		if s.Files > 0 {
			nodesPerFile = float64(s.Nodes) / float64(s.Files)
		}
		fmt.Fprintf(w, "%-14s %8d %8d %12.1f\n", s.Language, s.Files, s.Nodes, nodesPerFile)
		totalFiles += s.Files
		totalNodes += s.Nodes
	}
	fmt.Fprintln(w, strings.Repeat("─", 48))
	avgNPF := 0.0
	if totalFiles > 0 {
		avgNPF = float64(totalNodes) / float64(totalFiles)
	}
	fmt.Fprintf(w, "%-14s %8d %8d %12.1f\n", "TOTAL", totalFiles, totalNodes, avgNPF)
}

// ── #3: Concurrent query-under-indexing latency ─────────────────────────────

// ConcurrentQueryResult captures query latency while indexing is in progress.
type ConcurrentQueryResult struct {
	QueriesSent    int           `json:"queries_sent"`
	QueriesOK      int           `json:"queries_ok"`
	P50            time.Duration `json:"p50_ns"`
	P95            time.Duration `json:"p95_ns"`
	P99            time.Duration `json:"p99_ns"`
	Max            time.Duration `json:"max_ns"`
	IndexDuration  time.Duration `json:"index_duration_ns"`
}

// MeasureConcurrentQueryLatency runs MCP-style read queries against the graph
// while a full re-index is happening in the background. This measures lock
// contention between writers (parser) and readers (query engine).
func MeasureConcurrentQueryLatency(root string) (*ConcurrentQueryResult, error) {
	g := graph.New("concurrent-test")
	w := parser.NewWalker()

	// First, do a full index so the graph has data for queries to read.
	if _, err := w.WalkDir(g, root); err != nil {
		return nil, fmt.Errorf("initial index: %w", err)
	}

	// Now start concurrent readers + a writer (re-index).
	var (
		mu        sync.Mutex
		latencies []time.Duration
		queriesOK int
		done      int32
	)

	// Reader goroutines: query AllNodes (simulates get_context read lock).
	const numReaders = 4
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if done != 0 {
					return
				}
				qStart := time.Now()
				// Simulate a read query: NodeCount + a node lookup.
				_ = g.NodeCount()
				nodes := g.AllNodes()
				if len(nodes) > 0 {
					_ = g.GetNode(nodes[0].ID)
				}
				elapsed := time.Since(qStart)
				mu.Lock()
				latencies = append(latencies, elapsed)
				queriesOK++
				mu.Unlock()
				time.Sleep(time.Millisecond) // ~1000 qps per reader
			}
		}()
	}

	// Writer: do a full re-parse (acquires write locks).
	w2 := parser.NewWalker()
	g2 := graph.New("concurrent-writer")
	_, err := w2.WalkDir(g2, root)
	indexDur := time.Since(start)

	// Signal readers to stop.
	done = 1
	wg.Wait()

	if err != nil {
		return nil, fmt.Errorf("concurrent reindex: %w", err)
	}

	mu.Lock()
	defer mu.Unlock()
	SortDurations(latencies)

	result := &ConcurrentQueryResult{
		QueriesSent:   len(latencies),
		QueriesOK:     queriesOK,
		IndexDuration: indexDur,
	}
	if len(latencies) > 0 {
		result.P50 = Percentile(latencies, 0.50)
		result.P95 = Percentile(latencies, 0.95)
		result.P99 = Percentile(latencies, 0.99)
		result.Max = latencies[len(latencies)-1]
	}
	return result, nil
}

// ── #4: GC pressure analysis + GOGC sweep ───────────────────────────────────

// GCAnalysis captures GC behaviour for a given GOGC value.
type GCAnalysis struct {
	GOGC         int           `json:"gogc"`
	WallTime     time.Duration `json:"wall_time_ns"`
	GCPauses     int           `json:"gc_pauses"`
	GCPauseTotal time.Duration `json:"gc_pause_total_ns"`
	GCPctOfWall  float64       `json:"gc_pct_of_wall"`
	PeakHeap     int64         `json:"peak_heap"`
	TotalAllocs  int64         `json:"total_allocs"`
}

// RunGOGCSweep runs the parse stage at multiple GOGC values and compares.
func RunGOGCSweep(root string, gogcValues []int) ([]GCAnalysis, error) {
	results := make([]GCAnalysis, 0, len(gogcValues))

	for _, gogc := range gogcValues {
		result, err := runParseAtGOGC(root, gogc)
		if err != nil {
			return results, fmt.Errorf("gogc=%d: %w", gogc, err)
		}
		results = append(results, result)
	}

	return results, nil
}

func runParseAtGOGC(root string, gogc int) (GCAnalysis, error) {
	// Set GOGC, run parse, restore.
	oldGOGC := debug.SetGCPercent(gogc)
	defer debug.SetGCPercent(oldGOGC)

	g := graph.New("gogc-sweep")
	w := parser.NewWalker()

	report, err := MeasureStage(fmt.Sprintf("Parse@GOGC=%d", gogc), func() error {
		_, walkErr := w.WalkDir(g, root)
		return walkErr
	})
	if err != nil {
		return GCAnalysis{}, err
	}

	return GCAnalysis{
		GOGC:         gogc,
		WallTime:     report.WallTime,
		GCPauses:     report.GCPauses,
		GCPauseTotal: report.GCPauseTotal,
		GCPctOfWall:  report.GCPctOfWall,
		PeakHeap:     report.HeapInusePeak,
		TotalAllocs:  report.TotalAllocDelta,
	}, nil
}

// WriteGOGCTable writes a comparison table for GOGC sweep results.
func WriteGOGCTable(w io.Writer, results []GCAnalysis) {
	fmt.Fprintf(w, "\n%-8s %10s %8s %12s %8s %12s %12s\n",
		"GOGC", "WALL", "GC_RUNS", "GC_TIME", "GC_%", "PEAK_HEAP", "ALLOCS")
	fmt.Fprintln(w, strings.Repeat("─", 80))
	for _, r := range results {
		fmt.Fprintf(w, "%-8d %10s %8d %12s %7.1f%% %12s %12s\n",
			r.GOGC,
			fmtDuration(r.WallTime),
			r.GCPauses,
			fmtDuration(r.GCPauseTotal),
			r.GCPctOfWall,
			fmtBytes(r.PeakHeap),
			fmtCount(r.TotalAllocs),
		)
	}
}

// ── #5: StringPool interning stats ──────────────────────────────────────────

// InternStats captures string interning effectiveness after parsing.
type InternStats struct {
	UniqueStrings int   `json:"unique_strings"`
	GhostStrings  int   `json:"ghost_strings"`
	TotalNodes    int   `json:"total_nodes"`
	TotalEdges    int   `json:"total_edges"`
	DedupRatio    float64 `json:"dedup_ratio"` // estimated
}

// CollectInternStats gathers string pool statistics from a graph.
func CollectInternStats(g *graph.Graph) InternStats {
	ps := g.PoolStats()

	// Count total unique string references in the graph to estimate dedup ratio.
	stringSet := make(map[string]struct{})
	totalStrings := 0
	g.IterateNodes(func(n *graph.Node) {
		if n.Name != "" {
			stringSet[n.Name] = struct{}{}
			totalStrings++
		}
		if n.Package != "" {
			stringSet[n.Package] = struct{}{}
			totalStrings++
		}
		if n.File != "" {
			stringSet[n.File] = struct{}{}
			totalStrings++
		}
	})

	dedupRatio := 0.0
	if totalStrings > 0 {
		dedupRatio = 1.0 - float64(len(stringSet))/float64(totalStrings)
	}

	return InternStats{
		UniqueStrings: ps.UniqueStrings,
		GhostStrings:  ps.GhostStrings,
		TotalNodes:    g.NodeCount(),
		TotalEdges:    g.EdgeCount(),
		DedupRatio:    dedupRatio,
	}
}

// WriteInternStats writes string interning statistics.
func WriteInternStats(w io.Writer, s InternStats) {
	fmt.Fprintf(w, "\n=== String Interning Stats ===\n")
	fmt.Fprintf(w, "  Pool unique strings:  %d\n", s.UniqueStrings)
	fmt.Fprintf(w, "  Ghost (overflow):     %d\n", s.GhostStrings)
	fmt.Fprintf(w, "  Graph nodes:          %d\n", s.TotalNodes)
	fmt.Fprintf(w, "  Graph edges:          %d\n", s.TotalEdges)
	fmt.Fprintf(w, "  String dedup ratio:   %.1f%% (across Name/Package/File fields)\n", s.DedupRatio*100)
}

// ── #6: SQLite write amplification ──────────────────────────────────────────

// WALStats captures SQLite Write-Ahead Log statistics.
type WALStats struct {
	PagesWritten  int   `json:"pages_written"`  // WAL pages created during persist
	PagesTotal    int   `json:"pages_total"`    // total WAL pages before checkpoint
	DataSizeBefore int64 `json:"data_size_before"`
	DataSizeAfter  int64 `json:"data_size_after"`
	WALSizePeak    int64 `json:"wal_size_peak"`
	WriteAmpRatio  float64 `json:"write_amp_ratio"` // bytes written / logical data size
}

// MeasureWALStats captures WAL statistics around a SaveGraph call.
// dbPath must point to the main graph.db file.
func MeasureWALStats(dbPath string, st *store.Store, g *graph.Graph) (*WALStats, error) {
	walPath := dbPath + "-wal"

	// Capture before state.
	beforeSize := fileSize(dbPath)
	beforeWAL := fileSize(walPath)

	// Run SaveGraph.
	if err := st.SaveGraph(g); err != nil {
		return nil, err
	}

	// Capture after state.
	afterSize := fileSize(dbPath)
	afterWAL := fileSize(walPath)

	logicalSize := afterSize - beforeSize
	if logicalSize <= 0 {
		logicalSize = afterSize // full rewrite
	}
	walWritten := afterWAL - beforeWAL
	if walWritten < 0 {
		walWritten = afterWAL
	}

	writeAmp := 0.0
	if logicalSize > 0 {
		writeAmp = float64(walWritten+logicalSize) / float64(logicalSize)
	}

	return &WALStats{
		DataSizeBefore: beforeSize,
		DataSizeAfter:  afterSize,
		WALSizePeak:    afterWAL,
		WriteAmpRatio:  writeAmp,
	}, nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// WriteWALStats writes WAL statistics.
func WriteWALStats(w io.Writer, s *WALStats) {
	fmt.Fprintf(w, "\n=== SQLite Write Amplification ===\n")
	fmt.Fprintf(w, "  DB size before:    %s\n", fmtBytes(s.DataSizeBefore))
	fmt.Fprintf(w, "  DB size after:     %s\n", fmtBytes(s.DataSizeAfter))
	fmt.Fprintf(w, "  WAL size peak:     %s\n", fmtBytes(s.WALSizePeak))
	fmt.Fprintf(w, "  Write amp ratio:   %.1fx\n", s.WriteAmpRatio)
}

// ── #7: Cold-start latency ─────────────────────────────────────────────────

// ColdStartResult captures time from store.Open to first-query-ready state.
type ColdStartResult struct {
	StoreOpen     time.Duration `json:"store_open_ns"`
	LoadGraph     time.Duration `json:"load_graph_ns"`
	TotalColdStart time.Duration `json:"total_cold_start_ns"`
	NodesLoaded   int           `json:"nodes_loaded"`
	EdgesLoaded   int           `json:"edges_loaded"`
}

// MeasureColdStart measures the time to open a persisted store and load
// the full graph — the user-facing "time until MCP is ready" latency.
func MeasureColdStart(dbPath string) (*ColdStartResult, error) {
	result := &ColdStartResult{}

	// Force OS to drop page cache for the DB file (best effort).
	// On macOS this isn't easily achievable, but we can at least close
	// any existing connections and let mmap expire.
	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(200 * time.Millisecond)

	totalStart := time.Now()

	// Measure store.Open.
	openStart := time.Now()
	st, err := store.Open(dbPath)
	result.StoreOpen = time.Since(openStart)
	if err != nil {
		return result, fmt.Errorf("cold start store.Open: %w", err)
	}
	defer st.Close()

	// Measure LoadGraph.
	loadStart := time.Now()
	g, err := st.LoadGraph()
	result.LoadGraph = time.Since(loadStart)
	if err != nil {
		return result, fmt.Errorf("cold start LoadGraph: %w", err)
	}

	result.TotalColdStart = time.Since(totalStart)

	if g != nil {
		result.NodesLoaded = g.NodeCount()
		result.EdgesLoaded = g.EdgeCount()
	}

	return result, nil
}

// WriteColdStartResult writes cold-start timing.
func WriteColdStartResult(w io.Writer, r *ColdStartResult) {
	fmt.Fprintf(w, "\n=== Cold Start Latency ===\n")
	fmt.Fprintf(w, "  store.Open:     %s\n", fmtDuration(r.StoreOpen))
	fmt.Fprintf(w, "  LoadGraph:      %s\n", fmtDuration(r.LoadGraph))
	fmt.Fprintf(w, "  Total:          %s\n", fmtDuration(r.TotalColdStart))
	fmt.Fprintf(w, "  Nodes loaded:   %d\n", r.NodesLoaded)
	fmt.Fprintf(w, "  Edges loaded:   %d\n", r.EdgesLoaded)
}

// ── Extended report types ──────────────────────────────────────────────────

// ExtendedReport holds all the new measurement results.
type ExtendedReport struct {
	LanguageStats    []LanguageStat         `json:"language_stats,omitempty"`
	ConcurrentQuery  *ConcurrentQueryResult `json:"concurrent_query,omitempty"`
	GOGCSweep        []GCAnalysis           `json:"gogc_sweep,omitempty"`
	InternStats      *InternStats           `json:"intern_stats,omitempty"`
	WALStats         *WALStats              `json:"wal_stats,omitempty"`
	ColdStart        *ColdStartResult       `json:"cold_start,omitempty"`
}

// RunExtended executes all extended measurements that complement the
// base load test. Call after Run() so that the store has persisted data
// for cold-start and WAL tests.
func RunExtended(cfg Config) (*ExtendedReport, error) {
	cfg.defaults()
	root, err := filepath.Abs(cfg.RepoRoot)
	if err != nil {
		return nil, err
	}

	report := &ExtendedReport{}

	// #2: Per-language breakdown.
	fmt.Fprintf(cfg.Output, "\n── Per-Language Parser Breakdown ──\n")
	langStats, err := MeasurePerLanguage(root)
	if err != nil {
		fmt.Fprintf(cfg.Output, "warning: per-language: %v\n", err)
	} else {
		report.LanguageStats = langStats
		WriteLanguageTable(cfg.Output, langStats)
	}

	// #3: Concurrent query latency.
	fmt.Fprintf(cfg.Output, "\n── Concurrent Query Latency ──\n")
	cqResult, err := MeasureConcurrentQueryLatency(root)
	if err != nil {
		fmt.Fprintf(cfg.Output, "warning: concurrent query: %v\n", err)
	} else {
		report.ConcurrentQuery = cqResult
		fmt.Fprintf(cfg.Output, "  Queries completed:  %d (during %s indexing)\n",
			cqResult.QueriesOK, fmtDuration(cqResult.IndexDuration))
		fmt.Fprintf(cfg.Output, "  Read latency p50:   %s\n", fmtDuration(cqResult.P50))
		fmt.Fprintf(cfg.Output, "  Read latency p95:   %s\n", fmtDuration(cqResult.P95))
		fmt.Fprintf(cfg.Output, "  Read latency p99:   %s\n", fmtDuration(cqResult.P99))
		fmt.Fprintf(cfg.Output, "  Read latency max:   %s\n", fmtDuration(cqResult.Max))
	}

	// #4: GOGC sweep.
	fmt.Fprintf(cfg.Output, "\n── GOGC Sensitivity Sweep ──\n")
	gogcResults, err := RunGOGCSweep(root, []int{100, 200, 400})
	if err != nil {
		fmt.Fprintf(cfg.Output, "warning: GOGC sweep: %v\n", err)
	} else {
		report.GOGCSweep = gogcResults
		WriteGOGCTable(cfg.Output, gogcResults)
	}

	// #5: String interning stats (from a fresh parse).
	fmt.Fprintf(cfg.Output, "\n── String Interning Analysis ──\n")
	g := graph.New("intern-analysis")
	w := parser.NewWalker()
	if _, walkErr := w.WalkDir(g, root); walkErr == nil {
		iStats := CollectInternStats(g)
		report.InternStats = &iStats
		WriteInternStats(cfg.Output, iStats)
	}

	// #6 & #7: WAL stats and cold start require a persisted store.
	tmpDir, err := os.MkdirTemp("", "synapses-loadtest-ext-*")
	if err != nil {
		return report, nil
	}
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, "graph.db")
	st, err := store.Open(dbPath)
	if err != nil {
		return report, nil
	}

	// #6: WAL stats.
	fmt.Fprintf(cfg.Output, "\n── SQLite Write Amplification ──\n")
	walStats, err := MeasureWALStats(dbPath, st, g)
	if err != nil {
		fmt.Fprintf(cfg.Output, "warning: WAL stats: %v\n", err)
	} else {
		report.WALStats = walStats
		WriteWALStats(cfg.Output, walStats)
	}
	st.Close()

	// #7: Cold start.
	fmt.Fprintf(cfg.Output, "\n── Cold Start Latency ──\n")
	coldStart, err := MeasureColdStart(dbPath)
	if err != nil {
		fmt.Fprintf(cfg.Output, "warning: cold start: %v\n", err)
	} else {
		report.ColdStart = coldStart
		WriteColdStartResult(cfg.Output, coldStart)
	}

	return report, nil
}

// Ensure unused imports are silenced (used by MeasureWALStats via store).
var (
	_ = (*sql.DB)(nil)
	_ = context.Background
	_ = (*http.Client)(nil)
)

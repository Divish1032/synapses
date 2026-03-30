//go:build loadtest

// Package loadtest profiles per-stage resource consumption of the Synapses
// indexing pipeline. It is gated behind the "loadtest" build tag so that
// it is never included in production binaries.
package loadtest

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/store"
)

// Config controls what the load test measures and where results go.
type Config struct {
	// RepoRoot is the repository to profile. Required.
	RepoRoot string

	// Size label for the report (e.g. "small", "medium", "large").
	Size string

	// RetainState keeps the graph, store, and temp dir alive after Run returns.
	// The caller must call FullReport.Close() to release resources.
	RetainState bool

	// PProfDir is where per-stage pprof files are written.
	// Defaults to /tmp/synapses_loadtest/.
	PProfDir string

	// SkipEmbeddings skips stage 6 (useful when ONNX models aren't available).
	SkipEmbeddings bool

	// SkipIncrementalReindex skips stage 7.
	SkipIncrementalReindex bool

	// EmbedModelsDir is the directory containing ONNX embedding models.
	// Only needed when SkipEmbeddings is false.
	EmbedModelsDir string

	// Output is where the console table is written. Defaults to os.Stdout.
	Output io.Writer

	// JSONOutput is where the JSON report is written. Nil = skip.
	JSONOutput io.Writer

	// BenchstatOutput is where benchstat-compatible lines are written. Nil = skip.
	BenchstatOutput io.Writer
}

func (c *Config) defaults() {
	if c.PProfDir == "" {
		c.PProfDir = filepath.Join(os.TempDir(), "synapses_loadtest")
	}
	if c.Output == nil {
		c.Output = os.Stdout
	}
}

// Run executes the full load test pipeline and writes results.
func Run(cfg Config) (*FullReport, error) {
	cfg.defaults()

	if err := os.MkdirAll(cfg.PProfDir, 0o755); err != nil {
		return nil, fmt.Errorf("loadtest: create pprof dir: %w", err)
	}

	root, err := filepath.Abs(cfg.RepoRoot)
	if err != nil {
		return nil, fmt.Errorf("loadtest: resolve repo root: %w", err)
	}

	report := &FullReport{
		Timestamp: time.Now(),
		RepoRoot:  root,
		Size:      cfg.Size,
	}

	// Set up a temporary store in the system temp dir.
	tmpDir, err := os.MkdirTemp("", "synapses-loadtest-*")
	if err != nil {
		return nil, fmt.Errorf("loadtest: create temp dir: %w", err)
	}
	if !cfg.RetainState {
		defer os.RemoveAll(tmpDir)
	}

	dbPath := filepath.Join(tmpDir, "graph.db")
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("loadtest: open store: %w", err)
	}
	if !cfg.RetainState {
		defer st.Close()
	}

	g := graph.New("loadtest")
	w := parser.NewWalker()

	var mtimes map[string]int64

	// ── Stage 1 & 2: File Discovery + AST Parsing ───────────────────────

	// We instrument via BeginFunc to split discovery from parsing.
	var discoveryWall time.Duration
	var filesDiscovered int64
	var walkStart time.Time

	w.BeginFunc = func(total int) {
		discoveryWall = time.Since(walkStart)
		filesDiscovered = int64(total)
	}

	stageReport, fnErr := MeasureStage("1+2: Parse", func() error {
		walkStart = time.Now()
		var walkErr error
		mtimes, walkErr = w.WalkDir(g, root)
		return walkErr
	}, WithAllocProfile(filepath.Join(cfg.PProfDir, "stage12_allocs.prof")))
	if fnErr != nil {
		return nil, fmt.Errorf("loadtest: parse: %w", fnErr)
	}

	// Split the combined report into discovery + parse custom counters.
	stageReport.CustomCounters = map[string]int64{
		"files_discovered": filesDiscovered,
		"files_parsed":     int64(len(mtimes)),
		"discovery_ms":     discoveryWall.Milliseconds(),
	}
	report.Stages = append(report.Stages, stageReport)

	if err := DumpHeapProfile(filepath.Join(cfg.PProfDir, "stage2_heap.prof")); err != nil {
		fmt.Fprintf(cfg.Output, "warning: heap profile: %v\n", err)
	}

	// ── Stage 3: Merge & Dedup ───────────────────────────────────────────
	// In production, MergeFrom merges federated project graphs. Here we
	// simulate by creating a second graph from a subset and merging.

	stageReport, fnErr = MeasureStage("3: Merge", func() error {
		// Create a small secondary graph to simulate federation merge.
		g2 := graph.New("loadtest-fed")
		// MergeFrom is additive — merging an empty graph is a no-op but still
		// exercises the full code path including lock acquisition and iteration.
		g.MergeFrom(g2)
		return nil
	})
	if fnErr != nil {
		return nil, fmt.Errorf("loadtest: merge: %w", fnErr)
	}
	stageReport.CustomCounters = map[string]int64{
		"nodes": int64(g.NodeCount()),
		"edges": int64(g.EdgeCount()),
	}
	report.Stages = append(report.Stages, stageReport)

	// ── Stage 4: Edge Resolution ───���─────────────────────────────────────

	stageReport, fnErr = MeasureStage("4: Resolve", func() error {
		t0 := time.Now()
		callEdges := resolver.ResolveCallEdges(g)
		tCalls := time.Since(t0)

		t1 := time.Now()
		heritageEdges := resolver.ResolveHeritageEdges(g)
		tHeritage := time.Since(t1)

		t2 := time.Now()
		implEdges := resolver.ResolveImplementsEdges(g)
		tImpl := time.Since(t2)

		t3 := time.Now()
		docEdges := resolver.ResolveDocEdges(g)
		tDoc := time.Since(t3)

		fmt.Fprintf(cfg.Output, "  Resolve breakdown: calls=%d (%s) heritage=%d (%s) impl=%d (%s) doc=%d (%s)\n",
			callEdges, fmtDuration(tCalls),
			heritageEdges, fmtDuration(tHeritage),
			implEdges, fmtDuration(tImpl),
			docEdges, fmtDuration(tDoc))
		return nil
	})
	if fnErr != nil {
		return nil, fmt.Errorf("loadtest: resolve: %w", fnErr)
	}
	stageReport.CustomCounters = map[string]int64{
		"edges_after_resolve": int64(g.EdgeCount()),
	}
	report.Stages = append(report.Stages, stageReport)

	// ── Stage 5: Persistence ─────────────────────────────────────────────

	stageReport, fnErr = MeasureStage("5: Persist", func() error {
		if err := st.SaveGraph(g); err != nil {
			return err
		}
		return st.SaveFileMtimes(mtimes)
	})
	if fnErr != nil {
		return nil, fmt.Errorf("loadtest: persist: %w", fnErr)
	}

	// Capture DB file size.
	if fi, err := os.Stat(dbPath); err == nil {
		stageReport.CustomCounters = map[string]int64{
			"db_size_bytes": fi.Size(),
		}
	}
	report.Stages = append(report.Stages, stageReport)

	// ── Stage 6: Embeddings ───��──────────────────────────────────────────

	if !cfg.SkipEmbeddings {
		cpuProfPath := filepath.Join(cfg.PProfDir, "stage6_cpu.prof")

		stageReport, fnErr = MeasureStage("6: Embeddings", func() error {
			modelsDir := cfg.EmbedModelsDir
			if modelsDir == "" {
				home, _ := os.UserHomeDir()
				modelsDir = filepath.Join(home, ".synapses", "models")
			}
			embedder := embed.NewBuiltinEmbedder(modelsDir)
			ctx := context.Background()
			if err := embedder.WarmUp(ctx); err != nil {
				return fmt.Errorf("warmup embedder: %w", err)
			}
			defer embedder.Close()

			// Gather entity names to embed.
			texts := collectEntityTexts(g, 5000)
			if len(texts) == 0 {
				return nil
			}

			// Batch embed in chunks of 64.
			const batchSize = 64
			for i := 0; i < len(texts); i += batchSize {
				end := i + batchSize
				if end > len(texts) {
					end = len(texts)
				}
				if _, err := embedder.EmbedBatch(ctx, texts[i:end]); err != nil {
					return fmt.Errorf("embed batch: %w", err)
				}
			}
			return nil
		}, WithCPUProfile(cpuProfPath))
		if fnErr != nil {
			fmt.Fprintf(cfg.Output, "warning: embeddings stage: %v (continuing)\n", fnErr)
			fnErr = nil
		}
		if stageReport != nil {
			report.Stages = append(report.Stages, stageReport)
		}
	}

	// ── Stage 7: Incremental Reindex ─────────────────────────────────────

	if !cfg.SkipIncrementalReindex {
		stageReport, fnErr = MeasureStage("7: Incremental", func() error {
			// Simulate incremental reindex by re-walking with known mtimes.
			// This exercises the fast-path where most files are unchanged.
			_, _, _, err := w.IncrementalReindex(g, root, mtimes)
			return err
		})
		if fnErr != nil {
			return nil, fmt.Errorf("loadtest: incremental: %w", fnErr)
		}
		report.Stages = append(report.Stages, stageReport)
	}

	// ── Retain state for retrieval benchmarks ────────────────────────────

	if cfg.RetainState {
		report.Graph = g
		report.DBPath = dbPath
		report.cleanUp = func() {
			st.Close()
			os.RemoveAll(tmpDir)
		}
	}

	// ── Output ───────────────────────────────────────────────────────────

	fmt.Fprintf(cfg.Output, "\n=== Synapses Load Test: %s (%s) ===\n", cfg.Size, root)
	fmt.Fprintf(cfg.Output, "Go %s, GOMAXPROCS=%d, %s/%s\n\n",
		runtime.Version(), runtime.GOMAXPROCS(0), runtime.GOOS, runtime.GOARCH)
	WriteConsoleTable(cfg.Output, report.Stages)

	if cfg.JSONOutput != nil {
		if err := WriteJSON(cfg.JSONOutput, report); err != nil {
			fmt.Fprintf(cfg.Output, "warning: write JSON: %v\n", err)
		}
	}

	if cfg.BenchstatOutput != nil {
		WriteBenchstat(cfg.BenchstatOutput, report.Stages)
	}

	return report, nil
}

// collectEntityTexts gathers up to maxN entity names/labels from the graph
// for embedding. It prefers functions and types over files.
func collectEntityTexts(g *graph.Graph, maxN int) []string {
	nodes := g.AllNodes()
	texts := make([]string, 0, min(len(nodes), maxN))
	for _, n := range nodes {
		if len(texts) >= maxN {
			break
		}
		label := n.Name
		if label == "" {
			continue
		}
		texts = append(texts, label)
	}
	return texts
}

// LeakDetectorConfig controls steady-state leak detection.
type LeakDetectorConfig struct {
	Duration     time.Duration // observation window (default 5m)
	SampleEvery  time.Duration // polling interval (default 10s)
	AbsThreshold int64         // bytes — flag if growth exceeds this
	PctThreshold float64       // fraction — flag if growth exceeds this % of baseline
}

// DefaultLeakDetectorConfig returns sensible defaults.
func DefaultLeakDetectorConfig() LeakDetectorConfig {
	return LeakDetectorConfig{
		Duration:     5 * time.Minute,
		SampleEvery:  10 * time.Second,
		AbsThreshold: 50 * 1024 * 1024, // 50 MB
		PctThreshold: 0.05,              // 5%
	}
}

// RunLeakDetection monitors heap for growth over a quiescent period.
// Call this after the full pipeline completes.
func RunLeakDetection(cfg LeakDetectorConfig) *LeakResult {
	// Force clean state.
	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(500 * time.Millisecond)

	baseline := readSnapshot()
	start := time.Now()
	deadline := start.Add(cfg.Duration)

	ticker := time.NewTicker(cfg.SampleEvery)
	defer ticker.Stop()

	var maxHeap int64
	for {
		select {
		case <-ticker.C:
			snap := readSnapshot()
			if snap.HeapInuse > maxHeap {
				maxHeap = snap.HeapInuse
			}
			if time.Now().After(deadline) {
				goto done
			}
		}
	}

done:
	final := readSnapshot()
	growth := final.HeapInuse - baseline.HeapInuse
	var growthPct float64
	if baseline.HeapInuse > 0 {
		growthPct = float64(growth) / float64(baseline.HeapInuse)
	}

	result := &LeakResult{
		Duration:     time.Since(start),
		BaselineHeap: baseline.HeapInuse,
		FinalHeap:    final.HeapInuse,
		GrowthBytes:  growth,
		GrowthPct:    growthPct,
		Passed:       true,
	}

	if growth > cfg.AbsThreshold {
		result.Passed = false
		result.Reason = fmt.Sprintf("heap grew by %s (threshold %s)",
			fmtBytes(growth), fmtBytes(cfg.AbsThreshold))
	} else if growthPct > cfg.PctThreshold {
		result.Passed = false
		result.Reason = fmt.Sprintf("heap grew by %.1f%% (threshold %.1f%%)",
			growthPct*100, cfg.PctThreshold*100)
	}

	return result
}

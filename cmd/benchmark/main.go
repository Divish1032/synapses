// Command benchmark is a standalone binary for running Tier 1 external benchmarks
// against a running Synapses daemon. It is purely external — it calls the daemon
// via the REST HTTP transport (/v1/tools/{tool}?project=...) and does not import
// any internal Synapses packages.
//
// Usage:
//
//	benchmark --benchmark=contextbench --cb-data=contextbench.jsonl --limit=50
//	benchmark --benchmark=graphbench --gb-data=graphbench.jsonl
//	benchmark --benchmark=featurebench --fb-split=lite --mode=synapses
//	benchmark --benchmark=driftbench --db-data=driftbench.jsonl
//	benchmark --benchmark=recallbench --rb-data=recallbench.jsonl
//	benchmark --benchmark=swe-verified --swe-data=swebench_pilot.jsonl  # optional sanity check
package main

import (
	"flag"
	"log"
	"os"
	"strings"

	"github.com/SynapsesOS/synapses/cmd/benchmark/agent"
	"github.com/SynapsesOS/synapses/cmd/benchmark/benchmarks"
	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

func main() {
	var (
		benchmarkName = flag.String("benchmark", "contextbench", "Benchmark to run: contextbench | graphbench | featurebench | compactionbench | driftbench | recallbench | nlbench | swe-verified (optional)")
		endpoint      = flag.String("endpoint", "http://127.0.0.1:11435", "Synapses daemon REST endpoint")
		project       = flag.String("project", "", "Single project path (overrides per-repo routing)")
		outputDir     = flag.String("output-dir", "results", "Directory to write JSON and markdown results")
		limit         = flag.Int("limit", 0, "Max tasks (0 = all)")
		noSynapses    = flag.Bool("no-synapses", false, "Control run: disable Synapses MCP")
		reposDir      = flag.String("repos-dir", "/tmp/bench_repos", "Directory where repos are cloned")
		cacheFile     = flag.String("cache-file", "/tmp/bench_index_cache.json", "JSON cache of indexed repos")
		indexWorkers  = flag.Int("index-workers", 8, "Parallel workers for cloning+indexing")
		// index-only removed — use indexer.Run() directly if needed.
		skipIndex     = flag.Bool("skip-index", false, "Skip synapses index step (clone only)")
		// ContextBench-specific flags.
		cbDataFile  = flag.String("cb-data", "contextbench.jsonl", "Path to ContextBench JSONL dataset")
		cbLanguages = flag.String("cb-languages", "", "Comma-separated language filter for ContextBench (empty = all)")
		cbSources   = flag.String("cb-sources", "", "Comma-separated source filter for ContextBench (e.g. Verified)")
		cbWarmup    = flag.Int("cb-warmup", 0, "Cold/warm comparison: run N warmup sessions, then compare F1 (0 = off)")
		cbCompaction = flag.Bool("cb-compaction", false, "Compaction mode: test recovery quality after simulated context loss")
		cbMode       = flag.String("cb-mode", "heuristic", "ContextBench mode: heuristic (daemon-only) | llm (Claude + optional MCP)")
		cbLLMModel   = flag.String("cb-llm-model", "claude-sonnet-4-6", "Claude model for LLM contextbench")
		cbLLMTimeout = flag.Int("cb-llm-timeout", 180, "Timeout per task in seconds for LLM contextbench")
		cbBothModes  = flag.Bool("cb-both-modes", false, "LLM mode: run baseline + synapses, compute delta")
		cbLLMDebug   = flag.Bool("cb-debug", false, "Dump raw stream-json for LLM contextbench")
		// GraphBench-specific flags.
		gbDataFile = flag.String("gb-data", "graphbench.jsonl", "Path to GraphBench JSONL dataset")
		gbMode     = flag.String("gb-mode", "full", "GraphBench mode: full (curated ground truth) | smoke (self-validating, CI-safe)")
		// NLBench-specific flags.
		nlDataFile = flag.String("nl-data", "nlbench.jsonl", "Path to NLBench JSONL dataset")
		// DriftBench-specific flags.
		dbDataFile  = flag.String("db-data", "driftbench.jsonl", "Path to DriftBench JSONL dataset")
		dbSkipClean = flag.Bool("db-skip-clean", false, "Skip clean reindex verification (trust dataset ground truth)")
		// RecallBench-specific flags.
		rbDataFile = flag.String("rb-data", "recallbench.jsonl", "Path to RecallBench JSONL dataset")
		// SWE-bench-specific flags.
		sweDataFile = flag.String("swe-data", "swebench_pilot.jsonl", "Path to SWE-bench JSONL dataset")
		sweMode     = flag.String("mode", "baseline", "Agent mode: baseline | synapses")
		sweModel    = flag.String("model", "claude-sonnet-4-6", "Claude model for SWE-bench agent")
		// FeatureBench-specific flags.
		fbSplit     = flag.String("fb-split", "lite", "FeatureBench split: lite | fast | full")
		fbTaskIDs   = flag.String("fb-task-ids", "", "Comma-separated FeatureBench task IDs")
		fbLevel     = flag.Int("fb-level", 0, "FeatureBench level filter: 1 or 2 (0 = all)")
		fbTimeout   = flag.Int("fb-timeout", 1200, "Timeout per FeatureBench task in seconds")
		fbDebug     = flag.Bool("fb-debug", false, "Dump raw stream-json to file for MCP tool inspection")
		fbBothModes = flag.Bool("fb-both-modes", false, "Run both baseline and synapses modes, compute agent lift delta")
		sweMaxTurns = flag.Int("max-turns", 25, "Max agent loop turns for SWE-bench")
	)
	flag.Parse()

	*benchmarkName = strings.ToLower(strings.TrimSpace(*benchmarkName))

	// ── Build MCP client ────────────────────────────────────────────────────
	var mcpClient *agent.SynapsesClient
	if *noSynapses {
		mcpClient = agent.NewDisabledClient()
	} else {
		mcpClient = agent.NewClient(*endpoint, *project)
	}

	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	rep := reporter.New(*outputDir)

	switch *benchmarkName {
	case "contextbench":
		if *cbMode == "llm" {
			// LLM-powered ContextBench: Claude identifies relevant context.
			llmMode := *sweMode // reuse --mode flag (baseline|synapses)
			if llmMode == "" {
				llmMode = "synapses"
			}

			runLLMCB := func(mode string) *reporter.LLMContextBenchResult {
				llmOpts := benchmarks.LLMContextBenchOptions{
					DataFile:    *cbDataFile,
					ReposDir:    *reposDir,
					Limit:       *limit,
					Languages:   splitComma(*cbLanguages),
					Sources:     splitComma(*cbSources),
					Mode:        mode,
					Model:       *cbLLMModel,
					Timeout:     *cbLLMTimeout,
					OutputDir:   *outputDir,
					Debug:       *cbLLMDebug,
					DaemonPort:  "11435",
				}
				result, err := benchmarks.RunLLMContextBench(llmOpts)
				if err != nil {
					log.Fatalf("llm contextbench (%s) failed: %v", mode, err)
				}
				return result
			}

			if *cbBothModes {
				log.Printf("LLM ContextBench: both-modes — running baseline then synapses")
				baselineResult := runLLMCB("baseline")
				synapsesResult := runLLMCB("synapses")

				// Attach comparison to the synapses result.
				synapsesResult.BothModes = &reporter.LLMContextBenchComparison{
					BaselineF1:      baselineResult.AvgF1,
					SynapsesF1:      synapsesResult.AvgF1,
					AgentLift:       synapsesResult.AvgF1 - baselineResult.AvgF1,
					BaselineFileF1:  baselineResult.AvgFileF1,
					SynapsesFileF1:  synapsesResult.AvgFileF1,
					FileAgentLift:   synapsesResult.AvgFileF1 - baselineResult.AvgFileF1,
					BaselineAvgCost: baselineResult.AvgCostUSD,
					SynapsesAvgCost: synapsesResult.AvgCostUSD,
				}
				if baselineResult.AvgCostUSD > 0 {
					synapsesResult.BothModes.CostSavingsPct = (baselineResult.AvgCostUSD - synapsesResult.AvgCostUSD) / baselineResult.AvgCostUSD * 100
				}

				if err := rep.WriteLLMContextBench(baselineResult); err != nil {
					log.Fatalf("write baseline results: %v", err)
				}
				if err := rep.WriteLLMContextBench(synapsesResult); err != nil {
					log.Fatalf("write synapses results: %v", err)
				}
				rep.PrintLLMContextBenchSummary(synapsesResult)
			} else {
				result := runLLMCB(llmMode)
				if err := rep.WriteLLMContextBench(result); err != nil {
					log.Fatalf("write results: %v", err)
				}
				rep.PrintLLMContextBenchSummary(result)
			}
		} else {
			// Heuristic ContextBench (existing, daemon-only).
			cbOpts := benchmarks.ContextBenchOptions{
				DataFile:       *cbDataFile,
				ReposDir:       *reposDir,
				CacheFile:      *cacheFile,
				Limit:          *limit,
				Languages:      splitComma(*cbLanguages),
				Sources:        splitComma(*cbSources),
				IndexWorkers:   *indexWorkers,
				SkipIndex:      *skipIndex,
				WarmupSessions: *cbWarmup,
				CompactionMode: *cbCompaction,
			}
			cbResult, err := benchmarks.RunContextBench(mcpClient, cbOpts)
			if err != nil {
				log.Fatalf("contextbench failed: %v", err)
			}
			if err := rep.WriteContextBench(cbResult); err != nil {
				log.Fatalf("write results: %v", err)
			}
			rep.PrintContextBenchSummary(cbResult)
		}

	case "graphbench", "graph-bench", "graph_bench":
		gbOpts := benchmarks.GraphBenchOptions{
			DataFile: *gbDataFile,
			ReposDir: *reposDir,
			Limit:    *limit,
			Mode:     *gbMode,
		}
		gbResult, err := benchmarks.RunGraphBench(mcpClient, gbOpts)
		if err != nil {
			log.Fatalf("graphbench failed: %v", err)
		}
		if err := rep.WriteGraphBench(gbResult); err != nil {
			log.Fatalf("write results: %v", err)
		}
		rep.PrintGraphBenchSummary(gbResult)

	case "nlbench", "nl-bench", "nl_bench":
		nlOpts := benchmarks.NLBenchOptions{
			DataFile: *nlDataFile,
			ReposDir: *reposDir,
			Limit:    *limit,
		}
		nlResult, err := benchmarks.RunNLBench(mcpClient, nlOpts)
		if err != nil {
			log.Fatalf("nlbench failed: %v", err)
		}
		if err := rep.WriteNLBench(nlResult); err != nil {
			log.Fatalf("write results: %v", err)
		}
		rep.PrintNLBenchSummary(nlResult)

	case "swe-verified", "swe_verified":
		sweResult, err := benchmarks.RunSWEBench(mcpClient, benchmarks.SWEBenchOptions{
			DataFile: *sweDataFile,
			ReposDir: *reposDir,
			Limit:    *limit,
			Mode:     *sweMode,
			Model:    *sweModel,
			MaxTurns: *sweMaxTurns,
			Endpoint: *endpoint,
		})
		if err != nil {
			log.Fatalf("swe-bench failed: %v", err)
		}
		if err := rep.WriteSWEBench(sweResult); err != nil {
			log.Fatalf("write results: %v", err)
		}
		rep.PrintSWEBenchSummary(sweResult)

	case "featurebench", "feature-bench", "feature_bench":
		baseOpts := benchmarks.FeatureBenchOptions{
			Split:     *fbSplit,
			TaskIDs:   splitComma(*fbTaskIDs),
			Level:     *fbLevel,
			ReposDir:  *reposDir,
			Limit:     *limit,
			Mode:      *sweMode,
			Model:     *sweModel,
			Timeout:   *fbTimeout,
			OutputDir: *outputDir,
			Debug:     *fbDebug,
			BothModes: *fbBothModes,
		}

		if *fbBothModes {
			// Run baseline first, then synapses, compute delta.
			log.Printf("featurebench: both-modes enabled — running baseline then synapses")

			baselineOpts := baseOpts
			baselineOpts.Mode = "baseline"
			baselineResults, err := benchmarks.RunFeatureBench(baselineOpts)
			if err != nil {
				log.Fatalf("featurebench baseline failed: %v", err)
			}
			baselineReport := benchmarks.BuildFeatureBenchReport("baseline", *sweModel, baselineResults)

			synapsesOpts := baseOpts
			synapsesOpts.Mode = "synapses"
			synapsesResults, err := benchmarks.RunFeatureBench(synapsesOpts)
			if err != nil {
				log.Fatalf("featurebench synapses failed: %v", err)
			}
			synapsesReport := benchmarks.BuildFeatureBenchReport("synapses", *sweModel, synapsesResults)

			// Compute comparison.
			comparison := &reporter.FeatureBenchComparison{
				BaselinePatchRate:  baselineReport.PatchRate,
				SynapsesPatchRate:  synapsesReport.PatchRate,
				AgentLift:          synapsesReport.PatchRate - baselineReport.PatchRate,
				BaselineAvgCost:    baselineReport.AvgCostUSD,
				SynapsesAvgCost:    synapsesReport.AvgCostUSD,
				BaselineAvgTokens:  baselineReport.AvgInputTokens + baselineReport.AvgOutputTokens,
				SynapsesAvgTokens:  synapsesReport.AvgInputTokens + synapsesReport.AvgOutputTokens,
			}
			if baselineReport.AvgCostUSD > 0 {
				comparison.CostSavingsPct = (baselineReport.AvgCostUSD - synapsesReport.AvgCostUSD) / baselineReport.AvgCostUSD * 100
			}
			if comparison.BaselineAvgTokens > 0 {
				comparison.TokenSavingsPct = float64(comparison.BaselineAvgTokens-comparison.SynapsesAvgTokens) / float64(comparison.BaselineAvgTokens) * 100
			}

			// Attach comparison to the synapses report (primary output).
			synapsesReport.BothModes = comparison
			if err := rep.WriteFeatureBench(synapsesReport); err != nil {
				log.Fatalf("write results: %v", err)
			}
			rep.PrintFeatureBenchSummary(synapsesReport)

			log.Printf("\n=== Both-Modes Comparison ===")
			log.Printf("Baseline patch rate: %.1f%%", comparison.BaselinePatchRate)
			log.Printf("Synapses patch rate: %.1f%%", comparison.SynapsesPatchRate)
			log.Printf("Agent lift: %+.1f%%", comparison.AgentLift)
			log.Printf("Cost savings: %.1f%%", comparison.CostSavingsPct)
			log.Printf("Token savings: %.1f%%", comparison.TokenSavingsPct)
		} else {
			fbResults, err := benchmarks.RunFeatureBench(baseOpts)
			if err != nil {
				log.Fatalf("featurebench failed: %v", err)
			}
			fbReport := benchmarks.BuildFeatureBenchReport(*sweMode, *sweModel, fbResults)
			if err := rep.WriteFeatureBench(fbReport); err != nil {
				log.Fatalf("write results: %v", err)
			}
			rep.PrintFeatureBenchSummary(fbReport)
		}

	case "compactionbench", "compaction-bench", "compaction_bench":
		cbResults, err := benchmarks.RunCompactionBench(benchmarks.CompactionBenchOptions{
			Split:     *fbSplit,
			TaskIDs:   splitComma(*fbTaskIDs),
			Level:     *fbLevel,
			ReposDir:  *reposDir,
			Limit:     *limit,
			Mode:      *sweMode,
			Model:     *sweModel,
			P1Timeout: 300,
			P2Timeout: *fbTimeout,
			OutputDir: *outputDir,
			Debug:     *fbDebug,
		})
		if err != nil {
			log.Fatalf("compactionbench failed: %v", err)
		}
		cbReport := benchmarks.BuildCompactionBenchReport(*sweMode, *sweModel, cbResults)
		if err := rep.WriteCompactionBench(cbReport); err != nil {
			log.Fatalf("write results: %v", err)
		}
		rep.PrintCompactionBenchSummary(cbReport)

	case "driftbench", "drift-bench", "drift_bench":
		dbResult, err := benchmarks.RunDriftBench(mcpClient, benchmarks.DriftBenchOptions{
			DataFile:  *dbDataFile,
			ReposDir:  *reposDir,
			Limit:     *limit,
			SkipClean: *dbSkipClean,
			OutputDir: *outputDir,
		})
		if err != nil {
			log.Fatalf("driftbench failed: %v", err)
		}
		if err := rep.WriteDriftBench(dbResult); err != nil {
			log.Fatalf("write results: %v", err)
		}
		rep.PrintDriftBenchSummary(dbResult)

	case "recallbench", "recall-bench", "recall_bench":
		rbResult, err := benchmarks.RunRecallBench(mcpClient, benchmarks.RecallBenchOptions{
			DataFile: *rbDataFile,
			ReposDir: *reposDir,
			Limit:    *limit,
		})
		if err != nil {
			log.Fatalf("recallbench failed: %v", err)
		}
		if err := rep.WriteRecallBench(rbResult); err != nil {
			log.Fatalf("write results: %v", err)
		}
		rep.PrintRecallBenchSummary(rbResult)

	default:
		log.Fatalf("unknown benchmark %q", *benchmarkName)
	}
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

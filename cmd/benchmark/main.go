// Command benchmark is a standalone binary for running Tier 1 external benchmarks
// against a running Synapses daemon. It is purely external — it calls the daemon
// via the REST HTTP transport (/v1/tools/{tool}?project=...) and does not import
// any internal Synapses packages.
//
// Usage:
//
//	benchmark --benchmark=contextbench --cb-data=contextbench.jsonl --limit=50
//	benchmark --benchmark=graphbench --gb-data=graphbench.jsonl
//	benchmark --benchmark=driftbench --db-data=driftbench.jsonl
//	benchmark --benchmark=recallbench --rb-data=recallbench.jsonl
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
		benchmarkName = flag.String("benchmark", "contextbench", "Benchmark to run: contextbench | graphbench | taskbench | compactionbench | driftbench | recallbench | nlbench")
		endpoint      = flag.String("endpoint", "http://127.0.0.1:11435", "Synapses daemon REST endpoint")
		project       = flag.String("project", "", "Single project path (overrides per-repo routing)")
		outputDir     = flag.String("output-dir", "results", "Directory to write JSON and markdown results")
		limit         = flag.Int("limit", 0, "Max tasks (0 = all)")
		noSynapses    = flag.Bool("no-synapses", false, "Control run: disable Synapses MCP")
		reposDir      = flag.String("repos-dir", "/tmp/bench_repos", "Directory where repos are cloned")
		cacheFile     = flag.String("cache-file", "/tmp/bench_index_cache.json", "JSON cache of indexed repos")
		indexWorkers  = flag.Int("index-workers", 8, "Parallel workers for cloning+indexing")
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
		gbDataFile   = flag.String("gb-data", "graphbench.jsonl", "Path to GraphBench JSONL dataset")
		gbMode       = flag.String("gb-mode", "full", "GraphBench mode: full (curated ground truth) | smoke (self-validating, CI-safe)")
		gbSequential = flag.Bool("gb-sequential", true, "OOM-safe: clone→index→test→cleanup one repo at a time")
		gbRepoFilter = flag.String("gb-repo", "", "Only run suites whose repo name contains this substring")
		gbCompareLSP = flag.Bool("gb-compare-lsp", false, "Enable LSP call hierarchy comparison for Go/TypeScript find_callers and find_callees tests")
		// NLBench-specific flags.
		nlDataFile = flag.String("nl-data", "nlbench.jsonl", "Path to NLBench JSONL dataset")
		// DriftBench-specific flags.
		dbDataFile  = flag.String("db-data", "driftbench.jsonl", "Path to DriftBench JSONL dataset")
		dbSkipClean = flag.Bool("db-skip-clean", false, "Skip clean reindex verification (trust dataset ground truth)")
		// RecallBench-specific flags.
		rbDataFile = flag.String("rb-data", "recallbench.jsonl", "Path to RecallBench JSONL dataset")
		// Shared agent flags (used by CompactionBench, LLM ContextBench).
		agentMode  = flag.String("mode", "baseline", "Agent mode: baseline | synapses")
		agentModel = flag.String("model", "claude-sonnet-4-6", "Claude model for agent-based benchmarks")
		// CompactionBench-specific flags.
		compSplit   = flag.String("comp-split", "lite", "CompactionBench split: lite | fast | full")
		compTaskIDs = flag.String("comp-task-ids", "", "Comma-separated CompactionBench task IDs")
		compLevel   = flag.Int("comp-level", 0, "CompactionBench level filter: 1 or 2 (0 = all)")
		compTimeout = flag.Int("comp-timeout", 1200, "Timeout per CompactionBench task in seconds")
		compDebug   = flag.Bool("comp-debug", false, "Dump raw stream-json to file for MCP tool inspection")
		// TaskBench-specific flags.
		tbData       = flag.String("tb-data", "", "Path to TaskBench JSONL (default: load SWE-bench from HF)")
		tbTimeout    = flag.Int("tb-timeout", 600, "Timeout per TaskBench task in seconds")
		tbEval       = flag.Bool("tb-eval", true, "Run Docker eval after all tasks")
		tbBothModes  = flag.Bool("tb-both-modes", false, "Run baseline + synapses, compute delta")
		tbInstanceIDs = flag.String("tb-instance-ids", "", "Comma-separated instance IDs to filter")
		tbDebug      = flag.Bool("tb-debug", false, "Dump raw stream-json per task")
		tbDataset    = flag.String("tb-dataset", "", "Eval dataset (default: SWE-bench_Verified, or LiberCoders/FeatureBench)")
		tbFeature    = flag.Bool("tb-feature", false, "Use feature implementation prompt (FeatureBench) instead of bug-fix prompt")
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
			llmMode := *agentMode
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
			DataFile:   *gbDataFile,
			ReposDir:   *reposDir,
			OutputDir:  *outputDir,
			Limit:      *limit,
			Mode:       *gbMode,
			Sequential: *gbSequential,
			RepoFilter: *gbRepoFilter,
			CompareLSP: *gbCompareLSP,
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

	case "compactionbench", "compaction-bench", "compaction_bench":
		cbResults, err := benchmarks.RunCompactionBench(benchmarks.CompactionBenchOptions{
			Split:     *compSplit,
			TaskIDs:   splitComma(*compTaskIDs),
			Level:     *compLevel,
			ReposDir:  *reposDir,
			Limit:     *limit,
			Mode:      *agentMode,
			Model:     *agentModel,
			P1Timeout: 300,
			P2Timeout: *compTimeout,
			OutputDir: *outputDir,
			Debug:     *compDebug,
		})
		if err != nil {
			log.Fatalf("compactionbench failed: %v", err)
		}
		cbReport := benchmarks.BuildCompactionBenchReport(*agentMode, *agentModel, cbResults)
		if err := rep.WriteCompactionBench(cbReport); err != nil {
			log.Fatalf("write results: %v", err)
		}
		rep.PrintCompactionBenchSummary(cbReport)

	case "taskbench", "task-bench", "task_bench":
		runTB := func(mode string) *reporter.TaskBenchReport {
			tbOpts := benchmarks.TaskBenchOptions{
				DataFile:    *tbData,
				ReposDir:    *reposDir,
				Limit:       *limit,
				Mode:        mode,
				Model:       *agentModel,
				Timeout:     *tbTimeout,
				OutputDir:   *outputDir,
				Debug:       *tbDebug,
				Eval:        *tbEval,
				DaemonPort:  "11435",
				InstanceIDs: splitComma(*tbInstanceIDs),
				Dataset:     *tbDataset,
				IsFeature:   *tbFeature,
			}
			results, err := benchmarks.RunTaskBench(tbOpts)
			if err != nil {
				log.Fatalf("taskbench (%s) failed: %v", mode, err)
			}
			// Run eval if enabled.
			if *tbEval {
				if evalErr := benchmarks.EvalTaskBench(results, *outputDir, *tbDataset); evalErr != nil {
					log.Printf("WARNING: eval failed: %v (results still available without resolve status)", evalErr)
				}
			}
			return benchmarks.BuildTaskBenchReport(mode, *agentModel, results)
		}

		if *tbBothModes {
			log.Printf("TaskBench: both-modes — running baseline then synapses")
			baselineReport := runTB("baseline")
			synapsesReport := runTB("synapses")

			synapsesReport.BothModes = &reporter.TaskBenchComparison{
				BaselineResolveRate: baselineReport.ResolveRate,
				SynapsesResolveRate: synapsesReport.ResolveRate,
				ResolveLift:         synapsesReport.ResolveRate - baselineReport.ResolveRate,
				BaselineAvgTurns:    baselineReport.AvgTurns,
				SynapsesAvgTurns:    synapsesReport.AvgTurns,
				TurnsSaved:          baselineReport.AvgTurns - synapsesReport.AvgTurns,
				BaselineAvgCost:     baselineReport.AvgCostUSD,
				SynapsesAvgCost:     synapsesReport.AvgCostUSD,
			}
			if baselineReport.AvgCostUSD > 0 {
				synapsesReport.BothModes.CostSavingsPct = (baselineReport.AvgCostUSD - synapsesReport.AvgCostUSD) / baselineReport.AvgCostUSD * 100
			}

			if err := rep.WriteTaskBench(baselineReport); err != nil {
				log.Fatalf("write baseline: %v", err)
			}
			if err := rep.WriteTaskBench(synapsesReport); err != nil {
				log.Fatalf("write synapses: %v", err)
			}
			rep.PrintTaskBenchSummary(synapsesReport)
		} else {
			mode := *agentMode
			if mode == "" {
				mode = "baseline"
			}
			report := runTB(mode)
			if err := rep.WriteTaskBench(report); err != nil {
				log.Fatalf("write results: %v", err)
			}
			rep.PrintTaskBenchSummary(report)
		}

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

	case "securitybench", "security-bench", "security_bench":
		sbData := "securitybench.jsonl"
		if *cbDataFile != "contextbench.jsonl" {
			sbData = *cbDataFile // allow override via --cb-data (reuse flag)
		}
		sbResult, err := benchmarks.RunSecurityBench(sbData)
		if err != nil {
			log.Fatalf("securitybench failed: %v", err)
		}
		if err := rep.WriteSecurityBench(sbResult); err != nil {
			log.Fatalf("write results: %v", err)
		}
		rep.PrintSecurityBenchSummary(sbResult)

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

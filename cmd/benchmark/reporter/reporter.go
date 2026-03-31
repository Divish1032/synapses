// Package reporter writes benchmark results to disk as JSON and Markdown.
// The JSON file is machine-readable for post-processing and leaderboard
// submission. The Markdown file is the human-readable report.
package reporter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Reporter writes benchmark results to an output directory.
type Reporter struct {
	dir string
}

// New creates a reporter that writes files into dir.
func New(dir string) *Reporter {
	return &Reporter{dir: dir}
}

// ─── ContextBench ─────────────────────────────────────────────────────────────

// ContextBenchResult holds the full results of a ContextBench run.
type ContextBenchResult struct {
	Timestamp          string                   `json:"timestamp"`
	TotalTasks         int                      `json:"total_tasks"`
	AvgPrecision       float64                  `json:"avg_precision"`
	AvgRecall          float64                  `json:"avg_recall"`
	AvgF1              float64                  `json:"avg_f1"`
	AvgTokensRetrieved int                      `json:"avg_tokens_retrieved,omitempty"`
	AvgTokensGold      int                      `json:"avg_tokens_gold,omitempty"`
	AvgTokenPrecision  float64                  `json:"avg_token_precision,omitempty"`
	TokenEfficiency    float64                  `json:"token_efficiency,omitempty"` // gold/retrieved ratio
	// File-level metrics.
	AvgFilePrecision   float64                  `json:"avg_file_precision,omitempty"`
	AvgFileRecall      float64                  `json:"avg_file_recall,omitempty"`
	AvgFileF1          float64                  `json:"avg_file_f1,omitempty"`
	PerLanguage        []ContextBenchLangResult `json:"per_language"`
	TaskResults        []interface{}            `json:"tasks"` // []ContextBenchTaskResult from benchmarks pkg
	// Multi-budget evaluation (always populated).
	MultiBudget *MultiBudgetResult `json:"multi_budget,omitempty"`
	// Cold/warm comparison (populated when --cb-warmup > 0).
	ColdWarm *ColdWarmComparison `json:"cold_warm,omitempty"`
	// Compaction comparison (populated when --cb-compaction).
	Compaction *CompactionComparison `json:"compaction,omitempty"`
}

// MultiBudgetResult holds F1 at different retrieval budgets.
type MultiBudgetResult struct {
	Budget250  BudgetMetrics `json:"budget_250"`
	Budget500  BudgetMetrics `json:"budget_500"`
	Budget1000 BudgetMetrics `json:"budget_1000"`
}

// BudgetMetrics holds avg metrics for one budget level.
type BudgetMetrics struct {
	Budget       int     `json:"budget"`
	AvgPrecision float64 `json:"avg_precision"`
	AvgRecall    float64 `json:"avg_recall"`
	AvgF1        float64 `json:"avg_f1"`
}

// ColdWarmComparison holds the learning lift measurement.
type ColdWarmComparison struct {
	ColdF1         float64 `json:"cold_f1"`
	WarmF1         float64 `json:"warm_f1"`
	LearningLift   float64 `json:"learning_lift"` // warm - cold
	WarmupSessions int     `json:"warmup_sessions"`
}

// CompactionComparison holds pre/post compaction measurement.
type CompactionComparison struct {
	PreCompactionF1  float64 `json:"pre_compaction_f1"`
	PostCompactionF1 float64 `json:"post_compaction_f1"`
	RecoveryDelta    float64 `json:"recovery_delta"` // post - pre (negative = lost quality)
}

// ContextBenchLangResult holds per-language metrics.
type ContextBenchLangResult struct {
	Language     string  `json:"language"`
	Tasks        int     `json:"tasks"`
	AvgPrecision float64 `json:"avg_precision"`
	AvgRecall    float64 `json:"avg_recall"`
	AvgF1        float64 `json:"avg_f1"`
}

// ─── LLM ContextBench ─────────────────────────────────────────────────────────

// LLMContextBenchResult holds results for LLM-powered contextbench.
type LLMContextBenchResult struct {
	Timestamp        string                     `json:"timestamp"`
	Mode             string                     `json:"mode"`   // "baseline" or "synapses"
	Model            string                     `json:"model"`
	TotalTasks       int                        `json:"total_tasks"`
	AvgPrecision     float64                    `json:"avg_precision"`
	AvgRecall        float64                    `json:"avg_recall"`
	AvgF1            float64                    `json:"avg_f1"`
	AvgFilePrecision float64                    `json:"avg_file_precision"`
	AvgFileRecall    float64                    `json:"avg_file_recall"`
	AvgFileF1        float64                    `json:"avg_file_f1"`
	AvgTurns         float64                    `json:"avg_turns"`
	AvgCostUSD       float64                    `json:"avg_cost_usd"`
	TotalCostUSD     float64                    `json:"total_cost_usd"`
	ToolUsage        map[string]int             `json:"tool_usage"`
	PerLanguage      []ContextBenchLangResult   `json:"per_language"`
	Tasks            []interface{}              `json:"tasks"`
	BothModes        *LLMContextBenchComparison `json:"both_modes,omitempty"`
}

// LLMContextBenchComparison holds baseline vs synapses delta.
type LLMContextBenchComparison struct {
	BaselineF1      float64 `json:"baseline_f1"`
	SynapsesF1      float64 `json:"synapses_f1"`
	AgentLift       float64 `json:"agent_lift"`
	BaselineFileF1  float64 `json:"baseline_file_f1"`
	SynapsesFileF1  float64 `json:"synapses_file_f1"`
	FileAgentLift   float64 `json:"file_agent_lift"`
	BaselineAvgCost float64 `json:"baseline_avg_cost"`
	SynapsesAvgCost float64 `json:"synapses_avg_cost"`
	CostSavingsPct  float64 `json:"cost_savings_pct"`
}

// WriteLLMContextBench writes JSON + Markdown results for an LLM ContextBench run.
func (r *Reporter) WriteLLMContextBench(result *LLMContextBenchResult) error {
	ts := strings.ReplaceAll(result.Timestamp, ":", "-")
	jsonPath := filepath.Join(r.dir, fmt.Sprintf("contextbench_llm_%s_%s.json", result.Mode, ts))
	mdPath := filepath.Join(r.dir, fmt.Sprintf("contextbench_llm_%s_%s.md", result.Mode, ts))
	if err := writeJSON(jsonPath, result); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := os.WriteFile(mdPath, []byte(llmContextBenchMarkdown(result)), 0o644); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}
	fmt.Printf("Results written:\n  JSON: %s\n  Markdown: %s\n", jsonPath, mdPath)
	return nil
}

// PrintLLMContextBenchSummary prints a compact summary to stdout.
func (r *Reporter) PrintLLMContextBenchSummary(result *LLMContextBenchResult) {
	fmt.Printf("\n=== LLM ContextBench Summary (%s mode, %s) ===\n", result.Mode, result.Model)
	fmt.Printf("Tasks: %d | Line F1: %.1f%% (P=%.1f%% R=%.1f%%) | File F1: %.1f%% (P=%.1f%% R=%.1f%%)\n",
		result.TotalTasks,
		result.AvgF1*100, result.AvgPrecision*100, result.AvgRecall*100,
		result.AvgFileF1*100, result.AvgFilePrecision*100, result.AvgFileRecall*100,
	)
	fmt.Printf("Avg turns: %.1f | Avg cost: $%.4f | Total cost: $%.4f\n",
		result.AvgTurns, result.AvgCostUSD, result.TotalCostUSD)

	if len(result.ToolUsage) > 0 {
		fmt.Printf("\nTool Usage:\n")
		for name, count := range result.ToolUsage {
			fmt.Printf("  %-35s %d\n", name, count)
		}
	}

	if len(result.PerLanguage) > 0 {
		fmt.Printf("\n%-12s  %6s  %8s  %8s\n", "Language", "Tasks", "Line F1", "File F1")
		fmt.Printf("%s\n", strings.Repeat("-", 40))
		for _, l := range result.PerLanguage {
			fmt.Printf("%-12s  %6d  %7.1f%%  %7.1f%%\n",
				l.Language, l.Tasks, l.AvgF1*100, l.AvgPrecision*100)
		}
	}

	if result.BothModes != nil {
		bm := result.BothModes
		fmt.Printf("\n=== Baseline vs Synapses ===\n")
		fmt.Printf("Line F1:  baseline=%.1f%%  synapses=%.1f%%  lift=%+.1f%%\n",
			bm.BaselineF1*100, bm.SynapsesF1*100, bm.AgentLift*100)
		fmt.Printf("File F1:  baseline=%.1f%%  synapses=%.1f%%  lift=%+.1f%%\n",
			bm.BaselineFileF1*100, bm.SynapsesFileF1*100, bm.FileAgentLift*100)
		fmt.Printf("Cost:     baseline=$%.4f  synapses=$%.4f  savings=%.1f%%\n",
			bm.BaselineAvgCost, bm.SynapsesAvgCost, bm.CostSavingsPct)
	}
}

func llmContextBenchMarkdown(result *LLMContextBenchResult) string {
	var sb strings.Builder
	sb.WriteString("# LLM ContextBench Results\n\n")
	fmt.Fprintf(&sb, "**Mode:** %s | **Model:** %s | **Tasks:** %d\n\n", result.Mode, result.Model, result.TotalTasks)

	sb.WriteString("## Metrics\n\n")
	sb.WriteString("| Metric | Precision | Recall | F1 |\n")
	sb.WriteString("|--------|-----------|--------|----|\n")
	fmt.Fprintf(&sb, "| Line-level | %.1f%% | %.1f%% | %.1f%% |\n",
		result.AvgPrecision*100, result.AvgRecall*100, result.AvgF1*100)
	fmt.Fprintf(&sb, "| File-level | %.1f%% | %.1f%% | %.1f%% |\n\n",
		result.AvgFilePrecision*100, result.AvgFileRecall*100, result.AvgFileF1*100)

	fmt.Fprintf(&sb, "**Avg turns:** %.1f | **Avg cost:** $%.4f | **Total cost:** $%.4f\n\n",
		result.AvgTurns, result.AvgCostUSD, result.TotalCostUSD)

	if result.BothModes != nil {
		bm := result.BothModes
		sb.WriteString("## Baseline vs Synapses\n\n")
		sb.WriteString("| Metric | Baseline | Synapses | Lift |\n")
		sb.WriteString("|--------|----------|----------|------|\n")
		fmt.Fprintf(&sb, "| Line F1 | %.1f%% | %.1f%% | %+.1f%% |\n",
			bm.BaselineF1*100, bm.SynapsesF1*100, bm.AgentLift*100)
		fmt.Fprintf(&sb, "| File F1 | %.1f%% | %.1f%% | %+.1f%% |\n",
			bm.BaselineFileF1*100, bm.SynapsesFileF1*100, bm.FileAgentLift*100)
		fmt.Fprintf(&sb, "| Avg Cost | $%.4f | $%.4f | %.1f%% |\n\n",
			bm.BaselineAvgCost, bm.SynapsesAvgCost, bm.CostSavingsPct)
	}

	if len(result.PerLanguage) > 0 {
		sb.WriteString("## Per-Language\n\n")
		sb.WriteString("| Language | Tasks | Line F1 | File F1 |\n")
		sb.WriteString("|----------|-------|---------|---------|\n")
		for _, l := range result.PerLanguage {
			fmt.Fprintf(&sb, "| %s | %d | %.1f%% | %.1f%% |\n",
				l.Language, l.Tasks, l.AvgF1*100, l.AvgPrecision*100)
		}
	}

	return sb.String()
}

// WriteContextBench writes JSON + Markdown results for a ContextBench run.
func (r *Reporter) WriteContextBench(result *ContextBenchResult) error {
	ts := strings.ReplaceAll(result.Timestamp, ":", "-")
	jsonPath := filepath.Join(r.dir, fmt.Sprintf("contextbench_%s.json", ts))
	mdPath := filepath.Join(r.dir, fmt.Sprintf("contextbench_%s.md", ts))

	if err := writeJSON(jsonPath, result); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := os.WriteFile(mdPath, []byte(contextBenchMarkdown(result)), 0o644); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}
	fmt.Printf("Results written:\n  JSON: %s\n  Markdown: %s\n", jsonPath, mdPath)
	return nil
}

// PrintContextBenchSummary prints a compact summary to stdout.
func (r *Reporter) PrintContextBenchSummary(result *ContextBenchResult) {
	fmt.Printf("\n=== ContextBench Summary ===\n")
	fmt.Printf("Tasks: %d | Line F1: %.1f%% (P=%.1f%% R=%.1f%%) | File F1: %.1f%% (P=%.1f%% R=%.1f%%)\n",
		result.TotalTasks,
		result.AvgF1*100, result.AvgPrecision*100, result.AvgRecall*100,
		result.AvgFileF1*100, result.AvgFilePrecision*100, result.AvgFileRecall*100,
	)
	if result.MultiBudget != nil {
		fmt.Printf("\n%-12s  %10s  %8s  %6s\n", "Budget", "Precision", "Recall", "F1")
		fmt.Printf("%s\n", strings.Repeat("-", 42))
		for _, b := range []BudgetMetrics{result.MultiBudget.Budget250, result.MultiBudget.Budget500, result.MultiBudget.Budget1000} {
			fmt.Printf("%-12d  %9.1f%%  %7.1f%%  %5.1f%%\n",
				b.Budget, b.AvgPrecision*100, b.AvgRecall*100, b.AvgF1*100)
		}
	}

	if len(result.PerLanguage) > 0 {
		fmt.Printf("\n%-12s  %6s  %10s  %8s  %6s\n", "Language", "Tasks", "Precision", "Recall", "F1")
		fmt.Printf("%s\n", strings.Repeat("-", 50))
		for _, l := range result.PerLanguage {
			fmt.Printf("%-12s  %6d  %9.1f%%  %7.1f%%  %5.1f%%\n",
				l.Language, l.Tasks,
				l.AvgPrecision*100,
				l.AvgRecall*100,
				l.AvgF1*100,
			)
		}
	}
}

func contextBenchMarkdown(result *ContextBenchResult) string {
	var sb strings.Builder
	sb.WriteString("# ContextBench Results\n\n")
	fmt.Fprintf(&sb, "**Run timestamp:** %s  \n", result.Timestamp)
	fmt.Fprintf(&sb, "**Total tasks:** %d  \n\n", result.TotalTasks)

	sb.WriteString("## Overall Metrics\n\n")
	fmt.Fprintf(&sb, "- **Context Precision:** %.1f%%\n", result.AvgPrecision*100)
	fmt.Fprintf(&sb, "- **Context Recall:** %.1f%%\n", result.AvgRecall*100)
	fmt.Fprintf(&sb, "- **Context F1:** %.1f%%\n\n", result.AvgF1*100)

	if result.MultiBudget != nil {
		sb.WriteString("## Multi-Budget Evaluation\n\n")
		sb.WriteString("| Budget | Precision | Recall | F1 |\n")
		sb.WriteString("|--------|-----------|--------|----|\n")
		for _, b := range []BudgetMetrics{result.MultiBudget.Budget250, result.MultiBudget.Budget500, result.MultiBudget.Budget1000} {
			fmt.Fprintf(&sb, "| %d | %.1f%% | %.1f%% | %.1f%% |\n",
				b.Budget, b.AvgPrecision*100, b.AvgRecall*100, b.AvgF1*100)
		}
		sb.WriteString("\n")
	}

	if len(result.PerLanguage) > 0 {
		sb.WriteString("## Per-Language Breakdown\n\n")
		sb.WriteString("| Language | Tasks | Precision | Recall | F1 |\n")
		sb.WriteString("|----------|-------|-----------|--------|----|\n")
		for _, l := range result.PerLanguage {
			fmt.Fprintf(&sb, "| %s | %d | %.1f%% | %.1f%% | %.1f%% |\n",
				l.Language, l.Tasks,
				l.AvgPrecision*100,
				l.AvgRecall*100,
				l.AvgF1*100,
			)
		}
	}

	if len(result.TaskResults) > 0 {
		sb.WriteString("\n## Per-Task Results\n\n")
		sb.WriteString("| Task | Repo | P | R | F1 | Gold | Hits | Retrieved | Tools |\n")
		sb.WriteString("|------|------|---|---|----|------|------|-----------|-------|\n")
		for _, raw := range result.TaskResults {
			// TaskResults are stored as interface{} — could be a typed struct or
			// map[string]interface{} after JSON roundtrip.
			var (
				instanceID                   string
				repo                         string
				prec, rec, f1                float64
				gold, hits, retrieved, tools int
			)
			switch v := raw.(type) {
			case map[string]interface{}:
				instanceID, _ = v["instance_id"].(string)
				repo, _ = v["repo"].(string)
				prec, _ = v["precision"].(float64)
				rec, _ = v["recall"].(float64)
				f1, _ = v["f1"].(float64)
				gold = int(asFloat(v["gold_lines"]))
				hits = int(asFloat(v["hit_lines"]))
				retrieved = int(asFloat(v["total_retrieved_lines"]))
				tools = int(asFloat(v["tool_calls"]))
			default:
				continue
			}
			// Shorten instance ID: last 20 chars
			short := instanceID
			if len(short) > 20 {
				short = "…" + short[len(short)-20:]
			}
			fmt.Fprintf(&sb, "| %s | %s | %.1f%% | %.1f%% | %.1f%% | %d | %d | %d | %d |\n",
				short, repo,
				prec*100, rec*100, f1*100,
				gold, hits, retrieved, tools,
			)
		}
	}

	sb.WriteString("\n## Comparison Against Leaderboard\n\n")
	sb.WriteString("| System | Context F1 |\n")
	sb.WriteString("|--------|------------|\n")
	sb.WriteString("| Leaderboard avg (most entries) | <40% |\n")
	fmt.Fprintf(&sb, "| **Synapses** | **%.1f%%** |\n", result.AvgF1*100)

	return sb.String()
}

func asFloat(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

// ─── GraphBench ───────────────────────────────────────────────────────────────

// GraphBenchResult holds the full results of a GraphBench run.
type GraphBenchResult struct {
	Timestamp       string            `json:"timestamp"`
	Mode            string            `json:"mode,omitempty"` // "smoke" or "full" (default)
	TotalTests      int               `json:"total_tests"`
	ErrorCount      int               `json:"error_count"`
	Correctness     float64           `json:"correctness"`    // fraction of non-error tests with recall > 0
	Completeness    float64           `json:"completeness"`   // fraction of non-error tests with recall == 1.0
	LatencyP50Ms    int64             `json:"latency_p50_ms"` // query latency percentiles
	LatencyP95Ms    int64             `json:"latency_p95_ms"`
	LatencyP99Ms    int64             `json:"latency_p99_ms"`
	ErrorCategories map[string]int    `json:"error_categories,omitempty"` // category → count
	Summary         GraphBenchMetrics `json:"summary"`
	ByQueryType     []GraphBenchSlice `json:"by_query_type"`
	ByLanguage      []GraphBenchSlice `json:"by_language"`
	FailedQueries   []FailedQuery     `json:"failed_queries,omitempty"`
	RepoStatsData   []RepoStats       `json:"repo_stats,omitempty"`
	TestResults     []interface{}     `json:"tests"`
}

// FailedQuery records a test that produced an error or zero recall.
type FailedQuery struct {
	Repo      string `json:"repo"`
	Language  string `json:"language"`
	QueryType string `json:"query_type"`
	Query     string `json:"query"`
	Error     string `json:"error"`
}

// RepoStats records per-repo indexing metadata.
type RepoStats struct {
	Repo        string `json:"repo"`
	Language    string `json:"language"`
	IndexTimeMs int64  `json:"index_time_ms"`
	NodeCount   int    `json:"node_count"`
	EdgeCount   int    `json:"edge_count"`
}

// GraphBenchMetrics holds P/R/F1 scores.
type GraphBenchMetrics struct {
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
}

// GraphBenchSlice is a breakdown by one dimension (query_type or language).
type GraphBenchSlice struct {
	Label   string            `json:"label"`
	Tests   int               `json:"tests"`
	Metrics GraphBenchMetrics `json:"metrics"`
}

// WriteGraphBench writes JSON + Markdown results for a GraphBench run.
func (r *Reporter) WriteGraphBench(result *GraphBenchResult) error {
	ts := strings.ReplaceAll(result.Timestamp, ":", "-")
	jsonPath := filepath.Join(r.dir, fmt.Sprintf("graphbench_%s.json", ts))
	mdPath := filepath.Join(r.dir, fmt.Sprintf("graphbench_%s.md", ts))

	if err := writeJSON(jsonPath, result); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := os.WriteFile(mdPath, []byte(graphBenchMarkdown(result)), 0o644); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}
	fmt.Printf("Results written:\n  JSON: %s\n  Markdown: %s\n", jsonPath, mdPath)
	return nil
}

// PrintGraphBenchSummary prints a compact summary to stdout.
func (r *Reporter) PrintGraphBenchSummary(result *GraphBenchResult) {
	fmt.Printf("\n=== GraphBench Summary ===\n")
	fmt.Printf("Tests: %d | Errors: %d | Correctness: %.1f%% | Completeness: %.1f%%\n",
		result.TotalTests, result.ErrorCount,
		result.Correctness*100,
		result.Completeness*100,
	)
	fmt.Printf("Precision: %.1f%% | Recall: %.1f%% | F1: %.1f%%\n",
		result.Summary.Precision*100,
		result.Summary.Recall*100,
		result.Summary.F1*100,
	)
	fmt.Printf("Latency: P50=%dms P95=%dms P99=%dms\n",
		result.LatencyP50Ms, result.LatencyP95Ms, result.LatencyP99Ms)
	if len(result.ErrorCategories) > 0 {
		fmt.Printf("Error breakdown:")
		for cat, count := range result.ErrorCategories {
			fmt.Printf(" %s=%d", cat, count)
		}
		fmt.Println()
	}
	if len(result.ByQueryType) > 0 {
		fmt.Printf("\n%-25s  %6s  %10s  %8s  %6s\n", "Query Type", "Tests", "Precision", "Recall", "F1")
		fmt.Printf("%s\n", strings.Repeat("-", 60))
		for _, s := range result.ByQueryType {
			fmt.Printf("%-25s  %6d  %9.1f%%  %7.1f%%  %5.1f%%\n",
				s.Label, s.Tests,
				s.Metrics.Precision*100,
				s.Metrics.Recall*100,
				s.Metrics.F1*100,
			)
		}
	}
	if len(result.ByLanguage) > 0 {
		fmt.Printf("\n%-12s  %6s  %10s  %8s  %6s\n", "Language", "Tests", "Precision", "Recall", "F1")
		fmt.Printf("%s\n", strings.Repeat("-", 50))
		for _, s := range result.ByLanguage {
			fmt.Printf("%-12s  %6d  %9.1f%%  %7.1f%%  %5.1f%%\n",
				s.Label, s.Tests,
				s.Metrics.Precision*100,
				s.Metrics.Recall*100,
				s.Metrics.F1*100,
			)
		}
	}

	// Print top failures by language.
	if len(result.FailedQueries) > 0 {
		fmt.Printf("\n=== Failed Queries (%d) ===\n", len(result.FailedQueries))
		byLang := make(map[string]int)
		for _, fq := range result.FailedQueries {
			byLang[fq.Language]++
		}
		fmt.Printf("%-12s  %6s\n", "Language", "Fails")
		fmt.Printf("%s\n", strings.Repeat("-", 20))
		for lang, count := range byLang {
			fmt.Printf("%-12s  %6d\n", lang, count)
		}
		// Show first 10 failures.
		limit := 10
		if len(result.FailedQueries) < limit {
			limit = len(result.FailedQueries)
		}
		fmt.Printf("\nTop %d failures:\n", limit)
		for i := 0; i < limit; i++ {
			fq := result.FailedQueries[i]
			fmt.Printf("  [%s] %s(%s) — %s (%s)\n", fq.Language, fq.QueryType, fq.Query, fq.Error, fq.Repo)
		}
	}
}

func graphBenchMarkdown(result *GraphBenchResult) string {
	var sb strings.Builder
	sb.WriteString("# GraphBench Results (Graph Accuracy Benchmark)\n\n")
	fmt.Fprintf(&sb, "**Run timestamp:** %s  \n", result.Timestamp)
	fmt.Fprintf(&sb, "**Total tests:** %d | **Errors:** %d  \n\n", result.TotalTests, result.ErrorCount)

	sb.WriteString("## Overall Metrics\n\n")
	fmt.Fprintf(&sb, "- **Correctness:** %.1f%% (queries returning at least one correct answer)\n", result.Correctness*100)
	fmt.Fprintf(&sb, "- **Completeness:** %.1f%% (queries returning all correct answers)\n", result.Completeness*100)
	fmt.Fprintf(&sb, "- **Precision:** %.1f%%\n", result.Summary.Precision*100)
	fmt.Fprintf(&sb, "- **Recall:** %.1f%%\n", result.Summary.Recall*100)
	fmt.Fprintf(&sb, "- **F1:** %.1f%%\n", result.Summary.F1*100)
	fmt.Fprintf(&sb, "- **Latency:** P50=%dms, P95=%dms, P99=%dms\n\n", result.LatencyP50Ms, result.LatencyP95Ms, result.LatencyP99Ms)

	if len(result.ErrorCategories) > 0 {
		sb.WriteString("## Error Breakdown\n\n")
		sb.WriteString("| Category | Count |\n|----------|-------|\n")
		for cat, count := range result.ErrorCategories {
			fmt.Fprintf(&sb, "| %s | %d |\n", cat, count)
		}
		sb.WriteString("\n")
	}

	if len(result.RepoStatsData) > 0 {
		sb.WriteString("## Repo Index Stats\n\n")
		sb.WriteString("| Repo | Language | Index Time | Nodes | Edges |\n")
		sb.WriteString("|------|----------|------------|-------|-------|\n")
		for _, rs := range result.RepoStatsData {
			fmt.Fprintf(&sb, "| %s | %s | %dms | %d | %d |\n",
				rs.Repo, rs.Language, rs.IndexTimeMs, rs.NodeCount, rs.EdgeCount)
		}
		sb.WriteString("\n")
	}

	if len(result.ByQueryType) > 0 {
		sb.WriteString("## By Query Type\n\n")
		sb.WriteString("| Query Type | Tests | Precision | Recall | F1 |\n")
		sb.WriteString("|------------|-------|-----------|--------|----|\n")
		for _, s := range result.ByQueryType {
			fmt.Fprintf(&sb, "| %s | %d | %.1f%% | %.1f%% | %.1f%% |\n",
				s.Label, s.Tests,
				s.Metrics.Precision*100, s.Metrics.Recall*100, s.Metrics.F1*100)
		}
	}

	if len(result.ByLanguage) > 0 {
		sb.WriteString("\n## By Language\n\n")
		sb.WriteString("| Language | Tests | Precision | Recall | F1 |\n")
		sb.WriteString("|----------|-------|-----------|--------|----|\n")
		for _, s := range result.ByLanguage {
			fmt.Fprintf(&sb, "| %s | %d | %.1f%% | %.1f%% | %.1f%% |\n",
				s.Label, s.Tests,
				s.Metrics.Precision*100, s.Metrics.Recall*100, s.Metrics.F1*100)
		}
	}

	if len(result.FailedQueries) > 0 {
		sb.WriteString("\n## Failed Queries\n\n")
		sb.WriteString("| Language | Query Type | Query | Repo | Error |\n")
		sb.WriteString("|----------|------------|-------|------|-------|\n")
		for _, fq := range result.FailedQueries {
			fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s |\n",
				fq.Language, fq.QueryType, fq.Query, fq.Repo, fq.Error)
		}
	}

	return sb.String()
}

// ─── NLBench ──────────────────────────────────────────────────────────────────

// NLBenchResult holds the full results of an NLBench run.
// Reuses GraphBenchMetrics and GraphBenchSlice for consistency.
type NLBenchResult struct {
	Timestamp   string            `json:"timestamp"`
	TotalTests  int               `json:"total_tests"`
	ErrorCount  int               `json:"error_count"`
	Summary     GraphBenchMetrics `json:"summary"`
	ByQueryType []GraphBenchSlice `json:"by_query_type"`
	ByLanguage  []GraphBenchSlice `json:"by_language"`
	TestResults []interface{}     `json:"tests"`
}

// WriteNLBench writes JSON + Markdown results for an NLBench run.
func (r *Reporter) WriteNLBench(result *NLBenchResult) error {
	ts := strings.ReplaceAll(result.Timestamp, ":", "-")
	jsonPath := filepath.Join(r.dir, fmt.Sprintf("nlbench_%s.json", ts))
	mdPath := filepath.Join(r.dir, fmt.Sprintf("nlbench_%s.md", ts))

	if err := writeJSON(jsonPath, result); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := os.WriteFile(mdPath, []byte(nlBenchMarkdown(result)), 0o644); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}
	fmt.Printf("Results written:\n  JSON: %s\n  Markdown: %s\n", jsonPath, mdPath)
	return nil
}

// PrintNLBenchSummary prints a compact summary to stdout.
func (r *Reporter) PrintNLBenchSummary(result *NLBenchResult) {
	fmt.Printf("\n=== NLBench Summary ===\n")
	fmt.Printf("Tests: %d | Precision: %.1f%% | Recall: %.1f%% | F1: %.1f%%\n",
		result.TotalTests,
		result.Summary.Precision*100,
		result.Summary.Recall*100,
		result.Summary.F1*100,
	)
	if len(result.ByQueryType) > 0 {
		fmt.Printf("\n%-25s  %6s  %10s  %8s  %6s\n", "Query Type", "Tests", "Precision", "Recall", "F1")
		fmt.Printf("%s\n", strings.Repeat("-", 60))
		for _, s := range result.ByQueryType {
			fmt.Printf("%-25s  %6d  %9.1f%%  %7.1f%%  %5.1f%%\n",
				s.Label, s.Tests,
				s.Metrics.Precision*100, s.Metrics.Recall*100, s.Metrics.F1*100)
		}
	}
	if len(result.ByLanguage) > 0 {
		fmt.Printf("\n%-12s  %6s  %10s  %8s  %6s\n", "Language", "Tests", "Precision", "Recall", "F1")
		fmt.Printf("%s\n", strings.Repeat("-", 50))
		for _, s := range result.ByLanguage {
			fmt.Printf("%-12s  %6d  %9.1f%%  %7.1f%%  %5.1f%%\n",
				s.Label, s.Tests,
				s.Metrics.Precision*100, s.Metrics.Recall*100, s.Metrics.F1*100)
		}
	}
}

func nlBenchMarkdown(result *NLBenchResult) string {
	var sb strings.Builder
	sb.WriteString("# NLBench Results (NL Parsing Benchmark)\n\n")
	fmt.Fprintf(&sb, "**Run timestamp:** %s  \n", result.Timestamp)
	fmt.Fprintf(&sb, "**Total tests:** %d  \n\n", result.TotalTests)

	sb.WriteString("## Overall Metrics\n\n")
	fmt.Fprintf(&sb, "- **Precision:** %.1f%%\n", result.Summary.Precision*100)
	fmt.Fprintf(&sb, "- **Recall:** %.1f%%\n", result.Summary.Recall*100)
	fmt.Fprintf(&sb, "- **F1:** %.1f%%\n\n", result.Summary.F1*100)

	if len(result.ByQueryType) > 0 {
		sb.WriteString("## By Query Type\n\n")
		sb.WriteString("| Query Type | Tests | Precision | Recall | F1 |\n")
		sb.WriteString("|------------|-------|-----------|--------|----|\n")
		for _, s := range result.ByQueryType {
			fmt.Fprintf(&sb, "| %s | %d | %.1f%% | %.1f%% | %.1f%% |\n",
				s.Label, s.Tests,
				s.Metrics.Precision*100, s.Metrics.Recall*100, s.Metrics.F1*100)
		}
	}

	if len(result.ByLanguage) > 0 {
		sb.WriteString("\n## By Language\n\n")
		sb.WriteString("| Language | Tests | Precision | Recall | F1 |\n")
		sb.WriteString("|----------|-------|-----------|--------|----|\n")
		for _, s := range result.ByLanguage {
			fmt.Fprintf(&sb, "| %s | %d | %.1f%% | %.1f%% | %.1f%% |\n",
				s.Label, s.Tests,
				s.Metrics.Precision*100, s.Metrics.Recall*100, s.Metrics.F1*100)
		}
	}

	return sb.String()
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func writeJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func pct(v float64) float64 {
	return v * 100
}

// Timestamp returns the current UTC time as a compact string for filenames.
func Timestamp() string {
	return time.Now().UTC().Format("2006-01-02T15-04-05Z")
}

// ── CompactionBench ─────────────────────────────────────────────────────────

// CompactionBenchReport holds aggregated CompactionBench results.
type CompactionBenchReport struct {
	Timestamp      string         `json:"timestamp"`
	Mode           string         `json:"mode"`
	Model          string         `json:"model"`
	TotalTasks     int            `json:"total_tasks"`
	PatchCount     int            `json:"patch_count"`
	PatchRate      float64        `json:"patch_rate"`
	AvgP2Turns     float64        `json:"avg_p2_turns"`
	AvgTurnsToEdit float64        `json:"avg_turns_to_edit"`
	AvgP2Search    float64        `json:"avg_p2_search_calls"`
	P2ToolUsage    map[string]int `json:"p2_tool_usage"`
	Tasks          []interface{}  `json:"tasks"`
}

// WriteCompactionBench writes JSON + Markdown results.
func (r *Reporter) WriteCompactionBench(result *CompactionBenchReport) error {
	ts := strings.ReplaceAll(result.Timestamp, ":", "-")
	jsonPath := filepath.Join(r.dir, fmt.Sprintf("compactionbench_%s_%s.json", result.Mode, ts))
	mdPath := filepath.Join(r.dir, fmt.Sprintf("compactionbench_%s_%s.md", result.Mode, ts))

	if err := writeJSON(jsonPath, result); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := os.WriteFile(mdPath, []byte(compactionBenchMarkdown(result)), 0o644); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}
	return nil
}

// PrintCompactionBenchSummary prints a console summary.
func (r *Reporter) PrintCompactionBenchSummary(result *CompactionBenchReport) {
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("  CompactionBench  |  mode: %s  |  model: %s\n", result.Mode, result.Model)
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("  Tasks:              %d\n", result.TotalTasks)
	fmt.Printf("  Patches:            %d (%.1f%%)\n", result.PatchCount, result.PatchRate)
	fmt.Printf("  Avg P2 turns:       %.1f\n", result.AvgP2Turns)
	fmt.Printf("  Avg turns to edit:  %.1f\n", result.AvgTurnsToEdit)
	fmt.Printf("  Avg P2 search:      %.1f\n", result.AvgP2Search)
	fmt.Println("───────────────────────────────────────────────────────")
	if len(result.P2ToolUsage) > 0 {
		var builtIn, mcp []string
		for name := range result.P2ToolUsage {
			if strings.HasPrefix(name, "mcp__") {
				mcp = append(mcp, name)
			} else {
				builtIn = append(builtIn, name)
			}
		}
		if len(builtIn) > 0 {
			fmt.Println("  Phase 2 built-in tools:")
			for _, name := range builtIn {
				fmt.Printf("    %-30s %d\n", name, result.P2ToolUsage[name])
			}
		}
		if len(mcp) > 0 {
			fmt.Println("  Phase 2 Synapses MCP tools:")
			for _, name := range mcp {
				fmt.Printf("    %-30s %d\n", name, result.P2ToolUsage[name])
			}
		}
	}
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println()
}

func compactionBenchMarkdown(result *CompactionBenchReport) string {
	var sb strings.Builder
	sb.WriteString("# CompactionBench Results\n\n")
	fmt.Fprintf(&sb, "- **Mode:** %s\n", result.Mode)
	fmt.Fprintf(&sb, "- **Model:** %s\n", result.Model)
	fmt.Fprintf(&sb, "- **Timestamp:** %s\n\n", result.Timestamp)
	sb.WriteString("## Summary\n\n")
	sb.WriteString("| Metric | Value |\n|--------|-------|\n")
	fmt.Fprintf(&sb, "| Tasks | %d |\n", result.TotalTasks)
	fmt.Fprintf(&sb, "| Patches generated | %d (%.1f%%) |\n", result.PatchCount, result.PatchRate)
	fmt.Fprintf(&sb, "| Avg P2 turns | %.1f |\n", result.AvgP2Turns)
	fmt.Fprintf(&sb, "| Avg turns to first edit | %.1f |\n", result.AvgTurnsToEdit)
	fmt.Fprintf(&sb, "| Avg P2 search calls | %.1f |\n", result.AvgP2Search)
	if len(result.P2ToolUsage) > 0 {
		sb.WriteString("\n## Phase 2 Tool Usage\n\n")
		sb.WriteString("| Tool | Calls |\n|------|-------|\n")
		for name, count := range result.P2ToolUsage {
			fmt.Fprintf(&sb, "| %s | %d |\n", name, count)
		}
	}
	sb.WriteString("\n---\n*Generated by Synapses CompactionBench runner*\n")
	return sb.String()
}

// ─── DriftBench ─────────────────────────────────────────────────────────────

// DriftBenchResult holds the full results of a DriftBench run.
type DriftBenchResult struct {
	Timestamp         string                `json:"timestamp"`
	ReposRun          int                   `json:"repos_run"`
	AvgFidelity       float64               `json:"avg_fidelity"`
	AvgRenameSurvival float64               `json:"avg_rename_survival"`
	AvgDeletionClean  float64               `json:"avg_deletion_clean"`
	AvgSpeedRatio     float64               `json:"avg_speed_ratio"`
	Repos             []DriftBenchRepoResult `json:"repos"`
}

// DriftBenchRepoResult holds per-repo metrics.
type DriftBenchRepoResult struct {
	Repo                string                        `json:"repo"`
	Language            string                        `json:"language"`
	TotalCommits        int                           `json:"total_commits"`
	TotalQueries        int                           `json:"total_queries"`
	IncrementalFidelity float64                       `json:"incremental_fidelity"`
	EdgeLossRate        float64                       `json:"edge_loss_rate"`
	RenameSurvival      float64                       `json:"rename_survival"`
	DeletionClean       float64                       `json:"deletion_cleanliness"`
	SpeedRatio          float64                       `json:"speed_ratio"`
	DriftCurve          []float64                     `json:"drift_curve"`
	PerCategory         map[string]DriftCategoryMetrics `json:"per_category"`
	Error               string                        `json:"error,omitempty"`
}

// DriftCategoryMetrics holds metrics for one commit category.
type DriftCategoryMetrics struct {
	Commits  int     `json:"commits"`
	Queries  int     `json:"queries"`
	Fidelity float64 `json:"fidelity"`
}

// WriteDriftBench writes JSON + Markdown results for a DriftBench run.
func (r *Reporter) WriteDriftBench(result *DriftBenchResult) error {
	ts := strings.ReplaceAll(result.Timestamp, ":", "-")
	jsonPath := filepath.Join(r.dir, fmt.Sprintf("driftbench_%s.json", ts))
	mdPath := filepath.Join(r.dir, fmt.Sprintf("driftbench_%s.md", ts))

	if err := writeJSON(jsonPath, result); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := os.WriteFile(mdPath, []byte(driftBenchMarkdown(result)), 0o644); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}
	fmt.Printf("Results written:\n  JSON: %s\n  Markdown: %s\n", jsonPath, mdPath)
	return nil
}

// PrintDriftBenchSummary prints a compact summary.
func (r *Reporter) PrintDriftBenchSummary(result *DriftBenchResult) {
	fmt.Printf("\n=== DriftBench Summary ===\n")
	fmt.Printf("%-30s  %-8s  %8s  %8s  %8s  %8s\n",
		"Repo", "Lang", "Fidelity", "Rename", "Delete", "Speed")
	fmt.Printf("%s\n", strings.Repeat("-", 85))
	for _, repo := range result.Repos {
		if repo.Error != "" {
			fmt.Printf("%-30s  %-8s  ERROR: %s\n", repo.Repo, repo.Language, repo.Error)
			continue
		}
		fmt.Printf("%-30s  %-8s  %7.1f%%  %7.1f%%  %7.1f%%  %7.2fx\n",
			repo.Repo, repo.Language,
			repo.IncrementalFidelity*100,
			repo.RenameSurvival*100,
			repo.DeletionClean*100,
			repo.SpeedRatio)
	}
	fmt.Printf("%s\n", strings.Repeat("-", 85))
	fmt.Printf("%-30s  %-8s  %7.1f%%  %7.1f%%  %7.1f%%  %7.2fx\n",
		"AVERAGE", "",
		result.AvgFidelity*100,
		result.AvgRenameSurvival*100,
		result.AvgDeletionClean*100,
		result.AvgSpeedRatio)
}

func driftBenchMarkdown(result *DriftBenchResult) string {
	var sb strings.Builder
	sb.WriteString("# DriftBench Results — Incremental Correctness\n\n")
	fmt.Fprintf(&sb, "**Run timestamp:** %s  \n", result.Timestamp)
	fmt.Fprintf(&sb, "**Repos evaluated:** %d  \n\n", result.ReposRun)

	sb.WriteString("## Summary\n\n")
	fmt.Fprintf(&sb, "- **Avg Incremental Fidelity:** %.1f%%\n", result.AvgFidelity*100)
	fmt.Fprintf(&sb, "- **Avg Rename Survival:** %.1f%%\n", result.AvgRenameSurvival*100)
	fmt.Fprintf(&sb, "- **Avg Deletion Cleanliness:** %.1f%%\n", result.AvgDeletionClean*100)
	fmt.Fprintf(&sb, "- **Avg Speed Ratio:** %.2fx (incremental/clean)\n\n", result.AvgSpeedRatio)

	sb.WriteString("## Per-Repo Results\n\n")
	sb.WriteString("| Repo | Language | Fidelity | Rename | Delete | Speed |\n")
	sb.WriteString("|------|----------|----------|--------|--------|-------|\n")
	for _, repo := range result.Repos {
		if repo.Error != "" {
			fmt.Fprintf(&sb, "| %s | %s | ERROR | - | - | - |\n", repo.Repo, repo.Language)
			continue
		}
		fmt.Fprintf(&sb, "| %s | %s | %.1f%% | %.1f%% | %.1f%% | %.2fx |\n",
			repo.Repo, repo.Language,
			repo.IncrementalFidelity*100,
			repo.RenameSurvival*100,
			repo.DeletionClean*100,
			repo.SpeedRatio)
	}

	// Per-category breakdown from first non-error repo.
	for _, repo := range result.Repos {
		if repo.Error != "" || len(repo.PerCategory) == 0 {
			continue
		}
		sb.WriteString("\n## Per-Category Breakdown\n\n")
		sb.WriteString("| Category | Commits | Queries | Fidelity |\n")
		sb.WriteString("|----------|---------|---------|----------|\n")
		for cat, cm := range repo.PerCategory {
			fmt.Fprintf(&sb, "| %s | %d | %d | %.1f%% |\n",
				cat, cm.Commits, cm.Queries, cm.Fidelity*100)
		}
		break
	}

	sb.WriteString("\n---\n*Generated by Synapses DriftBench*\n")
	return sb.String()
}

// ─── RecallBench ────────────────────────────────────────────────────────────

// RecallBenchResult holds the full results of a RecallBench run.
type RecallBenchResult struct {
	Timestamp              string                            `json:"timestamp"`
	PairsRun               int                               `json:"pairs_run"`
	AvgColdF1              float64                           `json:"avg_cold_f1"`
	AvgWarmF1              float64                           `json:"avg_warm_f1"`
	AvgRecallLift          float64                           `json:"avg_recall_lift"`
	AvgCrossProjectHitRate float64                           `json:"avg_cross_project_hit_rate"`
	AvgDriftAccuracy       float64                           `json:"avg_drift_accuracy"`
	PerRelationship        map[string]RecallRelationshipMetrics `json:"per_relationship"`
	Pairs                  []RecallBenchPairResult           `json:"pairs"`
}

// RecallBenchPairResult holds per-pair metrics.
type RecallBenchPairResult struct {
	PairID            string  `json:"pair_id"`
	Relationship      string  `json:"relationship"`
	ColdF1            float64 `json:"cold_f1"`
	WarmF1            float64 `json:"warm_f1"`
	RecallLift        float64 `json:"recall_lift"`
	CrossProjectHits  int     `json:"cross_project_hits"`
	CrossProjectPrec  float64 `json:"cross_project_precision"`
	DriftDetected     int     `json:"drift_detected"`
	DriftAccuracy     float64 `json:"drift_accuracy"`
	Error             string  `json:"error,omitempty"`
}

// RecallRelationshipMetrics holds metrics per relationship type.
type RecallRelationshipMetrics struct {
	Pairs     int     `json:"pairs"`
	AvgLift   float64 `json:"avg_lift"`
	AvgHitRate float64 `json:"avg_hit_rate"`
}

// WriteRecallBench writes JSON + Markdown results for a RecallBench run.
func (r *Reporter) WriteRecallBench(result *RecallBenchResult) error {
	ts := strings.ReplaceAll(result.Timestamp, ":", "-")
	jsonPath := filepath.Join(r.dir, fmt.Sprintf("recallbench_%s.json", ts))
	mdPath := filepath.Join(r.dir, fmt.Sprintf("recallbench_%s.md", ts))

	if err := writeJSON(jsonPath, result); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := os.WriteFile(mdPath, []byte(recallBenchMarkdown(result)), 0o644); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}
	fmt.Printf("Results written:\n  JSON: %s\n  Markdown: %s\n", jsonPath, mdPath)
	return nil
}

// PrintRecallBenchSummary prints a compact summary.
func (r *Reporter) PrintRecallBenchSummary(result *RecallBenchResult) {
	fmt.Printf("\n=== RecallBench Summary ===\n")
	fmt.Printf("%-20s  %-18s  %8s  %8s  %8s  %8s\n",
		"Pair", "Relationship", "Cold F1", "Warm F1", "Lift", "XProj%")
	fmt.Printf("%s\n", strings.Repeat("-", 80))
	for _, p := range result.Pairs {
		if p.Error != "" {
			fmt.Printf("%-20s  %-18s  ERROR: %s\n", p.PairID, p.Relationship, p.Error)
			continue
		}
		fmt.Printf("%-20s  %-18s  %7.1f%%  %7.1f%%  %+6.1f%%  %7.1f%%\n",
			p.PairID, p.Relationship,
			p.ColdF1*100, p.WarmF1*100,
			p.RecallLift*100, p.CrossProjectPrec*100)
	}
	fmt.Printf("%s\n", strings.Repeat("-", 80))
	fmt.Printf("%-20s  %-18s  %7.1f%%  %7.1f%%  %+6.1f%%\n",
		"AVERAGE", "",
		result.AvgColdF1*100, result.AvgWarmF1*100, result.AvgRecallLift*100)
}

func recallBenchMarkdown(result *RecallBenchResult) string {
	var sb strings.Builder
	sb.WriteString("# RecallBench Results — Cross-Project Memory\n\n")
	fmt.Fprintf(&sb, "**Run timestamp:** %s  \n", result.Timestamp)
	fmt.Fprintf(&sb, "**Pairs evaluated:** %d  \n\n", result.PairsRun)

	sb.WriteString("## Summary\n\n")
	fmt.Fprintf(&sb, "- **Avg Cold F1:** %.1f%%\n", result.AvgColdF1*100)
	fmt.Fprintf(&sb, "- **Avg Warm F1:** %.1f%%\n", result.AvgWarmF1*100)
	fmt.Fprintf(&sb, "- **Avg Recall Lift:** %+.1f%%\n", result.AvgRecallLift*100)
	fmt.Fprintf(&sb, "- **Avg Cross-Project Hit Rate:** %.1f%%\n\n", result.AvgCrossProjectHitRate*100)

	sb.WriteString("## Per-Pair Results\n\n")
	sb.WriteString("| Pair | Relationship | Cold F1 | Warm F1 | Lift | XProj Hits |\n")
	sb.WriteString("|------|-------------|---------|---------|------|------------|\n")
	for _, p := range result.Pairs {
		if p.Error != "" {
			fmt.Fprintf(&sb, "| %s | %s | ERROR | - | - | - |\n", p.PairID, p.Relationship)
			continue
		}
		fmt.Fprintf(&sb, "| %s | %s | %.1f%% | %.1f%% | %+.1f%% | %d |\n",
			p.PairID, p.Relationship,
			p.ColdF1*100, p.WarmF1*100, p.RecallLift*100, p.CrossProjectHits)
	}

	sb.WriteString("\n---\n*Generated by Synapses RecallBench*\n")
	return sb.String()
}

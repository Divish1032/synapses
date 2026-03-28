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

// ─── RepoBench-R ─────────────────────────────────────────────────────────────

// RepoBenchResult holds the full results of a RepoBench-R run.
type RepoBenchResult struct {
	Timestamp     string            `json:"timestamp"`
	RetrievalMode string            `json:"retrieval_mode"`
	Configs       []RepoBenchConfig `json:"configs"`
	Summary       RepoBenchSummary  `json:"summary"`
}

// RepoBenchConfig holds results for one config×difficulty combination.
type RepoBenchConfig struct {
	Config     string          `json:"config"`     // e.g. "python_cff"
	Difficulty string          `json:"difficulty"` // "easy" | "hard"
	Samples    int             `json:"samples"`
	AccAtK     map[int]float64 `json:"acc_at_k"` // k → accuracy
	AvgRank    float64         `json:"avg_gold_rank"`
}

// RepoBenchSummary aggregates across all configs.
type RepoBenchSummary struct {
	TotalSamples int             `json:"total_samples"`
	AccAtK       map[int]float64 `json:"acc_at_k"` // macro-average across configs
}

// WriteRepoBench writes JSON + Markdown results for a RepoBench-R run.
func (r *Reporter) WriteRepoBench(result *RepoBenchResult) error {
	ts := strings.ReplaceAll(result.Timestamp, ":", "-")

	jsonPath := filepath.Join(r.dir, fmt.Sprintf("repobench_%s_%s.json", result.RetrievalMode, ts))
	mdPath := filepath.Join(r.dir, fmt.Sprintf("repobench_%s_%s.md", result.RetrievalMode, ts))

	if err := writeJSON(jsonPath, result); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := os.WriteFile(mdPath, []byte(repoBenchMarkdown(result)), 0o644); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}

	fmt.Printf("Results written:\n  JSON: %s\n  Markdown: %s\n", jsonPath, mdPath)
	return nil
}

// PrintRepoBenchSummary prints a compact summary to stdout.
func (r *Reporter) PrintRepoBenchSummary(result *RepoBenchResult) {
	fmt.Printf("\n=== RepoBench-R Summary [%s] ===\n", result.RetrievalMode)
	fmt.Printf("%-20s  %-10s  %6s  %6s  %6s  %6s\n",
		"Config", "Difficulty", "Acc@1", "Acc@3", "Acc@5", "Acc@10")
	fmt.Printf("%s\n", strings.Repeat("-", 64))
	for _, cfg := range result.Configs {
		fmt.Printf("%-20s  %-10s  %6.1f  %6.1f  %6.1f  %6.1f\n",
			cfg.Config, cfg.Difficulty,
			pct(cfg.AccAtK[1]),
			pct(cfg.AccAtK[3]),
			pct(cfg.AccAtK[5]),
			pct(cfg.AccAtK[10]),
		)
	}
	fmt.Printf("%s\n", strings.Repeat("-", 64))
	s := result.Summary
	fmt.Printf("%-20s  %-10s  %6.1f  %6.1f  %6.1f  %6.1f  (macro avg, n=%d)\n",
		"OVERALL", "",
		pct(s.AccAtK[1]),
		pct(s.AccAtK[3]),
		pct(s.AccAtK[5]),
		pct(s.AccAtK[10]),
		s.TotalSamples,
	)
}

func repoBenchMarkdown(result *RepoBenchResult) string {
	var sb strings.Builder
	sb.WriteString("# RepoBench-R Results\n\n")
	fmt.Fprintf(&sb, "**Retrieval mode:** `%s`  \n", result.RetrievalMode)
	fmt.Fprintf(&sb, "**Run timestamp:** %s  \n\n", result.Timestamp)

	sb.WriteString("## Per-Config Results\n\n")
	sb.WriteString("| Config | Difficulty | Samples | Acc@1 | Acc@3 | Acc@5 | Acc@10 | Avg Gold Rank |\n")
	sb.WriteString("|--------|-----------|---------|-------|-------|-------|--------|---------------|\n")
	for _, cfg := range result.Configs {
		fmt.Fprintf(&sb, "| %s | %s | %d | %.1f%% | %.1f%% | %.1f%% | %.1f%% | %.1f |\n",
			cfg.Config, cfg.Difficulty, cfg.Samples,
			pct(cfg.AccAtK[1]),
			pct(cfg.AccAtK[3]),
			pct(cfg.AccAtK[5]),
			pct(cfg.AccAtK[10]),
			cfg.AvgRank,
		)
	}

	sb.WriteString("\n## Summary (Macro Average)\n\n")
	s := result.Summary
	fmt.Fprintf(&sb, "- **Total samples:** %d\n", s.TotalSamples)
	fmt.Fprintf(&sb, "- **Acc@1:** %.1f%%\n", pct(s.AccAtK[1]))
	fmt.Fprintf(&sb, "- **Acc@3:** %.1f%%\n", pct(s.AccAtK[3]))
	fmt.Fprintf(&sb, "- **Acc@5:** %.1f%%\n", pct(s.AccAtK[5]))
	fmt.Fprintf(&sb, "- **Acc@10:** %.1f%%\n", pct(s.AccAtK[10]))

	sb.WriteString("\n## Comparison Against Published Baselines\n\n")
	sb.WriteString("| System | Acc@5 (hard) |\n")
	sb.WriteString("|--------|-------------|\n")
	sb.WriteString("| BM25 baseline (RepoBench paper) | ~60% |\n")
	sb.WriteString("| Dense retrieval, ada-002 | ~65% |\n")
	fmt.Fprintf(&sb, "| **Synapses %s** | **%.1f%%** |\n", result.RetrievalMode, pct(s.AccAtK[5]))

	return sb.String()
}

// ─── ContextBench ─────────────────────────────────────────────────────────────

// ContextBenchResult holds the full results of a ContextBench run.
type ContextBenchResult struct {
	Timestamp    string                   `json:"timestamp"`
	TotalTasks   int                      `json:"total_tasks"`
	AvgPrecision float64                  `json:"avg_precision"`
	AvgRecall    float64                  `json:"avg_recall"`
	AvgF1        float64                  `json:"avg_f1"`
	PerLanguage  []ContextBenchLangResult `json:"per_language"`
	TaskResults  []interface{}            `json:"tasks"` // []ContextBenchTaskResult from benchmarks pkg
}

// ContextBenchLangResult holds per-language metrics.
type ContextBenchLangResult struct {
	Language     string  `json:"language"`
	Tasks        int     `json:"tasks"`
	AvgPrecision float64 `json:"avg_precision"`
	AvgRecall    float64 `json:"avg_recall"`
	AvgF1        float64 `json:"avg_f1"`
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
	fmt.Printf("Tasks: %d | Precision: %.1f%% | Recall: %.1f%% | F1: %.1f%%\n",
		result.TotalTasks,
		result.AvgPrecision*100,
		result.AvgRecall*100,
		result.AvgF1*100,
	)
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
	Timestamp   string            `json:"timestamp"`
	TotalTests  int               `json:"total_tests"`
	ErrorCount  int               `json:"error_count"`
	Summary     GraphBenchMetrics `json:"summary"`
	ByQueryType []GraphBenchSlice `json:"by_query_type"`
	ByLanguage  []GraphBenchSlice `json:"by_language"`
	TestResults []interface{}     `json:"tests"`
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
}

func graphBenchMarkdown(result *GraphBenchResult) string {
	var sb strings.Builder
	sb.WriteString("# GraphBench Results (Graph Accuracy Benchmark)\n\n")
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

// ─── SWE-bench ──────────────────────────────────────────────────────────────

// SWEBenchResult holds the full results of a SWE-bench run.
type SWEBenchResult struct {
	Timestamp       string         `json:"timestamp"`
	Mode            string         `json:"mode"`
	Model           string         `json:"model"`
	TotalTasks      int            `json:"total_tasks"`
	PassCount       int            `json:"pass_count"`
	PatchCount      int            `json:"patch_count"`
	PassRate        float64        `json:"pass_rate"`
	PatchRate       float64        `json:"patch_rate"`
	AvgTurns        float64        `json:"avg_turns"`
	AvgTokens       int            `json:"avg_tokens"`
	ToolContribRate float64        `json:"tool_contrib_rate"`
	ToolUsage       map[string]int `json:"tool_usage"`
	Tasks           []interface{}  `json:"tasks"`
}

// WriteSWEBench writes JSON + Markdown results for a SWE-bench run.
func (r *Reporter) WriteSWEBench(result *SWEBenchResult) error {
	ts := strings.ReplaceAll(result.Timestamp, ":", "-")
	jsonPath := filepath.Join(r.dir, fmt.Sprintf("swebench_%s_%s.json", result.Mode, ts))
	mdPath := filepath.Join(r.dir, fmt.Sprintf("swebench_%s_%s.md", result.Mode, ts))

	if err := writeJSON(jsonPath, result); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := os.WriteFile(mdPath, []byte(sweBenchMarkdown(result)), 0o644); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}
	return nil
}

// PrintSWEBenchSummary prints a console summary table.
func (r *Reporter) PrintSWEBenchSummary(result *SWEBenchResult) {
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("  SWE-bench  |  mode: %s  |  model: %s\n", result.Mode, result.Model)
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("  Tasks:          %d\n", result.TotalTasks)
	fmt.Printf("  Patches:        %d (%.1f%%)\n", result.PatchCount, result.PatchRate)
	fmt.Printf("  Pass@1:         %d (%.1f%%)\n", result.PassCount, result.PassRate)
	fmt.Printf("  Avg turns:      %.1f\n", result.AvgTurns)
	fmt.Printf("  Avg tokens:     %d\n", result.AvgTokens)
	if result.Mode == "synapses" {
		fmt.Printf("  Tool contrib:   %.1f%%\n", result.ToolContribRate)
	}
	fmt.Println("───────────────────────────────────────────────────────")
	if len(result.ToolUsage) > 0 {
		fmt.Println("  Tool usage:")
		for name, count := range result.ToolUsage {
			fmt.Printf("    %-30s %d\n", name, count)
		}
	}
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println()
}

// ─── FeatureBench ───────────────────────────────────────────────────────────

// FeatureBenchReport holds aggregated FeatureBench results.
// Defined here to avoid circular import with benchmarks package.
type FeatureBenchReport struct {
	Timestamp  string         `json:"timestamp"`
	Mode       string         `json:"mode"`
	Model      string         `json:"model"`
	TotalTasks int            `json:"total_tasks"`
	PatchCount int            `json:"patch_count"`
	PatchRate  float64        `json:"patch_rate"`
	AvgTurns   float64        `json:"avg_turns"`
	ToolUsage  map[string]int `json:"tool_usage"`
	Tasks      []interface{}  `json:"tasks"`
}

// WriteFeatureBench writes JSON + Markdown results.
func (r *Reporter) WriteFeatureBench(result *FeatureBenchReport) error {
	ts := strings.ReplaceAll(result.Timestamp, ":", "-")
	jsonPath := filepath.Join(r.dir, fmt.Sprintf("featurebench_%s_%s.json", result.Mode, ts))
	mdPath := filepath.Join(r.dir, fmt.Sprintf("featurebench_%s_%s.md", result.Mode, ts))

	if err := writeJSON(jsonPath, result); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	if err := os.WriteFile(mdPath, []byte(featureBenchMarkdown(result)), 0o644); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}
	return nil
}

// PrintFeatureBenchSummary prints a console summary.
func (r *Reporter) PrintFeatureBenchSummary(result *FeatureBenchReport) {
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("  FeatureBench  |  mode: %s  |  model: %s\n", result.Mode, result.Model)
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("  Tasks:          %d\n", result.TotalTasks)
	fmt.Printf("  Patches:        %d (%.1f%%)\n", result.PatchCount, result.PatchRate)
	fmt.Printf("  Avg turns:      %.1f\n", result.AvgTurns)
	fmt.Println("───────────────────────────────────────────────────────")
	if len(result.ToolUsage) > 0 {
		// Separate MCP tools from built-in tools.
		var builtIn, mcp []string
		for name := range result.ToolUsage {
			if strings.HasPrefix(name, "mcp__") {
				mcp = append(mcp, name)
			} else {
				builtIn = append(builtIn, name)
			}
		}
		if len(builtIn) > 0 {
			fmt.Println("  Built-in tools:")
			for _, name := range builtIn {
				fmt.Printf("    %-30s %d\n", name, result.ToolUsage[name])
			}
		}
		if len(mcp) > 0 {
			fmt.Println("  Synapses MCP tools:")
			for _, name := range mcp {
				fmt.Printf("    %-30s %d\n", name, result.ToolUsage[name])
			}
		} else if result.Mode == "synapses" {
			fmt.Println("  Synapses MCP tools:          (none used)")
		}
	}
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println()
}

func featureBenchMarkdown(result *FeatureBenchReport) string {
	var sb strings.Builder
	sb.WriteString("# FeatureBench Results\n\n")
	fmt.Fprintf(&sb, "- **Mode:** %s\n", result.Mode)
	fmt.Fprintf(&sb, "- **Model:** %s\n", result.Model)
	fmt.Fprintf(&sb, "- **Timestamp:** %s\n\n", result.Timestamp)
	sb.WriteString("## Summary\n\n")
	sb.WriteString("| Metric | Value |\n|--------|-------|\n")
	fmt.Fprintf(&sb, "| Tasks | %d |\n", result.TotalTasks)
	fmt.Fprintf(&sb, "| Patches generated | %d (%.1f%%) |\n", result.PatchCount, result.PatchRate)
	fmt.Fprintf(&sb, "| Avg turns | %.1f |\n", result.AvgTurns)
	if len(result.ToolUsage) > 0 {
		sb.WriteString("\n## Tool Usage\n\n")
		sb.WriteString("| Tool | Calls |\n|------|-------|\n")
		for name, count := range result.ToolUsage {
			fmt.Fprintf(&sb, "| %s | %d |\n", name, count)
		}
	}
	sb.WriteString("\n---\n*Generated by Synapses FeatureBench runner*\n")
	return sb.String()
}

func sweBenchMarkdown(result *SWEBenchResult) string {
	var sb strings.Builder
	sb.WriteString("# SWE-bench Results\n\n")
	fmt.Fprintf(&sb, "- **Mode:** %s\n", result.Mode)
	fmt.Fprintf(&sb, "- **Model:** %s\n", result.Model)
	fmt.Fprintf(&sb, "- **Timestamp:** %s\n\n", result.Timestamp)

	sb.WriteString("## Summary\n\n")
	sb.WriteString("| Metric | Value |\n|--------|-------|\n")
	fmt.Fprintf(&sb, "| Tasks | %d |\n", result.TotalTasks)
	fmt.Fprintf(&sb, "| Patches generated | %d (%.1f%%) |\n", result.PatchCount, result.PatchRate)
	fmt.Fprintf(&sb, "| Pass@1 | %d (%.1f%%) |\n", result.PassCount, result.PassRate)
	fmt.Fprintf(&sb, "| Avg turns | %.1f |\n", result.AvgTurns)
	fmt.Fprintf(&sb, "| Avg tokens | %d |\n", result.AvgTokens)
	if result.Mode == "synapses" {
		fmt.Fprintf(&sb, "| Tool contribution rate | %.1f%% |\n", result.ToolContribRate)
	}

	if len(result.ToolUsage) > 0 {
		sb.WriteString("\n## Tool Usage\n\n")
		sb.WriteString("| Tool | Calls |\n|------|-------|\n")
		for name, count := range result.ToolUsage {
			fmt.Fprintf(&sb, "| %s | %d |\n", name, count)
		}
	}

	sb.WriteString("\n---\n*Generated by Synapses SWE-bench runner*\n")
	return sb.String()
}

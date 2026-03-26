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
	Timestamp     string              `json:"timestamp"`
	RetrievalMode string              `json:"retrieval_mode"`
	Configs       []RepoBenchConfig   `json:"configs"`
	Summary       RepoBenchSummary    `json:"summary"`
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
	sb.WriteString(fmt.Sprintf("**Retrieval mode:** `%s`  \n", result.RetrievalMode))
	sb.WriteString(fmt.Sprintf("**Run timestamp:** %s  \n\n", result.Timestamp))

	sb.WriteString("## Per-Config Results\n\n")
	sb.WriteString("| Config | Difficulty | Samples | Acc@1 | Acc@3 | Acc@5 | Acc@10 | Avg Gold Rank |\n")
	sb.WriteString("|--------|-----------|---------|-------|-------|-------|--------|---------------|\n")
	for _, cfg := range result.Configs {
		sb.WriteString(fmt.Sprintf("| %s | %s | %d | %.1f%% | %.1f%% | %.1f%% | %.1f%% | %.1f |\n",
			cfg.Config, cfg.Difficulty, cfg.Samples,
			pct(cfg.AccAtK[1]),
			pct(cfg.AccAtK[3]),
			pct(cfg.AccAtK[5]),
			pct(cfg.AccAtK[10]),
			cfg.AvgRank,
		))
	}

	sb.WriteString("\n## Summary (Macro Average)\n\n")
	s := result.Summary
	sb.WriteString(fmt.Sprintf("- **Total samples:** %d\n", s.TotalSamples))
	sb.WriteString(fmt.Sprintf("- **Acc@1:** %.1f%%\n", pct(s.AccAtK[1])))
	sb.WriteString(fmt.Sprintf("- **Acc@3:** %.1f%%\n", pct(s.AccAtK[3])))
	sb.WriteString(fmt.Sprintf("- **Acc@5:** %.1f%%\n", pct(s.AccAtK[5])))
	sb.WriteString(fmt.Sprintf("- **Acc@10:** %.1f%%\n", pct(s.AccAtK[10])))

	sb.WriteString("\n## Comparison Against Published Baselines\n\n")
	sb.WriteString("| System | Acc@5 (hard) |\n")
	sb.WriteString("|--------|-------------|\n")
	sb.WriteString("| BM25 baseline (RepoBench paper) | ~60% |\n")
	sb.WriteString("| Dense retrieval, ada-002 | ~65% |\n")
	sb.WriteString(fmt.Sprintf("| **Synapses %s** | **%.1f%%** |\n", result.RetrievalMode, pct(s.AccAtK[5])))

	return sb.String()
}

// ─── ContextBench ─────────────────────────────────────────────────────────────

// ContextBenchResult holds the full results of a ContextBench run.
type ContextBenchResult struct {
	Timestamp    string                    `json:"timestamp"`
	TotalTasks   int                       `json:"total_tasks"`
	AvgPrecision float64                   `json:"avg_precision"`
	AvgRecall    float64                   `json:"avg_recall"`
	AvgF1        float64                   `json:"avg_f1"`
	PerLanguage  []ContextBenchLangResult  `json:"per_language"`
	TaskResults  []interface{}             `json:"tasks"` // []ContextBenchTaskResult from benchmarks pkg
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
	sb.WriteString(fmt.Sprintf("**Run timestamp:** %s  \n", result.Timestamp))
	sb.WriteString(fmt.Sprintf("**Total tasks:** %d  \n\n", result.TotalTasks))

	sb.WriteString("## Overall Metrics\n\n")
	sb.WriteString(fmt.Sprintf("- **Context Precision:** %.1f%%\n", result.AvgPrecision*100))
	sb.WriteString(fmt.Sprintf("- **Context Recall:** %.1f%%\n", result.AvgRecall*100))
	sb.WriteString(fmt.Sprintf("- **Context F1:** %.1f%%\n\n", result.AvgF1*100))

	if len(result.PerLanguage) > 0 {
		sb.WriteString("## Per-Language Breakdown\n\n")
		sb.WriteString("| Language | Tasks | Precision | Recall | F1 |\n")
		sb.WriteString("|----------|-------|-----------|--------|----|\n")
		for _, l := range result.PerLanguage {
			sb.WriteString(fmt.Sprintf("| %s | %d | %.1f%% | %.1f%% | %.1f%% |\n",
				l.Language, l.Tasks,
				l.AvgPrecision*100,
				l.AvgRecall*100,
				l.AvgF1*100,
			))
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
				instanceID string
				repo       string
				prec, rec, f1 float64
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
			sb.WriteString(fmt.Sprintf("| %s | %s | %.1f%% | %.1f%% | %.1f%% | %d | %d | %d | %d |\n",
				short, repo,
				prec*100, rec*100, f1*100,
				gold, hits, retrieved, tools,
			))
		}
	}

	sb.WriteString("\n## Comparison Against Leaderboard\n\n")
	sb.WriteString("| System | Context F1 |\n")
	sb.WriteString("|--------|------------|\n")
	sb.WriteString("| Leaderboard avg (most entries) | <40% |\n")
	sb.WriteString(fmt.Sprintf("| **Synapses** | **%.1f%%** |\n", result.AvgF1*100))

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

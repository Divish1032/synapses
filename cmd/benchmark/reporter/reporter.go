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

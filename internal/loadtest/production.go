//go:build loadtest

package loadtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ProductionConfig controls a production-repo load test.
type ProductionConfig struct {
	// RepoURL is the git clone URL (e.g. "https://github.com/grafana/grafana").
	RepoURL string

	// CloneDir is where the repo is cloned. Reused if .git already exists.
	CloneDir string

	// ShallowClone uses --depth 1 (default true).
	ShallowClone bool

	// SkipExtended skips the extended measurements (GOGC sweep, per-language, etc.)
	SkipExtended bool

	// Base indexing config.
	Config
}

// ProductionReport aggregates indexing, retrieval, and extended results.
type ProductionReport struct {
	RepoURL       string           `json:"repo_url"`
	RepoLineCount int64            `json:"repo_line_count"`
	Indexing      *FullReport      `json:"indexing"`
	Retrieval     *RetrievalReport `json:"retrieval,omitempty"`
	Extended      *ExtendedReport  `json:"extended,omitempty"`
}

// RunProduction clones a large repo (if needed), indexes it, and benchmarks
// both indexing and retrieval performance.
func RunProduction(cfg ProductionConfig) (*ProductionReport, error) {
	cfg.Config.defaults()
	out := cfg.Config.Output
	if out == nil {
		out = os.Stdout
	}

	prodReport := &ProductionReport{RepoURL: cfg.RepoURL}

	// ── Clone ────────────────────────────────────────────────────────────

	if _, err := os.Stat(filepath.Join(cfg.CloneDir, ".git")); os.IsNotExist(err) {
		fmt.Fprintf(out, "Cloning %s into %s ...\n", cfg.RepoURL, cfg.CloneDir)
		args := []string{"clone"}
		if cfg.ShallowClone {
			args = append(args, "--depth", "1")
		}
		args = append(args, cfg.RepoURL, cfg.CloneDir)
		cmd := exec.Command("git", args...)
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git clone: %w", err)
		}
	} else {
		fmt.Fprintf(out, "Reusing existing clone at %s\n", cfg.CloneDir)
	}

	// ── Count lines (best-effort) ────────────────────────────────────────

	fmt.Fprintf(out, "Counting lines...\n")
	prodReport.RepoLineCount = countLines(cfg.CloneDir)
	fmt.Fprintf(out, "Estimated lines of code: %s\n", fmtCount(prodReport.RepoLineCount))

	// ── Indexing ─────────────────────────────────────────────────────────

	cfg.Config.RepoRoot = cfg.CloneDir
	cfg.Config.RetainState = true

	fmt.Fprintf(out, "\n%s\n", strings.Repeat("=", 80))
	fmt.Fprintf(out, "INDEXING: %s\n", cfg.RepoURL)
	fmt.Fprintf(out, "Go %s, GOMAXPROCS=%d, %s/%s\n",
		runtime.Version(), runtime.GOMAXPROCS(0), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(out, "%s\n", strings.Repeat("=", 80))

	indexStart := time.Now()
	report, err := Run(cfg.Config)
	indexTotal := time.Since(indexStart)
	if err != nil {
		return nil, fmt.Errorf("indexing: %w", err)
	}
	defer report.Close()
	prodReport.Indexing = report

	fmt.Fprintf(out, "\nTotal indexing wall time: %s\n", fmtDuration(indexTotal))
	if report.Graph != nil {
		fmt.Fprintf(out, "Graph: %d nodes, %d edges\n",
			report.Graph.NodeCount(), report.Graph.EdgeCount())
	}

	// ── Retrieval benchmarks ─────────────────────────────────────────────

	if report.Graph != nil {
		fmt.Fprintf(out, "\n%s\n", strings.Repeat("=", 80))
		fmt.Fprintf(out, "RETRIEVAL BENCHMARKS\n")
		fmt.Fprintf(out, "%s\n", strings.Repeat("=", 80))

		retReport, retErr := RunRetrieval(report.Graph, report.DBPath, out, RetrievalConfig{})
		if retErr != nil {
			fmt.Fprintf(out, "warning: retrieval benchmarks: %v\n", retErr)
		} else {
			prodReport.Retrieval = retReport
		}
	}

	// ── Extended measurements ────────────────────────────────────────────

	if !cfg.SkipExtended {
		fmt.Fprintf(out, "\n%s\n", strings.Repeat("=", 80))
		fmt.Fprintf(out, "EXTENDED ANALYSIS\n")
		fmt.Fprintf(out, "%s\n", strings.Repeat("=", 80))

		extReport, extErr := RunExtended(cfg.Config)
		if extErr != nil {
			fmt.Fprintf(out, "warning: extended analysis: %v\n", extErr)
		} else {
			prodReport.Extended = extReport
		}
	}

	// ── Summary ──────────────────────────────────────────────────────────

	fmt.Fprintf(out, "\n%s\n", strings.Repeat("=", 80))
	fmt.Fprintf(out, "PRODUCTION LOAD TEST SUMMARY\n")
	fmt.Fprintf(out, "%s\n", strings.Repeat("=", 80))
	fmt.Fprintf(out, "  Repo:           %s\n", cfg.RepoURL)
	fmt.Fprintf(out, "  Lines:          %s\n", fmtCount(prodReport.RepoLineCount))
	fmt.Fprintf(out, "  Total index:    %s\n", fmtDuration(indexTotal))
	if report.Graph != nil {
		fmt.Fprintf(out, "  Nodes:          %d\n", report.Graph.NodeCount())
		fmt.Fprintf(out, "  Edges:          %d\n", report.Graph.EdgeCount())
	}

	// Peak RSS from the last indexing stage.
	if len(report.Stages) > 0 {
		lastStage := report.Stages[len(report.Stages)-1]
		fmt.Fprintf(out, "  Peak RSS:       %s\n", fmtBytes(lastStage.PeakRSSAfter))
	}

	// Flag critical gaps.
	fmt.Fprintf(out, "\n--- Critical Gap Detection ---\n")
	gapFound := false
	for _, s := range report.Stages {
		if s.WallTime > 5*time.Minute {
			fmt.Fprintf(out, "  CRITICAL: Stage %q took %s (>5min)\n", s.Name, fmtDuration(s.WallTime))
			gapFound = true
		}
	}
	if len(report.Stages) > 0 {
		lastStage := report.Stages[len(report.Stages)-1]
		if lastStage.PeakRSSAfter > 2*1024*1024*1024 {
			fmt.Fprintf(out, "  CRITICAL: Peak RSS %s exceeds 2GB\n", fmtBytes(lastStage.PeakRSSAfter))
			gapFound = true
		}
	}
	if prodReport.Retrieval != nil {
		for _, ql := range prodReport.Retrieval.GetContext {
			if ql.P99 > 500*time.Millisecond {
				fmt.Fprintf(out, "  CRITICAL: %s p99=%s exceeds 500ms\n", ql.Label, fmtDuration(ql.P99))
				gapFound = true
			}
		}
		if prodReport.Retrieval.Search != nil && prodReport.Retrieval.Search.P99 > 200*time.Millisecond {
			fmt.Fprintf(out, "  CRITICAL: search p99=%s exceeds 200ms\n", fmtDuration(prodReport.Retrieval.Search.P99))
			gapFound = true
		}
	}
	if !gapFound {
		fmt.Fprintf(out, "  No critical gaps detected.\n")
	}

	return prodReport, nil
}

// countLines counts lines across common source file extensions.
func countLines(dir string) int64 {
	exts := []string{".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".java", ".rs", ".rb", ".c", ".h", ".cpp", ".hpp", ".cs", ".swift", ".kt"}
	extSet := make(map[string]bool, len(exts))
	for _, e := range exts {
		extSet[e] = true
	}

	var total int64
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		// Skip hidden dirs, vendor, node_modules.
		if info.IsDir() {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || base == "vendor" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !extSet[filepath.Ext(path)] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, b := range data {
			if b == '\n' {
				total++
			}
		}
		return nil
	})
	return total
}

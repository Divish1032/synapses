//go:build loadtest

package loadtest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadProfile_Small profiles the Synapses pipeline against the synapses
// repository itself (~1K files). This is the CI smoke test.
func TestLoadProfile_Small(t *testing.T) {
	root := findRepoRoot(t)

	var jsonBuf bytes.Buffer
	var benchBuf bytes.Buffer

	report, err := Run(Config{
		RepoRoot:               root,
		Size:                   "small",
		SkipEmbeddings:         true, // ONNX models may not be present in CI
		SkipIncrementalReindex: false,
		Output:                 os.Stdout,
		JSONOutput:             &jsonBuf,
		BenchstatOutput:        &benchBuf,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Sanity checks.
	if len(report.Stages) < 5 {
		t.Errorf("expected at least 5 stages, got %d", len(report.Stages))
	}

	for _, s := range report.Stages {
		if s.WallTime <= 0 {
			t.Errorf("stage %q: wall time should be > 0, got %v", s.Name, s.WallTime)
		}
		if s.HeapInusePeak < s.HeapInuseBefore {
			t.Errorf("stage %q: peak heap (%d) < before heap (%d)", s.Name, s.HeapInusePeak, s.HeapInuseBefore)
		}
	}

	// Verify JSON output is non-empty and parseable.
	if jsonBuf.Len() == 0 {
		t.Error("JSON output is empty")
	}

	// Verify benchstat output has the expected format.
	if benchBuf.Len() == 0 {
		t.Error("benchstat output is empty")
	}
	if !bytes.Contains(benchBuf.Bytes(), []byte("BenchmarkStage_")) {
		t.Error("benchstat output missing BenchmarkStage_ prefix")
	}

	t.Logf("JSON output: %d bytes", jsonBuf.Len())
	t.Logf("benchstat output:\n%s", benchBuf.String())
}

// TestLoadProfile_Medium profiles against a 10K-file repo.
// Set LOADTEST_REPO to an external path, or it defaults to /tmp/synthetic_10k.
func TestLoadProfile_Medium(t *testing.T) {
	syntheticRoot := os.Getenv("LOADTEST_REPO")
	if syntheticRoot == "" {
		syntheticRoot = "/tmp/synthetic_10k"
	}
	if _, err := os.Stat(syntheticRoot); os.IsNotExist(err) {
		t.Skipf("repo not found at %s; generate with: cd internal/loadtest/testdata && go run generate.go -files 10000 -out /tmp/synthetic_10k", syntheticRoot)
	}

	root, err := filepath.Abs(syntheticRoot)
	if err != nil {
		t.Fatal(err)
	}

	report, err := Run(Config{
		RepoRoot:       root,
		Size:           "medium",
		SkipEmbeddings: true,
		Output:         os.Stdout,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(report.Stages) < 5 {
		t.Errorf("expected at least 5 stages, got %d", len(report.Stages))
	}
}

// TestLoadProfile_Large profiles against a synthetic 50K+ file repo.
func TestLoadProfile_Large(t *testing.T) {
	syntheticRoot := os.Getenv("LOADTEST_REPO")
	if syntheticRoot == "" {
		syntheticRoot = "/tmp/synthetic_50k"
	}
	if _, err := os.Stat(syntheticRoot); os.IsNotExist(err) {
		t.Skipf("repo not found at %s; generate with: make loadtest/generate", syntheticRoot)
	}

	root, err := filepath.Abs(syntheticRoot)
	if err != nil {
		t.Fatal(err)
	}

	report, err := Run(Config{
		RepoRoot:       root,
		Size:           "large",
		SkipEmbeddings: true,
		Output:         os.Stdout,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(report.Stages) < 5 {
		t.Errorf("expected at least 5 stages, got %d", len(report.Stages))
	}
}

// TestLoadProfile_Extended runs all extended measurements (per-language,
// concurrent query, GOGC sweep, interning, WAL, cold start).
func TestLoadProfile_Extended(t *testing.T) {
	root := findRepoRoot(t)

	extReport, err := RunExtended(Config{
		RepoRoot: root,
		Size:     "small",
		Output:   os.Stdout,
	})
	if err != nil {
		t.Fatalf("RunExtended: %v", err)
	}

	// Sanity checks.
	if len(extReport.LanguageStats) == 0 {
		t.Error("expected at least 1 language stat")
	}
	if extReport.ConcurrentQuery == nil {
		t.Error("expected concurrent query result")
	} else if extReport.ConcurrentQuery.QueriesOK == 0 {
		t.Error("expected >0 concurrent queries completed")
	}
	if len(extReport.GOGCSweep) != 3 {
		t.Errorf("expected 3 GOGC results, got %d", len(extReport.GOGCSweep))
	}
	if extReport.InternStats == nil {
		t.Error("expected intern stats")
	}
	if extReport.ColdStart == nil {
		t.Error("expected cold start result")
	} else if extReport.ColdStart.NodesLoaded == 0 {
		t.Error("expected >0 nodes loaded in cold start")
	}

	t.Logf("Extended report: %d languages, %d concurrent queries, %d GOGC values",
		len(extReport.LanguageStats),
		extReport.ConcurrentQuery.QueriesOK,
		len(extReport.GOGCSweep))
}

// TestLeakDetection_Quick runs a shortened leak check (30s instead of 5min).
func TestLeakDetection_Quick(t *testing.T) {
	cfg := DefaultLeakDetectorConfig()
	cfg.Duration = 30 * time.Second
	cfg.SampleEvery = 1 * time.Second

	result := RunLeakDetection(cfg)
	t.Logf("Leak detection: passed=%v, growth=%s (%.1f%%)",
		result.Passed, fmtBytes(result.GrowthBytes), result.GrowthPct*100)

	if !result.Passed {
		t.Errorf("leak detected: %s", result.Reason)
	}
}

// TestLoadProfile_ProductionRepo profiles against a real large OSS repository.
// Set LOADTEST_PRODUCTION_REPO to the git clone URL and optionally
// LOADTEST_CLONE_DIR to control where it's cloned.
func TestLoadProfile_ProductionRepo(t *testing.T) {
	repoURL := os.Getenv("LOADTEST_PRODUCTION_REPO")
	if repoURL == "" {
		t.Skip("set LOADTEST_PRODUCTION_REPO to a git clone URL (e.g. https://github.com/grafana/grafana)")
	}

	cloneDir := os.Getenv("LOADTEST_CLONE_DIR")
	if cloneDir == "" {
		cloneDir = filepath.Join(os.TempDir(), "synapses-prod-loadtest")
	}

	report, err := RunProduction(ProductionConfig{
		RepoURL:      repoURL,
		CloneDir:     cloneDir,
		ShallowClone: true,
		Config: Config{
			Size:           "production",
			SkipEmbeddings: true,
			Output:         os.Stdout,
		},
	})
	if err != nil {
		t.Fatalf("RunProduction: %v", err)
	}

	// Sanity checks.
	if report.Indexing == nil || len(report.Indexing.Stages) < 5 {
		t.Error("expected at least 5 indexing stages")
	}
	if report.Retrieval == nil {
		t.Error("expected retrieval report")
	} else {
		for _, ql := range report.Retrieval.GetContext {
			if ql.P99 > 2*time.Second {
				t.Errorf("%s p99=%v exceeds 2s", ql.Label, ql.P99)
			}
		}
		if report.Retrieval.Search != nil && report.Retrieval.Search.P99 > time.Second {
			t.Errorf("search p99=%v exceeds 1s", report.Retrieval.Search.P99)
		}
	}

	t.Logf("Lines: %d, Nodes: %d, Edges: %d",
		report.RepoLineCount,
		report.Indexing.Graph.NodeCount(),
		report.Indexing.Graph.EdgeCount())
}

// findRepoRoot walks up from the test binary's directory to find the
// synapses repository root (identified by go.mod).
func findRepoRoot(t *testing.T) string {
	t.Helper()

	// Start from the current working directory.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root (go.mod)")
		}
		dir = parent
	}
}

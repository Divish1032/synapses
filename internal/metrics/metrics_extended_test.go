package metrics_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/metrics"
)

// ── RecentCommitsForFile ──────────────────────────────────────────────────────

func TestRecentCommitsForFile_NoGitRepo(t *testing.T) {
	// Non-git directory → git log fails → returns nil without panicking.
	dir := t.TempDir()
	result := metrics.RecentCommitsForFile(dir, "nonexistent.go", 3)
	if result != nil {
		t.Errorf("expected nil for non-git repo, got %v", result)
	}
}

func TestRecentCommitsForFile_ZeroLimit(t *testing.T) {
	// limit <= 0 defaults to 3, but non-git still returns nil.
	dir := t.TempDir()
	result := metrics.RecentCommitsForFile(dir, "file.go", 0)
	if result != nil {
		t.Errorf("expected nil for non-git repo (limit=0), got %v", result)
	}
}

func TestRecentCommitsForFile_NegativeLimit(t *testing.T) {
	dir := t.TempDir()
	result := metrics.RecentCommitsForFile(dir, "file.go", -1)
	if result != nil {
		t.Errorf("expected nil for non-git repo (limit=-1), got %v", result)
	}
}

// ── EnrichCoverage — additional path coverage ─────────────────────────────────

func TestEnrichCoverage_RelativeProfilePath(t *testing.T) {
	repoRoot := t.TempDir()
	g := buildMetricsGraph(t, repoRoot)

	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"),
		[]byte("module example.com/proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}

	coverContent := "mode: set\nexample.com/proj/pkg/svc.go:10.1,29.1 5 1\n"
	profFile := filepath.Join(repoRoot, "cover.out")
	if err := os.WriteFile(profFile, []byte(coverContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Relative path — EnrichCoverage joins it with repoRoot.
	metrics.EnrichCoverage(g, repoRoot, "cover.out")

	for _, n := range g.AllNodes() {
		if n.Name == "Serve" {
			if cov := n.Metadata["coverage"]; cov != "1.00" {
				t.Errorf("Serve coverage with relative profile = %q, want 1.00", cov)
			}
		}
	}
}

func TestEnrichCoverage_EmptyProfile(t *testing.T) {
	repoRoot := t.TempDir()
	g := buildMetricsGraph(t, repoRoot)

	// Profile with only the mode line and no data blocks.
	profilePath := writeCoverProfile(t, "mode: set\n")
	metrics.EnrichCoverage(g, repoRoot, profilePath)

	// No coverage should be assigned.
	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["coverage"] != "" {
			t.Errorf("node %s got unexpected coverage %q from empty profile", n.Name, n.Metadata["coverage"])
		}
	}
}

func TestEnrichCoverage_NoGoMod_FallbackPathStrip(t *testing.T) {
	// Without go.mod, readModuleName returns "". The fallback heuristic in
	// coverPathToRel strips leading domain-segment components.
	repoRoot := t.TempDir()
	g := buildMetricsGraph(t, repoRoot)

	// Note: without go.mod the module name is "" so coverPathToRel uses the
	// heuristic — it strips "github.com/example/proj/" to get "pkg/svc.go".
	coverContent := "mode: set\n" +
		"github.com/example/proj/pkg/svc.go:10.1,29.1 5 1\n"
	profilePath := writeCoverProfile(t, coverContent)

	// Must not panic; coverage may or may not match depending on path alignment.
	metrics.EnrichCoverage(g, repoRoot, profilePath)
}

func TestEnrichCoverage_NonFunctionNodeSkipped(t *testing.T) {
	// Non-function/method nodes should never get coverage metadata.
	repoRoot := t.TempDir()
	g := buildMetricsGraph(t, repoRoot)

	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"),
		[]byte("module example.com/proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	profilePath := writeCoverProfile(t, "mode: set\nexample.com/proj/pkg/svc.go:1.1,50.1 10 1\n")
	metrics.EnrichCoverage(g, repoRoot, profilePath)

	for _, n := range g.AllNodes() {
		if string(n.Type) != "function" && string(n.Type) != "method" {
			if n.Metadata != nil && n.Metadata["coverage"] != "" {
				t.Errorf("non-function node %s (%s) got coverage annotation", n.Name, n.Type)
			}
		}
	}
}

// ── EnrichChurn — additional edge cases ──────────────────────────────────────

func TestEnrichChurn_NegativeDays(t *testing.T) {
	// Negative days should be treated the same as 0 → defaults to 90.
	// Since there's no git repo, this is still a no-op.
	g := buildMetricsGraph(t, t.TempDir())
	metrics.EnrichChurn(g, t.TempDir(), -10) // must not panic
}

// ── EnrichPprof — additional edge cases ──────────────────────────────────────

func TestEnrichPprof_RelativeProfilePath(t *testing.T) {
	repoRoot := t.TempDir()
	g := buildMetricsGraph(t, repoRoot)
	// Non-existent relative path — EnrichPprof joins with repoRoot → silent.
	metrics.EnrichPprof(g, repoRoot, "cpu.pprof")

	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["cpu_pct"] != "" {
			t.Errorf("node %s got cpu_pct from missing relative profile", n.Name)
		}
	}
}

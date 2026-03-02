package metrics_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/metrics"
)

// buildMetricsGraph creates a simple graph with two function nodes.
func buildMetricsGraph(t *testing.T, repoRoot string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	g.SetRoot(repoRoot)

	fnID := g.MakeNodeID(filepath.Join(repoRoot, "pkg/svc.go"), "Serve")
	g.AddNode(&graph.Node{
		ID:       fnID,
		Type:     graph.NodeFunction,
		Name:     "Serve",
		Package:  "pkg",
		File:     filepath.Join(repoRoot, "pkg/svc.go"),
		Line:     10,
		Metadata: map[string]string{"line_count": "20"},
	})

	fn2ID := g.MakeNodeID(filepath.Join(repoRoot, "pkg/svc.go"), "Stop")
	g.AddNode(&graph.Node{
		ID:       fn2ID,
		Type:     graph.NodeFunction,
		Name:     "Stop",
		Package:  "pkg",
		File:     filepath.Join(repoRoot, "pkg/svc.go"),
		Line:     35,
		Metadata: map[string]string{"line_count": "5"},
	})

	return g
}

// writeCoverProfile writes a synthetic coverprofile to a temp file and returns its path.
func writeCoverProfile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "cover*.out")
	if err != nil {
		t.Fatalf("create cover profile: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write cover profile: %v", err)
	}
	return f.Name()
}

// --- EnrichChurn tests ---

func TestEnrichChurn_NoGitRepo(t *testing.T) {
	// EnrichChurn on a non-git directory should silently do nothing.
	g := buildMetricsGraph(t, t.TempDir())
	metrics.EnrichChurn(g, t.TempDir(), 30)

	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["churn"] != "" {
			t.Errorf("node %s got churn=%q but repo has no git history", n.Name, n.Metadata["churn"])
		}
	}
}

func TestEnrichChurn_ZeroDaysDefaultsTo90(t *testing.T) {
	// days=0 should not panic or error — function has a guard defaulting to 90.
	g := buildMetricsGraph(t, t.TempDir())
	metrics.EnrichChurn(g, t.TempDir(), 0) // should not panic
}

// --- EnrichCoverage tests ---

func TestEnrichCoverage_BasicCoverage(t *testing.T) {
	repoRoot := t.TempDir()
	g := buildMetricsGraph(t, repoRoot)

	// Write a minimal go.mod so readModuleName works.
	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"), []byte("module example.com/proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create the source file so paths align.
	if err := os.MkdirAll(filepath.Join(repoRoot, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Coverprofile: Serve (lines 10–29) is 100% covered; Stop (lines 35–39) is 0%.
	coverContent := `mode: set
example.com/proj/pkg/svc.go:10.1,29.1 5 1
example.com/proj/pkg/svc.go:35.1,39.1 2 0
`
	profilePath := writeCoverProfile(t, coverContent)

	metrics.EnrichCoverage(g, repoRoot, profilePath)

	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction {
			continue
		}
		switch n.Name {
		case "Serve":
			if n.Metadata["coverage"] != "1.00" {
				t.Errorf("Serve coverage = %q, want 1.00", n.Metadata["coverage"])
			}
		case "Stop":
			if n.Metadata["coverage"] != "0.00" {
				t.Errorf("Stop coverage = %q, want 0.00", n.Metadata["coverage"])
			}
		}
	}
}

func TestEnrichCoverage_MissingFile(t *testing.T) {
	g := buildMetricsGraph(t, t.TempDir())
	// Non-existent profile — should silently do nothing.
	metrics.EnrichCoverage(g, t.TempDir(), "/nonexistent/cover.out")

	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["coverage"] != "" {
			t.Errorf("node %s got coverage but profile was missing", n.Name)
		}
	}
}

func TestEnrichCoverage_MalformedLines(t *testing.T) {
	g := buildMetricsGraph(t, t.TempDir())

	// Profile with invalid/missing lines — should not panic, just skip bad lines.
	coverContent := `mode: set
this is not valid
example.com/proj/pkg/svc.go:10.1,29.1 notanumber 1
`
	profilePath := writeCoverProfile(t, coverContent)
	metrics.EnrichCoverage(g, t.TempDir(), profilePath) // must not panic
}

// --- EnrichPprof tests ---

func TestEnrichPprof_MissingProfile(t *testing.T) {
	g := buildMetricsGraph(t, t.TempDir())
	// Non-existent profile — should silently do nothing.
	metrics.EnrichPprof(g, t.TempDir(), "/nonexistent/cpu.pprof")

	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["cpu_pct"] != "" {
			t.Errorf("node %s got cpu_pct but pprof was missing", n.Name)
		}
	}
}

// --- pprofShortName indirectly via EnrichPprof path ---

// parseCoverProfile is internal so we test its effect through EnrichCoverage.
func TestEnrichCoverage_PartialOverlap(t *testing.T) {
	repoRoot := t.TempDir()
	g := buildMetricsGraph(t, repoRoot)

	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"), []byte("module example.com/proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two blocks: one covered, one not — Serve should get 0.50 coverage.
	coverContent := `mode: set
example.com/proj/pkg/svc.go:10.1,19.1 2 1
example.com/proj/pkg/svc.go:20.1,29.1 2 0
`
	profilePath := writeCoverProfile(t, coverContent)
	metrics.EnrichCoverage(g, repoRoot, profilePath)

	for _, n := range g.AllNodes() {
		if n.Name == "Serve" {
			if n.Metadata["coverage"] != "0.50" {
				t.Errorf("Serve partial coverage = %q, want 0.50", n.Metadata["coverage"])
			}
		}
	}
}

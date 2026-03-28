package metrics_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// initGitRepo creates a minimal git repo in dir with one commit touching files.
// Returns true if git is available and the setup succeeded.
func initGitRepo(t *testing.T, dir string, files map[string]string) bool {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
		return false
	}
	run := func(args ...string) bool {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Alice",
			"GIT_AUTHOR_EMAIL=alice@example.com",
			"GIT_COMMITTER_NAME=Alice",
			"GIT_COMMITTER_EMAIL=alice@example.com",
			"GIT_AUTHOR_DATE=2025-01-15T00:00:00Z",
			"GIT_COMMITTER_DATE=2025-01-15T00:00:00Z",
		)
		if err := cmd.Run(); err != nil {
			t.Logf("git %v: %v", args, err)
			return false
		}
		return true
	}
	if !run("init", "-b", "main") {
		// older git might not support -b
		if !run("init") {
			return false
		}
	}
	run("config", "user.email", "alice@example.com")
	run("config", "user.name", "Alice")
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		run("add", name)
	}
	run("commit", "-m", "fix: initial setup")
	return true
}

// --- EnrichBlame tests ---

func TestEnrichBlame_NoGitRepo(t *testing.T) {
	// EnrichBlame on a non-git directory should silently do nothing.
	g := buildMetricsGraph(t, t.TempDir())
	metrics.EnrichBlame(g, t.TempDir())

	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["blame_author"] != "" {
			t.Errorf("node %s got blame_author=%q but repo has no git history", n.Name, n.Metadata["blame_author"])
		}
	}
}

func TestEnrichBlame_WithGitRepo(t *testing.T) {
	repoRoot := t.TempDir()
	srcFile := filepath.Join(repoRoot, "pkg/svc.go")
	if !initGitRepo(t, repoRoot, map[string]string{
		"pkg/svc.go": "package pkg\nfunc Serve() {}\nfunc Stop() {}\n",
	}) {
		return
	}

	g := graph.New("testrepo")
	g.SetRoot(repoRoot)
	for _, name := range []string{"Serve", "Stop"} {
		id := g.MakeNodeID(srcFile, name)
		g.AddNode(&graph.Node{
			ID:      id,
			Type:    graph.NodeFunction,
			Name:    name,
			Package: "pkg",
			File:    srcFile,
			Line:    2,
			Metadata: map[string]string{"line_count": "1"},
		})
	}

	metrics.EnrichBlame(g, repoRoot)

	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction {
			continue
		}
		if n.Metadata["blame_author"] == "" {
			t.Errorf("node %s: blame_author not set", n.Name)
		}
		if n.Metadata["blame_date"] == "" {
			t.Errorf("node %s: blame_date not set", n.Name)
		}
		if n.Metadata["blame_commit"] == "" {
			t.Errorf("node %s: blame_commit not set", n.Name)
		}
		if n.Metadata["blame_subject"] == "" {
			t.Errorf("node %s: blame_subject not set", n.Name)
		}
		if n.Metadata["staleness_score"] == "" {
			t.Errorf("node %s: staleness_score not set", n.Name)
		}
	}
}

func TestEnrichBlame_SkipsVendored(t *testing.T) {
	repoRoot := t.TempDir()
	srcFile := filepath.Join(repoRoot, "vendor/pkg/svc.go")
	if !initGitRepo(t, repoRoot, map[string]string{
		"vendor/pkg/svc.go": "package pkg\nfunc Serve() {}\n",
	}) {
		return
	}

	g := graph.New("testrepo")
	g.SetRoot(repoRoot)
	id := g.MakeNodeID(srcFile, "Serve")
	g.AddNode(&graph.Node{
		ID:         id,
		Type:       graph.NodeFunction,
		Name:       "Serve",
		Package:    "pkg",
		File:       srcFile,
		Line:       2,
		Provenance: graph.ProvenanceVendored,
	})

	metrics.EnrichBlame(g, repoRoot)

	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["blame_author"] != "" {
			t.Errorf("vendored node %s got blame_author — should be skipped", n.Name)
		}
	}
}

func TestEnrichBlame_StalenessScore_UsesChurn(t *testing.T) {
	repoRoot := t.TempDir()
	srcFile := filepath.Join(repoRoot, "pkg/svc.go")
	if !initGitRepo(t, repoRoot, map[string]string{
		"pkg/svc.go": "package pkg\nfunc Serve() {}\n",
	}) {
		return
	}

	g := graph.New("testrepo")
	g.SetRoot(repoRoot)
	id := g.MakeNodeID(srcFile, "Serve")
	g.AddNode(&graph.Node{
		ID:      id,
		Type:    graph.NodeFunction,
		Name:    "Serve",
		Package: "pkg",
		File:    srcFile,
		Line:    2,
		Metadata: map[string]string{
			"line_count": "1",
			"churn":      "5", // pre-set churn so staleness > 0
		},
	})

	metrics.EnrichBlame(g, repoRoot)

	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction {
			continue
		}
		scoreStr := n.Metadata["staleness_score"]
		if scoreStr == "" {
			t.Fatal("staleness_score not set")
		}
		score, err := strconv.ParseFloat(scoreStr, 64)
		if err != nil {
			t.Fatalf("staleness_score %q is not a float: %v", scoreStr, err)
		}
		// With churn=5, log(6)≈1.79, and blame_date=2025-01-15 gives days > 0 → score > 0.
		if score <= 0 {
			t.Errorf("staleness_score = %f, want > 0 (churn=5, non-zero age)", score)
		}
	}
}

// TestEnrichBlame_UmbrellaWorkspace verifies that EnrichBlame succeeds when
// repoRoot is an umbrella workspace directory (no .git) that contains a
// git sub-repo in a subdirectory. This is the root cause of BUG-EVAL-6:
// passing the umbrella root to git -C fails with "not a git repository",
// so blame was silently absent from every get_context response.
func TestEnrichBlame_UmbrellaWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// umbrella/ (no .git) ← repoRoot passed to EnrichBlame
	//   └── subpkg/       (has .git)
	umbrella := t.TempDir()
	subpkg := filepath.Join(umbrella, "subpkg")
	if err := os.MkdirAll(subpkg, 0o755); err != nil {
		t.Fatalf("mkdir subpkg: %v", err)
	}

	srcFile := filepath.Join(subpkg, "svc.go")
	if !initGitRepo(t, subpkg, map[string]string{
		"svc.go": "package subpkg\nfunc Handle() {}\n",
	}) {
		return
	}

	g := graph.New("testrepo")
	g.SetRoot(umbrella)
	id := g.MakeNodeID(srcFile, "Handle")
	g.AddNode(&graph.Node{
		ID:      id,
		Type:    graph.NodeFunction,
		Name:    "Handle",
		Package: "subpkg",
		File:    srcFile,
		Line:    2,
	})

	// Pass the umbrella root — not a git repo itself.
	metrics.EnrichBlame(g, umbrella)

	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction {
			continue
		}
		if n.Metadata["blame_author"] == "" {
			t.Errorf("node %s: blame_author not set — umbrella workspace git root detection failed", n.Name)
		}
		if n.Metadata["blame_date"] == "" {
			t.Errorf("node %s: blame_date not set", n.Name)
		}
		if n.Metadata["staleness_score"] == "" {
			t.Errorf("node %s: staleness_score not set", n.Name)
		}
	}
}

// initGitRepoNow creates a minimal git repo in dir with one commit using the
// current timestamp, so churn queries with --since=N.days.ago include it.
// Returns true if git is available and setup succeeded.
func initGitRepoNow(t *testing.T, dir string, files map[string]string) bool {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
		return false
	}
	run := func(args ...string) bool {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if err := cmd.Run(); err != nil {
			t.Logf("git %v: %v", args, err)
			return false
		}
		return true
	}
	if !run("init", "-b", "main") {
		if !run("init") {
			return false
		}
	}
	run("config", "user.email", "alice@example.com")
	run("config", "user.name", "Alice")
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		run("add", name)
	}
	run("commit", "-m", "fix: initial setup")
	return true
}

// TestEnrichChurn_UmbrellaWorkspace verifies that EnrichChurn populates churn
// data for nodes in a git sub-repo when repoRoot is a non-git umbrella dir.
func TestEnrichChurn_UmbrellaWorkspace(t *testing.T) {
	umbrella := t.TempDir()
	subpkg := filepath.Join(umbrella, "subpkg")
	if err := os.MkdirAll(subpkg, 0o755); err != nil {
		t.Fatalf("mkdir subpkg: %v", err)
	}

	srcFile := filepath.Join(subpkg, "svc.go")
	// Use initGitRepoNow (current timestamp) so the commit falls within the
	// 30-day churn window.
	if !initGitRepoNow(t, subpkg, map[string]string{
		"svc.go": "package subpkg\nfunc Handle() {}\n",
	}) {
		return
	}

	g := graph.New("testrepo")
	g.SetRoot(umbrella)
	id := g.MakeNodeID(srcFile, "Handle")
	g.AddNode(&graph.Node{
		ID:      id,
		Type:    graph.NodeFunction,
		Name:    "Handle",
		Package: "subpkg",
		File:    srcFile,
		Line:    2,
	})

	metrics.EnrichChurn(g, umbrella, 30)

	found := false
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeFunction && n.Metadata["churn"] != "" {
			found = true
		}
	}
	if !found {
		t.Error("no churn data set for any node — umbrella workspace git root detection failed")
	}
}

// TestEnrichCommitContext_UmbrellaWorkspace verifies commit_context metadata
// is populated when repoRoot is a non-git umbrella workspace directory.
func TestEnrichCommitContext_UmbrellaWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	umbrella := t.TempDir()
	subpkg := filepath.Join(umbrella, "subpkg")
	if err := os.MkdirAll(subpkg, 0o755); err != nil {
		t.Fatalf("mkdir subpkg: %v", err)
	}

	srcFile := filepath.Join(subpkg, "svc.go")
	if !initGitRepo(t, subpkg, map[string]string{
		"svc.go": "package subpkg\nfunc Handle() {}\n",
	}) {
		return
	}

	g := graph.New("testrepo")
	g.SetRoot(umbrella)
	id := g.MakeNodeID(srcFile, "Handle")
	g.AddNode(&graph.Node{
		ID:      id,
		Type:    graph.NodeFunction,
		Name:    "Handle",
		Package: "subpkg",
		File:    srcFile,
		Line:    2,
	})

	metrics.EnrichCommitContext(g, umbrella)

	found := false
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeFunction && n.Metadata["commit_context"] != "" {
			found = true
		}
	}
	if !found {
		t.Error("no commit_context data set for any node — umbrella workspace git root detection failed")
	}
}

func TestEnrichBlameForFile_UpdatesOnlyTargetFile(t *testing.T) {
	repoRoot := t.TempDir()
	srcFile := filepath.Join(repoRoot, "pkg/svc.go")
	otherFile := filepath.Join(repoRoot, "pkg/other.go")
	if !initGitRepo(t, repoRoot, map[string]string{
		"pkg/svc.go":   "package pkg\nfunc Serve() {}\n",
		"pkg/other.go": "package pkg\nfunc Other() {}\n",
	}) {
		return
	}

	g := graph.New("testrepo")
	g.SetRoot(repoRoot)
	for _, tc := range []struct {
		file, name string
	}{
		{srcFile, "Serve"},
		{otherFile, "Other"},
	} {
		id := g.MakeNodeID(tc.file, tc.name)
		g.AddNode(&graph.Node{
			ID:      id,
			Type:    graph.NodeFunction,
			Name:    tc.name,
			Package: "pkg",
			File:    tc.file,
			Line:    2,
		})
	}

	// Only enrich svc.go — Other in other.go should remain unset.
	metrics.EnrichBlameForFile(g, repoRoot, srcFile)

	for _, n := range g.AllNodes() {
		switch n.Name {
		case "Serve":
			if n.Metadata["blame_author"] == "" {
				t.Error("Serve: blame_author not set after EnrichBlameForFile")
			}
		case "Other":
			if n.Metadata != nil && n.Metadata["blame_author"] != "" {
				t.Error("Other: blame_author should not be set (different file)")
			}
		}
	}
}

func TestEnrichBlameForFile_NoGitRepo(t *testing.T) {
	g := buildMetricsGraph(t, t.TempDir())
	// Must not panic or set any blame fields.
	metrics.EnrichBlameForFile(g, t.TempDir(), filepath.Join(t.TempDir(), "pkg/svc.go"))
	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["blame_author"] != "" {
			t.Errorf("node %s got blame_author — no git repo", n.Name)
		}
	}
}

// --- BlameAgeLabel tests ---

func TestBlameAgeLabel_Today(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	got := metrics.BlameAgeLabel(today)
	if got != "today" {
		t.Errorf("BlameAgeLabel(%q) = %q, want \"today\"", today, got)
	}
}

func TestBlameAgeLabel_Days(t *testing.T) {
	date := time.Now().AddDate(0, 0, -3).Format("2006-01-02")
	got := metrics.BlameAgeLabel(date)
	if !strings.HasSuffix(got, "d ago") {
		t.Errorf("BlameAgeLabel(%q) = %q, want \"Nd ago\"", date, got)
	}
}

func TestBlameAgeLabel_InvalidDate(t *testing.T) {
	got := metrics.BlameAgeLabel("not-a-date")
	if got != "" {
		t.Errorf("BlameAgeLabel(invalid) = %q, want \"\"", got)
	}
}

func TestBlameAgeLabel_Weeks(t *testing.T) {
	date := time.Now().AddDate(0, 0, -14).Format("2006-01-02")
	got := metrics.BlameAgeLabel(date)
	if !strings.HasSuffix(got, "w ago") {
		t.Errorf("BlameAgeLabel(%q) = %q, want \"Nw ago\"", date, got)
	}
}

func TestBlameAgeLabel_Months(t *testing.T) {
	date := time.Now().AddDate(0, -2, 0).Format("2006-01-02")
	got := metrics.BlameAgeLabel(date)
	if !strings.HasSuffix(got, "mo ago") {
		t.Errorf("BlameAgeLabel(%q) = %q, want \"Nmo ago\"", date, got)
	}
}

func TestBlameAgeLabel_Years(t *testing.T) {
	date := time.Now().AddDate(-2, 0, 0).Format("2006-01-02")
	got := metrics.BlameAgeLabel(date)
	if !strings.HasSuffix(got, "y ago") {
		t.Errorf("BlameAgeLabel(%q) = %q, want \"Ny ago\"", date, got)
	}
}

// --- StalenessLabel tests ---

func TestStalenessLabel(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{0, "low"},
		{29.9, "low"},
		{30, "medium"},
		{149.9, "medium"},
		{150, "high"},
		{999, "high"},
	}
	for _, c := range cases {
		got := metrics.StalenessLabel(c.score)
		if got != c.want {
			t.Errorf("StalenessLabel(%.1f) = %q, want %q", c.score, got, c.want)
		}
	}
}

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

// --- EnrichCommitContext tests ---

func TestEnrichCommitContext_NoGitRepo(t *testing.T) {
	// EnrichCommitContext on a non-git directory should silently do nothing.
	g := buildMetricsGraph(t, t.TempDir())
	metrics.EnrichCommitContext(g, t.TempDir())

	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["commit_context"] != "" {
			t.Errorf("node %s got commit_context=%q but repo has no git history", n.Name, n.Metadata["commit_context"])
		}
	}
}

func TestEnrichCommitContext_WithGitRepo(t *testing.T) {
	repoRoot := t.TempDir()
	srcFile := filepath.Join(repoRoot, "pkg/svc.go")
	if !initGitRepo(t, repoRoot, map[string]string{
		"pkg/svc.go": "package pkg\nfunc Serve() {}\nfunc Stop() {}\n",
	}) {
		return
	}

	g := graph.New("testrepo")
	g.SetRoot(repoRoot)
	for _, name := range []string{"Serve", "Stop"} {
		id := g.MakeNodeID(srcFile, name)
		g.AddNode(&graph.Node{
			ID:      id,
			Type:    graph.NodeFunction,
			Name:    name,
			Package: "pkg",
			File:    srcFile,
			Line:    2,
		})
	}

	metrics.EnrichCommitContext(g, repoRoot)

	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction {
			continue
		}
		raw := n.Metadata["commit_context"]
		if raw == "" {
			t.Errorf("node %s: commit_context not set", n.Name)
			continue
		}
		var commits []metrics.CommitInfo
		if err := json.Unmarshal([]byte(raw), &commits); err != nil {
			t.Errorf("node %s: commit_context is not valid JSON: %v", n.Name, err)
			continue
		}
		if len(commits) == 0 {
			t.Errorf("node %s: commit_context has 0 commits", n.Name)
			continue
		}
		if commits[0].Message == "" {
			t.Errorf("node %s: commits[0].Message is empty", n.Name)
		}
		if commits[0].Hash == "" {
			t.Errorf("node %s: commits[0].Hash is empty", n.Name)
		}
	}
}

func TestEnrichCommitContext_SkipsVendored(t *testing.T) {
	repoRoot := t.TempDir()
	srcFile := filepath.Join(repoRoot, "vendor/pkg/svc.go")
	if !initGitRepo(t, repoRoot, map[string]string{
		"vendor/pkg/svc.go": "package pkg\nfunc Serve() {}\n",
	}) {
		return
	}

	g := graph.New("testrepo")
	g.SetRoot(repoRoot)
	id := g.MakeNodeID(srcFile, "Serve")
	g.AddNode(&graph.Node{
		ID:         id,
		Type:       graph.NodeFunction,
		Name:       "Serve",
		Package:    "pkg",
		File:       srcFile,
		Line:       2,
		Provenance: graph.ProvenanceVendored,
	})

	metrics.EnrichCommitContext(g, repoRoot)

	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["commit_context"] != "" {
			t.Errorf("vendored node %s got commit_context — should be skipped", n.Name)
		}
	}
}

func TestEnrichCommitContextForFile_UpdatesOnlyTargetFile(t *testing.T) {
	repoRoot := t.TempDir()
	srcFile := filepath.Join(repoRoot, "pkg/svc.go")
	otherFile := filepath.Join(repoRoot, "pkg/other.go")
	if !initGitRepo(t, repoRoot, map[string]string{
		"pkg/svc.go":   "package pkg\nfunc Serve() {}\n",
		"pkg/other.go": "package pkg\nfunc Other() {}\n",
	}) {
		return
	}

	g := graph.New("testrepo")
	g.SetRoot(repoRoot)
	for _, tc := range []struct{ file, name string }{
		{srcFile, "Serve"},
		{otherFile, "Other"},
	} {
		id := g.MakeNodeID(tc.file, tc.name)
		g.AddNode(&graph.Node{
			ID:      id,
			Type:    graph.NodeFunction,
			Name:    tc.name,
			Package: "pkg",
			File:    tc.file,
			Line:    2,
		})
	}

	// Only enrich svc.go — Other in other.go should remain unset.
	metrics.EnrichCommitContextForFile(g, repoRoot, srcFile)

	for _, n := range g.AllNodes() {
		switch n.Name {
		case "Serve":
			if n.Metadata == nil || n.Metadata["commit_context"] == "" {
				t.Error("Serve: commit_context not set after EnrichCommitContextForFile")
			}
		case "Other":
			if n.Metadata != nil && n.Metadata["commit_context"] != "" {
				t.Error("Other: commit_context should not be set (different file)")
			}
		}
	}
}

func TestEnrichCommitContextForFile_NoGitRepo(t *testing.T) {
	g := buildMetricsGraph(t, t.TempDir())
	// Must not panic or set any commit_context fields.
	metrics.EnrichCommitContextForFile(g, t.TempDir(), filepath.Join(t.TempDir(), "pkg/svc.go"))

	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["commit_context"] != "" {
			t.Errorf("node %s got commit_context — no git repo", n.Name)
		}
	}
}

// --- Additional EnrichCoverage edge cases ---

func TestEnrichCoverage_NoOverlapBlocks(t *testing.T) {
	g := buildMetricsGraph(t, t.TempDir())
	repoRoot := t.TempDir()

	// Create a cover profile with blocks that don't overlap with function lines.
	profilePath := filepath.Join(repoRoot, "cover.out")
	content := `mode: set
github.com/foo/bar/pkg/svc.go:999:1000,5 1 0
github.com/foo/bar/pkg/svc.go:1001:1002,5 1 0`
	if err := os.WriteFile(profilePath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Should not panic when blocks don't overlap with any functions.
	// The functions are at lines 10-12, blocks are at 999-1002.
	metrics.EnrichCoverage(g, repoRoot, profilePath)

	// Verify no coverage metadata was set (since no blocks overlapped).
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		if n.Metadata != nil && n.Metadata["coverage"] != "" {
			t.Errorf("node %s should not have coverage (no overlap)", n.Name)
		}
	}
}

func TestEnrichCoverage_NoStatementsFunctionsIgnored(t *testing.T) {
	g := buildMetricsGraph(t, t.TempDir())
	repoRoot := t.TempDir()

	// Create cover profile with blocks that don't align to any function.
	profilePath := filepath.Join(repoRoot, "cover.out")
	content := `mode: set
github.com/foo/bar/pkg/svc.go:50:60,5 1 1`
	if err := os.WriteFile(profilePath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Should not crash when handling functions with no overlapping blocks.
	metrics.EnrichCoverage(g, repoRoot, profilePath)

	// Verify no coverage metadata was incorrectly set.
	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["coverage"] != "" {
			t.Errorf("node %s should not have coverage (no overlap)", n.Name)
		}
	}
}

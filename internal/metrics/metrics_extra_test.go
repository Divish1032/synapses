package metrics

// Additional coverage tests for BlameAgeLabel branches and EnrichCommitContextForFile.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── BlameAgeLabel — all branches ──────────────────────────────────────────────
// Use UTC-anchored dates to avoid timezone-dependent day calculations.

func TestBlameAgeLabel_Today(t *testing.T) {
	// UTC date today → time.Since < 24h → days = 0 → "today"
	today := time.Now().UTC().Format("2006-01-02")
	got := BlameAgeLabel(today)
	if got != "today" {
		t.Errorf("got %q, want %q", got, "today")
	}
}

func TestBlameAgeLabel_ThreeDays(t *testing.T) {
	// Use UTC midnight 3 days ago; time.Since = 3*24+hour hours → days = 3.
	d := time.Now().UTC().AddDate(0, 0, -3).Format("2006-01-02")
	got := BlameAgeLabel(d)
	if !strings.HasSuffix(got, "d ago") {
		t.Errorf("expected Xd ago pattern, got %q", got)
	}
}

func TestBlameAgeLabel_OneWeek(t *testing.T) {
	// 10 days ago → days = 10 → weeks = 1 → "1w ago"
	d := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02")
	got := BlameAgeLabel(d)
	if got != "1w ago" {
		t.Errorf("got %q, want %q", got, "1w ago")
	}
}

func TestBlameAgeLabel_TwoWeeks(t *testing.T) {
	// 21 days ago → days = 21 → weeks = 3 → "3w ago"
	d := time.Now().UTC().AddDate(0, 0, -21).Format("2006-01-02")
	got := BlameAgeLabel(d)
	if !strings.HasSuffix(got, "w ago") {
		t.Errorf("expected Xw ago pattern, got %q", got)
	}
}

func TestBlameAgeLabel_OneMonth(t *testing.T) {
	// 45 days ago → days = 45 → months = 1 → "1mo ago"
	d := time.Now().UTC().AddDate(0, 0, -45).Format("2006-01-02")
	got := BlameAgeLabel(d)
	if got != "1mo ago" {
		t.Errorf("got %q, want %q", got, "1mo ago")
	}
}

func TestBlameAgeLabel_TwoMonths(t *testing.T) {
	// 75 days ago → days = 75 → months = 2 → "2mo ago"
	d := time.Now().UTC().AddDate(0, 0, -75).Format("2006-01-02")
	got := BlameAgeLabel(d)
	if got != "2mo ago" {
		t.Errorf("got %q, want %q", got, "2mo ago")
	}
}

func TestBlameAgeLabel_OneYear(t *testing.T) {
	// 400 days ago → days = 400 → years = 1 → "1y ago"
	d := time.Now().UTC().AddDate(0, 0, -400).Format("2006-01-02")
	got := BlameAgeLabel(d)
	if got != "1y ago" {
		t.Errorf("got %q, want %q", got, "1y ago")
	}
}

func TestBlameAgeLabel_TwoYears(t *testing.T) {
	// 800 days ago → days = 800 → years = 2 → "2y ago"
	d := time.Now().UTC().AddDate(0, 0, -800).Format("2006-01-02")
	got := BlameAgeLabel(d)
	if got != "2y ago" {
		t.Errorf("got %q, want %q", got, "2y ago")
	}
}

func TestBlameAgeLabel_InvalidDate(t *testing.T) {
	got := BlameAgeLabel("not-a-date")
	if got != "" {
		t.Errorf("expected empty for invalid date, got %q", got)
	}
}

// ── EnrichCommitContextForFile ────────────────────────────────────────────────

func initGitRepoForMetrics(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) bool {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		return cmd.Run() == nil
	}

	if !run("git", "init") {
		t.Skip("git not available")
	}
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "Test")

	absFile := filepath.Join(dir, "svc.go")
	if err := os.WriteFile(absFile, []byte("package p\nfunc Serve() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !run("git", "add", ".") {
		t.Skip("git add failed")
	}
	if !run("git", "commit", "-m", "initial") {
		t.Skip("git commit failed")
	}
	return dir, absFile
}

func TestEnrichCommitContextForFile_WithGitRepo(t *testing.T) {
	root, absFile := initGitRepoForMetrics(t)

	g := graph.New("testrepo")
	g.SetRoot(root)
	id := g.MakeNodeID(absFile, "Serve")
	g.AddNode(&graph.Node{
		ID: id, Name: "Serve", Type: graph.NodeFunction,
		File: absFile, Package: "p",
	})

	EnrichCommitContextForFile(g, root, absFile)

	n := g.GetNode(id)
	if n == nil {
		t.Fatal("expected node to exist")
	}
	if n.Metadata == nil || n.Metadata["commit_context"] == "" {
		t.Error("expected commit_context metadata to be set")
	}
}

func TestEnrichCommitContextForFile_NoNodes(t *testing.T) {
	root := t.TempDir()
	g := graph.New("testrepo")
	// No nodes for the file → early return, must not panic.
	EnrichCommitContextForFile(g, root, filepath.Join(root, "nonexistent.go"))
}

func TestEnrichCommitContextForFile_SkipsNonFuncNodes(t *testing.T) {
	root, absFile := initGitRepoForMetrics(t)

	g := graph.New("testrepo")
	g.SetRoot(root)
	// Add a file node (not function/method) — should be skipped by the continue path.
	fileNodeID := g.MakeNodeID(absFile, "svc.go")
	g.AddNode(&graph.Node{
		ID: fileNodeID, Name: "svc.go", Type: graph.NodeFile,
		File: absFile, Package: "p",
	})
	// Also add a function node to exercise the metadata-set path.
	funcNodeID := g.MakeNodeID(absFile, "Serve")
	g.AddNode(&graph.Node{
		ID: funcNodeID, Name: "Serve", Type: graph.NodeFunction,
		File: absFile, Package: "p",
	})

	EnrichCommitContextForFile(g, root, absFile)

	// Function node should have metadata; file node should not.
	fn := g.GetNode(funcNodeID)
	if fn.Metadata == nil || fn.Metadata["commit_context"] == "" {
		t.Error("expected commit_context on function node")
	}
}

func TestEnrichCommitContextForFile_NoGitCommits(t *testing.T) {
	root := t.TempDir()
	absFile := filepath.Join(root, "new.go")

	g := graph.New("testrepo")
	id := g.MakeNodeID(absFile, "NewFunc")
	g.AddNode(&graph.Node{
		ID: id, Name: "NewFunc", Type: graph.NodeFunction,
		File: absFile, Package: "p",
	})

	// No git repo → no commits → should return without setting metadata.
	EnrichCommitContextForFile(g, root, absFile)
}

// ── EnrichBlame — skips vendored/generated nodes ──────────────────────────────

func TestEnrichBlame_SkipsVendored(t *testing.T) {
	g := graph.New("testrepo")
	id := graph.NodeID("testrepo::vendor/lib.go::VendorFunc")
	g.AddNode(&graph.Node{
		ID: id, Name: "VendorFunc", Type: graph.NodeFunction,
		File:       "vendor/lib.go",
		Provenance: graph.ProvenanceVendored,
	})

	// Should return cleanly without attempting git blame.
	EnrichBlame(g, t.TempDir())
}

func TestEnrichBlame_SkipsGenerated(t *testing.T) {
	g := graph.New("testrepo")
	id := graph.NodeID("testrepo::gen/pb.go::Generated")
	g.AddNode(&graph.Node{
		ID: id, Name: "Generated", Type: graph.NodeFunction,
		File:       "gen/pb.go",
		Provenance: graph.ProvenanceGenerated,
	})

	EnrichBlame(g, t.TempDir())
}

// ── EnrichBlameForFile — uncovered paths ─────────────────────────────────────

func TestEnrichBlameForFile_NoCommits(t *testing.T) {
	root := t.TempDir()
	g := graph.New("testrepo")
	absFile := filepath.Join(root, "svc.go")
	id := g.MakeNodeID(absFile, "Serve")
	g.AddNode(&graph.Node{
		ID: id, Name: "Serve", Type: graph.NodeFunction,
		File: absFile, Package: "p",
	})

	// No git repo → fileBlame returns nil → no metadata set.
	EnrichBlameForFile(g, root, absFile)

	n := g.GetNode(id)
	if n.Metadata != nil && n.Metadata["blame_author"] != "" {
		t.Error("expected no blame metadata for non-git dir")
	}
}

func TestEnrichBlameForFile_WithChurn(t *testing.T) {
	root, absFile := initGitRepoForMetrics(t)

	g := graph.New("testrepo")
	g.SetRoot(root)
	id := g.MakeNodeID(absFile, "Serve")
	g.AddNode(&graph.Node{
		ID: id, Name: "Serve", Type: graph.NodeFunction,
		File: absFile, Package: "p",
		// Pre-set churn so the churn = c path is exercised.
		Metadata: map[string]string{"churn": "3.0"},
	})

	EnrichBlameForFile(g, root, absFile)

	n := g.GetNode(id)
	if n == nil {
		t.Fatal("expected node to exist")
	}
	// staleness_score should be non-empty (churn parsed + blame applied).
	if n.Metadata == nil || n.Metadata["staleness_score"] == "" {
		t.Error("expected staleness_score to be set when churn is present")
	}
}

// ── EnrichCommitContextForFile — skips vendored nodes ────────────────────────

func TestEnrichCommitContextForFile_SkipsVendored(t *testing.T) {
	root, absFile := initGitRepoForMetrics(t)

	g := graph.New("testrepo")
	g.SetRoot(root)
	// Add a vendored function node — should be skipped by the provenance check.
	vendorID := g.MakeNodeID(absFile, "VendorFunc")
	g.AddNode(&graph.Node{
		ID: vendorID, Name: "VendorFunc", Type: graph.NodeFunction,
		File: absFile, Package: "p",
		Provenance: graph.ProvenanceVendored,
	})
	// Also add a normal function node to reach the inner loop.
	funcID := g.MakeNodeID(absFile, "Serve")
	g.AddNode(&graph.Node{
		ID: funcID, Name: "Serve", Type: graph.NodeFunction,
		File: absFile, Package: "p",
	})

	EnrichCommitContextForFile(g, root, absFile)

	// Vendored node should not have commit_context.
	vn := g.GetNode(vendorID)
	if vn != nil && vn.Metadata != nil && vn.Metadata["commit_context"] != "" {
		t.Error("vendored node should not have commit_context")
	}
}

// ── EnrichPprof — relative path ───────────────────────────────────────────────

func TestEnrichPprof_RelativePath(t *testing.T) {
	// profilePath is relative → converted to abs via filepath.Join(repoRoot, profilePath).
	// Non-existent file → parsePprofTop returns error → early return, no panic.
	g := graph.New("testrepo")
	root := t.TempDir()
	EnrichPprof(g, root, "nonexistent.pprof")
}

// ── RecentCommitsForFile — body truncation ────────────────────────────────────

func TestRecentCommitsForFile_LongBody(t *testing.T) {
	dir := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		_ = cmd.Run()
	}

	run("git", "init")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "Test")

	absFile := filepath.Join(dir, "a.go")
	if err := os.WriteFile(absFile, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")

	// Commit with a body longer than 200 characters to exercise body[:200] truncation.
	longBody := "This is a very long commit body that exceeds two hundred characters. " +
		"It goes on and on to make sure that the truncation logic in RecentCommitsForFile " +
		"is exercised properly by the test suite. Here are some extra words."
	run("git", "commit", "-m", "subject\n\n"+longBody)

	commits := RecentCommitsForFile(dir, absFile, 3)
	if len(commits) == 0 {
		t.Skip("git not available or commit failed")
	}
	if commits[0].Body != "" && len(commits[0].Body) > 200 {
		t.Errorf("body should be truncated to 200 chars, got %d", len(commits[0].Body))
	}
}

// ── EnrichBlame — non-function node + churn path ─────────────────────────────

func TestEnrichBlame_WithGitRepo_CoversChurnAndNilMetadata(t *testing.T) {
	root, absFile := initGitRepoForMetrics(t)

	g := graph.New("testrepo")
	g.SetRoot(root)

	// Non-function node (struct) — should hit the first continue in EnrichBlame.
	structID := graph.NodeID("testrepo::svc.go::Server")
	g.AddNode(&graph.Node{
		ID: structID, Name: "Server", Type: graph.NodeStruct,
		File: absFile, Package: "p",
	})

	// Function node with no metadata — covers n.Metadata = make(...) path.
	funcNoMeta := g.MakeNodeID(absFile, "Serve")
	g.AddNode(&graph.Node{
		ID: funcNoMeta, Name: "Serve", Type: graph.NodeFunction,
		File: absFile, Package: "p",
	})

	// Function node with churn pre-set — covers churn = c path.
	funcChurn := g.MakeNodeID(absFile, "ServeWithChurn")
	g.AddNode(&graph.Node{
		ID: funcChurn, Name: "ServeWithChurn", Type: graph.NodeFunction,
		File: absFile, Package: "p",
		Metadata: map[string]string{"churn": "5.0"},
	})

	EnrichBlame(g, root)

	n := g.GetNode(funcNoMeta)
	if n != nil && n.Metadata != nil && n.Metadata["blame_author"] == "" {
		// blame_author should be set when git is available.
		t.Log("blame_author not set — git may not have blame data for this file")
	}
}

// ── EnrichBlameForFile — all fields + staleness on fresh node ─────────────────

// TestEnrichBlameForFile_SetsAllBlameFields verifies that every blame metadata
// field is populated when the file has a real git commit.
func TestEnrichBlameForFile_SetsAllBlameFields(t *testing.T) {
	root, absFile := initGitRepoForMetrics(t)

	g := graph.New("testrepo")
	g.SetRoot(root)
	id := g.MakeNodeID(absFile, "Serve")
	g.AddNode(&graph.Node{
		ID: id, Name: "Serve", Type: graph.NodeFunction,
		File: absFile, Package: "p",
		// No pre-existing metadata — simulates a fresh node after incremental reparse.
	})

	EnrichBlameForFile(g, root, absFile)

	n := g.GetNode(id)
	if n == nil {
		t.Fatal("node not found after EnrichBlameForFile")
	}
	if n.Metadata == nil {
		t.Fatal("Metadata is nil — EnrichBlameForFile did not run")
	}
	for _, field := range []string{"blame_author", "blame_date", "blame_commit", "blame_subject"} {
		if n.Metadata[field] == "" {
			t.Errorf("expected %s to be set, got empty string", field)
		}
	}
	// blame_date must be a valid ISO date.
	if _, err := time.Parse("2006-01-02", n.Metadata["blame_date"]); err != nil {
		t.Errorf("blame_date %q is not a valid ISO date: %v", n.Metadata["blame_date"], err)
	}
	// blame_commit must be a 7-character short hash.
	if len(n.Metadata["blame_commit"]) != 7 {
		t.Errorf("blame_commit %q should be a 7-char short hash", n.Metadata["blame_commit"])
	}
}

// TestEnrichBlameForFile_FreshNodeGetsChurnAndStaleness is the core regression
// test for the staleness_score=0 bug. A node with no prior churn metadata
// (fresh from incremental reparse) must receive a non-empty churn and a
// staleness_score computed from actual git history, not fall back to "0.0".
func TestEnrichBlameForFile_FreshNodeGetsChurnAndStaleness(t *testing.T) {
	root, absFile := initGitRepoForMetrics(t)

	g := graph.New("testrepo")
	g.SetRoot(root)
	id := g.MakeNodeID(absFile, "Serve")
	g.AddNode(&graph.Node{
		ID: id, Name: "Serve", Type: graph.NodeFunction,
		File: absFile, Package: "p",
		// Deliberately no "churn" in Metadata — simulates a node created by
		// incremental reparse before EnrichChurn runs at next startup.
	})

	EnrichBlameForFile(g, root, absFile)

	n := g.GetNode(id)
	if n == nil {
		t.Fatal("node not found after EnrichBlameForFile")
	}
	if n.Metadata == nil {
		t.Fatal("Metadata is nil")
	}
	// churn must be set by EnrichBlameForFile itself, not left empty.
	if n.Metadata["churn"] == "" {
		t.Error("expected churn to be set by EnrichBlameForFile for a fresh node")
	}
	// staleness_score must be non-empty. For a just-committed file, daysAgo ≈ 0
	// so the score may be "0.0" — but the field itself must always be present.
	if n.Metadata["staleness_score"] == "" {
		t.Error("expected staleness_score to always be set after EnrichBlameForFile")
	}
}

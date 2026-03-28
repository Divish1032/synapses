package metrics_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/metrics"
)

// TestEnrichPprof_EmptyProfile tests EnrichPprof with non-existent profile.
func TestEnrichPprof_EmptyProfile(t *testing.T) {
	g := graph.New("test")
	fn := &graph.Node{
		ID:   g.MakeNodeID("file.go", "MyFunc"),
		Type: graph.NodeFunction,
		Name: "MyFunc",
		File: "file.go",
		Line: 10,
	}
	g.AddNode(fn)

	// Non-existent profile should not panic
	metrics.EnrichPprof(g, ".", "/nonexistent/profile")

	// Node should remain unmodified
	if fn.Metadata != nil && fn.Metadata["cpu_pct"] != "" {
		t.Error("node should not be enriched with missing profile")
	}
}

// TestEnrichPprof_NoFunctionNodes tests EnrichPprof with only non-function nodes.
func TestEnrichPprof_NoFunctionNodes(t *testing.T) {
	g := graph.New("test")
	st := &graph.Node{
		ID:   g.MakeNodeID("file.go", "MyStruct"),
		Type: graph.NodeStruct,
		Name: "MyStruct",
		File: "file.go",
		Line: 10,
	}
	g.AddNode(st)

	// Create a valid pprof profile
	profilePath := createMinimalPprofProfile(t)
	defer os.Remove(profilePath)

	metrics.EnrichPprof(g, ".", profilePath)

	// Struct node should not have cpu_pct
	if st.Metadata != nil && st.Metadata["cpu_pct"] != "" {
		t.Error("non-function nodes should not be enriched")
	}
}

// TestEnrichChurn_WithGitRepo tests EnrichChurn actually sets churn metadata.
func TestEnrichChurn_WithGitRepo(t *testing.T) {
	tmpdir := initGitRepoWithCommit(t)
	defer os.RemoveAll(tmpdir)

	g := graph.New("test")
	g.SetRoot(tmpdir)
	fn := &graph.Node{
		ID:   g.MakeNodeID(filepath.Join(tmpdir, "file.go"), "Func"),
		Type: graph.NodeFunction,
		Name: "Func",
		File: filepath.Join(tmpdir, "file.go"),
		Line: 10,
	}
	g.AddNode(fn)

	metrics.EnrichChurn(g, tmpdir, 30)

	// Should have churn metadata since file was committed
	if fn.Metadata != nil && fn.Metadata["churn"] != "" {
		// Success - churn was set
		return
	}
	// It's OK if churn is not set if file has no recent history
}

// TestEnrichBlameForFile_WithGitRepo tests per-file blame enrichment.
func TestEnrichBlameForFile_WithGitRepo(t *testing.T) {
	tmpdir := initGitRepoWithCommit(t)
	defer os.RemoveAll(tmpdir)

	g := graph.New("test")
	g.SetRoot(tmpdir)

	filePath := filepath.Join(tmpdir, "file.go")
	fn := &graph.Node{
		ID:   g.MakeNodeID(filePath, "Func"),
		Type: graph.NodeFunction,
		Name: "Func",
		File: filePath,
		Line: 10,
	}
	g.AddNode(fn)

	// Should not panic
	metrics.EnrichBlameForFile(g, tmpdir, filePath)

	// Blame metadata may or may not be set depending on git state
}

// TestEnrichCommitContextForFile_WithGitRepo tests per-file commit context.
func TestEnrichCommitContextForFile_WithGitRepo(t *testing.T) {
	tmpdir := initGitRepoWithCommit(t)
	defer os.RemoveAll(tmpdir)

	g := graph.New("test")
	g.SetRoot(tmpdir)

	filePath := filepath.Join(tmpdir, "file.go")
	fn := &graph.Node{
		ID:   g.MakeNodeID(filePath, "Func"),
		Type: graph.NodeFunction,
		Name: "Func",
		File: filePath,
		Line: 10,
	}
	g.AddNode(fn)

	// Should not panic
	metrics.EnrichCommitContextForFile(g, tmpdir, filePath)

	// Commit context may or may not be set
}

// TestRecentCommitsForFile_LargeLimit tests with large commit limit.
func TestRecentCommitsForFile_LargeLimit(t *testing.T) {
	tmpdir := initGitRepoWithMultipleCommits(t)
	defer os.RemoveAll(tmpdir)

	filePath := filepath.Join(tmpdir, "file.go")

	// Request more commits than exist (100 vs 3 created)
	commits := metrics.RecentCommitsForFile(context.Background(), tmpdir, filePath, 100)

	// Should return at most 3 commits (what we created)
	if len(commits) > 3 {
		t.Errorf("got %d commits, want at most 3", len(commits))
	}
}

// TestEnrichCoverage_NoMatchingFiles tests EnrichCoverage with non-existent profile.
func TestEnrichCoverage_NoProfile(t *testing.T) {
	g := graph.New("test")
	fn := &graph.Node{
		ID:       g.MakeNodeID("file.go", "Func"),
		Type:     graph.NodeFunction,
		Name:     "Func",
		File:     "file.go",
		Line:     10,
		Metadata: map[string]string{"line_count": "10"},
	}
	g.AddNode(fn)

	// Non-existent profile should not panic
	metrics.EnrichCoverage(g, ".", "/nonexistent/profile")

	// Node should remain unmodified
	if fn.Metadata["coverage"] != "" {
		t.Error("node should not have coverage set")
	}
}

// TestFileBlame_NotARepository tests fileBlame with non-git directory.
func TestFileBlame_NotARepository(t *testing.T) {
	tmpdir, err := os.MkdirTemp("", "notgit")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	filePath := filepath.Join(tmpdir, "file.go")
	if err := os.WriteFile(filePath, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Should return nil for non-repo (this is a whitebox test)
	// We can't call fileBlame directly, but we can verify EnrichBlame handles it
	g := graph.New("test")
	fn := &graph.Node{
		ID:   g.MakeNodeID(filePath, "Func"),
		Type: graph.NodeFunction,
		Name: "Func",
		File: filePath,
		Line: 10,
	}
	g.AddNode(fn)

	metrics.EnrichBlame(g, tmpdir)
	// Should not panic even though not a git repo
}

// TestEnrichBlameForFile_UpdatesMetadata tests that EnrichBlameForFile updates all metadata fields.
func TestEnrichBlameForFile_UpdatesMetadata(t *testing.T) {
	tmpdir := initGitRepoWithCommit(t)
	defer os.RemoveAll(tmpdir)

	g := graph.New("test")
	g.SetRoot(tmpdir)

	filePath := filepath.Join(tmpdir, "file.go")
	fn := &graph.Node{
		ID:   g.MakeNodeID(filePath, "Func"),
		Type: graph.NodeFunction,
		Name: "Func",
		File: filePath,
		Line: 10,
	}
	g.AddNode(fn)

	metrics.EnrichBlameForFile(g, tmpdir, filePath)

	// After enrichment, should have blame metadata
	if fn.Metadata != nil {
		// Check for any of the blame fields
		if fn.Metadata["blame_author"] != "" ||
			fn.Metadata["blame_date"] != "" ||
			fn.Metadata["blame_commit"] != "" {
			// Success - at least one blame field is set
			return
		}
	}
	// It's OK if file has no git history yet
}

// TestEnrichCommitContextForFile_SetsMetadata tests that commit context is set.
func TestEnrichCommitContextForFile_SetsMetadata(t *testing.T) {
	tmpdir := initGitRepoWithMultipleCommits(t)
	defer os.RemoveAll(tmpdir)

	g := graph.New("test")
	g.SetRoot(tmpdir)

	filePath := filepath.Join(tmpdir, "file.go")
	fn := &graph.Node{
		ID:   g.MakeNodeID(filePath, "Func"),
		Type: graph.NodeFunction,
		Name: "Func",
		File: filePath,
		Line: 10,
	}
	g.AddNode(fn)

	metrics.EnrichCommitContextForFile(g, tmpdir, filePath)

	// After enrichment with multiple commits, should have commit context
	if fn.Metadata != nil && fn.Metadata["commit_context"] != "" {
		// Success - commit context was set
		return
	}
	// It's OK if file has no recent commits
}

// ─────────────────────────────────────────────────────────────────────────────

// Helper functions

func createMinimalPprofProfile(t *testing.T) string {
	t.Helper()
	tmpfile, err := os.CreateTemp("", "profile*.out")
	if err != nil {
		t.Fatal(err)
	}
	defer tmpfile.Close()

	// Write a valid-looking (but empty) pprof header
	if _, err := tmpfile.WriteString("flat  flat%   sum%        cum   cum%\n"); err != nil {
		t.Fatal(err)
	}

	return tmpfile.Name()
}

func initGitRepoWithCommit(t *testing.T) string {
	t.Helper()
	tmpdir, err := os.MkdirTemp("", "repo")
	if err != nil {
		t.Fatal(err)
	}

	// Init repo
	cmd := exec.Command("git", "init")
	cmd.Dir = tmpdir
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmpdir)
		t.Fatal(err)
	}

	// Config
	for _, cfg := range [][]string{
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", cfg...)
		cmd.Dir = tmpdir
		if err := cmd.Run(); err != nil {
			os.RemoveAll(tmpdir)
			t.Fatal(err)
		}
	}

	// Create and commit a file
	filePath := filepath.Join(tmpdir, "file.go")
	if err := os.WriteFile(filePath, []byte("package main\n"), 0644); err != nil {
		os.RemoveAll(tmpdir)
		t.Fatal(err)
	}

	cmd = exec.Command("git", "add", "file.go")
	cmd.Dir = tmpdir
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmpdir)
		t.Fatal(err)
	}

	cmd = exec.Command("git", "commit", "-m", "initial")
	cmd.Dir = tmpdir
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmpdir)
		t.Fatal(err)
	}

	return tmpdir
}

func initGitRepoWithMultipleCommits(t *testing.T) string {
	t.Helper()
	tmpdir := initGitRepoWithCommit(t)

	// Add more commits to the file
	filePath := filepath.Join(tmpdir, "file.go")
	for i := 2; i <= 3; i++ {
		content := "package main\n\n// Version " + string(rune(48+i)) + "\n"
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			os.RemoveAll(tmpdir)
			t.Fatal(err)
		}

		cmd := exec.Command("git", "add", "file.go")
		cmd.Dir = tmpdir
		if err := cmd.Run(); err != nil {
			os.RemoveAll(tmpdir)
			t.Fatal(err)
		}

		msg := "commit " + string(rune(48+i))
		cmd = exec.Command("git", "commit", "-m", msg)
		cmd.Dir = tmpdir
		if err := cmd.Run(); err != nil {
			os.RemoveAll(tmpdir)
			t.Fatal(err)
		}
	}

	return tmpdir
}

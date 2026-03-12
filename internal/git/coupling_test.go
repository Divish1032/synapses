package git_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/git"
)

func TestAnalyzeChangeCoupling_NoGitRepo(t *testing.T) {
	dir := t.TempDir()
	_, err := git.AnalyzeChangeCoupling(dir, 0, 0)
	if err == nil {
		t.Error("expected error for non-git directory")
	}
}

func TestAnalyzeChangeCoupling_DefaultLimits(t *testing.T) {
	dir := t.TempDir()
	_, err := git.AnalyzeChangeCoupling(dir, 0, 0.0)
	if err == nil {
		t.Error("expected error for non-git directory")
	}
}

// initCouplingRepo creates a git repo with 5 commits that co-change fileA and fileB.
// 5 commits ensures co-changes >= 3 even if the initial commit is skipped by diff-tree.
func initCouplingRepo(t *testing.T) string {
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

	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	commits := []struct{ a, b, msg string }{
		{"func A1() {}", "func B1() {}", "commit 1"},
		{"func A2() {}", "func B2() {}", "commit 2"},
		{"func A3() {}", "func B3() {}", "commit 3"},
		{"func A4() {}", "func B4() {}", "commit 4"},
		{"func A5() {}", "func B5() {}", "commit 5"},
	}
	for _, c := range commits {
		writeFile("fileA.go", "package p\n"+c.a+"\n")
		writeFile("fileB.go", "package p\n"+c.b+"\n")
		run("git", "add", ".")
		run("git", "commit", "-m", c.msg)
	}
	return dir
}

func TestAnalyzeChangeCoupling_WithGitRepo(t *testing.T) {
	dir := initCouplingRepo(t)
	pairs, err := git.AnalyzeChangeCoupling(dir, 10, 0.3)
	if err != nil {
		t.Fatalf("AnalyzeChangeCoupling: %v", err)
	}
	if len(pairs) == 0 {
		t.Error("expected at least one coupling pair")
	}
	found := false
	for _, p := range pairs {
		if (p.FileA == "fileA.go" && p.FileB == "fileB.go") ||
			(p.FileA == "fileB.go" && p.FileB == "fileA.go") {
			found = true
			if p.CoChanges < 3 {
				t.Errorf("expected CoChanges >= 3, got %d", p.CoChanges)
			}
		}
	}
	if !found {
		t.Error("expected fileA.go and fileB.go as coupling pair")
	}
}

func TestAnalyzeChangeCoupling_HighMinConfidence(t *testing.T) {
	dir := initCouplingRepo(t)
	pairs, err := git.AnalyzeChangeCoupling(dir, 10, 1.0)
	if err != nil {
		t.Fatalf("AnalyzeChangeCoupling: %v", err)
	}
	_ = pairs
}

func TestAnalyzeChangeCoupling_SmallCommitLimit(t *testing.T) {
	dir := initCouplingRepo(t)
	pairs, err := git.AnalyzeChangeCoupling(dir, 1, 0.3)
	if err != nil {
		t.Fatalf("AnalyzeChangeCoupling: %v", err)
	}
	_ = pairs
}

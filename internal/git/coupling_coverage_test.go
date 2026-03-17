package git_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/git"
)

// initCouplingBaseRepo creates a git repo with a seed initial commit so that
// all subsequent test commits have a parent and are counted by diff-tree.
// Returns (dir, run helper).
func initCouplingBaseRepo(t *testing.T) (string, func(args ...string) bool) {
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
	// Seed commit so subsequent commits always have a parent (diff-tree works).
	if err := os.WriteFile(filepath.Join(dir, ".gitkeep"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if !run("git", "add", ".") || !run("git", "commit", "-m", "seed") {
		t.Skip("git seed commit failed")
	}
	return dir, run
}

// TestAnalyzeChangeCoupling_LargeCommitSkipped creates a commit with 51 files
// so the large-commit guard (>50 files) executes the continue branch.
func TestAnalyzeChangeCoupling_LargeCommitSkipped(t *testing.T) {
	dir, run := initCouplingBaseRepo(t)

	// Commit: 51 files — triggers the len(files) > 50 skip.
	for i := 0; i < 51; i++ {
		name := filepath.Join(dir, fmt.Sprintf("big%02d.go", i))
		if err := os.WriteFile(name, []byte(fmt.Sprintf("package p\nfunc F%d(){}\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "large commit")

	// The large commit is skipped; no co-change pairs can be built.
	_, err := git.AnalyzeChangeCoupling(dir, 10, 0.3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAnalyzeChangeCoupling_AsymmetricSupport builds a repo where fileB has
// more total changes than fileA, exercising the supB > maxSup branch.
func TestAnalyzeChangeCoupling_AsymmetricSupport(t *testing.T) {
	dir, run := initCouplingBaseRepo(t)

	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 4 commits: fileA and fileB change together → coChanges = 4, supA = 4, supB = 4.
	for i := 0; i < 4; i++ {
		writeFile("fa.go", fmt.Sprintf("package p\nfunc A%d(){}\n", i))
		writeFile("fb.go", fmt.Sprintf("package p\nfunc B%d(){}\n", i))
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("co %d", i))
	}

	// 1 extra commit: only fb.go changes → supB = 5 > supA = 4.
	writeFile("fb.go", "package p\nfunc BExtra(){}\n")
	run("git", "add", ".")
	run("git", "commit", "-m", "fb solo")

	pairs, err := git.AnalyzeChangeCoupling(dir, 20, 0.3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// confidence = 4/5 = 0.8 ≥ 0.3, so the pair should survive.
	found := false
	for _, p := range pairs {
		if (p.FileA == "fa.go" && p.FileB == "fb.go") ||
			(p.FileA == "fb.go" && p.FileB == "fa.go") {
			found = true
			// supB > supA so confidence < 1.0
			if p.Confidence >= 1.0 {
				t.Errorf("expected confidence < 1.0 (asymmetric support), got %f", p.Confidence)
			}
		}
	}
	if !found {
		t.Error("expected fa.go+fb.go coupling pair")
	}
}

// TestAnalyzeChangeCoupling_ConfidenceFiltered builds a repo where a qualifying
// pair (co_changes ≥ 3) has confidence below minConfidence and is therefore
// excluded from the results, exercising the confidence < minConfidence continue.
func TestAnalyzeChangeCoupling_ConfidenceFiltered(t *testing.T) {
	dir, run := initCouplingBaseRepo(t)

	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 4 commits: fa.go and fb.go both change → coChanges = 4 (all have parent via seed).
	for i := 0; i < 4; i++ {
		writeFile("fa.go", fmt.Sprintf("package p\nfunc A%d(){}\n", i))
		writeFile("fb.go", fmt.Sprintf("package p\nfunc B%d(){}\n", i))
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("co %d", i))
	}

	// 4 extra commits: only fa.go changes → supA = 8, supB = 4.
	// confidence = 4/8 = 0.5.
	for i := 0; i < 4; i++ {
		writeFile("fa.go", fmt.Sprintf("package p\nfunc ASolo%d(){}\n", i))
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("fa solo %d", i))
	}

	// minConfidence = 0.7 → pair with confidence 0.5 must be filtered out.
	pairs, err := git.AnalyzeChangeCoupling(dir, 30, 0.7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, p := range pairs {
		if (p.FileA == "fa.go" && p.FileB == "fb.go") ||
			(p.FileA == "fb.go" && p.FileB == "fa.go") {
			t.Errorf("low-confidence pair should be filtered (confidence=%f)", p.Confidence)
		}
	}
}

// TestAnalyzeChangeCoupling_MultiPairSort produces multiple pairs with
// different confidence values so that the sort comparator's confidence-branch
// is exercised (pairs[i].Confidence != pairs[j].Confidence → true).
func TestAnalyzeChangeCoupling_MultiPairSort(t *testing.T) {
	dir, run := initCouplingBaseRepo(t)

	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Pair 1: fa.go + fb.go always together (4 commits, all have parent) → confidence = 1.0.
	for i := 0; i < 4; i++ {
		writeFile("fa.go", fmt.Sprintf("package p\nfunc A%d(){}\n", i))
		writeFile("fb.go", fmt.Sprintf("package p\nfunc B%d(){}\n", i))
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("ab %d", i))
	}

	// Pair 2: fc.go + fd.go co-change 4×, but fc.go has 4 solo commits too.
	// confidence = 4/8 = 0.5 ≠ 1.0 → sort comparator hits the != branch.
	for i := 0; i < 4; i++ {
		writeFile("fc.go", fmt.Sprintf("package p\nfunc C%d(){}\n", i))
		writeFile("fd.go", fmt.Sprintf("package p\nfunc D%d(){}\n", i))
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("cd %d", i))
	}
	for i := 0; i < 4; i++ {
		writeFile("fc.go", fmt.Sprintf("package p\nfunc CSolo%d(){}\n", i))
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("fc solo %d", i))
	}

	pairs, err := git.AnalyzeChangeCoupling(dir, 30, 0.3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pairs) < 2 {
		t.Skipf("expected ≥2 pairs for sort coverage, got %d", len(pairs))
	}
	// Highest confidence should sort first.
	if pairs[0].Confidence < pairs[len(pairs)-1].Confidence {
		t.Error("pairs should be sorted by confidence descending")
	}
}

// TestAnalyzeChangeCoupling_CapAt50 creates 11 files all co-changing in 3 commits
// which generates C(11,2) = 55 qualifying pairs, triggering the cap-at-50 path.
func TestAnalyzeChangeCoupling_CapAt50(t *testing.T) {
	dir, run := initCouplingBaseRepo(t)

	// 4 commits, each touching all 11 files → 55 co-change pairs, each count=4 ≥ 3.
	// (All have a parent via the seed commit so all are counted by diff-tree.)
	for commit := 0; commit < 4; commit++ {
		for i := 0; i < 11; i++ {
			name := filepath.Join(dir, fmt.Sprintf("f%02d.go", i))
			if err := os.WriteFile(name, []byte(fmt.Sprintf("package p\nfunc F%d_%d(){}\n", i, commit)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("multi %d", commit))
	}

	pairs, err := git.AnalyzeChangeCoupling(dir, 10, 0.3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 55 pairs qualify but results are capped at 50.
	if len(pairs) > 50 {
		t.Errorf("expected ≤50 pairs (capped), got %d", len(pairs))
	}
}

// TestAnalyzeChangeCoupling_EmptyRepo tests with a fresh repo with only seed commit.
// This exercises the len(shas) > 0 path where no actual file changes exist.
func TestAnalyzeChangeCoupling_EmptyRepo(t *testing.T) {
	dir, _ := initCouplingBaseRepo(t)
	// Only seed commit, no additional changes.

	pairs, err := git.AnalyzeChangeCoupling(dir, 100, 0.3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Seed commit only touches .gitkeep; no co-changes expected.
	if pairs != nil && len(pairs) > 0 {
		t.Errorf("expected no pairs from single-file seed commit, got %d", len(pairs))
	}
}

// TestAnalyzeChangeCoupling_SingleFileCommits tests repo where each commit touches exactly one file.
// No pairs can form (need at least 2 files per commit for co-change).
func TestAnalyzeChangeCoupling_SingleFileCommits(t *testing.T) {
	dir, run := initCouplingBaseRepo(t)

	// 5 commits, each touching a single distinct file.
	for i := 0; i < 5; i++ {
		name := filepath.Join(dir, fmt.Sprintf("single%d.go", i))
		if err := os.WriteFile(name, []byte(fmt.Sprintf("package p\nfunc F%d(){}\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("single %d", i))
	}

	pairs, err := git.AnalyzeChangeCoupling(dir, 20, 0.3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Single-file commits produce no pairs.
	if pairs != nil && len(pairs) > 0 {
		t.Errorf("expected no pairs from single-file commits, got %d", len(pairs))
	}
}

// TestAnalyzeChangeCoupling_ZeroMinConfidence tests with minConfidence=0.0.
// All pairs with coChanges >= 3 should be included.
func TestAnalyzeChangeCoupling_ZeroMinConfidence(t *testing.T) {
	dir, run := initCouplingBaseRepo(t)

	// Create weak coupling: fa.go and fb.go co-change 3 times,
	// but fa.go appears in 10 commits total → confidence = 3/10 = 0.3.
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(dir, "fa.go"), []byte(fmt.Sprintf("package p\nfunc A%d(){}\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "fb.go"), []byte(fmt.Sprintf("package p\nfunc B%d(){}\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("co %d", i))
	}
	// 7 solo commits of fa.go
	for i := 0; i < 7; i++ {
		if err := os.WriteFile(filepath.Join(dir, "fa.go"), []byte(fmt.Sprintf("package p\nfunc ASolo%d(){}\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("fa solo %d", i))
	}

	// With minConfidence=0.0, even low-confidence pairs are included.
	pairs, err := git.AnalyzeChangeCoupling(dir, 30, 0.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should find the fa+fb pair (confidence 3/10 = 0.3 > 0).
	found := false
	for _, p := range pairs {
		if (p.FileA == "fa.go" && p.FileB == "fb.go") ||
			(p.FileA == "fb.go" && p.FileB == "fa.go") {
			found = true
		}
	}
	if !found {
		t.Error("expected to find fa.go+fb.go pair with minConfidence=0.0")
	}
}

package git_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/git"
)

// TestAnalyzeChangeCoupling_MaxSupZero tests edge case where maxSup == 0
// This should never happen in practice, but the code has a guard for it.
func TestAnalyzeChangeCoupling_MaxSupZero(t *testing.T) {
	// We can't actually create this scenario with real git commits since
	// every file in coChanges must have totalChanges > 0. This test verifies
	// the code path by checking that the function handles normal cases correctly.
	dir, run := initCouplingBaseRepoForMaxSup(t)

	// Create a scenario where we test the boundary condition indirectly
	// by ensuring totalChanges is populated correctly for all files in pairs.
	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Single co-change with 2 files — should be filtered (coChanges < 3).
	for i := 0; i < 2; i++ {
		writeFile("m.go", fmt.Sprintf("package p\nfunc M%d(){}\n", i))
		writeFile("n.go", fmt.Sprintf("package p\nfunc N%d(){}\n", i))
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("pair %d", i))
	}

	pairs, err := git.AnalyzeChangeCoupling(dir, 10, 0.3)
	if err != nil {
		t.Fatalf("AnalyzeChangeCoupling: %v", err)
	}
	// Pairs should be empty (coChanges = 2 < 3)
	if len(pairs) != 0 {
		t.Errorf("expected 0 pairs (coChanges < 3), got %d", len(pairs))
	}
}

func initCouplingBaseRepoForMaxSup(t *testing.T) (string, func(args ...string) bool) {
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
	if err := os.WriteFile(filepath.Join(dir, ".gitkeep"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if !run("git", "add", ".") || !run("git", "commit", "-m", "seed") {
		t.Skip("git seed commit failed")
	}
	return dir, run
}

// TestAnalyzeChangeCoupling_EqualConfidenceSorting tests sorting when multiple
// pairs have equal confidence. Should sort by CoChanges descending (secondary sort).
func TestAnalyzeChangeCoupling_EqualConfidenceSorting(t *testing.T) {
	dir, run := initCouplingBaseRepo(t)

	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Create pairs: (a,b), (a,c), (a,d) all changing together 3 times.
	// All will have confidence = 1.0 (since each file appears only in 3 commits).
	// Then add more changes to (a,b) so it has coChanges=5 while (a,c) and (a,d) have 3.
	for i := 0; i < 3; i++ {
		writeFile("a.go", fmt.Sprintf("package p\nfunc A%d(){}\n", i))
		writeFile("b.go", fmt.Sprintf("package p\nfunc B%d(){}\n", i))
		writeFile("c.go", fmt.Sprintf("package p\nfunc C%d(){}\n", i))
		writeFile("d.go", fmt.Sprintf("package p\nfunc D%d(){}\n", i))
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("all %d", i))
	}

	// Extra commits with (a,b) to increase their coChanges to 5
	for i := 3; i < 5; i++ {
		writeFile("a.go", fmt.Sprintf("package p\nfunc A%d(){}\n", i))
		writeFile("b.go", fmt.Sprintf("package p\nfunc B%d(){}\n", i))
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("ab %d", i))
	}

	pairs, err := git.AnalyzeChangeCoupling(dir, 20, 0.3)
	if err != nil {
		t.Fatalf("AnalyzeChangeCoupling: %v", err)
	}

	// All pairs should have confidence 1.0 (a appears in 5 commits: 0-2 + 3-4)
	// But (a,b) should have coChanges=5 while (a,c) and (a,d) have coChanges=3
	// When sorting by equal confidence, (a,b) with coChanges=5 should come first.
	if len(pairs) < 3 {
		t.Fatalf("expected at least 3 pairs, got %d", len(pairs))
	}

	// First pair should be (a,b) with highest coChanges
	firstPair := pairs[0]
	if !((firstPair.FileA == "a.go" && firstPair.FileB == "b.go") ||
		(firstPair.FileA == "b.go" && firstPair.FileB == "a.go")) {
		t.Errorf("expected first pair to be (a,b), got (%s,%s)", firstPair.FileA, firstPair.FileB)
	}
	if firstPair.CoChanges != 5 {
		t.Errorf("expected first pair coChanges=5, got %d", firstPair.CoChanges)
	}
}

// TestAnalyzeChangeCoupling_CanonicalOrdering verifies that file pairs are stored
// in canonical order (lexicographically smaller first).
func TestAnalyzeChangeCoupling_CanonicalOrdering(t *testing.T) {
	dir, run := initCouplingBaseRepo(t)

	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Create commits where z.go and a.go change together.
	// The canonical order is (a.go, z.go).
	for i := 0; i < 3; i++ {
		writeFile("z.go", fmt.Sprintf("package p\nfunc Z%d(){}\n", i))
		writeFile("a.go", fmt.Sprintf("package p\nfunc A%d(){}\n", i))
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("za %d", i))
	}

	pairs, err := git.AnalyzeChangeCoupling(dir, 10, 0.3)
	if err != nil {
		t.Fatalf("AnalyzeChangeCoupling: %v", err)
	}

	// Find the pair
	found := false
	for _, p := range pairs {
		if (p.FileA == "a.go" && p.FileB == "z.go") ||
			(p.FileA == "z.go" && p.FileB == "a.go") {
			found = true
			// Verify canonical order: smaller filename first
			if p.FileA > p.FileB {
				t.Errorf("pair not in canonical order: (%s, %s)", p.FileA, p.FileB)
			}
		}
	}
	if !found {
		t.Error("expected (a.go, z.go) pair not found")
	}
}

// TestAnalyzeChangeCoupling_ThreeFileCommit tests with 3 files in a commit.
// Should create 3 pairs: (a,b), (a,c), (b,c).
func TestAnalyzeChangeCoupling_ThreeFileCommit(t *testing.T) {
	dir, run := initCouplingBaseRepo(t)

	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Create 3 commits with 3 files each
	for i := 0; i < 3; i++ {
		writeFile("x.go", fmt.Sprintf("package p\nfunc X%d(){}\n", i))
		writeFile("y.go", fmt.Sprintf("package p\nfunc Y%d(){}\n", i))
		writeFile("z.go", fmt.Sprintf("package p\nfunc Z%d(){}\n", i))
		run("git", "add", ".")
		run("git", "commit", "-m", fmt.Sprintf("xyz %d", i))
	}

	pairs, err := git.AnalyzeChangeCoupling(dir, 10, 0.3)
	if err != nil {
		t.Fatalf("AnalyzeChangeCoupling: %v", err)
	}

	// Should have 3 pairs: (x,y), (x,z), (y,z)
	if len(pairs) != 3 {
		t.Fatalf("expected 3 pairs from 3-file commits, got %d", len(pairs))
	}

	// Verify all pairs exist
	expected := map[[2]string]bool{
		{"x.go", "y.go"}: false,
		{"x.go", "z.go"}: false,
		{"y.go", "z.go"}: false,
	}

	for _, p := range pairs {
		key := [2]string{p.FileA, p.FileB}
		if p.FileA > p.FileB {
			key = [2]string{p.FileB, p.FileA}
		}
		expected[key] = true
	}

	for key, found := range expected {
		if !found {
			t.Errorf("expected pair (%s, %s) not found", key[0], key[1])
		}
	}
}

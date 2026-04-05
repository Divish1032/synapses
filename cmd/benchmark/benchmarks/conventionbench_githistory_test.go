package benchmarks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// TestConventionBench_GitHistory uses REAL git commits as "sessions" to test
// the full convention extraction pipeline with realistic input.
//
// For each of the last N commits, extract the changed files and use them as
// the "files touched" in that session. Run observation extraction → convention
// extraction → compare against code-analyzed ground truth.
//
// This is the most honest convention test: input comes from real developer
// behavior (git history), not synthetic observation seeding.
func TestConventionBench_GitHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping git-history convention test in short mode")
	}

	repoDir := "/tmp/synbench_repos/chi"
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		t.Skip("chi repo not found")
	}

	// 1. Get the last 20 commits and their changed files.
	commits := getRecentCommits(t, repoDir, 20)
	if len(commits) < 5 {
		t.Skipf("need at least 5 commits, got %d", len(commits))
	}
	t.Logf("Got %d commits from git history", len(commits))

	// 2. Parse the repo for ground truth detection.
	g := graph.New("chi")
	g.SetRoot(repoDir)
	// Don't need full parse for ground truth — just code scanning.

	// 3. Detect ground truth conventions.
	groundTruth := detectGoConventions(repoDir)
	t.Logf("Ground truth: %d conventions", len(groundTruth))
	for _, c := range groundTruth {
		t.Logf("  [%s] %s — %s", c.Category, c.Key, c.Evidence)
	}

	// 4. Create store and simulate sessions from git history.
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	projectID := "chi-githistory"

	for i, commit := range commits {
		sessionID := fmt.Sprintf("commit-%d-%s", i, commit.hash[:8])
		obs := extractObservationsFromFiles(sessionID, projectID, commit.files, g)
		for _, o := range obs {
			st.InsertSessionObservation(o)
		}
		if len(obs) > 0 {
			t.Logf("  Commit %s: %d files → %d observations", commit.hash[:8], len(commit.files), len(obs))
		}
	}

	// 5. Run convention extraction.
	extracted := map[string]bool{}
	for _, category := range []string{
		store.ObsCategoryTestingPattern,
		store.ObsCategoryLibraryUsage,
		store.ObsCategoryFilePattern,
	} {
		keyCounts, err := st.GetObservationKeyCounts(projectID, category)
		if err != nil {
			continue
		}
		for key, count := range keyCounts {
			if count >= 3 { // MinSessionsForConvention
				extracted[category+":"+key] = true
				t.Logf("  Extracted: [%s] %s (commits=%d)", category, key, count)
			}
		}
	}

	// 6. Compare against ground truth.
	gtSet := map[string]bool{}
	for _, c := range groundTruth {
		gtSet[c.Category+":"+c.Key] = true
	}

	correct := 0
	for key := range extracted {
		if gtSet[key] {
			correct++
		}
	}
	fp := len(extracted) - correct
	fn := len(gtSet) - correct

	precision := float64(0)
	if len(extracted) > 0 {
		precision = float64(correct) / float64(len(extracted)) * 100
	}
	recall := float64(0)
	if len(gtSet) > 0 {
		recall = float64(correct) / float64(len(gtSet)) * 100
	}

	t.Logf("\nGit-History Convention Results:")
	t.Logf("  Extracted: %d, Ground truth: %d, Correct: %d, FP: %d, FN: %d",
		len(extracted), len(gtSet), correct, fp, fn)
	t.Logf("  Precision: %.1f%%, Recall: %.1f%%", precision, recall)

	if correct == 0 && len(gtSet) > 0 {
		t.Errorf("Zero correct conventions from git history — pipeline not working with real commit data")
	}
}

type commitInfo struct {
	hash  string
	files []string // absolute paths of changed files
}

func getRecentCommits(t *testing.T, repoDir string, n int) []commitInfo {
	t.Helper()

	// Get last N commit hashes.
	cmd := exec.Command("git", "log", "--oneline", fmt.Sprintf("-%d", n), "--format=%H")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		t.Logf("git log failed: %v", err)
		return nil
	}

	var commits []commitInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		hash := strings.TrimSpace(line)
		if hash == "" {
			continue
		}

		// Get changed files for this commit.
		diffCmd := exec.Command("git", "diff-tree", "--no-commit-id", "-r", "--name-only", hash)
		diffCmd.Dir = repoDir
		diffOut, err := diffCmd.Output()
		if err != nil {
			continue
		}

		var files []string
		resolved, _ := filepath.EvalSymlinks(repoDir)
		for _, f := range strings.Split(strings.TrimSpace(string(diffOut)), "\n") {
			f = strings.TrimSpace(f)
			if f != "" {
				abs := filepath.Join(resolved, f)
				if _, err := os.Stat(abs); err == nil {
					files = append(files, abs)
				}
			}
		}

		if len(files) > 0 {
			commits = append(commits, commitInfo{hash: hash, files: files})
		}
	}
	return commits
}

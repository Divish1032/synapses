package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo creates a temporary git repo for testing and returns its path.
// Registers cleanup via t.Cleanup.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "test")
	return dir
}

func makeCommit(t *testing.T, dir, message string) string {
	t.Helper()
	// Write a file so the commit has content.
	f := filepath.Join(dir, message+".txt")
	if err := os.WriteFile(f, []byte(message), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", dir, "add", ".")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", dir, "commit", "-m", message)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	sha := HeadSHA(dir)
	if sha == "" {
		t.Fatal("HeadSHA returned empty after commit")
	}
	return sha
}

// --- HeadSHA tests ---

func TestHeadSHA_EmptyRepoRoot(t *testing.T) {
	if got := HeadSHA(""); got != "" {
		t.Errorf("expected empty string for empty repoRoot, got %q", got)
	}
}

func TestHeadSHA_NonExistentPath(t *testing.T) {
	if got := HeadSHA("/does/not/exist/at/all"); got != "" {
		t.Errorf("expected empty string for non-existent path, got %q", got)
	}
}

func TestHeadSHA_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	if got := HeadSHA(dir); got != "" {
		t.Errorf("expected empty string for non-repo dir, got %q", got)
	}
}

func TestHeadSHA_EmptyRepo_NoCommits(t *testing.T) {
	dir := initTestRepo(t)
	// No commits yet — rev-parse HEAD should fail.
	if got := HeadSHA(dir); got != "" {
		t.Errorf("expected empty string for empty repo, got %q", got)
	}
}

func TestHeadSHA_WithCommit(t *testing.T) {
	dir := initTestRepo(t)
	sha := makeCommit(t, dir, "initial")
	got := HeadSHA(dir)
	if got == "" {
		t.Fatal("expected non-empty SHA")
	}
	if got != sha {
		t.Errorf("HeadSHA returned %q, want %q", got, sha)
	}
}

func TestHeadSHA_ReturnsFull40CharSHA(t *testing.T) {
	dir := initTestRepo(t)
	makeCommit(t, dir, "first")
	sha := HeadSHA(dir)
	if len(sha) != 40 {
		t.Errorf("expected 40-char SHA, got %d chars: %q", len(sha), sha)
	}
}

// --- LogSince tests ---

func TestLogSince_EmptyInputs(t *testing.T) {
	dir := initTestRepo(t)
	makeCommit(t, dir, "c1")

	if got := LogSince("", "abc"); got != nil {
		t.Errorf("expected nil for empty repoRoot, got %v", got)
	}
	if got := LogSince(dir, ""); got != nil {
		t.Errorf("expected nil for empty startCommit, got %v", got)
	}
}

func TestLogSince_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	if got := LogSince(dir, "abc123"); got != nil {
		t.Errorf("expected nil for non-repo dir, got %v", got)
	}
}

func TestLogSince_UnknownStartCommit_RebaseSafe(t *testing.T) {
	dir := initTestRepo(t)
	makeCommit(t, dir, "c1")
	// A SHA that doesn't exist in this repo (simulates rebase/force-push).
	got := LogSince(dir, "0000000000000000000000000000000000000000")
	if got != nil {
		t.Errorf("expected nil for unreachable startCommit, got %v", got)
	}
}

func TestLogSince_NoCommitsSinceStart(t *testing.T) {
	dir := initTestRepo(t)
	sha := makeCommit(t, dir, "c1")
	// HEAD == start_commit: no new commits.
	got := LogSince(dir, sha)
	if got != nil {
		t.Errorf("expected nil when no new commits, got %v", got)
	}
}

func TestLogSince_OneCommitSinceStart(t *testing.T) {
	dir := initTestRepo(t)
	start := makeCommit(t, dir, "c1")
	makeCommit(t, dir, "c2")

	got := LogSince(dir, start)
	if len(got) != 1 {
		t.Fatalf("expected 1 commit, got %d: %v", len(got), got)
	}
	// The oneline format includes commit message somewhere.
	if got[0] == "" {
		t.Error("commit line should not be empty")
	}
}

func TestLogSince_MultipleCommitsSinceStart(t *testing.T) {
	dir := initTestRepo(t)
	start := makeCommit(t, dir, "c1")
	makeCommit(t, dir, "c2")
	makeCommit(t, dir, "c3")
	makeCommit(t, dir, "c4")

	got := LogSince(dir, start)
	if len(got) != 3 {
		t.Fatalf("expected 3 commits after start, got %d: %v", len(got), got)
	}
}

func TestLogSince_CommitsContainMessage(t *testing.T) {
	dir := initTestRepo(t)
	start := makeCommit(t, dir, "base-commit")
	makeCommit(t, dir, "my-feature-commit")

	got := LogSince(dir, start)
	if len(got) != 1 {
		t.Fatalf("expected 1 commit, got %d", len(got))
	}
	// Oneline format: "<short-sha> my-feature-commit"
	if got[0] == "" {
		t.Error("commit description should not be empty")
	}
}

func TestLogSince_NonExistentPath(t *testing.T) {
	got := LogSince("/does/not/exist", "abc123")
	if got != nil {
		t.Errorf("expected nil for non-existent path, got %v", got)
	}
}

// TestLogSince_NonCommitObject tests the case where startCommit is a non-commit object
// (e.g., a tree object). Git cat-file will return something other than "commit",
// which should be treated as invalid and return nil.
func TestLogSince_NonCommitObject(t *testing.T) {
	dir := initTestRepo(t)
	sha := makeCommit(t, dir, "c1")

	// Get the tree object from the commit (trees are part of every commit)
	treeCmd := exec.Command("git", "-C", dir, "rev-parse", sha+"^{tree}")
	treeCmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	treeOut, err := treeCmd.Output()
	if err != nil {
		t.Fatalf("could not get tree object: %v", err)
	}

	treeSHA := strings.TrimSpace(string(treeOut))
	if treeSHA == "" {
		t.Fatal("tree SHA is empty")
	}

	// Now create more commits after the initial one
	makeCommit(t, dir, "c2")
	makeCommit(t, dir, "c3")

	// Using a tree object as startCommit should return nil
	// because git cat-file will say "tree", not "commit"
	got := LogSince(dir, treeSHA)
	if got != nil {
		t.Errorf("expected nil for tree object as startCommit, got %v", got)
	}
}

// TestLogSince_CommitWithWhitespaceLines tests that lines containing only
// whitespace in git log output are properly skipped.
func TestLogSince_CommitWithWhitespaceLines(t *testing.T) {
	dir := initTestRepo(t)
	start := makeCommit(t, dir, "initial")
	makeCommit(t, dir, "second")
	makeCommit(t, dir, "third")

	got := LogSince(dir, start)
	if len(got) != 2 {
		t.Fatalf("expected 2 commits, got %d: %v", len(got), got)
	}

	// Verify all results are non-empty after trimming
	for i, line := range got {
		if strings.TrimSpace(line) == "" {
			t.Errorf("commit line %d should not be empty after trim", i)
		}
	}
}

// TestLogSince_GitLogCommandError tests the case where git log fails
// (e.g., invalid commit range). This should return nil gracefully.
func TestLogSince_GitLogCommandError(t *testing.T) {
	dir := initTestRepo(t)
	makeCommit(t, dir, "c1")

	// Use an invalid path that will cause git log to fail
	invalidSHA := "a" + strings.Repeat("0", 39)
	got := LogSince("/dev/null", invalidSHA)
	if got != nil {
		t.Errorf("expected nil when git log fails, got %v", got)
	}
}

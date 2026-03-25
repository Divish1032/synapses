package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const gitCmdTimeout = 5 * time.Second

var (
	resolvedGitOnce sync.Once
	resolvedGitPath string
	resolvedGitErr  error
)

func resolveGit() (string, error) {
	resolvedGitOnce.Do(func() {
		resolvedGitPath, resolvedGitErr = exec.LookPath("git")
	})
	return resolvedGitPath, resolvedGitErr
}

// safeGitCmd creates a git command with a minimal environment to prevent
// secrets (GITHUB_TOKEN, OPENAI_API_KEY, etc.) from leaking to git hooks
// in indexed repositories. Uses the absolute path to git resolved at first
// call so that Homebrew/nix installs at non-standard PATH locations work.
func safeGitCmd(ctx context.Context, args ...string) (*exec.Cmd, error) {
	gitPath, err := resolveGit()
	if err != nil {
		return nil, fmt.Errorf("git not found: %w", err)
	}
	cmd := exec.CommandContext(ctx, gitPath, args...)
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_LOCAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
	}
	return cmd, nil
}

// HeadSHA returns the current HEAD commit SHA in the given repo.
// Returns "" if git is unavailable, repoRoot is not a git repo, or the repo
// has no commits yet. Never returns an error — callers treat "" as
// "git unavailable" and skip commit tracking gracefully.
func HeadSHA(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()
	cmd, err := safeGitCmd(ctx, "-C", repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// LogSince returns a slice of short commit descriptions (e.g. "a1b2c3d fix: typo")
// for all commits reachable from HEAD that are NOT reachable from startCommit.
// This is the set of commits made after startCommit became HEAD.
//
// Returns nil when:
//   - repoRoot or startCommit is empty
//   - git is not installed or repoRoot is not a git repo
//   - startCommit is no longer in history (rebase/force-push) — rebase-safe
//   - no commits exist since startCommit
//
// Note: commits are repo-wide. In multi-agent scenarios, commits from other
// agents working in the same repo are included. Git provides no per-agent
// isolation — this is the correct tradeoff.
func LogSince(repoRoot, startCommit string) []string {
	if repoRoot == "" || startCommit == "" {
		return nil
	}
	// Verify startCommit is still reachable before attempting the range log.
	// If not (rebase / force-push), return nil so the task update still succeeds.
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()
	catCmd, err := safeGitCmd(ctx, "-C", repoRoot, "cat-file", "-t", startCommit)
	if err != nil {
		return nil
	}
	catOut, err := catCmd.Output()
	if err != nil || strings.TrimSpace(string(catOut)) != "commit" {
		return nil
	}
	// git log <startCommit>..HEAD --oneline returns commits after startCommit.
	ctx2, cancel2 := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel2()
	logCmd, err := safeGitCmd(ctx2,
		"-C", repoRoot,
		"log", "--oneline",
		startCommit+"..HEAD",
	)
	if err != nil {
		return nil
	}
	out, err := logCmd.Output()
	if err != nil {
		return nil
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	result := make([]string, 0, len(lines))
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			result = append(result, l)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

package git

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

const gitCmdTimeout = 5 * time.Second

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
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "HEAD").Output()
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
	catOut, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "cat-file", "-t", startCommit).Output()
	if err != nil || strings.TrimSpace(string(catOut)) != "commit" {
		return nil
	}
	// git log <startCommit>..HEAD --oneline returns commits after startCommit.
	ctx2, cancel2 := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel2()
	out, err := exec.CommandContext(ctx2,
		"git", "-C", repoRoot,
		"log", "--oneline",
		startCommit+"..HEAD",
	).Output()
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

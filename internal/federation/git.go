package federation

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// gitTimeout is the per-command timeout for all git operations.
// Git commands on local repos are fast (<10ms); 500ms is generous.
const gitTimeout = 500 * time.Millisecond

// gitRevParseHead returns the current HEAD commit hash of a git repo.
// Returns ("", nil) if the path is not a git repo.
// Returns ("", err) on unexpected git failures.
func gitRevParseHead(ctx context.Context, repoPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	out, err := gitCmd(ctx, repoPath, "rev-parse", "HEAD")
	if err != nil {
		// "fatal: not a git repository" → not a git repo, not an error.
		if strings.Contains(err.Error(), "not a git repository") {
			return "", nil
		}
		return "", fmt.Errorf("rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// gitDiffNameOnly returns files changed between two commits.
// Returns (nil, nil) if oldCommit is unreachable (force push, squash merge,
// rebase) — the caller should fall back to signature comparison.
func gitDiffNameOnly(ctx context.Context, repoPath, oldCommit, newCommit string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	out, err := gitCmd(ctx, repoPath, "diff", "--name-only", oldCommit+".."+newCommit)
	if err != nil {
		// Old commit is unreachable (force push, squash merge, rebase).
		// Git reports various messages: "bad revision", "unknown revision",
		// "Invalid revision range", "bad object".
		errStr := err.Error()
		if strings.Contains(errStr, "bad revision") ||
			strings.Contains(errStr, "unknown revision") ||
			strings.Contains(errStr, "Invalid revision") ||
			strings.Contains(errStr, "bad object") {
			return nil, nil
		}
		return nil, fmt.Errorf("diff --name-only: %w", err)
	}

	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

// gitDiffFile returns the unified diff for a specific file between two commits.
// Returns ("", nil) if the file was not changed or commits are unreachable.
func gitDiffFile(ctx context.Context, repoPath, oldCommit, newCommit, filePath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	out, err := gitCmd(ctx, repoPath, "diff", oldCommit+".."+newCommit, "--", filePath)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "bad revision") ||
			strings.Contains(errStr, "unknown revision") ||
			strings.Contains(errStr, "Invalid revision") ||
			strings.Contains(errStr, "bad object") {
			return "", nil
		}
		return "", fmt.Errorf("diff file %s: %w", filePath, err)
	}
	return out, nil
}

// gitCmd runs a git command with minimal environment to prevent interactive
// prompts and credential helpers from blocking.
func gitCmd(ctx context.Context, repoPath string, args ...string) (string, error) {
	fullArgs := append([]string{"-C", repoPath}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Env = append(cmd.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
	)
	out, err := cmd.Output()
	if err != nil {
		// Include stderr in the error for diagnostics.
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("%s: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// diffTouchesEntity checks if a unified diff contains changes to a specific
// entity's signature line. Uses language-aware patterns to detect function,
// method, class, and type declarations.
//
// Only changed lines (starting with + or -) are checked, so unchanged context
// lines don't produce false positives.
func diffTouchesEntity(diff string, entityName string) bool {
	if diff == "" || entityName == "" {
		return false
	}

	// Build language-aware patterns for the entity name.
	patterns := entitySignaturePatterns(entityName)

	for _, line := range strings.Split(diff, "\n") {
		// Only inspect changed lines (added or removed).
		if len(line) == 0 {
			continue
		}
		if line[0] != '+' && line[0] != '-' {
			continue
		}
		// Skip diff headers (+++ and ---).
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}

		content := line[1:] // strip the +/- prefix
		for _, p := range patterns {
			if p.MatchString(content) {
				return true
			}
		}
	}
	return false
}

// entitySignaturePatterns returns compiled regexes that match function/method/
// class/type signature lines for the given entity name across common languages.
func entitySignaturePatterns(entityName string) []*regexp.Regexp {
	// Escape the entity name for use in regex.
	escaped := regexp.QuoteMeta(entityName)

	// If the entity is qualified (e.g. "Server.Validate"), also match
	// the unqualified part in method signatures.
	unqualified := escaped
	if idx := strings.LastIndex(entityName, "."); idx >= 0 {
		unqualified = regexp.QuoteMeta(entityName[idx+1:])
	}

	rawPatterns := []string{
		// Go: func Name(, func (r *Receiver) Name(
		`func\s+` + escaped + `\s*\(`,
		`func\s+\([^)]*\)\s*` + unqualified + `\s*\(`,
		// Go: type Name struct/interface
		`type\s+` + escaped + `\s+(struct|interface)`,

		// TypeScript/JavaScript: function Name(, export function Name(
		`function\s+` + escaped + `\s*\(`,
		`export\s+(default\s+)?function\s+` + escaped,
		// TS: export (const|let|class) Name
		`export\s+(const|let|var|class)\s+` + escaped,

		// Rust: fn name(, pub fn name(
		`(pub\s+)?fn\s+` + escaped + `\s*[\(<]`,
		// Rust: struct/enum/trait Name
		`(pub\s+)?(struct|enum|trait|impl)\s+` + escaped,

		// Python: def name(, class Name
		`def\s+` + escaped + `\s*\(`,
		`class\s+` + escaped + `[\s:(]`,
	}

	var compiled []*regexp.Regexp
	for _, p := range rawPatterns {
		if re, err := regexp.Compile(`(?i)` + p); err == nil {
			compiled = append(compiled, re)
		}
	}
	return compiled
}

// entityExistsInFile checks whether an entity name appears in a file's content
// at a signature-level position. Used as fallback when git diff is unavailable
// to determine if an entity was removed from a file.
func entityExistsInFile(ctx context.Context, repoPath, filePath, entityName string) bool {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	// Use git show HEAD:<file> to read the file content without checking it out.
	out, err := gitCmd(ctx, repoPath, "show", "HEAD:"+filePath)
	if err != nil {
		return false // file doesn't exist or git error
	}

	patterns := entitySignaturePatterns(entityName)
	for _, line := range strings.Split(out, "\n") {
		for _, p := range patterns {
			if p.MatchString(line) {
				return true
			}
		}
	}
	return false
}

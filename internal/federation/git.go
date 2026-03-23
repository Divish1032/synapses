package federation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// gitTimeoutFast is the per-command timeout for fast git metadata operations
// (rev-parse, cat-file -s). Git commands on local repos are fast (<10ms).
const gitTimeoutFast = 500 * time.Millisecond

// gitTimeoutSlow is the per-command timeout for git operations that may read
// file contents or compute diffs (diff, show). These can be slower on large repos.
const gitTimeoutSlow = 5 * time.Second

// gitRevParseHead returns the current HEAD commit hash of a git repo.
// Returns ("", nil) if the path is not a git repo.
// Returns ("", err) on unexpected git failures.
func gitRevParseHead(ctx context.Context, repoPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeoutFast)
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

// safeGitRef validates that a commit reference is safe to pass to git commands.
// Rejects strings that start with '-' (flag injection) or contain shell metacharacters.
var safeGitRef = regexp.MustCompile(`^[0-9a-zA-Z_./:~^{}-]{1,128}$`)

// gitDiffNameOnly returns files changed between two commits.
// Returns (nil, nil) if oldCommit is unreachable (force push, squash merge,
// rebase) — the caller should fall back to signature comparison.
func gitDiffNameOnly(ctx context.Context, repoPath, oldCommit, newCommit string) ([]string, error) {
	if !safeGitRef.MatchString(oldCommit) || strings.HasPrefix(oldCommit, "-") ||
		!safeGitRef.MatchString(newCommit) || strings.HasPrefix(newCommit, "-") {
		return nil, fmt.Errorf("invalid commit reference")
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeoutSlow)
	defer cancel()

	out, err := gitCmd(ctx, repoPath, "diff", "--name-only", oldCommit+".."+newCommit)
	if err != nil {
		if isUnreachableCommitErr(err) {
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
	if !safeGitRef.MatchString(oldCommit) || strings.HasPrefix(oldCommit, "-") ||
		!safeGitRef.MatchString(newCommit) || strings.HasPrefix(newCommit, "-") {
		return "", fmt.Errorf("invalid commit reference")
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeoutSlow)
	defer cancel()

	out, err := gitCmd(ctx, repoPath, "diff", oldCommit+".."+newCommit, "--", filePath)
	if err != nil {
		if isUnreachableCommitErr(err) {
			return "", nil
		}
		return "", fmt.Errorf("diff file %s: %w", filePath, err)
	}
	return out, nil
}

// isUnreachableCommitErr checks if a git error indicates the commit is
// unreachable (force push, squash merge, rebase). Git reports various
// messages across versions; we match all known variants.
func isUnreachableCommitErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "bad revision") ||
		strings.Contains(s, "unknown revision") ||
		strings.Contains(s, "Invalid revision") ||
		strings.Contains(s, "bad object")
}

// cachedGitEnvOnce guards one-time computation of cachedGitEnvSlice.
var (
	cachedGitEnvOnce  sync.Once
	cachedGitEnvSlice []string
)

// gitEnv returns a curated environment for git subprocesses. Only variables
// git needs for local operations are included — no API keys, tokens, or
// other sensitive values leak to the subprocess.
//
// The result is computed once at first call and cached — the environment
// variables git needs (PATH, HOME, TMPDIR) are stable for the daemon lifetime.
func gitEnv() []string {
	cachedGitEnvOnce.Do(func() {
		env := []string{
			"GIT_TERMINAL_PROMPT=0", // never prompt for credentials
			"GIT_ASKPASS=",          // disable credential helpers
			"SSH_ASKPASS=",          // disable SSH credential prompts
		}
		// Git needs PATH (to find itself + helpers), HOME (for ~/.gitconfig),
		// and TMPDIR (for temp files during diff).
		for _, key := range []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "LANG", "LC_ALL"} {
			if v := os.Getenv(key); v != "" {
				env = append(env, key+"="+v)
			}
		}
		cachedGitEnvSlice = env
	})
	return cachedGitEnvSlice
}

// gitCmd runs a git command with a curated environment to prevent
// interactive prompts and sensitive env var leakage.
func gitCmd(ctx context.Context, repoPath string, args ...string) (string, error) {
	fullArgs := append([]string{"-C", repoPath}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Env = gitEnv()
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

// gitCommitTime returns the author date of a specific commit as a time.Time.
// Used to compare against store SavedAt for freshness checks.
func gitCommitTime(ctx context.Context, repoPath, commitHash string) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeoutFast)
	defer cancel()

	// %aI = author date, strict ISO 8601 format
	out, err := gitCmd(ctx, repoPath, "log", "-1", "--format=%aI", commitHash)
	if err != nil {
		return time.Time{}, fmt.Errorf("commit time for %s: %w", commitHash[:min(8, len(commitHash))], err)
	}

	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("empty output for commit %s", commitHash[:min(8, len(commitHash))])
	}

	t, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse commit time %q: %w", trimmed, err)
	}
	return t, nil
}

// ── Bounded cache ────────────────────────────────────────────────────────────

// boundedCache is a generic mutex-protected map with a hard entry cap.
// When the cap is reached, the map is cleared and rebuilt from scratch.
// This prevents unbounded memory growth for caches whose key space is
// open-ended (e.g. one entry per unique entity name in the graph).
// The clear-on-full strategy is correct for deterministic caches: evicted
// entries are recomputed on next access with identical results.
type boundedCache[K comparable, V any] struct {
	mu  sync.RWMutex
	m   map[K]V
	max int
}

func newBoundedCache[K comparable, V any](max int) *boundedCache[K, V] {
	return &boundedCache[K, V]{m: make(map[K]V, max), max: max}
}

// load retrieves a value using a read lock, allowing concurrent cache reads.
func (c *boundedCache[K, V]) load(key K) (V, bool) {
	c.mu.RLock()
	v, ok := c.m[key]
	c.mu.RUnlock()
	return v, ok
}

// store adds a value under an exclusive write lock. When the map reaches max
// entries, it is cleared before the new entry is added. Evicted entries are
// recomputed on next access; the deterministic nature of all cached values
// (regex patterns) makes this safe.
func (c *boundedCache[K, V]) store(key K, val V) {
	c.mu.Lock()
	if len(c.m) >= c.max {
		c.m = make(map[K]V, c.max)
	}
	c.m[key] = val
	c.mu.Unlock()
}

// ── Pattern cache ───────────────────────────────────────────────────────────

// patternCache caches compiled regex patterns per entity name. Patterns are
// deterministic per name so the cache is correct. Bounded at 10 K entries to
// prevent unbounded growth in very large repos — evicted entries are
// recomputed on next access.
var patternCache = newBoundedCache[string, []*regexp.Regexp](10_000)

// getCachedPatterns returns compiled signature patterns for the entity name,
// using the cache to avoid recompilation on repeated lookups.
func getCachedPatterns(entityName string) []*regexp.Regexp {
	if cached, ok := patternCache.load(entityName); ok {
		return cached
	}
	patterns := compileSignaturePatterns(entityName)
	patternCache.store(entityName, patterns)
	return patterns
}

// ── Signature detection ─────────────────────────────────────────────────────

// diffTouchesEntity checks if a unified diff contains changes to a specific
// entity's signature line. Uses language-aware patterns first, then falls
// back to a generic name check on changed lines.
//
// Only changed lines (starting with + or -) are checked, so unchanged context
// lines don't produce false positives.
func diffTouchesEntity(diff string, entityName string) bool {
	if diff == "" || entityName == "" {
		return false
	}

	patterns := getCachedPatterns(entityName)

	// Extract the unqualified name for generic fallback.
	// "Server.Validate" → "Validate"
	genericName := entityName
	if idx := strings.LastIndex(entityName, "."); idx >= 0 {
		genericName = entityName[idx+1:]
	}

	for _, line := range strings.Split(diff, "\n") {
		if len(line) == 0 || (line[0] != '+' && line[0] != '-') {
			continue
		}
		// Skip diff headers (+++ and ---).
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}

		content := line[1:] // strip the +/- prefix

		// Language-specific patterns (high precision).
		for _, p := range patterns {
			if p.MatchString(content) {
				return true
			}
		}

		// Generic fallback: entity name appears in a changed line that looks
		// like a declaration (contains parentheses, braces, or colons after
		// the name — heuristic to filter out pure comments/strings).
		if strings.Contains(content, genericName) && looksLikeDeclaration(content, genericName) {
			return true
		}
	}
	return false
}

// looksLikeDeclaration checks whether a line containing entityName looks like
// a function/type/class declaration rather than a comment or string literal.
// This is the generic fallback for languages not covered by specific patterns.
func looksLikeDeclaration(line, entityName string) bool {
	idx := strings.Index(line, entityName)
	if idx < 0 {
		return false
	}
	after := line[idx+len(entityName):]
	// Declaration heuristic: something structural follows the name.
	// "func Foo(" → has (
	// "class Foo:" → has :
	// "type Foo {" → has {
	// "def foo(self):" → has (
	// "pub fn foo<T>" → has <
	for _, ch := range after {
		switch ch {
		case '(', '{', ':', '<':
			return true
		case ' ', '\t':
			continue // skip whitespace before the structural char
		default:
			return false // non-structural char — likely not a declaration
		}
	}
	return false
}

// compileSignaturePatterns returns compiled regexes that match function/method/
// class/type signature lines for the given entity name across common languages.
func compileSignaturePatterns(entityName string) []*regexp.Regexp {
	escaped := regexp.QuoteMeta(entityName)

	// If qualified (e.g. "Server.Validate"), also match the unqualified
	// part in method receiver signatures.
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

		// Java/C#/Kotlin: public/private/protected ... Name(
		`(public|private|protected|internal)\s+.*` + escaped + `\s*\(`,
	}

	var compiled []*regexp.Regexp
	for _, p := range rawPatterns {
		if re, err := regexp.Compile(`(?i)` + p); err == nil {
			compiled = append(compiled, re)
		}
	}
	return compiled
}

// entityExistsFileSizeLimit is the maximum file size (in bytes) that
// entityExistsInFile will read via git show. Files larger than this are
// typically generated or vendored — skipping them avoids 100MB+ allocations.
const entityExistsFileSizeLimit = 1 * 1024 * 1024 // 1 MB

// entityExistsInFile checks whether an entity name appears in a file's content
// at a signature-level position. Used as fallback when git diff is unavailable
// to determine if an entity was removed from a file.
func entityExistsInFile(ctx context.Context, repoPath, filePath, entityName string) bool {
	// Each git call gets its own independent timeout derived from the parent ctx.
	// A shared timeout would let the size-check call consume budget from the
	// content-read call — two separate timeout windows ensures each has the
	// full budget regardless of how fast the other completes.

	// Check file size before reading to avoid huge allocations.
	// git cat-file -s <object> prints the byte size of the object.
	sizeCtx, sizeCancel := context.WithTimeout(ctx, gitTimeoutFast)
	sizeOut, sizeErr := gitCmd(sizeCtx, repoPath, "cat-file", "-s", "HEAD:"+filePath)
	sizeCancel()
	if sizeErr == nil {
		if size, parseErr := strconv.ParseInt(strings.TrimSpace(sizeOut), 10, 64); parseErr == nil {
			if size > entityExistsFileSizeLimit {
				return false // file too large — skip to prevent excessive allocation
			}
		}
	}

	showCtx, showCancel := context.WithTimeout(ctx, gitTimeoutSlow)
	defer showCancel()
	out, err := gitCmd(showCtx, repoPath, "show", "HEAD:"+filePath)
	if err != nil {
		return false // file doesn't exist or git error
	}

	patterns := getCachedPatterns(entityName)
	genericName := entityName
	if idx := strings.LastIndex(entityName, "."); idx >= 0 {
		genericName = entityName[idx+1:]
	}

	for _, line := range strings.Split(out, "\n") {
		for _, p := range patterns {
			if p.MatchString(line) {
				return true
			}
		}
		if strings.Contains(line, genericName) && looksLikeDeclaration(line, genericName) {
			return true
		}
	}
	return false
}

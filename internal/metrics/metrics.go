// Package metrics enriches graph nodes with code health signals:
// cyclomatic complexity (computed during parsing), git churn, git blame, and test coverage.
package metrics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// gitRootForDir returns the absolute path of the git repository root that
// contains dir, or "" when dir is not inside any git repository.
// Uses 'git rev-parse --show-toplevel' — errors are silently ignored.
// The returned path has symlinks resolved (via filepath.EvalSymlinks) so that
// TrimPrefix comparisons with file paths remain accurate on platforms like
// macOS where /var/folders is a symlink to /private/var/folders.
// Callers that iterate over many files should cache results by directory
// to avoid repeated subprocess spawns.
func gitRootForDir(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	gr := strings.TrimSpace(string(out))
	if resolved, err := filepath.EvalSymlinks(gr); err == nil {
		gr = resolved
	}
	return gr
}

// EnrichChurn annotates every graph node with a "churn" metadata value
// indicating how many git commits touched the node's file in the last
// [days] days. Nodes in files with no recent commits are left unchanged.
// Errors (e.g. not a git repo) are silently ignored — the graph is still
// usable without churn data.
//
// Supports umbrella workspaces where repoRoot is not itself a git repository
// but contains sub-repos (e.g. a monorepo with separate .git in each package).
// In that case, each file's git root is detected automatically.
func EnrichChurn(g *graph.Graph, repoRoot string, days int) {
	if days <= 0 {
		days = 90
	}

	// Build per-git-root churn maps. dirToGitRoot caches git root lookups to
	// avoid one subprocess call per file. gitRootChurn maps each discovered
	// git root to its relPath→count map (nil = git call failed for that root).
	dirToGitRoot := make(map[string]string)
	gitRootChurn := make(map[string]map[string]int)

	nodes := g.AllNodes()
	for _, n := range nodes {
		if n.File == "" {
			continue
		}
		dir := filepath.Dir(n.File)
		if _, seen := dirToGitRoot[dir]; !seen {
			dirToGitRoot[dir] = gitRootForDir(dir)
		}
		gr := dirToGitRoot[dir]
		if gr == "" {
			continue
		}
		if _, loaded := gitRootChurn[gr]; !loaded {
			cm, err := fileChurn(gr, days)
			if err != nil {
				cm = nil
			}
			gitRootChurn[gr] = cm
		}
	}

	// resolvedFile caches filepath.EvalSymlinks results for node file paths.
	// This ensures TrimPrefix works when gitRootForDir returns a canonical path
	// but n.File uses a symlinked path (e.g. macOS /var → /private/var).
	resolvedFile := make(map[string]string)

	// Collect churn updates outside the graph lock.
	type churnUpdate struct {
		id    graph.NodeID
		count int
	}
	var updates []churnUpdate
	for _, n := range nodes {
		if n.File == "" {
			continue
		}
		gr := dirToGitRoot[filepath.Dir(n.File)]
		if gr == "" {
			continue
		}
		cm := gitRootChurn[gr]
		if cm == nil {
			continue
		}
		// Resolve the file path to canonical form so TrimPrefix matches the
		// canonical git root returned by gitRootForDir.
		absFile, ok := resolvedFile[n.File]
		if !ok {
			absFile = n.File
			if canon, err := filepath.EvalSymlinks(n.File); err == nil {
				absFile = canon
			}
			resolvedFile[n.File] = absFile
		}
		// churnMap keys are paths relative to the git root.
		rel := strings.TrimPrefix(absFile, gr+"/")
		count, ok2 := cm[rel]
		if !ok2 || count == 0 {
			continue
		}
		updates = append(updates, churnUpdate{id: n.ID, count: count})
	}
	// Apply metadata writes under the graph write lock to prevent concurrent
	// map read+write panics with get_context / CarveEgoGraph callers.
	for _, u := range updates {
		g.UpdateNodeMetadata(u.id, func(n *graph.Node) {
			if n.Metadata == nil {
				n.Metadata = make(map[string]string)
			}
			n.Metadata["churn"] = strconv.Itoa(u.count)
		})
	}
}

// fileChurn runs git log to count how many commits touched each file in the
// repo in the last [days] calendar days. Returns a map of relative path → count.
func fileChurn(repoRoot string, days int) (map[string]int, error) {
	since := fmt.Sprintf("--since=%d.days.ago", days)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx,
		"git", "-C", repoRoot,
		"log", since, "--name-only", "--format=",
	).Output()
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			counts[line]++
		}
	}
	return counts, nil
}

// CommitInfo holds a summary of a single git commit, as surfaced by
// get_context's recent_changes field (R34: the "why" layer).
type CommitInfo struct {
	Hash    string `json:"hash"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Message string `json:"message"`
	// Body is the first 200 characters of the commit body (everything after the
	// subject line). Empty when the commit has no body. Gives agents the "why"
	// beyond the one-line subject — e.g. "Temporary workaround — revert before prod".
	Body string `json:"body,omitempty"`
}

// RecentCommitsForFile returns the last [limit] commits that touched filePath,
// using git log relative to repoRoot. Returns nil when not a git repo or when
// no commits exist for the file. Errors are silently swallowed — caller treats
// this as an optional enrichment.
//
// Each CommitInfo includes Hash (short), Author, Date, Message (subject), and
// Body (first 200 chars of commit body, empty when none).
func RecentCommitsForFile(repoRoot, filePath string, limit int) []CommitInfo {
	if limit <= 0 {
		limit = 3
	}
	// Use \x1f (unit separator) between fields and \x1e (record separator) to
	// terminate each commit record. This handles multi-line commit bodies cleanly:
	// split by \x1e first, then by \x1f — body newlines don't interfere.
	// %H=hash %an=author %ad=date(short) %s=subject %b=body
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx,
		"git", "-C", repoRoot,
		"log", fmt.Sprintf("-n%d", limit),
		"--format=%H\x1f%an\x1f%ad\x1f%s\x1f%b\x1e",
		"--date=short",
		"--follow",
		"--", filePath,
	).Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	var commits []CommitInfo
	for _, record := range strings.Split(string(out), "\x1e") {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\x1f", 5)
		if len(parts) < 4 {
			continue
		}
		body := ""
		if len(parts) == 5 {
			// Collapse internal newlines to spaces; trim surrounding whitespace.
			body = strings.TrimSpace(strings.ReplaceAll(parts[4], "\n", " "))
			if len(body) > 200 {
				body = body[:200]
			}
		}
		hash := parts[0]
		commits = append(commits, CommitInfo{
			Hash:    hash[:min(7, len(hash))],
			Author:  parts[1],
			Date:    parts[2],
			Message: parts[3],
			Body:    body,
		})
	}
	return commits
}

// EnrichCoverage parses a go test -coverprofile output file and annotates
// function/method nodes with a "coverage" metadata value (0.0–1.0, the
// fraction of statements covered by tests). Nodes without matching coverage
// data are left unchanged. Errors are silently ignored.
func EnrichCoverage(g *graph.Graph, repoRoot, profilePath string) {
	// Resolve profile path relative to repoRoot if not absolute.
	if !filepath.IsAbs(profilePath) {
		profilePath = filepath.Join(repoRoot, profilePath)
	}

	blocks, err := parseCoverProfile(profilePath)
	if err != nil || len(blocks) == 0 {
		return
	}

	// Determine the Go module name to strip from coverage import paths.
	moduleName := readModuleName(repoRoot)

	// Build a lookup: relFilePath → sorted coverBlocks.
	fileBlocks := make(map[string][]coverBlock, len(blocks))
	for _, b := range blocks {
		rel := coverPathToRel(b.file, moduleName)
		fileBlocks[rel] = append(fileBlocks[rel], b)
	}

	// Normalize repoRoot to use forward slashes for comparison with coverage paths.
	normRepoRoot := filepath.ToSlash(repoRoot)
	prefix := normRepoRoot + "/"

	type covUpdate struct {
		id  graph.NodeID
		pct string
	}
	var covUpdates []covUpdate
	nodes := g.AllNodes()
	for _, n := range nodes {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		// Normalize file path to use forward slashes for comparison.
		normFile := filepath.ToSlash(n.File)
		rel := strings.TrimPrefix(normFile, prefix)
		blks, ok := fileBlocks[rel]
		if !ok {
			continue
		}

		// Determine function line range from metadata.
		startLine := n.Line
		endLine := startLine
		if n.Metadata != nil {
			if lc, err2 := strconv.Atoi(n.Metadata["line_count"]); err2 == nil && lc > 0 {
				endLine = startLine + lc - 1
			}
		}

		// Collect coverage blocks that overlap the function's line range.
		var totalStmts, coveredStmts int
		for _, b := range blks {
			if b.endLine < startLine || b.startLine > endLine {
				continue
			}
			totalStmts += b.numStmts
			if b.count > 0 {
				coveredStmts += b.numStmts
			}
		}
		if totalStmts == 0 {
			continue
		}

		pct := float64(coveredStmts) / float64(totalStmts)
		covUpdates = append(covUpdates, covUpdate{id: n.ID, pct: fmt.Sprintf("%.2f", pct)})
	}
	for _, u := range covUpdates {
		g.UpdateNodeMetadata(u.id, func(n *graph.Node) {
			if n.Metadata == nil {
				n.Metadata = make(map[string]string)
			}
			n.Metadata["coverage"] = u.pct
		})
	}
}

// coverBlock is one line from a go test -coverprofile output.
type coverBlock struct {
	file      string // import-path form, e.g. "github.com/foo/bar/pkg/file.go"
	startLine int
	endLine   int
	numStmts  int
	count     int // >0 = covered
}

// parseCoverProfile reads a go test -coverprofile file and returns all blocks.
// Format per line: "file:startLine.startCol,endLine.endCol numStmts count"
func parseCoverProfile(path string) ([]coverBlock, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var blocks []coverBlock
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		// "path/file.go:10.5,20.15 3 1"
		// Split at last two spaces first to get numStmts and count.
		parts := strings.Fields(line)
		if len(parts) != 3 {
			continue
		}
		numStmts, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		count, err := strconv.Atoi(parts[2])
		if err != nil {
			continue
		}

		// "path/file.go:10.5,20.15"
		loc := parts[0]
		colonIdx := strings.LastIndex(loc, ":")
		if colonIdx < 0 {
			continue
		}
		filePart := loc[:colonIdx]
		rangePart := loc[colonIdx+1:] // "10.5,20.15"

		comma := strings.Index(rangePart, ",")
		if comma < 0 {
			continue
		}
		startStr := rangePart[:comma] // "10.5"
		endStr := rangePart[comma+1:] // "20.15"
		startLine, _ := strconv.Atoi(strings.SplitN(startStr, ".", 2)[0])
		endLine, _ := strconv.Atoi(strings.SplitN(endStr, ".", 2)[0])

		blocks = append(blocks, coverBlock{
			file:      filePart,
			startLine: startLine,
			endLine:   endLine,
			numStmts:  numStmts,
			count:     count,
		})
	}
	return blocks, sc.Err()
}

// coverPathToRel converts a coverage import path to a repo-relative file path.
// e.g. "github.com/foo/bar/internal/pkg/file.go" → "internal/pkg/file.go"
// when moduleName = "github.com/foo/bar".
func coverPathToRel(importPath, moduleName string) string {
	if moduleName != "" {
		rel := strings.TrimPrefix(importPath, moduleName+"/")
		if rel != importPath {
			return rel
		}
	}
	// Fallback: strip until the second-to-last path segment looks like a
	// standard Go module prefix. Heuristic: strip up to the first component
	// that doesn't look like a domain (no dot in it).
	parts := strings.Split(importPath, "/")
	for i, p := range parts {
		if !strings.Contains(p, ".") {
			return strings.Join(parts[i:], "/")
		}
	}
	return importPath
}

// readModuleName extracts the module name from go.mod in repoRoot.
// Returns "" if go.mod is absent or unreadable.
func readModuleName(repoRoot string) string {
	data, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// EnrichPprof parses a Go pprof CPU profile and annotates function/method nodes
// with metadata["cpu_pct"] — the flat CPU percentage contributed by that function.
// Uses `go tool pprof -top` (standard with every Go installation); errors are
// silently ignored so the graph remains usable without pprof data.
func EnrichPprof(g *graph.Graph, repoRoot, profilePath string) {
	if !filepath.IsAbs(profilePath) {
		profilePath = filepath.Join(repoRoot, profilePath)
	}

	samples, err := parsePprofTop(profilePath)
	if err != nil || len(samples) == 0 {
		return
	}

	// Build name → nodes lookup for O(1) matching.
	nameToNodes := make(map[string][]*graph.Node, g.NodeCount())
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeFunction || n.Type == graph.NodeMethod {
			nameToNodes[n.Name] = append(nameToNodes[n.Name], n)
		}
	}

	type pprofUpdate struct {
		id  graph.NodeID
		val string
	}
	var pprofUpdates []pprofUpdate
	for rawName, pct := range samples {
		shortName := pprofShortName(rawName)
		nodes, ok := nameToNodes[shortName]
		if !ok {
			continue
		}
		val := fmt.Sprintf("%.2f", pct)
		for _, n := range nodes {
			pprofUpdates = append(pprofUpdates, pprofUpdate{id: n.ID, val: val})
		}
	}
	for _, u := range pprofUpdates {
		g.UpdateNodeMetadata(u.id, func(n *graph.Node) {
			if n.Metadata == nil {
				n.Metadata = make(map[string]string)
			}
			n.Metadata["cpu_pct"] = u.val
		})
	}
}

// parsePprofTop runs "go tool pprof -top -nodecount=100000 <profile>" and
// returns a map of raw pprof function name → flat CPU percentage.
func parsePprofTop(profilePath string) (map[string]float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx,
		"go", "tool", "pprof", "-top", "-nodecount=100000", profilePath,
	).Output()
	if err != nil {
		return nil, err
	}

	result := make(map[string]float64)
	sc := bufio.NewScanner(bytes.NewReader(out))
	inTable := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// The header line that precedes data rows starts with "flat".
		if strings.HasPrefix(line, "flat") {
			inTable = true
			continue
		}
		if !inTable || line == "" {
			continue
		}
		// Data row format:
		//   "480ms 19.12% 35.86%  480ms 19.12%  github.com/foo/bar.(*T).Method"
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		pctStr := strings.TrimSuffix(fields[1], "%")
		pct, err := strconv.ParseFloat(pctStr, 64)
		if err != nil {
			continue
		}
		result[fields[5]] = pct
	}
	return result, sc.Err()
}

// blameResult holds the most-recent git commit that touched a file.
type blameResult struct {
	Author  string
	Date    string // ISO short: "2025-01-15"
	Commit  string // 7-char short hash
	Subject string
}

// fileBlame runs git log -1 to get the most-recent commit that touched absFile,
// relative to repoRoot. Returns nil when git is unavailable or the file has no commits.
func fileBlame(repoRoot, absFile string) *blameResult {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx,
		"git", "-C", repoRoot,
		"log", "-1",
		"--format=%an\x1f%ad\x1f%H\x1f%s",
		"--date=short",
		"--follow",
		"--", absFile,
	).Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	line := strings.TrimSpace(string(out))
	parts := strings.SplitN(line, "\x1f", 4)
	if len(parts) != 4 {
		return nil
	}
	hash := parts[2]
	if len(hash) > 7 {
		hash = hash[:7]
	}
	return &blameResult{
		Author:  parts[0],
		Date:    parts[1],
		Commit:  hash,
		Subject: parts[3],
	}
}

// fileChurnCount returns the number of commits that touched absFile in the last
// [days] calendar days, relative to repoRoot. Returns 0 when git is unavailable,
// the file has no commits in the window, or the 5-second timeout expires.
// Callers should hold no locks — this spawns a subprocess.
func fileChurnCount(repoRoot, absFile string, days int) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx,
		"git", "-C", repoRoot,
		"log", fmt.Sprintf("--since=%d.days.ago", days),
		"--follow", "--oneline",
		"--", absFile,
	).Output()
	if err != nil || len(out) == 0 {
		return 0
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

// BlameAgeLabel converts an ISO short date ("2025-01-15") to a human-readable age
// relative to now ("3d ago", "1w ago", "2mo ago", "1y ago").
// Returns "" if the date cannot be parsed.
func BlameAgeLabel(dateStr string) string {
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return ""
	}
	days := int(time.Since(t).Hours() / 24)
	switch {
	case days < 1:
		return "today"
	case days == 1:
		return "1d ago"
	case days < 7:
		return fmt.Sprintf("%dd ago", days)
	case days < 30:
		if weeks := days / 7; weeks == 1 {
			return "1w ago"
		} else {
			return fmt.Sprintf("%dw ago", weeks)
		}
	case days < 365:
		if months := days / 30; months == 1 {
			return "1mo ago"
		} else {
			return fmt.Sprintf("%dmo ago", months)
		}
	default:
		if years := days / 365; years == 1 {
			return "1y ago"
		} else {
			return fmt.Sprintf("%dy ago", years)
		}
	}
}

// StalenessLabel converts a numeric staleness score to a qualitative label.
// low: < 30, medium: 30–149, high: ≥ 150.
func StalenessLabel(score float64) string {
	switch {
	case score < 30:
		return "low"
	case score < 150:
		return "medium"
	default:
		return "high"
	}
}

// EnrichBlame annotates every function/method node with git blame metadata:
// blame_author, blame_date, blame_commit, blame_subject, and staleness_score.
// Uses per-file granularity (one git log call per unique file) for performance.
// Nodes in vendored/generated paths are skipped.
// Must be called after EnrichChurn — staleness_score reads metadata["churn"].
// Git errors are silently ignored; the graph remains usable without blame data.
func EnrichBlame(g *graph.Graph, repoRoot string) {
	// dirToGitRoot caches git root lookups to avoid one subprocess per file.
	// Supports umbrella workspaces where repoRoot has no .git but sub-dirs do.
	dirToGitRoot := make(map[string]string)

	// Phase 1: collect unique files that need blame data and resolve git roots.
	// This is sequential because dirToGitRoot is a shared cache.
	type fileJob struct {
		absFile string
		gitRoot string
	}
	seenFiles := make(map[string]bool)
	var jobs []fileJob
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		if n.Provenance == graph.ProvenanceVendored || n.Provenance == graph.ProvenanceGenerated {
			continue
		}
		if n.File == "" || seenFiles[n.File] {
			continue
		}
		seenFiles[n.File] = true

		dir := filepath.Dir(n.File)
		gr, grSeen := dirToGitRoot[dir]
		if !grSeen {
			gr = gitRootForDir(dir)
			dirToGitRoot[dir] = gr
		}
		if gr == "" {
			gr = repoRoot
		}
		jobs = append(jobs, fileJob{absFile: n.File, gitRoot: gr})
	}

	// Phase 2: run fileBlame in parallel with bounded worker pool (4 concurrent
	// git subprocesses). This is the expensive I/O-bound phase — parallelism
	// gives ~4x speedup on repos with hundreds of unique files.
	cache := make(map[string]*blameResult, len(jobs))
	var cacheMu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // concurrency limit

	for _, j := range jobs {
		wg.Add(1)
		go func(f fileJob) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			bi := fileBlame(f.gitRoot, f.absFile)
			<-sem                    // release
			cacheMu.Lock()
			cache[f.absFile] = bi
			cacheMu.Unlock()
		}(j)
	}
	wg.Wait()

	// Phase 3: assemble blame updates from cache (sequential, no git calls).
	type blameUpdate struct {
		id             graph.NodeID
		author, date   string
		commit, subj   string
		stalenessScore string
	}
	var blameUpdates []blameUpdate
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		if n.Provenance == graph.ProvenanceVendored || n.Provenance == graph.ProvenanceGenerated {
			continue
		}
		bi := cache[n.File]
		if bi == nil {
			continue
		}

		daysAgo := 0.0
		if t, err := time.Parse("2006-01-02", bi.Date); err == nil {
			daysAgo = time.Since(t).Hours() / 24
		}
		churn := 0.0
		if n.Metadata != nil {
			if c, err := strconv.ParseFloat(n.Metadata["churn"], 64); err == nil {
				churn = c
			}
		}
		score := daysAgo * math.Log(1+churn)

		blameUpdates = append(blameUpdates, blameUpdate{
			id: n.ID, author: bi.Author, date: bi.Date,
			commit: bi.Commit, subj: bi.Subject,
			stalenessScore: fmt.Sprintf("%.1f", score),
		})
	}
	for _, u := range blameUpdates {
		g.UpdateNodeMetadata(u.id, func(n *graph.Node) {
			if n.Metadata == nil {
				n.Metadata = make(map[string]string)
			}
			n.Metadata["blame_author"] = u.author
			n.Metadata["blame_date"] = u.date
			n.Metadata["blame_commit"] = u.commit
			n.Metadata["blame_subject"] = u.subj
			n.Metadata["staleness_score"] = u.stalenessScore
		})
	}
}

// EnrichBlameForFile updates blame and churn metadata for all function/method
// nodes in a single file. Called by the watcher after a file is re-parsed to
// keep blame data current without a full re-blame of the entire graph.
// It is a no-op when git is unavailable, when the file has no commits, or when
// no function/method nodes exist in the file.
//
// Unlike EnrichBlame (startup batch), this also computes per-file churn so that
// staleness_score is accurate even for nodes that were just created by an
// incremental reparse (which would otherwise have no churn metadata from the
// startup EnrichChurn scan).
func EnrichBlameForFile(g *graph.Graph, repoRoot, absFile string) {
	// Phase 1: all git I/O — no lock held. Subprocess calls can take 10–100 ms
	// and must never block the graph's read lock or any other MCP caller.

	// Detect the actual git root for this file — repoRoot may be an umbrella
	// workspace directory that is not itself a git repository.
	gr := gitRootForDir(filepath.Dir(absFile))
	if gr == "" {
		gr = repoRoot // fall back; fileBlame handles git errors silently
	}
	bi := fileBlame(gr, absFile)
	if bi == nil {
		// File has no git history yet (new uncommitted file) or git unavailable.
		// Not an error — nodes simply carry no blame metadata.
		return
	}

	// Pre-compute the date-based staleness component (pure arithmetic, no lock).
	// Graceful: unparseable date → 0 days → staleness_score treated as fresh.
	daysAgo := 0.0
	if t, err := time.Parse("2006-01-02", bi.Date); err == nil {
		daysAgo = time.Since(t).Hours() / 24
	}

	// Compute per-file churn outside the lock so staleness_score is accurate
	// for nodes created by incremental reparse (which have no startup churn).
	churnCount := fileChurnCount(gr, absFile, 90)

	// Phase 2: metadata writes — held under the graph write lock to prevent a
	// concurrent map read+write panic with get_context / CarveEgoGraph callers.
	// The lock is held only for in-memory writes (microseconds); all git I/O is
	// already complete above.
	g.UpdateFileNodeMetadata(absFile, func(n *graph.Node) {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			return
		}
		if n.Provenance == graph.ProvenanceVendored || n.Provenance == graph.ProvenanceGenerated {
			return
		}
		if n.Metadata == nil {
			n.Metadata = make(map[string]string)
		}
		n.Metadata["blame_author"] = bi.Author
		n.Metadata["blame_date"] = bi.Date
		n.Metadata["blame_commit"] = bi.Commit
		n.Metadata["blame_subject"] = bi.Subject

		// Write churn and staleness_score together so they are always consistent
		// with each other. This overrides any startup EnrichChurn value for this
		// file with a freshly computed count — acceptable since both use the same
		// 90-day window and the fresh count is more accurate post-edit.
		n.Metadata["churn"] = strconv.Itoa(churnCount)
		score := daysAgo * math.Log(1+float64(churnCount))
		n.Metadata["staleness_score"] = fmt.Sprintf("%.1f", score)
	})
}

// EnrichCommitContext annotates every function/method node with the last 3
// commit messages that touched its file. The subjects are stored as a JSON
// array in metadata["commit_context"] so they can be parsed and rendered at
// query time without a git subprocess.
//
// Uses per-file granularity (one git log call per unique file, same as
// EnrichBlame) for performance. Nodes in vendored/generated paths are skipped.
// Git errors are silently ignored — the graph remains usable without commit data.
func EnrichCommitContext(g *graph.Graph, repoRoot string) {
	// Phase 1: collect unique files and resolve git roots (sequential — shared cache).
	type fileJob struct {
		absFile string
		gitRoot string
	}
	seen := make(map[string]bool)
	dirToGitRoot := make(map[string]string)
	var jobs []fileJob

	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		if n.Provenance == graph.ProvenanceVendored || n.Provenance == graph.ProvenanceGenerated {
			continue
		}
		absFile := n.File
		if seen[absFile] {
			continue
		}
		seen[absFile] = true
		dir := filepath.Dir(absFile)
		gr, grSeen := dirToGitRoot[dir]
		if !grSeen {
			gr = gitRootForDir(dir)
			if gr == "" {
				gr = repoRoot
			}
			dirToGitRoot[dir] = gr
		}
		jobs = append(jobs, fileJob{absFile: absFile, gitRoot: gr})
	}

	// Phase 2: run RecentCommitsForFile in parallel with bounded worker pool.
	cache := make(map[string]string, len(jobs))
	var cacheMu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // concurrency limit

	for _, j := range jobs {
		wg.Add(1)
		go func(f fileJob) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			commits := RecentCommitsForFile(f.gitRoot, f.absFile, 3)
			<-sem                    // release
			if len(commits) > 0 {
				if raw, err := json.Marshal(commits); err == nil {
					cacheMu.Lock()
					cache[f.absFile] = string(raw)
					cacheMu.Unlock()
				}
			}
		}(j)
	}
	wg.Wait()

	// Phase 3: apply commit context metadata (sequential).
	type ccUpdate struct {
		id  graph.NodeID
		raw string
	}
	var ccUpdates []ccUpdate
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		if n.Provenance == graph.ProvenanceVendored || n.Provenance == graph.ProvenanceGenerated {
			continue
		}
		raw := cache[n.File]
		if raw == "" {
			continue
		}
		ccUpdates = append(ccUpdates, ccUpdate{id: n.ID, raw: raw})
	}
	for _, u := range ccUpdates {
		g.UpdateNodeMetadata(u.id, func(n *graph.Node) {
			if n.Metadata == nil {
				n.Metadata = make(map[string]string)
			}
			n.Metadata["commit_context"] = u.raw
		})
	}
}

// EnrichCommitContextForFile updates commit context metadata for all
// function/method nodes in a single file. Called by the watcher after a file
// is re-parsed to keep commit data current without re-enriching the full graph.
// It is a no-op when git is unavailable, when the file has no commits, or when
// no function/method nodes exist in the file.
func EnrichCommitContextForFile(g *graph.Graph, repoRoot, absFile string) {
	// Phase 1: git I/O — no lock held.

	// Detect the actual git root for this file — repoRoot may be an umbrella
	// workspace directory that is not itself a git repository.
	gr := gitRootForDir(filepath.Dir(absFile))
	if gr == "" {
		gr = repoRoot // fall back; RecentCommitsForFile handles git errors silently
	}
	commits := RecentCommitsForFile(gr, absFile, 3)
	if len(commits) == 0 {
		// No commits yet or git unavailable — not an error, nodes carry no
		// commit_context metadata.
		return
	}
	raw, err := json.Marshal(commits)
	if err != nil {
		return
	}
	encoded := string(raw)

	// Phase 2: metadata writes — under graph write lock to prevent concurrent
	// map read+write panic. Lock is held only for in-memory writes (microseconds).
	g.UpdateFileNodeMetadata(absFile, func(n *graph.Node) {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			return
		}
		if n.Provenance == graph.ProvenanceVendored || n.Provenance == graph.ProvenanceGenerated {
			return
		}
		if n.Metadata == nil {
			n.Metadata = make(map[string]string)
		}
		n.Metadata["commit_context"] = encoded
	})
}

// pprofShortName converts a fully-qualified pprof function name to the short
// name used in graph nodes.
//
//	"github.com/foo/bar/pkg.(*Graph).AddEdge"  →  "Graph.AddEdge"
//	"github.com/foo/bar/pkg.FuncName"          →  "FuncName"
//	"runtime.mallocgc"                          →  "mallocgc"
func pprofShortName(s string) string {
	// Drop everything up to and including the last '/'.
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		s = s[idx+1:]
	}
	// s is now like "pkg.(*Graph).AddEdge" or "pkg.FuncName".
	// Drop the package prefix (up to and including the first '.').
	if dot := strings.Index(s, "."); dot >= 0 {
		s = s[dot+1:]
	}
	// Strip pointer-receiver syntax: "(*Graph).AddEdge" → "Graph.AddEdge".
	s = strings.ReplaceAll(s, "(*", "")
	s = strings.ReplaceAll(s, ")", "")
	return s
}

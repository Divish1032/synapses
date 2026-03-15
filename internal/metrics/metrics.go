// Package metrics enriches graph nodes with code health signals:
// cyclomatic complexity (computed during parsing), git churn, git blame, and test coverage.
package metrics

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// EnrichChurn annotates every graph node with a "churn" metadata value
// indicating how many git commits touched the node's file in the last
// [days] days. Nodes in files with no recent commits are left unchanged.
// Errors (e.g. not a git repo) are silently ignored — the graph is still
// usable without churn data.
func EnrichChurn(g *graph.Graph, repoRoot string, days int) {
	if days <= 0 {
		days = 90
	}
	churnMap, err := fileChurn(repoRoot, days)
	if err != nil || len(churnMap) == 0 {
		return
	}

	prefix := repoRoot + "/"
	nodes := g.AllNodes()
	for _, n := range nodes {
		rel := strings.TrimPrefix(n.File, prefix)
		count, ok := churnMap[rel]
		if !ok || count == 0 {
			continue
		}
		if n.Metadata == nil {
			n.Metadata = make(map[string]string)
		}
		n.Metadata["churn"] = strconv.Itoa(count)
	}
}

// fileChurn runs git log to count how many commits touched each file in the
// repo in the last [days] calendar days. Returns a map of relative path → count.
func fileChurn(repoRoot string, days int) (map[string]int, error) {
	since := fmt.Sprintf("--since=%d.days.ago", days)
	out, err := exec.Command(
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
// get_context's recent_changes field (GAP-7: the "why" layer).
type CommitInfo struct {
	Hash    string `json:"hash"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Message string `json:"message"`
}

// RecentCommitsForFile returns the last [limit] commits that touched filePath,
// using git log relative to repoRoot. Returns nil when not a git repo or when
// no commits exist for the file. Errors are silently swallowed — caller treats
// this as an optional enrichment.
func RecentCommitsForFile(repoRoot, filePath string, limit int) []CommitInfo {
	if limit <= 0 {
		limit = 3
	}
	// git log: %H=hash %an=author name %ad=author date (short) %s=subject
	// --follow tracks renames; -- separates the file path from flags.
	out, err := exec.Command(
		"git", "-C", repoRoot,
		"log", fmt.Sprintf("-n%d", limit),
		"--format=%H\x1f%an\x1f%ad\x1f%s",
		"--date=short",
		"--follow",
		"--", filePath,
	).Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	var commits []CommitInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "\x1f", 4)
		if len(parts) != 4 {
			continue
		}
		commits = append(commits, CommitInfo{
			Hash:    parts[0][:min(7, len(parts[0]))], // short hash
			Author:  parts[1],
			Date:    parts[2],
			Message: parts[3],
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
		if n.Metadata == nil {
			n.Metadata = make(map[string]string)
		}
		n.Metadata["coverage"] = fmt.Sprintf("%.2f", pct)
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

	for rawName, pct := range samples {
		shortName := pprofShortName(rawName)
		nodes, ok := nameToNodes[shortName]
		if !ok {
			continue
		}
		val := fmt.Sprintf("%.2f", pct)
		for _, n := range nodes {
			if n.Metadata == nil {
				n.Metadata = make(map[string]string)
			}
			n.Metadata["cpu_pct"] = val
		}
	}
}

// parsePprofTop runs "go tool pprof -top -nodecount=100000 <profile>" and
// returns a map of raw pprof function name → flat CPU percentage.
func parsePprofTop(profilePath string) (map[string]float64, error) {
	out, err := exec.Command(
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
	out, err := exec.Command(
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
	// Per-file blame cache: absFile → *blameResult (nil = no git data for file).
	cache := make(map[string]*blameResult)

	for _, n := range g.AllNodes() {
		// Only function/method nodes carry meaningful blame.
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		// Skip vendored/generated code — blame is not actionable.
		if n.Provenance == graph.ProvenanceVendored || n.Provenance == graph.ProvenanceGenerated {
			continue
		}

		absFile := n.File
		bi, seen := cache[absFile]
		if !seen {
			bi = fileBlame(repoRoot, absFile)
			cache[absFile] = bi
		}
		if bi == nil {
			continue
		}

		if n.Metadata == nil {
			n.Metadata = make(map[string]string)
		}
		n.Metadata["blame_author"] = bi.Author
		n.Metadata["blame_date"] = bi.Date
		n.Metadata["blame_commit"] = bi.Commit
		n.Metadata["blame_subject"] = bi.Subject

		// staleness_score = days_since_change × log(1 + churn).
		// churn is set by EnrichChurn (must run first).
		daysAgo := 0.0
		if t, err := time.Parse("2006-01-02", bi.Date); err == nil {
			daysAgo = time.Since(t).Hours() / 24
		}
		churn := 0.0
		if c, err := strconv.ParseFloat(n.Metadata["churn"], 64); err == nil {
			churn = c
		}
		score := daysAgo * math.Log(1+churn)
		n.Metadata["staleness_score"] = fmt.Sprintf("%.1f", score)
	}
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

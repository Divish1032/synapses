// GraphBench — Graph Accuracy Benchmark (Benchmark A).
//
// Tests whether Synapses' structural graph correctly represents code
// relationships. Unlike ContextBench (which conflates graph quality with
// retrieval strategy), GraphBench isolates graph correctness.
//
// Query types:
//   - find_callers(symbol)        — who calls this? via get_impact depth=1
//   - find_callees(symbol)        — what does this call? via get_context
//   - find_imports(file)          — what does this file import? via get_context
//   - impact_analysis(symbol)     — what's affected? via get_impact depth=3
//   - find_implementations(iface) — who implements this? via get_context
//
// Metrics: per-test Precision, Recall, F1. Aggregated by query_type and language.
package benchmarks

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/cmd/benchmark/agent"
	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

// ─── Data types ──────────────────────────────────────────────────────────────

// GraphBenchOptions controls a GraphBench run.
type GraphBenchOptions struct {
	DataFile string // path to graphbench.jsonl
	ReposDir string // where repos are cloned
	Limit    int    // max test suites (0 = all)
}

// GraphBenchSuite is one line from the JSONL file.
type GraphBenchSuite struct {
	Repo     string           `json:"repo"`
	Commit   string           `json:"commit"`
	Language string           `json:"language"`
	Tests    []GraphBenchTest `json:"tests"`
}

// GraphBenchTest is a single query+expected pair.
type GraphBenchTest struct {
	QueryType     string   `json:"query_type"`
	Query         string   `json:"query"`
	ExpectedNames []string `json:"expected_names,omitempty"`
	ExpectedFiles []string `json:"expected_files,omitempty"`
}

// GraphBenchTestResult holds the outcome of one test.
type GraphBenchTestResult struct {
	QueryType     string   `json:"query_type"`
	Query         string   `json:"query"`
	ExpectedNames []string `json:"expected_names,omitempty"`
	ExpectedFiles []string `json:"expected_files,omitempty"`
	ActualNames   []string `json:"actual_names"`
	ActualFiles   []string `json:"actual_files"`
	Precision     float64  `json:"precision"`
	Recall        float64  `json:"recall"`
	F1            float64  `json:"f1"`
	Error         string   `json:"error,omitempty"`
}

// ─── Runner ──────────────────────────────────────────────────────────────────

// RunGraphBench runs the full benchmark and returns a reporter-compatible result.
func RunGraphBench(client *agent.SynapsesClient, opts GraphBenchOptions) (*reporter.GraphBenchResult, error) {
	suites, err := loadGraphBenchData(opts.DataFile)
	if err != nil {
		return nil, fmt.Errorf("load data: %w", err)
	}
	if opts.Limit > 0 && len(suites) > opts.Limit {
		suites = suites[:opts.Limit]
	}

	log.Printf("graphbench: %d repo suites loaded", len(suites))

	var allResults []GraphBenchTestResult

	for i, suite := range suites {
		log.Printf("[%d/%d] %s @ %.8s (%s, %d tests)",
			i+1, len(suites), suite.Repo, suite.Commit, suite.Language, len(suite.Tests))

		repoDir, err := ensureRepo(opts.ReposDir, suite.Repo, suite.Commit)
		if err != nil {
			log.Printf("  SKIP: clone/checkout failed: %v", err)
			for _, t := range suite.Tests {
				allResults = append(allResults, GraphBenchTestResult{
					QueryType: t.QueryType,
					Query:     t.Query,
					Error:     fmt.Sprintf("repo setup: %v", err),
				})
			}
			continue
		}

		// Trigger indexing by calling search on the project.
		projClient := client.WithProject(repoDir)
		log.Printf("  indexing via search probe...")
		if _, err := projClient.Search("graphbench-index", "main"); err != nil {
			log.Printf("  warning: index probe failed: %v", err)
		}
		// Give daemon a moment to finish background indexing.
		time.Sleep(3 * time.Second)

		for j, test := range suite.Tests {
			result := runGraphTest(projClient, suite, test)
			allResults = append(allResults, result)
			status := "✓"
			if result.Error != "" {
				status = "✗ " + result.Error
			}
			log.Printf("  [%d/%d] %s(%s): P=%.0f%% R=%.0f%% F1=%.0f%% %s",
				j+1, len(suite.Tests), test.QueryType, test.Query,
				result.Precision*100, result.Recall*100, result.F1*100, status)
		}
	}

	return aggregateGraphResults(allResults, suites), nil
}

// runGraphTest executes a single test case against the daemon.
func runGraphTest(client *agent.SynapsesClient, suite GraphBenchSuite, test GraphBenchTest) GraphBenchTestResult {
	result := GraphBenchTestResult{
		QueryType:     test.QueryType,
		Query:         test.Query,
		ExpectedNames: test.ExpectedNames,
		ExpectedFiles: test.ExpectedFiles,
	}

	var rawText string

	switch test.QueryType {
	case "find_callers":
		// get_impact with depth=1 returns direct callers.
		resp, e := client.GetImpactWithDepth("graphbench", test.Query, 1)
		if e != nil {
			result.Error = e.Error()
			return result
		}
		rawText = resp.Text

	case "find_callees":
		// get_context returns callees in the response.
		resp, e := client.PrepareContext("graphbench", test.Query, "understand")
		if e != nil {
			result.Error = e.Error()
			return result
		}
		rawText = resp.Text

	case "find_imports":
		// get_context on a file shows its imports.
		resp, e := client.PrepareContext("graphbench", test.Query, "understand")
		if e != nil {
			result.Error = e.Error()
			return result
		}
		rawText = resp.Text

	case "impact_analysis":
		// get_impact with depth=3 for transitive impact.
		resp, e := client.GetImpactWithDepth("graphbench", test.Query, 3)
		if e != nil {
			result.Error = e.Error()
			return result
		}
		rawText = resp.Text

	case "find_implementations":
		// get_context on interface returns implementations.
		resp, e := client.PrepareContext("graphbench", test.Query, "understand")
		if e != nil {
			result.Error = e.Error()
			return result
		}
		rawText = resp.Text

	default:
		result.Error = fmt.Sprintf("unknown query_type %q", test.QueryType)
		return result
	}

	// Extract names and files from the response text.
	result.ActualNames = extractNamesFromResponse(rawText)
	result.ActualFiles = extractFilesFromResponse(rawText)

	// Compute metrics: check expected_names against actual_names,
	// and expected_files against actual_files. Combine both sets.
	hits, total := 0, 0
	if len(test.ExpectedNames) > 0 {
		h, t := setOverlap(test.ExpectedNames, result.ActualNames)
		hits += h
		total += t
	}
	if len(test.ExpectedFiles) > 0 {
		h, t := setOverlap(test.ExpectedFiles, result.ActualFiles)
		hits += h
		total += t
	}

	if total > 0 {
		result.Recall = float64(hits) / float64(total)
	}

	// Precision: of all things returned, how many were expected?
	actualTotal := len(result.ActualNames) + len(result.ActualFiles)
	if actualTotal > 0 {
		expectedSet := make(map[string]bool)
		for _, n := range test.ExpectedNames {
			expectedSet[normalizeName(n)] = true
		}
		for _, f := range test.ExpectedFiles {
			expectedSet[normalizeFile(f)] = true
		}
		precHits := 0
		for _, n := range result.ActualNames {
			if expectedSet[normalizeName(n)] {
				precHits++
			}
		}
		for _, f := range result.ActualFiles {
			if expectedSet[normalizeFile(f)] {
				precHits++
			}
		}
		result.Precision = float64(precHits) / float64(actualTotal)
	}

	if result.Precision+result.Recall > 0 {
		result.F1 = 2 * result.Precision * result.Recall / (result.Precision + result.Recall)
	}

	return result
}

// ─── Response parsing ────────────────────────────────────────────────────────

var (
	// Matches file paths like "src/flask/app.py:123" or "lib/router/index.js"
	filePathRe = regexp.MustCompile(`(?:^|[\s│\|])([a-zA-Z][\w./-]*\.\w{1,10})(?::(\d+))?`)
	// Matches symbol names like "Flask.run", "Session.request", "func New()"
	symbolNameRe = regexp.MustCompile(`(?:→|←|calls?|called by|imports?|implements?)\s+[` + "`" + `]?([A-Z]\w+(?:\.\w+)*)`)
	// Matches names in structured output like "- **Flask.run**" or "• Session.send"
	bulletNameRe = regexp.MustCompile(`(?:^|\n)\s*[-•*]\s+\*{0,2}([A-Z]\w+(?:\.\w+)*)\*{0,2}`)
	// Matches names after "Tier" headers: "Tier 1 (direct): Node, OtherNode"
	tierNameRe = regexp.MustCompile(`(?i)tier\s+\d+[^:]*:\s*(.+)`)
)

func extractNamesFromResponse(text string) []string {
	seen := make(map[string]bool)
	var names []string

	for _, re := range []*regexp.Regexp{symbolNameRe, bulletNameRe} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if len(m) >= 2 {
				name := m[1]
				norm := normalizeName(name)
				if !seen[norm] && norm != "" {
					seen[norm] = true
					names = append(names, name)
				}
			}
		}
	}

	// Parse tier names from structured impact output.
	for _, m := range tierNameRe.FindAllStringSubmatch(text, -1) {
		if len(m) >= 2 {
			for _, part := range strings.Split(m[1], ",") {
				name := strings.TrimSpace(part)
				name = strings.Trim(name, "`*")
				if name == "" {
					continue
				}
				norm := normalizeName(name)
				if !seen[norm] {
					seen[norm] = true
					names = append(names, name)
				}
			}
		}
	}

	return names
}

func extractFilesFromResponse(text string) []string {
	seen := make(map[string]bool)
	var files []string

	for _, m := range filePathRe.FindAllStringSubmatch(text, -1) {
		if len(m) >= 2 {
			f := strings.TrimSpace(m[1])
			norm := normalizeFile(f)
			if !seen[norm] && looksLikeFile(f) {
				seen[norm] = true
				files = append(files, f)
			}
		}
	}

	return files
}

func looksLikeFile(s string) bool {
	ext := filepath.Ext(s)
	switch ext {
	case ".py", ".go", ".js", ".ts", ".tsx", ".jsx", ".java", ".rs", ".rb", ".c", ".h", ".cpp", ".hpp":
		return true
	}
	return false
}

// ─── Set operations ──────────────────────────────────────────────────────────

// setOverlap returns (hits, total) where hits = |expected ∩ actual|, total = |expected|.
func setOverlap(expected, actual []string) (int, int) {
	actualSet := make(map[string]bool, len(actual))
	for _, a := range actual {
		actualSet[normalizeName(a)] = true
		// Also try file normalization.
		actualSet[normalizeFile(a)] = true
	}
	hits := 0
	for _, e := range expected {
		if actualSet[normalizeName(e)] || actualSet[normalizeFile(e)] {
			hits++
		}
	}
	return hits, len(expected)
}

func normalizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`*\"'")
	return strings.ToLower(s)
}

func normalizeFile(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`*\"'")
	// Strip leading "./" or "/"
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimPrefix(s, "/")
	return strings.ToLower(s)
}

// ─── Repo management ─────────────────────────────────────────────────────────

func ensureRepo(reposDir, repo, commit string) (string, error) {
	// repo = "pallets/flask" → dir = reposDir/pallets_flask
	safeName := strings.ReplaceAll(repo, "/", "_")
	dir := filepath.Join(reposDir, safeName)

	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return "", err
	}

	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		url := fmt.Sprintf("https://github.com/%s.git", repo)
		log.Printf("  cloning %s ...", url)
		cmd := exec.Command("git", "clone", "--no-checkout", url, dir)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git clone: %w", err)
		}
	}

	// Checkout specific commit.
	cmd := exec.Command("git", "checkout", commit)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Try fetching first in case it's a tag.
		fetch := exec.Command("git", "fetch", "--tags", "--depth=1", "origin", commit)
		fetch.Dir = dir
		fetch.Stdout = os.Stderr
		fetch.Stderr = os.Stderr
		_ = fetch.Run()
		// Retry checkout.
		cmd2 := exec.Command("git", "checkout", commit)
		cmd2.Dir = dir
		cmd2.Stdout = os.Stderr
		cmd2.Stderr = os.Stderr
		if err := cmd2.Run(); err != nil {
			return "", fmt.Errorf("git checkout %s: %w", commit, err)
		}
	}

	return dir, nil
}

// ─── Data loading ────────────────────────────────────────────────────────────

func loadGraphBenchData(path string) ([]GraphBenchSuite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var suites []GraphBenchSuite
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		var s GraphBenchSuite
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			log.Printf("graphbench: skip bad line: %v", err)
			continue
		}
		suites = append(suites, s)
	}
	return suites, nil
}

// ─── Aggregation ─────────────────────────────────────────────────────────────

func aggregateGraphResults(results []GraphBenchTestResult, suites []GraphBenchSuite) *reporter.GraphBenchResult {
	// Build language map from suites.
	repoLang := make(map[string]string)
	for _, s := range suites {
		repoLang[s.Repo] = s.Language
	}

	// Aggregate by query_type.
	byType := make(map[string]*metricAccum)
	byLang := make(map[string]*metricAccum)
	overall := &metricAccum{}

	for _, r := range results {
		if r.Error != "" {
			continue
		}
		if byType[r.QueryType] == nil {
			byType[r.QueryType] = &metricAccum{}
		}
		byType[r.QueryType].add(r.Precision, r.Recall, r.F1)
		overall.add(r.Precision, r.Recall, r.F1)
	}

	// Aggregate by language — need to match results back to suites.
	// Build a flat list pairing (result, suite) via index tracking.
	idx := 0
	for _, suite := range suites {
		lang := suite.Language
		if byLang[lang] == nil {
			byLang[lang] = &metricAccum{}
		}
		for range suite.Tests {
			if idx < len(results) && results[idx].Error == "" {
				byLang[lang].add(results[idx].Precision, results[idx].Recall, results[idx].F1)
			}
			idx++
		}
	}

	// Build result.
	gbResult := &reporter.GraphBenchResult{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Summary: reporter.GraphBenchMetrics{
			Precision: overall.avgP(),
			Recall:    overall.avgR(),
			F1:        overall.avgF1(),
		},
		TotalTests: len(results),
	}

	for qt, acc := range byType {
		gbResult.ByQueryType = append(gbResult.ByQueryType, reporter.GraphBenchSlice{
			Label:   qt,
			Tests:   acc.n,
			Metrics: reporter.GraphBenchMetrics{Precision: acc.avgP(), Recall: acc.avgR(), F1: acc.avgF1()},
		})
	}

	for lang, acc := range byLang {
		gbResult.ByLanguage = append(gbResult.ByLanguage, reporter.GraphBenchSlice{
			Label:   lang,
			Tests:   acc.n,
			Metrics: reporter.GraphBenchMetrics{Precision: acc.avgP(), Recall: acc.avgR(), F1: acc.avgF1()},
		})
	}

	// Include per-test detail.
	taskResults := make([]interface{}, len(results))
	for i, r := range results {
		taskResults[i] = r
	}
	gbResult.TestResults = taskResults

	return gbResult
}

type metricAccum struct {
	n          int
	sumP, sumR, sumF1 float64
}

func (m *metricAccum) add(p, r, f1 float64) {
	m.n++
	m.sumP += p
	m.sumR += r
	m.sumF1 += f1
}

func (m *metricAccum) avgP() float64 {
	if m.n == 0 {
		return 0
	}
	return m.sumP / float64(m.n)
}

func (m *metricAccum) avgR() float64 {
	if m.n == 0 {
		return 0
	}
	return m.sumR / float64(m.n)
}

func (m *metricAccum) avgF1() float64 {
	if m.n == 0 {
		return 0
	}
	return m.sumF1 / float64(m.n)
}

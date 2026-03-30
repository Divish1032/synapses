// DriftBench — Incremental Correctness Benchmark.
//
// Tests whether Synapses' incremental reindexing produces correct results as
// a codebase evolves through a sequence of real commits. No academic benchmark
// tests this — Synapses is first to define this category.
//
// Ground truth: clean full reindex at each commit step.
//
// For each commit in a sequence:
//  1. Apply commit (git checkout)
//  2. Trigger incremental reindex via daemon
//  3. Run query suite (find_callers, find_callees, find_implementations, find_imports)
//  4. Trigger clean full reindex
//  5. Run same query suite
//  6. Compare: incremental results vs clean results
//
// Metrics:
//   - Incremental Fidelity: % queries where incremental == clean result
//   - Edge Loss Rate: edges present in clean but missing in incremental
//   - Rename Survival: % renamed entities correctly tracked
//   - Deletion Cleanliness: % deleted entities removed
//   - Speed Ratio: incremental_time / clean_time
//   - Drift Curve: fidelity at each commit step
package benchmarks

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/cmd/benchmark/agent"
	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

// ─── Data types ──────────────────────────────────────────────────────────────

// DriftBenchOptions controls a DriftBench run.
type DriftBenchOptions struct {
	DataFile  string // path to driftbench.jsonl
	ReposDir  string // where repos are cloned
	Limit     int    // max repos (0 = all)
	SkipClean bool   // skip clean reindex verification (trust dataset ground truth)
	OutputDir string // result output directory
}

// DriftBenchSuite is one line from the JSONL file — one repo's commit sequence.
type DriftBenchSuite struct {
	Repo     string             `json:"repo"`
	Language string             `json:"language"`
	Commits  []DriftBenchCommit `json:"commits"`
}

// DriftBenchCommit is one commit in the evolution sequence.
type DriftBenchCommit struct {
	SHA         string            `json:"sha"`
	Category    string            `json:"category"` // normal_edit, rename, file_move, refactor, deletion, new_file, merge
	Description string            `json:"description"`
	Queries     []DriftBenchQuery `json:"queries"`
}

// DriftBenchQuery is a single expected-result query.
type DriftBenchQuery struct {
	Type          string   `json:"type"`           // find_callers, find_callees, find_implementations, find_imports
	Entity        string   `json:"entity"`         // entity name to query
	ExpectedNames []string `json:"expected_names"` // ground truth from clean reindex
}

// driftQueryResult holds per-query comparison.
type driftQueryResult struct {
	QueryType        string   `json:"query_type"`
	Entity           string   `json:"entity"`
	IncrementalNames []string `json:"incremental_names"`
	CleanNames       []string `json:"clean_names"`
	Match            bool     `json:"match"` // incremental == clean
	Precision        float64  `json:"precision"`
	Recall           float64  `json:"recall"`
	F1               float64  `json:"f1"`
	QueryError       error    `json:"-"` // non-nil if query failed
}

// ─── Runner ──────────────────────────────────────────────────────────────────

// RunDriftBench runs the incremental correctness benchmark.
func RunDriftBench(client *agent.SynapsesClient, opts DriftBenchOptions) (*reporter.DriftBenchResult, error) {
	suites, err := loadDriftBenchData(opts.DataFile)
	if err != nil {
		return nil, fmt.Errorf("load data: %w", err)
	}
	if opts.Limit > 0 && len(suites) > opts.Limit {
		suites = suites[:opts.Limit]
	}

	log.Printf("driftbench: %d repo suites loaded", len(suites))

	var allRepoResults []reporter.DriftBenchRepoResult

	for i, suite := range suites {
		log.Printf("[%d/%d] %s (%s, %d commits)",
			i+1, len(suites), suite.Repo, suite.Language, len(suite.Commits))

		repoResult, err := runDriftBenchSuite(client, suite, opts)
		if err != nil {
			log.Printf("  ERROR: %v", err)
			allRepoResults = append(allRepoResults, reporter.DriftBenchRepoResult{
				Repo:     suite.Repo,
				Language: suite.Language,
				Error:    err.Error(),
			})
			continue
		}
		allRepoResults = append(allRepoResults, *repoResult)

		log.Printf("  → Fidelity=%.1f%% EdgeLoss=%.1f%% RenameSurvival=%.1f%% Speed=%.2fx",
			repoResult.IncrementalFidelity*100,
			repoResult.EdgeLossRate*100,
			repoResult.RenameSurvival*100,
			repoResult.SpeedRatio)
	}

	// Aggregate across repos.
	result := aggregateDriftResults(allRepoResults)
	return result, nil
}

func runDriftBenchSuite(client *agent.SynapsesClient, suite DriftBenchSuite, opts DriftBenchOptions) (*reporter.DriftBenchRepoResult, error) {
	// Clone repo.
	repoDir := filepath.Join(opts.ReposDir, strings.ReplaceAll(suite.Repo, "/", "_"))
	if err := ensureDriftRepo(repoDir, suite.Repo); err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}

	// Use project-scoped client.
	projClient := client.WithProject(repoDir)

	result := &reporter.DriftBenchRepoResult{
		Repo:         suite.Repo,
		Language:     suite.Language,
		TotalCommits: len(suite.Commits),
	}

	var totalQueries, matchingQueries int
	var totalIncrementalMs, totalCleanMs int64
	var renameTotal, renameOK int
	var deletionTotal, deletionOK int
	categoryMetrics := make(map[string]*categoryAcc)

	// Checkout first commit and do initial clean index.
	if len(suite.Commits) == 0 {
		return result, nil
	}
	firstSHA := suite.Commits[0].SHA
	if err := gitCheckout(repoDir, firstSHA); err != nil {
		return nil, fmt.Errorf("checkout %s: %w", firstSHA, err)
	}

	// Initial clean index.
	if err := triggerCleanReindex(projClient, repoDir); err != nil {
		return nil, fmt.Errorf("initial index: %w", err)
	}

	for ci, commit := range suite.Commits {
		if ci == 0 {
			// First commit is the baseline — already indexed.
			continue
		}

		// Checkout this commit.
		if err := gitCheckout(repoDir, commit.SHA); err != nil {
			log.Printf("  [%d/%d] skip %s: checkout failed: %v",
				ci+1, len(suite.Commits), shortSHA(commit.SHA), err)
			continue
		}

		// Incremental reindex via admin endpoint (mtime-based, synchronous).
		incrStart := time.Now()
		changed, removed, err := triggerIncrementalReindex(projClient)
		if err != nil {
			log.Printf("  [%d/%d] skip %s: incremental reindex failed: %v",
				ci+1, len(suite.Commits), shortSHA(commit.SHA), err)
			continue
		}
		incrMs := time.Since(incrStart).Milliseconds()
		_ = changed
		_ = removed

		// Run queries against incremental index.
		incrResults := runDriftQueries(projClient, commit.Queries)

		// Clean reindex (ground truth) — full teardown + rebuild.
		var cleanMs int64
		var cleanResults []driftQueryResult
		if !opts.SkipClean {
			cleanStart := time.Now()
			if err := triggerCleanReindex(projClient, repoDir); err != nil {
				log.Printf("  [%d/%d] skip clean: %v", ci+1, len(suite.Commits), err)
				continue
			}
			cleanMs = time.Since(cleanStart).Milliseconds()
			cleanResults = runDriftQueries(projClient, commit.Queries)
		} else {
			// Use expected_names from dataset as ground truth.
			cleanResults = datasetGroundTruth(commit.Queries)
		}

		// Compare incremental vs clean.
		commitFidelity := compareDriftResults(incrResults, cleanResults)

		// Track per-commit metrics.
		for _, qr := range commitFidelity {
			totalQueries++
			if qr.Match {
				matchingQueries++
			}
		}

		// Track category-specific metrics.
		cat := commit.Category
		if categoryMetrics[cat] == nil {
			categoryMetrics[cat] = &categoryAcc{}
		}
		cm := categoryMetrics[cat]
		cm.commits++
		for _, qr := range commitFidelity {
			cm.queries++
			if qr.Match {
				cm.matching++
			}
		}

		// Track rename/deletion specific metrics.
		if cat == "rename" {
			for _, qr := range commitFidelity {
				renameTotal++
				if qr.Match {
					renameOK++
				}
			}
		}
		if cat == "deletion" {
			for _, qr := range commitFidelity {
				deletionTotal++
				if qr.Match {
					deletionOK++
				}
			}
		}

		totalIncrementalMs += incrMs
		totalCleanMs += cleanMs

		// Drift curve: fidelity at this commit step.
		stepFidelity := 0.0
		if len(commitFidelity) > 0 {
			for _, qr := range commitFidelity {
				if qr.Match {
					stepFidelity++
				}
			}
			stepFidelity /= float64(len(commitFidelity))
		}
		result.DriftCurve = append(result.DriftCurve, stepFidelity)

		log.Printf("  [%d/%d] %s (%s): fidelity=%.0f%% incr=%dms clean=%dms",
			ci+1, len(suite.Commits), shortSHA(commit.SHA), cat,
			stepFidelity*100, incrMs, cleanMs)
	}

	// Compute aggregates.
	if totalQueries > 0 {
		result.IncrementalFidelity = float64(matchingQueries) / float64(totalQueries)
	}
	result.TotalQueries = totalQueries
	if renameTotal > 0 {
		result.RenameSurvival = float64(renameOK) / float64(renameTotal)
	}
	if deletionTotal > 0 {
		result.DeletionClean = float64(deletionOK) / float64(deletionTotal)
	}
	if totalCleanMs > 0 {
		result.SpeedRatio = float64(totalIncrementalMs) / float64(totalCleanMs)
	}

	// Per-category breakdown.
	result.PerCategory = make(map[string]reporter.DriftCategoryMetrics)
	for cat, cm := range categoryMetrics {
		fidelity := 0.0
		if cm.queries > 0 {
			fidelity = float64(cm.matching) / float64(cm.queries)
		}
		result.PerCategory[cat] = reporter.DriftCategoryMetrics{
			Commits:  cm.commits,
			Queries:  cm.queries,
			Fidelity: fidelity,
		}
	}

	return result, nil
}

type categoryAcc struct {
	commits  int
	queries  int
	matching int
}

// ─── Query execution ────────────────────────────────────────────────────────

func runDriftQueries(client *agent.SynapsesClient, queries []DriftBenchQuery) []driftQueryResult {
	var results []driftQueryResult
	for _, q := range queries {
		names, queryErr := executeDriftQuery(client, q.Type, q.Entity)
		results = append(results, driftQueryResult{
			QueryType:        q.Type,
			Entity:           q.Entity,
			IncrementalNames: names,
			QueryError:       queryErr,
		})
	}
	return results
}

// fieldMap maps query types to the JSON field name in get_context response.
var driftQueryFieldMap = map[string]string{
	"find_callers":         "callers",
	"find_callees":         "callees",
	"find_implementations": "implementations",
	"find_imports":         "imports",
}

func executeDriftQuery(client *agent.SynapsesClient, queryType, entity string) ([]string, error) {
	field, ok := driftQueryFieldMap[queryType]
	if !ok {
		return nil, fmt.Errorf("unknown query type: %s", queryType)
	}
	raw, err := client.GetContextJSON("drift", entity, "full")
	if err != nil {
		return nil, fmt.Errorf("get_context(%s): %w", entity, err)
	}
	names := extractNamesFromContextJSON(raw, field)
	return names, nil
}

func extractNamesFromContextJSON(raw, field string) []string {
	// The get_context JSON response has sections like "callers", "callees", etc.
	// Each section is an array of objects with "name" fields.
	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil
	}
	section, ok := resp[field]
	if !ok {
		return nil
	}
	var items []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(section, &items); err != nil {
		// Try as array of strings.
		var names []string
		if err2 := json.Unmarshal(section, &names); err2 == nil {
			sort.Strings(names)
			return names
		}
		return nil
	}
	var names []string
	for _, item := range items {
		if item.Name != "" {
			names = append(names, item.Name)
		}
	}
	sort.Strings(names)
	return names
}

func datasetGroundTruth(queries []DriftBenchQuery) []driftQueryResult {
	var results []driftQueryResult
	for _, q := range queries {
		sorted := make([]string, len(q.ExpectedNames))
		copy(sorted, q.ExpectedNames)
		sort.Strings(sorted)
		results = append(results, driftQueryResult{
			QueryType:  q.Type,
			Entity:     q.Entity,
			CleanNames: sorted,
		})
	}
	return results
}

func compareDriftResults(incr, clean []driftQueryResult) []driftQueryResult {
	var results []driftQueryResult
	for i := range incr {
		qr := incr[i]
		if i < len(clean) {
			qr.CleanNames = clean[i].CleanNames
		}

		// Compare: are the name sets identical?
		incrSet := toSet(qr.IncrementalNames)
		cleanSet := toSet(qr.CleanNames)
		qr.Match = setsEqual(incrSet, cleanSet)

		// Compute P/R/F1 treating clean as ground truth.
		if len(cleanSet) > 0 || len(incrSet) > 0 {
			hits := 0
			for name := range incrSet {
				if cleanSet[name] {
					hits++
				}
			}
			if len(incrSet) > 0 {
				qr.Precision = float64(hits) / float64(len(incrSet))
			}
			if len(cleanSet) > 0 {
				qr.Recall = float64(hits) / float64(len(cleanSet))
			}
			if qr.Precision+qr.Recall > 0 {
				qr.F1 = 2 * qr.Precision * qr.Recall / (qr.Precision + qr.Recall)
			}
		} else {
			qr.Match = true // both empty = match
		}

		results = append(results, qr)
	}
	return results
}

func toSet(names []string) map[string]bool {
	s := make(map[string]bool, len(names))
	for _, n := range names {
		s[n] = true
	}
	return s
}

func setsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// ─── Aggregation ────────────────────────────────────────────────────────────

func aggregateDriftResults(repos []reporter.DriftBenchRepoResult) *reporter.DriftBenchResult {
	result := &reporter.DriftBenchResult{
		Timestamp: reporter.Timestamp(),
		ReposRun:  len(repos),
		Repos:     repos,
	}

	var totalFidelity, totalRename, totalDeletion, totalSpeed float64
	var n int
	for _, r := range repos {
		if r.Error != "" {
			continue
		}
		n++
		totalFidelity += r.IncrementalFidelity
		totalRename += r.RenameSurvival
		totalDeletion += r.DeletionClean
		totalSpeed += r.SpeedRatio
	}
	if n > 0 {
		result.AvgFidelity = totalFidelity / float64(n)
		result.AvgRenameSurvival = totalRename / float64(n)
		result.AvgDeletionClean = totalDeletion / float64(n)
		result.AvgSpeedRatio = totalSpeed / float64(n)
	}

	return result
}

// ─── Repo management ────────────────────────────────────────────────────────

func ensureDriftRepo(dir, repo string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return nil // already cloned
	}
	os.MkdirAll(filepath.Dir(dir), 0o755)
	cmd := exec.Command("git", "clone", "--no-checkout", fmt.Sprintf("https://github.com/%s.git", repo), dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func gitCheckout(dir, sha string) error {
	cmd := exec.Command("git", "checkout", "--force", sha)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// triggerCleanReindex runs `synapses index --path <dir> --reset` as a subprocess.
// This is more reliable than the admin API for benchmark use because it:
// (a) doesn't require CSRF tokens, (b) runs synchronously, (c) doesn't race
// with the daemon's project registry.
func triggerCleanReindex(client *agent.SynapsesClient, repoDir string) error {
	// Find the synapses binary (same directory as the benchmark binary).
	self, _ := os.Executable()
	synapsesBin := filepath.Join(filepath.Dir(self), "synapses")
	if _, err := os.Stat(synapsesBin); err != nil {
		synapsesBin = "synapses" // fall back to PATH
	}
	cmd := exec.Command(synapsesBin, "index", "--path", repoDir, "--reset")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("synapses index: %w", err)
	}
	// After CLI index, trigger daemon to load the project by calling a tool.
	_, loadErr := client.Search("driftbench-init", "test")
	if loadErr != nil {
		return fmt.Errorf("trigger project load: %w", loadErr)
	}
	return nil
}

// triggerIncrementalReindex does a mtime-based incremental reindex on the
// existing graph via the admin API. Runs synchronously — returns when done.
func triggerIncrementalReindex(client *agent.SynapsesClient) (changed, removed int, err error) {
	return client.TriggerIncrementalReindex()
}

// ─── Data loading ───────────────────────────────────────────────────────────

func loadDriftBenchData(path string) ([]DriftBenchSuite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var suites []DriftBenchSuite
	for lineNo, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s DriftBenchSuite
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
		}
		suites = append(suites, s)
	}
	return suites, nil
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// GraphBench — Graph Accuracy Benchmark (Benchmark A).
//
// Tests whether Synapses' structural graph correctly represents code
// relationships. Unlike ContextBench (which conflates graph quality with
// retrieval strategy), GraphBench isolates graph correctness.
//
// Query types:
//   - find_callers(symbol)        — who calls this? via get_context format=json
//   - find_callees(symbol)        — what does this call? via get_context format=json
//   - find_imports(file)          — what does this file import? via get_context format=json
//   - impact_analysis(symbol)     — what's affected? via get_impact depth=1
//   - find_implementations(iface) — who implements this? via get_context format=json
//   - find_cross_domain(entity)   — what config/deploy/doc files link to this? via get_context
//   - expected_categories(entity) — cross-domain filtered by edge type (deploys, configured_by, etc.)
//
// All daemon responses are parsed as structured JSON (not regex on Markdown).
// Metrics: per-test Precision, Recall, F1. Aggregated by query_type and language.
package benchmarks

import (
	"context"
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

// GraphBenchOptions controls a GraphBench run.
type GraphBenchOptions struct {
	DataFile   string // path to graphbench.jsonl
	ReposDir   string // where repos are cloned
	OutputDir  string // where per-repo JSON snapshots are written (empty = skip)
	Limit      int    // max test suites (0 = all)
	Mode       string // "full" (default, curated ground truth) or "smoke" (self-validating, CI-safe)
	Sequential bool   // OOM-safe: clone→index→test→cleanup one repo at a time
	RepoFilter string // if non-empty, only run suites whose repo contains this substring
	// CompareLSP enables LSP call hierarchy comparison for find_callers and
	// find_callees tests. Requires gopls (Go) or typescript-language-server (TS)
	// to be installed. Adds LSP F1 fields to each test result and an LSP summary
	// section to the aggregate result.
	CompareLSP bool
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
	QueryType          string   `json:"query_type"`
	Query              string   `json:"query"`
	ExpectedNames      []string `json:"expected_names,omitempty"`
	ExpectedFiles      []string `json:"expected_files,omitempty"`
	ExpectedCategories []string `json:"expected_categories,omitempty"` // for expected_categories query type
	Verified           bool     `json:"verified,omitempty"`            // true if expectations are compiler-verified
	// Disambiguation test fields:
	ExpectedDisambigCount int    `json:"expected_disambig_count,omitempty"` // expected number of candidates
	DisambigFile          string `json:"disambig_file,omitempty"`           // file to pin entity with
	// Call chain test fields:
	ChainFrom string `json:"chain_from,omitempty"` // source entity for find_call_chain
	ChainTo   string `json:"chain_to,omitempty"`   // target entity for find_call_chain
}

// GraphBenchTestResult holds the outcome of one test.
type GraphBenchTestResult struct {
	Repo          string   `json:"repo"`
	Language      string   `json:"language"`
	QueryType     string   `json:"query_type"`
	Query         string   `json:"query"`
	ExpectedNames []string `json:"expected_names,omitempty"`
	ExpectedFiles []string `json:"expected_files,omitempty"`
	ActualNames   []string `json:"actual_names"`
	ActualFiles   []string `json:"actual_files"`
	Precision     float64  `json:"precision"`
	Recall        float64  `json:"recall"`
	F1            float64  `json:"f1"`
	LatencyMs     int64    `json:"latency_ms"`
	Error         string   `json:"error,omitempty"`
	ErrorCategory string   `json:"error_category,omitempty"` // entity_not_found, empty_response, wrong_candidates, timeout
	RawResponse   string   `json:"raw_response,omitempty"`   // for debugging failures
	// LSP comparison fields — populated only when CompareLSP is true and the
	// query type is find_callers or find_callees.
	LSPNames []string `json:"lsp_names,omitempty"` // names returned by LSP call hierarchy
	LSPPrec  float64  `json:"lsp_precision,omitempty"`
	LSPRecall float64 `json:"lsp_recall,omitempty"`
	LSPF1    float64  `json:"lsp_f1,omitempty"`
}

// repoStats holds per-repo indexing metadata captured after waitForIndex.
type repoStats struct {
	Repo        string `json:"repo"`
	Language    string `json:"language"`
	IndexTimeMs int64  `json:"index_time_ms"`
	NodeCount   int    `json:"node_count"`
	EdgeCount   int    `json:"edge_count"`
}

// ─── Daemon response JSON shapes ─────────────────────────────────────────────

// impactResponse is the JSON shape of get_impact responses.
type impactResponse struct {
	Root struct {
		Name string `json:"name"`
		Type string `json:"type"`
		File string `json:"file"`
		Line int    `json:"line"`
	} `json:"root"`
	Tiers []struct {
		Depth int    `json:"depth"`
		Label string `json:"label"`
		Nodes []struct {
			Name string `json:"name"`
			Type string `json:"type"`
			File string `json:"file"`
			Line int    `json:"line"`
		} `json:"nodes"`
	} `json:"tiers"`
	AffectedFiles []string `json:"affected_files"`
	TotalAffected int      `json:"total_affected"`
}

// contextResponse is the JSON shape of get_context format=json responses.
type contextResponse struct {
	Root struct {
		Name string `json:"name"`
		Type string `json:"type"`
		File string `json:"file"`
		Line int    `json:"line"`
	} `json:"root"`
	Callees []struct {
		Node struct {
			Name string `json:"name"`
			Type string `json:"type"`
			File string `json:"file"`
		} `json:"node"`
		Relevance float64 `json:"relevance"`
	} `json:"callees"`
	Callers []struct {
		Node struct {
			Name string `json:"name"`
			Type string `json:"type"`
			File string `json:"file"`
		} `json:"node"`
	} `json:"callers"`
	Related []struct {
		Node struct {
			Name string `json:"name"`
			Type string `json:"type"`
			File string `json:"file"`
		} `json:"node"`
	} `json:"related"`
	Documentation []struct {
		Node struct {
			Name string `json:"name"`
			Type string `json:"type"`
			File string `json:"file"`
		} `json:"node"`
		Relevance float64 `json:"relevance"`
	} `json:"documentation"`
	CrossDomain struct {
		DeploysTo    []crossDomainNode `json:"deploys"`
		Consumes     []crossDomainNode `json:"consumes"`
		ConfiguredBy []crossDomainNode `json:"configured_by"`
		DocumentedIn []crossDomainNode `json:"documented_in"`
		Mentions     []crossDomainNode `json:"mentions"`
		Manual       []crossDomainNode `json:"manual"`
		Related      []crossDomainNode `json:"related"`
	} `json:"cross_domain"`
	Imports []struct {
		Node struct {
			Name string `json:"name"`
			Type string `json:"type"`
			File string `json:"file"`
		} `json:"node"`
	} `json:"imports"`
	OtherCandidates []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		File string `json:"file"`
	} `json:"other_candidates"`
	DisambigHint string `json:"disambig_hint"`
}

type crossDomainNode struct {
	Name string `json:"name"`
	Type string `json:"type"`
	File string `json:"file"`
}

// ─── Runner ──────────────────────────────────────────────────────────────────

// RunGraphBench runs the benchmark in the selected mode and returns a reporter-compatible result.
func RunGraphBench(client *agent.SynapsesClient, opts GraphBenchOptions) (*reporter.GraphBenchResult, error) {
	if opts.Mode == "smoke" {
		return runGraphBenchSmoke(client)
	}
	suites, err := loadGraphBenchData(opts.DataFile)
	if err != nil {
		return nil, fmt.Errorf("load data: %w", err)
	}
	if opts.Limit > 0 && len(suites) > opts.Limit {
		suites = suites[:opts.Limit]
	}
	if opts.RepoFilter != "" {
		filtered := suites[:0]
		for _, s := range suites {
			if strings.Contains(s.Repo, opts.RepoFilter) {
				filtered = append(filtered, s)
			}
		}
		suites = filtered
	}

	log.Printf("graphbench: %d repo suites loaded", len(suites))

	var allResults []GraphBenchTestResult
	var allRepoStats []repoStats

	for i, suite := range suites {
		log.Printf("[%d/%d] %s @ %s (%s, %d tests)",
			i+1, len(suites), suite.Repo, suite.Commit, suite.Language, len(suite.Tests))

		repoDir, err := ensureRepo(opts.ReposDir, suite.Repo, suite.Commit)
		if err != nil {
			log.Printf("  SKIP: clone/checkout failed: %v", err)
			for _, t := range suite.Tests {
				allResults = append(allResults, GraphBenchTestResult{
					Repo: suite.Repo, Language: suite.Language,
					QueryType: t.QueryType, Query: t.Query,
					Error:         fmt.Sprintf("repo setup: %v", err),
					ErrorCategory: "repo_setup",
				})
			}
			continue
		}

		projClient := client.WithProject(repoDir)

		// Trigger indexing and measure index time.
		indexStart := time.Now()
		if err := waitForIndex(projClient, repoDir); err != nil {
			log.Printf("  warning: indexing may be incomplete: %v", err)
		}
		indexTimeMs := time.Since(indexStart).Milliseconds()

		// Capture graph stats from health endpoint.
		// NOTE: /v1/health returns aggregate stats across ALL registered projects,
		// not just the current one. When running with the batch runner (which
		// restarts the daemon between repos), this gives per-repo stats.
		rs := repoStats{
			Repo:        suite.Repo,
			Language:    suite.Language,
			IndexTimeMs: indexTimeMs,
		}
		if health, err := client.GetHealth(); err == nil {
			rs.NodeCount = health.NodeCount
			rs.EdgeCount = health.EdgeCount
		}
		allRepoStats = append(allRepoStats, rs)
		log.Printf("  indexed in %dms (nodes=%d, edges=%d)", rs.IndexTimeMs, rs.NodeCount, rs.EdgeCount)

		// Create LSP runner for this repo if compare-lsp is enabled.
		// Only Go and TypeScript have LSP support; NewLSPBenchRunner returns nil for others.
		var lspRunner *LSPBenchRunner
		if opts.CompareLSP {
			lspRunner, _ = NewLSPBenchRunner(suite.Language, repoDir)
			if lspRunner != nil {
				log.Printf("  lsp: runner started for %s", suite.Language)
			}
		}

		for j, test := range suite.Tests {
			result := runGraphTest(projClient, suite, test, lspRunner)
			// In sequential mode, drop raw responses to save memory.
			if opts.Sequential {
				result.RawResponse = ""
			}
			allResults = append(allResults, result)
			status := "✓"
			if result.Error != "" {
				status = "✗ " + truncate(result.Error, 80)
			}
			lspSuffix := ""
			if result.LSPF1 > 0 {
				lspSuffix = fmt.Sprintf(" [LSP F1=%.0f%%]", result.LSPF1*100)
			}
			log.Printf("  [%d/%d] %s(%s): P=%.0f%% R=%.0f%% F1=%.0f%% %dms %s%s",
				j+1, len(suite.Tests), test.QueryType, test.Query,
				result.Precision*100, result.Recall*100, result.F1*100, result.LatencyMs, status, lspSuffix)
		}

		// Shut down LSP process before sequential cleanup to avoid file handle leaks.
		if lspRunner != nil {
			if err := lspRunner.Close(); err != nil {
				log.Printf("  warning: lsp close: %v", err)
			}
		}

		// Persist per-repo results immediately so progress survives crashes.
		if opts.OutputDir != "" {
			writePerRepoResult(opts.OutputDir, suite, rs, allResults)
		}

		// OOM-safe cleanup: remove project from daemon and delete repo directory.
		if opts.Sequential {
			log.Printf("  cleanup: removing %s", suite.Repo)
			_ = client.RemoveProject(repoDir)
			if err := os.RemoveAll(repoDir); err != nil {
				log.Printf("  warning: failed to remove repo dir: %v", err)
			}
		}
	}

	return aggregateGraphResults(allResults, suites, allRepoStats), nil
}

// writePerRepoResult writes a JSON file with the results for a single repo.
// File name: <outputDir>/repo/<owner>__<name>.json
// This allows progress to be inspected and survives benchmark crashes.
func writePerRepoResult(outputDir string, suite GraphBenchSuite, stats repoStats, allResults []GraphBenchTestResult) {
	// Collect only results for this repo.
	var repoResults []GraphBenchTestResult
	for _, r := range allResults {
		if r.Repo == suite.Repo {
			repoResults = append(repoResults, r)
		}
	}

	// Compute simple per-repo F1.
	var totalF1 float64
	for _, r := range repoResults {
		totalF1 += r.F1
	}
	avgF1 := 0.0
	if len(repoResults) > 0 {
		avgF1 = totalF1 / float64(len(repoResults))
	}

	payload := struct {
		Repo        string                  `json:"repo"`
		Language    string                  `json:"language"`
		Commit      string                  `json:"commit"`
		IndexTimeMs int64                   `json:"index_time_ms"`
		NodeCount   int                     `json:"node_count"`
		EdgeCount   int                     `json:"edge_count"`
		AvgF1       float64                 `json:"avg_f1"`
		Tests       []GraphBenchTestResult  `json:"tests"`
	}{
		Repo:        suite.Repo,
		Language:    suite.Language,
		Commit:      suite.Commit,
		IndexTimeMs: stats.IndexTimeMs,
		NodeCount:   stats.NodeCount,
		EdgeCount:   stats.EdgeCount,
		AvgF1:       avgF1,
		Tests:       repoResults,
	}

	dir := filepath.Join(outputDir, "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("  warning: could not create per-repo output dir: %v", err)
		return
	}
	safeName := strings.ReplaceAll(suite.Repo, "/", "__")
	path := filepath.Join(dir, safeName+".json")
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("  warning: could not marshal per-repo result: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("  warning: could not write per-repo result: %v", err)
		return
	}
	log.Printf("  saved: %s (F1=%.1f%%)", path, avgF1*100)
}

// waitForIndex triggers indexing via a search probe and polls until the daemon
// has indexed the project (subsequent search returns results or stops erroring).
func waitForIndex(client *agent.SynapsesClient, repoDir string) error {
	log.Printf("  triggering index for %s ...", filepath.Base(repoDir))

	// First call triggers indexing.
	_, _ = client.Search("graphbench-index", "main")

	// Poll: retry search up to 150 times (5 min total) waiting for index.
	// Large repos (Hono, Axum, Guava) need well over 60s to parse.
	for attempt := 0; attempt < 150; attempt++ {
		time.Sleep(2 * time.Second)
		resp, err := client.Search("graphbench-poll", "function")
		if err == nil && resp != nil && len(resp.Text) > 20 {
			log.Printf("  index ready (attempt %d)", attempt+1)
			return nil
		}
		if attempt%15 == 14 {
			log.Printf("  still waiting for index (attempt %d/150)...", attempt+1)
		}
	}
	return fmt.Errorf("indexing did not complete within 5 min")
}

// runGraphTest executes a single test case against the daemon.
// lspRunner is optional; when non-nil, LSP call hierarchy comparison is run for
// find_callers and find_callees tests and results are stored in LSP* fields.
func runGraphTest(client *agent.SynapsesClient, suite GraphBenchSuite, test GraphBenchTest, lspRunner *LSPBenchRunner) GraphBenchTestResult {
	result := GraphBenchTestResult{
		Repo:          suite.Repo,
		Language:      suite.Language,
		QueryType:     test.QueryType,
		Query:         test.Query,
		ExpectedNames: test.ExpectedNames,
		ExpectedFiles: test.ExpectedFiles,
	}

	var names []string
	var files []string
	var rawResp string

	queryStart := time.Now()

	switch test.QueryType {
	case "find_callers":
		names, files, rawResp = queryContextCallers(client, test.Query)

	case "find_callees":
		names, files, rawResp = queryContextCallees(client, test.Query)

	case "find_imports":
		names, files, rawResp = queryFileImports(client, test.Query)

	case "impact_analysis":
		names, files, rawResp = queryImpact(client, test.Query, 1)

	case "find_implementations":
		names, files, rawResp = queryContextRelated(client, test.Query)

	case "find_cross_domain":
		names, files, rawResp = queryContextCrossDomain(client, test.Query, nil)

	case "expected_categories":
		names, files, rawResp = queryContextCrossDomain(client, test.Query, test.ExpectedCategories)

	case "coverage_probe":
		names, files, rawResp = queryCoverageProbe(client, test.ExpectedNames)

	case "disambiguation":
		names, files, rawResp = queryDisambiguation(client, test)

	case "find_call_chain":
		names, files, rawResp = queryCallChain(client, test)

	case "exact_entity_lookup":
		names, files, rawResp = queryExactEntityLookup(client, test.Query)

	case "file_entities":
		names, files, rawResp = queryFileEntities(client, test.Query)

	default:
		result.Error = fmt.Sprintf("unknown query_type %q", test.QueryType)
		result.ErrorCategory = "unknown_type"
		return result
	}

	result.LatencyMs = time.Since(queryStart).Milliseconds()
	result.ActualNames = names
	result.ActualFiles = files
	if rawResp == "" {
		rawResp = "(empty response)"
	}
	result.RawResponse = truncate(rawResp, 500)

	// Classify error responses.
	if strings.Contains(rawResp, "entity not found") {
		result.Error = "entity not found"
		result.ErrorCategory = "entity_not_found"
		return result
	}
	if strings.Contains(rawResp, "error:") && strings.Contains(rawResp, "timeout") {
		result.Error = "timeout"
		result.ErrorCategory = "timeout"
		return result
	}
	// Empty response: entity resolved but no edges returned.
	if len(names) == 0 && len(files) == 0 && test.QueryType != "coverage_probe" && test.QueryType != "disambiguation" {
		hasExpected := len(test.ExpectedNames) > 0 || len(test.ExpectedFiles) > 0
		if hasExpected && !strings.Contains(rawResp, "error") {
			result.ErrorCategory = "empty_response"
		}
	}

	// Compute Recall: what fraction of expected items were found?
	recallHits, recallTotal := 0, 0
	if len(test.ExpectedNames) > 0 {
		h, t := setOverlap(test.ExpectedNames, names)
		recallHits += h
		recallTotal += t
	}
	if len(test.ExpectedFiles) > 0 {
		h, t := setOverlapFiles(test.ExpectedFiles, files)
		recallHits += h
		recallTotal += t
	}
	if recallTotal > 0 {
		result.Recall = float64(recallHits) / float64(recallTotal)
	}

	// Compute Precision: what fraction of returned items were expected?
	precHits, precTotal := 0, 0
	if len(test.ExpectedNames) > 0 && len(names) > 0 {
		// Precision: what fraction of returned names match any expected name?
		// Uses the same partial dot-suffix matching as recall.
		for _, n := range names {
			precTotal++
			nn := normalizeName(n)
			matched := false
			for _, e := range test.ExpectedNames {
				ne := normalizeName(e)
				if nn == ne || strings.HasSuffix(nn, "."+ne) || strings.HasSuffix(ne, "."+nn) ||
					strings.HasPrefix(nn, ne+".") || strings.HasPrefix(ne, nn+".") ||
					strings.HasPrefix(nn, ne+"::") || strings.HasPrefix(ne, nn+"::") {
					matched = true
					break
				}
			}
			if matched {
				precHits++
			}
		}
	}
	if len(test.ExpectedFiles) > 0 && len(files) > 0 {
		expectedFileSet := makeNormFileSet(test.ExpectedFiles)
		for _, f := range files {
			precTotal++
			if matchesFileSet(f, expectedFileSet) {
				precHits++
			}
		}
	}
	if precTotal > 0 {
		result.Precision = float64(precHits) / float64(precTotal)
	}

	if result.Precision+result.Recall > 0 {
		result.F1 = 2 * result.Precision * result.Recall / (result.Precision + result.Recall)
	}

	// LSP enrichment: run call hierarchy query alongside the baseline for
	// find_callers and find_callees. Uses Root.File/Root.Line from the context
	// response (function definition position, which LSP requires).
	if lspRunner != nil && len(test.ExpectedNames) > 0 &&
		(test.QueryType == "find_callers" || test.QueryType == "find_callees") {
		var cr contextResponse
		if json.Unmarshal([]byte(rawResp), &cr) == nil && cr.Root.File != "" {
			lspCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			var lspNames []string
			if test.QueryType == "find_callers" {
				lspNames = lspRunner.QueryCallers(lspCtx, cr.Root.File, cr.Root.Line)
			} else {
				lspNames = lspRunner.QueryCallees(lspCtx, cr.Root.File, cr.Root.Line)
			}
			cancel()

			if len(lspNames) > 0 {
				result.LSPNames = lspNames
				// Recall: what fraction of expected names were returned by LSP?
				lspRecallHits, lspRecallTotal := setOverlap(test.ExpectedNames, lspNames)
				if lspRecallTotal > 0 {
					result.LSPRecall = float64(lspRecallHits) / float64(lspRecallTotal)
				}
				// Precision: what fraction of LSP names match an expected name?
				lspPrecHits := 0
				for _, n := range lspNames {
					nn := normalizeName(n)
					for _, e := range test.ExpectedNames {
						ne := normalizeName(e)
						if nn == ne || strings.HasSuffix(nn, "."+ne) || strings.HasSuffix(ne, "."+nn) ||
							strings.HasPrefix(nn, ne+".") || strings.HasPrefix(ne, nn+".") ||
							strings.HasPrefix(nn, ne+"::") || strings.HasPrefix(ne, nn+"::") {
							lspPrecHits++
							break
						}
					}
				}
				result.LSPPrec = float64(lspPrecHits) / float64(len(lspNames))
				if result.LSPPrec+result.LSPRecall > 0 {
					result.LSPF1 = 2 * result.LSPPrec * result.LSPRecall / (result.LSPPrec + result.LSPRecall)
				}
			}
		}
	}

	return result
}

// ─── Query helpers (structured JSON parsing) ─────────────────────────────────

// queryImpact calls get_impact and extracts node names + affected_files from
// the structured JSON response.
func queryImpact(client *agent.SynapsesClient, symbol string, depth int) (names, files []string, raw string) {
	resp, err := client.GetImpactWithDepth("graphbench", symbol, depth)
	if err != nil {
		return nil, nil, fmt.Sprintf("error: %v", err)
	}
	raw = resp.Text

	var ir impactResponse
	if err := json.Unmarshal([]byte(raw), &ir); err != nil {
		// Fallback: try regex extraction if JSON parse fails.
		return extractNamesFromText(raw), extractFilesFromText(raw), raw
	}

	nameSet := make(map[string]bool)
	for _, tier := range ir.Tiers {
		for _, node := range tier.Nodes {
			// Skip test file nodes — impact analysis should focus on production code.
			if isTestFilePath(node.File) {
				continue
			}
			if node.Name != "" && !nameSet[strings.ToLower(node.Name)] {
				nameSet[strings.ToLower(node.Name)] = true
				names = append(names, node.Name)
			}
			if node.File != "" {
				files = appendUniqueFile(files, node.File)
			}
		}
	}

	// Also include affected_files from the response (filter test files).
	for _, f := range ir.AffectedFiles {
		if !isTestFilePath(f) {
			files = appendUniqueFile(files, f)
		}
	}

	return names, files, raw
}

// queryContextCallers calls get_context (JSON) and extracts caller names+files.
// This is far more precise than get_impact depth=1 which does BFS and returns
// dozens of unrelated nodes.
func queryContextCallers(client *agent.SynapsesClient, entity string) (names, files []string, raw string) {
	raw, err := client.GetContextJSON("graphbench", entity, "full")
	if err != nil {
		return nil, nil, fmt.Sprintf("error: %v", err)
	}

	var cr contextResponse
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		return extractNamesFromText(raw), extractFilesFromText(raw), raw
	}

	for _, caller := range cr.Callers {
		if caller.Node.Name != "" {
			names = appendUniqueName(names, caller.Node.Name)
		}
		if caller.Node.File != "" {
			files = appendUniqueFile(files, caller.Node.File)
		}
	}

	return names, files, raw
}

// queryFileImports handles find_imports by querying get_context with a file path.
// The daemon now returns an "imports" field containing all IMPORTS edges from
// the file's NodeFile node — directly answering "what does this file import?"
func queryFileImports(client *agent.SynapsesClient, filePath string) (names, files []string, raw string) {
	raw, err := client.GetContextJSON("graphbench", filePath, "full")
	if err != nil {
		return nil, nil, fmt.Sprintf("error: %v", err)
	}

	var cr contextResponse
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		return extractNamesFromText(raw), extractFilesFromText(raw), raw
	}

	seenNames := make(map[string]bool)
	seenFiles := make(map[string]bool)

	// Primary: use the new "imports" field (NodeFile → IMPORTS edges).
	// Import node names are the full import path (e.g., "net/http" for Go,
	// "werkzeug.test" for Python). Add both the full name and the top-level
	// package name (before first dot) so matching works against expected
	// names that may be either form.
	for _, imp := range cr.Imports {
		n := imp.Node.Name
		f := imp.Node.File
		if n != "" && !seenNames[strings.ToLower(n)] {
			seenNames[strings.ToLower(n)] = true
			names = append(names, n)
		}
		if f != "" && !seenFiles[normalizeFile(f)] {
			seenFiles[normalizeFile(f)] = true
			files = append(files, f)
		}
	}

	return names, files, truncate(raw, 2000)
}

// extractPackageName extracts the top-level package/module name from a symbol
// or file path. For a callee in a different file than the queried file, the
// first path component (Python package) or the base name is the "import".
func extractPackageName(calleeName, queryFile string) string {
	// If callee name has dots (e.g., "werkzeug.serving.run_simple"), extract root.
	parts := strings.Split(calleeName, ".")
	if len(parts) > 1 {
		root := strings.ToLower(parts[0])
		// Skip if root matches the queried file's own package.
		queryBase := strings.ToLower(filepath.Base(filepath.Dir(queryFile)))
		if root != queryBase && root != "" {
			return parts[0]
		}
	}
	return ""
}

// queryContextCallees calls get_context (JSON) and extracts callee names+files.
func queryContextCallees(client *agent.SynapsesClient, entity string) (names, files []string, raw string) {
	raw, err := client.GetContextJSON("graphbench", entity, "full")
	if err != nil {
		return nil, nil, fmt.Sprintf("error: %v", err)
	}

	var cr contextResponse
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		return extractNamesFromText(raw), extractFilesFromText(raw), raw
	}

	for _, callee := range cr.Callees {
		if callee.Node.Name != "" {
			names = appendUniqueName(names, callee.Node.Name)
		}
		if callee.Node.File != "" {
			files = appendUniqueFile(files, callee.Node.File)
		}
	}

	return names, files, raw
}

// queryContextRelated calls get_context (JSON) and extracts related nodes
// (implementations, callers, cross-domain) — used for find_implementations.
func queryContextRelated(client *agent.SynapsesClient, entity string) (names, files []string, raw string) {
	raw, err := client.GetContextJSON("graphbench", entity, "full")
	if err != nil {
		return nil, nil, fmt.Sprintf("error: %v", err)
	}

	var cr contextResponse
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		return extractNamesFromText(raw), extractFilesFromText(raw), raw
	}

	// Collect related nodes — for find_implementations, prefer struct/class types
	// over their methods. Methods of implementing types are noise.
	for _, rel := range cr.Related {
		if rel.Node.Name != "" {
			// Only include struct/interface/class types, not methods.
			nodeType := rel.Node.Type
			if nodeType == "struct" || nodeType == "class" || nodeType == "interface" {
				names = appendUniqueName(names, rel.Node.Name)
			}
		}
		if rel.Node.File != "" {
			files = appendUniqueFile(files, rel.Node.File)
		}
	}

	// Also check callers — interface implementations appear as callers in some cases.
	for _, caller := range cr.Callers {
		if caller.Node.Name != "" {
			names = appendUniqueName(names, caller.Node.Name)
		}
	}

	return names, files, raw
}

// queryCoverageProbe searches for each expected entity name and returns which
// ones the daemon can find. Coverage = found / total expected.
func queryCoverageProbe(client *agent.SynapsesClient, expectedNames []string) (names, files []string, raw string) {
	var found []string
	var rawParts []string
	for _, name := range expectedNames {
		resp, err := client.Search("graphbench-coverage", name)
		if err != nil {
			continue
		}
		rawParts = append(rawParts, truncate(resp.Text, 200))
		// If the search returned something mentioning the name, count it as found.
		if resp.Text != "" && len(resp.Text) > 20 {
			found = append(found, name)
		}
	}
	return found, nil, strings.Join(rawParts, "\n---\n")
}

// queryDisambiguation tests entity disambiguation: queries an ambiguous name,
// checks for other_candidates, then re-queries with file= hint.
func queryDisambiguation(client *agent.SynapsesClient, test GraphBenchTest) (names, files []string, raw string) {
	// First call: query without file hint — expect disambiguation.
	raw, err := client.GetContextJSON("graphbench", test.Query, "full")
	if err != nil {
		return nil, nil, fmt.Sprintf("error: %v", err)
	}

	var cr contextResponse
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		return nil, nil, raw
	}

	// Check if disambiguation was triggered.
	candidateCount := len(cr.OtherCandidates)
	if candidateCount > 0 {
		names = append(names, fmt.Sprintf("disambig_count:%d", candidateCount))
	}
	if cr.DisambigHint != "" {
		names = append(names, "has_disambig_hint")
	}

	// Second call: re-query with file= hint to pin entity.
	if test.DisambigFile != "" {
		raw2, err := client.GetContextJSONWithFile("graphbench", test.Query, "full", test.DisambigFile)
		if err == nil {
			var cr2 contextResponse
			if json.Unmarshal([]byte(raw2), &cr2) == nil {
				// After pinning, there should be no other_candidates.
				if len(cr2.OtherCandidates) == 0 {
					names = append(names, "pinned_ok")
				}
				if cr2.Root.File != "" {
					files = append(files, cr2.Root.File)
				}
			}
			raw = raw + "\n---pinned---\n" + raw2
		}
	}

	return names, files, raw
}

// queryContextCrossDomain calls get_context (JSON) and extracts cross-domain
// nodes (deploys, configured_by, documented_in, etc). If categories is non-nil,
// only nodes from those categories are included.
func queryContextCrossDomain(client *agent.SynapsesClient, entity string, categories []string) (names, files []string, raw string) {
	raw, err := client.GetContextJSON("graphbench", entity, "full")
	if err != nil {
		return nil, nil, fmt.Sprintf("error: %v", err)
	}

	var cr contextResponse
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		return extractNamesFromText(raw), extractFilesFromText(raw), raw
	}

	catSet := make(map[string]bool, len(categories))
	for _, c := range categories {
		catSet[strings.ToLower(c)] = true
	}
	filterByCategory := len(catSet) > 0

	collect := func(nodes []crossDomainNode, category string) {
		if filterByCategory && !catSet[strings.ToLower(category)] {
			return
		}
		for _, n := range nodes {
			if n.Name != "" {
				names = appendUniqueName(names, n.Name)
			}
			if n.File != "" {
				files = appendUniqueFile(files, n.File)
			}
		}
	}

	collect(cr.CrossDomain.DeploysTo, "deploys")
	collect(cr.CrossDomain.Consumes, "consumes")
	collect(cr.CrossDomain.ConfiguredBy, "configured_by")
	collect(cr.CrossDomain.DocumentedIn, "documented_in")
	collect(cr.CrossDomain.Mentions, "mentions")
	collect(cr.CrossDomain.Manual, "manual")
	collect(cr.CrossDomain.Related, "related")

	return names, files, raw
}

// callChainResponse is the JSON shape of get_context(mode=path) responses.
type callChainResponse struct {
	Found bool `json:"found"`
	From  struct {
		Name string `json:"name"`
		File string `json:"file"`
	} `json:"from"`
	To struct {
		Name string `json:"name"`
		File string `json:"file"`
	} `json:"to"`
	Path []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		File string `json:"file"`
	} `json:"path"`
	ClosestReachable struct {
		Name string `json:"name"`
		File string `json:"file"`
		Hops int    `json:"hops"`
	} `json:"closest_reachable"`
}

// queryCallChain calls get_context(mode=path) to find the call chain between two entities.
// Expected names are the entities along the path (including from/to).
func queryCallChain(client *agent.SynapsesClient, test GraphBenchTest) (names, files []string, raw string) {
	from := test.ChainFrom
	to := test.ChainTo
	if from == "" {
		from = test.Query
	}
	if to == "" {
		return nil, nil, "error: chain_to not specified"
	}

	raw, err := client.GetCallChain("graphbench", from, to)
	if err != nil {
		return nil, nil, fmt.Sprintf("error: %v", err)
	}

	var cr callChainResponse
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		// Fallback to text extraction.
		return extractNamesFromText(raw), extractFilesFromText(raw), raw
	}

	if cr.Found {
		// Collect all nodes along the path.
		if cr.From.Name != "" {
			names = appendUniqueName(names, cr.From.Name)
		}
		for _, node := range cr.Path {
			if node.Name != "" {
				names = appendUniqueName(names, node.Name)
			}
			if node.File != "" {
				files = appendUniqueFile(files, node.File)
			}
		}
		if cr.To.Name != "" {
			names = appendUniqueName(names, cr.To.Name)
		}
	}
	// If not found, names/files will be empty → zero recall (correct behavior:
	// the graph doesn't have this path, which is what we're measuring).

	return names, files, raw
}

// queryExactEntityLookup calls search(mode=exact) to resolve an entity by name.
// Tests that the name index correctly maps entity names to graph nodes.
func queryExactEntityLookup(client *agent.SynapsesClient, entity string) (names, files []string, raw string) {
	raw, err := client.SearchExact("graphbench", entity)
	if err != nil {
		return nil, nil, fmt.Sprintf("error: %v", err)
	}

	// Parse JSON array of search results.
	var results []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		File string `json:"file"`
		Line int    `json:"line"`
	}
	if err := json.Unmarshal([]byte(raw), &results); err != nil {
		// Fallback: try extracting from text.
		return extractNamesFromText(raw), extractFilesFromText(raw), raw
	}

	for _, r := range results {
		if r.Name != "" {
			names = appendUniqueName(names, r.Name)
		}
		if r.File != "" {
			files = appendUniqueFile(files, r.File)
		}
	}
	return names, files, raw
}

// queryFileEntities calls get_file_context to list all entities in a file.
// Tests that the parser correctly extracts all symbols from a source file.
func queryFileEntities(client *agent.SynapsesClient, filePath string) (names, files []string, raw string) {
	result, err := client.GetFileContext("graphbench", filePath)
	if err != nil {
		return nil, nil, fmt.Sprintf("error: %v", err)
	}

	for _, e := range result.Entities {
		if e.Name != "" {
			names = appendUniqueName(names, e.Name)
		}
	}
	if result.File != "" {
		files = append(files, result.File)
	}

	rawBytes, _ := json.Marshal(result)
	return names, files, string(rawBytes)
}

// ─── Text fallback extractors (used when JSON parse fails) ───────────────────

func extractNamesFromText(text string) []string {
	seen := make(map[string]bool)
	var names []string

	// Parse "Calls: a · b · c" and "Called by: x · y" patterns.
	for _, prefix := range []string{"Calls:", "Called by:", "DIRECT:", "INDIRECT:", "PERIPHERAL:"} {
		idx := strings.Index(text, prefix)
		if idx < 0 {
			continue
		}
		rest := text[idx+len(prefix):]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[:nl]
		}
		// Split on both · (daemon format) and , (common in DIRECT/INDIRECT lines).
		parts := strings.Split(rest, "·")
		if len(parts) == 1 {
			// No · separator found — try comma.
			parts = strings.Split(rest, ",")
		}
		for _, part := range parts {
			name := strings.TrimSpace(part)
			name = strings.Trim(name, "`*[]")
			if name == "" || name == "(none)" {
				continue
			}
			// Strip node type suffix like " method" or " function"
			if sp := strings.LastIndex(name, " "); sp > 0 {
				candidate := name[:sp]
				suffix := strings.ToLower(name[sp+1:])
				if suffix == "method" || suffix == "function" || suffix == "class" ||
					suffix == "struct" || suffix == "interface" || suffix == "module" {
					name = candidate
				}
			}
			norm := normalizeName(name)
			if !seen[norm] {
				seen[norm] = true
				names = append(names, name)
			}
		}
	}

	// Parse "[NodeName] type · file.go:line" headers.
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if len(line) > 2 && line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end > 1 {
				name := line[1:end]
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

func extractFilesFromText(text string) []string {
	seen := make(map[string]bool)
	var files []string

	for _, line := range strings.Split(text, "\n") {
		// Look for "file.ext:line" patterns.
		for _, word := range strings.Fields(line) {
			word = strings.Trim(word, "·│|`*[](){}")
			if colon := strings.LastIndex(word, ":"); colon > 0 {
				word = word[:colon]
			}
			if looksLikeFile(word) {
				norm := normalizeFile(word)
				if !seen[norm] {
					seen[norm] = true
					files = append(files, word)
				}
			}
		}
	}

	return files
}

func looksLikeFile(s string) bool {
	ext := filepath.Ext(s)
	switch ext {
	case ".py", ".go", ".js", ".ts", ".tsx", ".jsx", ".java", ".rs", ".rb",
		".c", ".h", ".cpp", ".hpp", ".cc", ".hh", ".cs", ".swift", ".kt",
		".scala", ".php", ".pl", ".pm", ".lua", ".ex", ".exs", ".hs",
		".ml", ".mli", ".clj", ".cljs", ".dart", ".zig", ".nim",
		".r", ".R", ".jl", ".m", ".sh", ".ps1", ".psm1", ".groovy",
		".vhd", ".vhdl", ".v", ".sv", ".f90", ".f95", ".adb", ".ads",
		".cob", ".cbl", ".erl", ".hrl", ".fs", ".fsi", ".fsx":
		return true
	}
	return false
}

// ─── Set operations ──────────────────────────────────────────────────────────

// setOverlap returns (hits, total) where hits = |expected ∩ actual|, total = |expected|.
// Uses name normalization for matching.
func setOverlap(expected, actual []string) (int, int) {
	actualSet := makeNormSet(actual)
	hits := 0
	for _, e := range expected {
		ne := normalizeName(e)
		if actualSet[ne] {
			hits++
			continue
		}
		// Partial match: "Flask.__init__" should match "__init__" or "Flask".
		// Also "Session.request" should match "request".
		// For Rust: "std::future" should match "std::future::Future".
		for _, a := range actual {
			na := normalizeName(a)
			if strings.HasSuffix(na, "."+ne) || strings.HasSuffix(ne, "."+na) ||
				strings.HasPrefix(na, ne+".") || strings.HasPrefix(ne, na+".") ||
				strings.HasPrefix(na, ne+"::") || strings.HasPrefix(ne, na+"::") {
				hits++
				break
			}
		}
	}
	return hits, len(expected)
}

// setOverlapFiles returns (hits, total) for file path matching.
// Uses suffix matching so "src/flask/app.py" matches "flask/app.py".
func setOverlapFiles(expected, actual []string) (int, int) {
	hits := 0
	for _, e := range expected {
		ne := normalizeFile(e)
		matched := false
		for _, a := range actual {
			na := normalizeFile(a)
			if na == ne || strings.HasSuffix(na, "/"+ne) || strings.HasSuffix(ne, "/"+na) {
				matched = true
				break
			}
		}
		if matched {
			hits++
		}
	}
	return hits, len(expected)
}

func makeNormSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[normalizeName(item)] = true
	}
	return set
}

func makeNormFileSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[normalizeFile(item)] = true
	}
	return set
}

func matchesFileSet(f string, expectedSet map[string]bool) bool {
	nf := normalizeFile(f)
	if expectedSet[nf] {
		return true
	}
	// Suffix match: actual "src/flask/app.py" should match expected "flask/app.py".
	for expected := range expectedSet {
		if strings.HasSuffix(nf, "/"+expected) || strings.HasSuffix(expected, "/"+nf) {
			return true
		}
	}
	return false
}

// isTestFilePath checks if a file path looks like a test file.
func isTestFilePath(f string) bool {
	fl := strings.ToLower(f)
	return strings.Contains(fl, "/test/") || strings.Contains(fl, "/tests/") ||
		strings.Contains(fl, "/spec/") || strings.Contains(fl, "/__tests__/") ||
		strings.HasPrefix(fl, "test/") || strings.HasPrefix(fl, "tests/") ||
		strings.HasSuffix(fl, "_test.go") || strings.HasSuffix(fl, "_test.py") ||
		strings.HasSuffix(fl, "_test.js") || strings.HasSuffix(fl, ".test.js") ||
		strings.HasSuffix(fl, "_test.ts") || strings.HasSuffix(fl, ".test.ts") ||
		strings.HasSuffix(fl, ".spec.ts") || strings.HasSuffix(fl, ".spec.js") ||
		strings.Contains(fl, "test_") || strings.Contains(fl, "/testdata/") ||
		strings.Contains(fl, "androidtest") || strings.Contains(fl, "robovm-test")
}

func normalizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`*\"'[]")
	return strings.ToLower(s)
}

func normalizeFile(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`*\"'[]")
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimPrefix(s, "/")
	return strings.ToLower(s)
}

func appendUniqueName(names []string, name string) []string {
	norm := normalizeName(name)
	for _, existing := range names {
		if normalizeName(existing) == norm {
			return names
		}
	}
	return append(names, name)
}

func appendUniqueFile(files []string, file string) []string {
	norm := normalizeFile(file)
	for _, existing := range files {
		if normalizeFile(existing) == norm {
			return files
		}
	}
	return append(files, file)
}

// ─── Repo management ─────────────────────────────────────────────────────────

func ensureRepo(reposDir, repo, commit string) (string, error) {
	safeName := strings.ReplaceAll(repo, "/", "_")
	dir := filepath.Join(reposDir, safeName)

	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return "", err
	}

	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		url := fmt.Sprintf("https://github.com/%s.git", repo)
		log.Printf("  cloning %s ...", url)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "clone", "--no-checkout", url, dir)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git clone: %w", err)
		}
	}

	// Try checkout directly first (works for branch names, commit SHAs).
	if tryCheckout(dir, commit) {
		return dir, nil
	}

	// Try as a tag: "tags/<commit>" (handles annotated tags).
	if tryCheckout(dir, "tags/"+commit) {
		return dir, nil
	}

	// Fetch tags and retry.
	fetch := exec.Command("git", "fetch", "--tags", "origin")
	fetch.Dir = dir
	fetch.Stdout = os.Stderr
	fetch.Stderr = os.Stderr
	_ = fetch.Run()

	if tryCheckout(dir, commit) {
		return dir, nil
	}
	if tryCheckout(dir, "tags/"+commit) {
		return dir, nil
	}

	// Last resort: fetch the specific ref.
	fetch2 := exec.Command("git", "fetch", "origin", commit)
	fetch2.Dir = dir
	fetch2.Stdout = os.Stderr
	fetch2.Stderr = os.Stderr
	_ = fetch2.Run()

	if tryCheckout(dir, "FETCH_HEAD") {
		return dir, nil
	}

	return "", fmt.Errorf("git checkout %s: all strategies failed", commit)
}

func tryCheckout(dir, ref string) bool {
	cmd := exec.Command("git", "checkout", ref)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run() == nil
}

// ─── Data loading ────────────────────────────────────────────────────────────

func loadGraphBenchData(path string) ([]GraphBenchSuite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var suites []GraphBenchSuite
	for lineNum, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		var s GraphBenchSuite
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			log.Printf("graphbench: skip bad line %d: %v", lineNum+1, err)
			continue
		}
		if s.Repo == "" || len(s.Tests) == 0 {
			log.Printf("graphbench: skip empty suite at line %d", lineNum+1)
			continue
		}
		suites = append(suites, s)
	}
	if len(suites) == 0 {
		return nil, fmt.Errorf("no valid test suites found in %s", path)
	}
	return suites, nil
}

// ─── Aggregation ─────────────────────────────────────────────────────────────

func aggregateGraphResults(results []GraphBenchTestResult, suites []GraphBenchSuite, stats []repoStats) *reporter.GraphBenchResult {
	byType := make(map[string]*metricAccum)
	byLang := make(map[string]*metricAccum)
	overall := &metricAccum{}
	lspOverall := &metricAccum{} // only tests that have LSP data
	errorCount := 0
	correctCount := 0
	completeCount := 0
	nonErrorCount := 0
	var failedQueries []reporter.FailedQuery
	var allLatencies []int64
	errorCategories := make(map[string]int)

	for _, r := range results {
		if r.Error != "" {
			errorCount++
			cat := r.ErrorCategory
			if cat == "" {
				cat = "unknown"
			}
			errorCategories[cat]++
			failedQueries = append(failedQueries, reporter.FailedQuery{
				Repo:      r.Repo,
				Language:  r.Language,
				QueryType: r.QueryType,
				Query:     r.Query,
				Error:     r.Error,
			})
			continue
		}
		nonErrorCount++
		allLatencies = append(allLatencies, r.LatencyMs)
		if r.Recall > 0 {
			correctCount++
		}
		if r.Recall >= 1.0 {
			completeCount++
		}
		if r.Recall == 0 && (len(r.ExpectedNames) > 0 || len(r.ExpectedFiles) > 0) {
			failedQueries = append(failedQueries, reporter.FailedQuery{
				Repo:      r.Repo,
				Language:  r.Language,
				QueryType: r.QueryType,
				Query:     r.Query,
				Error:     "zero recall",
			})
		}
		if r.ErrorCategory == "empty_response" {
			errorCategories["empty_response"]++
		}

		if byType[r.QueryType] == nil {
			byType[r.QueryType] = &metricAccum{}
		}
		byType[r.QueryType].add(r.Precision, r.Recall, r.F1)

		if byLang[r.Language] == nil {
			byLang[r.Language] = &metricAccum{}
		}
		byLang[r.Language].add(r.Precision, r.Recall, r.F1)

		overall.add(r.Precision, r.Recall, r.F1)

		// Track LSP metrics for tests where LSP data was collected.
		if len(r.LSPNames) > 0 || (r.LSPF1 > 0) {
			lspOverall.add(r.LSPPrec, r.LSPRecall, r.LSPF1)
		}
	}

	var correctness, completeness float64
	if nonErrorCount > 0 {
		correctness = float64(correctCount) / float64(nonErrorCount)
		completeness = float64(completeCount) / float64(nonErrorCount)
	}

	// Compute latency percentiles.
	sort.Slice(allLatencies, func(i, j int) bool { return allLatencies[i] < allLatencies[j] })
	latencyP50, latencyP95, latencyP99 := int64(0), int64(0), int64(0)
	if n := len(allLatencies); n > 0 {
		latencyP50 = allLatencies[n*50/100]
		latencyP95 = allLatencies[n*95/100]
		idx99 := n * 99 / 100
		if idx99 >= n {
			idx99 = n - 1
		}
		latencyP99 = allLatencies[idx99]
	}

	// Convert repo stats.
	var repoStatsOut []reporter.RepoStats
	for _, rs := range stats {
		repoStatsOut = append(repoStatsOut, reporter.RepoStats{
			Repo:        rs.Repo,
			Language:    rs.Language,
			IndexTimeMs: rs.IndexTimeMs,
			NodeCount:   rs.NodeCount,
			EdgeCount:   rs.EdgeCount,
		})
	}

	gbResult := &reporter.GraphBenchResult{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Summary: reporter.GraphBenchMetrics{
			Precision: overall.avgP(),
			Recall:    overall.avgR(),
			F1:        overall.avgF1(),
		},
		TotalTests:      len(results),
		ErrorCount:      errorCount,
		Correctness:     correctness,
		Completeness:    completeness,
		LatencyP50Ms:    latencyP50,
		LatencyP95Ms:    latencyP95,
		LatencyP99Ms:    latencyP99,
		ErrorCategories: errorCategories,
		FailedQueries:   failedQueries,
		RepoStatsData:   repoStatsOut,
	}

	// Populate LSP summary when LSP data was collected.
	if lspOverall.n > 0 {
		lspMetrics := reporter.GraphBenchMetrics{
			Precision: lspOverall.avgP(),
			Recall:    lspOverall.avgR(),
			F1:        lspOverall.avgF1(),
		}
		gbResult.LSPSummary = &lspMetrics
		// Delta: LSP minus baseline for the same tests.
		baselineOnLSPTests := &metricAccum{}
		for _, r := range results {
			if len(r.LSPNames) > 0 || r.LSPF1 > 0 {
				baselineOnLSPTests.add(r.Precision, r.Recall, r.F1)
			}
		}
		if baselineOnLSPTests.n > 0 {
			delta := reporter.GraphBenchMetrics{
				Precision: lspMetrics.Precision - baselineOnLSPTests.avgP(),
				Recall:    lspMetrics.Recall - baselineOnLSPTests.avgR(),
				F1:        lspMetrics.F1 - baselineOnLSPTests.avgF1(),
			}
			gbResult.LSPDelta = &delta
		}
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

	taskResults := make([]interface{}, len(results))
	for i, r := range results {
		taskResults[i] = r
	}
	gbResult.TestResults = taskResults

	return gbResult
}

type metricAccum struct {
	n                 int
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

// ─── Helpers ─────────────────────────────────────────────────────────────────

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// ─── Smoke Mode ─────────────────────────────────────────────────────────────

// runGraphBenchSmoke calls the daemon's MCP benchmark tool (scenario=all) which
// runs the 6 self-validating scenarios against the currently indexed graph.
// No external dataset needed — ground truth is derived from the graph topology.
func runGraphBenchSmoke(client *agent.SynapsesClient) (*reporter.GraphBenchResult, error) {
	log.Printf("graphbench smoke: calling daemon benchmark tool (scenario=all)")

	raw, err := client.RunBenchmark("all")
	if err != nil {
		return nil, fmt.Errorf("daemon benchmark call: %w", err)
	}

	// Parse the benchmark.Result JSON from daemon.
	var bmResult struct {
		Timestamp  string `json:"timestamp"`
		RepoID     string `json:"repo_id"`
		NodeCount  int    `json:"node_count"`
		EdgeCount  int    `json:"edge_count"`
		DurationMs int64  `json:"duration_ms"`
		Summary    struct {
			ScenariosRun     int     `json:"scenarios_run"`
			ScenariosPassed  int     `json:"scenarios_passed"`
			ScenariosErrored int     `json:"scenarios_errored"`
			AvgPrecision     float64 `json:"avg_precision"`
			AvgRecall        float64 `json:"avg_recall"`
			AvgF1            float64 `json:"avg_f1"`
			AvgLatencyMs     float64 `json:"avg_latency_ms"`
			P95LatencyMs     float64 `json:"p95_latency_ms"`
		} `json:"summary"`
		Scenarios []struct {
			Name         string  `json:"name"`
			Description  string  `json:"description"`
			Passed       bool    `json:"passed"`
			AvgPrecision float64 `json:"avg_precision"`
			AvgRecall    float64 `json:"avg_recall"`
			AvgF1        float64 `json:"avg_f1"`
			AvgLatencyMs float64 `json:"avg_latency_ms"`
			Error        string  `json:"error"`
			Queries      []struct {
				Label     string  `json:"label"`
				Precision float64 `json:"precision"`
				Recall    float64 `json:"recall"`
				F1        float64 `json:"f1"`
				LatencyMs float64 `json:"latency_ms"`
				Expected  int     `json:"expected"`
				Returned  int     `json:"returned"`
				Relevant  int     `json:"relevant"`
			} `json:"queries"`
		} `json:"scenarios"`
	}

	if err := json.Unmarshal([]byte(raw), &bmResult); err != nil {
		return nil, fmt.Errorf("parse benchmark response: %w", err)
	}

	// Convert to reporter.GraphBenchResult.
	result := &reporter.GraphBenchResult{
		Timestamp:   bmResult.Timestamp,
		Mode:        "smoke",
		Correctness: float64(bmResult.Summary.ScenariosPassed) / float64(max(bmResult.Summary.ScenariosRun, 1)),
		Summary: reporter.GraphBenchMetrics{
			Precision: bmResult.Summary.AvgPrecision,
			Recall:    bmResult.Summary.AvgRecall,
			F1:        bmResult.Summary.AvgF1,
		},
	}

	// Map scenarios to by_query_type slices.
	var totalTests int
	var errorCount int
	for _, sc := range bmResult.Scenarios {
		totalTests += len(sc.Queries)
		if sc.Error != "" {
			errorCount++
		}
		// Compute per-scenario precision/recall from individual queries if available.
		var scP, scR float64
		if len(sc.Queries) > 0 {
			for _, q := range sc.Queries {
				scP += q.Precision
				scR += q.Recall
			}
			scP /= float64(len(sc.Queries))
			scR /= float64(len(sc.Queries))
		} else {
			scP = sc.AvgPrecision
			scR = sc.AvgRecall
		}
		result.ByQueryType = append(result.ByQueryType, reporter.GraphBenchSlice{
			Label: sc.Name,
			Tests: len(sc.Queries),
			Metrics: reporter.GraphBenchMetrics{
				Precision: scP,
				Recall:    scR,
				F1:        sc.AvgF1,
			},
		})
	}
	result.TotalTests = totalTests
	result.ErrorCount = errorCount

	// Add repo stats.
	result.RepoStatsData = []reporter.RepoStats{{
		Repo:      bmResult.RepoID,
		Language:  "mixed",
		NodeCount: bmResult.NodeCount,
		EdgeCount: bmResult.EdgeCount,
	}}

	log.Printf("graphbench smoke: %d scenarios, %d passed, avg F1=%.1f%%",
		bmResult.Summary.ScenariosRun, bmResult.Summary.ScenariosPassed,
		bmResult.Summary.AvgF1*100)

	return result, nil
}

// GraphBench — Graph Accuracy Benchmark (Benchmark A).
//
// Tests whether Synapses' structural graph correctly represents code
// relationships. Unlike ContextBench (which conflates graph quality with
// retrieval strategy), GraphBench isolates graph correctness.
//
// Query types:
//   - find_callers(symbol)        — who calls this? via get_impact depth=1
//   - find_callees(symbol)        — what does this call? via get_context format=json
//   - find_imports(file)          — what does this file import? via get_context format=json
//   - impact_analysis(symbol)     — what's affected? via get_impact depth=3
//   - find_implementations(iface) — who implements this? via get_context format=json
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
	Error         string   `json:"error,omitempty"`
	RawResponse   string   `json:"raw_response,omitempty"` // for debugging failures
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
		log.Printf("[%d/%d] %s @ %s (%s, %d tests)",
			i+1, len(suites), suite.Repo, suite.Commit, suite.Language, len(suite.Tests))

		repoDir, err := ensureRepo(opts.ReposDir, suite.Repo, suite.Commit)
		if err != nil {
			log.Printf("  SKIP: clone/checkout failed: %v", err)
			for _, t := range suite.Tests {
				allResults = append(allResults, GraphBenchTestResult{
					Repo: suite.Repo, Language: suite.Language,
					QueryType: t.QueryType, Query: t.Query,
					Error: fmt.Sprintf("repo setup: %v", err),
				})
			}
			continue
		}

		projClient := client.WithProject(repoDir)

		// Trigger indexing and wait for it to complete.
		if err := waitForIndex(projClient, repoDir); err != nil {
			log.Printf("  warning: indexing may be incomplete: %v", err)
		}

		for j, test := range suite.Tests {
			result := runGraphTest(projClient, suite, test)
			allResults = append(allResults, result)
			status := "✓"
			if result.Error != "" {
				status = "✗ " + truncate(result.Error, 80)
			}
			log.Printf("  [%d/%d] %s(%s): P=%.0f%% R=%.0f%% F1=%.0f%% %s",
				j+1, len(suite.Tests), test.QueryType, test.Query,
				result.Precision*100, result.Recall*100, result.F1*100, status)
		}
	}

	return aggregateGraphResults(allResults, suites), nil
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
func runGraphTest(client *agent.SynapsesClient, suite GraphBenchSuite, test GraphBenchTest) GraphBenchTestResult {
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

	switch test.QueryType {
	case "find_callers":
		// Use get_context (callers field) — much more precise than get_impact BFS.
		names, files, rawResp = queryContextCallers(client, test.Query)

	case "find_callees":
		names, files, rawResp = queryContextCallees(client, test.Query)

	case "find_imports":
		// File-level entities don't resolve in the daemon (root: null).
		// Strategy: search for symbols defined in the file, then aggregate
		// their callees' source packages as proxy for imports.
		names, files, rawResp = queryFileImports(client, test.Query)

	case "impact_analysis":
		names, files, rawResp = queryImpact(client, test.Query, 1)

	case "find_implementations":
		names, files, rawResp = queryContextRelated(client, test.Query)

	default:
		result.Error = fmt.Sprintf("unknown query_type %q", test.QueryType)
		return result
	}

	result.ActualNames = names
	result.ActualFiles = files
	if rawResp == "" {
		rawResp = "(empty response)"
	}
	// Store truncated raw response for debugging.
	result.RawResponse = truncate(rawResp, 500)

	// Check for error responses.
	if strings.Contains(rawResp, "entity not found") {
		result.Error = "entity not found"
		return result
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

	// Fallback: if no imports found, try callees of related symbols (old approach).
	if len(names) == 0 && len(files) == 0 {
		var symbols []string
		for _, rel := range cr.Related {
			if rel.Node.Name != "" {
				symbols = append(symbols, rel.Node.Name)
			}
		}
		if len(symbols) > 3 {
			symbols = symbols[:3]
		}
		var allRaw strings.Builder
		allRaw.WriteString(raw)
		for _, sym := range symbols {
			symRaw, err := client.GetContextJSON("graphbench", sym, "full")
			if err != nil {
				continue
			}
			allRaw.WriteString("\n---\n")
			allRaw.WriteString(symRaw)
			var symCR contextResponse
			if err := json.Unmarshal([]byte(symRaw), &symCR); err != nil {
				continue
			}
			for _, callee := range symCR.Callees {
				if callee.Node.Name != "" {
					pkg := extractPackageName(callee.Node.Name, filePath)
					if pkg != "" && !seenNames[strings.ToLower(pkg)] {
						seenNames[strings.ToLower(pkg)] = true
						names = append(names, pkg)
					}
				}
				if callee.Node.File != "" && !seenFiles[normalizeFile(callee.Node.File)] {
					seenFiles[normalizeFile(callee.Node.File)] = true
					files = append(files, callee.Node.File)
				}
			}
		}
		raw = truncate(allRaw.String(), 2000)
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
		".c", ".h", ".cpp", ".hpp", ".cc", ".hh", ".cs", ".swift", ".kt":
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

func aggregateGraphResults(results []GraphBenchTestResult, suites []GraphBenchSuite) *reporter.GraphBenchResult {
	byType := make(map[string]*metricAccum)
	byLang := make(map[string]*metricAccum)
	overall := &metricAccum{}
	errorCount := 0

	for _, r := range results {
		if r.Error != "" {
			errorCount++
			continue
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
	}

	gbResult := &reporter.GraphBenchResult{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Summary: reporter.GraphBenchMetrics{
			Precision: overall.avgP(),
			Recall:    overall.avgR(),
			F1:        overall.avgF1(),
		},
		TotalTests: len(results),
		ErrorCount: errorCount,
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

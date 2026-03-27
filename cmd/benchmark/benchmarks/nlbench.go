// NLBench — Natural Language Parsing Benchmark.
//
// Tests whether Synapses correctly parses documentation files (.md, .txt, etc.)
// into knowledge graph nodes and can surface them via search and get_context.
//
// Query types:
//   - find_doc_entities(file)    — search for a doc file, check expected entity names
//   - doc_explains_code(symbol)  — get_context for a code entity, check cross_domain docs
//   - concept_search(query)      — search for a concept, check expected names in results
//
// Metrics: per-test Precision, Recall, F1. Aggregated by query_type and language.
package benchmarks

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/cmd/benchmark/agent"
	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

// ─── Data types ──────────────────────────────────────────────────────────────

// NLBenchOptions controls an NLBench run.
type NLBenchOptions struct {
	DataFile string // path to nlbench.jsonl
	ReposDir string // where repos are cloned
	Limit    int    // max test suites (0 = all)
}

// NLBenchSuite is one line from the JSONL file.
type NLBenchSuite struct {
	Repo     string        `json:"repo"`
	Commit   string        `json:"commit"`
	Language string        `json:"language"`
	Tests    []NLBenchTest `json:"tests"`
}

// NLBenchTest is a single query+expected pair.
type NLBenchTest struct {
	QueryType    string   `json:"query_type"`
	Query        string   `json:"query"`
	ExpectedNames []string `json:"expected_names,omitempty"`
	ExpectedDocs  []string `json:"expected_docs,omitempty"`
	Description   string   `json:"description"`
}

// NLBenchTestResult holds the outcome of one test.
type NLBenchTestResult struct {
	Repo          string   `json:"repo"`
	Language      string   `json:"language"`
	QueryType     string   `json:"query_type"`
	Query         string   `json:"query"`
	Description   string   `json:"description"`
	ExpectedNames []string `json:"expected_names,omitempty"`
	ExpectedDocs  []string `json:"expected_docs,omitempty"`
	ActualNames   []string `json:"actual_names"`
	Precision     float64  `json:"precision"`
	Recall        float64  `json:"recall"`
	F1            float64  `json:"f1"`
	Error         string   `json:"error,omitempty"`
	RawResponse   string   `json:"raw_response,omitempty"`
}

// ─── Runner ──────────────────────────────────────────────────────────────────

// RunNLBench runs the full NL benchmark and returns a reporter-compatible result.
func RunNLBench(client *agent.SynapsesClient, opts NLBenchOptions) (*reporter.NLBenchResult, error) {
	suites, err := loadNLBenchData(opts.DataFile)
	if err != nil {
		return nil, fmt.Errorf("load data: %w", err)
	}
	if opts.Limit > 0 && len(suites) > opts.Limit {
		suites = suites[:opts.Limit]
	}

	log.Printf("nlbench: %d repo suites loaded", len(suites))

	var allResults []NLBenchTestResult

	for i, suite := range suites {
		log.Printf("[%d/%d] %s @ %s (%s, %d tests)",
			i+1, len(suites), suite.Repo, suite.Commit, suite.Language, len(suite.Tests))

		repoDir, err := ensureRepo(opts.ReposDir, suite.Repo, suite.Commit)
		if err != nil {
			log.Printf("  SKIP: clone/checkout failed: %v", err)
			for _, t := range suite.Tests {
				allResults = append(allResults, NLBenchTestResult{
					Repo: suite.Repo, Language: suite.Language,
					QueryType: t.QueryType, Query: t.Query,
					Description: t.Description,
					Error:       fmt.Sprintf("repo setup: %v", err),
				})
			}
			continue
		}

		projClient := client.WithProject(repoDir)

		if err := waitForIndex(projClient, repoDir); err != nil {
			log.Printf("  warning: indexing may be incomplete: %v", err)
		}

		for j, test := range suite.Tests {
			result := runNLTest(projClient, suite, test)
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

	return aggregateNLResults(allResults), nil
}

// runNLTest executes a single NL test case against the daemon.
func runNLTest(client *agent.SynapsesClient, suite NLBenchSuite, test NLBenchTest) NLBenchTestResult {
	result := NLBenchTestResult{
		Repo:          suite.Repo,
		Language:      suite.Language,
		QueryType:     test.QueryType,
		Query:         test.Query,
		Description:   test.Description,
		ExpectedNames: test.ExpectedNames,
		ExpectedDocs:  test.ExpectedDocs,
	}

	var names []string
	var rawResp string

	switch test.QueryType {
	case "find_doc_entities":
		names, rawResp = nlQueryDocEntities(client, test.Query)

	case "doc_explains_code":
		names, rawResp = nlQueryDocExplainsCode(client, test.Query, test.ExpectedDocs)

	case "concept_search":
		names, rawResp = nlQueryConceptSearch(client, test.Query)

	case "find_relevant_docs":
		names, rawResp = nlQueryFindRelevantDocs(client, test.Query)

	default:
		result.Error = fmt.Sprintf("unknown query_type %q", test.QueryType)
		return result
	}

	result.ActualNames = names
	if rawResp == "" {
		rawResp = "(empty response)"
	}
	result.RawResponse = truncate(rawResp, 500)

	// Compute metrics.
	// For doc_explains_code: expected has both names and docs. We score against
	// the combined set of actual returned items.
	expected := test.ExpectedNames
	if test.QueryType == "doc_explains_code" {
		// For doc_explains_code, the actual names include both code entities and
		// doc keywords extracted. Score docs separately as a bonus.
		expected = append(append([]string{}, test.ExpectedNames...), test.ExpectedDocs...)
	}

	if len(expected) > 0 {
		recallHits, recallTotal := nlSetOverlap(expected, names)
		result.Recall = float64(recallHits) / float64(recallTotal)

		if len(names) > 0 {
			precHits := 0
			for _, n := range names {
				if nlMatchesAny(n, expected) {
					precHits++
				}
			}
			result.Precision = float64(precHits) / float64(len(names))
		}
	}

	if result.Precision+result.Recall > 0 {
		result.F1 = 2 * result.Precision * result.Recall / (result.Precision + result.Recall)
	}

	return result
}

// ─── Query helpers ───────────────────────────────────────────────────────────

// nlQueryDocEntities uses get_context on a doc file to extract what entities
// and sections it contains. Falls back to search if get_context doesn't resolve.
func nlQueryDocEntities(client *agent.SynapsesClient, docFile string) (names []string, raw string) {
	// Strategy 1: get_context on the doc file — returns sections and cross-domain links.
	ctxRaw, err := client.GetContextJSON("nlbench", docFile, "full")
	if err == nil && !strings.Contains(ctxRaw, "\"root\":null") && len(ctxRaw) > 50 {
		raw = ctxRaw
		var cr contextResponse
		if err := json.Unmarshal([]byte(ctxRaw), &cr); err == nil {
			// Extract section names (split on § to get topic).
			for _, rel := range cr.Related {
				names = appendSectionWords(names, rel.Node.Name)
			}
			// Cross-domain references — code entities this doc links to.
			for _, doc := range cr.CrossDomain.DocumentedIn {
				if doc.Name != "" {
					names = append(names, doc.Name)
				}
			}
			for _, m := range cr.CrossDomain.Mentions {
				if m.Name != "" {
					names = append(names, m.Name)
				}
			}
			for _, r := range cr.CrossDomain.Related {
				if r.Name != "" {
					names = append(names, r.Name)
					names = appendSectionWords(names, r.Name)
				}
			}
			// Callees = things this doc's entities reference.
			for _, c := range cr.Callees {
				if c.Node.Name != "" {
					names = append(names, c.Node.Name)
				}
			}
			if len(names) > 0 {
				return dedup(names), raw
			}
		}
	}

	// Strategy 2: search for the filename — returns matching sections.
	resp, err := client.Search("nlbench", docFile)
	if err != nil {
		return nil, fmt.Sprintf("error: %v", err)
	}
	raw = resp.Text
	names = extractNLNamesFromSearch(raw)
	return names, raw
}

// nlQueryDocExplainsCode queries get_context for a code entity and extracts
// cross-domain doc references.
func nlQueryDocExplainsCode(client *agent.SynapsesClient, entity string, expectedDocs []string) (names []string, raw string) {
	raw, err := client.GetContextJSON("nlbench", entity, "full")
	if err != nil {
		return nil, fmt.Sprintf("error: %v", err)
	}

	var cr contextResponse
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		return extractNLKeywords(raw), raw
	}

	// Collect the root entity name.
	if cr.Root.Name != "" {
		names = append(names, cr.Root.Name)
	}

	// Collect cross-domain doc references — this is the core signal.
	for _, doc := range cr.CrossDomain.DocumentedIn {
		if doc.Name != "" {
			names = append(names, doc.Name)
			// Also extract meaningful words from section names.
			names = appendSectionWords(names, doc.Name)
		}
	}
	for _, rel := range cr.CrossDomain.Related {
		if rel.Name != "" {
			names = append(names, rel.Name)
			names = appendSectionWords(names, rel.Name)
		}
	}
	for _, m := range cr.CrossDomain.Mentions {
		if m.Name != "" {
			names = append(names, m.Name)
			names = appendSectionWords(names, m.Name)
		}
	}

	// Related nodes (may include doc sections linked via embedding).
	for _, rel := range cr.Related {
		if rel.Node.Name != "" {
			names = append(names, rel.Node.Name)
		}
	}

	// Callees — functions this entity calls (useful context).
	for _, c := range cr.Callees {
		if c.Node.Name != "" {
			names = append(names, c.Node.Name)
		}
	}

	return dedup(names), raw
}

// nlQueryConceptSearch searches for a concept and extracts matching names.
func nlQueryConceptSearch(client *agent.SynapsesClient, query string) (names []string, raw string) {
	resp, err := client.Search("nlbench", query)
	if err != nil {
		return nil, fmt.Sprintf("error: %v", err)
	}
	raw = resp.Text
	names = extractNLNamesFromSearch(raw)
	return names, raw
}

// nlQueryFindRelevantDocs checks what documentation sections explain a code entity.
// This is the reverse of doc_explains_code: given code, find docs about it.
// Strategy: get_context first (cross-domain links), then search as fallback.
func nlQueryFindRelevantDocs(client *agent.SynapsesClient, entity string) (names []string, raw string) {
	raw, err := client.GetContextJSON("nlbench", entity, "full")
	if err == nil {
		var cr contextResponse
		if err := json.Unmarshal([]byte(raw), &cr); err == nil {
			// Extract from direct documentation field first.
			for _, doc := range cr.Documentation {
				if doc.Node.File != "" {
					names = append(names, doc.Node.File)
				}
				if doc.Node.Name != "" {
					names = append(names, doc.Node.Name)
					names = appendSectionWords(names, doc.Node.Name)
				}
			}
			// Extract doc file names and section titles from cross-domain.
			for _, doc := range cr.CrossDomain.DocumentedIn {
				if doc.File != "" {
					names = append(names, doc.File)
				}
				if doc.Name != "" {
					names = append(names, doc.Name)
					names = appendSectionWords(names, doc.Name)
				}
			}
			for _, m := range cr.CrossDomain.Mentions {
				if m.File != "" {
					names = append(names, m.File)
				}
				if m.Name != "" {
					names = appendSectionWords(names, m.Name)
				}
			}
			for _, r := range cr.CrossDomain.Related {
				if r.File != "" {
					names = append(names, r.File)
				}
				if r.Name != "" {
					names = appendSectionWords(names, r.Name)
				}
			}
		}
	}

	// Fallback: search for the entity name — may find doc sections mentioning it.
	if len(names) == 0 {
		resp, err := client.Search("nlbench", entity)
		if err == nil {
			if raw == "" {
				raw = resp.Text
			}
			// Extract doc-domain results (sections, not code entities).
			searchNames := extractNLNamesFromSearch(resp.Text)
			for _, n := range searchNames {
				nl := strings.ToLower(n)
				// Only keep results that look like doc sections or doc files.
				if strings.Contains(nl, "§") || strings.HasSuffix(nl, ".md") ||
					strings.HasSuffix(nl, ".rst") || strings.HasSuffix(nl, ".txt") {
					names = append(names, n)
					names = appendSectionWords(names, n)
				}
			}
		}
	}

	return dedup(names), raw
}

// ─── Response parsers ────────────────────────────────────────────────────────

// extractNLNamesFromSearch extracts entity/concept names from a search response.
func extractNLNamesFromSearch(text string) []string {
	var names []string
	seen := make(map[string]bool)

	var resp struct {
		Count   int `json:"count"`
		Results []struct {
			Name string `json:"name"`
			File string `json:"file"`
			Type string `json:"type"`
			Doc  string `json:"doc"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err == nil {
		// If 0 results, return empty — don't fall through to keyword extraction
		// which would just pick up JSON field names.
		if resp.Count == 0 || len(resp.Results) == 0 {
			return nil
		}
		for _, r := range resp.Results {
			if r.Name == "" {
				continue
			}
			n := strings.ToLower(r.Name)
			if !seen[n] {
				seen[n] = true
				names = append(names, r.Name)
			}
			// For section names like "api.rst § Template Rendering",
			// also extract the meaningful words after §.
			names = appendSectionWords(names, r.Name)
		}
		return dedup(names)
	}

	return nil // don't fallback to keyword extraction on parse failure
}

// appendSectionWords extracts meaningful words from a section name.
// E.g., "CONTRIBUTING.rst § Running the tests" → ["running", "tests"]
// E.g., "errorhandling.rst § Handling Application Errors" → ["handling", "application", "errors", "errorhandling"]
func appendSectionWords(names []string, sectionName string) []string {
	// Extract text after § if present.
	text := sectionName
	if idx := strings.Index(sectionName, "§"); idx >= 0 {
		text = sectionName[idx+len("§"):]
		// Also extract the filename prefix (before .rst/.md) as a keyword.
		prefix := strings.TrimSpace(sectionName[:idx])
		if dot := strings.LastIndex(prefix, "."); dot > 0 {
			prefix = prefix[:dot]
		}
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if len(prefix) >= 3 && !nlBenchStopWords[prefix] {
			names = append(names, prefix)
		}
	}

	for _, word := range strings.Fields(text) {
		word = strings.Trim(word, ".,;:!?()[]{}\"'`*|#>-/")
		word = strings.ToLower(word)
		if len(word) >= 3 && !nlBenchStopWords[word] {
			names = append(names, word)
		}
	}
	return names
}

// extractNLKeywords extracts meaningful words from raw text as a last-resort fallback.
func extractNLKeywords(text string) []string {
	seen := make(map[string]bool)
	var names []string
	for _, word := range strings.Fields(text) {
		word = strings.Trim(word, ".,;:!?()[]{}\"'`*|#>-\\")
		word = strings.ToLower(word)
		if len(word) < 3 || nlBenchStopWords[word] {
			continue
		}
		if !seen[word] {
			seen[word] = true
			names = append(names, word)
		}
	}
	return names
}

// dedup removes duplicate names (case-insensitive).
func dedup(names []string) []string {
	seen := make(map[string]bool, len(names))
	var out []string
	for _, n := range names {
		key := strings.ToLower(n)
		if !seen[key] {
			seen[key] = true
			out = append(out, n)
		}
	}
	return out
}

// nlBenchStopWords are common words to skip in keyword extraction.
var nlBenchStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "that": true, "with": true,
	"this": true, "from": true, "are": true, "was": true, "were": true,
	"been": true, "have": true, "has": true, "had": true, "not": true,
	"but": true, "can": true, "will": true, "all": true, "its": true,
	"you": true, "your": true, "they": true, "their": true, "which": true,
	"when": true, "where": true, "how": true, "who": true, "what": true,
	"than": true, "then": true, "also": true, "into": true, "more": true,
	"some": true, "such": true, "only": true, "other": true, "each": true,
}

// ─── NL set matching ─────────────────────────────────────────────────────────

// nlSetOverlap returns (hits, total) for NL-style fuzzy matching.
// expected items are matched against actual using case-insensitive substring matching.
func nlSetOverlap(expected, actual []string) (int, int) {
	hits := 0
	for _, e := range expected {
		if nlMatchesAny(e, actual) {
			hits++
		}
	}
	return hits, len(expected)
}

// nlMatchesAny checks if needle matches any item in haystack.
// Uses case-insensitive exact match, prefix/suffix match, and substring containment.
func nlMatchesAny(needle string, haystack []string) bool {
	nn := strings.ToLower(strings.TrimSpace(needle))
	for _, h := range haystack {
		nh := strings.ToLower(strings.TrimSpace(h))
		// Exact match.
		if nn == nh {
			return true
		}
		// Substring containment (either direction).
		if strings.Contains(nh, nn) || strings.Contains(nn, nh) {
			return true
		}
		// Dot/underscore separated suffix match (e.g., "Flask.run" matches "run").
		for _, sep := range []string{".", "_", "::", "/"} {
			if strings.HasSuffix(nh, sep+nn) || strings.HasSuffix(nn, sep+nh) {
				return true
			}
		}
	}
	return false
}

// ─── Data loading ────────────────────────────────────────────────────────────

func loadNLBenchData(path string) ([]NLBenchSuite, error) {
	data, err := loadNLBenchFile(path)
	if err != nil {
		return nil, err
	}
	var suites []NLBenchSuite
	for lineNum, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		var s NLBenchSuite
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			log.Printf("nlbench: skip bad line %d: %v", lineNum+1, err)
			continue
		}
		if s.Repo == "" || len(s.Tests) == 0 {
			log.Printf("nlbench: skip empty suite at line %d", lineNum+1)
			continue
		}
		suites = append(suites, s)
	}
	if len(suites) == 0 {
		return nil, fmt.Errorf("no valid test suites found in %s", path)
	}
	return suites, nil
}

func loadNLBenchFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// ─── Aggregation ─────────────────────────────────────────────────────────────

func aggregateNLResults(results []NLBenchTestResult) *reporter.NLBenchResult {
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

	result := &reporter.NLBenchResult{
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
		result.ByQueryType = append(result.ByQueryType, reporter.GraphBenchSlice{
			Label:   qt,
			Tests:   acc.n,
			Metrics: reporter.GraphBenchMetrics{Precision: acc.avgP(), Recall: acc.avgR(), F1: acc.avgF1()},
		})
	}

	for lang, acc := range byLang {
		result.ByLanguage = append(result.ByLanguage, reporter.GraphBenchSlice{
			Label:   lang,
			Tests:   acc.n,
			Metrics: reporter.GraphBenchMetrics{Precision: acc.avgP(), Recall: acc.avgR(), F1: acc.avgF1()},
		})
	}

	taskResults := make([]interface{}, len(results))
	for i, r := range results {
		taskResults[i] = r
	}
	result.TestResults = taskResults

	return result
}

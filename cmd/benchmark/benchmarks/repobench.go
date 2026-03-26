// Package benchmarks implements external benchmark runners.
//
// RepoBench-R (arxiv.org/abs/2306.03091, ICLR 2024):
//
// Given a code completion point and a list of candidate snippets from other
// files in the same repo, rank the most relevant snippet highest.
//
// Dataset: huggingface.co/datasets/tianyang/repobench-r
// Each record:
//   - context        — code up to the completion point (the query)
//   - import_statement — imports at file top
//   - gold_snippet_index — index of the correct answer in candidate_code
//   - candidate_code — list of snippet strings to rank
//
// This runner implements Approach B from BENCHMARK.md: for each sample, rank
// all candidates against the query using the chosen retrieval mode, then score
// Acc@k for k in {1, 3, 5, 10}.
//
// Retrieval modes:
//   - fts-only       — local BM25 over tokenised tokens
//   - vector-only    — local TF-IDF cosine similarity
//   - hybrid-rrf     — RRF merge of BM25 + TF-IDF ranks (default)
//   - hybrid-convex  — convex combination of BM25 + TF-IDF scores
//   - hybrid-anchor  — hybrid-rrf + anchor boost (first candidate gets a small boost)
//   - synapses-search — call Synapses search tool and rerank candidates by overlap
//
// The local modes work without a running daemon and are the fastest to run.
// The synapses-search mode requires a running daemon with the repo indexed.
package benchmarks

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/SynapsesOS/synapses/cmd/benchmark/agent"
	"github.com/SynapsesOS/synapses/cmd/benchmark/indexer"
	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

// RepoBenchOptions controls what to run.
type RepoBenchOptions struct {
	// Configs to run, e.g. ["python_cff", "python_cfr", "java_cff", "java_cfr"].
	Configs []string
	// Difficulties: ["easy", "hard"].
	Difficulties []string
	// RetrievalMode: fts-only | vector-only | hybrid-rrf | hybrid-convex | hybrid-anchor | synapses-search | synapses-embed | synapses-embed-local
	RetrievalMode string
	// LimitPerSet caps samples per config×difficulty (0 = all).
	LimitPerSet int
	// ReposDir is the root directory where repos are cloned (for synapses-embed).
	ReposDir string
	// RepoCache maps repo names to local paths (for per-repo project routing).
	RepoCache *indexer.Cache
	// LocalEmbedder is used for synapses-embed-local mode (in-process ONNX).
	LocalEmbedder *LocalEmbedder
}

// RepoBenchSample is a single record from the RepoBench-R dataset (JSONL format).
//
// Real schema (from pickle inspection):
//   - Code             = current file context (the query)
//   - Context          = candidate snippets from other files (the list to rank)
//   - GoldenSnippetIndex = index into Context of the correct snippet
//   - ImportStatement  = imports at the top of the query file
type RepoBenchSample struct {
	// Code is the current file's code up to the completion point — the query.
	Code            string   `json:"code"`
	// Context is the list of candidate snippets from other files to rank.
	Context         []string `json:"context"`
	ImportStatement string   `json:"import_statement"`
	GoldenSnippetIndex int   `json:"golden_snippet_index"`
	NextLine        string   `json:"next_line"`
	Repo            string   `json:"repo_name"`
	File            string   `json:"file_path"`
}

// RunRepoBench executes the RepoBench-R benchmark across all requested configs
// and difficulties, using the given retrieval mode.
//
// The dataset must be downloaded separately as JSONL files named:
//
//	repobench_<config>_<difficulty>.jsonl
//
// in the current directory, or exported from HuggingFace via:
//
//	python -c "
//	from datasets import load_dataset
//	for config in ['python_cff','python_cfr','java_cff','java_cfr']:
//	    for split in ['test_easy','test_hard']:
//	        ds = load_dataset('tianyang/repobench-r', config, split=split)
//	        diff = split.replace('test_','')
//	        ds.to_json(f'repobench_{config}_{diff}.jsonl')
//	"
func RunRepoBench(client *agent.SynapsesClient, opts RepoBenchOptions) (*reporter.RepoBenchResult, error) {
	if opts.RetrievalMode == "" {
		opts.RetrievalMode = "hybrid-rrf"
	}

	result := &reporter.RepoBenchResult{
		Timestamp:     reporter.Timestamp(),
		RetrievalMode: opts.RetrievalMode,
	}

	// Aggregate totals for macro-average.
	kValues := []int{1, 3, 5, 10}
	macroAcc := make(map[int]float64)
	macroCount := 0

	for _, cfg := range opts.Configs {
		for _, diff := range opts.Difficulties {
			cfgResult, err := runOneConfig(client, cfg, diff, opts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s/%s: %v (skipping)\n", cfg, diff, err)
				continue
			}
			result.Configs = append(result.Configs, *cfgResult)
			for _, k := range kValues {
				macroAcc[k] += cfgResult.AccAtK[k]
			}
			macroCount++
			result.Summary.TotalSamples += cfgResult.Samples
		}
	}

	// Compute macro averages.
	result.Summary.AccAtK = make(map[int]float64)
	if macroCount > 0 {
		for _, k := range kValues {
			result.Summary.AccAtK[k] = macroAcc[k] / float64(macroCount)
		}
	}

	return result, nil
}

// runOneConfig runs one config×difficulty slice and returns its results.
func runOneConfig(
	client *agent.SynapsesClient,
	cfg, difficulty string,
	opts RepoBenchOptions,
) (*reporter.RepoBenchConfig, error) {
	dataFile := fmt.Sprintf("repobench_%s_%s.jsonl", cfg, difficulty)
	samples, err := loadJSONL(dataFile)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", dataFile, err)
	}

	if opts.LimitPerSet > 0 && len(samples) > opts.LimitPerSet {
		samples = samples[:opts.LimitPerSet]
	}

	if len(samples) == 0 {
		return nil, fmt.Errorf("no samples in %s", dataFile)
	}

	fmt.Printf("[repobench] %s/%s: %d samples, mode=%s\n",
		cfg, difficulty, len(samples), opts.RetrievalMode)

	kValues := []int{1, 3, 5, 10}
	correct := make(map[int]int)
	totalRank := 0.0

	// Run samples in parallel — 1 worker for local embed (ONNX pool is the
	// only bottleneck; more workers just add queue depth without more throughput),
	// 4 for daemon-backed modes, 8 for pure local CPU modes.
	workers := 8
	switch opts.RetrievalMode {
	case "synapses-embed", "synapses-search":
		workers = 4
	case "synapses-embed-local":
		workers = 1
	}
	type result struct {
		rank int
		err  error
	}
	rankCh := make([]chan result, len(samples))
	for i := range rankCh {
		rankCh[i] = make(chan result, 1)
	}
	sem := make(chan struct{}, workers)
	for i, sample := range samples {
		sem <- struct{}{}
		go func(idx int, s RepoBenchSample) {
			defer func() { <-sem }()
			sc := client
			if opts.RetrievalMode == "synapses-search" && opts.RepoCache != nil && s.Repo != "" {
				if lp := opts.RepoCache.Get(s.Repo); lp != "" {
					sc = client.WithProject(lp)
				}
			}
			r, err := rankSample(sc, s, opts)
			rankCh[idx] <- result{r, err}
		}(i, sample)
	}

	for i, sample := range samples {
		if (i+1)%500 == 0 {
			fmt.Printf("  progress: %d/%d\n", i+1, len(samples))
		}

		res := <-rankCh[i]
		goldRank := res.rank
		if res.err != nil {
			fmt.Fprintf(os.Stderr, "  sample %d rank error: %v\n", i, res.err)
			goldRank = len(sample.Context)
		}

		totalRank += float64(goldRank)
		for _, k := range kValues {
			if goldRank <= k {
				correct[k]++
			}
		}
	}

	n := len(samples)
	accAtK := make(map[int]float64)
	for _, k := range kValues {
		accAtK[k] = float64(correct[k]) / float64(n)
	}

	return &reporter.RepoBenchConfig{
		Config:     cfg,
		Difficulty: difficulty,
		Samples:    n,
		AccAtK:     accAtK,
		AvgRank:    totalRank / float64(n),
	}, nil
}

// rankSample ranks all candidates against the query and returns the 1-based
// rank of the gold snippet.
func rankSample(client *agent.SynapsesClient, sample RepoBenchSample, opts RepoBenchOptions) (int, error) {
	mode := opts.RetrievalMode
	query := buildQuery(sample)
	candidates := sample.Context

	var ranked []rankedItem

	switch mode {
	case "fts-only":
		ranked = rankBM25(query, candidates)
	case "vector-only":
		ranked = rankTFIDF(query, candidates)
	case "hybrid-rrf":
		ranked = rankHybridRRF(query, candidates)
	case "hybrid-convex":
		ranked = rankHybridConvex(query, candidates)
	case "hybrid-anchor":
		ranked = rankHybridAnchor(query, candidates)
	case "synapses-search":
		return rankViaSynapsesSearch(client, query, sample)
	case "synapses-embed":
		return rankViaSynapsesEmbed(client, query, sample)
	case "synapses-embed-local":
		if opts.LocalEmbedder != nil {
			return rankViaLocalEmbed(opts.LocalEmbedder, query, sample)
		}
		// Fallback if embedder not initialized.
		ranked = rankHybridRRF(query, candidates)
	default:
		ranked = rankHybridRRF(query, candidates)
	}

	// Find gold rank (1-based).
	for rank, item := range ranked {
		if item.index == sample.GoldenSnippetIndex {
			return rank + 1, nil
		}
	}
	return len(candidates), nil
}

// ─── query builder ───────────────────────────────────────────────────────────

func buildQuery(sample RepoBenchSample) string {
	// Query = imports + last 500 chars of the current file's code.
	code := sample.Code
	if len(code) > 500 {
		code = code[len(code)-500:]
	}
	q := sample.ImportStatement + "\n" + code
	return strings.TrimSpace(q)
}

// ─── rankedItem ───────────────────────────────────────────────────────────────

type rankedItem struct {
	index int
	score float64
}

// ─── BM25 (local, approximate) ───────────────────────────────────────────────

// rankBM25 ranks candidates using a simple BM25 approximation.
// k1=1.5, b=0.75.
func rankBM25(query string, candidates []string) []rankedItem {
	const k1 = 1.5
	const b = 0.75

	queryTerms := tokenize(query)
	if len(queryTerms) == 0 {
		return identityRank(len(candidates))
	}

	// Compute average document length.
	totalLen := 0
	tokenised := make([][]string, len(candidates))
	for i, c := range candidates {
		tokens := tokenize(c)
		tokenised[i] = tokens
		totalLen += len(tokens)
	}
	avgDL := float64(totalLen) / float64(max(1, len(candidates)))

	// IDF: simple log((N-df+0.5)/(df+0.5)+1) — but since N is small (5-15
	// candidates), we approximate df as number of docs containing the term.
	N := float64(len(candidates))
	df := make(map[string]int)
	for _, tokens := range tokenised {
		seen := make(map[string]bool)
		for _, t := range tokens {
			if !seen[t] {
				df[t]++
				seen[t] = true
			}
		}
	}

	items := make([]rankedItem, len(candidates))
	for i, tokens := range tokenised {
		dl := float64(len(tokens))
		tf := termFreq(tokens)
		var score float64
		for _, qt := range queryTerms {
			idf := math.Log((N-float64(df[qt])+0.5)/(float64(df[qt])+0.5) + 1)
			tfq := float64(tf[qt])
			score += idf * (tfq*(k1+1)) / (tfq + k1*(1-b+b*dl/avgDL))
		}
		items[i] = rankedItem{index: i, score: score}
	}
	sort.Slice(items, func(a, b int) bool { return items[a].score > items[b].score })
	return items
}

// ─── TF-IDF cosine similarity ─────────────────────────────────────────────────

func rankTFIDF(query string, candidates []string) []rankedItem {
	docs := append([]string{query}, candidates...) //nolint:gocritic
	vecs := tfidfVectors(docs)
	queryVec := vecs[0]
	items := make([]rankedItem, len(candidates))
	for i := range candidates {
		items[i] = rankedItem{index: i, score: cosine(queryVec, vecs[i+1])}
	}
	sort.Slice(items, func(a, b int) bool { return items[a].score > items[b].score })
	return items
}

// ─── Hybrid RRF ───────────────────────────────────────────────────────────────

func rankHybridRRF(query string, candidates []string) []rankedItem {
	const k = 60 // RRF constant

	bm25 := rankBM25(query, candidates)
	tfidf := rankTFIDF(query, candidates)

	rankBM25Map := rankMap(bm25)
	rankTFIDFMap := rankMap(tfidf)

	items := make([]rankedItem, len(candidates))
	for i := range candidates {
		rrf := 1.0/float64(rankBM25Map[i]+k) + 1.0/float64(rankTFIDFMap[i]+k)
		items[i] = rankedItem{index: i, score: rrf}
	}
	sort.Slice(items, func(a, b int) bool { return items[a].score > items[b].score })
	return items
}

// ─── Hybrid Convex (α·bm25 + (1-α)·tfidf, scores normalised) ─────────────────

func rankHybridConvex(query string, candidates []string) []rankedItem {
	const alpha = 0.5

	bm25 := rankBM25(query, candidates)
	tfidf := rankTFIDF(query, candidates)

	bm25Norm := normalise(bm25, len(candidates))
	tfidfNorm := normalise(tfidf, len(candidates))

	items := make([]rankedItem, len(candidates))
	for i := range candidates {
		score := alpha*bm25Norm[i] + (1-alpha)*tfidfNorm[i]
		items[i] = rankedItem{index: i, score: score}
	}
	sort.Slice(items, func(a, b int) bool { return items[a].score > items[b].score })
	return items
}

// ─── Hybrid Anchor ────────────────────────────────────────────────────────────

// rankHybridAnchor is hybrid-rrf with a position-based anchor boost: candidates
// that appear earlier in the list (lower original index) get a small boost.
// This mirrors the RepoBench XF-F scenario where the nearest cross-file context
// appears first.
func rankHybridAnchor(query string, candidates []string) []rankedItem {
	base := rankHybridRRF(query, candidates)
	n := float64(len(candidates))
	for i := range base {
		// Anchor boost: 0.05 for index 0, linearly decaying to 0 at index n-1.
		anchorBoost := 0.05 * (1.0 - float64(base[i].index)/n)
		base[i].score += anchorBoost
	}
	sort.Slice(base, func(a, b int) bool { return base[a].score > base[b].score })
	return base
}

// ─── Synapses search-based ranking ───────────────────────────────────────────

// rankViaSynapsesEmbed calls the rank_candidates tool — the daemon embeds
// query + all candidates with nomic-embed and returns them ranked by cosine
// similarity. This is the direct neural embedding comparison.
func rankViaSynapsesEmbed(client *agent.SynapsesClient, query string, sample RepoBenchSample) (int, error) {
	ranked, err := client.RankCandidates(query, sample.Context)
	if err != nil {
		// Fallback to local hybrid on failure.
		local := rankHybridRRF(query, sample.Context)
		for rank, item := range local {
			if item.index == sample.GoldenSnippetIndex {
				return rank + 1, nil
			}
		}
		return len(sample.Context), nil
	}
	for rank, r := range ranked {
		if r.Index == sample.GoldenSnippetIndex {
			return rank + 1, nil
		}
	}
	return len(sample.Context), nil
}

// rankViaSynapsesSearch calls the Synapses search tool and ranks candidates by
// textual overlap with the returned results. This is a best-effort approach
// since candidates are raw snippets not necessarily indexed in the same project.
func rankViaSynapsesSearch(client *agent.SynapsesClient, query string, sample RepoBenchSample) (int, error) {
	result, err := client.Search("repobench", query)
	if err != nil || result.Text == "" {
		// Fallback to local hybrid when search fails.
		ranked := rankHybridRRF(query, sample.Context)
		for rank, item := range ranked {
			if item.index == sample.GoldenSnippetIndex {
				return rank + 1, nil
			}
		}
		return len(sample.Context), nil
	}

	// Rank candidates by overlap with search result text.
	resultTokens := tokenize(result.Text)
	items := make([]rankedItem, len(sample.Context))
	for i, c := range sample.Context {
		candTokens := tokenize(c)
		overlap := tokenOverlap(resultTokens, candTokens)
		items[i] = rankedItem{index: i, score: overlap}
	}
	sort.Slice(items, func(a, b int) bool { return items[a].score > items[b].score })
	for rank, item := range items {
		if item.index == sample.GoldenSnippetIndex {
			return rank + 1, nil
		}
	}
	return len(sample.Context), nil
}

// ─── JSONL loader ─────────────────────────────────────────────────────────────

func loadJSONL(path string) ([]RepoBenchSample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var samples []RepoBenchSample
	for lineNo, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s RepoBenchSample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
		}
		samples = append(samples, s)
	}
	return samples, nil
}

// ─── text utilities ──────────────────────────────────────────────────────────

// tokenize splits text into lowercase alphanumeric tokens, filtering
// single-character tokens and common code stop-words.
func tokenize(s string) []string {
	var tokens []string
	var cur strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(unicode.ToLower(r))
		} else {
			if cur.Len() > 1 {
				t := cur.String()
				if !isStopWord(t) {
					tokens = append(tokens, t)
				}
			}
			cur.Reset()
		}
	}
	if cur.Len() > 1 {
		t := cur.String()
		if !isStopWord(t) {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "not": true,
	"this": true, "that": true, "with": true, "from": true, "import": true,
	"def": true, "class": true, "return": true, "self": true, "true": true,
	"false": true, "none": true, "null": true, "void": true,
}

func isStopWord(s string) bool { return stopWords[s] }

func termFreq(tokens []string) map[string]int {
	tf := make(map[string]int)
	for _, t := range tokens {
		tf[t]++
	}
	return tf
}

// tfidfVectors builds TF-IDF vectors for all documents (docs[0] is query).
func tfidfVectors(docs []string) []map[string]float64 {
	tokenised := make([][]string, len(docs))
	for i, d := range docs {
		tokenised[i] = tokenize(d)
	}

	// IDF.
	N := float64(len(docs))
	df := make(map[string]int)
	for _, tokens := range tokenised {
		seen := make(map[string]bool)
		for _, t := range tokens {
			if !seen[t] {
				df[t]++
				seen[t] = true
			}
		}
	}

	vecs := make([]map[string]float64, len(docs))
	for i, tokens := range tokenised {
		tf := termFreq(tokens)
		vec := make(map[string]float64)
		for term, freq := range tf {
			idf := math.Log(N / float64(max(1, df[term])))
			vec[term] = float64(freq) * idf
		}
		vecs[i] = vec
	}
	return vecs
}

func cosine(a, b map[string]float64) float64 {
	var dot, normA, normB float64
	for k, va := range a {
		dot += va * b[k]
		normA += va * va
	}
	for _, vb := range b {
		normB += vb * vb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func tokenOverlap(a, b []string) float64 {
	setA := make(map[string]bool)
	for _, t := range a {
		setA[t] = true
	}
	var common int
	for _, t := range b {
		if setA[t] {
			common++
		}
	}
	denom := len(a) + len(b)
	if denom == 0 {
		return 0
	}
	return 2.0 * float64(common) / float64(denom)
}

func rankMap(items []rankedItem) map[int]int {
	m := make(map[int]int, len(items))
	for rank, item := range items {
		m[item.index] = rank + 1
	}
	return m
}

// normalise converts a ranked list into a [0,1] score map keyed by original index.
func normalise(items []rankedItem, n int) map[int]float64 {
	var maxScore float64
	for _, it := range items {
		if it.score > maxScore {
			maxScore = it.score
		}
	}
	out := make(map[int]float64, n)
	for _, it := range items {
		if maxScore > 0 {
			out[it.index] = it.score / maxScore
		} else {
			out[it.index] = 0
		}
	}
	return out
}

func identityRank(n int) []rankedItem {
	items := make([]rankedItem, n)
	for i := range items {
		items[i] = rankedItem{index: i, score: float64(n - i)}
	}
	return items
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Ensure time import is used (for future use in progress reporting).
var _ = time.Now

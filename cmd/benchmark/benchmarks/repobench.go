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
//   - fts-only          — local BM25 over tokenised tokens
//   - vector-only       — local TF-IDF cosine similarity
//   - hybrid-rrf        — RRF merge of BM25 + TF-IDF ranks (default)
//   - hybrid-convex     — convex combination of BM25 + TF-IDF scores
//   - hybrid-anchor     — hybrid-rrf + anchor boost (first candidate gets a small boost)
//   - next-hint         — hybrid-convex with next-line identifiers injected into query (V2-A5)
//   - bm25-lenorm       — hybrid-convex with adaptive BM25 length normalisation b-param (V2-A6)
//   - hybrid-ngram      — hybrid-convex + word-bigram overlap signal (V2-A9)
//   - cluster-hybrid    — TF-IDF k-means cluster pre-filter + hybrid-convex on top cluster (V2-A10)
//   - synapses-search   — call Synapses search tool and rerank candidates by overlap
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
	// RetrievalMode: fts-only | vector-only | hybrid-rrf | hybrid-convex | hybrid-anchor |
	//   next-hint | bm25-lenorm | hybrid-ngram | cluster-hybrid |
	//   synapses-search | synapses-embed | synapses-embed-local |
	//   rerank-bm25 | rerank-tfidf | rerank-hybrid | rerank-convex |
	//   embed-codebert | embed-jina-v2-code | embed-jina-v3
	RetrievalMode string
	// LimitPerSet caps samples per config×difficulty (0 = all).
	LimitPerSet int
	// ReposDir is the root directory where repos are cloned (for synapses-embed).
	ReposDir string
	// RepoCache maps repo names to local paths (for per-repo project routing).
	RepoCache *indexer.Cache
	// LocalEmbedder is used for synapses-embed-local mode (in-process ONNX).
	LocalEmbedder *LocalEmbedder
	// Reranker is used for rerank-* modes (in-process ONNX cross-encoder).
	Reranker *CrossEncoderReranker
	// CodeEmbedder is used for embed-codebert / embed-jina-v2-code / embed-jina-v3 modes.
	CodeEmbedder *CodeModelEmbedder
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
	switch {
	case opts.RetrievalMode == "synapses-embed" || opts.RetrievalMode == "synapses-search":
		workers = 4
	case opts.RetrievalMode == "synapses-embed-local":
		workers = 1
	case IsRerankMode(opts.RetrievalMode):
		// Cross-encoder is mutex-serialized; more workers just queue.
		// 2 workers: one runs cross-encoder while the other prepares first-stage.
		workers = 2
	case IsCodeEmbedMode(opts.RetrievalMode):
		// Code embedder is mutex-serialized (single ONNX session).
		// 2 workers: one embeds while the other prepares candidate text.
		workers = 2
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
	// V2-A5: next-line hint injection — use identifiers from the completion target.
	case "next-hint":
		hintQuery := buildQueryWithNextHint(sample)
		ranked = rankHybridConvex(hintQuery, candidates)
	// V2-A6: adaptive BM25 length normalisation — higher b for large candidate pools.
	case "bm25-lenorm":
		ranked = rankBM25LenormConvex(query, candidates)
	// V2-A9: word-bigram features — hybrid-convex + bigram overlap.
	case "hybrid-ngram":
		ranked = rankHybridNgram(query, candidates)
	// V2-A10: candidate clustering pre-filter — k-means on TF-IDF, rank within top cluster.
	case "cluster-hybrid":
		ranked = rankClusterHybrid(query, candidates)
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
	case "rerank-bm25", "rerank-tfidf", "rerank-hybrid", "rerank-convex":
		ranked = rankRerankSample(opts.Reranker, mode, query, candidates)
	default:
		// Handle all code-embed modes generically so adding a new model to
		// CodeModelSpecs automatically routes here without touching this switch.
		if IsCodeEmbedMode(mode) {
			if opts.CodeEmbedder != nil {
				return rankViaCodeEmbed(opts.CodeEmbedder, query, sample)
			}
			// Fallback if embedder not initialized (e.g. model download failed).
			ranked = rankHybridRRF(query, candidates)
		} else {
			ranked = rankHybridRRF(query, candidates)
		}
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
	// Query = imports + last 500 runes of the current file's code.
	// Rune-based truncation avoids slicing mid-UTF-8 sequence, which matters
	// for neural tokenizers that validate UTF-8 strictly.
	code := sample.Code
	if runes := []rune(code); len(runes) > 500 {
		code = string(runes[len(runes)-500:])
	}
	q := sample.ImportStatement + "\n" + code
	return strings.TrimSpace(q)
}

// buildQueryWithNextHint extends the base query with identifiers extracted from
// the next_line field (V2-A5). next_line is the actual completion target — its
// identifiers reveal what the developer is about to write, which is highly
// discriminative for finding the correct cross-file snippet.
func buildQueryWithNextHint(sample RepoBenchSample) string {
	base := buildQuery(sample)
	if sample.NextLine == "" {
		return base
	}
	hints := extractNextLineIdentifiers(sample.NextLine)
	if len(hints) == 0 {
		return base
	}
	// Append identifiers twice to amplify their term-frequency signal in BM25.
	hint := strings.Join(hints, " ")
	return base + "\n" + hint + " " + hint
}

// extractNextLineIdentifiers tokenizes the next_line value but keeps only
// meaningful identifiers — skipping stop-words and very short tokens.
func extractNextLineIdentifiers(nextLine string) []string {
	tokens := tokenize(nextLine)
	var out []string
	for _, t := range tokens {
		// Keep identifiers of length ≥ 3 that look like real names (not just digits).
		hasLetter := false
		for _, r := range t {
			if unicode.IsLetter(r) {
				hasLetter = true
				break
			}
		}
		if hasLetter && len(t) >= 3 {
			out = append(out, t)
		}
	}
	return out
}

// ─── rankedItem ───────────────────────────────────────────────────────────────

type rankedItem struct {
	index int
	score float64
}

// ─── BM25 (local, approximate) ───────────────────────────────────────────────

// rankBM25 ranks candidates using BM25 with k1=1.5, b=0.75.
func rankBM25(query string, candidates []string) []rankedItem {
	return rankBM25WithB(query, candidates, 0.75)
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

// rankHybridConvex ranks candidates using a convex combination of BM25 and
// TF-IDF scores. V2-E5: alpha is adaptive — easy tasks (≤10 candidates) favour
// TF-IDF (α=0.4), hard tasks (>10 candidates) favour BM25 (α=0.7) because
// exact term matching dominates when the candidate pool is large.
func rankHybridConvex(query string, candidates []string) []rankedItem {
	alpha := 0.4 // easy: more TF-IDF weight
	if len(candidates) > 10 {
		alpha = 0.7 // hard: more BM25 weight
	}

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

// ─── V2-A6: BM25 with adaptive length normalisation ──────────────────────────

// rankBM25WithB is the parameterised BM25 scorer. Callers pass the desired
// b (length-normalisation) value.
func rankBM25WithB(query string, candidates []string, b float64) []rankedItem {
	const k1 = 1.5

	queryTerms := tokenize(query)
	if len(queryTerms) == 0 {
		return identityRank(len(candidates))
	}

	totalLen := 0
	tokenised := make([][]string, len(candidates))
	for i, c := range candidates {
		tokens := tokenize(c)
		tokenised[i] = tokens
		totalLen += len(tokens)
	}
	// Guard: if all candidates produce empty token lists (e.g. heavily expanded
	// stopwords), avgDL = 0 causes NaN via b*dl/avgDL. Floor at 1.0.
	avgDL := math.Max(1.0, float64(totalLen)/float64(max(1, len(candidates))))

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
			score += idf * (tfq * (k1 + 1)) / (tfq + k1*(1-b+b*dl/avgDL))
		}
		items[i] = rankedItem{index: i, score: score}
	}
	sort.Slice(items, func(a, b int) bool { return items[a].score > items[b].score })
	return items
}

// rankBM25Adaptive computes the length-normalisation parameter b dynamically:
// b=0.75 for ≤7 candidates (easy), linearly increasing to b=0.95 for ≥15
// candidates (hard). More candidates → more variable document lengths →
// stronger length normalisation improves discrimination.
func rankBM25Adaptive(query string, candidates []string) []rankedItem {
	n := float64(len(candidates))
	b := 0.75
	if n > 7 {
		// Interpolate: 0.75 at n=7, 0.95 at n≥15 (range of 8).
		b = 0.75 + 0.20*math.Min((n-7)/8.0, 1.0)
	}
	return rankBM25WithB(query, candidates, b)
}

// rankBM25LenormConvex is hybrid-convex with adaptive BM25 (V2-A6) + V2-E5 alpha.
func rankBM25LenormConvex(query string, candidates []string) []rankedItem {
	alpha := 0.4
	if len(candidates) > 10 {
		alpha = 0.7
	}
	bm25 := rankBM25Adaptive(query, candidates)
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

// ─── V2-A9: Word-bigram features ─────────────────────────────────────────────

// extractBigrams returns consecutive token pairs as "a_b" strings.
func extractBigrams(tokens []string) []string {
	if len(tokens) < 2 {
		return nil
	}
	bigrams := make([]string, len(tokens)-1)
	for i := range bigrams {
		bigrams[i] = tokens[i] + "_" + tokens[i+1]
	}
	return bigrams
}

// bigramOverlapScore computes set-based Dice-coefficient overlap of word
// bigrams. Both sides are deduplicated before computing the intersection —
// this prevents over-counting repeated patterns (e.g. "return_None" appearing
// 10 times in a loop body would otherwise inflate the score 10×).
//
// Set Dice = 2 * |setA ∩ setB| / (|setA| + |setB|)
func bigramOverlapScore(queryTokens, candTokens []string) float64 {
	qBigrams := extractBigrams(queryTokens)
	cBigrams := extractBigrams(candTokens)
	if len(qBigrams) == 0 || len(cBigrams) == 0 {
		return 0
	}
	setQ := make(map[string]struct{}, len(qBigrams))
	for _, b := range qBigrams {
		setQ[b] = struct{}{}
	}
	setC := make(map[string]struct{}, len(cBigrams))
	for _, b := range cBigrams {
		setC[b] = struct{}{}
	}
	var common int
	for b := range setQ {
		if _, ok := setC[b]; ok {
			common++
		}
	}
	denom := len(setQ) + len(setC)
	if denom == 0 {
		return 0
	}
	return 2.0 * float64(common) / float64(denom)
}

// rankHybridNgram combines hybrid-convex with word-bigram overlap (V2-A9).
// Weight: 0.7 hybrid-convex + 0.3 bigram. Bigrams capture sequential code
// patterns (e.g. "self_foo", "return_bar") that unigrams miss.
func rankHybridNgram(query string, candidates []string) []rankedItem {
	hybridItems := rankHybridConvex(query, candidates)
	hybridNorm := normalise(hybridItems, len(candidates))

	queryTokens := tokenize(query)
	items := make([]rankedItem, len(candidates))
	for i, c := range candidates {
		candTokens := tokenize(c)
		bg := bigramOverlapScore(queryTokens, candTokens)
		items[i] = rankedItem{index: i, score: 0.7*hybridNorm[i] + 0.3*bg}
	}
	sort.Slice(items, func(a, b int) bool { return items[a].score > items[b].score })
	return items
}

// ─── V2-A10: Candidate clustering pre-filter ─────────────────────────────────

// rankClusterHybrid uses TF-IDF k-means to cluster candidates, finds the
// cluster closest to the query, ranks within-cluster candidates first by
// hybrid-convex, then appends out-of-cluster candidates ranked by cosine (V2-A10).
//
// For easy samples (≤12 candidates) there is no benefit — fall through to
// plain hybrid-convex.
func rankClusterHybrid(query string, candidates []string) []rankedItem {
	if len(candidates) <= 12 {
		return rankHybridConvex(query, candidates)
	}

	// Build TF-IDF vectors for query (index 0) + all candidates.
	allDocs := make([]string, 1+len(candidates))
	allDocs[0] = query
	copy(allDocs[1:], candidates)
	vecs := tfidfVectors(allDocs)
	queryVec := vecs[0]
	candVecs := vecs[1:]

	// Choose k based on candidate count.
	k := 3
	if len(candidates) > 15 {
		k = 4
	}
	if len(candidates) > 20 {
		k = 5
	}

	// K-means clustering on candidate TF-IDF vectors (10 iterations).
	assignments := kMeansCandidates(candVecs, k, 10)
	centroids := computeCentroids(candVecs, assignments, k)
	queryCentroid := closestCentroid(queryVec, centroids)

	// Partition candidates into in-cluster and out-of-cluster.
	inCluster := make([]int, 0, len(candidates))
	outCluster := make([]int, 0, len(candidates))
	for i, c := range assignments {
		if c == queryCentroid {
			inCluster = append(inCluster, i)
		} else {
			outCluster = append(outCluster, i)
		}
	}

	// Guard: fall back to plain hybrid-convex if clustering is degenerate.
	// Case 1: all candidates in one cluster — no filtering benefit.
	// Case 2: query's cluster is empty — can happen when a centroid drifts to
	//         zero assignments after k-means convergence and the query has no
	//         term overlap with any non-empty centroid.
	if len(inCluster) == len(candidates) || len(inCluster) == 0 {
		return rankHybridConvex(query, candidates)
	}

	// Rank in-cluster candidates with hybrid-convex.
	inTexts := make([]string, len(inCluster))
	for j, idx := range inCluster {
		inTexts[j] = candidates[idx]
	}
	inRanked := rankHybridConvex(query, inTexts)

	// Rank out-of-cluster candidates by cosine similarity (TF-IDF).
	outItems := make([]rankedItem, len(outCluster))
	for j, idx := range outCluster {
		outItems[j] = rankedItem{index: idx, score: cosine(queryVec, candVecs[idx])}
	}
	sort.Slice(outItems, func(a, b int) bool { return outItems[a].score > outItems[b].score })

	// Merge: in-cluster scores offset above out-of-cluster scores.
	result := make([]rankedItem, 0, len(candidates))
	for _, r := range inRanked {
		result = append(result, rankedItem{index: inCluster[r.index], score: r.score + 2.0})
	}
	result = append(result, outItems...)
	return result
}

// kMeansCandidates runs k-means on sparse TF-IDF vectors for up to maxIter
// iterations. Returns per-candidate cluster assignments (0-indexed).
func kMeansCandidates(vecs []map[string]float64, k, maxIter int) []int {
	n := len(vecs)
	if n == 0 {
		return nil
	}
	if k > n {
		k = n
	}

	// Initialise centroids using farthest-first (greedy max-spread) selection.
	// Pick the first candidate as seed, then repeatedly pick the candidate
	// with the minimum maximum-similarity to any existing centroid.
	// This spreads seeds across the space and avoids seeding two centroids in
	// the same semantic region (the main failure mode of evenly-spaced init).
	// Time: O(n * k) — trivial for n ≤ 20, k ≤ 5.
	centroids := make([]map[string]float64, k)
	// chosen[j] = candidate index that seeds centroid j.
	chosen := make([]int, 0, k)
	chosen = append(chosen, 0) // deterministic: always start with first candidate
	centroids[0] = vecs[0]
	for ci := 1; ci < k; ci++ {
		// For each unselected candidate, compute its maximum cosine similarity
		// to the already-chosen centroids (indexed 0..ci-1). Pick the candidate
		// with the LOWEST max-sim — it is farthest from the existing seeds.
		bestIdx := -1
		bestDist := math.Inf(1)
		for i := range vecs {
			alreadyChosen := false
			for _, c := range chosen {
				if c == i {
					alreadyChosen = true
					break
				}
			}
			if alreadyChosen {
				continue
			}
			// maxSim against centroids 0..ci-1 (not candidate indices).
			maxSim := 0.0
			for j := 0; j < ci; j++ {
				if s := cosine(vecs[i], centroids[j]); s > maxSim {
					maxSim = s
				}
			}
			if maxSim < bestDist {
				bestDist = maxSim
				bestIdx = i
			}
		}
		if bestIdx == -1 {
			bestIdx = ci // fallback: shouldn't happen when k ≤ n
		}
		chosen = append(chosen, bestIdx)
		centroids[ci] = vecs[bestIdx]
	}

	assignments := make([]int, n)
	for iter := 0; iter < maxIter; iter++ {
		changed := false
		for i, v := range vecs {
			best := 0
			bestSim := cosine(v, centroids[0])
			for j := 1; j < k; j++ {
				if s := cosine(v, centroids[j]); s > bestSim {
					bestSim = s
					best = j
				}
			}
			if assignments[i] != best {
				assignments[i] = best
				changed = true
			}
		}
		if !changed {
			break
		}
		centroids = computeCentroids(vecs, assignments, k)
	}
	return assignments
}

// computeCentroids recalculates cluster centroids as mean TF-IDF vectors.
func computeCentroids(vecs []map[string]float64, assignments []int, k int) []map[string]float64 {
	centroids := make([]map[string]float64, k)
	counts := make([]int, k)
	for i := range centroids {
		centroids[i] = make(map[string]float64)
	}
	for i, v := range vecs {
		c := assignments[i]
		counts[c]++
		for term, score := range v {
			centroids[c][term] += score
		}
	}
	for c := range centroids {
		if counts[c] > 0 {
			for term := range centroids[c] {
				centroids[c][term] /= float64(counts[c])
			}
		}
	}
	return centroids
}

// closestCentroid returns the index of the centroid most similar to the query.
func closestCentroid(query map[string]float64, centroids []map[string]float64) int {
	best := 0
	bestSim := cosine(query, centroids[0])
	for i := 1; i < len(centroids); i++ {
		if s := cosine(query, centroids[i]); s > bestSim {
			bestSim = s
			best = i
		}
	}
	return best
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

// stopWords (V2-A8: expanded) — pure language KEYWORDS that appear in nearly
// every file of that language and carry zero discriminative signal between
// candidates from the same repo.
//
// Deliberately excluded: primitive type names (int, string, bool, error, etc.)
// These CAN be discriminative — e.g. a function named "string_encoder" would
// tokenize to ["string","encoder"] and removing "string" loses half its identity.
// Within a single repo candidates will share most type names, so their IDF is
// low, but we let the IDF calculation handle that rather than hard-excluding them.
var stopWords = map[string]bool{
	// Natural language
	"the": true, "and": true, "for": true, "are": true, "not": true,
	"this": true, "that": true, "with": true, "from": true,
	// Python keywords
	"import": true, "def": true, "class": true, "return": true, "self": true,
	"true": true, "false": true, "none": true, "pass": true, "elif": true,
	"lambda": true, "yield": true, "async": true, "await": true,
	// Java / Kotlin keywords (pure syntax, not type names)
	"null": true, "void": true, "new": true, "public": true, "private": true,
	"protected": true, "static": true, "final": true, "extends": true,
	"implements": true, "override": true, "super": true, "abstract": true,
	"interface": true,
	// Go keywords (pure syntax)
	"func": true, "package": true, "var": true, "const": true, "type": true,
	"range": true, "make": true, "append": true,
	// TypeScript / JavaScript keywords
	"let": true, "export": true, "default": true, "typeof": true,
	// Control flow (universal, every file has these)
	"if": true, "else": true, "break": true, "continue": true,
	// Structural keywords (not type names)
	"struct": true,
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

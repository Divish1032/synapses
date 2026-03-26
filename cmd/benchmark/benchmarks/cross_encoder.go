package benchmarks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	hugot "github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
)

const (
	// crossEncoderModel is the HuggingFace model ID for the reranker.
	// cross-encoder/ms-marco-MiniLM-L-6-v2: 22M params, BERT-base, trained on
	// MS-MARCO passage ranking. Produces a single relevance score per
	// (query, document) pair — captures fine-grained relevance that bi-encoders miss.
	crossEncoderModel   = "cross-encoder/ms-marco-MiniLM-L-6-v2"
	crossEncoderDirName = "cross-encoder_ms-marco-MiniLM-L-6-v2"
	crossEncoderOnnx    = "onnx/model.onnx"
	crossEncoderFile    = "model.onnx"

	// rerankTopN is the number of first-stage candidates to rerank.
	// Cross-encoder sees query+candidate jointly — expensive per pair.
	// Top-20 balances recall (gold snippet is in top-20 ~85% of the time
	// for hybrid-rrf) against latency.
	rerankTopN = 20

	// crossEncoderBatchSize controls how many (query, doc) pairs are processed
	// per ONNX forward pass. Larger batches are faster but use more RAM.
	crossEncoderBatchSize = 8

	// rerankTimeout is the max time for a single cross-encoder inference call.
	// 20 candidates × 512 tokens ≈ 50-150ms on M-series; 5s is a generous cap
	// that catches hangs without false-triggering on slow machines.
	rerankTimeout = 5 * time.Second
)

// CrossEncoderReranker wraps a hugot cross-encoder pipeline for in-process
// second-stage reranking. Thread-safe: a mutex serializes access to the
// single ONNX session (cross-encoder inference is fast enough that a pool
// is unnecessary — <200ms for 20 candidates).
type CrossEncoderReranker struct {
	session  *hugot.Session
	pipeline *pipelines.CrossEncoderPipeline
	mu       sync.Mutex
}

// NewCrossEncoderReranker downloads (if needed) and initializes the
// cross-encoder model from ~/.synapses/models/.
func NewCrossEncoderReranker() (*CrossEncoderReranker, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	modelsDir := filepath.Join(homeDir, ".synapses", "models")
	modelPath := filepath.Join(modelsDir, crossEncoderDirName)
	onnxDisk := filepath.Join(modelPath, crossEncoderFile)

	// Download if not cached.
	if _, err := os.Stat(onnxDisk); os.IsNotExist(err) {
		fmt.Printf("[cross-encoder] downloading %s to %s ...\n", crossEncoderModel, modelsDir)
		if err := os.MkdirAll(modelsDir, 0o755); err != nil {
			return nil, fmt.Errorf("create models dir: %w", err)
		}
		opts := hugot.NewDownloadOptions()
		opts.OnnxFilePath = crossEncoderOnnx
		opts.Verbose = false
		opts.MaxRetries = 3
		opts.RetryInterval = 2

		// Run download with a 10-minute timeout to avoid hanging indefinitely
		// on slow/broken connections.
		type dlResult struct{ err error }
		dlCh := make(chan dlResult, 1)
		go func() {
			_, dlErr := hugot.DownloadModel(crossEncoderModel, modelsDir, opts)
			dlCh <- dlResult{err: dlErr}
		}()
		select {
		case res := <-dlCh:
			if res.err != nil {
				return nil, fmt.Errorf("download cross-encoder: %w", res.err)
			}
		case <-time.After(10 * time.Minute):
			return nil, fmt.Errorf("download cross-encoder: timed out after 10 minutes")
		}
		fmt.Printf("[cross-encoder] download complete\n")
	}

	// Verify model exists after potential download.
	if _, err := os.Stat(onnxDisk); err != nil {
		return nil, fmt.Errorf("cross-encoder model not found at %s: %w", onnxDisk, err)
	}

	session, err := hugot.NewGoSession()
	if err != nil {
		return nil, fmt.Errorf("create cross-encoder session: %w", err)
	}

	config := hugot.CrossEncoderConfig{
		ModelPath:    modelPath,
		Name:         "cross-encoder-reranker",
		OnnxFilename: crossEncoderFile,
		Options: []hugot.CrossEncoderOption{
			pipelines.WithBatchSize(crossEncoderBatchSize),
			pipelines.WithSortResults(true),
		},
	}
	pipeline, err := hugot.NewPipeline(session, config)
	if err != nil {
		session.Destroy() //nolint:errcheck
		return nil, fmt.Errorf("create cross-encoder pipeline: %w", err)
	}

	return &CrossEncoderReranker{
		session:  session,
		pipeline: pipeline,
	}, nil
}

// Close releases ONNX resources.
func (r *CrossEncoderReranker) Close() {
	if r.session != nil {
		r.session.Destroy() //nolint:errcheck
	}
}

// Rerank takes first-stage ranked items and reranks the top-N using the
// cross-encoder. Items beyond top-N retain their original order appended
// after the reranked portion.
//
// Score normalization: reranked items get scores in [1.0, 2.0] (cross-encoder
// sigmoid score + 1.0 offset) and tail items get scores in [0.0, 1.0) (decaying
// from the lowest reranked score). This ensures reranked items always sort above
// tail items regardless of first-stage score scale.
func (r *CrossEncoderReranker) Rerank(ctx context.Context, query string, candidates []string, firstStage []rankedItem) []rankedItem {
	topN := rerankTopN
	if topN > len(firstStage) {
		topN = len(firstStage)
	}
	if topN == 0 {
		return firstStage
	}

	// Extract top-N candidate texts.
	docs := make([]string, topN)
	for i := 0; i < topN; i++ {
		docs[i] = candidates[firstStage[i].index]
	}

	// Run cross-encoder with timeout to prevent hangs.
	type ceResult struct {
		output *pipelines.CrossEncoderOutput
		err    error
	}
	ch := make(chan ceResult, 1)
	go func() {
		r.mu.Lock()
		out, err := r.pipeline.RunPipeline(query, docs)
		r.mu.Unlock()
		ch <- ceResult{out, err}
	}()

	// Use context deadline if set, otherwise use rerankTimeout.
	deadline := rerankTimeout
	if d, ok := ctx.Deadline(); ok {
		if remaining := time.Until(d); remaining < deadline {
			deadline = remaining
		}
	}

	var output *pipelines.CrossEncoderOutput
	select {
	case res := <-ch:
		if res.err != nil {
			fmt.Fprintf(os.Stderr, "[cross-encoder] inference error: %v (falling back to first-stage)\n", res.err)
			return firstStage
		}
		output = res.output
	case <-time.After(deadline):
		fmt.Fprintf(os.Stderr, "[cross-encoder] inference timed out after %s (falling back to first-stage)\n", deadline)
		return firstStage
	case <-ctx.Done():
		fmt.Fprintf(os.Stderr, "[cross-encoder] context cancelled (falling back to first-stage)\n")
		return firstStage
	}

	if output == nil || len(output.Results) == 0 {
		return firstStage
	}

	// Build a set of which first-stage indices were covered by cross-encoder output.
	// The cross-encoder may return fewer results if its score threshold filtered some.
	covered := make(map[int]bool, len(output.Results))

	// Reranked items: cross-encoder sigmoid score (0-1) shifted to [1.0, 2.0]
	// so they always sort above tail items.
	reranked := make([]rankedItem, 0, len(firstStage))
	for _, res := range output.Results {
		if res.Index >= 0 && res.Index < topN {
			covered[res.Index] = true
			reranked = append(reranked, rankedItem{
				index: firstStage[res.Index].index,
				score: float64(res.Score) + 1.0, // shift to [1.0, 2.0]
			})
		}
	}

	// Any top-N items not covered by cross-encoder output (filtered by threshold)
	// get appended with a score just below the reranked range.
	for i := 0; i < topN; i++ {
		if !covered[i] {
			reranked = append(reranked, rankedItem{
				index: firstStage[i].index,
				score: 0.999 - float64(i)*0.001, // just below 1.0, preserving first-stage order
			})
		}
	}

	// Tail items (beyond top-N) get decaying scores in [0, 0.5).
	tailCount := len(firstStage) - topN
	for i := topN; i < len(firstStage); i++ {
		tailPos := i - topN
		score := 0.0
		if tailCount > 1 {
			score = 0.5 * (1.0 - float64(tailPos)/float64(tailCount))
		}
		reranked = append(reranked, rankedItem{
			index: firstStage[i].index,
			score: score,
		})
	}

	return reranked
}

// rankWithRerank runs a first-stage retrieval then cross-encoder reranking.
func rankWithRerank(reranker *CrossEncoderReranker, firstStageMode string, query string, candidates []string) []rankedItem {
	var firstStage []rankedItem
	switch firstStageMode {
	case "bm25":
		firstStage = rankBM25(query, candidates)
	case "tfidf":
		firstStage = rankTFIDF(query, candidates)
	case "hybrid":
		firstStage = rankHybridRRF(query, candidates)
	case "convex":
		firstStage = rankHybridConvex(query, candidates)
	default:
		firstStage = rankHybridRRF(query, candidates)
	}

	if reranker == nil {
		return firstStage
	}

	return reranker.Rerank(context.Background(), query, candidates, firstStage)
}

// rerankModeToFirstStage maps rerank-* mode names to first-stage algorithms.
func rerankModeToFirstStage(mode string) string {
	switch mode {
	case "rerank-bm25":
		return "bm25"
	case "rerank-tfidf":
		return "tfidf"
	case "rerank-convex":
		return "convex"
	case "rerank-hybrid":
		return "hybrid"
	default:
		return "hybrid"
	}
}

// IsRerankMode returns true if the retrieval mode uses cross-encoder reranking.
func IsRerankMode(mode string) bool {
	switch mode {
	case "rerank-bm25", "rerank-tfidf", "rerank-hybrid", "rerank-convex":
		return true
	}
	return false
}

// rankRerankSample ranks candidates using first-stage retrieval + cross-encoder reranking.
// The returned list is already properly scored with normalized scores (reranked items
// in [1.0, 2.0], tail items in [0.0, 1.0)) so a final sort produces the correct order.
func rankRerankSample(reranker *CrossEncoderReranker, mode, query string, candidates []string) []rankedItem {
	firstStage := rerankModeToFirstStage(mode)
	ranked := rankWithRerank(reranker, firstStage, query, candidates)
	// Sort by normalized score — safe because Rerank() normalizes all scores
	// to a unified scale ([1,2] for reranked, [0,1) for tail).
	sortByScoreDesc(ranked)
	return ranked
}

// sortByScoreDesc sorts rankedItems by score descending (stable for equal scores).
func sortByScoreDesc(items []rankedItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].score > items[j-1].score; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

package benchmarks

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	hugot "github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
)

// ── Model registry ────────────────────────────────────────────────────────────

// CodeModelSpec describes one HuggingFace ONNX embedding model.
type CodeModelSpec struct {
	// ModelID is the HuggingFace model identifier (e.g. "jinaai/jina-embeddings-v2-base-code").
	ModelID string
	// DirName is the local directory name under ~/.synapses/models/.
	DirName string
	// OnnxRepoPath is the path within the HF repo to the ONNX file (e.g. "onnx/model_quantized.onnx").
	OnnxRepoPath string
	// OnnxFile is the local filename (e.g. "model_quantized.onnx").
	OnnxFile string
	// Dims is the native embedding dimension. 0 means use the full output dimension.
	Dims int
	// Description is a human-readable label used in benchmark reports.
	Description string
}

// CodeModelSpecs maps each benchmark retrieval mode to its model config.
// Only models with confirmed ONNX exports compatible with hugot are listed.
//
// Validation notes:
//   - jinaai/jina-embeddings-v2-base-code: Sentence Transformer, mean pooling built
//     into the ONNX graph, 768-dim. Confirmed hugot-compatible.
//   - jinaai/jina-embeddings-v3: Sentence Transformer, 1024-dim, multilingual +
//     code-optimized. Confirmed ONNX export.
//   - Xenova/codebert-base: Xenova's re-export of microsoft/codebert-base with ONNX.
//     Uses CLS-token pooling (standard BERT). 768-dim.
var CodeModelSpecs = map[string]CodeModelSpec{
	"embed-jina-v2-code": {
		ModelID:      "jinaai/jina-embeddings-v2-base-code",
		DirName:      "jinaai_jina-embeddings-v2-base-code",
		OnnxRepoPath: "onnx/model_quantized.onnx",
		OnnxFile:     "model_quantized.onnx",
		Dims:         768,
		Description:  "Jina v2 code (768-dim, mean-pooled, code-optimized)",
	},
	"embed-jina-v3": {
		ModelID:      "jinaai/jina-embeddings-v3",
		DirName:      "jinaai_jina-embeddings-v3",
		OnnxRepoPath: "onnx/model_quantized.onnx",
		OnnxFile:     "model_quantized.onnx",
		Dims:         1024,
		Description:  "Jina v3 (1024-dim, multilingual + code)",
	},
	"embed-codebert": {
		ModelID:      "Xenova/codebert-base",
		DirName:      "Xenova_codebert-base",
		OnnxRepoPath: "onnx/model_quantized.onnx",
		OnnxFile:     "model_quantized.onnx",
		Dims:         768,
		Description:  "CodeBERT base (768-dim, CLS-token, code+NL pairs)",
	},
}

// IsCodeEmbedMode returns true when mode is one of the V2-E1 code embedding modes.
func IsCodeEmbedMode(mode string) bool {
	_, ok := CodeModelSpecs[mode]
	return ok
}

// ── CodeModelEmbedder ─────────────────────────────────────────────────────────

// embedRequest is sent to the worker goroutine for serialized inference.
type embedRequest struct {
	texts  []string
	respCh chan embedResponse
}

// embedResponse carries the result back from the worker goroutine.
type embedResponse struct {
	vecs [][]float32
	err  error
}

// CodeModelEmbedder wraps a hugot FeatureExtractionPipeline for a specific code
// embedding model. A single worker goroutine serializes ONNX inference — these
// models use 300-800 MB RAM each so one instance per benchmark run is appropriate.
//
// The worker-channel design (instead of a mutex) prevents goroutine accumulation
// when inference is slow: a timed-out caller simply abandons its channel without
// spawning a new goroutine. Close() drains the in-flight request and waits for
// the worker to exit before destroying the ONNX session.
type CodeModelEmbedder struct {
	spec     CodeModelSpec
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
	reqCh    chan embedRequest // closed by Close() to stop the worker
	done     chan struct{}     // closed when the worker exits
}

const (
	codeEmbedBatchSize = 8
	codeEmbedTimeout   = 10 * time.Second
	codeEmbedDownloadTimeout = 15 * time.Minute
)

// NewCodeModelEmbedder downloads (if needed) and initializes a code embedding
// model. Returns an error if the model cannot be downloaded or the ONNX pipeline
// cannot be created — callers should handle this gracefully and skip the mode.
func NewCodeModelEmbedder(spec CodeModelSpec) (*CodeModelEmbedder, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	modelsDir := filepath.Join(homeDir, ".synapses", "models")
	modelPath := filepath.Join(modelsDir, spec.DirName)
	onnxDisk := filepath.Join(modelPath, spec.OnnxFile)

	// Download if not cached.
	if _, err := os.Stat(onnxDisk); os.IsNotExist(err) {
		fmt.Printf("[code-embed] downloading %s (%s) to %s ...\n",
			spec.ModelID, spec.Description, modelsDir)
		if err := os.MkdirAll(modelsDir, 0o755); err != nil {
			return nil, fmt.Errorf("create models dir: %w", err)
		}
		opts := hugot.NewDownloadOptions()
		opts.OnnxFilePath = spec.OnnxRepoPath
		opts.Verbose = false
		opts.MaxRetries = 3
		opts.RetryInterval = 2

		type dlResult struct{ err error }
		dlCh := make(chan dlResult, 1)
		go func() {
			_, dlErr := hugot.DownloadModel(spec.ModelID, modelsDir, opts)
			dlCh <- dlResult{err: dlErr}
		}()
		select {
		case res := <-dlCh:
			if res.err != nil {
				return nil, fmt.Errorf("download %s: %w", spec.ModelID, res.err)
			}
		case <-time.After(codeEmbedDownloadTimeout):
			return nil, fmt.Errorf("download %s: timed out after %s", spec.ModelID, codeEmbedDownloadTimeout)
		}
		fmt.Printf("[code-embed] download complete: %s\n", spec.ModelID)
	}

	// Verify ONNX exists.
	if _, err := os.Stat(onnxDisk); err != nil {
		return nil, fmt.Errorf("model file not found at %s: %w", onnxDisk, err)
	}

	session, err := hugot.NewGoSession()
	if err != nil {
		return nil, fmt.Errorf("create session for %s: %w", spec.ModelID, err)
	}

	config := hugot.FeatureExtractionConfig{
		ModelPath:    modelPath,
		Name:         "code-embed-" + spec.DirName,
		OnnxFilename: spec.OnnxFile,
		// WithNormalization: sentence-transformer models (Jina v2/v3) benefit
		// from L2-normalized output for cosine similarity. CodeBERT (CLS-token)
		// also benefits. Applied after mean/CLS pooling inside the pipeline.
		Options: []hugot.FeatureExtractionOption{
			pipelines.WithNormalization(),
		},
	}
	pipeline, err := hugot.NewPipeline(session, config)
	if err != nil {
		session.Destroy() //nolint:errcheck
		return nil, fmt.Errorf("create pipeline for %s: %w", spec.ModelID, err)
	}

	emb := &CodeModelEmbedder{
		spec:     spec,
		session:  session,
		pipeline: pipeline,
		reqCh:    make(chan embedRequest), // unbuffered: caller blocks until worker accepts
		done:     make(chan struct{}),
	}
	go emb.worker()
	return emb, nil
}

// worker processes embed requests one at a time. It exits when reqCh is closed.
func (c *CodeModelEmbedder) worker() {
	defer close(c.done)
	for req := range c.reqCh {
		out, err := c.pipeline.RunPipeline(req.texts)
		var vecs [][]float32
		if err == nil && out != nil {
			vecs = out.Embeddings
		}
		req.respCh <- embedResponse{vecs: vecs, err: err}
	}
}

// Close stops the worker goroutine and releases ONNX resources.
// It is safe to call even if inference is in flight: the worker completes the
// current request, then exits, ensuring session.Destroy() is never called while
// RunPipeline is executing.
func (c *CodeModelEmbedder) Close() {
	close(c.reqCh) // signal worker to stop after current request
	<-c.done       // wait for worker to exit
	if c.session != nil {
		c.session.Destroy() //nolint:errcheck
	}
}

// EmbedBatch embeds a batch of texts, returning one float32 vector per text.
//
// Requests are serialized through a single worker goroutine so the ONNX session
// is never accessed concurrently. If the worker does not respond within
// codeEmbedTimeout the call returns an error without leaking a goroutine — the
// abandoned respCh is buffered so the worker can always send when it finishes.
func (c *CodeModelEmbedder) EmbedBatch(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// Buffer of 1 so the worker can always send even if the caller has
	// already returned due to a timeout.
	respCh := make(chan embedResponse, 1)
	req := embedRequest{texts: texts, respCh: respCh}

	// Send request to the worker, respecting the timeout.
	select {
	case c.reqCh <- req:
	case <-time.After(codeEmbedTimeout):
		return nil, fmt.Errorf("embed batch: timed out waiting for worker after %s", codeEmbedTimeout)
	}

	// Wait for the worker to complete, with the same timeout budget.
	select {
	case res := <-respCh:
		if res.err != nil {
			return nil, fmt.Errorf("embed batch: %w", res.err)
		}
		if len(res.vecs) == 0 {
			return nil, fmt.Errorf("embed batch: empty output")
		}
		return res.vecs, nil
	case <-time.After(codeEmbedTimeout):
		// Worker is still running; result will be sent to the buffered channel
		// and discarded. No goroutine leak.
		return nil, fmt.Errorf("embed batch: inference timed out after %s", codeEmbedTimeout)
	}
}

// ── Ranking ──────────────────────────────────────────────────────────────────

// rankViaCodeEmbed ranks RepoBench-R candidates by cosine similarity using
// the provided code embedding model. Texts are embedded in batches of
// codeEmbedBatchSize to bound memory usage. Falls back to hybrid-rrf on error.
func rankViaCodeEmbed(emb *CodeModelEmbedder, query string, sample RepoBenchSample) (int, error) {
	candidates := sample.Context
	if len(candidates) == 0 {
		return 1, nil
	}

	// Embed query + all candidates in one pass, batched.
	all := make([]string, 1+len(candidates))
	all[0] = query
	copy(all[1:], candidates)

	vecs := make([][]float32, len(all))
	for start := 0; start < len(all); start += codeEmbedBatchSize {
		end := start + codeEmbedBatchSize
		if end > len(all) {
			end = len(all)
		}
		bvecs, err := emb.EmbedBatch(all[start:end])
		if err != nil {
			// Fallback to hybrid-rrf on embed failure — never return an error
			// that aborts the whole benchmark run.
			ranked := rankHybridRRF(query, candidates)
			for rank, item := range ranked {
				if item.index == sample.GoldenSnippetIndex {
					return rank + 1, nil
				}
			}
			return len(candidates), nil
		}
		if len(bvecs) != end-start {
			ranked := rankHybridRRF(query, candidates)
			for rank, item := range ranked {
				if item.index == sample.GoldenSnippetIndex {
					return rank + 1, nil
				}
			}
			return len(candidates), nil
		}
		copy(vecs[start:end], bvecs)
	}

	queryVec := vecs[0]
	type scored struct {
		index int
		score float64
	}
	scores := make([]scored, len(candidates))
	for i, candVec := range vecs[1:] {
		scores[i] = scored{
			index: i,
			score: float64(cosineF32(queryVec, candVec)),
		}
	}

	// Sort descending (insertion sort — same as used elsewhere in benchmarks).
	for i := 1; i < len(scores); i++ {
		for j := i; j > 0 && scores[j].score > scores[j-1].score; j-- {
			scores[j], scores[j-1] = scores[j-1], scores[j]
		}
	}

	for rank, s := range scores {
		if s.index == sample.GoldenSnippetIndex {
			return rank + 1, nil
		}
	}
	return len(candidates), nil
}

// cosineF32 computes cosine similarity between two float32 vectors.
// Returns 0 for zero-length or mismatched vectors.
func cosineF32(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}

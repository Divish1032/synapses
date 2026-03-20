package embed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	hugot "github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

const (
	// builtinModelName is the HuggingFace model ID for the default embedding model.
	builtinModelName = "KnightsAnalytics/all-MiniLM-L6-v2"
	// builtinModelDirName is the local directory name for the cached model.
	builtinModelDirName = "KnightsAnalytics_all-MiniLM-L6-v2"
	// builtinModelFile is the ONNX model filename within the model directory.
	builtinModelFile = "model.onnx"
	// builtinModel is the model identifier used in UpsertMemoryEmbedding.
	builtinModel = "all-MiniLM-L6-v2"
)

// BuiltinEmbedder uses the pure-Go hugot library to run all-MiniLM-L6-v2
// inference locally without any external dependencies. The ONNX model
// (~23MB) is auto-downloaded from HuggingFace on first use and cached
// in the models directory.
//
// Thread-safe: all Embed calls are serialized by a mutex because the
// underlying hugot pipeline is not goroutine-safe per-call.
//
// Init retry: if model download fails (e.g., no internet), subsequent
// Embed() calls will retry initialization — not permanently broken.
type BuiltinEmbedder struct {
	modelsDir string

	mu            sync.Mutex
	session       *hugot.Session
	pipeline      *pipelines.FeatureExtractionPipeline
	ready         bool
	initAttempted bool // true once ensureModel() has been called at least once
}

// NewBuiltinEmbedder creates a BuiltinEmbedder that stores its model in
// modelsDir (typically ~/.synapses/models). The model is lazily downloaded
// on the first Embed() call.
func NewBuiltinEmbedder(modelsDir string) *BuiltinEmbedder {
	return &BuiltinEmbedder{modelsDir: modelsDir}
}

// ensureModel downloads the model if not already cached, and initializes the
// hugot session and pipeline. Must be called under b.mu lock.
// Returns nil on success. On failure, the caller should retry next time.
func (b *BuiltinEmbedder) ensureModel() error {
	if b.ready {
		return nil
	}
	b.initAttempted = true

	modelPath := filepath.Join(b.modelsDir, builtinModelDirName)
	onnxPath := filepath.Join(modelPath, builtinModelFile)

	// Check if model already exists.
	if _, err := os.Stat(onnxPath); os.IsNotExist(err) {
		// Download from HuggingFace.
		logutil.Info("synapses: downloading embedding model %s to %s …\n", builtinModelName, b.modelsDir)
		if err := os.MkdirAll(b.modelsDir, 0o755); err != nil {
			return fmt.Errorf("create models dir: %w", err)
		}
		opts := hugot.NewDownloadOptions()
		opts.Verbose = false
		opts.MaxRetries = 3
		opts.RetryInterval = 2
		if _, err := hugot.DownloadModel(builtinModelName, b.modelsDir, opts); err != nil {
			return fmt.Errorf("download embedding model: %w", err)
		}
		logutil.Info("synapses: embedding model downloaded\n")
	}

	// Verify model file exists after potential download.
	if _, err := os.Stat(onnxPath); err != nil {
		return fmt.Errorf("embedding model not found at %s: %w", onnxPath, err)
	}

	// Initialize hugot with pure Go backend.
	session, err := hugot.NewGoSession()
	if err != nil {
		return fmt.Errorf("create Go inference session: %w", err)
	}

	config := hugot.FeatureExtractionConfig{
		ModelPath:    modelPath,
		Name:         "memory-embedder",
		OnnxFilename: builtinModelFile,
		Options: []hugot.FeatureExtractionOption{
			pipelines.WithNormalization(),
		},
	}
	pipeline, err := hugot.NewPipeline(session, config)
	if err != nil {
		session.Destroy() //nolint:errcheck
		return fmt.Errorf("create embedding pipeline: %w", err)
	}

	b.session = session
	b.pipeline = pipeline
	b.ready = true
	return nil
}

// Embed generates a 384-dimensional embedding for text using the builtin
// all-MiniLM-L6-v2 model. Thread-safe but serialized.
//
// If model initialization fails (e.g., no internet for download), subsequent
// calls will retry — the embedder is never permanently broken.
func (b *BuiltinEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// Check context before acquiring lock to fail fast on cancelled contexts.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Check context again after acquiring lock (may have waited).
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if err := b.ensureModel(); err != nil {
		return nil, err
	}

	result, err := b.pipeline.RunPipeline([]string{text})
	if err != nil {
		return nil, fmt.Errorf("builtin embed: %w", err)
	}
	if len(result.Embeddings) == 0 {
		return nil, fmt.Errorf("builtin embed: empty result")
	}
	return result.Embeddings[0], nil
}

// Model returns the builtin model identifier.
func (b *BuiltinEmbedder) Model() string {
	return builtinModel
}

// IsReady reports whether the model is downloaded and the inference pipeline
// is initialized. Thread-safe.
func (b *BuiltinEmbedder) IsReady() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ready
}

// StatusDetail returns a human-readable string describing the current
// initialization state. Thread-safe. Three possible values:
//   - "ready"                    — pipeline initialized, embeddings working
//   - "model not yet downloaded" — Embed() has never been called; no init attempted
//   - "unavailable"              — init was attempted but failed (download error,
//     pipeline error, or air-gapped environment); Embed() will retry automatically
func (b *BuiltinEmbedder) StatusDetail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ready {
		return "ready"
	}
	if b.initAttempted {
		return "unavailable"
	}
	return "model not yet downloaded"
}

// Close releases the hugot session resources.
func (b *BuiltinEmbedder) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != nil {
		err := b.session.Destroy()
		b.session = nil
		b.pipeline = nil
		b.ready = false
		return err
	}
	return nil
}


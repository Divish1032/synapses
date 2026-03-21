package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	hugot "github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

const (
	// builtinModelName is the HuggingFace model ID for the default embedding model.
	// Upgraded from KnightsAnalytics/all-MiniLM-L6-v2 (384-dim, 256-token, 2021)
	// to nomic-embed-text-v1.5 (768-dim Matryoshka → 384, 8192-token, 2024).
	// 32× longer context captures full decision rationales without truncation.
	builtinModelName = "nomic-ai/nomic-embed-text-v1.5"
	// builtinModelDirName is the local directory name for the cached model.
	builtinModelDirName = "nomic-ai_nomic-embed-text-v1.5"
	// builtinModelFile is the ONNX model filename within the model directory.
	// hugot strips the onnx/ directory prefix: onnx/model_quantized.onnx → model_quantized.onnx.
	// Quantized variant (~137 MB) used for CPU deployments; GPU users can override via config.
	builtinModelFile = "model_quantized.onnx"
	// builtinOnnxFilePath is the path within the HuggingFace repo for download.
	// hugot uses this to select which ONNX variant to download.
	builtinOnnxFilePath = "onnx/model_quantized.onnx"
	// builtinModel is the model identifier used in UpsertMemoryEmbedding.
	// Changing this triggers automatic re-embedding of all memories on next startup.
	builtinModel = "nomic-embed-text-v1.5"

	// matryoshkaDims is the output dimensionality after Matryoshka truncation.
	// nomic-embed-text-v1.5 produces 768 dims; truncating to 384 preserves
	// ranking quality (validated in Sprint 12 #1 spike: <15% drift on well-separated pairs)
	// while maintaining storage compatibility with the existing 384-dim schema.
	matryoshkaDims = 384

	// defaultPoolSize is the number of pipeline instances to create.
	// 3 balances memory (~411 MB total for nomic quantized) against concurrency for 50+ sessions.
	defaultPoolSize = 3

	// builtinModelRevision pins the HuggingFace download to a specific commit
	// hash, ensuring every user gets the exact same ONNX model bytes. Without
	// this, a repo owner pushing an update could silently change model behavior.
	// Update this hash when intentionally adopting a newer model version.
	builtinModelRevision = "e5cf08aadaa33385f5990def41f7a23405aec398"

	// builtinModelSHA256 is the expected SHA-256 hash of the downloaded ONNX
	// model file. After download, the file is verified against this hash to
	// detect tampering (compromised CDN, MITM with rogue CA, partial download).
	// Update this value whenever builtinModelRevision changes.
	// Captured from nomic-embed-text-v1.5 quantized ONNX at revision e5cf08aa.
	builtinModelSHA256 = "b4342336debaea79de872370664b0aaeb67dea4605513d00ee236ea871a81f27"
)

// pipelineSlot is one independently-usable ONNX pipeline instance.
// Each slot has its own hugot Session so slots can run concurrently.
type pipelineSlot struct {
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
}

// BuiltinEmbedder uses the pure-Go hugot library to run nomic-embed-text-v1.5
// inference locally without any external dependencies. The quantized ONNX model
// (~137MB) is auto-downloaded from HuggingFace on first use and cached
// in the models directory. Output is Matryoshka-truncated to 384 dims.
//
// Concurrent: a pool of pipeline instances allows bounded parallel
// inference. The pool size defaults to 3 — up to 3 Embed calls run
// simultaneously; additional callers block (respecting context) until
// a slot is returned.
//
// Init retry: if model download fails (e.g., no internet), subsequent
// Embed() calls will retry initialization — not permanently broken.
type BuiltinEmbedder struct {
	modelsDir string
	poolSize  int

	mu            sync.Mutex
	pool          chan *pipelineSlot // buffered channel acting as a slot pool
	ready         bool
	initAttempted bool // true once ensureModel() has been called at least once
	closed        bool
	done          chan struct{}   // closed by Close() to unblock pool waiters
	inflight      sync.WaitGroup // tracks in-flight Embed calls for graceful shutdown
}

// NewBuiltinEmbedder creates a BuiltinEmbedder that stores its model in
// modelsDir (typically ~/.synapses/models). The nomic-embed-text-v1.5 model
// is lazily downloaded on the first Embed() call. Uses a pool of 3 pipeline
// instances for concurrent inference. Output is 384-dim (Matryoshka truncated).
func NewBuiltinEmbedder(modelsDir string) *BuiltinEmbedder {
	return &BuiltinEmbedder{
		modelsDir: modelsDir,
		poolSize:  defaultPoolSize,
		done:      make(chan struct{}),
	}
}

// NewBuiltinEmbedderWithPoolSize creates a BuiltinEmbedder with a custom
// pool size. poolSize is clamped to [1, 8].
func NewBuiltinEmbedderWithPoolSize(modelsDir string, poolSize int) *BuiltinEmbedder {
	if poolSize < 1 {
		poolSize = 1
	}
	if poolSize > 8 {
		poolSize = 8
	}
	return &BuiltinEmbedder{
		modelsDir: modelsDir,
		poolSize:  poolSize,
		done:      make(chan struct{}),
	}
}

// ensureModel downloads the model if not already cached, and initializes
// the pipeline pool. Must be called under b.mu lock.
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
		opts.Branch = builtinModelRevision
		opts.OnnxFilePath = builtinOnnxFilePath // download only the quantized variant
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

	// Verify integrity of the ONNX model file.
	if err := verifyModelIntegrity(onnxPath); err != nil {
		return err
	}

	// Create poolSize pipeline instances, each with its own session.
	// Collect in a slice first so cleanup on partial failure is simple.
	slots := make([]*pipelineSlot, 0, b.poolSize)
	for i := 0; i < b.poolSize; i++ {
		session, err := hugot.NewGoSession()
		if err != nil {
			for _, s := range slots {
				s.session.Destroy() //nolint:errcheck
			}
			return fmt.Errorf("create Go inference session [%d]: %w", i, err)
		}

		config := hugot.FeatureExtractionConfig{
			ModelPath:    modelPath,
			Name:         fmt.Sprintf("memory-embedder-%d", i),
			OnnxFilename: builtinModelFile,
			// No WithNormalization() — we truncate to matryoshkaDims first,
			// then the store's UpsertMemoryEmbedding normalizes the truncated vector.
			// Normalizing the full 768 dims before truncation wastes FLOPs and
			// produces a non-unit vector after truncation anyway.
		}
		pipeline, err := hugot.NewPipeline(session, config)
		if err != nil {
			session.Destroy() //nolint:errcheck
			for _, s := range slots {
				s.session.Destroy() //nolint:errcheck
			}
			return fmt.Errorf("create embedding pipeline [%d]: %w", i, err)
		}

		slots = append(slots, &pipelineSlot{session: session, pipeline: pipeline})
	}

	// Move all slots into the buffered channel.
	pool := make(chan *pipelineSlot, b.poolSize)
	for _, s := range slots {
		pool <- s
	}

	b.pool = pool
	b.ready = true
	logutil.Info("synapses: embedding pool ready (%d pipeline instances)\n", b.poolSize)
	return nil
}

// verifyModelIntegrity computes the SHA-256 hash of the ONNX model file and
// compares it against the expected hash. If the hash doesn't match (tampered
// download, partial write, CDN compromise), the file is removed and an error
// is returned so the next Embed() call triggers a fresh download.
// When builtinModelSHA256 is empty, the hash is logged for capture but not enforced.
func verifyModelIntegrity(onnxPath string) error {
	f, err := os.Open(onnxPath)
	if err != nil {
		return fmt.Errorf("open model for integrity check: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash model file: %w", err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if builtinModelSHA256 == "" {
		// First download — log the hash so it can be captured and hardcoded.
		logutil.Info("synapses: embedding model SHA-256: %s (capture this for builtinModelSHA256)\n", got)
		return nil
	}
	if got != builtinModelSHA256 {
		// Remove the tampered/corrupt file so the next attempt re-downloads.
		os.Remove(onnxPath)
		return fmt.Errorf("embedding model integrity check failed: expected sha256:%s, got sha256:%s — removed corrupt file", builtinModelSHA256, got)
	}
	return nil
}

// Embed generates a 384-dimensional embedding for text using the builtin
// nomic-embed-text-v1.5 model (768 dims → Matryoshka truncation to 384).
// Concurrent: up to poolSize calls run in parallel;
// additional callers block until a pipeline slot is available (respecting
// context cancellation and shutdown).
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

	// Hold mu to check closed flag and register in-flight.
	// inflight.Add MUST happen under mu before Close sets closed=true,
	// so Close().Wait() never returns while an Add is pending.
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, fmt.Errorf("builtin embedder: closed")
	}
	b.inflight.Add(1)
	err := b.ensureModel()
	pool := b.pool
	b.mu.Unlock()

	defer b.inflight.Done()

	if err != nil {
		return nil, err
	}

	// Acquire a pipeline slot from the pool (bounded concurrency).
	// Respects context cancellation and embedder shutdown.
	var slot *pipelineSlot
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.done:
		return nil, fmt.Errorf("builtin embedder: closed")
	case slot = <-pool:
		// Got a slot — proceed with inference.
	}

	// Guarantee slot return even if RunPipeline panics (e.g., corrupted
	// model, ONNX runtime OOM). Without this, a panic would leak the slot
	// and its ONNX session resources permanently.
	defer func() {
		pool <- slot
	}()

	// Run inference without holding any lock — this is the concurrency win.
	result, runErr := slot.pipeline.RunPipeline([]string{text})

	if runErr != nil {
		return nil, fmt.Errorf("builtin embed: %w", runErr)
	}
	if len(result.Embeddings) == 0 {
		return nil, fmt.Errorf("builtin embed: empty result")
	}
	vec := result.Embeddings[0]
	// Matryoshka truncation: nomic-embed-text-v1.5 produces 768 dims;
	// truncate to 384 to match storage schema and reduce memory footprint.
	// Matryoshka training ensures the leading dimensions carry the most signal.
	// No task prefix — Sprint 12 #1 spike proved it hurts discrimination by 0.04.
	if len(vec) > matryoshkaDims {
		vec = vec[:matryoshkaDims]
	}
	// L2-normalize after truncation so callers always receive a unit vector.
	// This is necessary because truncation breaks the pre-normalization invariant.
	// The store's UpsertMemoryEmbedding also normalizes, making that a no-op.
	vec = l2Normalize(vec)
	return vec, nil
}

// Model returns the builtin model identifier.
func (b *BuiltinEmbedder) Model() string {
	return builtinModel
}

// IsReady reports whether the model is downloaded and the inference pipeline
// pool is initialized. Thread-safe.
func (b *BuiltinEmbedder) IsReady() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ready
}

// PoolSize returns the configured pool size for this embedder.
func (b *BuiltinEmbedder) PoolSize() int {
	return b.poolSize
}

// StatusDetail returns a human-readable string describing the current
// initialization state. Thread-safe. Four possible values:
//   - "ready"                              — pipeline pool initialized, embeddings working
//   - "model cached"                       — model on disk but pool not yet started;
//     will initialize automatically on the first recall() call (e.g. after daemon restart)
//   - "model not yet downloaded"           — Embed() has never been called and no model
//     found on disk; model will be downloaded on first recall()
//   - "unavailable"                        — init was attempted but failed (download error,
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
	// Check the filesystem: if the ONNX model file is already on disk the pool
	// just hasn't been initialized yet (lazy init — happens on first Embed call).
	// This is the common case after a daemon restart when the model was previously
	// downloaded. Reporting "not yet downloaded" in this state misleads agents.
	onnxPath := filepath.Join(b.modelsDir, builtinModelDirName, builtinModelFile)
	if _, err := os.Stat(onnxPath); err == nil {
		return "model cached"
	}
	return "model not yet downloaded"
}

// Close releases all hugot session resources in the pool. Blocks until all
// in-flight Embed calls complete and return their slots, then destroys all
// pipeline instances. Safe to call concurrently; second call is a no-op.
func (b *BuiltinEmbedder) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	close(b.done) // unblock goroutines waiting for a pool slot
	b.mu.Unlock()

	// Wait for all in-flight Embed calls to finish and return their slots.
	// Safe because: inflight.Add(1) only happens under mu while closed==false,
	// and we just set closed=true, so no new Add calls can occur.
	b.inflight.Wait()

	// All slots are now back in the pool. Drain and destroy.
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.pool == nil {
		b.ready = false
		return nil
	}

	close(b.pool)
	var firstErr error
	for slot := range b.pool {
		if err := slot.session.Destroy(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	b.pool = nil
	b.ready = false
	return firstErr
}

// l2Normalize returns a unit-length copy of vec (L2 norm = 1.0).
// Returns the original vec unchanged if it's already unit-length, zero-length,
// or zero-magnitude. This is a local utility — the store package has its own
// normalizeVec using SIMD; this one is for the embed package only.
func l2Normalize(vec []float32) []float32 {
	if len(vec) == 0 {
		return vec
	}
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSq)
	if norm == 0 || (norm > 0.999 && norm < 1.001) {
		return vec // already normalized or zero
	}
	out := make([]float32, len(vec))
	scale := float32(1.0 / norm)
	for i, v := range vec {
		out[i] = v * scale
	}
	return out
}


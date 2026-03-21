package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

	// defaultPoolSize is the number of pipeline instances to create.
	// 3 balances memory (~69 MB total) against concurrency for 50+ sessions.
	defaultPoolSize = 3

	// builtinModelRevision pins the HuggingFace download to a specific commit
	// hash, ensuring every user gets the exact same ONNX model bytes. Without
	// this, a repo owner pushing an update could silently change model behavior.
	// Update this hash when intentionally adopting a newer model version.
	builtinModelRevision = "24b4c6b40fc5571f11ed3bf6061596fa4e8290d4"

	// builtinModelSHA256 is the expected SHA-256 hash of the downloaded ONNX
	// model file. After download, the file is verified against this hash to
	// detect tampering (compromised CDN, MITM with rogue CA, partial download).
	// Update this value whenever builtinModelRevision changes.
	builtinModelSHA256 = "6fd5d72fe4589f189f8ebc006442dbb529bb7ce38f8082112682524616046452"
)

// pipelineSlot is one independently-usable ONNX pipeline instance.
// Each slot has its own hugot Session so slots can run concurrently.
type pipelineSlot struct {
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
}

// BuiltinEmbedder uses the pure-Go hugot library to run all-MiniLM-L6-v2
// inference locally without any external dependencies. The ONNX model
// (~23MB) is auto-downloaded from HuggingFace on first use and cached
// in the models directory.
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

	// P8-11: optional callback for model download lifecycle events.
	// eventType is "download_start" or "download_complete".
	OnModelEvent func(eventType string)
}

// NewBuiltinEmbedder creates a BuiltinEmbedder that stores its model in
// modelsDir (typically ~/.synapses/models). The model is lazily downloaded
// on the first Embed() call. Uses a pool of 3 pipeline instances for
// concurrent inference.
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
		// P8-11: emit download_start event. Called under b.mu — recover so a
		// panicking callback can't leave the mutex permanently locked.
		b.safeModelEvent("download_start")
		// Download from HuggingFace.
		logutil.Info("synapses: downloading embedding model %s to %s …\n", builtinModelName, b.modelsDir)
		if err := os.MkdirAll(b.modelsDir, 0o755); err != nil {
			return fmt.Errorf("create models dir: %w", err)
		}
		opts := hugot.NewDownloadOptions()
		opts.Branch = builtinModelRevision
		opts.Verbose = false
		opts.MaxRetries = 3
		opts.RetryInterval = 2
		if _, err := hugot.DownloadModel(builtinModelName, b.modelsDir, opts); err != nil {
			return fmt.Errorf("download embedding model: %w", err)
		}
		logutil.Info("synapses: embedding model downloaded\n")
		// P8-11: emit download_complete event.
		b.safeModelEvent("download_complete")
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
			Options: []hugot.FeatureExtractionOption{
				pipelines.WithNormalization(),
			},
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
	if got != builtinModelSHA256 {
		// Remove the tampered/corrupt file so the next attempt re-downloads.
		os.Remove(onnxPath)
		return fmt.Errorf("embedding model integrity check failed: expected sha256:%s, got sha256:%s — removed corrupt file", builtinModelSHA256, got)
	}
	return nil
}

// safeModelEvent invokes OnModelEvent inside a recover guard. ensureModel()
// runs under b.mu, so a panicking callback would leave the mutex permanently
// locked, deadlocking the entire embedder. This ensures that never happens.
func (b *BuiltinEmbedder) safeModelEvent(eventType string) {
	if b.OnModelEvent == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logutil.Error("synapses: OnModelEvent(%s) panicked: %v\n", eventType, r)
		}
	}()
	b.OnModelEvent(eventType)
}

// Embed generates a 384-dimensional embedding for text using the builtin
// all-MiniLM-L6-v2 model. Concurrent: up to poolSize calls run in parallel;
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
	return result.Embeddings[0], nil
}

// Model returns the builtin model identifier.
func (b *BuiltinEmbedder) Model() string {
	return builtinModel
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


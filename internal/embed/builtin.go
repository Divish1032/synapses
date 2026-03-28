package embed

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	hugot "github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
	"golang.org/x/sync/singleflight"

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
	// builtinModelFileQuantized is the ONNX model filename for CPU deployments.
	// hugot strips the onnx/ directory prefix: onnx/model_quantized.onnx → model_quantized.onnx.
	// Quantized variant (~137 MB) is faster and smaller on CPU.
	builtinModelFileQuantized = "model_quantized.onnx"
	// builtinOnnxFilePathQuantized is the HuggingFace repo path for the quantized variant.
	builtinOnnxFilePathQuantized = "onnx/model_quantized.onnx"
	// builtinModelFileFP32 is the ONNX model filename for GPU deployments.
	// Full-precision fp32 variant (~548 MB) offers higher quality on GPU hardware.
	builtinModelFileFP32 = "model.onnx"
	// builtinOnnxFilePathFP32 is the HuggingFace repo path for the fp32 variant.
	builtinOnnxFilePathFP32 = "onnx/model.onnx"
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

	// builtinModelSHA256FP32 is the expected SHA-256 of onnx/model.onnx at
	// builtinModelRevision (e5cf08aa). Verified from the Git LFS object ID,
	// which equals the SHA-256 of the file content (confirmed by cross-checking
	// the quantized model's LFS OID against sha256sum of the cached file).
	// Update this value whenever builtinModelRevision changes.
	// Captured from nomic-embed-text-v1.5 fp32 ONNX at revision e5cf08aa.
	builtinModelSHA256FP32 = "147d5aa88c2101237358e17796cf3a227cead1ec304ec34b465bb08e9d952965"

	// maxOnnxVerifyBytes caps how many bytes are read from disk when hashing
	// the ONNX model file for integrity verification. The quantized model is
	// ~137 MB and the FP32 model is ~548 MB; 2 GiB is far above the realistic
	// ceiling for any ONNX embedding model. This prevents an adversary who
	// replaces the model path with a symlink to a large file (e.g. /dev/zero)
	// from causing the daemon to spin in an unbounded read inside doInit.
	// A file above this size fails the SHA-256 check and triggers a re-download.
	maxOnnxVerifyBytes = 2 << 30 // 2 GiB
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
	done          chan struct{}      // closed by Close() to unblock pool waiters
	inflight      sync.WaitGroup     // tracks in-flight Embed calls for graceful shutdown
	dlWg          sync.WaitGroup     // tracks active download goroutines for graceful shutdown
	initGroup     singleflight.Group // coalesces concurrent model downloads

	// P8-11: optional callback for model download lifecycle events.
	// eventType is "download_start" or "download_complete".
	OnModelEvent func(eventType string)

	// onnxFile is the selected ONNX model filename, set during ensureModel().
	// GPU → "model.onnx" (fp32); CPU → "model_quantized.onnx".
	onnxFile string
	// onnxPath is the HuggingFace repo path for the selected variant.
	// GPU → "onnx/model.onnx"; CPU → "onnx/model_quantized.onnx".
	onnxPath string
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

// WarmUp pre-initializes the embedder by downloading the model if not already
// cached and setting up the inference pipeline pool. Call at daemon startup in
// a background goroutine so the first Embed() call doesn't block on download.
// Safe to call concurrently — uses singleflight internally.
func (b *BuiltinEmbedder) WarmUp(ctx context.Context) error {
	// WarmUp delegates to ensureModelWithSingleflight, threading ctx so that
	// cancellation (e.g. daemon shutdown) is respected during model download.
	return b.ensureModelWithSingleflight(ctx)
}

// ensureModelWithSingleflight downloads the model if not already cached, and
// initializes the pipeline pool. Uses singleflight so that only one goroutine
// downloads while others wait, without holding the main mutex during download.
// Returns nil on success. On failure, the caller should retry next time.
func (b *BuiltinEmbedder) ensureModelWithSingleflight(ctx context.Context) error {
	// Fast path under lock: already initialized.
	b.mu.Lock()
	if b.ready {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	// Use DoChan (not Do) so that each waiting caller can select on both the
	// singleflight result channel AND its own ctx.Done(). With Do(), every
	// waiter is unconditionally blocked until the download finishes regardless
	// of their individual context deadlines. DoChan() returns a channel that
	// all sharers of the same in-flight call receive, allowing each caller to
	// bail out independently without cancelling the underlying download.
	ch := b.initGroup.DoChan("init", func() (interface{}, error) {
		// Re-check under lock in case another goroutine finished first.
		b.mu.Lock()
		if b.ready {
			b.mu.Unlock()
			return nil, nil
		}
		b.initAttempted = true
		b.mu.Unlock()

		// Use a lifecycle context tied to b.done rather than the first
		// caller's ctx. This prevents a short-lived request context from
		// cancelling a multi-GB download that all waiters depend on.
		// The download continues even if all current callers cancel —
		// it will be available for the next Embed() call.
		lctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-b.done:
				cancel()
			case <-lctx.Done():
			}
		}()
		defer cancel()

		// All work below runs WITHOUT holding b.mu.
		return nil, b.doInit(lctx)
	})

	// Wait for either our own context to expire or the singleflight to finish.
	// If our ctx is cancelled we return immediately — other waiters and the
	// download goroutine are unaffected.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return fmt.Errorf("builtin embedder: closed")
	case result := <-ch:
		return result.Err
	}
}

// doInit performs the actual model download, integrity verification, and pipeline
// pool creation. Called from singleflight — only one goroutine runs this at a time.
func (b *BuiltinEmbedder) doInit(ctx context.Context) error {
	// Select ONNX variant based on hardware: fp32 for GPU, quantized for CPU.
	modelFile, repoPath := selectOnnxVariant()

	modelPath := filepath.Join(b.modelsDir, builtinModelDirName)

	// Check for stale model files from previous variant selections.
	if modelFile == builtinModelFileQuantized {
		staleFile := filepath.Join(modelPath, builtinModelFileFP32)
		if _, statErr := os.Stat(staleFile); statErr == nil {
			logutil.Info("synapses: stale fp32 model found at %s — safe to delete (not in use)\n", staleFile)
		}
	} else if modelFile == builtinModelFileFP32 {
		staleFile := filepath.Join(modelPath, builtinModelFileQuantized)
		if _, statErr := os.Stat(staleFile); statErr == nil {
			logutil.Info("synapses: stale quantized model found at %s — safe to delete (not in use)\n", staleFile)
		}
	}

	onnxDisk := filepath.Join(modelPath, modelFile)

	// Check if model already exists.
	if _, err := os.Stat(onnxDisk); os.IsNotExist(err) {
		// P8-11: emit download_start event.
		b.safeModelEvent("download_start")
		// Download from HuggingFace.
		logutil.Info("synapses: downloading embedding model %s (%s) to %s …\n", builtinModelName, modelFile, b.modelsDir)
		if err := os.MkdirAll(b.modelsDir, 0o755); err != nil {
			return fmt.Errorf("create models dir: %w", err)
		}
		opts := hugot.NewDownloadOptions()
		opts.Branch = builtinModelRevision
		opts.OnnxFilePath = repoPath // download the selected variant (fp32 or quantized)
		opts.Verbose = false
		opts.MaxRetries = 3
		opts.RetryInterval = 2
		// Check context before starting the potentially slow download.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("download cancelled before start: %w", err)
		}
		// hugot.DownloadModel does not accept a context, so we run it in a
		// goroutine and race against ctx cancellation.
		type dlResult struct{ err error }
		dlCh := make(chan dlResult, 1)
		b.dlWg.Add(1)
		go func() {
			defer b.dlWg.Done()
			_, dlErr := hugot.DownloadModel(builtinModelName, b.modelsDir, opts)
			dlCh <- dlResult{err: dlErr}
		}()
		select {
		case <-ctx.Done():
			return fmt.Errorf("download cancelled: %w", ctx.Err())
		case res := <-dlCh:
			if res.err != nil {
				// BUG-027: surface a clear message for air-gapped environments.
				logutil.Error("synapses: embedding model download failed — semantic search will be unavailable. "+
					"If this machine has no internet access, pre-download the model on a connected machine "+
					"and place it at %s\n", filepath.Join(modelPath, modelFile))
				return fmt.Errorf("download embedding model: %w", res.err)
			}
		}
		logutil.Info("synapses: embedding model downloaded (%s)\n", modelFile)
		// P8-11: emit download_complete event.
		b.safeModelEvent("download_complete")
	}

	// Verify model file exists after potential download.
	if _, err := os.Stat(onnxDisk); err != nil {
		return fmt.Errorf("embedding model not found at %s: %w", onnxDisk, err)
	}

	// Verify integrity of the ONNX model file.
	// If fp32 fails integrity (hash not yet captured), fall back to the
	// verified quantized variant rather than serving unverified embeddings.
	if err := verifyModelIntegrity(onnxDisk, modelFile); err != nil {
		if modelFile != builtinModelFileQuantized {
			logutil.Warn("synapses: %s integrity check failed (%v) — falling back to verified quantized model\n", modelFile, err)
			modelFile = builtinModelFileQuantized
			repoPath = builtinOnnxFilePathQuantized
			onnxDisk = filepath.Join(modelPath, modelFile)
			// Download quantized if not present.
			if _, statErr := os.Stat(onnxDisk); os.IsNotExist(statErr) {
				opts := hugot.NewDownloadOptions()
				opts.Branch = builtinModelRevision
				opts.OnnxFilePath = repoPath
				opts.Verbose = false
				opts.MaxRetries = 3
				opts.RetryInterval = 2
				dlCh := make(chan error, 1)
				b.dlWg.Add(1)
				go func() {
					defer b.dlWg.Done()
					_, dlErr := hugot.DownloadModel(builtinModelName, b.modelsDir, opts)
					dlCh <- dlErr
				}()
				select {
				case <-ctx.Done():
					return fmt.Errorf("download fallback cancelled: %w", ctx.Err())
				case dlErr := <-dlCh:
					if dlErr != nil {
						return fmt.Errorf("download fallback quantized model: %w", dlErr)
					}
				}
			}
			if fallbackErr := verifyModelIntegrity(onnxDisk, modelFile); fallbackErr != nil {
				return fallbackErr
			}
		} else {
			return err
		}
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
			OnnxFilename: modelFile,
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

	// Commit results under lock.
	b.mu.Lock()
	b.pool = pool
	b.onnxFile = modelFile
	b.onnxPath = repoPath
	b.ready = true
	b.mu.Unlock()

	logutil.Info("synapses: embedding pool ready (%d pipeline instances, variant=%s)\n", b.poolSize, modelFile)
	return nil
}

// verifyModelIntegrity computes the SHA-256 hash of the ONNX model file and
// compares it against the expected hash. If the hash doesn't match (tampered
// download, partial write, CDN compromise), the file is removed and an error
// is returned so the next Embed() call triggers a fresh download.
//
// modelFile identifies which variant is being verified:
//   - builtinModelFileQuantized → hash verified against builtinModelSHA256
//   - builtinModelFileFP32 → hash verified against builtinModelSHA256FP32
//
// When the expected hash is empty, the hash is written to a .sha256 sidecar
// file on first use (TOFU) and verified against it on subsequent loads.
// Both variants currently have pinned hashes — TOFU path is a safety fallback.
func verifyModelIntegrity(onnxPath string, modelFile string) error {
	f, err := os.Open(onnxPath)
	if err != nil {
		return fmt.Errorf("open model for integrity check: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	// Cap reads to maxOnnxVerifyBytes — prevents an adversarially large file
	// at the model path (e.g. /dev/zero via symlink) from spinning the daemon
	// in an unbounded read inside doInit. Any file above this size fails the
	// pinned SHA-256 check and triggers a re-download.
	if _, err := io.Copy(h, io.LimitReader(f, maxOnnxVerifyBytes)); err != nil {
		return fmt.Errorf("hash model file: %w", err)
	}

	got := hex.EncodeToString(h.Sum(nil))

	// Select the expected hash based on the model variant.
	var expected string
	switch modelFile {
	case builtinModelFileFP32:
		expected = builtinModelSHA256FP32
	default:
		expected = builtinModelSHA256
	}

	if expected == "" {
		// No hardcoded hash for this variant — use Trust-On-First-Use (TOFU).
		// On first download: compute hash, store in sidecar file, trust it.
		// On subsequent loads: verify against stored hash. Mismatch → tampered.
		// Same trust model as SSH known_hosts: first contact is trusted,
		// all subsequent loads are integrity-verified.
		sidecar := onnxPath + ".sha256"
		if stored, err := os.ReadFile(sidecar); err == nil {
			storedHash := strings.TrimSpace(string(stored))
			if subtle.ConstantTimeCompare([]byte(storedHash), []byte(got)) != 1 {
				os.Remove(onnxPath)
				return fmt.Errorf("embedding model integrity check failed (TOFU): stored sha256:%s, got sha256:%s — removed corrupt file", storedHash, got)
			}
			return nil // TOFU verified
		}
		// First use — store the hash for future verification.
		if err := os.WriteFile(sidecar, []byte(got+"\n"), 0o600); err != nil {
			logutil.Warn("synapses: could not write TOFU hash sidecar %s: %v\n", sidecar, err)
			// Non-fatal: proceed without stored hash. Next load will re-capture.
		}
		logutil.Info("synapses: embedding model (%s) SHA-256: %s — TOFU: hash stored for future integrity verification\n", modelFile, got)
		return nil
	}
	// Constant-time comparison — consistent with DownloadGGUF's use of
	// subtle.ConstantTimeCompare; safe if this path is ever called from a
	// context where timing leaks matter.
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		// Remove the tampered/corrupt file so the next attempt re-downloads.
		os.Remove(onnxPath)
		return fmt.Errorf("embedding model integrity check failed: expected sha256:%s, got sha256:%s — removed corrupt file", expected, got)
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

	// Hold mu briefly to check closed flag and register in-flight.
	// inflight.Add MUST happen under mu before Close sets closed=true,
	// so Close().Wait() never returns while an Add is pending.
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, fmt.Errorf("builtin embedder: closed")
	}
	b.inflight.Add(1)
	b.mu.Unlock()

	defer b.inflight.Done()

	// Initialize model outside the main mutex. Uses singleflight so only one
	// goroutine downloads while others wait — no 2-5 minute mutex hold.
	if err := b.ensureModelWithSingleflight(ctx); err != nil {
		return nil, err
	}

	b.mu.Lock()
	pool := b.pool
	b.mu.Unlock()

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
	if vec == nil {
		return nil, fmt.Errorf("builtin embed: l2 normalization failed (NaN/Inf in embedding)")
	}
	return vec, nil
}

// EmbedBatch embeds multiple texts in a single ONNX forward pass, which is
// significantly faster than calling Embed() N times. Returns one vector per
// input text in the same order. Uses one pool slot for the entire batch.
func (b *BuiltinEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, fmt.Errorf("builtin embedder: closed")
	}
	b.inflight.Add(1)
	b.mu.Unlock()
	defer b.inflight.Done()

	if err := b.ensureModelWithSingleflight(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	pool := b.pool
	b.mu.Unlock()

	var slot *pipelineSlot
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.done:
		return nil, fmt.Errorf("builtin embedder: closed")
	case slot = <-pool:
	}
	defer func() { pool <- slot }()

	result, runErr := slot.pipeline.RunPipeline(texts)
	if runErr != nil {
		return nil, fmt.Errorf("builtin embed batch: %w", runErr)
	}
	if len(result.Embeddings) != len(texts) {
		return nil, fmt.Errorf("builtin embed batch: got %d embeddings for %d texts", len(result.Embeddings), len(texts))
	}
	out := make([][]float32, len(texts))
	for i, vec := range result.Embeddings {
		if len(vec) > matryoshkaDims {
			vec = vec[:matryoshkaDims]
		}
		vec = l2Normalize(vec)
		if vec == nil {
			return nil, fmt.Errorf("builtin embed batch: l2 normalization failed for text[%d]", i)
		}
		out[i] = vec
	}
	return out, nil
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
	// Check both variants — either one being present means the model is cached.
	for _, mf := range []string{builtinModelFileQuantized, builtinModelFileFP32} {
		onnxOnDisk := filepath.Join(b.modelsDir, builtinModelDirName, mf)
		if _, err := os.Stat(onnxOnDisk); err == nil {
			return "model cached"
		}
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

	// Wait for any in-progress model downloads to complete or be cancelled.
	b.dlWg.Wait()

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

// detectAccelerator probes the host for GPU hardware and returns a string
// indicating the best available accelerator: "cuda", "rocm", "metal", or "cpu".
// This is a lightweight check (exec.LookPath + stat, no GPU driver calls) so
// it is safe to call on every ensureModel() invocation.
func detectAccelerator() string {
	// Apple Silicon Metal: arm64 macOS always has Metal support.
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return "metal"
	}

	// NVIDIA CUDA: check for nvidia-smi binary.
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		return "cuda"
	}
	// Fallback: check /proc/driver/nvidia (present even if nvidia-smi is not in PATH).
	if _, err := os.Stat("/proc/driver/nvidia"); err == nil {
		return "cuda"
	}

	// AMD ROCm: check for rocm-smi binary.
	if _, err := exec.LookPath("rocm-smi"); err == nil {
		return "rocm"
	}

	return "cpu"
}

// selectOnnxVariant returns the (modelFile, onnxRepoPath) tuple for the ONNX
// variant best suited to the detected hardware. GPU (cuda/rocm/metal) gets the
// full-precision fp32 model for higher quality; CPU gets the quantized model
// for lower memory and faster inference.
func selectOnnxVariant() (modelFile, onnxRepoPath string) {
	// Both FP32 and quantized variants now have pinned SHA-256 hashes and can
	// be served with full cryptographic verification.
	// SYNAPSES_EMBED_FP32=1 forces fp32 model selection regardless of GPU
	// detection — useful for testing or benchmarking on CPU hardware.
	if os.Getenv("SYNAPSES_EMBED_FP32") == "1" {
		logutil.Info("synapses: SYNAPSES_EMBED_FP32=1 — selecting fp32 ONNX model\n")
		return builtinModelFileFP32, builtinOnnxFilePathFP32
	}
	// SYNAPSES_EMBED_QUANTIZED=1 forces quantized model even on GPU hardware —
	// useful for bulk inference benchmarks where CPU INT8 throughput > GPU fp32.
	if os.Getenv("SYNAPSES_EMBED_QUANTIZED") == "1" {
		logutil.Info("synapses: SYNAPSES_EMBED_QUANTIZED=1 — selecting quantized ONNX model\n")
		return builtinModelFileQuantized, builtinOnnxFilePathQuantized
	}

	accel := detectAccelerator()
	switch accel {
	case "cuda", "rocm", "metal":
		logutil.Info("synapses: GPU detected (%s) — selecting fp32 ONNX model\n", accel)
		return builtinModelFileFP32, builtinOnnxFilePathFP32
	default:
		logutil.Info("synapses: no GPU detected — selecting quantized ONNX model\n")
		return builtinModelFileQuantized, builtinOnnxFilePathQuantized
	}
}

// l2Normalize returns a unit-length copy of vec (L2 norm = 1.0).
// Returns the original vec unchanged if it's already unit-length, zero-length,
// or zero-magnitude. This is a local utility — the store package has its own
// normalizeVec using SIMD; this one is for the embed package only.
func l2Normalize(vec []float32) []float32 {
	if len(vec) == 0 {
		return vec
	}
	// Guard: reject input containing NaN or Inf values.
	for _, v := range vec {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil
		}
	}
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSq)
	if math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil
	}
	if norm == 0 {
		return nil // zero-magnitude vector — cannot normalize
	}
	if norm > 0.999 && norm < 1.001 {
		return vec // already normalized
	}
	out := make([]float32, len(vec))
	scale := float32(1.0 / norm)
	for i, v := range vec {
		out[i] = v * scale
	}
	// Guard: reject output if normalization produced NaN/Inf in any element.
	for _, v := range out {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil
		}
	}
	return out
}

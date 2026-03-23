// Package embed provides a zero-Ollama embedding server for synapses-intelligence.
//
// It spawns a llama-server subprocess (from the llama.cpp project) in
// embedding-only mode, then exposes a simple Embed(text) → []float32 API by
// calling the subprocess's /embedding HTTP endpoint.
//
// No CGo. No Ollama. One pre-built binary (~8 MB) + one GGUF model file
// (~274 MB for nomic-embed-text-v1.5.Q4_K_M) are all that is needed.
//
// Hardware acceleration is automatic:
//   - macOS Apple Silicon → Metal (unified memory, very fast)
//   - Linux NVIDIA        → CUDA (if the binary was built with CuBLAS)
//   - CPU fallback        → AVX2/AVX-512 SIMD
//
// Lifecycle: call Start(ctx) once; the server stays up for the lifetime of
// the context. Stop() terminates the subprocess cleanly.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// normalizeL2Vec returns a unit-length copy of v. Returns nil if any element
// is NaN or Inf, ensuring degenerate vectors never reach the vector store.
func normalizeL2Vec(v []float32) []float32 {
	if len(v) == 0 {
		return v
	}
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil
		}
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil
	}
	if norm == 0 {
		return nil // zero-magnitude vector: model failure — must not enter index
	}
	if norm > 0.999 && norm < 1.001 {
		return v // already normalized
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	for _, v := range out {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil
		}
	}
	return out
}

// Server manages a llama-server subprocess running in embedding-only mode.
type Server struct {
	mu        sync.Mutex
	modelPath string
	port      int    // subprocess bind port — passed to llama-server --port
	baseURL   string // base URL for HTTP calls, set once at construction, never mutated
	llamaBin  string

	proc       *exec.Cmd
	client     *http.Client
	started    bool
	restarting bool
}

// New creates an embedding Server.
//   - modelPath is the absolute path to the GGUF embedding model.
//   - port is the internal TCP port the subprocess will listen on (e.g. 11437).
//   - llamaBin is the path to the llama-server executable.
//
// Security: the HTTP base URL is permanently fixed to http://127.0.0.1:<port>
// at construction time. It cannot be changed after creation. The subprocess is
// also told to bind to 127.0.0.1 only (--host 127.0.0.1 in Start), so the
// model is never reachable from outside the local machine.
func New(modelPath string, port int, llamaBin string) *Server {
	return &Server{
		modelPath: modelPath,
		port:      port,
		baseURL:   fmt.Sprintf("http://127.0.0.1:%d", port), // locked at construction
		llamaBin:  llamaBin,
		client:    &http.Client{Timeout: 15 * time.Second},
	}
}

// newWithBaseURL creates a Server with an explicit base URL.
// This constructor is intentionally unexported — it exists solely for
// package-internal tests that inject an httptest.Server address.
// Production code must use New(), which always locks to 127.0.0.1.
func newWithBaseURL(modelPath string, port int, llamaBin string, baseURL string) *Server {
	return &Server{
		modelPath: modelPath,
		port:      port,
		baseURL:   baseURL,
		llamaBin:  llamaBin,
		client:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Start launches the llama-server subprocess and waits until it reports healthy.
// Returns an error if the binary is not found, the model is missing, or the
// server does not become ready within 60 seconds.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return nil
	}

	if _, err := os.Stat(s.llamaBin); err != nil {
		return fmt.Errorf("llama-server binary not found at %s — run: brain setup --with-embeddings", s.llamaBin)
	}
	if _, err := os.Stat(s.modelPath); err != nil {
		return fmt.Errorf("embedding model not found at %s — run: brain setup --with-embeddings", s.modelPath)
	}

	threads := runtime.NumCPU()
	if threads > 8 {
		threads = 8 // cap to avoid starving the main process
	}

	args := []string{
		"--model", s.modelPath,
		"--embedding",       // embedding-only mode
		"--pooling", "mean", // nomic-embed-text is trained with mean pooling;
		// default "cls" gives measurably worse retrieval
		"--port", fmt.Sprintf("%d", s.port),
		"--host", "127.0.0.1",
		"--ctx-size", "1024", // 1024 covers long code signatures (nomic max: 2048)
		"--batch-size", "512",
		"--threads", fmt.Sprintf("%d", threads),
		"--log-disable", // suppress llama.cpp verbose logs
	}

	// Use all GPU layers on Apple Silicon (Metal). On other platforms the
	// binary was compiled with the correct accelerator anyway.
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		args = append(args, "--n-gpu-layers", "99")
	}

	s.proc = exec.CommandContext(ctx, s.llamaBin, args...)
	s.proc.Stderr = os.Stderr // surface llama.cpp startup messages

	if err := s.proc.Start(); err != nil {
		return fmt.Errorf("start llama-server: %w", err)
	}

	// Wait for the health endpoint to respond (up to 60 s).
	if err := s.waitReady(ctx, 60*time.Second); err != nil {
		_ = s.proc.Process.Kill()
		_ = s.proc.Wait()
		return fmt.Errorf("embedding server not ready: %w", err)
	}

	s.started = true

	// Supervisor: if the subprocess exits unexpectedly, restart it with
	// exponential backoff (1s → 2s → 4s → … capped at 30s).
	go s.supervise(ctx, args)

	return nil
}

// supervise waits for the subprocess to exit and restarts it unless the
// context is done (clean shutdown) or Stop() was called.
func (s *Server) supervise(ctx context.Context, args []string) {
	backoff := time.Second
	for {
		// Capture proc under lock so we don't race with Stop() calling Wait()
		// on the same process handle.
		s.mu.Lock()
		proc := s.proc
		s.mu.Unlock()
		if proc == nil {
			return
		}
		_ = proc.Wait() // blocks until the process exits

		// Check whether the exit was intentional (Stop() sets started=false).
		// Set restarting=true under lock so Embed() callers see a clear state
		// instead of hitting connection refused during the restart window.
		s.mu.Lock()
		if !s.started {
			s.mu.Unlock()
			return
		}
		s.restarting = true
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.restarting = false
			s.mu.Unlock()
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}

		logutil.Error("synapses-intelligence/embed: llama-server exited unexpectedly; restarting (backoff %s)…\n", backoff)

		s.mu.Lock()
		newProc := exec.CommandContext(ctx, s.llamaBin, args...)
		newProc.Stderr = os.Stderr
		if err := newProc.Start(); err != nil {
			s.restarting = false
			s.mu.Unlock()
			logutil.Error("synapses-intelligence/embed: restart failed: %v\n", err)
			return // cannot restart — stop supervisor to avoid busy-loop on proc.Wait
		}
		s.proc = newProc
		s.mu.Unlock()

		if err := s.waitReady(ctx, 60*time.Second); err != nil {
			logutil.Error("synapses-intelligence/embed: restarted server not ready: %v\n", err)
		} else {
			logutil.Info("synapses-intelligence/embed: llama-server restarted successfully\n")
			backoff = time.Second // reset backoff on successful restart
		}
		s.mu.Lock()
		s.restarting = false
		s.mu.Unlock()
	}
}

// Embed returns the embedding vector for text.
// Returns an error if the server is not started or the request fails.
func (s *Server) Embed(ctx context.Context, text string) ([]float32, error) {
	s.mu.Lock()
	started := s.started
	restarting := s.restarting
	s.mu.Unlock()

	if !started {
		return nil, fmt.Errorf("embedding server not started")
	}
	if restarting {
		return nil, fmt.Errorf("embedding server is restarting")
	}

	body, _ := json.Marshal(map[string]string{"content": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.baseURL+"/embedding",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed request: HTTP %d", resp.StatusCode)
	}

	// llama-server /embedding response: {"embedding": [float, ...]}
	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if len(result.Embedding) < 2 {
		return nil, fmt.Errorf("invalid embedding dimensionality: got %d, want ≥2", len(result.Embedding))
	}
	normed := normalizeL2Vec(result.Embedding)
	if normed == nil {
		return nil, fmt.Errorf("embedding contains NaN/Inf values")
	}
	return normed, nil
}

// EmbedBatch returns embedding vectors for a batch of texts in one round-trip.
// Uses llama-server's array form: POST /embedding {"content": ["t1", "t2", ...]}.
// Response: [{"embedding": [...]}, ...] — one entry per input text.
// On response length mismatch, returns an error and the caller should fall back
// to individual Embed() calls. Returns an error if the server is not started.
func (s *Server) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()

	if !started {
		return nil, fmt.Errorf("embedding server not started")
	}
	if len(texts) == 0 {
		return nil, nil
	}

	body, _ := json.Marshal(map[string]interface{}{"content": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.baseURL+"/embedding",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed batch request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed batch request: HTTP %d", resp.StatusCode)
	}

	// llama-server /embedding batch response: [{"embedding": [...]}, ...]
	var results []struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("decode batch embedding response: %w", err)
	}
	if len(results) != len(texts) {
		return nil, fmt.Errorf("batch response length mismatch: got %d, want %d", len(results), len(texts))
	}

	// Validate dimensionality: all embeddings must have ≥2 dimensions and
	// the same length. A malformed server response with inconsistent
	// dimensions would silently create bad embeddings without this check.
	expectedDims := len(results[0].Embedding)
	if expectedDims < 2 {
		return nil, fmt.Errorf("invalid embedding dimensionality: got %d, want ≥2", expectedDims)
	}
	for i, r := range results[1:] {
		if len(r.Embedding) != expectedDims {
			return nil, fmt.Errorf("embedding[%d] dimension mismatch: got %d, want %d", i+1, len(r.Embedding), expectedDims)
		}
	}

	vecs := make([][]float32, len(results))
	for i, r := range results {
		normed := normalizeL2Vec(r.Embedding)
		if normed == nil {
			return nil, fmt.Errorf("embedding[%d] contains NaN/Inf values", i)
		}
		vecs[i] = normed
	}
	return vecs, nil
}

// Available returns true if the subprocess is running and responding.
func (s *Server) Available() bool {
	s.mu.Lock()
	started := s.started
	baseURL := s.baseURL
	s.mu.Unlock()
	if !started {
		return false
	}
	resp, err := s.client.Get(baseURL + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Stop terminates the llama-server subprocess.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc != nil && s.proc.Process != nil {
		_ = s.proc.Process.Kill()
		_ = s.proc.Wait()
	}
	s.started = false
}

// waitReady polls the /health endpoint until it responds OK or timeout.
func (s *Server) waitReady(ctx context.Context, timeout time.Duration) error {
	url := s.baseURL + "/health"
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := s.client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s", timeout)
}

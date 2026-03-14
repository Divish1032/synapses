package llm

// local.go — LocalClient: embedded llama.cpp inference via go-llama.cpp CGo bindings.
//
// This client implements the LLMClient interface using the go-skynet/go-llama.cpp
// library, which embeds llama.cpp directly in the Go binary via CGo. No external
// process (Ollama, llama-server) is required at runtime.
//
// The model is loaded once at construction time from a local .gguf file.
// Hardware acceleration is selected automatically:
//   - Apple Silicon (M-series): Metal GPU via llama.cpp's Metal backend
//   - NVIDIA:                   CUDA via CuBLAS
//   - CPU fallback:             AVX-512 / AVX2 SIMD
//
// Anti-OOM: if DetectHardware reports less than 3GB of available RAM, the client
// disables itself and Available() returns false. The intelligence pipeline then
// falls back to the SQLite-only ContextPacket path which requires no LLM at all.
//
// Build tag: this file is ALWAYS compiled. The go-llama.cpp dependency is only
// activated when you add it to go.mod and set CGO_ENABLED=1.
// Until that point, the file compiles cleanly with the stub guard below.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// minRAMGB is the minimum available RAM (GB) required to load the local model.
// Below this threshold, LocalClient reports itself as unavailable and the
// pipeline falls back to the Ollama or no-LLM path.
const minRAMGB = 3.0

// LocalClient runs a fine-tuned GGUF model embedded in-process via godeps/gollama.
// Zero network calls — everything happens in RAM.
//
// gollama is not goroutine-safe per context instance, so all Generate calls
// are serialised through mu. For high-throughput workloads consider a pool of
// LocalClient instances, one per goroutine.
type LocalClient struct {
	mu        sync.Mutex
	modelPath string
	modelName string
	hw        HardwareConfig
	think     bool // enable extended reasoning (<think> mode for Qwen3)

	// model holds the gollama model handle (*llama.Model).
	// Declared as interface{} so this file compiles without the CGo dependency.
	model interface{}

	// llamaCtx holds the gollama inference context (*llama.Context).
	// Created from model in loadModel(); used by generate().
	llamaCtx interface{}

	// available tracks whether the model loaded successfully.
	available bool

	// contextSize is the maximum token context window.
	contextSize int
}

// NewLocalClient loads a GGUF model file and returns a ready LocalClient.
// Returns an error if the model cannot be loaded or available RAM is too low.
//
// Usage:
//
//	cli, err := llm.NewLocalClient("/path/to/sil-9b-gguf/model.gguf", llm.DetectHardware())
func NewLocalClient(ggufPath string, hw HardwareConfig) (*LocalClient, error) {
	if hw.AvailableRAMGB < minRAMGB {
		return nil, fmt.Errorf(
			"local LLM: insufficient RAM (%.1f GB available, need %.1f GB)",
			hw.AvailableRAMGB, minRAMGB,
		)
	}

	c := &LocalClient{
		modelPath:   ggufPath,
		modelName:   ggufModelName(ggufPath),
		hw:          hw,
		contextSize: 2048,
	}

	if err := c.loadModel(); err != nil {
		return nil, fmt.Errorf("local LLM: load model %q: %w", ggufPath, err)
	}

	c.available = true
	return c, nil
}

// WithThinking enables or disables extended reasoning mode (Qwen3 <think> blocks).
func (c *LocalClient) WithThinking(enabled bool) *LocalClient {
	c.think = enabled
	return c
}

// ---------------------------------------------------------------------------
// LLMClient interface
// ---------------------------------------------------------------------------

// Generate runs inference on prompt and returns the decoded response text.
// Thread-safe: serialised via mu.
func (c *LocalClient) Generate(ctx context.Context, prompt string) (string, error) {
	if !c.available || c.model == nil {
		return "", errors.New("local LLM: model not loaded")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Check context cancellation before entering CGo.
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	return c.generate(ctx, prompt)
}

// Available returns true if the model is loaded and RAM is sufficient.
func (c *LocalClient) Available(_ context.Context) bool {
	return c.available && c.llamaCtx != nil
}

// ModelName returns the GGUF file name without path, used for logging.
func (c *LocalClient) ModelName() string {
	return c.modelName
}

// ModelPulled always returns true — local GGUF files are already on disk.
func (c *LocalClient) ModelPulled(_ context.Context) bool {
	return true
}

// PullModel is a no-op for local files (nothing to download).
func (c *LocalClient) PullModel(_ context.Context, _ io.Writer) error {
	return nil
}

// ---------------------------------------------------------------------------
// CGo bridge — conditionally compiled
// ---------------------------------------------------------------------------

// loadModel and generate are implemented in local_cgo.go (CGo build tag)
// or local_stub.go (pure-Go stub when CGo is disabled).
// This keeps the interface here and isolates the CGo boundary.

// ggufModelName extracts a human-readable name from the GGUF file path.
func ggufModelName(path string) string {
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	name = strings.TrimSuffix(name, ".gguf")
	return name
}

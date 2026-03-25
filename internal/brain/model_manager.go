// Package brain — model_manager.go provides RAM-aware on-demand model loading
// for the Ollama backend (Sprint 17 #3).
//
// ModelManager is called by the Scheduler's drain goroutine before dispatching
// P1/P2 tasks. It checks available RAM, optionally pre-loads the model, and
// can downgrade a 4B model to 2B when memory is constrained.
//
// Integration path (all background):
//
//	Scheduler.runEligible
//	  → ModelManager.EnsureModel(ctx)
//	  → returns model name to use, or "" to defer this drain cycle
//
// P0 (user-waiting) tasks bypass the scheduler entirely and rely on
// Scheduler.ShouldDegrade() for their own gate.
package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
	"github.com/SynapsesOS/synapses/internal/logutil"
)

// Model RAM requirements (model size + 1 GB safety buffer).
const (
	// model2BRAMRequired is the minimum free RAM needed before loading a ~1.5 GB
	// 2B model: 1.5 GB model + 1 GB safety buffer = 2.5 GB.
	// Expressed as integer bytes: 2684354560 = 2.5 * 1024^3
	model2BRAMRequired int64 = 2684354560

	// model4BRAMRequired is the minimum free RAM needed before loading a ~3.7 GB
	// 4B model: 3.7 GB model + 1 GB safety buffer = 4.7 GB.
	// Expressed as integer bytes: 5047730790 ≈ 4.7 * 1024^3
	model4BRAMRequired int64 = 5047730790

	// modelManagerWarmupTimeout is the HTTP timeout for the warmup request.
	// Long enough for Ollama to load a cold model (~10s on an SSD).
	modelManagerWarmupTimeout = 30 * time.Second
)

// ModelManager decides which model to use for background LLM tasks and
// pre-loads it when there is sufficient free RAM. It is safe for concurrent use.
//
// Decision logic in EnsureModel:
//  1. Model already loaded in Ollama → use it at no extra RAM cost.
//  2. Sufficient RAM for preferred model → warm it up, return preferred.
//  3. Preferred is 4B, only 2B fits → warm up 2B fallback, return fallback name.
//     NOTE: Sprint 17 #4 (fallback chains) will make OllamaClients use the returned
//     fallback name to route inference to the 2B tier. Until then, EnsureModel's
//     return value is used as a go/no-go signal only; actual inference uses whichever
//     model the OllamaClients are configured with (primary). The warmup pre-positions
//     the fallback model so Ollama can swap quickly once #4 wires the routing.
//  4. Insufficient RAM for any model → return "" (drain cycle deferred).
//
// When pulse is nil (NullBrain / testing) all RAM checks are skipped and
// EnsureModel returns the primary model unconditionally.
type ModelManager struct {
	pulse     *SystemPulse
	baseURL   string
	primary   string // preferred model for this intelligence mode
	fallback  string // 2B fallback when primary is 4B and RAM is tight; "" if primary is already 2B
	keepAlive int    // keep_alive seconds to pass on warmup requests
	http      *http.Client
}

// NewModelManager creates a ModelManager configured for cfg.
//
// pulse must be the same *SystemPulse passed to the Scheduler so that both
// share the same system-state snapshot. Pass nil to disable RAM gating
// (useful in unit tests without a live system monitor).
func NewModelManager(pulse *SystemPulse, cfg brainconfig.BrainConfig) *ModelManager {
	// Use BaseModelTag() for the raw Ollama model tag (e.g. "qwen3.5:4b"),
	// not the identity name ("synapses/sentry"). Identity names don't encode
	// model size, so is4BModel("synapses/sentry") returns false — but the
	// underlying weights may be 4B and need 3.7 GB of RAM.
	primary := cfg.BaseModelTag()

	fallback := ""
	if is4BModel(primary) {
		// When the primary model is 4B and RAM is insufficient, fall back to
		// the 2B variant. The fallback model name is inferred from the primary.
		fallback = fallback2BFor(primary)
	}

	return &ModelManager{
		pulse:     pulse,
		baseURL:   strings.TrimRight(cfg.OllamaURL, "/"),
		primary:   primary,
		fallback:  fallback,
		keepAlive: cfg.KeepAlive(),
		http:      &http.Client{Timeout: modelManagerWarmupTimeout},
	}
}

// EnsureModel returns the Ollama model name to use for the next drain cycle,
// or "" if no model can be loaded given current RAM constraints.
//
// When the returned model is non-empty, the caller may proceed with LLM tasks.
// When the returned model is "", the caller should skip this drain cycle and
// retry on the next tick; tasks remain in the deferred queue.
func (m *ModelManager) EnsureModel(ctx context.Context) string {
	if m.pulse == nil {
		// No system monitoring (NullBrain / test path) — proceed unconditionally.
		return m.primary
	}

	state := m.pulse.Current()

	// Case 1: A model is already resident in Ollama.
	// Using it costs no additional RAM — proceed with whatever is loaded.
	if state.OllamaModelLoaded != "" {
		return state.OllamaModelLoaded
	}

	available := state.AvailableRAM

	// Case 2: Sufficient RAM for the preferred (primary) model.
	if available >= modelRAMRequired(m.primary) {
		m.warmUp(ctx, m.primary)
		return m.primary
	}

	// Case 3: Primary is a 4B model but only 2B fits.
	if m.fallback != "" {
		if available >= modelRAMRequired(m.fallback) {
			logutil.Warn("brain: model_manager: not enough RAM for %s (%.1f GB free), downgrading to %s\n",
				m.primary, float64(available)/(1024*1024*1024), m.fallback)
			m.warmUp(ctx, m.fallback)
			return m.fallback
		}
	}

	// Case 4: Insufficient RAM for any model.
	logutil.Warn("brain: model_manager: insufficient RAM (%.1f GB free) for any model — deferring drain cycle\n",
		float64(available)/(1024*1024*1024))
	return ""
}

// warmUp sends a no-output generate request to Ollama so the model is resident
// before the actual inference requests arrive. This amortises the model-load
// latency across the batch of tasks that follow.
//
// Errors are silently discarded: if Ollama is unreachable, EnsureModel already
// returned a non-empty model name, and the real inference request will fail
// (and be recorded by the circuit breaker) normally.
func (m *ModelManager) warmUp(ctx context.Context, model string) {
	type warmupOptions struct {
		NumPredict int `json:"num_predict"`
	}
	type warmupRequest struct {
		Model     string        `json:"model"`
		Prompt    string        `json:"prompt"`
		Stream    bool          `json:"stream"`
		KeepAlive *int          `json:"keep_alive,omitempty"`
		Options   warmupOptions `json:"options"`
	}

	body := warmupRequest{
		Model:     model,
		Prompt:    "",
		Stream:    false,
		KeepAlive: &m.keepAlive,
		Options:   warmupOptions{NumPredict: 1},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.baseURL+"/api/generate", bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// modelRAMRequired returns the minimum free RAM (bytes) needed to load model,
// including a 1 GB safety buffer.
func modelRAMRequired(model string) int64 {
	if is4BModel(model) {
		return model4BRAMRequired
	}
	return model2BRAMRequired
}

// is4BModel returns true when the model name corresponds to a ~3.7 GB 4B model.
// Checks for ":4b", "-4b", "_4b" separators (case-insensitive) to avoid
// false positives from larger models like "qwen3.5:14b" or "llama3.1:40b".
func is4BModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, ":4b") ||
		strings.Contains(lower, "-4b") ||
		strings.Contains(lower, "_4b")
}

// fallback2BFor returns the 2B variant model name for a 4B model.
// For synapses/* named models it keeps the namespace; for raw names it
// substitutes "4b" → "2b". Returns "qwen3.5:2b" as the universal fallback.
func fallback2BFor(model string) string {
	// Raw Ollama tag like "qwen3.5:4b" → "qwen3.5:2b"
	if replaced := strings.ReplaceAll(model, ":4b", ":2b"); replaced != model {
		return replaced
	}
	// Suffix patterns like "model-4b-instruct" → substitute 4b→2b
	lower := strings.ToLower(model)
	if idx := strings.Index(lower, "4b"); idx >= 0 {
		return model[:idx] + "2b" + model[idx+2:]
	}
	return "qwen3.5:2b"
}

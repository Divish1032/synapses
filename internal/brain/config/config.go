// Package config provides BrainConfig loading and defaults for synapses-intelligence.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// IntelligenceMode controls which models are loaded and how they are cached,
// allowing the brain to fit different RAM budgets.
type IntelligenceMode string

const (
	// ModeOptimal targets 8 GB RAM systems (<800 MB brain budget).
	// Sentry is always pinned. Librarian, Navigator, and Archivist are loaded
	// on demand and evicted immediately after each request.
	// Guardian falls back to Librarian (no separate Critic slot).
	ModeOptimal IntelligenceMode = "optimal"

	// ModeStandard targets 16 GB RAM systems (~2.2 GB brain budget).
	// Sentry and Librarian are pinned. Critic, Navigator, and Archivist share
	// a single rotating slot with a 5-minute TTL.
	ModeStandard IntelligenceMode = "standard"

	// ModeFull targets 32 GB+ RAM systems (~4.5 GB brain budget).
	// All tiers are pinned in RAM for zero cold-start latency.
	// Q8_0 quantization used for maximum quality.
	ModeFull IntelligenceMode = "full"
)

// BrainConfig holds all configuration for the thinking brain.
type BrainConfig struct {
	// Enabled controls whether the brain is active. Default: false.
	Enabled bool `json:"enabled"`

	// IntelligenceMode controls RAM residency and model quality tier.
	// "optimal" (<800 MB, 8 GB systems), "standard" (~2.2 GB, 16 GB systems),
	// "full" (~4.5 GB, 32 GB+ systems). Leave empty to use legacy auto-scaling.
	IntelligenceMode IntelligenceMode `json:"intelligence_mode,omitempty"`

	// OllamaURL is the base URL of the Ollama server. Default: "http://localhost:11434".
	OllamaURL string `json:"ollama_url,omitempty"`

	// Model is the primary model tag (enrichment fallback when ModelEnrich is unset).
	// Default: "qwen3.5:2b" — base model until fine-tuned SIL models are available.
	// Used by llama-server (default) and Ollama (opt-in) backends.
	Model string `json:"model,omitempty"`

	// FastModel is the model tag for bulk ingestion (fallback when ModelIngest is unset).
	// Default: "qwen3.5:2b" — unified single model until fine-tuned tiers are ready.
	FastModel string `json:"fast_model,omitempty"`

	// --- Tiered Nervous System: per-task model assignment ---
	// Each tier defaults to the appropriate Qwen3.5 model.
	// Set to "" to fall back to FastModel/Model. All 4 can point to the same model.

	// ModelIngest is the model for bulk node summarization at index time.
	// Tier 0 (Reflex): simple extraction, no reasoning needed. Default: "qwen3.5:2b".
	// Future: fine-tuned Sentry 0.5B (see PLAN-5-MODELS.md).
	ModelIngest string `json:"model_ingest,omitempty"`

	// ModelGuardian is the model for rule violation explanations.
	// Tier 1 (Sensory): structured plain-English output. Default: "qwen3.5:2b".
	// Future: fine-tuned Critic 1.5B (see PLAN-5-MODELS.md).
	ModelGuardian string `json:"model_guardian,omitempty"`

	// ModelEnrich is the model for architectural enrichment and insight generation.
	// Tier 2 (Specialist): complex analysis across multiple callers/callees. Default: "qwen3.5:2b".
	// Future: fine-tuned Librarian 1.5B (see PLAN-5-MODELS.md).
	ModelEnrich string `json:"model_enrich,omitempty"`

	// ModelOrchestrate is the model for multi-agent conflict resolution.
	// Tier 3 (Architect): deep reasoning about competing scope claims. Default: "qwen3.5:2b".
	// Future: fine-tuned Navigator 2B (see PLAN-5-MODELS.md).
	ModelOrchestrate string `json:"model_orchestrate,omitempty"`

	// ModelArchivist is the model for session memory synthesis.
	// Tier 2 (Specialist): analyzes agent sessions and extracts persistent memories.
	// Default: same as ModelOrchestrate. Future: fine-tuned Archivist 2B.
	ModelArchivist string `json:"model_archivist,omitempty"`

	// Backend selects the LLM backend.
	// "ollama" (default): calls the Ollama HTTP API. Recommended for all users.
	// "local": loads a GGUF file directly via gollama (no Ollama required).
	// Build with -tags llamacpp and CGO_ENABLED=1 for the local backend.
	Backend string `json:"backend,omitempty"`

	// GGUFPath is the path to the fine-tuned GGUF model file.
	// Only used when Backend == "local". If empty, auto-computed as ModelDir/HFFilename.
	// Example: "~/.synapses/models/sil-coder-Q5_K_M.gguf"
	GGUFPath string `json:"gguf_path,omitempty"`

	// ModelDir is the directory where GGUF models are stored.
	// Default: ~/.synapses/models/
	ModelDir string `json:"model_dir,omitempty"`

	// HFRepo is the HuggingFace repository to download the model from.
	// Example: "divish/sil-coder"
	// Used by `brain config download` and `brain serve` (auto-download on first run).
	HFRepo string `json:"hf_repo,omitempty"`

	// HFFilename is the GGUF filename within the HuggingFace repo.
	// Default: "sil-coder-Q5_K_M.gguf"
	HFFilename string `json:"hf_filename,omitempty"`

	// TimeoutMS is the per-request LLM timeout in milliseconds.
	// The HTTP server WriteTimeout is set to 2× this value. Default: 60000 (60s).
	// Must exceed the slowest LLM inference time on your hardware (~25s for 9b CPU).
	TimeoutMS int `json:"timeout_ms,omitempty"`

	// DBPath is the path to the brain's own SQLite database.
	// Default: ~/.synapses/brain.sqlite
	DBPath string `json:"db_path,omitempty"`

	// Port is the HTTP server port for sidecar mode. Default: 11435.
	Port int `json:"port,omitempty"`

	// v0.1.0 feature flags — all default to true when Enabled=true.
	Ingest      bool `json:"ingest"`
	Enrich      bool `json:"enrich"`
	Guardian    bool `json:"guardian"`
	Orchestrate bool `json:"orchestrate"`
	Memorize    bool `json:"memorize"`

	// v0.2.0: Context Packet and SDLC intelligence.
	// ContextBuilder enables BuildContextPacket (default: true when Enabled=true).
	ContextBuilder bool `json:"context_builder"`
	// LearningEnabled enables the decision log and co-occurrence learning (default: true).
	LearningEnabled bool `json:"learning_enabled"`
	// DefaultPhase is the initial SDLC phase stored in brain.sqlite if none is set.
	// Values: "planning" | "development" | "testing" | "review" | "deployment"
	// Default: "development"
	DefaultPhase string `json:"default_phase,omitempty"`
	// DefaultMode is the initial quality mode stored in brain.sqlite if none is set.
	// Values: "quick" | "standard" | "enterprise"
	// Default: "standard"
	DefaultMode string `json:"default_mode,omitempty"`

	// --- Ollama-free embedding (v0.7.0) ---
	// EmbeddingEnabled activates the /v1/embed endpoint on the brain HTTP server.
	// When true, brain spawns a llama-server subprocess in embedding-only mode.
	// No Ollama required. Default: false.
	EmbeddingEnabled bool `json:"embedding_enabled"`

	// EmbedModelPath is the path to the embedding GGUF model file.
	// Default: ModelDir/nomic-embed-text-v1.5.Q4_K_M.gguf
	EmbedModelPath string `json:"embed_model_path,omitempty"`

	// EmbedHFRepo is the HuggingFace repo to download the embedding model from.
	// Default: "nomic-ai/nomic-embed-text-v1.5-GGUF"
	EmbedHFRepo string `json:"embed_hf_repo,omitempty"`

	// EmbedHFFilename is the GGUF filename in the HuggingFace repo.
	// Default: "nomic-embed-text-v1.5.Q4_K_M.gguf"
	EmbedHFFilename string `json:"embed_hf_filename,omitempty"`

	// EmbedPort is the internal port used by the llama-server subprocess.
	// This is NOT exposed externally — brain proxies /v1/embed to it.
	// Default: 11437
	EmbedPort int `json:"embed_port,omitempty"`

	// LlamaBinDir is the directory where llama.cpp binaries are installed.
	// Default: ~/.synapses/bin
	LlamaBinDir string `json:"llama_bin_dir,omitempty"`

	// LlamaCPPVersion pins the llama.cpp GitHub release used for binary downloads.
	// Default: "b5618"
	LlamaCPPVersion string `json:"llama_cpp_version,omitempty"`

	// PulseURL is the base URL of the synapses-pulse analytics sidecar.
	// When set, every LLM inference call is reported as a BrainUsageEvent.
	// Leave empty to disable (default). Example: "http://localhost:11437"
	PulseURL string `json:"pulse_url,omitempty"`

	// PulseTimeoutSec is the per-request HTTP timeout for pulse calls.
	// Defaults to 2 when PulseURL is set.
	PulseTimeoutSec int `json:"pulse_timeout_sec,omitempty"`
}

// DefaultConfig returns a BrainConfig with all defaults applied.
func DefaultConfig() BrainConfig {
	home, _ := os.UserHomeDir()
	return BrainConfig{
		Enabled:          false,
		Backend:          "ollama",
		OllamaURL:        "http://localhost:11434",
		Model:            "qwen3.5:2b",
		FastModel:        "qwen3.5:2b",
		ModelIngest:      "qwen3.5:2b",
		ModelGuardian:    "qwen3.5:2b",
		ModelEnrich:      "qwen3.5:2b",
		ModelOrchestrate: "qwen3.5:2b",
		TimeoutMS:        60000,
		DBPath:           filepath.Join(home, ".synapses", "brain.sqlite"),
		ModelDir:         filepath.Join(home, ".synapses", "models"),
		HFRepo:           "Qwen/Qwen3.5-2B-Instruct-GGUF",
		HFFilename:       "qwen3.5-2b-instruct-q4_k_m.gguf",
		Port:             11435,
		Ingest:           true,
		Enrich:           true,
		Guardian:         true,
		Orchestrate:      true,
		ContextBuilder:   true,
		LearningEnabled:  true,
		DefaultPhase:     "development",
		DefaultMode:      "standard",
		EmbeddingEnabled: false,
		EmbedHFRepo:      "nomic-ai/nomic-embed-text-v1.5-GGUF",
		EmbedHFFilename:  "nomic-embed-text-v1.5.Q4_K_M.gguf",
		EmbedPort:        11437,
		LlamaBinDir:     filepath.Join(home, ".synapses", "bin"),
		LlamaCPPVersion: "b5618",
	}
}

// DefaultConfigPath returns the conventional path for brain.json:
// $BRAIN_CONFIG if set, otherwise ~/.synapses/brain.json.
func DefaultConfigPath() string {
	if p := os.Getenv("BRAIN_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".synapses", "brain.json")
}

// SaveFile writes cfg as indented JSON to path, creating parent directories
// as needed.
func SaveFile(path string, cfg BrainConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// LoadFile reads a JSON config file and merges it onto the defaults.
// Missing fields in the file retain their default values.
func LoadFile(path string) (BrainConfig, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	cfg.applyDefaults()
	return cfg, nil
}

// applyDefaults fills in zero values with defaults.
// Tier models fall back to the legacy fast_model/model fields if unset.
func (c *BrainConfig) applyDefaults() {
	if c.Backend == "" {
		c.Backend = "ollama"
	}
	if c.OllamaURL == "" {
		c.OllamaURL = "http://localhost:11434"
	}
	if c.Model == "" {
		c.Model = "qwen3.5:2b"
	}
	if c.FastModel == "" {
		c.FastModel = c.Model
	}
	// Tier fallback chain: tier model → Model → hardcoded default.
	// All tiers share qwen3.5:2b until fine-tuned SIL models are available.
	if c.ModelIngest == "" {
		c.ModelIngest = c.FastModel
	}
	if c.ModelGuardian == "" {
		c.ModelGuardian = c.Model
	}
	if c.ModelEnrich == "" {
		c.ModelEnrich = c.Model
	}
	if c.ModelOrchestrate == "" {
		c.ModelOrchestrate = c.Model
	}
	if c.ModelArchivist == "" {
		// Archivist always uses a Qwen 3.5 base model (not fine-tuned).
		// Default to qwen3.5:2b regardless of what ModelOrchestrate is set to,
		// so that changing the orchestrate tier doesn't accidentally break Archivist.
		c.ModelArchivist = "qwen3.5:2b"
	}
	if c.TimeoutMS <= 0 {
		c.TimeoutMS = 60000
	}
	if c.Port <= 0 {
		c.Port = 11435
	}
	if c.DBPath == "" {
		home, _ := os.UserHomeDir()
		c.DBPath = filepath.Join(home, ".synapses", "brain.sqlite")
	}
	if c.DefaultPhase == "" {
		c.DefaultPhase = "development"
	}
	if c.DefaultMode == "" {
		c.DefaultMode = "standard"
	}
	if c.ModelDir == "" {
		home, _ := os.UserHomeDir()
		c.ModelDir = filepath.Join(home, ".synapses", "models")
	}
	if c.HFFilename == "" {
		c.HFFilename = "qwen3.5-2b-instruct-q4_k_m.gguf"
	}
	if c.HFRepo == "" && c.Backend == "local" {
		c.HFRepo = "Qwen/Qwen3.5-2B-Instruct-GGUF"
	}
	// Expand leading ~/ in paths.
	for _, p := range []*string{&c.DBPath, &c.GGUFPath, &c.ModelDir} {
		if strings.HasPrefix(*p, "~/") {
			home, _ := os.UserHomeDir()
			*p = filepath.Join(home, (*p)[2:])
		}
	}
	// Auto-compute GGUFPath from ModelDir+HFFilename when backend needs a GGUF file.
	if c.Backend == "local" && c.GGUFPath == "" {
		c.GGUFPath = filepath.Join(c.ModelDir, c.HFFilename)
	}
	// Embedding defaults.
	if c.EmbedHFRepo == "" {
		c.EmbedHFRepo = "nomic-ai/nomic-embed-text-v1.5-GGUF"
	}
	if c.EmbedHFFilename == "" {
		c.EmbedHFFilename = "nomic-embed-text-v1.5.Q4_K_M.gguf"
	}
	if c.EmbedPort <= 0 {
		c.EmbedPort = 11437
	}
	if c.LlamaBinDir == "" {
		home, _ := os.UserHomeDir()
		c.LlamaBinDir = filepath.Join(home, ".synapses", "bin")
	}
	if c.LlamaCPPVersion == "" {
		c.LlamaCPPVersion = "b5618"
	}
	// Auto-compute EmbedModelPath from ModelDir+EmbedHFFilename when not set.
	if c.EmbeddingEnabled && c.EmbedModelPath == "" {
		c.EmbedModelPath = filepath.Join(c.ModelDir, c.EmbedHFFilename)
	}

	// Smart model selection: if any tier model is set to "auto", auto-configure
	// based on system RAM. Pass 0 here as a sentinel — actual RAM detection
	// happens in main.go before the backend switch.
	if c.ModelIngest == "auto" || c.ModelGuardian == "auto" ||
		c.ModelEnrich == "auto" || c.ModelOrchestrate == "auto" || c.ModelArchivist == "auto" {
		c.AutoConfigureModels(0)
	}
}

// AutoConfigureModels sets per-tier model tags based on IntelligenceMode.
//
// The three modes map to the specialist/generalist split:
//   - Fine-tuned (FT) models handle code-structural tasks: Sentry, Librarian, Critic.
//   - Base Qwen 3.5 2B handles general reasoning tasks: Navigator, Archivist (always).
//
// Model tag conventions:
//
//	synapses/sentry:q4   — Sentry 0.5B FT, Q4_K_M quantization
//	synapses/sentry:q8   — Sentry 0.5B FT, Q8_0 quantization
//	synapses/librarian:q4 — Librarian 1.5B FT, Q4_K_M
//	synapses/librarian:q8 — Librarian 1.5B FT, Q8_0
//	synapses/critic:q4   — Critic 1.5B FT, Q4_K_M
//	synapses/critic:q8   — Critic 1.5B FT, Q8_0
//	qwen3.5:2b           — Base Qwen 3.5 2B (Navigator + Archivist always)
//
// Mode summary:
//
//	| Tier      | Optimal (8GB)          | Standard (16GB)        | Full (32GB+)           |
//	|-----------|------------------------|------------------------|------------------------|
//	| Sentry T0 | synapses/sentry:q4     | synapses/sentry:q4     | synapses/sentry:q8     |
//	| Librarian | synapses/librarian:q4  | synapses/librarian:q4  | synapses/librarian:q8  |
//	| Guardian  | synapses/librarian:q4  | synapses/critic:q4     | synapses/critic:q8     |
//	| Navigator | qwen3.5:2b             | qwen3.5:2b             | qwen3.5:2b             |
//	| Archivist | qwen3.5:2b             | qwen3.5:2b             | qwen3.5:2b             |
//
// In Optimal mode Guardian shares Librarian's model slot (sequential queue,
// rarely contended in practice). In Standard/Full, Critic gets its own slot.
//
// totalRAMGB is used only when IntelligenceMode is "" (legacy auto-scaling path).
func (c *BrainConfig) AutoConfigureModels(totalRAMGB float64) {
	switch c.IntelligenceMode {
	case ModeOptimal:
		c.ModelIngest = "synapses/sentry:q4"
		c.ModelEnrich = "synapses/librarian:q4"
		// Guardian shares Librarian's model in Optimal mode — same Ollama slot,
		// sequential queue. Saves ~1.1 GB vs running a separate Critic model.
		c.ModelGuardian = "synapses/librarian:q4"
		c.ModelOrchestrate = "qwen3.5:2b"
		c.ModelArchivist = "qwen3.5:2b"

	case ModeStandard:
		c.ModelIngest = "synapses/sentry:q4"
		c.ModelEnrich = "synapses/librarian:q4"
		c.ModelGuardian = "synapses/critic:q4"
		c.ModelOrchestrate = "qwen3.5:2b"
		c.ModelArchivist = "qwen3.5:2b"

	case ModeFull:
		c.ModelIngest = "synapses/sentry:q8"
		c.ModelEnrich = "synapses/librarian:q8"
		c.ModelGuardian = "synapses/critic:q8"
		c.ModelOrchestrate = "qwen3.5:2b"
		c.ModelArchivist = "qwen3.5:2b"

	default:
		// Legacy auto-scaling: no IntelligenceMode set.
		// Keep existing behaviour until user explicitly picks a mode.
		c.ModelIngest = "qwen3.5:2b"
		c.ModelGuardian = "qwen3.5:2b"
		switch {
		case totalRAMGB <= 16:
			c.ModelEnrich = "qwen3.5:2b"
			c.ModelOrchestrate = "qwen3.5:2b"
		case totalRAMGB <= 24:
			c.ModelEnrich = "qwen3.5:4b"
			c.ModelOrchestrate = "qwen3.5:4b"
		default:
			c.ModelEnrich = "qwen3.5:4b"
			c.ModelOrchestrate = "qwen3.5:9b"
		}
		c.ModelArchivist = "qwen3.5:2b"
	}
}

// ProbeAndDowngradeModels verifies that each tier's configured model is present
// in Ollama's local library. Tiers whose model is missing are downgraded to the
// ingest-tier model (the lowest common denominator that must always be present).
// Logs a warning for each downgraded tier. Returns nil if Ollama is unreachable
// — startup should continue with the originally configured models in that case.
//
// ollamaURL must be the base URL passed to Ollama (e.g. "http://localhost:11434").
// listFn is an injection point for testing; pass nil to use the real Ollama API.
func (c *BrainConfig) ProbeAndDowngradeModels(ctx context.Context, ollamaURL string, listFn func(context.Context, string) ([]string, error)) error {
	if listFn == nil {
		return nil // real implementation supplied by brain package to avoid circular import
	}
	available, err := listFn(ctx, ollamaURL)
	if err != nil {
		// Ollama unreachable at probe time — not fatal, keep configured models.
		return nil
	}
	// normalize strips :latest for comparison — Ollama's /api/tags returns
	// "synapses/navigator:latest" but brain.json typically omits the tag.
	normalize := func(s string) string {
		return strings.TrimSuffix(strings.TrimSpace(s), ":latest")
	}
	present := make(map[string]bool, len(available))
	for _, m := range available {
		present[normalize(m)] = true
	}
	base := c.ModelIngest
	type tierField struct {
		name  string
		value *string
	}
	tiers := []tierField{
		{"guardian", &c.ModelGuardian},
		{"enrich", &c.ModelEnrich},
		{"orchestrate", &c.ModelOrchestrate},
		{"archivist", &c.ModelArchivist},
	}
	for _, t := range tiers {
		if *t.value != "" && !present[normalize(*t.value)] {
			// keep base in canonical form; if it's also missing, nothing we can do
			fmt.Fprintf(os.Stderr, "brain: tier %q model %q not found in Ollama — downgrading to %q\n",
				t.name, *t.value, base)
			*t.value = base
		}
	}
	return nil
}

// keepAliveValues returns the keep_alive seconds for enrich, orchestrate, and
// archivist tiers based on the configured IntelligenceMode.
//
//   - Optimal  : Librarian JIT (0), Navigator/Archivist JIT (0)
//   - Standard : Librarian pinned (-1), Navigator/Archivist 5-min TTL (300)
//   - Full     : all pinned (-1)
//   - ""       : legacy behaviour — Librarian pinned, others use Ollama default
func (c *BrainConfig) KeepAliveValues() (kaEnrich, kaOrchestrate, kaArchivist int) {
	switch c.IntelligenceMode {
	case ModeOptimal:
		return 0, 0, 0
	case ModeStandard:
		return -1, 300, 300
	case ModeFull:
		return -1, -1, -1
	default:
		// No mode set — preserve existing behaviour: Librarian pinned,
		// Navigator/Archivist use Ollama's server-side default (5 min).
		// Return -1 for enrich and 0 for the others as a safe sentinel;
		// callers that want "use server default" should not call WithKeepAlive.
		// We return -1 / 0 / 0 which matches the pre-mode hardcoded values.
		return -1, 0, 0
	}
}

// ModelsToInstall returns a deduplicated, sorted list of model tags needed
// for the current tier configuration.
func (c *BrainConfig) ModelsToInstall() []string {
	seen := make(map[string]struct{})
	for _, m := range []string{c.ModelIngest, c.ModelGuardian, c.ModelEnrich, c.ModelOrchestrate, c.ModelArchivist} {
		if m != "" {
			seen[m] = struct{}{}
		}
	}
	models := make([]string, 0, len(seen))
	for m := range seen {
		models = append(models, m)
	}
	sort.Strings(models)
	return models
}

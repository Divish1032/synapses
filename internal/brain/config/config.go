// Package config provides BrainConfig loading and defaults for synapses-intelligence.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// IntelligenceMode controls which models are loaded and how they are cached,
// allowing the brain to fit different RAM budgets.
type IntelligenceMode string

const (
	// ModeOptimal targets 8 GB RAM systems.
	// Guardian shares Librarian's identity (no separate Critic).
	// All identities share qwen3.5:2b weights (~1.5GB), evicts after 2 min.
	ModeOptimal IntelligenceMode = "optimal"

	// ModeStandard targets 16 GB+ RAM systems.
	// Critic gets its own identity for violation explanations.
	// All identities share qwen3.5:4b Q4_K_M weights (~2.7GB), evicts after 5 min.
	ModeStandard IntelligenceMode = "standard"

	// ModeFull is identical to ModeStandard. Kept for config compatibility.
	// All identities share qwen3.5:4b Q4_K_M weights (~2.7GB), pinned.
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
		ModelArchivist:   "qwen3.5:2b",
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
		Memorize:         true,
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
	return os.WriteFile(path, append(data, '\n'), 0o600)
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
	// All tiers share the base model for the current intelligence mode.
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
		// Archivist uses the base model for the current intelligence mode.
		// BaseModelTag() returns qwen3.5:2b for optimal, qwen3.5:4b for standard/full.
		c.ModelArchivist = c.BaseModelTag()
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
		switch c.IntelligenceMode {
		case ModeStandard, ModeFull:
			c.HFFilename = "qwen3.5-4b-instruct-q4_k_m.gguf"
		default:
			c.HFFilename = "qwen3.5-2b-instruct-q4_k_m.gguf"
		}
	}
	if c.HFRepo == "" && c.Backend == "local" {
		switch c.IntelligenceMode {
		case ModeStandard, ModeFull:
			c.HFRepo = "Qwen/Qwen3.5-4B-Instruct-GGUF"
		default:
			c.HFRepo = "Qwen/Qwen3.5-2B-Instruct-GGUF"
		}
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
// Optimal mode uses base Qwen 3.5 2B (~1.5 GB). Standard and Full modes use
// Qwen 3.5 4B Q4_K_M (~2.7 GB) — the 4B model reaches IFEval 89.8, the
// threshold for reliable format compliance in classification tasks.
//
// All tiers in a given mode share the same base model via Ollama Modelfile
// identities (system prompts + parameters). Ollama deduplicates weights:
// all 5 identities share one copy of the weights in RAM.
//
// Model tags:
//
//	synapses/sentry    — Gate & Router (classify entities)
//	synapses/librarian — Enricher (analyze graph slices)
//	synapses/critic    — Guardian (review diffs for violations)
//	synapses/navigator — Orchestrator (resolve scope conflicts)
//	synapses/archivist — Memory synthesizer (session summaries)
//
// In Optimal mode Guardian reuses Librarian's identity (same model, different
// system prompt would be wasted — they share weights anyway).
//
// totalRAMGB is used only when IntelligenceMode is "" (legacy auto-scaling path).
func (c *BrainConfig) AutoConfigureModels(totalRAMGB float64) {
	switch c.IntelligenceMode {
	case ModeOptimal:
		c.ModelIngest = "synapses/sentry"
		c.ModelEnrich = "synapses/librarian"
		c.ModelGuardian = "synapses/librarian" // shares slot in Optimal
		c.ModelOrchestrate = "synapses/navigator"
		c.ModelArchivist = "synapses/archivist"

	case ModeStandard, ModeFull:
		c.ModelIngest = "synapses/sentry"
		c.ModelEnrich = "synapses/librarian"
		c.ModelGuardian = "synapses/critic"
		c.ModelOrchestrate = "synapses/navigator"
		c.ModelArchivist = "synapses/archivist"

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

// KeepAlive returns the keep_alive seconds for this intelligence mode.
//
// All 5 Ollama identities (synapses/sentry, synapses/librarian, etc.) share the
// same base model weights, so Ollama treats them as a single loaded model.
// A single keep_alive value applies to all tiers — the last request's value wins.
//
// Per-mode policy (Sprint 17 #3 — Model Manager):
//
//	optimal  (8 GB)  : 120s — model evicts after 2 min of idle; saves ~1.5 GB on
//	                          tight 8 GB machines between bursts.
//	standard (16 GB) : 300s — model evicts after 5 min of idle; good tradeoff for
//	                          developer workstations where the model is used often.
//	full     (32 GB+): -1   — model stays pinned; the machine can afford it.
//	default (unset)  : -1   — backward-compatible behaviour; pinned.
func (c *BrainConfig) KeepAlive() int {
	switch c.IntelligenceMode {
	case ModeOptimal:
		return 120
	case ModeStandard:
		return 300
	case ModeFull:
		return -1
	default:
		return -1
	}
}

// KeepAliveValues returns the keep_alive seconds for guardian, enrich,
// orchestrate, and archivist tiers. All four values are identical — they
// delegate to KeepAlive() because all Ollama identities share the same model
// weights. Retained for backward compatibility with existing callers.
func (c *BrainConfig) KeepAliveValues() (kaGuardian, kaEnrich, kaOrchestrate, kaArchivist int) {
	ka := c.KeepAlive()
	return ka, ka, ka, ka
}

// BaseModelTag returns the raw Ollama model tag backing all synapses/*
// identities for this intelligence mode. This is the actual model weights
// loaded in RAM — not the identity name (synapses/sentry etc.).
//
// Used by ModelManager for RAM requirement checks: is4BModel("synapses/sentry")
// returns false, but is4BModel(BaseModelTag()) correctly returns true when the
// mode is standard/full.
//
//	optimal  → "qwen3.5:2b"  (~1.5 GB, IFEval ~80)
//	standard → "qwen3.5:4b"  (~2.7 GB, IFEval 89.8, Q4_K_M)
//	full     → "qwen3.5:4b"  (~2.7 GB, IFEval 89.8, Q4_K_M)
//	""       → c.Model       (legacy path, no mode set)
func (c *BrainConfig) BaseModelTag() string {
	switch c.IntelligenceMode {
	case ModeOptimal:
		return "qwen3.5:2b"
	case ModeStandard, ModeFull:
		return "qwen3.5:4b"
	default:
		return c.Model
	}
}


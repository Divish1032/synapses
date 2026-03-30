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
	// Uses qwen3.5:0.8b (Sentry) + qwen3.5:2b (all other tiers).
	// Models evict after 2 min idle.
	ModeOptimal IntelligenceMode = "optimal"

	// ModeStandard targets 16 GB+ RAM systems.
	// Uses qwen3.5:0.8b (Sentry) + qwen3.5:2b (Critic/Navigator/Archivist) +
	// qwen3.5:4b (Librarian). Models evict after 5 min idle.
	ModeStandard IntelligenceMode = "standard"

	// ModeFull targets 32 GB+ RAM systems.
	// Uses qwen3.5:0.8b (Sentry) + qwen3.5:4b (all other tiers).
	// Models pinned in RAM.
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
		ModelIngest:      "qwen3.5:0.8b",
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
// Each tier is assigned the smallest model that can reliably handle its task.
// System prompts are passed per-request (via WithSystemPrompt on OllamaClient),
// so no Ollama Modelfile identity registration is needed.
//
// Model matrix (tier × mode):
//
//	Tier        Optimal (8GB)  Standard (16GB)  Full (32GB+)
//	──────────  ─────────────  ───────────────  ────────────
//	Sentry      qwen3.5:0.8b   qwen3.5:0.8b    qwen3.5:0.8b
//	Critic      qwen3.5:2b     qwen3.5:2b      qwen3.5:4b
//	Librarian   qwen3.5:2b     qwen3.5:4b      qwen3.5:4b
//	Navigator   qwen3.5:2b     qwen3.5:2b      qwen3.5:4b
//	Archivist   qwen3.5:2b     qwen3.5:2b      qwen3.5:4b
//
// Sentry (entity classification + JSON tagging) uses 0.8b everywhere — the
// task is simple extraction that doesn't need reasoning capability.
//
// totalRAMGB is used only when IntelligenceMode is "" (legacy auto-scaling path).
func (c *BrainConfig) AutoConfigureModels(totalRAMGB float64) {
	switch c.IntelligenceMode {
	case ModeOptimal:
		// 8 GB budget: 0.8b (~500MB) + 2b (~1.5GB) = ~2GB peak
		c.ModelIngest = "qwen3.5:0.8b"
		c.ModelEnrich = "qwen3.5:2b"
		c.ModelGuardian = "qwen3.5:2b"
		c.ModelOrchestrate = "qwen3.5:2b"
		c.ModelArchivist = "qwen3.5:2b"

	case ModeStandard:
		// 16 GB budget: 0.8b + 2b + 4b
		c.ModelIngest = "qwen3.5:0.8b"
		c.ModelEnrich = "qwen3.5:4b"  // benefits most from reasoning capability
		c.ModelGuardian = "qwen3.5:2b"
		c.ModelOrchestrate = "qwen3.5:2b"
		c.ModelArchivist = "qwen3.5:2b"

	case ModeFull:
		// 32 GB+ budget: 0.8b + 4b for all reasoning tiers
		c.ModelIngest = "qwen3.5:0.8b"
		c.ModelEnrich = "qwen3.5:4b"
		c.ModelGuardian = "qwen3.5:4b"
		c.ModelOrchestrate = "qwen3.5:4b"
		c.ModelArchivist = "qwen3.5:4b"

	default:
		// Legacy auto-scaling: no IntelligenceMode set.
		c.ModelIngest = "qwen3.5:0.8b"
		c.ModelGuardian = "qwen3.5:2b"
		switch {
		case totalRAMGB <= 16:
			c.ModelEnrich = "qwen3.5:2b"
			c.ModelOrchestrate = "qwen3.5:2b"
		case totalRAMGB <= 24:
			c.ModelEnrich = "qwen3.5:4b"
			c.ModelOrchestrate = "qwen3.5:2b"
		default:
			c.ModelEnrich = "qwen3.5:4b"
			c.ModelOrchestrate = "qwen3.5:4b"
		}
		c.ModelArchivist = "qwen3.5:2b"
	}
}

// KeepAlive returns the keep_alive seconds for this intelligence mode.
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
// orchestrate, and archivist tiers. Retained for backward compatibility.
func (c *BrainConfig) KeepAliveValues() (kaGuardian, kaEnrich, kaOrchestrate, kaArchivist int) {
	ka := c.KeepAlive()
	return ka, ka, ka, ka
}

// BaseModelTag returns the heaviest Ollama model tag used in this intelligence
// mode. Used by ModelManager for RAM requirement checks.
//
//	optimal  → "qwen3.5:2b"  (heaviest tier model)
//	standard → "qwen3.5:4b"  (Librarian uses 4b)
//	full     → "qwen3.5:4b"  (all reasoning tiers use 4b)
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

// ModelsRequired returns all distinct Ollama model tags needed for this mode.
// Used by brain setup to know which models to pull.
func (c *BrainConfig) ModelsRequired() []string {
	seen := map[string]bool{}
	var models []string
	for _, m := range []string{c.ModelIngest, c.ModelGuardian, c.ModelEnrich, c.ModelOrchestrate, c.ModelArchivist} {
		if m != "" && !seen[m] {
			seen[m] = true
			models = append(models, m)
		}
	}
	return models
}

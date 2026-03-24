package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain/config"
)

// ---------------------------------------------------------------------------
// DefaultConfigPath
// ---------------------------------------------------------------------------

func TestDefaultConfigPath_EnvOverride(t *testing.T) {
	want := "/tmp/custom-brain.json"
	t.Setenv("BRAIN_CONFIG", want)

	got := config.DefaultConfigPath()
	if got != want {
		t.Errorf("DefaultConfigPath() = %q, want %q", got, want)
	}
}

func TestDefaultConfigPath_DefaultLocation(t *testing.T) {
	// Ensure env var is not set.
	t.Setenv("BRAIN_CONFIG", "")

	got := config.DefaultConfigPath()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	want := filepath.Join(home, ".synapses", "brain.json")
	if got != want {
		t.Errorf("DefaultConfigPath() = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// SaveFile + LoadFile round-trip
// ---------------------------------------------------------------------------

func TestSaveAndLoadFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "brain.json")

	cfg := config.DefaultConfig()
	cfg.Enabled = true
	cfg.Port = 19999
	cfg.OllamaURL = "http://example.com:11434"

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile() error: %v", err)
	}

	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile() error: %v", err)
	}
	if loaded.Port != 19999 {
		t.Errorf("Port = %d, want 19999", loaded.Port)
	}
	if loaded.OllamaURL != "http://example.com:11434" {
		t.Errorf("OllamaURL = %q, want http://example.com:11434", loaded.OllamaURL)
	}
}

func TestLoadFile_MissingFile_ReturnsError(t *testing.T) {
	_, err := config.LoadFile("/nonexistent/path/brain.json")
	if err == nil {
		t.Error("LoadFile(missing) should return an error")
	}
}

func TestLoadFile_InvalidJSON_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := config.LoadFile(path)
	if err == nil {
		t.Error("LoadFile(invalid JSON) should return an error")
	}
}

// ---------------------------------------------------------------------------
// applyDefaults — tested via partial JSON + LoadFile
// ---------------------------------------------------------------------------

// writePartialConfig writes a minimal JSON config to a temp file and returns its path.
func writePartialConfig(t *testing.T, v map[string]interface{}) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "brain.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestApplyDefaults_EmptyBackendFallsToOllama(t *testing.T) {
	// Write JSON with backend explicitly set to empty string.
	path := writePartialConfig(t, map[string]interface{}{"backend": ""})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Backend != "ollama" {
		t.Errorf("Backend = %q after applyDefaults, want %q", cfg.Backend, "ollama")
	}
}

func TestApplyDefaults_EmptyOllamaURLFallsToDefault(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"ollama_url": ""})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Errorf("OllamaURL = %q after applyDefaults, want %q", cfg.OllamaURL, "http://localhost:11434")
	}
}

func TestApplyDefaults_EmptyModelFallsToDefault(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"model": ""})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Model != "qwen3.5:2b" {
		t.Errorf("Model = %q after applyDefaults, want %q", cfg.Model, "qwen3.5:2b")
	}
}

func TestApplyDefaults_ModelIngestFallsToFastModel(t *testing.T) {
	// Set fast_model to a custom value; leave model_ingest unset.
	path := writePartialConfig(t, map[string]interface{}{
		"fast_model":   "customfast:1b",
		"model_ingest": "",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// ModelIngest should fall back to FastModel.
	if cfg.ModelIngest != "customfast:1b" {
		t.Errorf("ModelIngest = %q after applyDefaults, want %q", cfg.ModelIngest, "customfast:1b")
	}
}

func TestApplyDefaults_ModelArchivistDefaultsToQwen35(t *testing.T) {
	// Archivist always defaults to qwen3.5:2b independently of ModelOrchestrate.
	// Navigator and Archivist are base-model tiers; they must not inherit FT model tags.
	path := writePartialConfig(t, map[string]interface{}{
		"model_orchestrate": "myorch:3b",
		"model_archivist":   "",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.ModelArchivist != "qwen3.5:2b" {
		t.Errorf("ModelArchivist = %q after applyDefaults, want %q", cfg.ModelArchivist, "qwen3.5:2b")
	}
}

func TestApplyDefaults_TimeoutMSZeroFallsToDefault(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"timeout_ms": 0})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.TimeoutMS != 60000 {
		t.Errorf("TimeoutMS = %d after applyDefaults, want 60000", cfg.TimeoutMS)
	}
}

func TestApplyDefaults_PortZeroFallsToDefault(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"port": 0})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Port != 11435 {
		t.Errorf("Port = %d after applyDefaults, want 11435", cfg.Port)
	}
}

func TestApplyDefaults_TildeExpansionInDBPath(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"db_path": "~/test/brain.sqlite"})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if strings.HasPrefix(cfg.DBPath, "~/") {
		t.Errorf("DBPath = %q — tilde was not expanded", cfg.DBPath)
	}
	home, _ := os.UserHomeDir()
	wantPrefix := filepath.Join(home, "test")
	if !strings.HasPrefix(cfg.DBPath, wantPrefix) {
		t.Errorf("DBPath = %q, want prefix %q", cfg.DBPath, wantPrefix)
	}
}

func TestApplyDefaults_LocalBackendAutoComputesGGUFPath(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{
		"backend":      "local",
		"gguf_path":    "",
		"model_dir":    "/tmp/models",
		"hf_filename":  "sil-coder.gguf",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := "/tmp/models/sil-coder.gguf"
	if cfg.GGUFPath != want {
		t.Errorf("GGUFPath = %q after applyDefaults, want %q", cfg.GGUFPath, want)
	}
}

func TestApplyDefaults_EmbeddingEnabledAutoComputesEmbedModelPath(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{
		"embedding_enabled": true,
		"embed_model_path":  "",
		"model_dir":         "/tmp/models",
		"embed_hf_filename": "nomic-embed.gguf",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := "/tmp/models/nomic-embed.gguf"
	if cfg.EmbedModelPath != want {
		t.Errorf("EmbedModelPath = %q after applyDefaults, want %q", cfg.EmbedModelPath, want)
	}
}

// ---------------------------------------------------------------------------
// AutoConfigureModels — additional coverage beyond config_test.go
// ---------------------------------------------------------------------------

func TestAutoConfigureModels_8GB(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(8)
	if cfg.ModelEnrich != "qwen3.5:2b" {
		t.Errorf("8GB ModelEnrich = %q, want qwen3.5:2b", cfg.ModelEnrich)
	}
	if cfg.ModelOrchestrate != "qwen3.5:2b" {
		t.Errorf("8GB ModelOrchestrate = %q, want qwen3.5:2b", cfg.ModelOrchestrate)
	}
}

func TestAutoConfigureModels_16GB(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(16)
	if cfg.ModelEnrich != "qwen3.5:2b" {
		t.Errorf("16GB ModelEnrich = %q, want qwen3.5:2b", cfg.ModelEnrich)
	}
	if cfg.ModelOrchestrate != "qwen3.5:2b" {
		t.Errorf("16GB ModelOrchestrate = %q, want qwen3.5:2b", cfg.ModelOrchestrate)
	}
}

func TestAutoConfigureModels_20GB(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(20)
	if cfg.ModelEnrich != "qwen3.5:4b" {
		t.Errorf("20GB ModelEnrich = %q, want qwen3.5:4b", cfg.ModelEnrich)
	}
	if cfg.ModelOrchestrate != "qwen3.5:4b" {
		t.Errorf("20GB ModelOrchestrate = %q, want qwen3.5:4b", cfg.ModelOrchestrate)
	}
}

func TestAutoConfigureModels_30GB(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(30)
	if cfg.ModelEnrich != "qwen3.5:4b" {
		t.Errorf("30GB ModelEnrich = %q, want qwen3.5:4b", cfg.ModelEnrich)
	}
	if cfg.ModelOrchestrate != "qwen3.5:9b" {
		t.Errorf("30GB ModelOrchestrate = %q, want qwen3.5:9b", cfg.ModelOrchestrate)
	}
}

// ---------------------------------------------------------------------------
// applyDefaults — "auto" model trigger path
// ---------------------------------------------------------------------------

func TestApplyDefaults_AutoModelTriggersAutoConfigureModels(t *testing.T) {
	// Write a config file with model_ingest="auto" then LoadFile triggers applyDefaults
	// which calls AutoConfigureModels(0) when any tier model is "auto".
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"model_ingest":"auto","backend":"ollama"}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// After AutoConfigureModels(0) with 0 GB RAM, models should no longer be "auto".
	if cfg.ModelIngest == "auto" {
		t.Error("expected ModelIngest to be resolved from 'auto', still 'auto'")
	}
}

// ---------------------------------------------------------------------------
// SaveFile — MkdirAll error path
// ---------------------------------------------------------------------------

func TestSaveFile_MkdirAllError(t *testing.T) {
	// Use a path whose parent is a file (not a directory), so MkdirAll fails.
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// "notadir/sub/config.json" — MkdirAll fails because "notadir" is a file
	err := config.SaveFile(filepath.Join(file, "sub", "config.json"), config.BrainConfig{})
	if err == nil {
		t.Fatal("expected error when MkdirAll fails, got nil")
	}
}

// ---------------------------------------------------------------------------
// applyDefaults — comprehensive zero-value overrides (covers remaining branches)
// All fields set to "" via JSON override DefaultConfig() values.
// ---------------------------------------------------------------------------

func TestApplyDefaults_AllZeroOverrides(t *testing.T) {
	// Explicitly zero out fields that DefaultConfig() pre-fills.
	// json.Unmarshal overwrites them → applyDefaults sets defaults.
	path := writePartialConfig(t, map[string]interface{}{
		"backend":            "ollama",
		"fast_model":         "",
		"model_guardian":     "",
		"model_enrich":       "",
		"model_orchestrate":  "",
		"db_path":            "",
		"default_phase":      "",
		"default_mode":       "",
		"model_dir":          "",
		"hf_filename":        "",
		"embed_hf_repo":      "",
		"embed_hf_filename":  "",
		"embed_port":         0,
		"llama_bin_dir":      "",
		"llama_cpp_version":  "",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// All should be filled in by applyDefaults.
	if cfg.FastModel == "" {
		t.Error("expected FastModel to be set")
	}
	if cfg.ModelGuardian == "" {
		t.Error("expected ModelGuardian to be set")
	}
	if cfg.ModelEnrich == "" {
		t.Error("expected ModelEnrich to be set")
	}
	if cfg.ModelOrchestrate == "" {
		t.Error("expected ModelOrchestrate to be set")
	}
	if cfg.DBPath == "" {
		t.Error("expected DBPath to be set")
	}
	if cfg.DefaultPhase == "" {
		t.Error("expected DefaultPhase to be set")
	}
	if cfg.DefaultMode == "" {
		t.Error("expected DefaultMode to be set")
	}
	if cfg.ModelDir == "" {
		t.Error("expected ModelDir to be set")
	}
	if cfg.HFFilename == "" {
		t.Error("expected HFFilename to be set")
	}
	if cfg.EmbedHFRepo == "" {
		t.Error("expected EmbedHFRepo to be set")
	}
	if cfg.EmbedHFFilename == "" {
		t.Error("expected EmbedHFFilename to be set")
	}
	if cfg.EmbedPort == 0 {
		t.Error("expected EmbedPort to be set")
	}
	if cfg.LlamaBinDir == "" {
		t.Error("expected LlamaBinDir to be set")
	}
	if cfg.LlamaCPPVersion == "" {
		t.Error("expected LlamaCPPVersion to be set")
	}
}

// ---------------------------------------------------------------------------
// applyDefaults — local backend with all defaults zeroed → sets HFRepo
// ---------------------------------------------------------------------------

func TestApplyDefaults_LocalBackend_SetsHFRepo(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{
		"backend": "local",
		"hf_repo": "",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.HFRepo == "" {
		t.Error("expected HFRepo to be set for local backend")
	}
}

// ---------------------------------------------------------------------------
// DefaultConfig — fields that must always be non-zero (regression for Bug 2+3)
// ---------------------------------------------------------------------------

func TestDefaultConfig_ModelArchivistIsSet(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.ModelArchivist == "" {
		t.Error("DefaultConfig().ModelArchivist must not be empty — Archivist client would call Ollama with empty model string")
	}
	if cfg.ModelArchivist != "qwen3.5:2b" {
		t.Errorf("DefaultConfig().ModelArchivist = %q, want qwen3.5:2b", cfg.ModelArchivist)
	}
}

func TestDefaultConfig_MemorizeIsTrue(t *testing.T) {
	cfg := config.DefaultConfig()
	if !cfg.Memorize {
		t.Error("DefaultConfig().Memorize must be true — feature is silently disabled otherwise, inconsistent with other feature flags")
	}
}

// ---------------------------------------------------------------------------
// AutoConfigureModels — IntelligenceMode paths (Optimal / Standard / Full)
// ---------------------------------------------------------------------------

// All modes use synapses/* Ollama identities backed by base qwen3.5:2b.
// Modes differ only in keep_alive, not model tags.

func TestAutoConfigureModels_ModeOptimal(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IntelligenceMode = config.ModeOptimal
	cfg.AutoConfigureModels(0)

	if cfg.ModelIngest != "synapses/sentry" {
		t.Errorf("Optimal ModelIngest = %q, want synapses/sentry", cfg.ModelIngest)
	}
	if cfg.ModelEnrich != "synapses/librarian" {
		t.Errorf("Optimal ModelEnrich = %q, want synapses/librarian", cfg.ModelEnrich)
	}
	// Guardian shares Librarian in Optimal (no separate Critic slot).
	if cfg.ModelGuardian != "synapses/librarian" {
		t.Errorf("Optimal ModelGuardian = %q, want synapses/librarian (shares Librarian)", cfg.ModelGuardian)
	}
	if cfg.ModelOrchestrate != "synapses/navigator" {
		t.Errorf("Optimal ModelOrchestrate = %q, want synapses/navigator", cfg.ModelOrchestrate)
	}
	if cfg.ModelArchivist != "synapses/archivist" {
		t.Errorf("Optimal ModelArchivist = %q, want synapses/archivist", cfg.ModelArchivist)
	}
}

func TestAutoConfigureModels_ModeStandard(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IntelligenceMode = config.ModeStandard
	cfg.AutoConfigureModels(0)

	if cfg.ModelIngest != "synapses/sentry" {
		t.Errorf("Standard ModelIngest = %q, want synapses/sentry", cfg.ModelIngest)
	}
	if cfg.ModelEnrich != "synapses/librarian" {
		t.Errorf("Standard ModelEnrich = %q, want synapses/librarian", cfg.ModelEnrich)
	}
	if cfg.ModelGuardian != "synapses/critic" {
		t.Errorf("Standard ModelGuardian = %q, want synapses/critic", cfg.ModelGuardian)
	}
	if cfg.ModelOrchestrate != "synapses/navigator" {
		t.Errorf("Standard ModelOrchestrate = %q, want synapses/navigator", cfg.ModelOrchestrate)
	}
	if cfg.ModelArchivist != "synapses/archivist" {
		t.Errorf("Standard ModelArchivist = %q, want synapses/archivist", cfg.ModelArchivist)
	}
}

func TestAutoConfigureModels_ModeFull(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IntelligenceMode = config.ModeFull
	cfg.AutoConfigureModels(0)

	// Full uses same model identities as Standard — difference is keep_alive only.
	if cfg.ModelIngest != "synapses/sentry" {
		t.Errorf("Full ModelIngest = %q, want synapses/sentry", cfg.ModelIngest)
	}
	if cfg.ModelEnrich != "synapses/librarian" {
		t.Errorf("Full ModelEnrich = %q, want synapses/librarian", cfg.ModelEnrich)
	}
	if cfg.ModelGuardian != "synapses/critic" {
		t.Errorf("Full ModelGuardian = %q, want synapses/critic", cfg.ModelGuardian)
	}
	if cfg.ModelOrchestrate != "synapses/navigator" {
		t.Errorf("Full ModelOrchestrate = %q, want synapses/navigator", cfg.ModelOrchestrate)
	}
	if cfg.ModelArchivist != "synapses/archivist" {
		t.Errorf("Full ModelArchivist = %q, want synapses/archivist", cfg.ModelArchivist)
	}
}

// ---------------------------------------------------------------------------
// KeepAlive / KeepAliveValues — per-mode keep_alive (Sprint 17 #3)
// ---------------------------------------------------------------------------

// All 5 Ollama identities share the same base model weights so a single
// keep_alive value applies. Per-mode policy:
//   - optimal  → 120s  (2-min eviction on 8 GB machines)
//   - standard → 300s  (5-min eviction on 16 GB machines)
//   - full     → -1    (pinned on 32 GB+ machines)
//   - ""       → -1    (backward-compatible default)
func TestKeepAlive_PerMode(t *testing.T) {
	cases := []struct {
		mode config.IntelligenceMode
		want int
	}{
		{config.ModeOptimal, 120},
		{config.ModeStandard, 300},
		{config.ModeFull, -1},
		{"", -1},
	}
	for _, tc := range cases {
		cfg := config.DefaultConfig()
		cfg.IntelligenceMode = tc.mode
		if got := cfg.KeepAlive(); got != tc.want {
			t.Errorf("mode=%q KeepAlive() = %d, want %d", tc.mode, got, tc.want)
		}
	}
}

// KeepAliveValues delegates to KeepAlive() — all four returned values must
// equal the single per-mode keep_alive.
func TestKeepAliveValues_DelegatesPerMode(t *testing.T) {
	cases := []struct {
		mode config.IntelligenceMode
		want int
	}{
		{config.ModeOptimal, 120},
		{config.ModeStandard, 300},
		{config.ModeFull, -1},
		{"", -1},
	}
	for _, tc := range cases {
		cfg := config.DefaultConfig()
		cfg.IntelligenceMode = tc.mode
		kaG, kaE, kaO, kaA := cfg.KeepAliveValues()
		for name, v := range map[string]int{
			"guardian": kaG, "enrich": kaE, "orchestrate": kaO, "archivist": kaA,
		} {
			if v != tc.want {
				t.Errorf("mode=%q %s = %d, want %d", tc.mode, name, v, tc.want)
			}
		}
	}
}
